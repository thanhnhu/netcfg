// Package wpactrl speaks the wpa_supplicant control interface protocol over a
// Unix datagram socket. Talking to the socket directly (instead of shelling out
// to wpa_cli) keeps credentials out of any process argument list.
package wpactrl

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const readBuffer = 64 << 10

var counter atomic.Uint64

// Conn is a single control connection. It is safe for concurrent use.
type Conn struct {
	mu        sync.Mutex
	conn      *net.UnixConn
	localPath string
}

// Dial binds a private socket in localDir and connects it to the supplicant
// control socket for one interface, e.g. /run/wpa_supplicant/wlan0.
//
// localDir must be visible to wpa_supplicant as well, because it replies to the
// path we bind. A runtime directory is preferable to /tmp so the service can
// still enable PrivateTmp.
func Dial(remotePath, localDir string) (*Conn, error) {
	if localDir == "" {
		localDir = os.TempDir()
	}
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return nil, fmt.Errorf("create local socket directory %s: %w", localDir, err)
	}

	localPath := filepath.Join(localDir,
		fmt.Sprintf("wpa_ctrl_%d_%d", os.Getpid(), counter.Add(1)))
	_ = os.Remove(localPath)

	conn, err := net.DialUnix("unixgram",
		&net.UnixAddr{Name: localPath, Net: "unixgram"},
		&net.UnixAddr{Name: remotePath, Net: "unixgram"})
	if err != nil {
		_ = os.Remove(localPath)
		return nil, fmt.Errorf("open control socket %s: %w", remotePath, err)
	}
	return &Conn{conn: conn, localPath: localPath}, nil
}

// Request sends one command and returns the first non-event reply.
func (c *Conn) Request(cmd string, timeout time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	deadline := time.Now().Add(timeout)
	if err := c.conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	if _, err := c.conn.Write([]byte(cmd)); err != nil {
		return "", fmt.Errorf("send control command: %w", err)
	}

	buf := make([]byte, readBuffer)
	for time.Now().Before(deadline) {
		n, err := c.conn.Read(buf)
		if err != nil {
			return "", fmt.Errorf("read control reply: %w", err)
		}
		reply := string(buf[:n])
		// Unsolicited events are prefixed with a priority marker such as <3>.
		if strings.HasPrefix(reply, "<") {
			continue
		}
		return strings.TrimRight(reply, "\n"), nil
	}
	return "", fmt.Errorf("timed out waiting for a reply to command %q", firstWord(cmd))
}

// ReadEvent blocks until the next unsolicited event arrives on an attached
// connection. Call Attach first.
func (c *Conn) ReadEvent(timeout time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", err
	}
	buf := make([]byte, readBuffer)
	n, err := c.conn.Read(buf)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(buf[:n]), "\n"), nil
}

// Attach subscribes this connection to the event stream.
func (c *Conn) Attach(timeout time.Duration) error {
	reply, err := c.Request("ATTACH", timeout)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(reply, "OK") {
		return fmt.Errorf("ATTACH was rejected: %s", reply)
	}
	return nil
}

// Close releases the socket and removes the private endpoint file.
func (c *Conn) Close() error {
	// A blocked ReadEvent holds the mutex for its whole timeout, so waiting for
	// it here would stall every caller that needs to retire this connection.
	// Expiring the deadline wakes the reader first.
	_ = c.conn.SetDeadline(time.Now())

	c.mu.Lock()
	defer c.mu.Unlock()

	err := c.conn.Close()
	_ = os.Remove(c.localPath)
	return err
}

func firstWord(s string) string {
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}
