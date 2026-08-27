package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"beads_watch/internal/events"
)

// Repo is one beads workspace this node serves.
type Repo struct {
	// Name is the URL path segment. It is an alias, not a filesystem fact, so
	// repos can be renamed on the wire without moving anything on disk.
	Name string `json:"name"`
	// Path is the directory br runs in. br walks up from here to find .beads,
	// which is what lets a .beads/redirect file point somewhere else entirely.
	Path string `json:"path"`
}

// Config is the daemon's whole configuration.
type Config struct {
	Socket string `json:"socket"`
	Repos  []Repo `json:"repos"`

	// Allow is the set of permitted br subcommands, matched against the first
	// token of args. Empty means DefaultAllow.
	Allow []string `json:"allow,omitempty"`

	BrPath         string `json:"br_path,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	LockTimeoutMS  int    `json:"lock_timeout_ms,omitempty"`
	MaxOutputBytes int    `json:"max_output_bytes,omitempty"`

	// InjectJSON adds --json when the caller did not pick a format. Callers
	// that want TOON or human text just pass --format themselves.
	InjectJSON *bool `json:"inject_json,omitempty"`

	// Notify, when present, tails every served repo's beads mutation log and
	// publishes typed events to ntfy. Absent means the feature is off and the
	// daemon behaves exactly as it did before events existed.
	Notify *events.Config `json:"notify,omitempty"`
}

// DefaultAllow is the br surface reachable over the network out of the box:
// the read commands, plus the ordinary create/claim/close workflow.
//
// Deliberately absent, and why — each of these is available by adding it to
// `allow` in the config file, which is a decision someone makes on purpose:
//
//	upgrade    replaces the br binary — remote code execution
//	init       creates new workspaces on the host
//	delete     destructive; writes tombstones
//	doctor     --repair mutates the database
//	config     rewrites workspace configuration
//	history    manipulates local backups
//	completions/help  shell plumbing, nothing to serve
var DefaultAllow = []string{
	"blocked", "capabilities", "changelog", "close", "comments",
	"coordination", "count", "create", "defer", "dep", "epic", "graph",
	"info", "label", "lint", "list", "orphans", "q", "query", "ready",
	"reopen", "robot-docs", "scheduler", "schema", "search", "show",
	"stale", "stats", "status", "sync", "undefer", "update", "vcs-status",
	"version", "where",
}

// DeniedFlags are arguments refused anywhere in argv, because each one breaks
// a promise the URL makes.
//
//	--db     picks a different workspace than the repo in the path — the
//	         response would be correctly formed and about the wrong repo
//	--actor  forges the audit trail the forwarded identity exists to record
var DeniedFlags = []string{"--db", "--actor"}

var validRepoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// DefaultSocket is the unix socket tailscale serve proxies to.
func DefaultSocket() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "beads_watch.sock")
	}
	return "/tmp/beads_watch.sock"
}

// DefaultConfigPath is where the daemon looks when --config is not given.
func DefaultConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "beads_watch", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "beads_watch", "config.json")
}

// LoadConfig reads and validates a config file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.Normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// Normalize fills in defaults and rejects configurations that would misbehave
// at request time rather than at startup.
func (c *Config) Normalize() error {
	if c.Socket == "" {
		c.Socket = DefaultSocket()
	}
	if c.TimeoutSeconds <= 0 {
		c.TimeoutSeconds = 30
	}
	if c.LockTimeoutMS <= 0 {
		// Agent swarms write these repos concurrently. Waiting on .write.lock
		// is cheaper and far kinder than a client retry loop.
		c.LockTimeoutMS = 5000
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = 8 << 20
	}
	if c.InjectJSON == nil {
		yes := true
		c.InjectJSON = &yes
	}
	if len(c.Allow) == 0 {
		c.Allow = append([]string(nil), DefaultAllow...)
	}
	if len(c.Repos) == 0 {
		return fmt.Errorf("no repos configured")
	}

	seen := make(map[string]string, len(c.Repos))
	for i := range c.Repos {
		r := &c.Repos[i]
		if r.Path == "" {
			return fmt.Errorf("repo %q: path is required", r.Name)
		}
		abs, err := filepath.Abs(r.Path)
		if err != nil {
			return fmt.Errorf("repo %q: %w", r.Name, err)
		}
		r.Path = filepath.Clean(abs)
		if r.Name == "" {
			r.Name = filepath.Base(r.Path)
		}
		if !validRepoName.MatchString(r.Name) {
			return fmt.Errorf("repo name %q is not a valid path segment", r.Name)
		}
		if prev, dup := seen[r.Name]; dup {
			return fmt.Errorf("repo name %q used twice (%s and %s)", r.Name, prev, r.Path)
		}
		seen[r.Name] = r.Path

		info, err := os.Stat(r.Path)
		if err != nil {
			return fmt.Errorf("repo %q: %w", r.Name, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("repo %q: %s is not a directory", r.Name, r.Path)
		}
	}

	if c.Notify != nil {
		if err := c.Notify.Normalize(); err != nil {
			return fmt.Errorf("notify: %w", err)
		}
		for _, name := range c.Notify.Repos {
			if _, ok := seen[name]; !ok {
				return fmt.Errorf("notify: repo %q is not served by this daemon", name)
			}
		}
	}
	return nil
}

// NotifyRepos is the subset of served repos the notify block watches: the
// named subset when one is given, otherwise every served repo.
func (c *Config) NotifyRepos() []events.Repo {
	if c.Notify == nil {
		return nil
	}
	want := map[string]bool{}
	for _, name := range c.Notify.Repos {
		want[name] = true
	}
	var out []events.Repo
	for _, r := range c.Repos {
		if len(want) == 0 || want[r.Name] {
			out = append(out, events.Repo{Name: r.Name, Path: r.Path})
		}
	}
	return out
}

// Timeout is the per-request br deadline.
func (c *Config) Timeout() time.Duration {
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// Lookup finds a repo by its URL name.
func (c *Config) Lookup(name string) (Repo, bool) {
	for _, r := range c.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return Repo{}, false
}
