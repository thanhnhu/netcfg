//go:build !linux

package rpc

import (
	"errors"
	"net"
)

// peerUID is unavailable outside Linux. The deployment target is Debian; other
// platforms exist only for development builds, where the server refuses to
// enforce a UID allowlist and says so loudly at startup.
func peerUID(conn *net.UnixConn) (uint32, error) {
	return 0, errors.New("SO_PEERCRED is only supported on Linux")
}

func peerCredSupported() bool { return false }
