package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ListenSocket binds the unix socket, refusing to steal it from a live daemon
// but clearing it when the previous process died without cleaning up. The
// socket is mode 0600: it is the local path, and on a box running agent
// swarms every other local user stays out.
func ListenSocket(path string) (net.Listener, error) {
	if _, err := os.Stat(path); err == nil {
		conn, derr := net.DialTimeout("unix", path, time.Second)
		if derr == nil {
			conn.Close()
			return nil, fmt.Errorf("another beads_watch is already listening on %s", path)
		}
		// Nothing accepting: a stale socket from a killed process.
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("removing stale socket %s: %w", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// ListenTCP binds the tailnet-facing address. The socket is bound with
// IP_FREEBIND where the platform has it, because the address is this box's
// tailscale IP and a user unit cannot order itself after the system
// tailscaled: at login the address may not exist yet, and without freebind
// the daemon would fail to start and burn through its restart budget before
// tailscaled came up.
func ListenTCP(addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: freebind}
	return lc.Listen(context.Background(), "tcp", addr)
}
