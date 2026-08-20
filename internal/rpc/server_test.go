package rpc

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestPrepareDirOpensTheDirectoryToTheSocketGroup covers the failure a real
// install exposed: systemd hands over /run/netcfgd owned by the unit's group,
// and a 0750 root:root directory hides the socket from the web tier however
// permissive the socket itself is.
func TestPrepareDirOpensTheDirectoryToTheSocketGroup(t *testing.T) {
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Skip("POSIX directory permissions behave differently off Linux")
	}

	tests := []struct {
		name  string
		start os.FileMode
		gid   int
		want  os.FileMode
	}{
		{name: "a directory closed to the group is opened", start: 0o700, gid: os.Getgid(), want: 0o750},
		{name: "an already searchable directory is left alone", start: 0o750, gid: os.Getgid(), want: 0o750},
		{name: "a wider directory is not narrowed", start: 0o755, gid: os.Getgid(), want: 0o755},
		{name: "without a group nothing is touched", start: 0o700, gid: -1, want: 0o700},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "run")
			if err := os.Mkdir(dir, tc.start); err != nil {
				t.Fatal(err)
			}
			// Mkdir is subject to umask, so set the mode explicitly.
			if err := os.Chmod(dir, tc.start); err != nil {
				t.Fatal(err)
			}

			s := &Server{
				cfg: ServerConfig{SocketPath: filepath.Join(dir, "netcfgd.sock"), GID: tc.gid},
				log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			if err := s.prepareDir(dir); err != nil {
				t.Fatalf("prepareDir: %v", err)
			}

			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != tc.want {
				t.Errorf("mode is %o, want %o", got, tc.want)
			}
		})
	}
}

// TestPrepareDirCreatesAMissingDirectory keeps netcfgd usable when it is not
// started by systemd, which is how the sandbox and the tests run it.
func TestPrepareDirCreatesAMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	s := &Server{
		cfg: ServerConfig{SocketPath: filepath.Join(dir, "netcfgd.sock"), GID: -1},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := s.prepareDir(dir); err != nil {
		t.Fatalf("prepareDir: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("directory was not created: %v", err)
	}
}
