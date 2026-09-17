package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/a44-io/beads_watch/internal/brexec"
	"github.com/a44-io/beads_watch/internal/tsidentity"
)

// Version is stamped on every response so a client can tell which daemon
// answered without a separate round trip.
const Version = "1.1.1"

// Commit and BuiltAt are stamped at link time by scripts/publish-dist.sh:
//
//	go build -ldflags "-X github.com/a44-io/beads_watch/internal/server.Commit=$(git rev-parse HEAD)"
//
// A source build leaves them empty, which is the honest answer: an unstamped
// binary cannot say which commit it came from. The publisher relies on this to
// tell an already-current binary from one that must be rebuilt, and setup.sh
// relies on it to skip reinstalling a binary that is already the served commit.
var (
	Commit  string
	BuiltAt string
)

// maxRequestBody bounds the argv document. Nothing legitimate is large.
const maxRequestBody = 64 << 10

// maxStderrHeader caps the base64 stderr echo, since HTTP header blocks have
// their own limits and br's warnings are short.
const maxStderrHeader = 2048

// Server serves br over HTTP. It holds no state about issues — only the repo
// map and the argument policy.
type Server struct {
	cfg      *Config
	runner   *brexec.Runner
	resolver *tsidentity.Resolver
	allow    map[string]bool
	log      *slog.Logger
	node     string
	bell     *doorbell
}

// New builds a Server from a normalized Config. It starts nothing; Start
// begins the background watchers.
func New(cfg *Config, log *slog.Logger) *Server {
	allow := make(map[string]bool, len(cfg.Allow))
	for _, c := range cfg.Allow {
		allow[c] = true
	}
	node, err := os.Hostname()
	if err != nil {
		node = "unknown"
	}
	runner := &brexec.Runner{
		Path:      cfg.BrPath,
		MaxOutput: cfg.MaxOutputBytes,
	}
	return &Server{
		cfg:      cfg,
		runner:   runner,
		resolver: &tsidentity.Resolver{},
		allow:    allow,
		log:      log,
		node:     node,
		bell:     newDoorbell(cfg, runner, log),
	}
}

// Start begins the per-repo watchers behind GET /v1/events. They stop when
// ctx does.
func (s *Server) Start(ctx context.Context) {
	s.bell.start(ctx)
}

// Handler returns the daemon's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/whoami", s.handleWhoami)
	mux.HandleFunc("GET /v1/repos", s.handleRepos)
	mux.HandleFunc("GET /v1/events", s.handleEvents)
	mux.HandleFunc("POST /v1/repos/{repo}/br", s.handleBr)
	mux.HandleFunc("/", s.handleNotFound)
	return s.withCommonHeaders(mux)
}

func (s *Server) withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Beads-Watch-Version", Version)
		w.Header().Set("X-Beads-Watch-Node", s.node)
		next.ServeHTTP(w, r)
	})
}

// setHeader writes one response header with CR and LF flattened to spaces.
// Every X-Br-* value comes from outside the daemon: the repo name from the
// config file, stderr from br, the actor from tailscaled or, self-asserted,
// from the request body (that one is refused by validActor before it gets
// here). net/http already rewrites CR and LF in header values to spaces, so a
// header cannot be split whatever reaches it; this makes that guarantee ours
// instead of the stdlib's, and gives every such value one place to pass
// through.
func setHeader(h http.Header, key, value string) {
	h.Set(key, strings.NewReplacer("\r", " ", "\n", " ").Replace(value))
}

// --- transport ------------------------------------------------------------

// Transport is what the listener knew about a connection before a byte of
// any request arrived: which listener it came in on and, for TCP, the peer's
// address. Trust is decided on this and nothing else — the peer cannot change
// mid-connection and nothing in a request can forge it — which is what makes
// forwarded identity headers safe to believe from a proxy and safe to ignore
// from anyone else.
type Transport struct {
	// Network is "tcp" or "unix"; empty when the request did not arrive
	// through a listener this daemon tagged (tests, mainly), which is treated
	// as the least trusted case.
	Network string `json:"network"`
	// Peer is the TCP peer's address, absent on the unix socket.
	Peer string `json:"peer,omitempty"`
	// TrustedProxy is whether Peer is in trusted_proxies, so its forwarded
	// identity headers are believed.
	TrustedProxy bool `json:"trusted_proxy"`

	addr netip.Addr
}

type transportKey struct{}

// ConnContext tags each accepted connection with its Transport. It is meant
// for http.Server.ConnContext, and every request on the connection sees it.
func (s *Server) ConnContext(ctx context.Context, c net.Conn) context.Context {
	t := Transport{Network: c.RemoteAddr().Network()}
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		t.addr = ap.Addr().Unmap()
		t.Peer = t.addr.String()
		t.TrustedProxy = s.cfg.TrustedProxy(t.addr)
	}
	return context.WithValue(ctx, transportKey{}, t)
}

func transportOf(ctx context.Context) Transport {
	t, _ := ctx.Value(transportKey{}).(Transport)
	return t
}

// --- identity -------------------------------------------------------------

// Identity is what the transport told us about the caller. Absent identity is
// represented honestly rather than filled in with a guess: an invented actor
// in the audit trail is worse than a missing one.
type Identity struct {
	// Login is the human, when there is one. Tagged devices have no user, so
	// this stays empty for them no matter how the request arrived.
	Login string `json:"login,omitempty"`
	Name  string `json:"name,omitempty"`
	// Peer is the calling machine, resolved via WhoIs — from the trusted
	// proxy's X-Forwarded-For, or from the TCP peer itself.
	Peer *tsidentity.Peer `json:"peer,omitempty"`
	// Actor is what lands in BD_ACTOR; empty means br uses its own default.
	Actor string `json:"actor"`
	// Source records how Actor was determined, so a reader of the audit trail
	// knows whether it was verified by the transport or merely claimed:
	// tailscale-user-header and tailscale-whois came through a trusted proxy,
	// tailscale-whois-direct is the TCP peer resolved by its own address,
	// request is self-asserted, none is nothing at all.
	Source string `json:"source"`
	// Verified is true when the transport vouched for Actor, false when the
	// caller asserted it themselves.
	Verified bool `json:"verified"`
}

// identify resolves the caller, most trustworthy signal first. Which signals
// exist at all is decided by the transport: forwarded headers are the proxy's
// word only when the connection actually came from a trusted proxy. From any
// other TCP peer they are that peer's own claim and are not read; the peer's
// address is resolved instead, so a direct caller gets a correct verified
// identity — its own machine — rather than whatever it typed. On the unix
// socket nothing can vouch for a header, so only the body's actor counts.
func (s *Server) identify(ctx context.Context, r *http.Request, requested string) Identity {
	id := Identity{Source: "none"}
	tr := transportOf(ctx)

	switch {
	case tr.TrustedProxy:
		id.Login = strings.TrimSpace(r.Header.Get("Tailscale-User-Login"))
		id.Name = strings.TrimSpace(r.Header.Get("Tailscale-User-Name"))

		// 1. The proxy names a user. It strips this header when a client
		//    sends it, so its presence means the proxy put it there.
		if id.Login != "" {
			id.Actor, id.Source, id.Verified = id.Login, "tailscale-user-header", true
			return id
		}

		// 2. No user header — the usual case for tagged devices, which have
		//    no owning user. The proxy still tells us which machine called:
		//    it appends its own view of the peer to X-Forwarded-For, and
		//    PeerAddr takes the last entry, so a caller's forged prefix loses.
		if addr := tsidentity.PeerAddr(r.Header.Get("X-Forwarded-For")); addr != "" {
			if peer := s.whois(ctx, addr); peer != nil {
				id.Peer = peer
				if a := peer.Actor(); a != "" {
					id.Actor, id.Source, id.Verified = a, "tailscale-whois", true
					return id
				}
			}
		}

	case tr.Network == "tcp" && tr.addr.IsValid():
		// A peer that reached the TCP listener itself. Its headers are its
		// own claims; its address is the transport's fact, so that is what
		// gets resolved.
		if peer := s.whois(ctx, tr.addr.String()); peer != nil {
			id.Peer = peer
			if a := peer.Actor(); a != "" {
				id.Actor, id.Source, id.Verified = a, "tailscale-whois-direct", true
				return id
			}
		}
	}

	// 3. Self-asserted. The only signal on the unix socket, and the fallback
	//    when a TCP peer is not a tailnet node; marked unverified so the
	//    distinction survives into the audit trail.
	if requested = strings.TrimSpace(requested); requested != "" {
		id.Actor, id.Source, id.Verified = requested, "request", false
		return id
	}

	return id
}

func (s *Server) whois(ctx context.Context, addr string) *tsidentity.Peer {
	peer, err := s.resolver.WhoIs(ctx, addr)
	if err != nil {
		s.log.Debug("whois failed", "addr", addr, "err", err)
		return nil
	}
	return peer
}

// --- handlers -------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"ok":      true,
		"node":    s.node,
		"version": Version,
		"repos":   len(s.cfg.Repos),
	}
	// Only present on a published binary. Absent means a source build, which
	// is a real distinction: it says the running code was never stamped, so
	// no commit can be claimed for it.
	if Commit != "" {
		body["commit"] = Commit
	}
	// A repo whose path is not usable right now is still served in name, so
	// it counts in "repos"; this is the signal that some of them will answer
	// 503. A stat per repo is cheap enough for a liveness check, and it stays
	// honest: the number reflects the box as it is, not as it was at startup.
	if n := s.unavailable(); n > 0 {
		body["repos_unavailable"] = n
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) unavailable() int {
	n := 0
	for _, repo := range s.cfg.Repos {
		if repo.Probe() != nil {
			n++
		}
	}
	return n
}

// handleWhoami reports exactly which identity headers arrived. Tagged devices
// and user-owned nodes behave differently here, so this exists to settle the
// question by observation instead of assumption.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	id := s.identify(r.Context(), r, "")

	forwarded := map[string]string{}
	all := map[string]string{}
	for name, vals := range r.Header {
		joined := strings.Join(vals, ", ")
		all[name] = joined
		if strings.HasPrefix(strings.ToLower(name), "tailscale-") {
			forwarded[name] = joined
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"identity":              id,
		"transport":             transportOf(r.Context()),
		"user_header_forwarded": len(forwarded) > 0,
		"tailscale_headers":     forwarded,
		"peer_addr":             tsidentity.PeerAddr(r.Header.Get("X-Forwarded-For")),
		"remote_addr":           r.RemoteAddr,
		"all_headers":           all,
	})
}

type repoStatus struct {
	Name  string          `json:"name"`
	Path  string          `json:"path"`
	OK    bool            `json:"ok"`
	Where json.RawMessage `json:"where,omitempty"`
	Error string          `json:"error,omitempty"`
}

// handleRepos lists the repos this node serves. The per-repo detail comes from
// `br where` itself — the daemon does not know a repo's prefix or database
// path, it asks br, which is why a .beads/redirect resolves correctly here.
func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	id := s.identify(r.Context(), r, "")

	out := make([]repoStatus, len(s.cfg.Repos))
	var wg sync.WaitGroup
	for i, repo := range s.cfg.Repos {
		wg.Add(1)
		go func(i int, repo Repo) {
			defer wg.Done()
			st := repoStatus{Name: repo.Name, Path: repo.Path}

			// A path that is not there is reported as itself rather than as
			// br's chdir failure, and costs no fork.
			if err := repo.Probe(); err != nil {
				st.Error = err.Error()
				out[i] = st
				return
			}

			ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout())
			defer cancel()

			res, err := s.runner.Run(ctx, brexec.Request{
				Dir:   repo.Path,
				Args:  []string{"--json", "where"},
				Actor: id.Actor,
			})
			switch {
			case err != nil:
				st.Error = err.Error()
			case res.ExitCode != 0:
				st.Error = fmt.Sprintf("br exited %d: %s", res.ExitCode, res.ErrorMessage())
			case json.Valid(res.Stdout):
				st.OK = true
				st.Where = json.RawMessage(res.Stdout)
			default:
				st.Error = "br where returned unparseable output"
			}
			out[i] = st
		}(i, repo)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]any{
		"node":  s.node,
		"repos": out,
	})
}

type brRequest struct {
	Args  []string `json:"args"`
	Actor string   `json:"actor,omitempty"`
}

// handleBr is the whole point of the daemon: run br in a repo and hand back
// what it said, byte for byte, with its exit code intact.
func (s *Server) handleBr(w http.ResponseWriter, r *http.Request) {
	repoName := r.PathValue("repo")
	repo, ok := s.cfg.Lookup(repoName)
	if !ok {
		writeError(w, http.StatusNotFound, "REPO_NOT_FOUND",
			fmt.Sprintf("This node does not serve a repo named %q.", repoName),
			"GET /v1/repos lists the repos this node serves.")
		return
	}
	// Probed now rather than trusted from startup, so a checkout that arrived
	// since then answers and one that left since then is refused. Running br
	// anyway would fail on chdir with a message that reads like a daemon bug.
	// The hint names the sandbox case because "no such file" is misleading
	// when the operator can ls the path themselves: the unit's namespace may
	// simply not include it.
	if err := repo.Probe(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "REPO_UNAVAILABLE",
			fmt.Sprintf("The repo %q is configured on this node but its path is not usable: %v.", repo.Name, err),
			"The checkout may be missing from this box, or hidden from the service by its sandbox "+
				"(PrivateTmp, ProtectHome, BindPaths). GET /v1/repos shows every repo's state.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req brRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST",
			"Could not parse the request body: "+err.Error(),
			`Send {"args":["ready","--limit","20"]}.`)
		return
	}

	if !validActor(req.Actor) {
		writeError(w, http.StatusBadRequest, "ACTOR_INVALID",
			"The requested actor contains control characters or is too long.",
			"BD_ACTOR is written verbatim into the audit trail; keep it a plain short name.")
		return
	}

	argv, contentType, err := s.buildArgv(req.Args)
	if err != nil {
		var pe *policyError
		if errors.As(err, &pe) {
			writeError(w, pe.status, pe.code, pe.message, pe.hint)
			return
		}
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), "")
		return
	}

	id := s.identify(r.Context(), r, req.Actor)

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout())
	defer cancel()

	res, err := s.runner.Run(ctx, brexec.Request{
		Dir:   repo.Path,
		Args:  argv,
		Actor: id.Actor,
	})
	if err != nil {
		if errors.Is(err, brexec.ErrTimeout) {
			s.log.Warn("br timed out", "repo", repo.Name, "args", argv)
			writeError(w, http.StatusGatewayTimeout, "BR_TIMEOUT",
				fmt.Sprintf("br did not finish within %s.", s.cfg.Timeout()),
				"A write may be waiting on .beads/.write.lock; retry or raise timeout_seconds.")
			return
		}
		s.log.Error("br failed to run", "repo", repo.Name, "err", err)
		writeError(w, http.StatusInternalServerError, "BR_EXEC_FAILED",
			"Could not run br: "+err.Error(),
			"Check br_path in the daemon config and that br is executable.")
		return
	}

	h := w.Header()
	// X-Br-Exit is the discriminator: present means br ran and this body is
	// br's own output. Absent means the daemon failed before br spoke.
	setHeader(h, "X-Br-Exit", strconv.Itoa(res.ExitCode))
	setHeader(h, "X-Br-Repo", repo.Name)
	setHeader(h, "X-Br-Duration-Ms", strconv.FormatInt(res.Duration.Milliseconds(), 10))
	setHeader(h, "X-Br-Actor-Source", id.Source)
	if id.Actor != "" {
		setHeader(h, "X-Br-Actor", id.Actor)
		setHeader(h, "X-Br-Actor-Verified", strconv.FormatBool(id.Verified))
	}
	if res.StdoutTruncated {
		h.Set("X-Br-Stdout-Truncated", "1")
	}
	if len(res.Stderr) > 0 {
		// br writes non-fatal warnings here (AUTO_FLUSH_FAILED and friends).
		// Dropping them would hide a mutation that succeeded but did not export.
		e := res.Stderr
		if len(e) > maxStderrHeader {
			e = e[:maxStderrHeader]
		}
		setHeader(h, "X-Br-Stderr", base64.StdEncoding.EncodeToString(e))
	}
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")

	// 200 means "br ran and this is what it said". br's own verdict travels in
	// X-Br-Exit, untranslated: flattening exit 3 into HTTP 404 would discard
	// the distinction between not-found, validation, and dependency-cycle.
	w.WriteHeader(http.StatusOK)
	w.Write(res.Stdout)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "NO_SUCH_ROUTE",
		fmt.Sprintf("No route for %s %s.", r.Method, r.URL.Path),
		"Routes: GET /v1/health, GET /v1/whoami, GET /v1/repos, GET /v1/events, POST /v1/repos/{repo}/br")
}

// maxActorLen bounds a self-asserted actor. Real names are far shorter.
const maxActorLen = 128

// validActor screens a caller-supplied actor. This is not about HTTP header
// injection — Go already replaces CR and LF in response header values. It is
// about the audit trail: BD_ACTOR is written verbatim into .beads JSONL and
// audit events, where an embedded newline would corrupt the record itself.
// Identity that came from the proxy is not screened here; tailscaled is the
// one vouching for it.
func validActor(actor string) bool {
	if len(actor) > maxActorLen {
		return false
	}
	for _, r := range actor {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// --- argv policy ----------------------------------------------------------

type policyError struct {
	status  int
	code    string
	message string
	hint    string
}

func (e *policyError) Error() string { return e.message }

// buildArgv validates the caller's args and prepends the daemon's global
// flags. Global flags go first so they are unambiguous regardless of the
// subcommand's own positional arguments.
func (s *Server) buildArgv(args []string) (argv []string, contentType string, err error) {
	if len(args) == 0 {
		return nil, "", &policyError{http.StatusBadRequest, "EMPTY_ARGS",
			"args must contain at least the br subcommand.",
			`Example: {"args":["ready","--limit","20"]}`}
	}
	cmd := args[0]
	if strings.HasPrefix(cmd, "-") {
		return nil, "", &policyError{http.StatusBadRequest, "COMMAND_EXPECTED",
			fmt.Sprintf("args[0] must be a br subcommand, got the flag %q.", cmd),
			"Put global flags after the subcommand."}
	}
	if !s.allow[cmd] {
		return nil, "", &policyError{http.StatusForbidden, "COMMAND_NOT_ALLOWED",
			fmt.Sprintf("The br subcommand %q is not served by this daemon.", cmd),
			"Allowed: " + strings.Join(s.cfg.Allow, ", ")}
	}
	for _, a := range args {
		if denied, ok := deniedFlag(a); ok {
			return nil, "", &policyError{http.StatusForbidden, "FLAG_NOT_ALLOWED",
				fmt.Sprintf("The flag %s may not be passed to this daemon.", denied),
				"--db would target a different repo than the URL names; --actor would forge the audit identity."}
		}
	}

	var globals []string
	if !hasFlag(args, "--lock-timeout") {
		globals = append(globals, "--lock-timeout", strconv.Itoa(s.cfg.LockTimeoutMS))
	}

	contentType = "application/json"
	if format, explicit := requestedFormat(args); explicit {
		contentType = contentTypeFor(format)
	} else if *s.cfg.InjectJSON {
		globals = append(globals, "--json")
	} else {
		contentType = "text/plain; charset=utf-8"
	}

	argv = make([]string, 0, len(globals)+len(args))
	argv = append(argv, globals...)
	argv = append(argv, args...)
	return argv, contentType, nil
}

// requestedFormat reports whether the caller already chose an output format,
// and which one. The daemon only injects --json when they did not.
func requestedFormat(args []string) (format string, explicit bool) {
	for i, a := range args {
		switch {
		case a == "--json", a == "--robot":
			return "json", true
		case a == "--format":
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		case strings.HasPrefix(a, "--format="):
			return strings.TrimPrefix(a, "--format="), true
		}
	}
	return "", false
}

func contentTypeFor(format string) string {
	switch format {
	case "json":
		return "application/json"
	case "csv":
		return "text/csv; charset=utf-8"
	default:
		// toon, text, and anything br adds later.
		return "text/plain; charset=utf-8"
	}
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

func deniedFlag(arg string) (string, bool) {
	for _, d := range DeniedFlags {
		if arg == d || strings.HasPrefix(arg, d+"=") {
			return d, true
		}
	}
	return "", false
}

// --- responses ------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(body)
}

// writeError emits a daemon-level failure. The shape mirrors br's own error
// envelope so clients need one parser, but "source":"beads_watch" and the
// absent X-Br-Exit header make the origin unambiguous.
func writeError(w http.ResponseWriter, status int, code, message, hint string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
			"hint":    hint,
			"source":  "beads_watch",
		},
	})
}
