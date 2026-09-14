// Package tsidentity resolves a tailnet address to the machine and user
// behind it, with `tailscale whois`, and reads the address a reverse proxy
// forwarded.
//
// Which address to resolve is the server's decision, made from the transport:
// the proxy's forwarded X-Forwarded-For when the connection came from a
// trusted proxy, the TCP peer itself otherwise. This package only knows how
// to read the header and ask tailscale.
//
// Two properties of the proxies in use make the forwarded header worth
// reading at all, both verified by experiment rather than assumed:
//
//   - A client-supplied Tailscale-User-Login header is STRIPPED, not passed
//     through (natively by `tailscale serve`; by an explicit header_up in the
//     caddy site files). So when that header arrives, the proxy put it there.
//   - X-Forwarded-For is either REPLACED (`tailscale serve`) or APPENDED to
//     (caddy) with the proxy's own view of the peer. Either way the LAST entry
//     is the proxy's, and a caller cannot prepend a forged entry to shadow it.
//
// If either were untrue, identity from a proxy would be self-asserted and
// worthless for an audit trail.
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

// PeerAddr extracts the caller's tailnet address from the proxy's
// X-Forwarded-For. It returns "" when there is none. The caller decides
// whether the header is worth believing; see the package comment.
func PeerAddr(xForwardedFor string) string {
	xForwardedFor = strings.TrimSpace(xForwardedFor)
	if xForwardedFor == "" {
		return ""
	}
	// Caddy appends its observed peer, so the last entry is the proxy's own
	// and a caller's forged prefix must not win. tailscale serve replaces the
	// header outright, which the same rule covers.
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
	// dressed as an identity. Whois of a node's OWN address is the other
	// case: there tailscale fills LoginName with the node's FQDN, which is
	// not a person either, and would render the actor as "fqdn@node".
	if login := w.UserProfile.LoginName; login != "" && login != "tagged-devices" && !sameNode(login, fqdn) {
		peer.Login = login
	}
	return peer, nil
}

// sameNode reports whether a LoginName is just the node's own name in
// disguise: equal to its FQDN, with or without the trailing dot.
func sameNode(login, fqdn string) bool {
	return fqdn != "" && strings.TrimSuffix(login, ".") == fqdn
}
