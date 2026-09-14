package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testServer(t *testing.T, brScript string) (*Server, string) {
	t.Helper()
	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return serverFor(t, brScript, Repo{Name: "demo", Path: repoDir}), repoDir
}

// serverFor builds a Server over exactly these repos and a fake br. Paths
// need not exist; at least one must, or Normalize refuses to serve nothing.
func serverFor(t *testing.T, brScript string, repos ...Repo) *Server {
	t.Helper()
	br := filepath.Join(t.TempDir(), "br")
	if err := os.WriteFile(br, []byte("#!/bin/sh\n"+brScript), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Repos: repos, BrPath: br}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg, log)
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func post(t *testing.T, s *Server, repo, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/repos/"+repo+"/br", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAllowlistBlocksUnservedCommands(t *testing.T) {
	s, _ := testServer(t, `echo ran`)

	// `upgrade` replaces the br binary: allowing it over the network would be
	// remote code execution on the host.
	for _, cmd := range []string{"upgrade", "init", "delete", "doctor", "config"} {
		rec := post(t, s, "demo", `{"args":["`+cmd+`"]}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", cmd, rec.Code)
		}
		if rec.Header().Get("X-Br-Exit") != "" {
			t.Errorf("%s: X-Br-Exit present on a daemon-level refusal", cmd)
		}
	}
	if rec := post(t, s, "demo", `{"args":["ready"]}`); rec.Code != http.StatusOK {
		t.Errorf("ready: status = %d, want 200", rec.Code)
	}
}

// TestDeniedFlagsBlockEscapes covers the two flags that would let a caller
// break the promise the URL makes: --db picks a different workspace, --actor
// forges the audit identity.
func TestDeniedFlagsBlockEscapes(t *testing.T) {
	s, _ := testServer(t, `echo ran`)

	cases := []string{
		`{"args":["ready","--db","/elsewhere/beads.db"]}`,
		`{"args":["ready","--db=/elsewhere/beads.db"]}`,
		`{"args":["create","x","--actor","root"]}`,
		`{"args":["create","x","--actor=root"]}`,
	}
	for _, body := range cases {
		rec := post(t, s, "demo", body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", body, rec.Code)
		}
		var out struct {
			Error struct{ Code, Source string } `json:"error"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Error.Code != "FLAG_NOT_ALLOWED" {
			t.Errorf("%s: code = %q, want FLAG_NOT_ALLOWED", body, out.Error.Code)
		}
		if out.Error.Source != "beads_watch" {
			t.Errorf("%s: source = %q, want the daemon to own its errors", body, out.Error.Source)
		}
	}
}

func TestArgsMustStartWithSubcommand(t *testing.T) {
	s, _ := testServer(t, `echo ran`)

	if rec := post(t, s, "demo", `{"args":[]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty args: status = %d, want 400", rec.Code)
	}
	// A global flag in front would smuggle a command past the allowlist check.
	if rec := post(t, s, "demo", `{"args":["--json","ready"]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("leading flag: status = %d, want 400", rec.Code)
	}
}

func TestGlobalFlagsInjected(t *testing.T) {
	s, _ := testServer(t, `echo "$@"`)

	rec := post(t, s, "demo", `{"args":["ready","--limit","20"]}`)
	got := strings.TrimSpace(rec.Body.String())
	want := "--lock-timeout 5000 --json ready --limit 20"
	if got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestCallerFormatChoiceIsRespected(t *testing.T) {
	s, _ := testServer(t, `echo "$@"`)

	// The caller asked for TOON; the daemon must not force --json on top.
	rec := post(t, s, "demo", `{"args":["ready","--format","toon"]}`)
	if strings.Contains(rec.Body.String(), "--json") {
		t.Errorf("argv = %q, want no injected --json", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain for toon", ct)
	}

	// An explicit lock-timeout must not be duplicated.
	rec = post(t, s, "demo", `{"args":["ready","--lock-timeout","99"]}`)
	if strings.Count(rec.Body.String(), "--lock-timeout") != 1 {
		t.Errorf("argv = %q, want exactly one --lock-timeout", rec.Body.String())
	}
}

// TestBrExitCodeIsPassedThrough is the contract the tv client depends on:
// HTTP 200 means br ran, and br's own verdict lives in X-Br-Exit.
func TestBrExitCodeIsPassedThrough(t *testing.T) {
	s, _ := testServer(t, `echo '{"error":{"code":"ISSUE_NOT_FOUND"}}'; exit 3`)

	rec := post(t, s, "demo", `{"args":["show","nope"]}`)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — br failing is not the daemon failing", rec.Code)
	}
	if got := rec.Header().Get("X-Br-Exit"); got != "3" {
		t.Errorf("X-Br-Exit = %q, want 3", got)
	}
	if !strings.Contains(rec.Body.String(), "ISSUE_NOT_FOUND") {
		t.Errorf("body = %q, want br's own envelope verbatim", rec.Body.String())
	}
}

func TestStderrIsSurfaced(t *testing.T) {
	// A mutation that succeeded but failed to export writes a warning here;
	// dropping it would hide a half-finished write.
	s, _ := testServer(t, `echo '{}'; echo '{"warning":{"code":"AUTO_FLUSH_FAILED"}}' >&2`)

	rec := post(t, s, "demo", `{"args":["create","x"]}`)
	raw, err := base64.StdEncoding.DecodeString(rec.Header().Get("X-Br-Stderr"))
	if err != nil {
		t.Fatalf("decoding X-Br-Stderr: %v", err)
	}
	if !strings.Contains(string(raw), "AUTO_FLUSH_FAILED") {
		t.Errorf("X-Br-Stderr = %q, want br's warning", raw)
	}
}

func TestUnknownRepo(t *testing.T) {
	s, _ := testServer(t, `echo ran`)

	rec := post(t, s, "nope", `{"args":["ready"]}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if rec.Header().Get("X-Br-Exit") != "" {
		t.Error("X-Br-Exit present though br never ran")
	}
}

func TestRequestActorIsUnverified(t *testing.T) {
	s, _ := testServer(t, `printf '%s' "$BD_ACTOR"`)

	rec := post(t, s, "demo", `{"args":["create","x"],"actor":"claimed-name"}`)
	if got := rec.Body.String(); got != "claimed-name" {
		t.Errorf("BD_ACTOR = %q, want the requested actor", got)
	}
	if got := rec.Header().Get("X-Br-Actor-Verified"); got != "false" {
		t.Errorf("X-Br-Actor-Verified = %q, want false for a self-asserted actor", got)
	}
	if got := rec.Header().Get("X-Br-Actor-Source"); got != "request" {
		t.Errorf("X-Br-Actor-Source = %q, want request", got)
	}
}

// --- identity by transport ---------------------------------------------------

// fakeTailscale answers `tailscale whois --json <addr>` for two made-up
// tailnet peers and fails for anything else, the way the real one does for a
// non-tailnet address.
func fakeTailscale(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tailscale")
	script := `#!/bin/sh
case "$3" in
  100.1.1.1) echo '{"Node":{"Name":"dev.example.ts.net.","Tags":["tag:box"]},"UserProfile":{"LoginName":"tagged-devices"}}' ;;
  100.2.2.2) echo '{"Node":{"Name":"pi.example.ts.net.","Tags":["tag:box"]},"UserProfile":{"LoginName":"tagged-devices"}}' ;;
  *) echo "no such peer: $3" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// identityServer is a server whose whois goes to the fake, with pi
// (100.2.2.2) as the one trusted proxy.
func identityServer(t *testing.T, brScript string) *Server {
	t.Helper()
	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	br := filepath.Join(t.TempDir(), "br")
	if err := os.WriteFile(br, []byte("#!/bin/sh\n"+brScript), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Repos:          []Repo{{Name: "demo", Path: repoDir}},
		BrPath:         br,
		TrustedProxies: []string{"100.2.2.2"},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.resolver.Path = fakeTailscale(t)
	return s
}

// via tags a request with the transport ConnContext would have recorded for
// a connection from peer, so identity is decided the way it is in production.
func via(t *testing.T, s *Server, req *http.Request, network, peer string) *http.Request {
	t.Helper()
	conn := &fakeConn{network: network, addr: peer}
	return req.WithContext(s.ConnContext(req.Context(), conn))
}

type fakeConn struct {
	net.Conn
	network, addr string
}

func (c *fakeConn) RemoteAddr() net.Addr { return fakeAddr{c.network, c.addr} }

type fakeAddr struct{ network, addr string }

func (a fakeAddr) Network() string { return a.network }
func (a fakeAddr) String() string  { return a.addr }

func createVia(t *testing.T, s *Server, network, peer string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/repos/demo/br", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req = via(t, s, req, network, peer)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestTrustedProxyHeadersAreHonoured is the regression guard for the proxy
// path: through a trusted proxy the forwarded headers are the proxy's word,
// and today's behaviour must survive unchanged.
func TestTrustedProxyHeadersAreHonoured(t *testing.T) {
	s := identityServer(t, `printf '%s' "$BD_ACTOR"`)

	rec := createVia(t, s, "tcp", "100.2.2.2:40000",
		map[string]string{"Tailscale-User-Login": "someone@example.com"}, `{"args":["create","x"]}`)
	if got := rec.Body.String(); got != "someone@example.com" {
		t.Errorf("BD_ACTOR = %q, want the proxy-supplied login", got)
	}
	if got := rec.Header().Get("X-Br-Actor-Source"); got != "tailscale-user-header" {
		t.Errorf("source = %q, want tailscale-user-header", got)
	}

	// No user header (tagged caller): the proxy's X-Forwarded-For names the
	// machine. Caddy appends, so the LAST entry is the proxy's own view and a
	// forged prefix loses.
	rec = createVia(t, s, "tcp", "100.2.2.2:40001",
		map[string]string{"X-Forwarded-For": "9.9.9.9, 100.1.1.1"}, `{"args":["create","x"]}`)
	if got := rec.Body.String(); got != "dev" {
		t.Errorf("BD_ACTOR = %q, want dev from the proxy's X-Forwarded-For", got)
	}
	if got := rec.Header().Get("X-Br-Actor-Source"); got != "tailscale-whois" {
		t.Errorf("source = %q, want tailscale-whois", got)
	}
	if got := rec.Header().Get("X-Br-Actor-Verified"); got != "true" {
		t.Errorf("verified = %q, want true", got)
	}
}

// TestDirectPeerCannotForgeIdentity is the fix: a caller that reaches the TCP
// listener itself, skipping the proxy, gets its own machine as actor no
// matter what headers it sends.
func TestDirectPeerCannotForgeIdentity(t *testing.T) {
	s := identityServer(t, `printf '%s' "$BD_ACTOR"`)

	forged := map[string]string{
		"Tailscale-User-Login": "attacker@evil.com",
		"X-Forwarded-For":      "100.2.2.2",
	}
	rec := createVia(t, s, "tcp", "100.1.1.1:50000", forged, `{"args":["create","x"],"actor":"also-claimed"}`)
	if got := rec.Body.String(); got != "dev" {
		t.Errorf("BD_ACTOR = %q, want the caller's own machine", got)
	}
	if got := rec.Header().Get("X-Br-Actor-Source"); got != "tailscale-whois-direct" {
		t.Errorf("source = %q, want tailscale-whois-direct", got)
	}
	if got := rec.Header().Get("X-Br-Actor-Verified"); got != "true" {
		t.Errorf("verified = %q, want true — the transport vouches for the peer", got)
	}

	// And whoami must not echo the forgery anywhere in identity.
	req := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	for k, v := range forged {
		req.Header.Set(k, v)
	}
	req = via(t, s, req, "tcp", "100.1.1.1:50001")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var out struct {
		Identity  json.RawMessage `json:"identity"`
		Transport Transport       `json:"transport"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding whoami: %v", err)
	}
	if strings.Contains(string(out.Identity), "evil") || strings.Contains(string(out.Identity), "100.2.2.2") {
		t.Errorf("identity = %s, want no trace of the forged headers", out.Identity)
	}
	if out.Transport.Network != "tcp" || out.Transport.Peer != "100.1.1.1" || out.Transport.TrustedProxy {
		t.Errorf("transport = %+v, want tcp from 100.1.1.1, untrusted", out.Transport)
	}
}

// TestUnixSocketIgnoresForwardedHeaders: nothing on the local socket can
// vouch for a header, so only the body's actor counts there.
func TestUnixSocketIgnoresForwardedHeaders(t *testing.T) {
	s := identityServer(t, `printf '%s' "$BD_ACTOR"`)
	forged := map[string]string{"Tailscale-User-Login": "attacker@evil.com", "X-Forwarded-For": "100.1.1.1"}

	rec := createVia(t, s, "unix", "@", forged, `{"args":["create","x"]}`)
	if got := rec.Body.String(); got != "" {
		t.Errorf("BD_ACTOR = %q, want unset on the socket with no body actor", got)
	}
	if got := rec.Header().Get("X-Br-Actor-Source"); got != "none" {
		t.Errorf("source = %q, want none", got)
	}

	rec = createVia(t, s, "unix", "@", forged, `{"args":["create","x"],"actor":"local-agent"}`)
	if got := rec.Body.String(); got != "local-agent" {
		t.Errorf("BD_ACTOR = %q, want the self-asserted actor", got)
	}
	if got := rec.Header().Get("X-Br-Actor-Verified"); got != "false" {
		t.Errorf("verified = %q, want false", got)
	}
}

// TestUntaggedRequestIsLeastTrusted pins the zero value: a request that
// never went through ConnContext believes no header.
func TestUntaggedRequestIsLeastTrusted(t *testing.T) {
	s := identityServer(t, `printf '%s' "$BD_ACTOR"`)
	req := httptest.NewRequest(http.MethodPost, "/v1/repos/demo/br", strings.NewReader(`{"args":["create","x"]}`))
	req.Header.Set("Tailscale-User-Login", "someone@example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Br-Actor-Source"); got != "none" {
		t.Errorf("source = %q, want none for an untagged request", got)
	}
}

// TestDirectPeerOffTailnetFallsBackToRequest: a TCP peer whois cannot name
// (127.0.0.1 in a --listen trial) is not verified, but its self-asserted
// actor still counts, marked as such.
func TestDirectPeerOffTailnetFallsBackToRequest(t *testing.T) {
	s := identityServer(t, `printf '%s' "$BD_ACTOR"`)
	rec := createVia(t, s, "tcp", "127.0.0.1:60000",
		map[string]string{"Tailscale-User-Login": "attacker@evil.com"},
		`{"args":["create","x"],"actor":"me"}`)
	if got := rec.Body.String(); got != "me" {
		t.Errorf("BD_ACTOR = %q, want the body actor", got)
	}
	if got := rec.Header().Get("X-Br-Actor-Source"); got != "request" {
		t.Errorf("source = %q, want request", got)
	}
}

func TestTrustedProxiesConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Repos:          []Repo{{Name: "a", Path: dir}},
		TrustedProxies: []string{"100.2.2.2", "10.0.0.0/8", "fd7a:115c:a1e0::1", "::ffff:192.0.2.9"},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("Normalize() = %v", err)
	}
	for addr, want := range map[string]bool{
		"100.2.2.2":         true,
		"10.200.3.4":        true,
		"fd7a:115c:a1e0::1": true,
		"192.0.2.9":         true, // the v4-mapped entry matches its plain form
		"::ffff:100.2.2.2":  true, // and a mapped peer matches a plain entry
		"100.2.2.3":         false,
		"11.0.0.1":          false,
	} {
		if got := cfg.TrustedProxy(netip.MustParseAddr(addr)); got != want {
			t.Errorf("TrustedProxy(%s) = %v, want %v", addr, got, want)
		}
	}

	// Empty means trust no proxy, never "trust everyone".
	cfg = &Config{Repos: []Repo{{Name: "a", Path: dir}}}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedProxy(netip.MustParseAddr("100.2.2.2")) {
		t.Error("empty trusted_proxies trusted a peer")
	}

	// Garbage is a config error that names the entry.
	cfg = &Config{Repos: []Repo{{Name: "a", Path: dir}}, TrustedProxies: []string{"100.2.2.2", "pi"}}
	err := cfg.Normalize()
	if err == nil || !strings.Contains(err.Error(), `trusted_proxies[1] "pi"`) {
		t.Errorf("Normalize() = %v, want an error naming trusted_proxies[1]", err)
	}
}

func TestSocketDefaultDependsOnListen(t *testing.T) {
	dir := t.TempDir()
	repos := []Repo{{Name: "a", Path: dir}}

	cfg := &Config{Repos: repos}
	if err := cfg.Normalize(); err != nil || cfg.Socket == "" || cfg.Listen != "" {
		t.Errorf("neither set: socket=%q listen=%q err=%v, want the socket default", cfg.Socket, cfg.Listen, err)
	}
	cfg = &Config{Repos: repos, Listen: "127.0.0.1:0"}
	if err := cfg.Normalize(); err != nil || cfg.Socket != "" {
		t.Errorf("listen only: socket=%q err=%v, want TCP only", cfg.Socket, err)
	}
	cfg = &Config{Repos: repos, Socket: "/tmp/x.sock", Listen: "127.0.0.1:0"}
	if err := cfg.Normalize(); err != nil || cfg.Socket != "/tmp/x.sock" || cfg.Listen != "127.0.0.1:0" {
		t.Errorf("both: socket=%q listen=%q err=%v, want both kept", cfg.Socket, cfg.Listen, err)
	}
	cfg = &Config{Repos: repos, Listen: "no-port"}
	if err := cfg.Normalize(); err == nil || !strings.Contains(err.Error(), `listen "no-port"`) {
		t.Errorf("bad listen: err=%v, want an error naming it", err)
	}
}

// TestServesSocketAndTCPTogether runs one server on both listeners, tagged by
// ConnContext, and checks each answers and knows which transport it is.
func TestServesSocketAndTCPTogether(t *testing.T) {
	s := identityServer(t, `echo ran`)
	sock := filepath.Join(t.TempDir(), "bw.sock")
	uln, err := ListenSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	tln, err := ListenTCP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.Handler(), ConnContext: s.ConnContext}
	go srv.Serve(uln)
	go srv.Serve(tln)
	defer srv.Close()

	unix := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return net.Dial("unix", sock) },
	}}
	for name, c := range map[string]struct {
		client *http.Client
		url    string
		net    string
	}{
		"unix": {unix, "http://local/v1/whoami", "unix"},
		"tcp":  {http.DefaultClient, "http://" + tln.Addr().String() + "/v1/whoami", "tcp"},
	} {
		resp, err := c.client.Get(c.url)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var out struct {
			Transport Transport `json:"transport"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("X-Beads-Watch-Node") != s.node {
			t.Errorf("%s: status %d node %q, want 200 from this daemon", name, resp.StatusCode, resp.Header.Get("X-Beads-Watch-Node"))
		}
		if out.Transport.Network != c.net {
			t.Errorf("%s: transport = %+v, want network %s", name, out.Transport, c.net)
		}
		if c.net == "tcp" && out.Transport.Peer != "127.0.0.1" {
			t.Errorf("tcp: peer = %q, want 127.0.0.1", out.Transport.Peer)
		}
	}
	if info, err := os.Stat(sock); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v (err %v), want 0600", info.Mode().Perm(), err)
	}
}

// TestListenTCPBindsUnassignedAddress: at login the tailscale address may not
// exist yet, so the listener has to bind it anyway (IP_FREEBIND). 192.0.2.0/24
// is TEST-NET-1, assigned to no interface anywhere.
func TestListenTCPBindsUnassignedAddress(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("IP_FREEBIND is linux-only")
	}
	if ln, err := net.Listen("tcp", "192.0.2.1:0"); err == nil {
		ln.Close()
		t.Skip("192.0.2.1 is assigned on this box; cannot tell freebind from a plain bind")
	}
	ln, err := ListenTCP("192.0.2.1:0")
	if err != nil {
		t.Fatalf("ListenTCP on an unassigned address: %v, want IP_FREEBIND to allow it", err)
	}
	ln.Close()
}

func TestBodyLimits(t *testing.T) {
	s, _ := testServer(t, `echo ran`)

	if rec := post(t, s, "demo", `{"args":["ready"],"bogus":1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: status = %d, want 400", rec.Code)
	}
	if rec := post(t, s, "demo", `not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad json: status = %d, want 400", rec.Code)
	}
}

func TestHealthNeedsNoBr(t *testing.T) {
	s, _ := testServer(t, `exit 1`)

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 even when br is broken", rec.Code)
	}
}

// mixedRepos is one repo that exists and one whose path does not — the
// partially-broken config that used to crash-loop the daemon.
func mixedRepos(t *testing.T) (good, ghost Repo) {
	t.Helper()
	dir := t.TempDir()
	goodDir := filepath.Join(dir, "good")
	if err := os.MkdirAll(goodDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return Repo{Name: "good", Path: goodDir}, Repo{Name: "ghost", Path: filepath.Join(dir, "ghost")}
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) (code, message, hint string) {
	t.Helper()
	var out struct {
		Error struct{ Code, Message, Hint string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding error envelope %q: %v", rec.Body.String(), err)
	}
	return out.Error.Code, out.Error.Message, out.Error.Hint
}

// TestUnavailableRepoIsRefusedNotExecuted: a repo whose path is gone answers
// with a daemon-level 503 that says so, instead of br's chdir failure dressed
// up as BR_EXEC_FAILED — and the healthy repo beside it keeps working.
func TestUnavailableRepoIsRefusedNotExecuted(t *testing.T) {
	good, ghost := mixedRepos(t)
	s := serverFor(t, `echo ran`, good, ghost)

	rec := post(t, s, "ghost", `{"args":["ready"]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Br-Exit") != "" {
		t.Error("X-Br-Exit present though br never ran")
	}
	code, msg, hint := decodeError(t, rec)
	if code != "REPO_UNAVAILABLE" {
		t.Errorf("code = %q, want REPO_UNAVAILABLE", code)
	}
	if !strings.Contains(msg, "ghost") || !strings.Contains(msg, "no such file") {
		t.Errorf("message = %q, want the repo name and the stat error", msg)
	}
	// The sandbox case is the misleading one: the path exists for the operator
	// and not for the unit, and the hint has to say so.
	if !strings.Contains(hint, "ProtectHome") {
		t.Errorf("hint = %q, want it to mention unit sandboxing", hint)
	}

	if rec := post(t, s, "good", `{"args":["ready"]}`); rec.Code != http.StatusOK {
		t.Errorf("good: status = %d, want 200 — the ghost must not take it down", rec.Code)
	}
}

// TestUnavailableRepoHealsWithoutRestart: the probe is per request, so a
// clone that lands after startup starts answering on the next call.
func TestUnavailableRepoHealsWithoutRestart(t *testing.T) {
	good, ghost := mixedRepos(t)
	s := serverFor(t, `echo ran`, good, ghost)

	if rec := post(t, s, "ghost", `{"args":["ready"]}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before clone: status = %d, want 503", rec.Code)
	}
	if err := os.MkdirAll(ghost.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	if rec := post(t, s, "ghost", `{"args":["ready"]}`); rec.Code != http.StatusOK {
		t.Errorf("after clone: status = %d, want 200 with no restart", rec.Code)
	}

	// And the reverse: a checkout that disappears is refused, not exec'd.
	if err := os.RemoveAll(good.Path); err != nil {
		t.Fatal(err)
	}
	if rec := post(t, s, "good", `{"args":["ready"]}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("after removal: status = %d, want 503", rec.Code)
	}
}

func TestReposListsUnavailableRepoWithoutRunningBr(t *testing.T) {
	good, ghost := mixedRepos(t)
	// The fake br records every invocation, so the test can see that the
	// ghost repo cost no fork.
	calls := filepath.Join(t.TempDir(), "calls")
	s := serverFor(t, `pwd >> `+calls+`; echo '{"prefix":"t"}'`, good, ghost)

	rec := get(t, s, "/v1/repos")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Repos []struct {
			Name  string          `json:"name"`
			OK    bool            `json:"ok"`
			Where json.RawMessage `json:"where"`
			Error string          `json:"error"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	byName := map[string]int{}
	for i, r := range out.Repos {
		byName[r.Name] = i
	}
	g := out.Repos[byName["good"]]
	if !g.OK || len(g.Where) == 0 || g.Error != "" {
		t.Errorf("good = %+v, want ok with a where block", g)
	}
	gh := out.Repos[byName["ghost"]]
	if gh.OK || gh.Error == "" || !strings.Contains(gh.Error, "no such file") {
		t.Errorf("ghost = %+v, want ok:false with the stat error", gh)
	}

	raw, _ := os.ReadFile(calls)
	if got := strings.TrimSpace(string(raw)); got != good.Path {
		t.Errorf("br ran in %q, want only %q — a missing path must not be exec'd", got, good.Path)
	}
}

// TestReposRelaysBrErrorMessage: br pretty-prints its error envelope to
// stdout and exits 2, so the first line of that stream is "{". The error
// field exists to say why a repo is broken, and a brace says nothing.
func TestReposRelaysBrErrorMessage(t *testing.T) {
	s, _ := testServer(t, `cat <<'EOF'
{
  "error": {
    "code": "NOT_INITIALIZED",
    "message": "Beads not initialized: run 'br init' first",
    "hint": "Run: br init",
    "retryable": false,
    "context": null
  }
}
EOF
exit 2`)

	rec := get(t, s, "/v1/repos")
	var out struct {
		Repos []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	r := out.Repos[0]
	want := "br exited 2: Beads not initialized: run 'br init' first (hint: Run: br init)"
	if r.OK || r.Error != want {
		t.Errorf("error = %q, want %q", r.Error, want)
	}

	// Plain-text failures keep every line, joined, not just the first.
	s, _ = testServer(t, `echo 'thread main panicked' >&2; echo '  at src/x.rs:1' >&2; exit 101`)
	rec = get(t, s, "/v1/repos")
	out.Repos = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got := out.Repos[0].Error; got != "br exited 101: thread main panicked / at src/x.rs:1" {
		t.Errorf("error = %q, want both stderr lines joined", got)
	}
}

func TestHealthCountsUnavailableRepos(t *testing.T) {
	good, ghost := mixedRepos(t)
	s := serverFor(t, `exit 1`, good, ghost)

	var body map[string]any
	rec := get(t, s, "/v1/health")
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body["ok"] != true || body["repos"] != float64(2) {
		t.Errorf("health = %v, want ok with both repos counted as served", body)
	}
	if body["repos_unavailable"] != float64(1) {
		t.Errorf("repos_unavailable = %v, want 1", body["repos_unavailable"])
	}

	// The field is a signal, not a constant: absent when there is nothing
	// to report, like commit on a source build.
	s = serverFor(t, `exit 1`, good)
	body = nil
	rec = get(t, s, "/v1/health")
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if _, present := body["repos_unavailable"]; present {
		t.Errorf("health = %v, want no repos_unavailable when every repo is fine", body)
	}
}

func TestConfigRejectsDuplicateRepoNames(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{Repos: []Repo{{Name: "a", Path: dir}, {Name: "a", Path: dir}}}
	if err := cfg.Normalize(); err == nil {
		t.Error("want an error for duplicate repo names")
	}
}

func TestConfigRejectsUnsafeRepoNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../escape", "a/b", ".."} {
		cfg := &Config{Repos: []Repo{{Name: name, Path: dir}}}
		if err := cfg.Normalize(); err == nil {
			t.Errorf("name %q: want an error, it is not a safe path segment", name)
		}
	}
}
