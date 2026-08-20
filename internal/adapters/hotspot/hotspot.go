// Package hotspot runs the fallback access point (hostapd + dnsmasq) that makes
// a headless device recoverable when no known network is reachable.
package hotspot

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"netcfg/internal/adapters/sysexec"
	"netcfg/internal/domain"
	"netcfg/internal/platform/fileutil"
)

const virtualIface = "ap0"

// Manager implements ports.Hotspot.
type Manager struct {
	runtimeDir string
	log        *slog.Logger

	mu         sync.Mutex
	status     domain.HotspotStatus
	cfg        domain.HotspotConfig
	iface      string
	createdIf  bool
	stoppedWPA string
	cancel     context.CancelFunc
	procs      []*exec.Cmd
}

// New returns a manager writing generated configs into runtimeDir.
func New(runtimeDir string, log *slog.Logger) *Manager {
	if runtimeDir == "" {
		runtimeDir = "/run/netcfgd"
	}
	return &Manager{runtimeDir: runtimeDir, log: log}
}

// Available reports whether the tooling needed for a fallback AP is installed.
func (m *Manager) Available() error {
	for _, binary := range []string{"hostapd", "dnsmasq", "iw", "ip"} {
		if !sysexec.Available(binary) {
			return domain.Unavailable("%s is missing; install it with: apt install hostapd dnsmasq iw", binary)
		}
	}
	return nil
}

// Status returns the current state of the fallback AP.
func (m *Manager) Status(ctx context.Context) domain.HotspotStatus {
	m.mu.Lock()
	status := m.status
	m.mu.Unlock()

	if status.Active {
		status.Clients = m.countLeases()
	}
	return status
}

// Start brings up the fallback access point.
func (m *Manager) Start(ctx context.Context, cfg domain.HotspotConfig, reason domain.Message) error {
	if err := m.Available(); err != nil {
		return err
	}

	m.mu.Lock()
	if m.status.Active {
		m.mu.Unlock()
		return domain.Conflict("the fallback access point is already running")
	}
	m.mu.Unlock()

	if err := domain.ValidateLinkName(cfg.Link); err != nil {
		return err
	}
	if err := os.MkdirAll(m.runtimeDir, 0o750); err != nil {
		return domain.Internal("create %s: %v", m.runtimeDir, err)
	}

	iface, mode, created := m.prepareInterface(ctx, cfg.Link)
	if mode == domain.HotspotExclusive {
		m.stopSupplicant(ctx, cfg.Link)
	}

	if err := m.configureAddress(ctx, iface, cfg.Address); err != nil {
		m.cleanup(context.Background(), iface, created, cfg.Link)
		return err
	}

	hostapdConf := filepath.Join(m.runtimeDir, "hostapd.conf")
	dnsmasqConf := filepath.Join(m.runtimeDir, "dnsmasq.conf")
	if err := fileutil.WriteAtomic(hostapdConf, renderHostapd(cfg, iface), 0o600); err != nil {
		m.cleanup(context.Background(), iface, created, cfg.Link)
		return err
	}
	if err := fileutil.WriteAtomic(dnsmasqConf, renderDnsmasq(cfg, iface, m.runtimeDir), 0o644); err != nil {
		m.cleanup(context.Background(), iface, created, cfg.Link)
		return err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	hostapd := exec.CommandContext(runCtx, "hostapd", hostapdConf)
	dnsmasq := exec.CommandContext(runCtx, "dnsmasq", "--keep-in-foreground", "--conf-file="+dnsmasqConf)

	for _, cmd := range []*exec.Cmd{hostapd, dnsmasq} {
		cmd.Env = []string{"LC_ALL=C", "PATH=" + sysexec.SafePath}
		if err := cmd.Start(); err != nil {
			cancel()
			m.cleanup(context.Background(), iface, created, cfg.Link)
			return domain.Unavailable("cannot start %s: %v", cmd.Path, err)
		}
		go m.supervise(cmd)
	}

	m.mu.Lock()
	m.cfg = cfg
	m.iface = iface
	m.createdIf = created
	m.cancel = cancel
	m.procs = []*exec.Cmd{hostapd, dnsmasq}
	m.status = domain.HotspotStatus{
		Active:     true,
		Link:       iface,
		Mode:       mode,
		SSID:       cfg.SSID,
		Passphrase: cfg.Passphrase.Reveal(),
		Address:    cfg.Address,
		PortalURL:  fmt.Sprintf("http://%s/", addressOnly(cfg.Address)),
		Since:      time.Now(),
		Reason:     reason,
	}
	m.mu.Unlock()

	m.log.Warn("fallback access point started",
		"iface", iface, "mode", mode, "ssid", cfg.SSID, "portal", addressOnly(cfg.Address), "reason", reason.String())
	return nil
}

// Stop tears the access point down and restores the client role.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	if !m.status.Active {
		m.mu.Unlock()
		return nil
	}
	cancel, iface, created, link := m.cancel, m.iface, m.createdIf, m.cfg.Link
	m.status = domain.HotspotStatus{}
	m.cancel = nil
	m.procs = nil
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	m.cleanup(ctx, iface, created, link)
	m.log.Info("fallback access point stopped", "iface", iface)
	return nil
}

// prepareInterface prefers a virtual AP interface so the client role can keep
// running; not every chipset supports that combination.
func (m *Manager) prepareInterface(ctx context.Context, link string) (iface string, mode domain.HotspotMode, created bool) {
	_, _ = sysexec.Run(ctx, "iw", "dev", virtualIface, "del")
	if _, err := sysexec.Run(ctx, "iw", "dev", link, "interface", "add", virtualIface, "type", "__ap"); err == nil {
		return virtualIface, domain.HotspotConcurrent, true
	}
	m.log.Info("the Wi-Fi chipset cannot run AP and client at once, falling back to exclusive mode", "link", link)
	return link, domain.HotspotExclusive, false
}

func (m *Manager) stopSupplicant(ctx context.Context, link string) {
	unit := "wpa_supplicant@" + link + ".service"
	if _, err := sysexec.Run(ctx, "systemctl", "stop", unit); err != nil {
		m.log.Warn("cannot stop wpa_supplicant", "unit", unit, "err", err)
		return
	}
	m.mu.Lock()
	m.stoppedWPA = unit
	m.mu.Unlock()
}

func (m *Manager) configureAddress(ctx context.Context, iface, address string) error {
	if _, err := sysexec.Run(ctx, "ip", "addr", "flush", "dev", iface); err != nil {
		return err
	}
	if _, err := sysexec.Run(ctx, "ip", "addr", "add", address, "dev", iface); err != nil {
		return err
	}
	_, err := sysexec.Run(ctx, "ip", "link", "set", "dev", iface, "up")
	return err
}

func (m *Manager) cleanup(ctx context.Context, iface string, created bool, link string) {
	_, _ = sysexec.Run(ctx, "ip", "addr", "flush", "dev", iface)
	if created {
		_, _ = sysexec.Run(ctx, "iw", "dev", iface, "del")
	}

	m.mu.Lock()
	unit := m.stoppedWPA
	m.stoppedWPA = ""
	m.mu.Unlock()

	if unit != "" {
		if _, err := sysexec.Run(ctx, "systemctl", "start", unit); err != nil {
			m.log.Error("cannot restart wpa_supplicant", "unit", unit, "err", err)
		}
	}
	_ = link
}

func (m *Manager) supervise(cmd *exec.Cmd) {
	if err := cmd.Wait(); err != nil {
		m.mu.Lock()
		active := m.status.Active
		m.mu.Unlock()
		if active {
			m.log.Warn("fallback access point process exited", "cmd", filepath.Base(cmd.Path), "err", err)
		}
	}
}

func (m *Manager) countLeases() int {
	data, err := os.ReadFile(filepath.Join(m.runtimeDir, "dnsmasq.leases"))
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func renderHostapd(cfg domain.HotspotConfig, iface string) []byte {
	var b strings.Builder
	b.WriteString("# Generated by netcfgd for fallback access point mode.\n")
	fmt.Fprintf(&b, "interface=%s\n", iface)
	b.WriteString("driver=nl80211\n")
	fmt.Fprintf(&b, "ssid=%s\n", cfg.SSID)
	fmt.Fprintf(&b, "country_code=%s\n", cfg.Country)
	b.WriteString("hw_mode=g\n")
	fmt.Fprintf(&b, "channel=%d\n", cfg.Channel)
	b.WriteString("ieee80211n=1\nwmm_enabled=1\n")
	b.WriteString("auth_algs=1\nignore_broadcast_ssid=0\n")
	// WPA2 rather than an open network: anyone in radio range would otherwise be
	// able to reconfigure the device.
	b.WriteString("wpa=2\nwpa_key_mgmt=WPA-PSK\nrsn_pairwise=CCMP\n")
	fmt.Fprintf(&b, "wpa_passphrase=%s\n", cfg.Passphrase.Reveal())
	return []byte(b.String())
}

func renderDnsmasq(cfg domain.HotspotConfig, iface, runtimeDir string) []byte {
	host := addressOnly(cfg.Address)
	base := strings.Join(strings.Split(host, ".")[:3], ".")

	var b strings.Builder
	b.WriteString("# Generated by netcfgd for fallback access point mode.\n")
	fmt.Fprintf(&b, "interface=%s\n", iface)
	b.WriteString("bind-interfaces\nexcept-interface=lo\n")
	b.WriteString("no-resolv\nno-hosts\nlog-facility=-\n")
	fmt.Fprintf(&b, "dhcp-leasefile=%s\n", filepath.Join(runtimeDir, "dnsmasq.leases"))
	fmt.Fprintf(&b, "dhcp-range=%s.50,%s.150,255.255.255.0,12h\n", base, base)
	fmt.Fprintf(&b, "dhcp-option=option:router,%s\n", host)
	fmt.Fprintf(&b, "dhcp-option=option:dns-server,%s\n", host)
	// Wildcard DNS is what turns this into a captive portal: every lookup lands
	// on the device, so the phone's connectivity check triggers the sign-in page.
	fmt.Fprintf(&b, "address=/#/%s\n", host)
	return []byte(b.String())
}

func addressOnly(cidr string) string {
	host, _, found := strings.Cut(cidr, "/")
	if !found {
		return cidr
	}
	return host
}
