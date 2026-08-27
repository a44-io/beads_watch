package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"beads_watch/internal/brexec"
)

func TestTitles(t *testing.T) {
	cases := []struct {
		ev   Event
		want string
	}{
		{Event{IssueID: "nt-1", EventType: "created", Actor: "goku", IssueTitle: "Fix it"}, "nt-1 created by goku: Fix it"},
		{Event{IssueID: "nt-1", EventType: "closed", Actor: "goku", IssueTitle: "Fix it"}, "nt-1 closed by goku: Fix it"},
		{Event{IssueID: "nt-1", EventType: "reopened", Actor: "goku"}, "nt-1 reopened by goku"},
		{Event{IssueID: "nt-1", EventType: "commented", Actor: "goku"}, "nt-1 commented by goku"},
		{Event{IssueID: "nt-1", EventType: "status_changed", Actor: "goku", OldValue: "open", NewValue: "in_progress"}, "nt-1 open→in_progress by goku"},
		{Event{IssueID: "nt-1", EventType: "priority_changed", Actor: "goku", OldValue: "2", NewValue: "1"}, "nt-1 priority 2→1 by goku"},
		{Event{IssueID: "nt-1", EventType: "assignee_changed", Actor: "goku", NewValue: "agent-x"}, "nt-1 assigned to agent-x by goku"},
		{Event{IssueID: "nt-1", EventType: "assignee_changed", Actor: "goku", OldValue: "agent-x"}, "nt-1 unassigned by goku"},
		{Event{IssueID: "nt-1", EventType: "dependency_added", Actor: "goku"}, "nt-1 dependency_added by goku"},
		{Event{IssueID: "nt-1", EventType: "label_added"}, "nt-1 label_added"},
	}
	for _, c := range cases {
		if got := c.ev.title(); got != c.want {
			t.Errorf("title(%s) = %q, want %q", c.ev.EventType, got, c.want)
		}
	}
}

func TestTags(t *testing.T) {
	ev := Event{
		Node: "arch", Repo: "nt", EventType: "status_changed",
		Actor: "Jolly Ember", OldValue: "open", NewValue: "in_progress",
		AgentName: "CrimsonFox",
	}
	got := strings.Join(ev.tags(), ",")
	want := "ev-status_changed,repo-nt,node-arch,actor-Jolly_Ember,agent-CrimsonFox,from-open,to-in_progress"
	if got != want {
		t.Errorf("tags = %q, want %q", got, want)
	}

	// Non-status events carry no from-/to-, and empty actor no actor tag.
	ev2 := Event{Node: "arch", Repo: "nt", EventType: "closed"}
	if got := strings.Join(ev2.tags(), ","); got != "ev-closed,repo-nt,node-arch" {
		t.Errorf("tags = %q", got)
	}
}

func TestSanitizeTag(t *testing.T) {
	cases := map[string]string{
		"CrimsonFox":   "CrimsonFox",
		"agent name":   "agent_name",
		"a/b@c":        "a_b_c",
		"":             "_",
		"héllo":        "h_llo",
		strings.Repeat("x", 100): strings.Repeat("x", 60),
	}
	for in, want := range cases {
		if got := sanitizeTag(in); got != want {
			t.Errorf("sanitizeTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	ev := Event{Comment: strings.Repeat("a", 5000), IssueTitle: "short"}
	ev.truncate()
	if !ev.Truncated {
		t.Fatal("expected Truncated to be set")
	}
	if len(ev.Comment) >= 5000 || ev.IssueTitle != "short" {
		t.Errorf("truncate: comment len %d, title %q", len(ev.Comment), ev.IssueTitle)
	}
}

func TestConfigNormalize(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("tk_secret\n"), 0o600)

	c := &Config{URL: "https://ntfy.example.com/", Topic: "beads-events", TokenFile: tokenFile}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.URL != "https://ntfy.example.com" {
		t.Errorf("URL not trimmed: %q", c.URL)
	}
	if c.Token != "tk_secret" {
		t.Errorf("token_file not read: %q", c.Token)
	}
	if c.PollMS != 1000 || c.AgentTopicPrefix != "agent-" || !*c.RouteAssignments {
		t.Errorf("defaults wrong: %+v", c)
	}
	if c.Node == "" || sanitizeTag(c.Node) != c.Node {
		t.Errorf("node default not tag-safe: %q", c.Node)
	}

	for _, bad := range []Config{
		{Topic: "beads"},                          // no url
		{URL: "ntfy.example.com", Topic: "beads"}, // no scheme
		{URL: "https://x", Topic: "no spaces!"},   // bad topic
		{URL: "https://x", Topic: "b", TokenFile: "/nonexistent-path-xyz"},
	} {
		bad := bad
		if err := bad.Normalize(); err == nil {
			t.Errorf("Normalize(%+v) should have failed", bad)
		}
	}
}

func TestStateRoundtrip(t *testing.T) {
	state := filepath.Join(t.TempDir(), "cursor.json")
	w := &Watcher{cfg: &Config{StateFile: state}, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	if err := w.loadState(); err != nil {
		t.Fatal(err)
	}
	w.setCursor("nt", 42)
	if err := w.saveState(); err != nil {
		t.Fatal(err)
	}

	w2 := &Watcher{cfg: &Config{StateFile: state}, log: w.log}
	if err := w2.loadState(); err != nil {
		t.Fatal(err)
	}
	if got := w2.cursor("nt"); got != 42 {
		t.Errorf("cursor = %d, want 42", got)
	}

	// Corrupt state must not fail the daemon.
	os.WriteFile(state, []byte("not json"), 0o644)
	w3 := &Watcher{cfg: &Config{StateFile: state}, log: w.log}
	if err := w3.loadState(); err != nil {
		t.Fatal(err)
	}
	if _, ok := w3.peekCursor("nt"); ok {
		t.Error("corrupt state should read as empty")
	}
}

// --- end to end over a real sqlite database --------------------------------

func needSqlite(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not on PATH")
	}
}

func mkdb(t *testing.T, dir string) string {
	t.Helper()
	db := filepath.Join(dir, "beads.db")
	schema := `
CREATE TABLE issues (id TEXT PRIMARY KEY, title TEXT, assignee TEXT);
CREATE TABLE events (id INTEGER PRIMARY KEY AUTOINCREMENT, issue_id TEXT NOT NULL,
  event_type TEXT NOT NULL, actor TEXT NOT NULL DEFAULT '', old_value TEXT, new_value TEXT,
  comment TEXT, created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  agent_name TEXT, harness TEXT, model TEXT);
INSERT INTO issues VALUES ('t-1', 'First issue', '');
`
	if out, err := exec.Command("sqlite3", db, schema).CombinedOutput(); err != nil {
		t.Fatalf("mkdb: %v: %s", err, out)
	}
	return db
}

func addEvent(t *testing.T, db, sql string) {
	t.Helper()
	if out, err := exec.Command("sqlite3", db, sql).CombinedOutput(); err != nil {
		t.Fatalf("addEvent: %v: %s", err, out)
	}
}

type capture struct {
	mu   sync.Mutex
	reqs []capturedReq
	fail int // fail this many requests with 500 before succeeding
}

type capturedReq struct {
	topic, title, tags string
	body               Event
}

func (c *capture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.fail > 0 {
			c.fail--
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		var ev Event
		json.NewDecoder(r.Body).Decode(&ev)
		c.reqs = append(c.reqs, capturedReq{
			topic: strings.TrimPrefix(r.URL.Path, "/"),
			title: r.URL.Query().Get("title"),
			tags:  r.URL.Query().Get("tags"),
			body:  ev,
		})
		w.WriteHeader(http.StatusOK)
	})
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

func testTail(t *testing.T, db string, srvURL string, cap *capture) (*Watcher, *repoTail) {
	t.Helper()
	cfg := &Config{
		URL: srvURL, Topic: "beads-events", Node: "testnode",
		StateFile: filepath.Join(t.TempDir(), "cursor.json"),
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	cfg.Node = "testnode"
	w := &Watcher{
		cfg: cfg,
		log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		sql: &brexec.Runner{Path: "sqlite3", MaxOutput: 8 << 20},
		web: srvClient(),
	}
	if err := w.loadState(); err != nil {
		t.Fatal(err)
	}
	tail := &repoTail{w: w, repo: Repo{Name: "t", Path: filepath.Dir(db)}, db: db,
		hasAttribution: true, hasTitle: true, hasAssignee: true}
	return w, tail
}

func srvClient() *http.Client { return &http.Client{} }

func TestDrainPublishesAndAdvances(t *testing.T) {
	needSqlite(t)
	db := mkdb(t, t.TempDir())
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	w, tail := testTail(t, db, srv.URL, cap)
	ctx := context.Background()

	addEvent(t, db, `INSERT INTO events (issue_id, event_type, actor) VALUES ('t-1','created','goku');`)
	addEvent(t, db, `INSERT INTO events (issue_id, event_type, actor, old_value, new_value) VALUES ('t-1','status_changed','goku','open','closed');`)
	addEvent(t, db, `INSERT INTO events (issue_id, event_type, actor, comment, agent_name) VALUES ('t-1','closed','goku','done','CrimsonFox');`)

	if err := tail.drain(ctx); err != nil {
		t.Fatal(err)
	}
	if cap.count() != 3 {
		t.Fatalf("published %d, want 3", cap.count())
	}
	if got := w.cursor("t"); got != 3 {
		t.Errorf("cursor = %d, want 3", got)
	}

	first := cap.reqs[0]
	if first.topic != "beads-events" || first.body.IssueTitle != "First issue" ||
		first.body.Node != "testnode" || first.body.EventID != 1 {
		t.Errorf("first publish wrong: %+v", first)
	}
	last := cap.reqs[2]
	if last.body.Comment != "done" || last.body.AgentName != "CrimsonFox" ||
		!strings.Contains(last.tags, "agent-CrimsonFox") {
		t.Errorf("last publish wrong: %+v", last)
	}

	// Nothing new: drain publishes nothing and the cursor stays put.
	if err := tail.drain(ctx); err != nil {
		t.Fatal(err)
	}
	if cap.count() != 3 || w.cursor("t") != 3 {
		t.Errorf("idle drain moved things: count=%d cursor=%d", cap.count(), w.cursor("t"))
	}
}

func TestAssignmentRoutesToAgentTopic(t *testing.T) {
	needSqlite(t)
	db := mkdb(t, t.TempDir())
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	w, tail := testTail(t, db, srv.URL, cap)
	addEvent(t, db, `INSERT INTO events (issue_id, event_type, actor, old_value, new_value) VALUES ('t-1','assignee_changed','goku','','CrimsonFox');`)

	if err := tail.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cap.count() != 2 {
		t.Fatalf("published %d, want 2 (firehose + agent topic)", cap.count())
	}
	if cap.reqs[1].topic != "agent-CrimsonFox" {
		t.Errorf("agent topic = %q", cap.reqs[1].topic)
	}
	if !strings.Contains(cap.reqs[1].title, "assigned t-1 in t@testnode") {
		t.Errorf("agent title = %q", cap.reqs[1].title)
	}
	if w.cursor("t") != 1 {
		t.Errorf("cursor = %d", w.cursor("t"))
	}
}

func TestCreateWithAssigneeRoutes(t *testing.T) {
	// br emits only `created` for `br create --assignee X` — no
	// assignee_changed — so routing keys on the issue's current assignee.
	needSqlite(t)
	db := mkdb(t, t.TempDir())
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	_, tail := testTail(t, db, srv.URL, cap)
	addEvent(t, db, `INSERT INTO issues VALUES ('t-2', 'Preassigned work', 'CrimsonFox');`)
	addEvent(t, db, `INSERT INTO events (issue_id, event_type, actor) VALUES ('t-2','created','goku');`)

	if err := tail.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cap.count() != 2 {
		t.Fatalf("published %d, want 2 (firehose + agent topic)", cap.count())
	}
	if cap.reqs[1].topic != "agent-CrimsonFox" {
		t.Errorf("agent topic = %q", cap.reqs[1].topic)
	}
	if cap.reqs[1].body.IssueAssignee != "CrimsonFox" {
		t.Errorf("issue_assignee = %q", cap.reqs[1].body.IssueAssignee)
	}
}

func TestPublishFailureHoldsCursor(t *testing.T) {
	needSqlite(t)
	db := mkdb(t, t.TempDir())
	cap := &capture{fail: 1}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	w, tail := testTail(t, db, srv.URL, cap)
	addEvent(t, db, `INSERT INTO events (issue_id, event_type, actor) VALUES ('t-1','created','goku');`)

	if err := tail.drain(context.Background()); err == nil {
		t.Fatal("drain should fail while ntfy is failing")
	}
	if w.cursor("t") != 0 {
		t.Errorf("cursor advanced past a failed publish: %d", w.cursor("t"))
	}

	// Retry succeeds and delivers the same event: at-least-once.
	if err := tail.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cap.count() != 1 || w.cursor("t") != 1 {
		t.Errorf("after retry: count=%d cursor=%d", cap.count(), w.cursor("t"))
	}
}

func TestRebuiltDatabaseResetsCursor(t *testing.T) {
	needSqlite(t)
	db := mkdb(t, t.TempDir())
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	w, tail := testTail(t, db, srv.URL, cap)
	w.setCursor("t", 500) // as if the old database had 500 events

	if err := tail.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.cursor("t"); got != 0 {
		t.Errorf("cursor = %d, want reset to 0", got)
	}
}

func TestFirstRunStartsAtTip(t *testing.T) {
	needSqlite(t)
	dir := t.TempDir()
	db := mkdb(t, dir)
	addEvent(t, db, `INSERT INTO events (issue_id, event_type, actor) VALUES ('t-1','created','goku');`)
	addEvent(t, db, `INSERT INTO events (issue_id, event_type, actor) VALUES ('t-1','closed','goku');`)

	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	w, tail := testTail(t, db, srv.URL, cap)
	max, err := tail.maxID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if max != 2 {
		t.Fatalf("maxID = %d, want 2", max)
	}
	w.setCursor("t", max)

	if err := tail.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cap.count() != 0 {
		t.Errorf("history replayed on first run: %d publishes", cap.count())
	}

	// The next mutation flows.
	addEvent(t, db, `INSERT INTO events (issue_id, event_type, actor) VALUES ('t-1','reopened','goku');`)
	if err := tail.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cap.count() != 1 || cap.reqs[0].body.EventType != "reopened" {
		t.Errorf("new event not delivered: count=%d", cap.count())
	}
}

func TestSchemaProbe(t *testing.T) {
	needSqlite(t)
	db := mkdb(t, t.TempDir())
	w, tail := testTail(t, db, "http://unused", &capture{})
	_ = w

	cols, err := tail.columns(context.Background(), "events")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"id", "issue_id", "event_type", "agent_name"} {
		if !cols[c] {
			t.Errorf("missing column %q in probe", c)
		}
	}
	if _, err := tail.columns(context.Background(), "nonexistent"); err == nil {
		t.Error("probe of missing table should fail")
	}
}

func TestEventTypesAreExercised(t *testing.T) {
	// The titles above cover every event type br 0.2.22 was observed to emit
	// on a full lifecycle (create, priority, claim, comment, label, dep,
	// assign, defer, undefer, close, reopen). This guard documents the list;
	// a new type still flows through the generic title, it just reads plainer.
	observed := []string{
		"created", "priority_changed", "status_changed", "assignee_changed",
		"commented", "label_added", "dependency_added", "dependency_removed",
		"closed", "reopened",
	}
	for _, typ := range observed {
		ev := Event{IssueID: "x-1", EventType: typ}
		if ev.title() == "" || len(ev.tags()) < 3 {
			t.Errorf("type %q renders empty", typ)
		}
	}
}

var _ = fmt.Sprintf // keep fmt imported if cases shift
