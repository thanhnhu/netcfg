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
)

// fakeRoutes stands in for iproute2 and records what the monitor asked for.
type fakeRoutes struct {
	mu      sync.Mutex
	metrics map[string]uint32
	moves   int
	fail    bool
}

func newFakeRoutes(metrics map[string]uint32) *fakeRoutes {
	return &fakeRoutes{metrics: metrics}
}

func (f *fakeRoutes) Available() error { return nil }

func (f *fakeRoutes) DefaultRoute(_ context.Context, link string) (string, uint32, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	metric, ok := f.metrics[link]
	return "10.0.0.1", metric, ok, nil
}

func (f *fakeRoutes) MoveDefault(_ context.Context, link, _ string, _, to uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return domain.Unavailable("simulated failure")
	}
	f.metrics[link] = to
	f.moves++
	return nil
}

func (f *fakeRoutes) metric(link string) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.metrics[link]
}

// failoverHarness wires an agent with two routed interfaces.
type failoverHarness struct {
	agent  *Agent
	routes *fakeRoutes
	prober *fakeProber
	links  *fakeLinks
	policy FailoverPolicy
}

func newFailoverHarness(t *testing.T) *failoverHarness {
	t.Helper()

	links := &fakeLinks{links: []domain.Link{
		{Name: "eth0", AdminUp: true, OperUp: true, Addresses: []string{"10.0.0.5/24"}, Gateway: "10.0.0.1"},
		{Name: "wlan0", AdminUp: true, OperUp: true, Addresses: []string{"10.0.1.5/24"}, Gateway: "10.0.1.1", Wireless: true},
		{Name: "lo", AdminUp: true, OperUp: true},
	}}
	routes := newFakeRoutes(map[string]uint32{"eth0": 100, "wlan0": 600})
	prober := &fakeProber{ok: true}
	policy := FailoverPolicy{Enabled: true, Interval: time.Second, Fails: 2, Recovers: 2}

	agent, err := NewAgent(context.Background(), Deps{
		Links:  links,
		WiFi:   fakeSupplicant{},
		IP:     &fakeIP{current: domain.IPPlan{Link: "eth0", Mode: domain.ModeDHCP}},
		Store:  newFakeStore(),
		Prober: prober,
		Routes: routes,
		Clock:  clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		Pub:    &recorder{},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, Config{Failover: policy}, HotspotPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	return &failoverHarness{agent: agent, routes: routes, prober: prober, links: links, policy: policy.withDefaults()}
}

func (h *failoverHarness) check(rounds int) {
	for i := 0; i < rounds; i++ {
		h.agent.checkFailover(context.Background(), h.policy)
	}
}

func TestFailoverDemotesALinkThatStopsForwarding(t *testing.T) {
	h := newFailoverHarness(t)

	h.prober.set("eth0", false)
	h.check(1)
	if h.routes.metric("eth0") != 100 {
		t.Fatalf("one failure already demoted eth0 to %d; the threshold is %d", h.routes.metric("eth0"), h.policy.Fails)
	}

	h.check(1)
	if got := h.routes.metric("eth0"); got != domain.DemotedMetric {
		t.Errorf("eth0 metric = %d, want %d after %d failures", got, domain.DemotedMetric, h.policy.Fails)
	}

	status := h.agent.FailoverStatus(context.Background())
	health, ok := status.Health("eth0")
	if !ok || !health.Demoted || health.Reachable {
		t.Errorf("status = %+v, want eth0 demoted and unreachable", health)
	}
	if other, _ := status.Health("wlan0"); other.Demoted {
		t.Error("wlan0 was demoted as well; only the failing link may move")
	}
}

// The monitor exists to keep the device reachable. Demoting the last working
// path would do the opposite, so a total outage must leave routing alone.
func TestFailoverKeepsTheLastPathWhenEverythingFails(t *testing.T) {
	h := newFailoverHarness(t)

	h.prober.ok = false
	h.check(5)

	if h.routes.moves != 0 {
		t.Errorf("routing changed %d times while no link was healthy", h.routes.moves)
	}
}

func TestFailoverRestoresTheOriginalMetricOnRecovery(t *testing.T) {
	h := newFailoverHarness(t)

	h.prober.set("eth0", false)
	h.check(2)
	if h.routes.metric("eth0") != domain.DemotedMetric {
		t.Fatalf("eth0 was not demoted, metric = %d", h.routes.metric("eth0"))
	}

	h.prober.set("eth0", true)
	h.check(2)
	if got := h.routes.metric("eth0"); got != 100 {
		t.Errorf("eth0 metric = %d, want the original 100 back", got)
	}
	if health, _ := h.agent.FailoverStatus(context.Background()).Health("eth0"); health.Demoted {
		t.Error("eth0 is still marked demoted after recovering")
	}
}

// A link the kernel reports as down is unhealthy whatever the probe says, and
// it must not be counted as the healthy peer that justifies a demotion.
func TestFailoverTreatsADownLinkAsUnhealthy(t *testing.T) {
	h := newFailoverHarness(t)

	h.links.links[1].OperUp = false
	h.prober.set("wlan0", false)
	h.prober.set("eth0", false)
	h.check(3)

	if h.routes.moves != 0 {
		t.Errorf("routing changed %d times with no healthy link left", h.routes.moves)
	}
	if health, _ := h.agent.FailoverStatus(context.Background()).Health("wlan0"); health.OperUp {
		t.Error("wlan0 is reported up although the kernel says otherwise")
	}
}
