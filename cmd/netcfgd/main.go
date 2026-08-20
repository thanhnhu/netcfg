// Command netcfgd is the privileged agent. It is the only process that touches
// wpa_supplicant, the IP backends and the kernel, and it owns the rollback timer
// so a change is reverted even if the web tier or the operator's link dies.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"netcfg/internal/adapters/hotspot"
	"netcfg/internal/adapters/ipbackend"
	"netcfg/internal/adapters/ipbackend/ifupdown"
	"netcfg/internal/adapters/ipbackend/networkd"
	"netcfg/internal/adapters/ipbackend/nm"
	"netcfg/internal/adapters/linkinfo"
	"netcfg/internal/adapters/prober"
	"netcfg/internal/adapters/routectl"
	"netcfg/internal/adapters/sshctl"
	"netcfg/internal/adapters/store/fsstore"
	"netcfg/internal/adapters/sysinfo"
	"netcfg/internal/adapters/wifibackend"
	"netcfg/internal/adapters/wifibackend/nmwifi"
	"netcfg/internal/adapters/wpactrl"
	"netcfg/internal/app"
	"netcfg/internal/domain"
	"netcfg/internal/platform/clock"
	"netcfg/internal/platform/eventbus"
	"netcfg/internal/platform/logging"
	"netcfg/internal/rpc"
)

func main() {
	var (
		socketPath    = flag.String("socket", "/run/netcfgd/netcfgd.sock", "Unix socket served to the web tier")
		socketGroup   = flag.String("socket-group", "netcfg", "group allowed to reach the socket")
		allowUsers    = flag.String("allow-users", "netcfg", "comma separated users allowed to call the agent")
		ctrlDir       = flag.String("ctrl-dir", "/run/wpa_supplicant", "wpa_supplicant control socket directory")
		ctrlLocalDir  = flag.String("ctrl-local-dir", "/run/netcfgd", "where to bind the local socket wpa_supplicant replies to")
		wpaConfDir    = flag.String("wpa-conf-dir", "/etc/wpa_supplicant", "wpa_supplicant configuration directory")
		stateDir      = flag.String("state-dir", "/var/lib/netcfgd", "where the desired state and history are stored")
		networkDir    = flag.String("network-dir", networkd.DefaultDir, "systemd-networkd unit directory")
		ifupdownDir   = flag.String("ifupdown-dir", ifupdown.DefaultDir, "ifupdown drop-in directory")
		confirmWindow = flag.Duration("confirm-window", 90*time.Second, "default window to confirm a change")
		allowSSH      = flag.Bool("allow-ssh", false, "let the web UI open the device's SSH server for a diagnostic window")
		probeTargets  = flag.String("probe-targets", "", "extra host:port connectivity probes, besides the gateway")
		failover      = flag.Bool("failover-monitor", true, "probe every default route and demote one that stops forwarding")
		failoverEvery = flag.Duration("failover-interval", 10*time.Second, "how often the failover monitor probes each link")
		failoverFails = flag.Int("failover-fails", 3, "consecutive failed probes before a link is demoted")
		failoverBack  = flag.Int("failover-recovers", 2, "consecutive good probes before a demoted link is restored")
		apEnabled     = flag.Bool("ap-fallback", true, "start the fallback access point when connectivity is lost")
		apAfter       = flag.Duration("ap-fallback-after", 5*time.Minute, "how long without connectivity before the fallback AP starts")
		apAutoStop    = flag.Bool("ap-auto-stop", true, "stop the fallback AP once connectivity returns")
		apSSID        = flag.String("ap-ssid", "", "fallback AP SSID (defaults to netcfg-<mac>)")
		apPassphrase  = flag.String("ap-passphrase", "", "fallback AP passphrase (generated when empty)")
		apChannel     = flag.Int("ap-channel", 6, "fallback AP 2.4GHz channel")
		apCountry     = flag.String("ap-country", "VN", "hostapd country code")
		apAddress     = flag.String("ap-address", domain.DefaultHotspotAddress, "portal address of the fallback AP")
		logFormat     = flag.String("log-format", "text", "log format: text or json")
		logLevel      = flag.String("log-level", "info", "log level: debug, info, warn, error")
	)
	flag.Parse()

	log := logging.New(*logFormat, *logLevel, "netcfgd")

	if os.Geteuid() != 0 {
		log.Warn("netcfgd is not running as root; configuration changes will most likely fail")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bus := eventbus.New()
	supplicant := wpactrl.New(*ctrlDir, *ctrlLocalDir, *wpaConfDir, bus, log)
	defer supplicant.Close()

	// NetworkManager first: where it owns the radio it keeps wpa_supplicant as a
	// private child, so no control socket exists for the direct path to use.
	wifi := wifibackend.New(log, nmwifi.New(), supplicant)

	fallbackAP := hotspot.New(*ctrlLocalDir, log)

	// NetworkManager first: when it runs it claims devices exclusively, and
	// writing through another backend would leave two managers fighting.
	router := ipbackend.NewRegistry(log,
		nm.New(),
		networkd.New(*networkDir),
		ifupdown.New(*ifupdownDir),
	)

	agent, err := app.NewAgent(ctx, app.Deps{
		Links:   linkinfo.New(),
		WiFi:    wifi,
		IP:      router,
		Store:   fsstore.New(*stateDir),
		Prober:  prober.New(splitList(*probeTargets)),
		Routes:  routectl.New(),
		Clock:   clock.Real{},
		Pub:     bus,
		Hotspot: fallbackAP,
		Sys:     sysinfo.New(),
		SSH:     sshctl.New(),
		Log:     log,
	}, app.Config{
		DefaultConfirmWindow: *confirmWindow,
		AllowSSHToggle:       *allowSSH,
		ProbeTargets:         splitList(*probeTargets),
		Failover: app.FailoverPolicy{
			Enabled:  *failover,
			Interval: *failoverEvery,
			Fails:    *failoverFails,
			Recovers: *failoverBack,
		},
	}, app.HotspotPolicy{
		Enabled:    *apEnabled,
		After:      *apAfter,
		CheckEvery: 20 * time.Second,
		AutoStop:   *apAutoStop,
		Config: domain.HotspotConfig{
			SSID:       *apSSID,
			Passphrase: domain.NewSecret(*apPassphrase),
			Channel:    *apChannel,
			Country:    *apCountry,
			Address:    *apAddress,
		},
	})
	if err != nil {
		log.Error("cannot start the agent", "err", err)
		os.Exit(1)
	}

	if err := agent.Reconcile(ctx); err != nil {
		log.Warn("startup reconciliation failed", "err", err)
	}

	go agent.WatchConnectivity(ctx)
	go agent.WatchFailover(ctx)
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = fallbackAP.Stop(stopCtx)
	}()

	server := rpc.NewServer(rpc.ServerConfig{
		SocketPath:  *socketPath,
		AllowedUIDs: resolveUIDs(*allowUsers, log),
		GID:         resolveGID(*socketGroup, log),
	}, agent, bus, log)

	if err := server.Serve(ctx); err != nil {
		log.Error("agent stopped", "err", err)
		os.Exit(1)
	}
	log.Info("agent shut down")
}

// resolveUIDs turns user names into ids. Root is always allowed so the socket
// stays usable for local administration.
func resolveUIDs(list string, log interface{ Warn(string, ...any) }) []uint32 {
	uids := []uint32{0}
	for _, name := range splitList(list) {
		u, err := user.Lookup(name)
		if err != nil {
			log.Warn("ignoring an unknown user", "user", name, "err", err)
			continue
		}
		id, err := strconv.ParseUint(u.Uid, 10, 32)
		if err != nil {
			continue
		}
		uids = append(uids, uint32(id))
	}
	return uids
}

func resolveGID(name string, log interface{ Warn(string, ...any) }) int {
	if name == "" {
		return -1
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		log.Warn("ignoring an unknown group", "group", name, "err", err)
		return -1
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return -1
	}
	return gid
}

func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
