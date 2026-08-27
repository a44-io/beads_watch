// Package events tails each served repo's beads mutation log and publishes
// every event to ntfy, categorized by type, repo, node, and actor.
//
// It never infers an event. br records every mutation as a typed row in the
// workspace's events table (the same rows `br audit log` prints), so this
// package only replays that audit trail through a monotonic id cursor. If we
// never decide what happened ourselves, our answer cannot drift from br's.
//
// Reads go through the sqlite3 CLI, read-only, keeping the daemon's
// zero-dependency and pipe-to-a-real-binary character.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"beads_watch/internal/brexec"
)

const (
	// forcePoll is the safety net: even with no observed file change, the
	// cursor query runs this often so a missed mtime cannot silence the feed.
	forcePoll = time.Minute

	// batchLimit bounds one query so a huge import cannot balloon memory. The
	// drain loop keeps querying until a batch comes back short.
	batchLimit = 200

	queryTimeout   = 10 * time.Second
	publishTimeout = 15 * time.Second

	// resolveRetry is how long a repo that failed to resolve (br missing, no
	// .beads yet, schema too old) waits before trying again. A broken repo
	// must never take the daemon down with it — it just sits out a round.
	resolveRetry = time.Minute
)

// Repo is one watched workspace, named the same way the server names it.
type Repo struct {
	Name string
	Path string
}

// Watcher owns the per-repo tails and the shared cursor state.
type Watcher struct {
	cfg *Config
	log *slog.Logger
	br  *brexec.Runner
	sql *brexec.Runner
	web *http.Client

	mu      sync.Mutex
	cursors map[string]int64
}

// Start loads cursor state and begins tailing every repo. Startup errors are
// config-shaped (unreadable state file directory); per-repo trouble is logged
// and retried instead of returned, so one bad repo cannot stop the rest.
func Start(ctx context.Context, log *slog.Logger, cfg *Config, repos []Repo, brPath string) (*Watcher, error) {
	sqlBin := cfg.SqlitePath
	if sqlBin == "" {
		sqlBin = "sqlite3"
	}
	w := &Watcher{
		cfg: cfg,
		log: log,
		br:  &brexec.Runner{Path: brPath, MaxOutput: 8 << 20},
		sql: &brexec.Runner{Path: sqlBin, MaxOutput: 8 << 20},
		web: &http.Client{Timeout: publishTimeout},
	}
	if err := w.loadState(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.StateFile), 0o755); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	for _, r := range repos {
		go w.watchRepo(ctx, r)
	}
	return w, nil
}

// --- per-repo tail ---------------------------------------------------------

type repoTail struct {
	w    *Watcher
	repo Repo

	db    string
	jsonl string

	// hasAttribution, hasTitle, and hasAssignee are schema facts probed at
	// startup, so the query matches what this workspace's br version wrote.
	hasAttribution bool
	hasTitle       bool
	hasAssignee    bool

	lastSig  string
	lastPoll time.Time
}

func (w *Watcher) watchRepo(ctx context.Context, repo Repo) {
	t := &repoTail{w: w, repo: repo}
	for {
		if err := t.resolve(ctx); err == nil {
			break
		} else {
			w.log.Warn("events: repo not watchable yet", "repo", repo.Name, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(resolveRetry):
		}
	}
	w.log.Info("events: tailing", "repo", repo.Name, "db", t.db, "cursor", w.cursor(repo.Name))

	tick := time.NewTicker(time.Duration(w.cfg.PollMS) * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		sig := t.signature()
		forced := time.Since(t.lastPoll) >= forcePoll
		if sig == t.lastSig && !forced {
			continue
		}
		if err := t.drain(ctx); err != nil {
			// Publish or query failure: the cursor did not advance past the
			// failed event, so the next tick retries it. At-least-once.
			w.log.Warn("events: drain failed, will retry", "repo", repo.Name, "err", err)
			continue
		}
		t.lastSig = sig
		t.lastPoll = time.Now()
	}
}

// resolve asks br where the workspace database lives — through br, so a
// .beads/redirect resolves here exactly as it does for every other command —
// then probes the schema this workspace actually has.
func (t *repoTail) resolve(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	res, err := t.w.br.Run(rctx, brexec.Request{Dir: t.repo.Path, Args: []string{"--json", "where"}})
	if err != nil {
		return fmt.Errorf("br where: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("br where exited %d: %s", res.ExitCode, firstLine(res.Stderr, res.Stdout))
	}
	var where struct {
		DatabasePath string `json:"database_path"`
		JSONLPath    string `json:"jsonl_path"`
	}
	if err := json.Unmarshal(res.Stdout, &where); err != nil || where.DatabasePath == "" {
		return fmt.Errorf("br where: unparseable output")
	}
	t.db = where.DatabasePath
	t.jsonl = where.JSONLPath

	eventCols, err := t.columns(ctx, "events")
	if err != nil {
		return err
	}
	for _, req := range []string{"id", "issue_id", "event_type", "actor", "old_value", "new_value", "comment", "created_at"} {
		if !eventCols[req] {
			return fmt.Errorf("events table lacks column %q; br too old for the feed", req)
		}
	}
	t.hasAttribution = eventCols["agent_name"] && eventCols["harness"] && eventCols["model"]
	issueCols, err := t.columns(ctx, "issues")
	if err != nil {
		return err
	}
	t.hasTitle = issueCols["title"]
	t.hasAssignee = issueCols["assignee"]

	// First sight of this repo: start at the tip. Blasting the entire
	// historical log at subscribers on first deploy helps nobody.
	if _, ok := t.w.peekCursor(t.repo.Name); !ok {
		max, err := t.maxID(ctx)
		if err != nil {
			return err
		}
		t.w.setCursor(t.repo.Name, max)
		if err := t.w.saveState(); err != nil {
			return err
		}
	}
	return nil
}

func (t *repoTail) columns(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := t.query(ctx, fmt.Sprintf("SELECT name FROM pragma_table_info('%s')", table))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no %s table in %s", table, t.db)
	}
	cols := make(map[string]bool, len(rows))
	for _, r := range rows {
		if name, ok := r["name"].(string); ok {
			cols[name] = true
		}
	}
	return cols, nil
}

func (t *repoTail) maxID(ctx context.Context) (int64, error) {
	rows, err := t.query(ctx, "SELECT COALESCE(MAX(id),0) AS max_id FROM events")
	if err != nil {
		return 0, err
	}
	if len(rows) == 1 {
		if f, ok := rows[0]["max_id"].(float64); ok {
			return int64(f), nil
		}
	}
	return 0, fmt.Errorf("MAX(id) query returned no usable row")
}

// signature is a cheap change detector over the files a mutation touches. Any
// difference (or its once-a-minute absence) triggers a real cursor query; a
// spurious trigger costs one cheap SELECT that finds nothing.
func (t *repoTail) signature() string {
	var b strings.Builder
	for _, p := range []string{t.db, t.db + "-wal", t.jsonl} {
		if fi, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%d:%d;", fi.Size(), fi.ModTime().UnixNano())
		} else {
			b.WriteString("-;")
		}
	}
	return b.String()
}

// drain publishes everything past the cursor, oldest first, advancing the
// cursor only past events whose publishes all succeeded.
func (t *repoTail) drain(ctx context.Context) error {
	for {
		rows, err := t.pending(ctx)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return t.checkRegression(ctx)
		}
		for _, r := range rows {
			ev := t.event(r)
			if err := t.w.publishEvent(ctx, ev); err != nil {
				return err
			}
			t.w.setCursor(t.repo.Name, ev.EventID)
		}
		if err := t.w.saveState(); err != nil {
			return err
		}
		if len(rows) < batchLimit {
			return nil
		}
	}
}

// checkRegression handles a rebuilt database: if the log's MAX(id) fell below
// the cursor (re-import, restored backup), the feed would otherwise go silent
// forever. Reset to the new tip and say so.
func (t *repoTail) checkRegression(ctx context.Context) error {
	cursor := t.w.cursor(t.repo.Name)
	if cursor == 0 {
		return nil
	}
	max, err := t.maxID(ctx)
	if err != nil {
		return err
	}
	if max < cursor {
		t.w.log.Warn("events: database rebuilt, cursor reset", "repo", t.repo.Name, "old", cursor, "new", max)
		t.w.setCursor(t.repo.Name, max)
		return t.w.saveState()
	}
	return nil
}

func (t *repoTail) pending(ctx context.Context) ([]map[string]any, error) {
	attr := "'' AS agent_name, '' AS harness, '' AS model"
	if t.hasAttribution {
		attr = "COALESCE(e.agent_name,'') AS agent_name, COALESCE(e.harness,'') AS harness, COALESCE(e.model,'') AS model"
	}
	title := "'' AS issue_title"
	if t.hasTitle {
		title = "COALESCE(i.title,'') AS issue_title"
	}
	// issue_assignee is the issue's CURRENT assignee, not part of the audit
	// row. It exists so a `created --assignee X` issue can be routed to X's
	// dispatch topic even though br emits no assignee_changed for it.
	assignee := "'' AS issue_assignee"
	if t.hasAssignee {
		assignee = "COALESCE(i.assignee,'') AS issue_assignee"
	}
	join := ""
	if t.hasTitle || t.hasAssignee {
		join = "LEFT JOIN issues i ON i.id = e.issue_id"
	}
	q := fmt.Sprintf(`SELECT e.id, e.issue_id, e.event_type,
		COALESCE(e.actor,'') AS actor, COALESCE(e.old_value,'') AS old_value,
		COALESCE(e.new_value,'') AS new_value, COALESCE(e.comment,'') AS comment,
		COALESCE(e.created_at,'') AS created_at, %s, %s, %s
		FROM events e %s WHERE e.id > %d ORDER BY e.id LIMIT %d`,
		attr, title, assignee, join, t.w.cursor(t.repo.Name), batchLimit)
	return t.query(ctx, q)
}

// query runs one read-only SQL statement through the sqlite3 CLI. `-json`
// prints an array of objects, or nothing at all for zero rows.
func (t *repoTail) query(ctx context.Context, sql string) ([]map[string]any, error) {
	rctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	res, err := t.w.sql.Run(rctx, brexec.Request{
		Dir:  filepath.Dir(t.db),
		Args: []string{"-readonly", "-json", "-cmd", ".timeout 2000", t.db, sql},
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite3: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("sqlite3 exited %d: %s", res.ExitCode, firstLine(res.Stderr, res.Stdout))
	}
	out := strings.TrimSpace(string(res.Stdout))
	if out == "" {
		return nil, nil
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("sqlite3 output unparseable: %w", err)
	}
	return rows, nil
}

func (t *repoTail) event(r map[string]any) Event {
	ev := Event{
		V:          1,
		Node:       t.w.cfg.Node,
		Repo:       t.repo.Name,
		EventID:       asInt64(r["id"]),
		IssueID:       asString(r["issue_id"]),
		IssueTitle:    asString(r["issue_title"]),
		IssueAssignee: asString(r["issue_assignee"]),
		EventType:  asString(r["event_type"]),
		Actor:      asString(r["actor"]),
		OldValue:   asString(r["old_value"]),
		NewValue:   asString(r["new_value"]),
		Comment:    asString(r["comment"]),
		CreatedAt:  asString(r["created_at"]),
		AgentName:  asString(r["agent_name"]),
		Harness:    asString(r["harness"]),
		Model:      asString(r["model"]),
	}
	ev.truncate()
	return ev
}

// --- cursor state ----------------------------------------------------------

type stateDoc struct {
	Repos map[string]int64 `json:"repos"`
}

func (w *Watcher) loadState() error {
	w.cursors = map[string]int64{}
	raw, err := os.ReadFile(w.cfg.StateFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("state file: %w", err)
	}
	var doc stateDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		// A corrupt state file must not stop the daemon; starting each repo at
		// its tip loses no future events, only suppresses a replay.
		w.log.Warn("events: state file unreadable, starting at tips", "path", w.cfg.StateFile, "err", err)
		return nil
	}
	if doc.Repos != nil {
		w.cursors = doc.Repos
	}
	return nil
}

func (w *Watcher) saveState() error {
	w.mu.Lock()
	raw, err := json.MarshalIndent(stateDoc{Repos: w.cursors}, "", "  ")
	w.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := w.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, w.cfg.StateFile)
}

func (w *Watcher) cursor(repo string) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cursors[repo]
}

func (w *Watcher) peekCursor(repo string) (int64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, ok := w.cursors[repo]
	return c, ok
}

func (w *Watcher) setCursor(repo string, id int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cursors[repo] = id
}

// --- small helpers ---------------------------------------------------------

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt64(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
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
