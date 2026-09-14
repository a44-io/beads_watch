//go:build linux

package server

import "syscall"

// freebind sets IP_FREEBIND so the listener can bind an address that is not
// yet assigned to any interface. Linux applies the option at IPPROTO_IP to
// IPv6 sockets as well, so one call covers both families.
func freebind(network, address string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_FREEBIND, 1)
	}); err != nil {
		return err
	}
	return serr
}
