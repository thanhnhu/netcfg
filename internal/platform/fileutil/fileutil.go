// Package fileutil provides atomic file replacement for generated configs.
package fileutil

import (
	"os"
	"path/filepath"

	"netcfg/internal/domain"
)

// WriteAtomic replaces path with data via a temp file and rename, so a crash
// can never leave a half written network configuration behind.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return domain.Internal("cannot create directory %s: %v", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".netcfg-*")
	if err != nil {
		return domain.Internal("cannot create temp file in %s: %v", dir, err)
	}
	name := tmp.Name()
	defer os.Remove(name)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return domain.Internal("write %s: %v", name, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return domain.Internal("chmod %s: %v", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return domain.Internal("sync %s: %v", name, err)
	}
	if err := tmp.Close(); err != nil {
		return domain.Internal("close %s: %v", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return domain.Internal("replace %s: %v", path, err)
	}

	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
