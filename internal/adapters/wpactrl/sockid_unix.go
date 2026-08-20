//go:build !windows

package wpactrl

import (
	"io/fs"
	"syscall"
)

// socketID identifies the socket instance sitting at a path. Timestamps cannot
// do this job: a filesystem with one second granularity gives a restarted
// wpa_supplicant the same mtime as the socket it replaced.
func socketID(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
