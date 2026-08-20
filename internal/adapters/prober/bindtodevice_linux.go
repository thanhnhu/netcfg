//go:build linux

package prober

import (
	"syscall"
)

// bindToDevice pins the socket to one interface. SO_BINDTODEVICE needs
// CAP_NET_RAW, which the agent's unit grants; without it the dial simply
// follows the routing table and the caller learns nothing new.
func bindToDevice(link string) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var setErr error
		err := c.Control(func(fd uintptr) {
			setErr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, link)
		})
		if err != nil {
			return err
		}
		return setErr
	}
}
