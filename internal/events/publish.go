package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/a44-io/beads_watch/internal/brexec"
)

// Event is the JSON body of every published message: the audit row br wrote,
// plus where it came from. Version 1; additive changes only.
type Event struct {
	V          int    `json:"v"`
	Node       string `json:"node"`
	Repo       string `json:"repo"`
	EventID    int64  `json:"event_id"`
	IssueID    string `json:"issue_id"`
	IssueTitle string `json:"issue_title,omitempty"`
	// IssueAssignee is the issue's assignee at publish time — state, not part
	// of the audit row — carried so created-with-assignee can be dispatched.
	IssueAssignee string `json:"issue_assignee,omitempty"`
	EventType     string `json:"event_type"`
	Actor      string `json:"actor,omitempty"`
	OldValue   string `json:"old_value,omitempty"`
	NewValue   string `json:"new_value,omitempty"`
	Comment    string `json:"comment,omitempty"`
	CreatedAt  string `json:"created_at"`
	AgentName  string `json:"agent_name,omitempty"`
	Harness    string `json:"harness,omitempty"`
	Model      string `json:"model,omitempty"`
	// Truncated is set when a long field was cut to fit ntfy's message cap.
	// The full row is still in the repo: `br audit log <issue_id>`.
	Truncated bool `json:"truncated,omitempty"`
}

// fieldCap keeps the whole body far under ntfy's default 4KB message limit
// even with several long fields at once.
const fieldCap = 700

func (e *Event) truncate() {
	for _, f := range []*string{&e.IssueTitle, &e.OldValue, &e.NewValue, &e.Comment} {
		if len(*f) > fieldCap {
			*f = string([]rune(*f)[:fieldCap/4*3]) // rune-safe, roughly fieldCap bytes for ASCII-heavy text
			e.Truncated = true
		}
	}
}

// publishEvent sends one event to the firehose topic and, for assignments,
// to the assignee's personal dispatch topic. Both must succeed before the
// cursor may advance past this event; a duplicate on retry is fine (consumers
// dedup on event_id), a silently lost assignment is not.
func (w *Watcher) publishEvent(ctx context.Context, ev Event) error {
	if err := w.publish(ctx, w.cfg.Topic, ev.title(), ev.tags(), ev); err != nil {
		return err
	}
	// Assignments dispatch to the assignee's personal topic. br emits no
	// assignee_changed for `create --assignee X`, so created events route on
	// the issue's current assignee instead.
	target := ""
	switch ev.EventType {
	case "assignee_changed":
		target = ev.NewValue
	case "created":
		target = ev.IssueAssignee
	}
	if *w.cfg.RouteAssignments && target != "" {
		topic := w.cfg.AgentTopicPrefix + sanitizeTag(target)
		title := fmt.Sprintf("assigned %s in %s@%s", ev.IssueID, ev.Repo, ev.Node)
		if ev.IssueTitle != "" {
			title += ": " + ev.IssueTitle
		}
		if err := w.publish(ctx, topic, title, ev.tags(), ev); err != nil {
			return fmt.Errorf("agent topic %s: %w", topic, err)
		}
	}
	w.log.Debug("events: published", "repo", ev.Repo, "id", ev.EventID, "type", ev.EventType)
	return nil
}

// publish POSTs one message. Title and tags travel as query parameters, which
// ntfy URL-decodes as UTF-8 — headers would mangle non-ASCII issue titles.
func (w *Watcher) publish(ctx context.Context, topic, title string, tags []string, ev Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("title", title)
	q.Set("tags", strings.Join(tags, ","))
	u := w.cfg.URL + "/" + topic + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if w.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+w.cfg.Token)
	}
	resp, err := w.web.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		peek, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("ntfy %s: %s: %s", topic, resp.Status, brexec.Excerpt(peek))
	}
	return nil
}

// tags is the filter surface of the whole contract. Every value is prefixed
// with its dimension (ev-, repo-, node-, actor-, agent-, from-, to-) so a
// subscriber's ?tags= filter can never collide across dimensions — a repo
// named "closed" must not match an event-type filter.
func (e *Event) tags() []string {
	tags := []string{
		"ev-" + sanitizeTag(e.EventType),
		"repo-" + sanitizeTag(e.Repo),
		"node-" + sanitizeTag(e.Node),
	}
	if e.Actor != "" {
		tags = append(tags, "actor-"+sanitizeTag(e.Actor))
	}
	if e.AgentName != "" {
		tags = append(tags, "agent-"+sanitizeTag(e.AgentName))
	}
	if e.EventType == "status_changed" {
		if e.OldValue != "" {
			tags = append(tags, "from-"+sanitizeTag(e.OldValue))
		}
		if e.NewValue != "" {
			tags = append(tags, "to-"+sanitizeTag(e.NewValue))
		}
	}
	return tags
}

// title renders the one-line human summary shown by phones and `ntfy sub`.
// Machines should ignore it and read the JSON body.
func (e *Event) title() string {
	by := ""
	if e.Actor != "" {
		by = " by " + e.Actor
	}
	withTitle := func(s string) string {
		if e.IssueTitle != "" {
			return s + ": " + e.IssueTitle
		}
		return s
	}
	switch e.EventType {
	case "created":
		return withTitle(e.IssueID + " created" + by)
	case "closed":
		return withTitle(e.IssueID + " closed" + by)
	case "reopened":
		return withTitle(e.IssueID + " reopened" + by)
	case "commented":
		return e.IssueID + " commented" + by
	case "status_changed":
		return fmt.Sprintf("%s %s→%s%s", e.IssueID, e.OldValue, e.NewValue, by)
	case "priority_changed":
		return fmt.Sprintf("%s priority %s→%s%s", e.IssueID, e.OldValue, e.NewValue, by)
	case "assignee_changed":
		if e.NewValue == "" {
			return e.IssueID + " unassigned" + by
		}
		return e.IssueID + " assigned to " + e.NewValue + by
	default:
		return e.IssueID + " " + e.EventType + by
	}
}

// sanitizeTag maps arbitrary text into ntfy's topic/tag charset, so an actor
// or status value can never break a topic URL or a tag filter.
func sanitizeTag(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 60 {
		out = out[:60]
	}
	if out == "" {
		out = "_"
	}
	return out
}
