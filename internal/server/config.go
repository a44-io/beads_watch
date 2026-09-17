package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/a44-io/beads_watch/internal/events"
)

// Repo is one beads workspace this node serves.
type Repo struct {
	// Name is the URL path segment. It is an alias, not a filesystem fact, so
	// repos can be renamed on the wire without moving anything on disk.
	Name string `json:"name"`
	// Path is the directory br runs in. br walks up from here to find .beads,
	// which is what lets a .beads/redirect file point somewhere else entirely.
	Path string `json:"path"`

	// err is why Path was unusable when the config was normalized, or nil. A
	// missing checkout is a fact about this box — archived, never cloned here,
	// or hidden by the unit's sandbox — not a typo in the config, so it is
	// recorded rather than fatal and the daemon keeps serving every other repo.
	err error
}

// Probe reports whether Path is a directory this process can see right now.
// It is cheap enough to run per request, which is what lets a repo cloned
// after startup start answering without a restart.
func (r Repo) Probe() error {
	info, err := os.Stat(r.Path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", r.Path)
	}
	return nil
}

// Err is the startup probe's verdict: nil when Path stat'd cleanly, otherwise
// why it did not. Request handlers re-probe instead of trusting this, since
// the answer can change under a running daemon.
func (r Repo) Err() error { return r.err }

// Config is the daemon's whole configuration.
type Config struct {
	// Socket is the unix socket for local callers, mode 0600. It defaults to
	// $XDG_RUNTIME_DIR/beads_watch.sock when Listen is not set either, so a
	// config with neither still serves; set Listen alone for TCP only.
	Socket string `json:"socket"`
	// Listen is a TCP address served alongside the socket: this box's
	// tailscale IP and the bridge port, so the reverse proxy reaches the
	// daemon directly and the daemon sees the real peer. Empty means none.
	Listen string `json:"listen,omitempty"`
	// TrustedProxies are the addresses, IPs or CIDRs, whose forwarded identity
	// headers are believed. Any other TCP peer is identified by its own
	// address, whatever its headers say; on the unix socket forwarded headers
	// are never believed, since nothing there can vouch for them.
	TrustedProxies []string `json:"trusted_proxies,omitempty"`
	Repos          []Repo   `json:"repos"`

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

	// trusted is TrustedProxies parsed, filled by Normalize.
	trusted []netip.Prefix
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

// DefaultSocket is where local callers find the daemon.
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

// LoadConfig reads a config file. It does not Normalize: flags may still
// override fields, and the socket default depends on whether a TCP address
// ends up set, so the caller normalizes once after every override is in.
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
	return &cfg, nil
}

// Normalize fills in defaults and rejects configurations that would misbehave
// at request time rather than at startup.
//
// Two kinds of repo trouble are told apart here. A config-shaped error — no
// path, an unsafe name, a name used twice, notify naming a repo that is not
// served — is a typo the operator has to fix, and retrying cannot help, so it
// is returned and the daemon exits. An environment-shaped one — the path is
// gone, or is not a directory — is ordinary drift on a multi-machine setup and
// is recorded on the Repo instead, so one archived checkout cannot take every
// healthy repo offline with it. The one exception is when no repo at all
// survives the probe: a daemon that comes up and serves nothing is worse than
// one that refuses to start, so that case stays fatal.
func (c *Config) Normalize() error {
	c.Socket = strings.TrimSpace(c.Socket)
	c.Listen = strings.TrimSpace(c.Listen)
	if c.Socket == "" && c.Listen == "" {
		c.Socket = DefaultSocket()
	}
	if c.Listen != "" {
		if _, _, err := net.SplitHostPort(c.Listen); err != nil {
			return fmt.Errorf("listen %q: %w", c.Listen, err)
		}
	}
	c.trusted = c.trusted[:0]
	for i, raw := range c.TrustedProxies {
		p, err := parsePrefix(raw)
		if err != nil {
			return fmt.Errorf("trusted_proxies[%d] %q: not an IP address or CIDR", i, raw)
		}
		c.trusted = append(c.trusted, p)
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
	var unservable []string
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

		if r.err = r.Probe(); r.err != nil {
			unservable = append(unservable, fmt.Sprintf("%q: %v", r.Name, r.err))
		}
	}
	if len(unservable) == len(c.Repos) {
		return fmt.Errorf("no servable repos: %s", strings.Join(unservable, "; "))
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
// named subset when one is given, otherwise every served repo. A repo whose
// path failed the startup probe is left out — there is nothing there to tail,
// and the watcher would only repeat once a minute what the startup warning
// already said.
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
		if r.err != nil {
			continue
		}
		if len(want) == 0 || want[r.Name] {
			out = append(out, events.Repo{Name: r.Name, Path: r.Path})
		}
	}
	return out
}

// parsePrefix accepts an IP or a CIDR, so a config can name one proxy box or
// a whole subnet without two syntaxes to remember.
func parsePrefix(raw string) (netip.Prefix, error) {
	raw = strings.TrimSpace(raw)
	if p, err := netip.ParsePrefix(raw); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// TrustedProxy reports whether forwarded identity headers from this peer are
// believed. Only Normalize's parsed list counts, never the raw strings.
func (c *Config) TrustedProxy(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range c.trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Unavailable is every repo whose path failed the startup probe.
func (c *Config) Unavailable() []Repo {
	var out []Repo
	for _, r := range c.Repos {
		if r.err != nil {
			out = append(out, r)
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
