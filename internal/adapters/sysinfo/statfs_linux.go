//go:build linux

package sysinfo

import "syscall"

// diskUsage reports bytes for one mount point. Available uses Bavail, the space
// an unprivileged process may actually claim, while Used counts the reserved
// blocks too, which is what df prints.
func diskUsage(mountpoint string) (total, used, available uint64, err error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(mountpoint, &fs); err != nil {
		return 0, 0, 0, err
	}

	blockSize := uint64(fs.Bsize)
	total = fs.Blocks * blockSize
	available = fs.Bavail * blockSize
	used = (fs.Blocks - fs.Bfree) * blockSize
	return total, used, available, nil
}
