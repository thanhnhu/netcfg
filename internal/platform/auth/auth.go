// Package auth stores the admin credential and issues browser sessions.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"

	"netcfg/internal/domain"
	"netcfg/internal/kdf"
	"netcfg/internal/platform/fileutil"
)

const (
	pbkdf2Iterations = 240_000
	saltLen          = 16
	hashLen          = 32
	minPasswordLen   = 8
	// PBKDF2 cost is paid by the server, so an unbounded password is a cheap
	// way for an authenticated client to burn CPU.
	maxPasswordLen = 256
)

// ErrNoCredentials means the app has never been given a password.
var ErrNoCredentials = errors.New("no administrator account configured; run again with -set-password")

// Credentials is the on-disk admin account; only a PBKDF2 digest is stored.
type Credentials struct {
	Username   string `json:"username"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
	Iterations int    `json:"iterations"`
}

// Load reads the credential file.
func Load(path string) (*Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoCredentials
		}
		return nil, err
	}

	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, errors.New("credential file is corrupt: " + err.Error())
	}
	if c.Username == "" || c.Salt == "" || c.Hash == "" || c.Iterations <= 0 {
		return nil, ErrNoCredentials
	}
	return &c, nil
}

// Save writes the admin account with restrictive permissions.
func Save(path, username, password string) error {
	creds, err := newCredentials(username, password)
	if err != nil {
		return err
	}
	return write(path, creds)
}

// newCredentials derives a digest with a fresh salt.
func newCredentials(username, password string) (Credentials, error) {
	if username == "" || len(username) > 64 {
		return Credentials{}, domain.Invalid("username must be 1-64 characters")
	}
	if len(password) < minPasswordLen {
		return Credentials{}, domain.Invalid("administrator password must be at least 8 characters")
	}
	if len(password) > maxPasswordLen {
		return Credentials{}, domain.Invalid("administrator password must be at most 256 characters")
	}

	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return Credentials{}, domain.Internal("cannot generate a salt: %v", err)
	}
	return Credentials{
		Username:   username,
		Salt:       hex.EncodeToString(salt),
		Hash:       hex.EncodeToString(kdf.Key([]byte(password), salt, pbkdf2Iterations, hashLen, sha256.New)),
		Iterations: pbkdf2Iterations,
	}, nil
}

func write(path string, creds Credentials) error {
	body, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteAtomic(path, body, 0o600)
}

// Verify compares a login attempt in constant time. The derivation always runs
// so a wrong username and a wrong password take the same time.
func (c *Credentials) Verify(username, password string) bool {
	salt, err := hex.DecodeString(c.Salt)
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(c.Hash)
	if err != nil {
		return false
	}

	got := kdf.Key([]byte(password), salt, c.Iterations, len(want), sha256.New)
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(c.Username)) == 1
	passOK := subtle.ConstantTimeCompare(got, want) == 1
	return userOK && passOK
}

// Manager owns the credential file at runtime so the password can be changed
// without restarting the service.
type Manager struct {
	mu    sync.RWMutex
	path  string
	creds Credentials
}

func NewManager(path string, creds *Credentials) *Manager {
	return &Manager{path: path, creds: *creds}
}

func (m *Manager) Username() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.creds.Username
}

func (m *Manager) Verify(username, password string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.creds.Verify(username, password)
}

// ChangePassword re-derives the digest with a fresh salt and replaces the file
// atomically. Verification happens under the write lock, so two concurrent
// requests cannot both succeed against the same old password.
func (m *Manager) ChangePassword(username, current, next string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.creds.Verify(username, current) {
		return domain.Invalid("the current password is wrong")
	}
	if current == next {
		return domain.Invalid("the new password must be different from the current one")
	}

	updated, err := newCredentials(m.creds.Username, next)
	if err != nil {
		return err
	}
	if err := write(m.path, updated); err != nil {
		return domain.Internal("cannot save the new password: %v", err)
	}
	m.creds = updated
	return nil
}

// Throttle slows down password guessing per client address.
type Throttle struct {
	mu       sync.Mutex
	failures map[string]int
	until    map[string]time.Time
	max      int
	lockout  time.Duration
}

func NewThrottle(max int, lockout time.Duration) *Throttle {
	return &Throttle{
		failures: map[string]int{},
		until:    map[string]time.Time{},
		max:      max,
		lockout:  lockout,
	}
}

// Blocked reports whether a client must wait, and for how long.
func (t *Throttle) Blocked(key string) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	deadline, ok := t.until[key]
	if !ok {
		return false, 0
	}
	if remaining := time.Until(deadline); remaining > 0 {
		return true, remaining
	}
	delete(t.until, key)
	delete(t.failures, key)
	return false, 0
}

func (t *Throttle) Fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.failures[key]++
	if t.failures[key] >= t.max {
		t.until[key] = time.Now().Add(t.lockout)
	}
}

func (t *Throttle) Reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, key)
	delete(t.until, key)
}

// RandomToken returns 256 bits of hex encoded entropy.
func RandomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
