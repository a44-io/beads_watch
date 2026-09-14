package tsidentity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeTailscale stands in for `tailscale whois --json <addr>`, answering
// from a table of the shapes tailscale actually produces: a tagged node, a
// user-owned node, and a node asked about its own address, where LoginName
// comes back as the node's FQDN rather than a person.
func fakeTailscale(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tailscale")
	script := `#!/bin/sh
case "$3" in
  100.1.1.1) echo '{"Node":{"Name":"dev.example.ts.net.","Tags":["tag:vps-dev"]},"UserProfile":{"LoginName":"tagged-devices"}}' ;;
  100.3.3.3) echo '{"Node":{"Name":"phone.example.ts.net."},"UserProfile":{"LoginName":"someone@example.com","DisplayName":"Someone"}}' ;;
  100.4.4.4) echo '{"Node":{"Name":"arch.example.ts.net.","Tags":["tag:home-gpu"]},"UserProfile":{"LoginName":"arch.example.ts.net"}}' ;;
  100.5.5.5) echo '{"Node":{"Name":"arch.example.ts.net."},"UserProfile":{"LoginName":"arch.example.ts.net."}}' ;;
  *) echo "no such peer: $3" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWhoIsRendersActor(t *testing.T) {
	r := &Resolver{Path: fakeTailscale(t)}
	cases := []struct {
		name, addr, node, login, actor string
	}{
		{"tagged device: machine only", "100.1.1.1", "dev", "", "dev"},
		{"user owned: login@node", "100.3.3.3", "phone", "someone@example.com", "someone@example.com@phone"},
		// Whois of the node's own address: LoginName is the FQDN, which names
		// no human, so the actor must stay the bare machine name.
		{"own address: machine only", "100.4.4.4", "arch", "", "arch"},
		{"own address, trailing dot: machine only", "100.5.5.5", "arch", "", "arch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			peer, err := r.WhoIs(context.Background(), c.addr)
			if err != nil {
				t.Fatalf("WhoIs(%s): %v", c.addr, err)
			}
			if peer.Node != c.node || peer.Login != c.login {
				t.Errorf("peer = node %q login %q, want node %q login %q", peer.Node, peer.Login, c.node, c.login)
			}
			if got := peer.Actor(); got != c.actor {
				t.Errorf("Actor() = %q, want %q", got, c.actor)
			}
		})
	}
	if _, err := r.WhoIs(context.Background(), "127.0.0.1"); err == nil {
		t.Error("WhoIs of a non-tailnet address: want an error")
	}
}

func TestPeerAddr(t *testing.T) {
	cases := []struct {
		name, header, want string
	}{
		{"absent", "", ""},
		{"single", "100.70.239.127", "100.70.239.127"},
		{"padded", "  100.70.239.127  ", "100.70.239.127"},
		{"not an address", "evil", ""},
		// tailscale replaces rather than appends, so a list should not occur.
		// If one ever does, the proxy's entry is last and a caller's forged
		// prefix must not win.
		{"forged prefix ignored", "1.2.3.4, 100.70.239.127", "100.70.239.127"},
		{"ipv6", "fd7a:115c:a1e0::ae37:ef80", "fd7a:115c:a1e0::ae37:ef80"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PeerAddr(c.header); got != c.want {
				t.Errorf("PeerAddr(%q) = %q, want %q", c.header, got, c.want)
			}
		})
	}
}

func TestPeerActor(t *testing.T) {
	cases := []struct {
		name string
		peer *Peer
		want string
	}{
		{"nil", nil, ""},
		// A tagged device has no owning user, so only the machine is named.
		{"tagged device", &Peer{Node: "dev", Tags: []string{"tag:vps-dev"}}, "dev"},
		{"user owned", &Peer{Node: "phone", Login: "someone@example.com"}, "someone@example.com@phone"},
		{"login only", &Peer{Login: "someone@example.com"}, "someone@example.com"},
		{"nothing known", &Peer{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.peer.Actor(); got != c.want {
				t.Errorf("Actor() = %q, want %q", got, c.want)
			}
		})
	}
}
