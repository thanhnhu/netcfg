package app

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"netcfg/internal/domain"
	"netcfg/internal/platform/clock"
	"netcfg/internal/ports"
)

// ---------- fakes ----------

type fakeLinks struct{ links []domain.Link }

func (f *fakeLinks) Links(context.Context) ([]domain.Link, error) { return f.links, nil }
func (f *fakeLinks) SetUp(context.Context, string) error          { return nil }

type fakeIP struct {
	mu      sync.Mutex
	current domain.IPPlan
	applied []domain.IPPlan
	failOn  string
}

func (f *fakeIP) Kind() domain.BackendKind { return domain.BackendNetworkd }

func (f *fakeIP) KindFor(context.Context, string) domain.BackendKind {
	return domain.BackendNetworkd
}

func (f *fakeIP) Detect(context.Context) ([]string, error) { return []string{"eth0"}, nil }

func (f *fakeIP) Current(_ context.Context, link string) (domain.IPPlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, nil
}

func (f *fakeIP) Apply(_ context.Context, plan domain.IPPlan) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn != "" && plan.Address == f.failOn {
		return domain.Unavailable("simulated apply failure")
	}
	f.applied = append(f.applied, plan)
	f.current = plan
	return nil
}

func (f *fakeIP) history() []domain.IPPlan {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.IPPlan(nil), f.applied...)
}

type fakeStore struct {
	mu    sync.Mutex
	state domain.DesiredState
	good  domain.DesiredState
}

func newFakeStore() *fakeStore {
	return &fakeStore{state: domain.NewDesiredState(), good: domain.NewDesiredState()}
}

func (f *fakeStore) Load(context.Context) (domain.DesiredState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.Clone(), nil
}

func (f *fakeStore) Save(_ context.Context, s domain.DesiredState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s.Clone()
	return nil
}

func (f *fakeStore) LastKnownGood(context.Context) (domain.DesiredState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.good.Clone(), nil
}

func (f *fakeStore) MarkGood(_ context.Context, s domain.DesiredState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.good = s.Clone()
	return nil
}

type fakeProber struct {
	mu sync.Mutex
	ok bool
	// perLink overrides ok for a named interface, which is what the failover
	// monitor tests need: one path healthy, another not.
	perLink map[string]bool
}

func (f *fakeProber) Probe(context.Context, domain.Link) domain.ProbeResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return domain.ProbeResult{OK: f.ok, Detail: domain.Msg("simulated")}
}

func (f *fakeProber) ProbeVia(_ context.Context, link domain.Link) domain.ProbeResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := f.ok
	if override, found := f.perLink[link.Name]; found {
		ok = override
	}
	return domain.ProbeResult{OK: ok, Detail: domain.Msg("simulated")}
}

func (f *fakeProber) set(link string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.perLink == nil {
		f.perLink = map[string]bool{}
	}
	f.perLink[link] = ok
}

type fakeSupplicant struct{}

func (fakeSupplicant) Scan(context.Context, string) ([]domain.AccessPoint, error) { return nil, nil }
func (fakeSupplicant) Status(context.Context, string) (domain.WiFiStatus, error) {
	return domain.WiFiStatus{ProfileID: -1}, nil
}
func (fakeSupplicant) Profiles(context.Context, string) ([]domain.Profile, error) { return nil, nil }
func (fakeSupplicant) Secret(context.Context, string, int) (domain.ProfileSecret, error) {
	return domain.ProfileSecret{}, nil
}
func (fakeSupplicant) Upsert(context.Context, domain.WiFiRequest) (int, domain.Message, error) {
	return 0, domain.Message{}, nil
}
func (fakeSupplicant) Select(context.Context, string, int) error { return nil }
func (fakeSupplicant) Remove(context.Context, string, int) error { return nil }
func (fakeSupplicant) Disconnect(context.Context, string) error  { return nil }
func (fakeSupplicant) Reconnect(context.Context, string) error   { return nil }
func (fakeSupplicant) Close() error                              { return nil }

// fakeHotspot reports whatever state a test needs the radio to be in.
type fakeHotspot struct {
	status  domain.HotspotStatus
	stopped bool
}

func (f *fakeHotspot) Available() error { return nil }
func (f *fakeHotspot) Start(context.Context, domain.HotspotConfig, domain.Message) error {
	return nil
}
func (f *fakeHotspot) Stop(context.Context) error                  { f.stopped = true; return nil }
func (f *fakeHotspot) Status(context.Context) domain.HotspotStatus { return f.status }

type recorder struct {
	mu     sync.Mutex
	events []domain.Event
}

func (r *recorder) Publish(evt domain.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, evt)
}

func (r *recorder) has(t domain.EventType) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Type == t {
			return true
		}
	}
	return false
}

// ---------- harness ----------

type harness struct {
	agent  *Agent
	ip     *fakeIP
	store  *fakeStore
	clock  *clock.Fake
	events *recorder
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	ip := &fakeIP{current: domain.IPPlan{Link: "eth0", Mode: domain.ModeDHCP, Mode6: domain.ModeAuto}}
	store := newFakeStore()
	fake := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	events := &recorder{}

	agent, err := NewAgent(context.Background(), Deps{
		Links: &fakeLinks{links: []domain.Link{{
			Name: "eth0", AdminUp: true, OperUp: true,
			Addresses: []string{"192.168.1.10/24"}, Gateway: "192.168.1.1",
		}}},
		WiFi:   fakeSupplicant{},
		IP:     ip,
		Store:  store,
		Prober: &fakeProber{ok: true},
		Clock:  fake,
		Pub:    events,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, Config{DefaultConfirmWindow: 90 * time.Second, ProbeGrace: time.Millisecond}, HotspotPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{agent: agent, ip: ip, store: store, clock: fake, events: events}
}

var staticPlan = domain.IPPlan{
	Link: "eth0", Mode: domain.ModeStatic, Address: "10.0.0.5/24", Gateway: "10.0.0.1",
	Mode6: domain.ModeAuto,
}

// ---------- tests ----------

func TestDisruptiveChangeRollsBackWhenNotConfirmed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	pending, err := h.agent.ApplyIP(ctx, staticPlan, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil {
		t.Fatal("a disruptive change must create a pending confirmation")
	}
	if h.agent.Pending() == nil {
		t.Fatal("the agent must record the pending change")
	}

	h.clock.Advance(91 * time.Second)

	if h.agent.Pending() != nil {
		t.Fatal("nothing may stay pending after the window expires")
	}
	history := h.ip.history()
	if len(history) != 2 {
		t.Fatalf("expected 2 applies (change then rollback), got %d", len(history))
	}
	if history[1].Mode != domain.ModeDHCP {
		t.Fatalf("must roll back to DHCP, got %+v", history[1])
	}
	if !h.events.has(domain.EventApplyReverted) {
		t.Fatal("a rollback event must be published")
	}
}

func TestConfirmDisarmsRollback(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	pending, err := h.agent.ApplyIP(ctx, staticPlan, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.agent.Confirm(ctx, pending.Generation); err != nil {
		t.Fatal(err)
	}

	h.clock.Advance(10 * time.Minute)

	if got := len(h.ip.history()); got != 1 {
		t.Fatalf("a confirmed change must not roll back, applies = %d", got)
	}
	good, _ := h.store.LastKnownGood(ctx)
	if good.Links["eth0"].IP == nil || good.Links["eth0"].IP.Address != staticPlan.Address {
		t.Fatal("a confirmed config must become last known good")
	}
}

func TestManualRollbackRestoresPreviousConfig(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	pending, err := h.agent.ApplyIP(ctx, staticPlan, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.agent.Rollback(ctx, pending.Generation); err != nil {
		t.Fatal(err)
	}

	history := h.ip.history()
	if history[len(history)-1].Mode != domain.ModeDHCP {
		t.Fatal("a manual rollback must restore the previous config")
	}
	if h.agent.Pending() != nil {
		t.Fatal("nothing may stay pending after a rollback")
	}
}

func TestNonDisruptiveChangeCommitsImmediately(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	pending, err := h.agent.ApplyIP(ctx, domain.IPPlan{
		Link: "eth0", Mode: domain.ModeDHCP, Mode6: domain.ModeAuto, DNS: []string{"1.1.1.1"},
	}, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pending != nil {
		t.Fatal("a DNS-only change must not require confirmation")
	}

	h.clock.Advance(10 * time.Minute)
	if got := len(h.ip.history()); got != 1 {
		t.Fatalf("a safe change must not roll back, applies = %d", got)
	}
}

func TestNoRollbackOptOut(t *testing.T) {
	h := newHarness(t)

	pending, err := h.agent.ApplyIP(context.Background(), staticPlan, ApplyOptions{NoRollback: true})
	if err != nil {
		t.Fatal(err)
	}
	if pending != nil {
		t.Fatal("opting out of rollback must leave nothing pending")
	}

	h.clock.Advance(10 * time.Minute)
	if got := len(h.ip.history()); got != 1 {
		t.Fatalf("must not roll back after the operator opted out, applies = %d", got)
	}
}

func TestSecondApplyIsRejectedWhileOneIsPending(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.agent.ApplyIP(ctx, staticPlan, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}

	_, err := h.agent.ApplyIP(ctx, domain.IPPlan{
		Link: "eth0", Mode: domain.ModeStatic, Address: "10.0.0.9/24", Gateway: "10.0.0.1",
	}, ApplyOptions{})
	if err == nil {
		t.Fatal("a second change must be rejected while one is pending")
	}
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("expected the conflict code, got %s", domain.CodeOf(err))
	}
}

func TestFailedApplyLeavesNothingPending(t *testing.T) {
	h := newHarness(t)
	h.ip.failOn = staticPlan.Address

	if _, err := h.agent.ApplyIP(context.Background(), staticPlan, ApplyOptions{}); err == nil {
		t.Fatal("a failed apply must return an error")
	}
	if h.agent.Pending() != nil {
		t.Fatal("a failed apply must leave nothing pending")
	}
}

func TestReconcileAppliesStoredState(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	state := domain.NewDesiredState()
	plan := staticPlan
	state.Links["eth0"] = domain.LinkDesired{IP: &plan}
	if err := h.store.Save(ctx, state); err != nil {
		t.Fatal(err)
	}

	if err := h.agent.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.ip.history(); len(got) != 1 || got[0].Address != staticPlan.Address {
		t.Fatalf("reconcile must apply the stored config, got %+v", got)
	}

	// Reconciling again must be a no-op: the operation has to be idempotent.
	if err := h.agent.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(h.ip.history()); got != 1 {
		t.Fatalf("reconcile must be idempotent, applies = %d", got)
	}
}

var _ ports.Store = (*fakeStore)(nil)
var _ ports.Supplicant = fakeSupplicant{}
var _ ports.LinkInspector = (*fakeLinks)(nil)
var _ IPRouter = (*fakeIP)(nil)

// TestAnExclusiveHotspotBlocksWiFiCalls covers the failure the fallback AP
// produced in the field: wpa_supplicant is stopped, so reaching for it only
// bought a ten second timeout and a socket path the operator cannot act on.
func TestAnExclusiveHotspotBlocksWiFiCalls(t *testing.T) {
	h := newHarness(t)
	ap := &fakeHotspot{status: domain.HotspotStatus{
		Active: true, Mode: domain.HotspotExclusive, Link: "wlan0",
	}}
	h.agent.hotspot = ap

	if err := h.agent.radioTaken(context.Background(), "wlan0"); err == nil {
		t.Fatal("expected the exclusive access point to block the radio")
	}
	if err := h.agent.radioTaken(context.Background(), "wlan1"); err != nil {
		t.Fatalf("another link must stay usable: %v", err)
	}

	ap.status.Mode = domain.HotspotConcurrent
	if err := h.agent.radioTaken(context.Background(), "wlan0"); err != nil {
		t.Fatalf("concurrent mode leaves the client role alive: %v", err)
	}

	ap.status.Active = false
	ap.status.Mode = domain.HotspotExclusive
	if err := h.agent.radioTaken(context.Background(), "wlan0"); err != nil {
		t.Fatalf("an inactive access point must not block anything: %v", err)
	}
}

// TestTheWatchdogLeavesAManualHotspotAlone covers the surprise a real device
// produced: starting the access point deliberately, only to have the watchdog
// tear it down twenty seconds later because the wired link was healthy.
func TestTheWatchdogLeavesAManualHotspotAlone(t *testing.T) {
	h := newHarness(t)
	ap := &fakeHotspot{status: domain.HotspotStatus{Active: true, Link: "wlan0"}}
	h.agent.hotspot = ap
	h.agent.links = &fakeLinks{links: []domain.Link{
		{Name: "wlan0", Wireless: true, AdminUp: true, OperUp: true},
	}}
	h.agent.hotspotPolicy = HotspotPolicy{Enabled: true, AutoStop: true, After: time.Minute}

	if err := h.agent.StartHotspot(context.Background(), "wlan0"); err != nil {
		t.Fatalf("StartHotspot: %v", err)
	}
	if h.agent.hotspotIsAuto() {
		t.Fatal("an access point the operator asked for must not be marked automatic")
	}

	// What the watchdog does once it sees connectivity again.
	if h.agent.hotspotIsAuto() {
		t.Fatal("the watchdog would stop an access point it did not start")
	}

	// An access point the watchdog raised is its own to take down.
	h.agent.setHotspotAuto(true)
	if !h.agent.hotspotIsAuto() {
		t.Fatal("an automatic access point must stay eligible for auto-stop")
	}

	// Stopping clears the mark so the next manual start is not mistaken for one.
	if err := h.agent.StopHotspot(context.Background()); err != nil {
		t.Fatalf("StopHotspot: %v", err)
	}
	if h.agent.hotspotIsAuto() {
		t.Fatal("stopping must clear the automatic mark")
	}
}
