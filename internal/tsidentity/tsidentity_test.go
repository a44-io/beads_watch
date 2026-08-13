package tsidentity

import "testing"

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
