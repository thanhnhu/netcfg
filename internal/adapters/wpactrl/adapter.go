package wpactrl

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"netcfg/internal/domain"
	"netcfg/internal/ports"
)

const (
	requestTimeout = 10 * time.Second
	scanTimeout    = 20 * time.Second
	// A supplicant that is actually listening answers PING in microseconds, so
	// waiting longer only delays a diagnosis the operator already needs.
	livenessTimeout = 2 * time.Second
)

var _ ports.WiFiBackend = (*Adapter)(nil)

// Adapter implements ports.Supplicant on top of the control socket.
type Adapter struct {
	dir      string
	localDir string
	confDir  string
	pub      ports.Publisher
	log      *slog.Logger

	mu    sync.Mutex
	links map[string]*linkSession
}

// New returns an adapter reading control sockets from dir (e.g.
// /run/wpa_supplicant) and binding its own endpoints in localDir. confDir holds
// the wpa_supplicant configuration files.
func New(dir, localDir, confDir string, pub ports.Publisher, log *slog.Logger) *Adapter {
	if dir == "" {
		dir = "/run/wpa_supplicant"
	}
	if confDir == "" {
		confDir = "/etc/wpa_supplicant"
	}
	return &Adapter{dir: dir, localDir: localDir, confDir: confDir, pub: pub, log: log, links: map[string]*linkSession{}}
}

type linkSession struct {
	name    string
	cmd     *Conn
	events  *Conn
	scan    *notifier
	stop    chan struct{}
	stopped sync.Once
	// sockID identifies the socket this session is bound to. A restarted
	// wpa_supplicant unlinks the old socket and creates a new one; the connection
	// to the dead peer swallows requests, so the caller would otherwise wait out
	// the whole timeout on every call.
	sockID uint64
}

func (a *Adapter) Kind() domain.WiFiBackendKind { return domain.WiFiBackendWPA }

// Detect lists the links wpa_supplicant exposes a control socket for. Under
// NetworkManager the supplicant stays private, so no socket appears here and
// this backend correctly steps aside.
func (a *Adapter) Detect(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		// A stopped wpa_supplicant takes its runtime directory with it, so the
		// bare "no such file or directory" would only restate the path.
		if errors.Is(err, os.ErrNotExist) {
			return nil, domain.Unavailable("no control sockets in %s; check wpa_supplicant@<link>.service", a.dir)
		}
		return nil, domain.Unavailable("cannot read %s: %v", a.dir, err)
	}

	var links []string
	for _, entry := range entries {
		if domain.ValidateLinkName(entry.Name()) == nil {
			links = append(links, entry.Name())
		}
	}
	if len(links) == 0 {
		return nil, domain.Unavailable("no control sockets in %s; check wpa_supplicant@<link>.service", a.dir)
	}
	return links, nil
}

func (a *Adapter) session(link string) (*linkSession, error) {
	if err := domain.ValidateLinkName(link); err != nil {
		return nil, err
	}

	path := filepath.Join(a.dir, link)
	info, statErr := os.Stat(path)

	a.mu.Lock()
	if s, ok := a.links[link]; ok {
		if statErr == nil && socketID(info) == s.sockID {
			a.mu.Unlock()
			return s, nil
		}
		delete(a.links, link)
		a.mu.Unlock()
		s.close()
	} else {
		a.mu.Unlock()
	}

	if statErr != nil {
		return nil, domain.Unavailable("wpa_supplicant control socket not found at %s; check wpa_supplicant@%s.service", path, link)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.links[link]; ok {
		return s, nil
	}

	cmd, err := Dial(path, a.localDir)
	if err != nil {
		return nil, domain.Unavailable("cannot reach wpa_supplicant on %s: %v", link, err)
	}
	// The socket file outlives the process that owned it, so connecting proves
	// nothing. PING is the cheapest way to tell a live supplicant from the stale
	// file one leaves behind while systemd restarts it.
	if _, err := cmd.Request("PING", livenessTimeout); err != nil {
		_ = cmd.Close()
		return nil, domain.Unavailable("wpa_supplicant on %s is not answering yet; it may still be starting", link)
	}
	events, err := Dial(path, a.localDir)
	if err != nil {
		_ = cmd.Close()
		return nil, domain.Unavailable("cannot open the event channel on %s: %v", link, err)
	}
	if err := events.Attach(requestTimeout); err != nil {
		_ = cmd.Close()
		_ = events.Close()
		return nil, domain.Unavailable("cannot subscribe to events on %s: %v", link, err)
	}

	s := &linkSession{name: link, cmd: cmd, events: events, scan: newNotifier(), stop: make(chan struct{}), sockID: socketID(info)}
	a.links[link] = s
	go a.pump(s)
	return s, nil
}

// drop retires one session. It compares identity because a pump goroutine from
// a session that has already been replaced must not evict its successor.
func (a *Adapter) dropSession(s *linkSession) {
	a.mu.Lock()
	if current, ok := a.links[s.name]; ok && current == s {
		delete(a.links, s.name)
	}
	a.mu.Unlock()
	s.close()
}

func (a *Adapter) drop(link string) {
	a.mu.Lock()
	s, ok := a.links[link]
	delete(a.links, link)
	a.mu.Unlock()
	if ok {
		s.close()
	}
}

func (s *linkSession) close() {
	s.stopped.Do(func() {
		close(s.stop)
		_ = s.cmd.Close()
		_ = s.events.Close()
	})
}

// pump turns supplicant events into domain events and wakes scan waiters.
func (a *Adapter) pump(s *linkSession) {
	for {
		select {
		case <-s.stop:
			return
		default:
		}

		raw, err := s.events.ReadEvent(30 * time.Second)
		if err != nil {
			if isTimeout(err) {
				continue
			}
			a.log.Warn("wpa_supplicant event channel closed", "link", s.name, "err", err)
			a.dropSession(s)
			return
		}

		msg := stripPriority(raw)
		switch {
		case strings.HasPrefix(msg, "CTRL-EVENT-SCAN-RESULTS"):
			s.scan.broadcast()
			a.pub.Publish(domain.NewEvent(domain.EventScanResults, s.name, domain.Message{}, nil))
		case strings.HasPrefix(msg, "CTRL-EVENT-CONNECTED"):
			a.pub.Publish(domain.NewEvent(domain.EventWiFiState, s.name, domain.Msg("Wi-Fi connected"), nil))
		case strings.HasPrefix(msg, "CTRL-EVENT-DISCONNECTED"):
			a.pub.Publish(domain.NewEvent(domain.EventWiFiState, s.name, domain.Msg("Wi-Fi disconnected"), nil))
		case strings.HasPrefix(msg, "CTRL-EVENT-SSID-TEMP-DISABLED"):
			if strings.Contains(msg, "WRONG_KEY") {
				a.pub.Publish(domain.NewEvent(domain.EventWiFiState, s.name, domain.Msg("Wrong Wi-Fi password"), nil))
			}
		case strings.HasPrefix(msg, "CTRL-EVENT-ASSOC-REJECT"):
			a.pub.Publish(domain.NewEvent(domain.EventWiFiState, s.name, domain.Msg("The access point rejected the association"), nil))
		case strings.HasPrefix(msg, "CTRL-EVENT-TERMINATING"):
			a.log.Warn("wpa_supplicant is shutting down", "link", s.name)
			a.dropSession(s)
			return
		}
	}
}

func (a *Adapter) request(link, cmd string, timeout time.Duration) (string, error) {
	reply, err := a.send(link, cmd, timeout)
	if err != nil {
		var known *domain.Error
		if errors.As(err, &known) {
			return "", err
		}
		// The session can be retired mid-flight when wpa_supplicant restarts or
		// the fallback AP takes the radio, which surfaces here as a transport
		// error on a connection that no longer matters. One clean retry tells
		// that apart from a supplicant that is really gone.
		a.drop(link)
		if reply, err = a.send(link, cmd, timeout); err != nil {
			if errors.As(err, &known) {
				return "", err
			}
			a.drop(link)
			return "", domain.Unavailable("wpa_supplicant on %s is not responding: %v", link, err)
		}
	}
	if strings.HasPrefix(reply, "FAIL") || reply == "UNKNOWN COMMAND" {
		return reply, domain.Invalid("wpa_supplicant rejected command %s: %s", firstWord(cmd), reply)
	}
	return reply, nil
}

func (a *Adapter) send(link, cmd string, timeout time.Duration) (string, error) {
	s, err := a.session(link)
	if err != nil {
		return "", err
	}
	return s.cmd.Request(cmd, timeout)
}

func (a *Adapter) mustOK(link, cmd string) error {
	reply, err := a.request(link, cmd, requestTimeout)
	if err != nil {
		return err
	}
	if strings.TrimSpace(reply) != "OK" {
		return domain.Invalid("command %s failed: %s", firstWord(cmd), strings.TrimSpace(reply))
	}
	return nil
}

// Scan triggers a scan and waits for the radio to report results.
func (a *Adapter) Scan(ctx context.Context, link string) ([]domain.AccessPoint, error) {
	s, err := a.session(link)
	if err != nil {
		return nil, err
	}

	wait, cancel := s.scan.wait()
	defer cancel()

	// FAIL-BUSY simply means a scan is already running; its results still arrive.
	if reply, err := s.cmd.Request("SCAN", requestTimeout); err != nil {
		a.drop(link)
		return nil, domain.Unavailable("cannot start a scan on %s: %v", link, err)
	} else if strings.HasPrefix(reply, "FAIL") && !strings.Contains(reply, "BUSY") {
		return nil, domain.Unavailable("scan failed on %s: %s", link, reply)
	}

	select {
	case <-wait:
	case <-time.After(scanTimeout):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return a.ScanResults(ctx, link)
}

// ScanResults returns the cached scan table.
func (a *Adapter) ScanResults(ctx context.Context, link string) ([]domain.AccessPoint, error) {
	reply, err := a.request(link, "SCAN_RESULTS", requestTimeout)
	if err != nil {
		return nil, err
	}
	return parseScanResults(reply), nil
}

// parseScanResults reads the tab separated scan table. The input comes from the
// radio environment and is therefore untrusted: every field is optional and no
// malformed line may abort the parse.
func parseScanResults(reply string) []domain.AccessPoint {
	best := map[string]domain.AccessPoint{}
	for _, line := range strings.Split(reply, "\n") {
		cols := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(cols) < 5 || !strings.Contains(cols[0], ":") {
			continue
		}
		ssid := cols[4]
		if ssid == "" || domain.ValidateSSID(ssid) != nil {
			continue // hidden or malformed, nothing safe to show
		}

		ap := domain.AccessPoint{SSID: ssid, BSSID: cols[0], Flags: cols[3]}
		ap.Freq, _ = strconv.Atoi(cols[1])
		ap.Signal, _ = strconv.Atoi(cols[2])
		ap.Band = domain.Band(ap.Freq)
		ap.Quality = domain.Quality(ap.Signal)
		ap.Security = domain.SecurityFromFlags(ap.Flags)

		if prev, ok := best[ssid]; !ok || ap.Signal > prev.Signal {
			best[ssid] = ap
		}
	}

	out := make([]domain.AccessPoint, 0, len(best))
	for _, ap := range best {
		out = append(out, ap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Signal > out[j].Signal })
	return out
}

// Status reports the live association state.
func (a *Adapter) Status(ctx context.Context, link string) (domain.WiFiStatus, error) {
	reply, err := a.request(link, "STATUS", requestTimeout)
	if err != nil {
		return domain.WiFiStatus{}, err
	}

	fields := map[string]string{}
	for _, line := range strings.Split(reply, "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			fields[key] = value
		}
	}

	st := domain.WiFiStatus{
		State: fields["wpa_state"],
		SSID:  fields["ssid"],
		BSSID: fields["bssid"],
	}
	st.Freq, _ = strconv.Atoi(fields["freq"])
	st.ProfileID = -1
	if id, err := strconv.Atoi(fields["id"]); err == nil {
		st.ProfileID = id
	}
	st.Associated = st.State == "COMPLETED"

	if poll, err := a.request(link, "SIGNAL_POLL", requestTimeout); err == nil {
		for _, line := range strings.Split(poll, "\n") {
			if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok && key == "RSSI" {
				st.Signal, _ = strconv.Atoi(value)
			}
		}
	}
	return st, nil
}

// Profiles lists the networks stored by wpa_supplicant.
func (a *Adapter) Profiles(ctx context.Context, link string) ([]domain.Profile, error) {
	reply, err := a.request(link, "LIST_NETWORKS", requestTimeout)
	if err != nil {
		return nil, err
	}

	var out []domain.Profile
	for _, line := range strings.Split(reply, "\n") {
		cols := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(cols) < 2 {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(cols[0]))
		if err != nil {
			continue
		}

		p := domain.Profile{ID: id, SSID: cols[1]}
		if len(cols) > 2 {
			p.BSSID = cols[2]
		}
		if len(cols) > 3 {
			p.Flags = cols[3]
		}
		p.Current = strings.Contains(p.Flags, "[CURRENT]")
		p.Enabled = !strings.Contains(p.Flags, "[DISABLED]")
		out = append(out, p)
	}
	return out, nil
}

// Upsert replaces any profile with the same SSID and selects the new one. The
// returned string is a non-fatal warning, e.g. when the config cannot persist.
func (a *Adapter) Upsert(ctx context.Context, req domain.WiFiRequest) (int, domain.Message, error) {
	var noWarning domain.Message
	if err := req.Validate(); err != nil {
		return 0, noWarning, err
	}

	existing, err := a.Profiles(ctx, req.Link)
	if err != nil {
		return 0, noWarning, err
	}
	for _, p := range existing {
		if p.SSID == req.SSID {
			if err := a.mustOK(req.Link, "REMOVE_NETWORK "+strconv.Itoa(p.ID)); err != nil {
				return 0, noWarning, err
			}
		}
	}

	reply, err := a.request(req.Link, "ADD_NETWORK", requestTimeout)
	if err != nil {
		return 0, noWarning, err
	}
	id, err := strconv.Atoi(strings.TrimSpace(reply))
	if err != nil {
		return 0, noWarning, domain.Internal("ADD_NETWORK returned an unexpected reply: %s", strings.TrimSpace(reply))
	}

	if err := a.configure(req, id); err != nil {
		_, _ = a.request(req.Link, "REMOVE_NETWORK "+strconv.Itoa(id), requestTimeout)
		return 0, noWarning, err
	}
	if err := a.mustOK(req.Link, "ENABLE_NETWORK "+strconv.Itoa(id)); err != nil {
		return 0, noWarning, err
	}
	if err := a.mustOK(req.Link, "SELECT_NETWORK "+strconv.Itoa(id)); err != nil {
		return 0, noWarning, err
	}

	var warning domain.Message
	if err := a.mustOK(req.Link, "SAVE_CONFIG"); err != nil {
		warning = domain.Msg("Connected, but the configuration could not be saved. Add update_config=1 to wpa_supplicant.conf so it survives a reboot.")
	}
	return id, warning, nil
}

// configure writes the profile fields. SSID and credentials are sent hex encoded,
// which removes all quoting ambiguity from the control protocol.
func (a *Adapter) configure(req domain.WiFiRequest, id int) error {
	nid := strconv.Itoa(id)
	set := func(key, value string) error {
		return a.mustOK(req.Link, fmt.Sprintf("SET_NETWORK %s %s %s", nid, key, value))
	}

	if err := set("ssid", hex.EncodeToString([]byte(req.SSID))); err != nil {
		return err
	}
	if req.Hidden {
		if err := set("scan_ssid", "1"); err != nil {
			return err
		}
	}

	pass := hex.EncodeToString([]byte(req.Passphrase.Reveal()))
	switch req.Security {
	case domain.SecOpen:
		return set("key_mgmt", "NONE")
	case domain.SecPSK:
		if err := set("key_mgmt", "WPA-PSK"); err != nil {
			return err
		}
		return set("psk", domain.WPAPSK(req.SSID, req.Passphrase))
	case domain.SecSAE:
		if err := set("key_mgmt", "SAE"); err != nil {
			return err
		}
		if err := set("ieee80211w", "2"); err != nil {
			return err
		}
		return set("sae_password", pass)
	case domain.SecPSKSAE:
		if err := set("key_mgmt", "WPA-PSK SAE"); err != nil {
			return err
		}
		if err := set("ieee80211w", "1"); err != nil {
			return err
		}
		if err := set("psk", domain.WPAPSK(req.SSID, req.Passphrase)); err != nil {
			return err
		}
		return set("sae_password", pass)
	}
	return domain.Invalid("invalid security type: %q", string(req.Security))
}

// Secret returns the credential stored for a profile. wpa_supplicant refuses to
// hand out psk over the control socket, so the configuration file is the only
// source available.
func (a *Adapter) Secret(ctx context.Context, link string, id int) (domain.ProfileSecret, error) {
	profiles, err := a.Profiles(ctx, link)
	if err != nil {
		return domain.ProfileSecret{}, err
	}
	for _, p := range profiles {
		if p.ID == id {
			return readConfSecret(a.confDir, link, p.SSID)
		}
	}
	return domain.ProfileSecret{}, domain.NotFound("no saved network with id %d on %s", id, link)
}

// Select activates an already stored profile.
func (a *Adapter) Select(ctx context.Context, link string, id int) error {
	if id < 0 {
		return domain.Invalid("invalid network id")
	}
	return a.mustOK(link, "SELECT_NETWORK "+strconv.Itoa(id))
}

// Remove deletes a stored profile and persists the change.
func (a *Adapter) Remove(ctx context.Context, link string, id int) error {
	if id < 0 {
		return domain.Invalid("invalid network id")
	}
	if err := a.mustOK(link, "REMOVE_NETWORK "+strconv.Itoa(id)); err != nil {
		return err
	}
	return a.mustOK(link, "SAVE_CONFIG")
}

func (a *Adapter) Disconnect(ctx context.Context, link string) error {
	return a.mustOK(link, "DISCONNECT")
}

func (a *Adapter) Reconnect(ctx context.Context, link string) error {
	return a.mustOK(link, "RECONNECT")
}

// Close tears down every control connection.
func (a *Adapter) Close() error {
	a.mu.Lock()
	sessions := make([]*linkSession, 0, len(a.links))
	for _, s := range a.links {
		sessions = append(sessions, s)
	}
	a.links = map[string]*linkSession{}
	a.mu.Unlock()

	for _, s := range sessions {
		s.close()
	}
	return nil
}

// notifier is a one-to-many wakeup used to park scan callers until results land.
type notifier struct {
	mu      sync.Mutex
	waiters map[chan struct{}]struct{}
}

func newNotifier() *notifier {
	return &notifier{waiters: map[chan struct{}]struct{}{}}
}

func (n *notifier) wait() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	n.waiters[ch] = struct{}{}
	n.mu.Unlock()

	return ch, func() {
		n.mu.Lock()
		delete(n.waiters, ch)
		n.mu.Unlock()
	}
}

func (n *notifier) broadcast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for ch := range n.waiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func stripPriority(msg string) string {
	if strings.HasPrefix(msg, "<") {
		if i := strings.IndexByte(msg, '>'); i > 0 {
			return msg[i+1:]
		}
	}
	return msg
}

func isTimeout(err error) bool {
	var netErr interface{ Timeout() bool }
	return errors.As(err, &netErr) && netErr.Timeout()
}
