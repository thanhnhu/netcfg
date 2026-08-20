// Package app contains the use cases. It orchestrates ports and owns the
// commit-confirm safety net that prevents an operator from locking themselves
// out of the device.
package app

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"netcfg/internal/domain"
	"netcfg/internal/ports"
)

// Config tunes the safety behaviour of the agent.
type Config struct {
	// DefaultConfirmWindow is used when the caller does not specify one.
	DefaultConfirmWindow time.Duration
	// ProbeGrace is how long to wait after an apply before testing connectivity.
	ProbeGrace time.Duration
	// AllowSSHToggle opts into opening the device's SSH server from the web UI.
	// Off by default: it widens what a compromised web tier can reach for.
	AllowSSHToggle bool
	// Failover tunes the active monitor that watches the paths themselves.
	Failover FailoverPolicy
	// ProbeTargets is shown on the failover panel so an operator can see what the
	// monitor actually tests; the prober holds the same list.
	ProbeTargets []string
}

// IPRouter is an IPBackend that can also report which backend owns a link.
type IPRouter interface {
	ports.IPBackend
	KindFor(ctx context.Context, link string) domain.BackendKind
}

// Agent is the single writer for network configuration on this host.
type Agent struct {
	links   ports.LinkInspector
	wifi    ports.Supplicant
	ip      IPRouter
	store   ports.Store
	prober  ports.Prober
	routes  ports.RouteControl
	clock   ports.Clock
	pub     ports.Publisher
	hotspot ports.Hotspot
	sys     ports.SystemInfo
	ssh     ports.SSHControl
	log     *slog.Logger
	cfg     Config

	hotspotPolicy  HotspotPolicy
	failoverPolicy FailoverPolicy
	probeTargets   []string
	failover       failoverState

	// applyMu serialises configuration changes; the network is a single resource.
	applyMu sync.Mutex

	mu      sync.Mutex
	pending *pendingApply
	gen     atomic.Uint64

	// sshTimer closes the diagnostic window again. It is only ever armed for a
	// server netcfgd started itself.
	sshMu      sync.Mutex
	sshTimer   ports.Timer
	sshStopsAt time.Time
	// sshOpenedFirewall records that netcfgd added the firewall rule, so closing
	// the window removes only what it put there.
	sshOpenedFirewall bool

	// hotspotAuto records that the watchdog raised the access point. Only an AP
	// this agent started on its own may be taken down on its own; one the
	// operator asked for stays until they say otherwise.
	hotspotMu   sync.Mutex
	hotspotAuto bool
}

type pendingApply struct {
	info  domain.PendingApply
	undo  func(context.Context) error
	timer ports.Timer
	state domain.DesiredState
}

// Deps bundles the ports an agent needs.
type Deps struct {
	Links   ports.LinkInspector
	WiFi    ports.Supplicant
	IP      IPRouter
	Store   ports.Store
	Prober  ports.Prober
	Routes  ports.RouteControl
	Clock   ports.Clock
	Pub     ports.Publisher
	Hotspot ports.Hotspot
	Sys     ports.SystemInfo
	SSH     ports.SSHControl
	Log     *slog.Logger
}

// NewAgent wires an agent and seeds the generation counter from the store.
func NewAgent(ctx context.Context, d Deps, cfg Config, hotspot HotspotPolicy) (*Agent, error) {
	if cfg.DefaultConfirmWindow <= 0 {
		cfg.DefaultConfirmWindow = domain.DefaultConfirmWindow
	}
	if cfg.ProbeGrace <= 0 {
		cfg.ProbeGrace = 5 * time.Second
	}

	a := &Agent{
		links: d.Links, wifi: d.WiFi, ip: d.IP, store: d.Store,
		prober: d.Prober, routes: d.Routes, clock: d.Clock, pub: d.Pub, hotspot: d.Hotspot,
		sys: d.Sys, ssh: d.SSH, log: d.Log, cfg: cfg, hotspotPolicy: hotspot,
		failoverPolicy: cfg.Failover, probeTargets: cfg.ProbeTargets,
	}
	a.failover.health = map[string]domain.LinkHealth{}
	a.failover.original = map[string]uint32{}

	state, err := d.Store.Load(ctx)
	if err != nil {
		return nil, err
	}
	a.gen.Store(uint64(state.Generation))
	return a, nil
}

// ---------- read side ----------

// Links returns the interfaces of this host.
func (a *Agent) Links(ctx context.Context) ([]domain.Link, error) {
	return a.links.Links(ctx)
}

// SystemStats reports host health: load, memory, disks and temperatures.
func (a *Agent) SystemStats(ctx context.Context) (domain.SystemStats, error) {
	if a.sys == nil {
		return domain.SystemStats{}, domain.Unavailable("host statistics are not available on this platform")
	}
	return a.sys.Stats(ctx)
}

// SSHStatus reports whether remote access is open, and until when.
func (a *Agent) SSHStatus(ctx context.Context) (domain.SSHStatus, error) {
	if err := a.sshAllowed(); err != nil {
		return domain.SSHStatus{Detail: domain.MessageOf(err)}, nil
	}

	status, err := a.ssh.Status(ctx)
	if err != nil {
		return status, err
	}

	a.sshMu.Lock()
	status.StopsAt = a.sshStopsAt
	a.sshMu.Unlock()
	return status, nil
}

// SSHEnable opens remote access and arms a timer to close it again. A server
// the operator enabled at boot is left alone: closing it later would take away
// access they arranged deliberately.
func (a *Agent) SSHEnable(ctx context.Context, window time.Duration) (domain.SSHStatus, error) {
	if err := a.sshAllowed(); err != nil {
		return domain.SSHStatus{}, err
	}

	before, err := a.ssh.Status(ctx)
	if err != nil {
		return before, err
	}
	if !before.Available {
		return before, domain.Unavailable("no SSH server is installed")
	}
	openedFirewall, err := a.ssh.Start(ctx)
	if err != nil {
		return before, err
	}

	window = domain.ClampSSHWindow(window)
	a.sshMu.Lock()
	if a.sshTimer != nil {
		a.sshTimer.Stop()
		a.sshTimer = nil
		a.sshStopsAt = time.Time{}
	}
	a.sshOpenedFirewall = a.sshOpenedFirewall || openedFirewall
	if !before.EnabledAtBoot {
		a.sshStopsAt = a.clock.Now().Add(window)
		a.sshTimer = a.clock.AfterFunc(window, a.closeSSHWindow)
	}
	a.sshMu.Unlock()

	a.log.Warn("SSH access opened", "window", window, "permanent", before.EnabledAtBoot, "firewallOpened", openedFirewall)
	return a.SSHStatus(ctx)
}

// SSHDisable closes remote access now.
func (a *Agent) SSHDisable(ctx context.Context) (domain.SSHStatus, error) {
	if err := a.sshAllowed(); err != nil {
		return domain.SSHStatus{}, err
	}

	a.sshMu.Lock()
	if a.sshTimer != nil {
		a.sshTimer.Stop()
		a.sshTimer = nil
	}
	a.sshStopsAt = time.Time{}
	closeFirewall := a.sshOpenedFirewall
	a.sshOpenedFirewall = false
	a.sshMu.Unlock()

	if err := a.ssh.Stop(ctx, closeFirewall); err != nil {
		return domain.SSHStatus{}, err
	}
	a.log.Warn("SSH access closed")
	return a.SSHStatus(ctx)
}

func (a *Agent) sshAllowed() error {
	if a.ssh == nil || !a.cfg.AllowSSHToggle {
		return domain.Unavailable("remote access control is disabled; start netcfgd with -allow-ssh")
	}
	return nil
}

func (a *Agent) closeSSHWindow() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	a.sshMu.Lock()
	a.sshTimer = nil
	a.sshStopsAt = time.Time{}
	closeFirewall := a.sshOpenedFirewall
	a.sshOpenedFirewall = false
	a.sshMu.Unlock()

	if err := a.ssh.Stop(ctx, closeFirewall); err != nil {
		a.log.Error("cannot close the SSH window", "err", err)
		return
	}
	a.log.Warn("SSH access closed automatically")
}

// LinkView is everything the UI needs about one interface.
type LinkView struct {
	Link     domain.Link          `json:"link"`
	Backend  domain.BackendKind   `json:"backend"`
	IP       domain.IPPlan        `json:"ip"`
	WiFi     *domain.WiFiStatus   `json:"wifi,omitempty"`
	Profiles []domain.Profile     `json:"profiles,omitempty"`
	Pending  *domain.PendingApply `json:"pending,omitempty"`
	Hotspot  domain.HotspotStatus `json:"hotspot"`
	Notices  []domain.Message     `json:"notices,omitempty"`
}

// Snapshot aggregates the state of one link, degrading gracefully when a
// subsystem is unavailable so the UI can still render and explain the problem.
func (a *Agent) Snapshot(ctx context.Context, name string) (LinkView, error) {
	links, err := a.links.Links(ctx)
	if err != nil {
		return LinkView{}, err
	}
	link, err := a.pickLink(ctx, links, name)
	if err != nil {
		return LinkView{}, err
	}

	view := LinkView{Link: link, Backend: a.ip.KindFor(ctx, link.Name)}

	if plan, err := a.ip.Current(ctx, link.Name); err == nil {
		view.IP = plan
	} else {
		view.IP = domain.IPPlan{Link: link.Name, Mode: domain.ModeDHCP, Mode6: domain.ModeAuto}
		view.Notices = append(view.Notices, domain.MessageOf(err))
	}

	if link.Wireless {
		if err := a.radioTaken(ctx, link.Name); err != nil {
			view.Notices = append(view.Notices, domain.MessageOf(err))
		} else {
			if status, err := a.wifi.Status(ctx, link.Name); err == nil {
				view.WiFi = &status
			} else {
				view.Notices = append(view.Notices, domain.MessageOf(err))
			}
			if profiles, err := a.wifi.Profiles(ctx, link.Name); err == nil {
				view.Profiles = profiles
			}
		}
	}

	view.Pending = a.Pending()
	view.Hotspot = a.HotspotStatus(ctx)
	return view, nil
}

// Scan performs a Wi-Fi scan on a wireless link.
func (a *Agent) Scan(ctx context.Context, name string) ([]domain.AccessPoint, error) {
	link, err := a.wirelessLink(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := a.radioTaken(ctx, link.Name); err != nil {
		return nil, err
	}
	if err := a.links.SetUp(ctx, link.Name); err != nil {
		return nil, err
	}
	return a.wifi.Scan(ctx, link.Name)
}

// radioTaken reports that an exclusive fallback AP holds the radio. In that
// state wpa_supplicant has been stopped, so asking it anything only buys a
// timeout and an error about a socket the operator cannot act on.
func (a *Agent) radioTaken(ctx context.Context, link string) error {
	if a.hotspot == nil {
		return nil
	}
	status := a.hotspot.Status(ctx)
	if status.Active && status.Mode == domain.HotspotExclusive && status.Link == link {
		return domain.Unavailable("the fallback access point is using the radio on %s; stop it to manage Wi-Fi", link)
	}
	return nil
}

// PlanIP shows what an IP change would do without touching anything.
func (a *Agent) PlanIP(ctx context.Context, plan domain.IPPlan) (domain.Diff, error) {
	normalized, err := plan.Normalize()
	if err != nil {
		return domain.Diff{}, err
	}
	links, err := a.links.Links(ctx)
	if err != nil {
		return domain.Diff{}, err
	}
	if _, err := domain.FindLink(links, normalized.Link); err != nil {
		return domain.Diff{}, err
	}

	current, err := a.ip.Current(ctx, normalized.Link)
	if err != nil {
		return domain.Diff{}, err
	}
	return domain.DiffIP(current, normalized, a.ip.KindFor(ctx, normalized.Link)), nil
}

// Pending returns the change awaiting confirmation, if any.
func (a *Agent) Pending() *domain.PendingApply {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending == nil {
		return nil
	}
	info := a.pending.info
	return &info
}

// ---------- write side ----------

// ApplyOptions controls the safety net around a change.
type ApplyOptions struct {
	// ConfirmWindow is how long the operator has to confirm.
	ConfirmWindow time.Duration
	// NoRollback opts out of automatic rollback. Use when the operator knowingly
	// moves the device to a network they cannot reach from the current session.
	NoRollback bool
}

// ApplyIP changes addressing under the commit-confirm safety net. A change that
// cannot break the session is committed immediately.
func (a *Agent) ApplyIP(ctx context.Context, plan domain.IPPlan, opts ApplyOptions) (*domain.PendingApply, error) {
	normalized, err := plan.Normalize()
	if err != nil {
		return nil, err
	}
	diff, err := a.PlanIP(ctx, normalized)
	if err != nil {
		return nil, err
	}
	previous, err := a.ip.Current(ctx, normalized.Link)
	if err != nil {
		return nil, err
	}
	if len(diff.Changes) == 0 {
		return nil, nil
	}

	state, err := a.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	next := state.Clone()
	entry := next.Links[normalized.Link]
	planCopy := normalized
	entry.IP = &planCopy
	next.Links[normalized.Link] = entry

	return a.commit(ctx, commitRequest{
		kind:       domain.ChangeIP,
		link:       normalized.Link,
		summary:    diff.Changes,
		disruptive: diff.Disruptive,
		state:      next,
		opts:       opts,
		do: func(ctx context.Context) (func(context.Context) error, error) {
			if err := a.ip.Apply(ctx, normalized); err != nil {
				return nil, err
			}
			return func(ctx context.Context) error { return a.ip.Apply(ctx, previous) }, nil
		},
	})
}

// ApplyWiFi joins a network under the commit-confirm safety net.
func (a *Agent) ApplyWiFi(ctx context.Context, req domain.WiFiRequest, opts ApplyOptions) (*domain.PendingApply, domain.Message, error) {
	var warning domain.Message
	if err := req.Validate(); err != nil {
		return nil, warning, err
	}
	link, err := a.wirelessLink(ctx, req.Link)
	if err != nil {
		return nil, warning, err
	}

	// An exclusive fallback AP owns the radio, so it must go before the client
	// role can associate with the network the operator just entered.
	if a.hotspot != nil {
		if status := a.hotspot.Status(ctx); status.Active && status.Mode == domain.HotspotExclusive {
			a.log.Info("stopping the fallback access point to apply Wi-Fi settings", "link", link.Name)
			if err := a.hotspot.Stop(ctx); err != nil {
				return nil, warning, err
			}
		}
	}

	if err := a.links.SetUp(ctx, link.Name); err != nil {
		return nil, warning, err
	}

	before, err := a.wifi.Status(ctx, link.Name)
	if err != nil {
		return nil, warning, err
	}

	state, err := a.store.Load(ctx)
	if err != nil {
		return nil, warning, err
	}
	next := state.Clone()
	entry := next.Links[link.Name]
	entry.WiFiSSID = req.SSID
	next.Links[link.Name] = entry

	pending, err := a.commit(ctx, commitRequest{
		kind:       domain.ChangeWiFi,
		link:       link.Name,
		summary:    []domain.Change{{Field: "ssid", From: before.SSID, To: req.SSID}},
		disruptive: true,
		state:      next,
		opts:       opts,
		do: func(ctx context.Context) (func(context.Context) error, error) {
			id, warn, err := a.wifi.Upsert(ctx, req)
			if err != nil {
				return nil, err
			}
			warning = warn
			previousID := before.ProfileID
			return func(ctx context.Context) error {
				if err := a.wifi.Remove(ctx, link.Name, id); err != nil {
					return err
				}
				if previousID >= 0 {
					return a.wifi.Select(ctx, link.Name, previousID)
				}
				return a.wifi.Reconnect(ctx, link.Name)
			}, nil
		},
	})
	return pending, warning, err
}

type commitRequest struct {
	kind       domain.ChangeKind
	link       string
	summary    []domain.Change
	disruptive bool
	state      domain.DesiredState
	opts       ApplyOptions
	do         func(context.Context) (func(context.Context) error, error)
}

// commit performs a change and, when it could cut the operator's session, arms
// a rollback timer that only an explicit confirmation can disarm.
func (a *Agent) commit(ctx context.Context, req commitRequest) (*domain.PendingApply, error) {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()

	if p := a.Pending(); p != nil {
		return nil, domain.Conflict("a change is already awaiting confirmation (generation %s); confirm or roll it back first", p.Generation)
	}

	generation := domain.Generation(a.gen.Add(1))
	req.state.Generation = generation

	undo, err := req.do(ctx)
	if err != nil {
		return nil, err
	}

	immediate := !req.disruptive || req.opts.NoRollback
	if immediate {
		if err := a.persistGood(ctx, req.state); err != nil {
			return nil, err
		}
		a.log.Info("change applied", "generation", generation, "kind", req.kind, "link", req.link, "rollback", false)
		a.pub.Publish(domain.NewEvent(domain.EventApplyConfirmed, req.link, domain.Msg("Change applied"), generation))
		return nil, nil
	}

	window := domain.ClampConfirmWindow(orDefault(req.opts.ConfirmWindow, a.cfg.DefaultConfirmWindow))
	now := a.clock.Now()
	info := domain.PendingApply{
		Generation: generation,
		Kind:       req.kind,
		Link:       req.link,
		StartedAt:  now,
		Deadline:   now.Add(window),
		Summary:    req.summary,
	}

	a.mu.Lock()
	a.pending = &pendingApply{info: info, undo: undo, state: req.state}
	a.pending.timer = a.clock.AfterFunc(window, func() { a.expire(generation) })
	a.mu.Unlock()

	a.log.Warn("change awaiting confirmation", "generation", generation, "kind", req.kind,
		"link", req.link, "deadline", info.Deadline)
	a.pub.Publish(domain.NewEvent(domain.EventApplyPending, req.link,
		domain.Msg("Confirmation required before the device rolls back"), info))

	go a.probeLater(generation, req.link)
	return &info, nil
}

// Confirm disarms the rollback timer and promotes the change to last known good.
func (a *Agent) Confirm(ctx context.Context, generation domain.Generation) error {
	a.mu.Lock()
	pending := a.pending
	if pending == nil || pending.info.Generation != generation {
		a.mu.Unlock()
		return domain.NotFound("no change is awaiting confirmation with generation %s", generation)
	}
	a.pending = nil
	a.mu.Unlock()

	if pending.timer != nil {
		pending.timer.Stop()
	}
	if err := a.persistGood(ctx, pending.state); err != nil {
		return err
	}

	a.log.Info("change confirmed", "generation", generation, "link", pending.info.Link)
	a.pub.Publish(domain.NewEvent(domain.EventApplyConfirmed, pending.info.Link, domain.Msg("Change confirmed"), generation))
	return nil
}

// Rollback reverts a pending change immediately at operator request.
func (a *Agent) Rollback(ctx context.Context, generation domain.Generation) error {
	a.mu.Lock()
	pending := a.pending
	if pending == nil || pending.info.Generation != generation {
		a.mu.Unlock()
		return domain.NotFound("no change is awaiting confirmation with generation %s", generation)
	}
	a.pending = nil
	a.mu.Unlock()

	if pending.timer != nil {
		pending.timer.Stop()
	}
	return a.revert(ctx, pending, "rolled back on request")
}

// expire is the rollback timer callback. It runs inside the agent, not inside a
// request, so it still fires when the browser or the web tier is gone.
func (a *Agent) expire(generation domain.Generation) {
	a.mu.Lock()
	pending := a.pending
	if pending == nil || pending.info.Generation != generation {
		a.mu.Unlock()
		return
	}
	a.pending = nil
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = a.revert(ctx, pending, "confirmation window expired")
}

func (a *Agent) revert(ctx context.Context, pending *pendingApply, reason string) error {
	a.log.Warn("restoring previous configuration", "generation", pending.info.Generation,
		"link", pending.info.Link, "reason", reason)

	if err := pending.undo(ctx); err != nil {
		a.log.Error("rollback failed", "generation", pending.info.Generation, "err", err)
		a.pub.Publish(domain.NewEvent(domain.EventApplyReverted, pending.info.Link,
			domain.Msg("Rollback failed: %s", domain.MessageOf(err)), pending.info.Generation))
		return err
	}

	a.pub.Publish(domain.NewEvent(domain.EventApplyReverted, pending.info.Link,
		domain.Msg("Restored the previous configuration (%s)", reason), pending.info.Generation))
	return nil
}

// probeLater reports connectivity as a hint; it never auto-confirms, because
// only a request from the browser proves the operator still has access.
func (a *Agent) probeLater(generation domain.Generation, name string) {
	time.Sleep(a.cfg.ProbeGrace)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	links, err := a.links.Links(ctx)
	if err != nil {
		return
	}
	link, err := domain.FindLink(links, name)
	if err != nil {
		return
	}
	result := a.prober.Probe(ctx, link)

	a.mu.Lock()
	if a.pending != nil && a.pending.info.Generation == generation {
		a.pending.info.Probe = result
	}
	a.mu.Unlock()

	a.pub.Publish(domain.NewEvent(domain.EventProbe, name, result.Detail, result))
}
func (a *Agent) persistGood(ctx context.Context, state domain.DesiredState) error {
	if err := a.store.Save(ctx, state); err != nil {
		return err
	}
	return a.store.MarkGood(ctx, state)
}

// ---------- profile management ----------

// SelectProfile activates a stored Wi-Fi profile.
func (a *Agent) SelectProfile(ctx context.Context, name string, id int) error {
	link, err := a.wirelessLink(ctx, name)
	if err != nil {
		return err
	}
	return a.wifi.Select(ctx, link.Name, id)
}

// ProfileSecret returns the credential stored for a Wi-Fi profile.
func (a *Agent) ProfileSecret(ctx context.Context, name string, id int) (domain.ProfileSecret, error) {
	link, err := a.wirelessLink(ctx, name)
	if err != nil {
		return domain.ProfileSecret{}, err
	}
	a.log.Warn("revealing a stored Wi-Fi credential", "link", link.Name, "id", id)
	return a.wifi.Secret(ctx, link.Name, id)
}

// RemoveProfile deletes a stored Wi-Fi profile.
func (a *Agent) RemoveProfile(ctx context.Context, name string, id int) error {
	link, err := a.wirelessLink(ctx, name)
	if err != nil {
		return err
	}
	return a.wifi.Remove(ctx, link.Name, id)
}

// Disconnect drops the current association.
func (a *Agent) Disconnect(ctx context.Context, name string) error {
	link, err := a.wirelessLink(ctx, name)
	if err != nil {
		return err
	}
	return a.wifi.Disconnect(ctx, link.Name)
}

// Reconnect re-enables automatic association.
func (a *Agent) Reconnect(ctx context.Context, name string) error {
	link, err := a.wirelessLink(ctx, name)
	if err != nil {
		return err
	}
	return a.wifi.Reconnect(ctx, link.Name)
}

// Reconcile drives the host towards the stored desired state. It runs at
// startup so a reboot cannot silently lose the operator's configuration.
func (a *Agent) Reconcile(ctx context.Context) error {
	state, err := a.store.Load(ctx)
	if err != nil {
		return err
	}
	if len(state.Links) == 0 {
		return nil
	}

	links, err := a.links.Links(ctx)
	if err != nil {
		return err
	}

	for name, desired := range state.Links {
		if desired.IP == nil {
			continue
		}
		if _, err := domain.FindLink(links, name); err != nil {
			a.log.Info("skipping reconciliation for a link that does not exist", "link", name)
			continue
		}
		current, err := a.ip.Current(ctx, name)
		if err != nil {
			a.log.Warn("cannot read the running configuration", "link", name, "err", err)
			continue
		}
		if current.Equal(*desired.IP) {
			continue
		}
		a.log.Info("reconciling IP configuration", "link", name)
		if err := a.ip.Apply(ctx, *desired.IP); err != nil {
			a.log.Error("reconciliation failed", "link", name, "err", err)
		}
	}
	return nil
}

func (a *Agent) wirelessLink(ctx context.Context, name string) (domain.Link, error) {
	links, err := a.links.Links(ctx)
	if err != nil {
		return domain.Link{}, err
	}
	link, err := domain.FindLink(links, name)
	if err != nil {
		return domain.Link{}, err
	}
	if !link.Wireless {
		return domain.Link{}, domain.Invalid("%s is not a wireless interface", link.Name)
	}
	return link, nil
}

// pickLink resolves the requested interface. Without one it prefers the
// wireless device wpa_supplicant answers for, since virtual Wi-Fi Direct
// devices such as p2p0 sort first yet have no control socket.
func (a *Agent) pickLink(ctx context.Context, links []domain.Link, name string) (domain.Link, error) {
	if name != "" {
		return domain.FindLink(links, name)
	}
	fallback := -1
	for i, l := range links {
		if !l.Wireless {
			continue
		}
		if fallback < 0 {
			fallback = i
		}
		if _, err := a.wifi.Status(ctx, l.Name); err == nil {
			return l, nil
		}
	}
	if fallback >= 0 {
		return links[fallback], nil
	}
	if len(links) > 0 {
		return links[0], nil
	}
	return domain.Link{}, domain.NotFound("no network interface found")
}

func orDefault(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}
