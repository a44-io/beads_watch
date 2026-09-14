//go:build !linux

package server

import "syscall"

// freebind is a no-op off Linux: the address has to exist at bind time.
func freebind(network, address string, c syscall.RawConn) error { return nil }
