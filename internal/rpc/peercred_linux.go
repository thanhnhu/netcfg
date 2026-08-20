//go:build linux

package rpc

import (
	"net"
	"syscall"
)

// peerUID reads the credentials the kernel attached to the socket, which cannot
// be forged by the connecting process.
func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}

	var ucred *syscall.Ucred
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		ucred, sockErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return 0, err
	}
	if sockErr != nil {
		return 0, sockErr
	}
	return ucred.Uid, nil
}

func peerCredSupported() bool { return true }
