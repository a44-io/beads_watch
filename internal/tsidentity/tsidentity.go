// Package tsidentity resolves a caller's tailnet identity from what the
// `tailscale serve` proxy tells us about them.
//
// Two properties of the proxy make this trustworthy, both verified by
// experiment against tailscale 1.98.9 rather than assumed:
//
//   - A client-supplied Tailscale-User-Login header is STRIPPED, not passed
//     through. So when that header arrives, the proxy put it there.
//   - A client-supplied X-Forwarded-For is REPLACED, not appended. So the
//     single address in it is the proxy's own view of the peer, and a caller
//     cannot prepend a forged entry to shadow it.
//
// If either were untrue, identity here would be self-asserted and worthless
// for an audit trail.
package tsidentity

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Peer is who the proxy says called.
type Peer struct {
	// Node is the short hostname, e.g. "dev".
	Node string `json:"node"`
	// FQDN is the MagicDNS name, e.g. "dev.example.ts.net".
	FQDN string `json:"fqdn,omitempty"`
	// Login is the owning user, or empty for a tagged device, which has no
	// user at all — that is why tagged nodes cannot yield a human identity.
	Login string   `json:"login,omitempty"`
	Tags  []string `json:"tags,omitempty"`
}

// Actor renders the peer as a BD_ACTOR string. A tagged node contributes the
// machine only, because there is no user behind it to name.
func (p *Peer) Actor() string {
	switch {
	case p == nil:
		return ""
	case p.Login != "" && p.Node != "":
		return p.Login + "@" + p.Node
	case p.Login != "":
		return p.Login
	default:
		return p.Node
	}
}

// Resolver answers WhoIs queries, caching briefly so a burst of requests from
// one node does not fork `tailscale` once per call. Caching identity is safe
// in a way caching beads data is not: it is transport metadata, not the
// answer the caller asked for.
type Resolver struct {
	// Path is the tailscale binary; empty means look it up on PATH.
	Path string
	// TTL bounds how long a resolved peer is reused. Zero means 60s.
	TTL time.Duration
	// Timeout bounds one whois call. Zero means 3s.
	Timeout time.Duration

	mu    sync.Mutex
	cache map[string]entry
}

type entry struct {
	peer *Peer
	err  error
	at   time.Time
}

// PeerAddr extracts the caller's tailnet address from the proxy's headers.
// It returns "" when the request did not come through the proxy.
func PeerAddr(xForwardedFor string) string {
	xForwardedFor = strings.TrimSpace(xForwardedFor)
	if xForwardedFor == "" {
		return ""
	}
	// The proxy replaces rather than appends, so there is normally exactly one
	// entry. Take the last defensively: if a future tailscale ever appends,
	// the proxy's own entry is the trustworthy one, and a caller's forged
	// prefix must not win.
	parts := strings.Split(xForwardedFor, ",")
	addr := strings.TrimSpace(parts[len(parts)-1])
	if net.ParseIP(addr) == nil {
		return ""
	}
	return addr
}

// WhoIs resolves a tailnet address to a peer.
func (r *Resolver) WhoIs(ctx context.Context, addr string) (*Peer, error) {
	if addr == "" {
		return nil, fmt.Errorf("no peer address")
	}
	ttl := r.TTL
	if ttl <= 0 {
		ttl = 60 * time.Second
	}

	r.mu.Lock()
	if e, ok := r.cache[addr]; ok && time.Since(e.at) < ttl {
		r.mu.Unlock()
		return e.peer, e.err
	}
	r.mu.Unlock()

	peer, err := r.lookup(ctx, addr)

	r.mu.Lock()
	if r.cache == nil {
		r.cache = make(map[string]entry)
	}
	r.cache[addr] = entry{peer: peer, err: err, at: time.Now()}
	r.mu.Unlock()

	return peer, err
}

// whoisOutput is the subset of `tailscale whois --json` we rely on.
type whoisOutput struct {
	Node struct {
		Name string   `json:"Name"`
		Tags []string `json:"Tags"`
	} `json:"Node"`
	UserProfile struct {
		LoginName   string `json:"LoginName"`
		DisplayName string `json:"DisplayName"`
	} `json:"UserProfile"`
}

func (r *Resolver) lookup(ctx context.Context, addr string) (*Peer, error) {
	bin := r.Path
	if bin == "" {
		bin = "tailscale"
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin, "whois", "--json", addr).Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale whois %s: %w", addr, err)
	}
	var w whoisOutput
	if err := json.Unmarshal(out, &w); err != nil {
		return nil, fmt.Errorf("tailscale whois %s: %w", addr, err)
	}

	fqdn := strings.TrimSuffix(w.Node.Name, ".")
	peer := &Peer{
		FQDN: fqdn,
		Node: fqdn,
		Tags: w.Node.Tags,
	}
	if i := strings.IndexByte(fqdn, '.'); i > 0 {
		peer.Node = fqdn[:i]
	}
	// "tagged-devices" is the synthetic owner tailscale reports for tagged
	// nodes. It names no human, so recording it as the actor would be a lie
	// dressed as an identity.
	if login := w.UserProfile.LoginName; login != "" && login != "tagged-devices" {
		peer.Login = login
	}
	return peer, nil
}
