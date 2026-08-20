package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"netcfg/internal/platform/fileutil"
)

const sessionFileVersion = 1

// Session is an authenticated browser session.
type Session struct {
	// Token is only ever held in memory and in the operator's cookie; the store
	// persists its digest so a leaked state file cannot be replayed.
	Token    string    `json:"-"`
	User     string    `json:"user"`
	CSRF     string    `json:"csrf"`
	Expires  time.Time `json:"expires"`
	Created  time.Time `json:"created"`
	Absolute time.Time `json:"absolute"`
}

type sessionFile struct {
	Version  int                `json:"version"`
	Sessions map[string]Session `json:"sessions"`
}

// SessionStore keeps sessions across restarts so a service restart during a
// commit-confirm window cannot log the operator out and trigger a rollback.
type SessionStore struct {
	mu      sync.Mutex
	items   map[string]Session
	ttl     time.Duration
	maxLife time.Duration
	path    string
	dirty   bool
	log     *slog.Logger
}

// NewSessionStore returns a store. When path is empty sessions stay in memory.
func NewSessionStore(ttl, maxLife time.Duration, path string, log *slog.Logger) *SessionStore {
	if maxLife <= 0 {
		maxLife = 12 * time.Hour
	}
	if log == nil {
		log = slog.Default()
	}
	return &SessionStore{items: map[string]Session{}, ttl: ttl, maxLife: maxLife, path: path, log: log}
}

// Load restores sessions written by a previous run.
func (s *SessionStore) Load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	var file sessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		s.log.Warn("session file is corrupt, ignoring it", "path", s.path, "err", err)
		return nil
	}
	if file.Version != sessionFileVersion {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for digest, sess := range file.Sessions {
		if now.After(sess.Expires) || now.After(sess.Absolute) {
			continue
		}
		s.items[digest] = sess
	}
	s.log.Info("restored saved sessions", "count", len(s.items))
	return nil
}

// Create issues a fresh session. A new token on every login defeats session
// fixation.
func (s *SessionStore) Create(user string) (Session, error) {
	token, err := RandomToken()
	if err != nil {
		return Session{}, err
	}
	csrf, err := RandomToken()
	if err != nil {
		return Session{}, err
	}

	now := time.Now()
	sess := Session{
		Token:    token,
		User:     user,
		CSRF:     csrf,
		Created:  now,
		Expires:  now.Add(s.ttl),
		Absolute: now.Add(s.maxLife),
	}

	s.mu.Lock()
	s.purgeLocked(now)
	s.items[digestOf(token)] = sess
	s.mu.Unlock()

	s.persist()
	return sess, nil
}

// Get returns a live session and slides its idle expiry.
func (s *SessionStore) Get(token string) (Session, bool) {
	digest := digestOf(token)

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.items[digest]
	if !ok {
		return Session{}, false
	}

	now := time.Now()
	if now.After(sess.Expires) || now.After(sess.Absolute) {
		delete(s.items, digest)
		s.dirty = true
		return Session{}, false
	}

	sess.Expires = now.Add(s.ttl)
	if sess.Expires.After(sess.Absolute) {
		sess.Expires = sess.Absolute
	}
	s.items[digest] = sess
	s.dirty = true

	sess.Token = token
	return sess, true
}

// Drop revokes a session immediately and on disk.
func (s *SessionStore) Drop(token string) {
	s.mu.Lock()
	delete(s.items, digestOf(token))
	s.mu.Unlock()
	s.persist()
}

// DropOthers revokes every session but one. A password change must not leave
// sessions opened with the old credential alive.
func (s *SessionStore) DropOthers(keep string) int {
	digest := digestOf(keep)

	s.mu.Lock()
	revoked := 0
	for d := range s.items {
		if d != digest {
			delete(s.items, d)
			revoked++
		}
	}
	s.mu.Unlock()

	s.persist()
	return revoked
}

// Run flushes slid expiry times and purges dead sessions until ctx ends.
func (s *SessionStore) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.flushIfDirty()
			return
		case <-ticker.C:
			s.mu.Lock()
			s.purgeLocked(time.Now())
			s.mu.Unlock()
			s.flushIfDirty()
		}
	}
}

func (s *SessionStore) flushIfDirty() {
	s.mu.Lock()
	dirty := s.dirty
	s.mu.Unlock()
	if dirty {
		s.persist()
	}
}

func (s *SessionStore) persist() {
	if s.path == "" {
		return
	}

	s.mu.Lock()
	file := sessionFile{Version: sessionFileVersion, Sessions: make(map[string]Session, len(s.items))}
	for digest, sess := range s.items {
		file.Sessions[digest] = sess
	}
	s.dirty = false
	s.mu.Unlock()

	data, err := json.Marshal(file)
	if err != nil {
		s.log.Warn("cannot encode sessions", "err", err)
		return
	}
	if err := fileutil.WriteAtomic(s.path, data, 0o600); err != nil {
		s.log.Warn("cannot persist sessions", "path", s.path, "err", err)
	}
}

func (s *SessionStore) purgeLocked(now time.Time) {
	for digest, sess := range s.items {
		if now.After(sess.Expires) || now.After(sess.Absolute) {
			delete(s.items, digest)
			s.dirty = true
		}
	}
}

func digestOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
