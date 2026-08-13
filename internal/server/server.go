package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"beads_watch/internal/brexec"
	"beads_watch/internal/tsidentity"
)

// Version is stamped on every response so a client can tell which daemon
// answered without a separate round trip.
const Version = "0.1.0"

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
}

// New builds a Server from a normalized Config.
func New(cfg *Config, log *slog.Logger) *Server {
	allow := make(map[string]bool, len(cfg.Allow))
	for _, c := range cfg.Allow {
		allow[c] = true
	}
	node, err := os.Hostname()
	if err != nil {
		node = "unknown"
	}
	return &Server{
		cfg: cfg,
		runner: &brexec.Runner{
			Path:      cfg.BrPath,
			MaxOutput: cfg.MaxOutputBytes,
		},
		resolver: &tsidentity.Resolver{},
		allow:    allow,
		log:      log,
		node:     node,
	}
}

// Handler returns the daemon's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/whoami", s.handleWhoami)
	mux.HandleFunc("GET /v1/repos", s.handleRepos)
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

// --- identity -------------------------------------------------------------

// Identity is what the proxy told us about the caller. Absent identity is
// represented honestly rather than filled in with a guess: an invented actor
// in the audit trail is worse than a missing one.
type Identity struct {
	// Login is the human, when there is one. Tagged devices have no user, so
	// this stays empty for them no matter how the request arrived.
	Login string `json:"login,omitempty"`
	Name  string `json:"name,omitempty"`
	// Peer is the calling machine, resolved via WhoIs from the proxy's
	// X-Forwarded-For.
	Peer *tsidentity.Peer `json:"peer,omitempty"`
	// Actor is what lands in BD_ACTOR; empty means br uses its own default.
	Actor string `json:"actor"`
	// Source records how Actor was determined, so a reader of the audit trail
	// knows whether it was verified by the transport or merely claimed.
	Source string `json:"source"`
	// Verified is true when the proxy vouched for Actor, false when the caller
	// asserted it themselves.
	Verified bool `json:"verified"`
}

// identify resolves the caller, most trustworthy signal first.
func (s *Server) identify(ctx context.Context, r *http.Request, requested string) Identity {
	id := Identity{Source: "none"}
	id.Login = strings.TrimSpace(r.Header.Get("Tailscale-User-Login"))
	id.Name = strings.TrimSpace(r.Header.Get("Tailscale-User-Name"))

	// 1. The proxy names a user. It strips this header when a client sends it,
	//    so its presence means tailscaled put it there.
	if id.Login != "" {
		id.Actor, id.Source, id.Verified = id.Login, "tailscale-user-header", true
		return id
	}

	// 2. No user header — the usual case for tagged devices, which have no
	//    owning user. The proxy still tells us which machine called, and it
	//    replaces rather than appends X-Forwarded-For, so that address is
	//    its own view of the peer and not the caller's claim.
	if addr := tsidentity.PeerAddr(r.Header.Get("X-Forwarded-For")); addr != "" {
		if peer, err := s.resolver.WhoIs(ctx, addr); err == nil && peer != nil {
			id.Peer = peer
			if a := peer.Actor(); a != "" {
				id.Actor, id.Source, id.Verified = a, "tailscale-whois", true
				return id
			}
		} else if err != nil {
			s.log.Debug("whois failed", "addr", addr, "err", err)
		}
	}

	// 3. Self-asserted. Useful for local callers that never traverse the
	//    proxy, and marked unverified so the distinction survives.
	if requested = strings.TrimSpace(requested); requested != "" {
		id.Actor, id.Source, id.Verified = requested, "request", false
		return id
	}

	return id
}

// --- handlers -------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"node":    s.node,
		"version": Version,
		"repos":   len(s.cfg.Repos),
	})
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
				st.Error = fmt.Sprintf("br exited %d: %s", res.ExitCode, firstLine(res.Stdout, res.Stderr))
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
	h.Set("X-Br-Exit", strconv.Itoa(res.ExitCode))
	h.Set("X-Br-Repo", repo.Name)
	h.Set("X-Br-Duration-Ms", strconv.FormatInt(res.Duration.Milliseconds(), 10))
	h.Set("X-Br-Actor-Source", id.Source)
	if id.Actor != "" {
		h.Set("X-Br-Actor", id.Actor)
		h.Set("X-Br-Actor-Verified", strconv.FormatBool(id.Verified))
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
		h.Set("X-Br-Stderr", base64.StdEncoding.EncodeToString(e))
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
		"Routes: GET /v1/health, GET /v1/whoami, GET /v1/repos, POST /v1/repos/{repo}/br")
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

func firstLine(streams ...[]byte) string {
	for _, s := range streams {
		t := strings.TrimSpace(string(s))
		if t == "" {
			continue
		}
		if i := strings.IndexByte(t, '\n'); i >= 0 {
			t = t[:i]
		}
		if len(t) > 200 {
			t = t[:200]
		}
		return t
	}
	return "no output"
}
