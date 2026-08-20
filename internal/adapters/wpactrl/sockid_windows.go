//go:build windows

package wpactrl

import "io/fs"

// Windows has no wpa_supplicant; this exists so the package still builds for
// developers running the test suite there.
func socketID(info fs.FileInfo) uint64 { return uint64(info.ModTime().UnixNano()) }
