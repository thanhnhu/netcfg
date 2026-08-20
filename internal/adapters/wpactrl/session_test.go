package wpactrl

import (
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// serve answers control requests on a unixgram socket the way wpa_supplicant
// does, so the adapter can be exercised without one running.
func serve(t *testing.T, path string) *net.UnixConn {
	t.Helper()
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, peer, err := conn.ReadFromUnix(buf)
			if err != nil {
				return
			}
			reply := "OK\n"
			if string(buf[:n]) == "PING" {
				reply = "PONG\n"
			}
			_, _ = conn.WriteToUnix([]byte(reply), peer)
		}
	}()
	return conn
}

// TestASessionSurvivesTheSupplicantRestarting is the failure the fallback AP
// exposed: stopping wpa_supplicant unlinks the socket, and a cached connection
// to the dead peer swallows every request until the timeout expires.
func TestASessionSurvivesTheSupplicantRestarting(t *testing.T) {
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Skip("unix domain sockets behave differently off Linux")
	}

	dir := t.TempDir()
	local := t.TempDir()
	path := filepath.Join(dir, "wlan0")

	first := serve(t, path)
	adapter := New(dir, local, t.TempDir(), nil, testLogger())
	defer adapter.Close()

	if _, err := adapter.request("wlan0", "PING", time.Second); err != nil {
		t.Fatalf("first request: %v", err)
	}

	// Restart: the old socket goes away and a new one takes its place, exactly
	// as systemd stopping and starting the unit would do.
	_ = first.Close()
	_ = os.Remove(path)
	time.Sleep(10 * time.Millisecond)
	second := serve(t, path)
	defer second.Close()

	done := make(chan error, 1)
	go func() {
		_, err := adapter.request("wlan0", "PING", 5*time.Second)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("request after restart: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request hung on the dead socket instead of reconnecting")
	}
}

// TestAMissingSocketFailsFastWithAdvice keeps the operator facing message
// useful when the supplicant is simply not running.
func TestAMissingSocketFailsFastWithAdvice(t *testing.T) {
	adapter := New(t.TempDir(), t.TempDir(), t.TempDir(), nil, testLogger())
	defer adapter.Close()

	_, err := adapter.session("wlan0")
	if err == nil {
		t.Fatal("expected an error when no socket exists")
	}
	if got := err.Error(); got == "" {
		t.Fatal("error carries no message")
	}
}

// TestARequestSurvivesItsSessionBeingRetired covers what closing a connection
// does to a call already in flight: expiring the deadline to wake the event
// reader also aborts that call, which used to surface as a bogus i/o timeout.
func TestARequestSurvivesItsSessionBeingRetired(t *testing.T) {
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Skip("unix domain sockets behave differently off Linux")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "wlan0")
	server := serve(t, path)
	defer server.Close()

	adapter := New(dir, t.TempDir(), t.TempDir(), nil, testLogger())
	defer adapter.Close()

	if _, err := adapter.request("wlan0", "PING", 2*time.Second); err != nil {
		t.Fatalf("warm up: %v", err)
	}

	// Retire the session from under the caller, as the pump goroutine does when
	// the supplicant announces it is terminating.
	go func() {
		time.Sleep(5 * time.Millisecond)
		adapter.drop("wlan0")
	}()

	for i := 0; i < 20; i++ {
		if _, err := adapter.request("wlan0", "PING", 2*time.Second); err != nil {
			t.Fatalf("request %d failed while the session was recycled: %v", i, err)
		}
	}
}

// TestAnOrphanedSocketFailsFast covers the window while systemd restarts
// wpa_supplicant: the socket file it left behind still accepts a connect, so
// only a probe tells a live supplicant from a dead one.
func TestAnOrphanedSocketFailsFast(t *testing.T) {
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Skip("unix domain sockets behave differently off Linux")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "wlan0")

	// Bind and abandon: the file stays, nobody answers.
	orphan, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer orphan.Close()

	adapter := New(dir, t.TempDir(), t.TempDir(), nil, testLogger())
	defer adapter.Close()

	start := time.Now()
	_, err = adapter.session("wlan0")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a socket nobody is serving")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %v to give up; the operator should not wait that long", elapsed)
	}
	if strings.Contains(err.Error(), "unixgram") {
		t.Errorf("error leaks socket plumbing at the operator: %v", err)
	}
}
