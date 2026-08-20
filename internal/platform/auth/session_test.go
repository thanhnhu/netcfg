package auth

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T, path string) *SessionStore {
	t.Helper()
	return NewSessionStore(30*time.Minute, 12*time.Hour, path,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestSessionsSurviveRestart is the point of persisting sessions: a service
// restart during a commit-confirm window must not sign the operator out and
// cause an unwanted rollback.
func TestSessionsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")

	first := newStore(t, path)
	sess, err := first.Create("admin")
	if err != nil {
		t.Fatal(err)
	}

	second := newStore(t, path)
	if err := second.Load(); err != nil {
		t.Fatal(err)
	}

	restored, ok := second.Get(sess.Token)
	if !ok {
		t.Fatal("the session must survive a restart")
	}
	if restored.User != "admin" || restored.CSRF != sess.CSRF {
		t.Fatalf("restored session is wrong: %+v", restored)
	}
}

// TestStoredFileHoldsNoUsableToken keeps a leaked state file from being replayed.
func TestStoredFileHoldsNoUsableToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")

	store := newStore(t, path)
	sess, err := store.Create("admin")
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data := string(raw)
	if strings.Contains(data, sess.Token) {
		t.Fatal("the raw token must never be written to the session file")
	}
	if !strings.Contains(data, digestOf(sess.Token)) {
		t.Fatal("the file must contain the token digest")
	}
}

func TestDropRevokesImmediately(t *testing.T) {
	store := newStore(t, filepath.Join(t.TempDir(), "sessions.json"))

	sess, err := store.Create("admin")
	if err != nil {
		t.Fatal(err)
	}
	store.Drop(sess.Token)

	if _, ok := store.Get(sess.Token); ok {
		t.Fatal("a signed out session is still usable")
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	store := NewSessionStore(time.Nanosecond, time.Hour, "",
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	sess, err := store.Create("admin")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)

	if _, ok := store.Get(sess.Token); ok {
		t.Fatal("an expired session is still usable")
	}
}

func TestCredentialRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	if err := Save(path, "admin", "long-enough-password"); err != nil {
		t.Fatal(err)
	}
	creds, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if !creds.Verify("admin", "long-enough-password") {
		t.Fatal("the correct password was rejected")
	}
	if creds.Verify("admin", "wrong-password") {
		t.Fatal("a wrong password was accepted")
	}
	if creds.Verify("other", "long-enough-password") {
		t.Fatal("a wrong username was accepted")
	}
	if err := Save(path, "admin", "short"); err == nil {
		t.Fatal("a too short password must be rejected")
	}
}
