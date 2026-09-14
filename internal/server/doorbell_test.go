package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fastBell shortens every interval so a test runs in milliseconds.
func fastBell(s *Server) {
	s.bell.poll = 10 * time.Millisecond
	s.bell.debounce = 20 * time.Millisecond
	s.bell.ping = 50 * time.Millisecond
	s.bell.retry = 20 * time.Millisecond
}

func expectRing(t *testing.T, l *listener, repo string, within time.Duration) change {
	t.Helper()
	select {
	case ev := <-l.ch:
		if ev.Repo != repo {
			t.Fatalf("rang for %q, want %q", ev.Repo, repo)
		}
		return ev
	case <-time.After(within):
		t.Fatalf("no ring for %q within %s", repo, within)
	}
	return change{}
}

func expectSilence(t *testing.T, l *listener, for_ time.Duration) {
	t.Helper()
	select {
	case ev := <-l.ch:
		t.Fatalf("unexpected ring: %+v", ev)
	case <-time.After(for_):
	}
}

func TestDoorbellRingsOncePerExport(t *testing.T) {
	s, _ := testServer(t, `echo`)
	fastBell(s)
	jsonl := filepath.Join(t.TempDir(), "issues.jsonl")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, _ := s.bell.subscribe(nil)
	defer s.bell.unsubscribe(l)
	go s.bell.watchPath(ctx, "demo", jsonl)
	time.Sleep(30 * time.Millisecond) // baseline stat: absent

	// The file appearing is a change.
	os.WriteFile(jsonl, []byte(`{"id":"t-1"}`+"\n"), 0o644)
	ev := expectRing(t, l, "demo", time.Second)
	if ev.TS.IsZero() || ev.TS.Location() != time.UTC {
		t.Errorf("ts = %v, want a UTC timestamp", ev.TS)
	}
	expectSilence(t, l, 100*time.Millisecond)

	// Two writes inside the debounce window ring once.
	os.WriteFile(jsonl, []byte(`{"id":"t-1"}`+"\n"+`{"id":"t-2"}`+"\n"), 0o644)
	time.Sleep(5 * time.Millisecond)
	os.WriteFile(jsonl, []byte(`{"id":"t-1"}`+"\n"+`{"id":"t-2"}`+"\n"+`{"id":"t-3"}`+"\n"), 0o644)
	expectRing(t, l, "demo", time.Second)
	expectSilence(t, l, 100*time.Millisecond)
}

// TestDoorbellWatchesOnlyTheJSONL pins the design constraint: the database
// and its WAL move on every read, so they must never be part of the
// fingerprint, or the daemon's own /br handler rings the bell.
func TestDoorbellWatchesOnlyTheJSONL(t *testing.T) {
	s, _ := testServer(t, `echo`)
	fastBell(s)
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "issues.jsonl")
	os.WriteFile(jsonl, []byte("{}\n"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, _ := s.bell.subscribe(nil)
	defer s.bell.unsubscribe(l)
	go s.bell.watchPath(ctx, "demo", jsonl)
	time.Sleep(30 * time.Millisecond)

	for _, f := range []string{"beads.db", "beads.db-wal", "beads.db-shm", ".write.lock"} {
		os.WriteFile(filepath.Join(dir, f), []byte(strings.Repeat("x", 100)), 0o644)
	}
	expectSilence(t, l, 100*time.Millisecond)

	// A rewrite that leaves size and mtime alone is not a change either:
	// the bell is a fingerprint, not a timer. Staged and renamed into place
	// so the watcher never sees an intermediate mtime.
	fi, _ := os.Stat(jsonl)
	staged := filepath.Join(dir, "staged")
	os.WriteFile(staged, []byte("[]\n"), 0o644) // same 3 bytes
	os.Chtimes(staged, fi.ModTime(), fi.ModTime())
	os.Rename(staged, jsonl)
	expectSilence(t, l, 100*time.Millisecond)
}

func TestDoorbellFilterAndFanout(t *testing.T) {
	s, _ := testServer(t, `echo`)
	all, _ := s.bell.subscribe(nil)
	onlyA, _ := s.bell.subscribe(map[string]bool{"a": true})
	defer s.bell.unsubscribe(all)
	defer s.bell.unsubscribe(onlyA)

	s.bell.ring("b")
	expectRing(t, all, "b", time.Second)
	expectSilence(t, onlyA, 20*time.Millisecond)
	s.bell.ring("a")
	expectRing(t, all, "a", time.Second)
	expectRing(t, onlyA, "a", time.Second)

	// A listener that stopped reading fills its buffer and is then skipped,
	// never blocked on: ring must return, and at-most-once is the contract.
	stuck, _ := s.bell.subscribe(nil)
	defer s.bell.unsubscribe(stuck)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 40; i++ {
			s.bell.ring("a")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ring blocked on a listener that is not reading")
	}
	if n := len(stuck.ch); n != cap(stuck.ch) {
		t.Errorf("stuck listener buffered %d, want a full buffer of %d then drops", n, cap(stuck.ch))
	}
}

// sse reads one server-sent event (or comment) from the stream: the lines
// up to a blank one.
func sse(t *testing.T, r *bufio.Reader) []string {
	t.Helper()
	var lines []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended: %v (got %q so far)", err, lines)
		}
		line = strings.TrimRight(line, "\n")
		if line == "" {
			return lines
		}
		lines = append(lines, line)
	}
}

func openStream(t *testing.T, base, path string) (*http.Response, *bufio.Reader) {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatal(err)
	}
	return resp, bufio.NewReader(resp.Body)
}

func TestEventsStream(t *testing.T) {
	s, _ := testServer(t, `echo`)
	fastBell(s)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, rd := openStream(t, ts.URL, "/v1/events")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for k, want := range map[string]string{
		"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got := sse(t, rd); len(got) != 1 || got[0] != ": connected" {
		t.Errorf("preamble = %q, want ': connected'", got)
	}

	// The registration exists before the preamble returns, so a ring now
	// reaches this client.
	s.bell.ring("demo")
	got := sse(t, rd)
	if len(got) != 2 || got[0] != "event: change" || !strings.HasPrefix(got[1], "data: ") {
		t.Fatalf("event = %q, want 'event: change' + 'data: …'", got)
	}
	var ev struct {
		Repo string `json:"repo"`
		TS   string `json:"ts"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got[1], "data: ")), &ev); err != nil {
		t.Fatalf("data is not JSON: %v", err)
	}
	if ev.Repo != "demo" {
		t.Errorf("repo = %q, want demo, the name /v1/repos lists", ev.Repo)
	}
	if _, err := time.Parse(time.RFC3339Nano, ev.TS); err != nil {
		t.Errorf("ts = %q, want RFC3339: %v", ev.TS, err)
	}

	// Quiet streams get a ping.
	if got := sse(t, rd); len(got) != 1 || got[0] != ": ping" {
		t.Errorf("idle = %q, want ': ping'", got)
	}
}

func TestEventsFilterAndUnknownRepo(t *testing.T) {
	s, _ := testServer(t, `echo`)
	fastBell(s)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/events?repo=nope")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 || !strings.Contains(string(body), "REPO_NOT_FOUND") || !strings.Contains(string(body), "beads_watch") {
		t.Errorf("status %d body %s, want the daemon's 404 envelope", resp.StatusCode, body)
	}

	resp, rd := openStream(t, ts.URL, "/v1/events?repo=demo")
	defer resp.Body.Close()
	sse(t, rd) // preamble
	s.bell.ring("other")
	s.bell.ring("demo")
	got := sse(t, rd)
	if len(got) != 2 || !strings.Contains(got[1], `"repo":"demo"`) {
		t.Errorf("filtered stream got %q, want only demo", got)
	}
}

// TestEventsListenersAreFreed: a client that goes away must take its
// registration and goroutine with it.
func TestEventsListenersAreFreed(t *testing.T) {
	s, _ := testServer(t, `echo`)
	fastBell(s)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	before := runtime.NumGoroutine()
	for i := 0; i < 25; i++ {
		resp, rd := openStream(t, ts.URL, "/v1/events")
		sse(t, rd)
		if s.bell.count() != 1 {
			t.Fatalf("cycle %d: %d listeners registered, want 1", i, s.bell.count())
		}
		resp.Body.Close()
		deadline := time.Now().Add(time.Second)
		for s.bell.count() != 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if s.bell.count() != 0 {
			t.Fatalf("cycle %d: listener not freed after disconnect", i)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+3 {
		t.Errorf("goroutines %d → %d after 25 connect/disconnect cycles; something leaks", before, after)
	}
}

func TestEventsListenerCap(t *testing.T) {
	s, _ := testServer(t, `echo`)
	var held []*listener
	for i := 0; i < maxListeners; i++ {
		l, ok := s.bell.subscribe(nil)
		if !ok {
			t.Fatalf("subscribe %d refused below the cap", i)
		}
		held = append(held, l)
	}
	defer func() {
		for _, l := range held {
			s.bell.unsubscribe(l)
		}
	}()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 || !strings.Contains(string(body), "TOO_MANY_LISTENERS") {
		t.Errorf("status %d body %s, want 503 TOO_MANY_LISTENERS at the cap", resp.StatusCode, body)
	}
}

// TestDoorbellWithRealBr is the acceptance test end to end: a workspace made
// by the real br, resolved through `br where`, rung by a mutation through
// /v1/repos/{repo}/br and silent for a read through the same route.
func TestDoorbellWithRealBr(t *testing.T) {
	br, err := exec.LookPath("br")
	if err != nil {
		t.Skip("br not on PATH")
	}
	repo := t.TempDir()
	init := exec.Command(br, "init", "--prefix", "dbt")
	init.Dir = repo
	if out, err := init.CombinedOutput(); err != nil {
		t.Skipf("br init failed: %v\n%s", err, out)
	}

	cfg := &Config{Repos: []Repo{{Name: "live", Path: repo}}, BrPath: br}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	fastBell(s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, rd := openStream(t, ts.URL, "/v1/events")
	defer resp.Body.Close()
	sse(t, rd)
	events := frames(rd) // pings are filtered out; only "event: change" frames arrive
	// Let the watcher resolve `br where` and take its baseline.
	time.Sleep(300 * time.Millisecond)

	// A read must not ring, even though br touches the WAL and lock files.
	if rec := post(t, s, "live", `{"args":["list"]}`); rec.Code != 200 || rec.Header().Get("X-Br-Exit") != "0" {
		t.Fatalf("br list: status %d exit %q; body %s", rec.Code, rec.Header().Get("X-Br-Exit"), rec.Body.String())
	}
	select {
	case ev := <-events:
		t.Fatalf("a read-only br list rang the bell: %q", ev)
	case <-time.After(300 * time.Millisecond):
	}

	// A mutation exports the JSONL and rings exactly once.
	if rec := post(t, s, "live", `{"args":["create","doorbell test","-p","3"]}`); rec.Header().Get("X-Br-Exit") != "0" {
		t.Fatalf("br create: exit %q; body %s", rec.Header().Get("X-Br-Exit"), rec.Body.String())
	}
	select {
	case ev := <-events:
		if !strings.Contains(ev[1], `"repo":"live"`) {
			t.Fatalf("rang for the wrong repo: %q", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("br create did not ring the bell within 3s")
	}
	select {
	case ev := <-events:
		t.Fatalf("second ring for one create: %q", ev)
	case <-time.After(300 * time.Millisecond):
	}
}

// frames feeds every "event:" frame on the stream into a channel, dropping
// comments (": ping"), so a test can select on real events with a timeout.
func frames(rd *bufio.Reader) <-chan []string {
	ch := make(chan []string, 16)
	go func() {
		defer close(ch)
		var lines []string
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\n")
			if line != "" {
				lines = append(lines, line)
				continue
			}
			if len(lines) > 0 && strings.HasPrefix(lines[0], "event:") {
				ch <- lines
			}
			lines = nil
		}
	}()
	return ch
}
