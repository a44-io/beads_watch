package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testServer(t *testing.T, brScript string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	br := filepath.Join(dir, "br")
	if err := os.WriteFile(br, []byte("#!/bin/sh\n"+brScript), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Repos:  []Repo{{Name: "demo", Path: repoDir}},
		BrPath: br,
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg, log), repoDir
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

// TestForgedUserHeaderIsTakenFromProxy documents the boundary: the daemon
// trusts Tailscale-User-Login because the proxy strips a client-supplied one.
// That guarantee is the proxy's, so the daemon must sit behind it.
func TestForgedUserHeaderIsTakenFromProxy(t *testing.T) {
	s, _ := testServer(t, `printf '%s' "$BD_ACTOR"`)

	req := httptest.NewRequest(http.MethodPost, "/v1/repos/demo/br", strings.NewReader(`{"args":["create","x"]}`))
	req.Header.Set("Tailscale-User-Login", "someone@example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if got := rec.Body.String(); got != "someone@example.com" {
		t.Errorf("BD_ACTOR = %q, want the proxy-supplied login", got)
	}
	if got := rec.Header().Get("X-Br-Actor-Source"); got != "tailscale-user-header" {
		t.Errorf("source = %q, want tailscale-user-header", got)
	}
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
