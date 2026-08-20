//go:build !linux

package sysinfo

import "errors"

func diskUsage(string) (total, used, available uint64, err error) {
	return 0, 0, 0, errors.New("filesystem usage is only available on Linux")
}
