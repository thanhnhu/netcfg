// Package fsstore persists the desired state and the last known good snapshot.
package fsstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"netcfg/internal/domain"
	"netcfg/internal/platform/fileutil"
)

const (
	desiredFile   = "desired.json"
	lastGoodFile  = "last-known-good.json"
	historyDir    = "history"
	historyKeep   = 20
	filePerm      = 0o600
	directoryPerm = 0o700
)

// Store implements ports.Store on the local filesystem.
type Store struct {
	dir string
	mu  sync.Mutex
}

// New returns a store rooted at dir, e.g. /var/lib/netcfgd.
func New(dir string) *Store { return &Store{dir: dir} }

// Load reads the desired state, returning an empty state on first run.
func (s *Store) Load(ctx context.Context) (domain.DesiredState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(filepath.Join(s.dir, desiredFile))
}

// Save writes the desired state and appends a history snapshot.
func (s *Store) Save(ctx context.Context, state domain.DesiredState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state.Version = domain.StateVersion
	state.UpdatedAt = time.Now()
	if err := s.write(filepath.Join(s.dir, desiredFile), state); err != nil {
		return err
	}
	return s.appendHistory(state)
}

// LastKnownGood returns the newest state that was confirmed by an operator.
func (s *Store) LastKnownGood(ctx context.Context) (domain.DesiredState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(filepath.Join(s.dir, lastGoodFile))
}

// MarkGood records a state as safe to fall back to.
func (s *Store) MarkGood(ctx context.Context, state domain.DesiredState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(filepath.Join(s.dir, lastGoodFile), state)
}

func (s *Store) read(path string) (domain.DesiredState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.NewDesiredState(), nil
		}
		return domain.NewDesiredState(), domain.Internal("read %s: %v", path, err)
	}

	var state domain.DesiredState
	if err := json.Unmarshal(data, &state); err != nil {
		return domain.NewDesiredState(), domain.Internal("%s is corrupt: %v", path, err)
	}
	if state.Version != domain.StateVersion {
		return domain.NewDesiredState(), domain.Internal("%s uses schema version %s, supported version is %s", path, state.Version, domain.StateVersion)
	}
	if state.Links == nil {
		state.Links = map[string]domain.LinkDesired{}
	}
	return state, nil
}

func (s *Store) write(path string, state domain.DesiredState) error {
	if err := os.MkdirAll(s.dir, directoryPerm); err != nil {
		return domain.Internal("create %s: %v", s.dir, err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return domain.Internal("encode state: %v", err)
	}
	return fileutil.WriteAtomic(path, data, filePerm)
}

// appendHistory keeps a bounded ring of snapshots for post-mortem analysis.
func (s *Store) appendHistory(state domain.DesiredState) error {
	dir := filepath.Join(s.dir, historyDir)
	if err := os.MkdirAll(dir, directoryPerm); err != nil {
		return nil
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil
	}
	name := filepath.Join(dir, time.Now().UTC().Format("20060102T150405Z")+".json")
	if err := fileutil.WriteAtomic(name, data, filePerm); err != nil {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= historyKeep {
		return nil
	}
	for _, e := range entries[:len(entries)-historyKeep] {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	return nil
}
