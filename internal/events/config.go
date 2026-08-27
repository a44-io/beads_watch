package events

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Config is the opt-in "notify" block of the daemon config. When the block is
// absent the daemon behaves exactly as it did before events existed.
type Config struct {
	// URL is the ntfy server, e.g. https://ntfy.example.ts.net.
	URL string `json:"url"`

	// Topic is the firehose every event publishes to. One topic for the whole
	// tailnet: subscribers slice it server-side with ?tags= filters, so more
	// topics would only fragment the stream.
	Topic string `json:"topic"`

	// Token authenticates to the ntfy server. TokenFile wins over Token so the
	// secret can live in a 0600 file instead of the config.
	Token     string `json:"token,omitempty"`
	TokenFile string `json:"token_file,omitempty"`

	// Node names this machine in tags and payloads. Defaults to the lowercased
	// short hostname, so a subscriber's `node-arch` filter is stable across
	// whatever capitalization the kernel reports.
	Node string `json:"node,omitempty"`

	// PollMS is how often each repo's database files are stat'd for change.
	// The mutation log is only queried when an mtime actually moved (plus a
	// once-a-minute safety poll), so this can be aggressive.
	PollMS int `json:"poll_ms,omitempty"`

	// StateFile persists each repo's event cursor across restarts, so a
	// restart neither replays history nor drops events. Defaults to
	// $XDG_STATE_HOME/beads_watch/events-cursor.json.
	StateFile string `json:"state_file,omitempty"`

	// RouteAssignments additionally publishes assignee_changed events to
	// <agent_topic_prefix><assignee>, giving every named agent a personal
	// dispatch topic. Default true.
	RouteAssignments *bool  `json:"route_assignments,omitempty"`
	AgentTopicPrefix string `json:"agent_topic_prefix,omitempty"`

	// Repos restricts watching to a subset of served repo names. Empty means
	// every repo the daemon serves. A name that matches no served repo is a
	// startup error, not a silent no-op.
	Repos []string `json:"repos,omitempty"`

	// SqlitePath is the sqlite3 binary; empty means PATH lookup.
	SqlitePath string `json:"sqlite_path,omitempty"`
}

// validTopic is ntfy's own topic charset. Tags share it here so that a tag is
// always a valid filter value.
var validTopic = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Normalize fills in defaults and rejects configurations that would misbehave
// at publish time rather than at startup.
func (c *Config) Normalize() error {
	c.URL = strings.TrimRight(strings.TrimSpace(c.URL), "/")
	if c.URL == "" {
		return fmt.Errorf("url is required")
	}
	if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
		return fmt.Errorf("url %q must start with http:// or https://", c.URL)
	}
	if !validTopic.MatchString(c.Topic) {
		return fmt.Errorf("topic %q is not a valid ntfy topic ([A-Za-z0-9_-], 1-64 chars)", c.Topic)
	}
	if c.TokenFile != "" {
		raw, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return fmt.Errorf("token_file: %w", err)
		}
		c.Token = strings.TrimSpace(string(raw))
	}
	if c.Node == "" {
		host, err := os.Hostname()
		if err != nil {
			host = "unknown"
		}
		host, _, _ = strings.Cut(host, ".")
		c.Node = strings.ToLower(host)
	}
	if sanitizeTag(c.Node) != c.Node {
		return fmt.Errorf("node %q must be tag-safe ([A-Za-z0-9_-])", c.Node)
	}
	if c.PollMS <= 0 {
		c.PollMS = 1000
	}
	if c.PollMS < 200 {
		c.PollMS = 200
	}
	if c.StateFile == "" {
		c.StateFile = defaultStateFile()
		if c.StateFile == "" {
			return fmt.Errorf("cannot derive a state file path; set state_file")
		}
	}
	if c.RouteAssignments == nil {
		yes := true
		c.RouteAssignments = &yes
	}
	if c.AgentTopicPrefix == "" {
		c.AgentTopicPrefix = "agent-"
	}
	if !validTopic.MatchString(c.AgentTopicPrefix + "x") {
		return fmt.Errorf("agent_topic_prefix %q is not topic-safe", c.AgentTopicPrefix)
	}
	return nil
}

func defaultStateFile() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "beads_watch", "events-cursor.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "beads_watch", "events-cursor.json")
}
