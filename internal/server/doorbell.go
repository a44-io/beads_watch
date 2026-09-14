package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"beads_watch/internal/brexec"
)

// The doorbell is GET /v1/events: a server-sent event per served repo each
// time its JSONL export is rewritten, saying "something changed in <repo>, go
// ask br" and nothing else. It is not the ntfy feed (EVENTS.md): no broker,
// no token, no cursor, no replay, no bead semantics. A client that
// reconnects re-asks br once and is current again.
//
// The one design constraint is what NOT to watch. A read-only `br list`
// opens beads.db-wal, -shm and the lock files O_RDWR and fires a few hundred
// close_write events, so a watch on .beads/ — or a fingerprint that includes
// the WAL — rings on every read, including the daemon's own /br handler
// serving the very client that is listening. That loop ran for three weeks
// in omabeads. Only the JSONL export moves when br mutated something, so the
// fingerprint is the JSONL's size and mtime, and nothing else.

const (
	// doorbellPoll is how often each JSONL is stat'd. A stat a second per repo
	// is what the events tailer already does; fsnotify on the single file is
	// the upgrade if sub-second latency ever matters, not a directory watch.
	doorbellPoll = time.Second
	// doorbellDebounce folds a multi-file export into one ring.
	doorbellDebounce = 250 * time.Millisecond
	// doorbellPing keeps caddy and any idle-timeout proxy from dropping a
	// quiet stream.
	doorbellPing = 30 * time.Second
	// doorbellRetry is how long a repo that cannot be resolved (path missing,
	// br where failing) waits before trying again; same cadence as the events
	// tailer.
	doorbellRetry = time.Minute
	// maxListeners is a soft cap so a misbehaving client cannot hold thousands
	// of connections; one per omabeads instance per node is the real number.
	maxListeners = 64
)

// change is one event on the stream.
type change struct {
	Repo string    `json:"repo"`
	TS   time.Time `json:"ts"`
}

type listener struct {
	repos map[string]bool // nil means every repo
	ch    chan change
}

// doorbell owns the per-repo watchers and the listener registry.
type doorbell struct {
	cfg *Config
	br  *brexec.Runner
	log *slog.Logger

	poll, debounce, ping, retry time.Duration

	mu        sync.Mutex
	listeners map[*listener]struct{}
}

func newDoorbell(cfg *Config, br *brexec.Runner, log *slog.Logger) *doorbell {
	return &doorbell{
		cfg: cfg, br: br, log: log,
		poll: doorbellPoll, debounce: doorbellDebounce, ping: doorbellPing, retry: doorbellRetry,
		listeners: map[*listener]struct{}{},
	}
}

// start begins watching every served repo. Repos whose path is unusable or
// whose br where fails sit out a round and are retried, so one broken repo
// cannot silence the rest.
func (d *doorbell) start(ctx context.Context) {
	for _, r := range d.cfg.Repos {
		go d.watch(ctx, r)
	}
}

func (d *doorbell) watch(ctx context.Context, repo Repo) {
	var path string
	warned := false
	for {
		p, err := d.resolve(ctx, repo)
		if err == nil {
			path = p
			break
		}
		// One warning, then quiet: a missing checkout was already named at
		// startup, and once a minute forever would be the journal noise the
		// events tailer is guilty of.
		if !warned {
			d.log.Warn("doorbell: repo not watchable yet", "repo", repo.Name, "err", err)
			warned = true
		} else {
			d.log.Debug("doorbell: repo still not watchable", "repo", repo.Name, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.retry):
		}
	}
	d.log.Info("doorbell: watching", "repo", repo.Name, "jsonl", path)
	d.watchPath(ctx, repo.Name, path)
}

// resolve asks br where the JSONL export lives — through br, so a
// .beads/redirect resolves here exactly as it does for every other command.
func (d *doorbell) resolve(ctx context.Context, repo Repo) (string, error) {
	if err := repo.Probe(); err != nil {
		return "", err
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := d.br.Run(rctx, brexec.Request{Dir: repo.Path, Args: []string{"--json", "where"}})
	if err != nil {
		return "", fmt.Errorf("br where: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("br where exited %d: %s", res.ExitCode, res.ErrorMessage())
	}
	var where struct {
		JSONLPath string `json:"jsonl_path"`
	}
	if err := json.Unmarshal(res.Stdout, &where); err != nil || where.JSONLPath == "" {
		return "", fmt.Errorf("br where: no jsonl_path in output")
	}
	return where.JSONLPath, nil
}

// fingerprint is the JSONL's size and mtime — and deliberately nothing about
// the database or its WAL; see the file comment. "-" when it does not exist
// yet, which a fresh workspace's does not until the first export.
func fingerprint(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
}

// watchPath rings once per change to the file at path. The first stat is the
// baseline: a daemon coming up does not announce every repo it found.
func (d *doorbell) watchPath(ctx context.Context, name, path string) {
	last := fingerprint(path)
	tick := time.NewTicker(d.poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		sig := fingerprint(path)
		if sig == last {
			continue
		}
		// Debounce: an export that lands in several writes rings once, and
		// the fingerprint recorded is the one after the dust settled, so the
		// next tick does not ring again for the tail of the same write.
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.debounce):
		}
		last = fingerprint(path)
		d.ring(name)
	}
}

// ring fans one change out to every listener that wants this repo. A
// listener whose buffer is full is skipped, not blocked on: the stream is
// at-most-once by contract, and a client that far behind will re-ask br on
// the next event it does receive.
func (d *doorbell) ring(name string) {
	ev := change{Repo: name, TS: time.Now().UTC()}
	d.mu.Lock()
	defer d.mu.Unlock()
	for l := range d.listeners {
		if l.repos != nil && !l.repos[name] {
			continue
		}
		select {
		case l.ch <- ev:
		default:
		}
	}
}

// subscribe registers a listener, or reports that the cap is reached.
func (d *doorbell) subscribe(repos map[string]bool) (*listener, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.listeners) >= maxListeners {
		return nil, false
	}
	l := &listener{repos: repos, ch: make(chan change, 16)}
	d.listeners[l] = struct{}{}
	return l, true
}

func (d *doorbell) unsubscribe(l *listener) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.listeners, l)
}

func (d *doorbell) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.listeners)
}

// handleEvents is GET /v1/events. It never runs br, so it is safe to leave
// open indefinitely and is untouched by timeout_seconds.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	var want map[string]bool
	if q := strings.TrimSpace(r.URL.Query().Get("repo")); q != "" {
		want = map[string]bool{}
		for _, name := range strings.Split(q, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if _, ok := s.cfg.Lookup(name); !ok {
				writeError(w, http.StatusNotFound, "REPO_NOT_FOUND",
					fmt.Sprintf("This node does not serve a repo named %q.", name),
					"GET /v1/repos lists the repos this node serves; ?repo= takes a comma-separated subset of them.")
				return
			}
			want[name] = true
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "STREAM_UNSUPPORTED",
			"This connection cannot stream.", "")
		return
	}

	l, ok := s.bell.subscribe(want)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "TOO_MANY_LISTENERS",
			fmt.Sprintf("This node already has %d event listeners.", maxListeners),
			"One stream per client is plenty; close the others.")
		return
	}
	defer s.bell.unsubscribe(l)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ping := time.NewTicker(s.bell.ping)
	defer ping.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-l.ch:
			body, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: change\ndata: %s\n\n", body)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		}
		flusher.Flush()
	}
}
