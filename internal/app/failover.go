package app

import (
	"context"
	"sort"
	"sync"
	"time"

	"netcfg/internal/domain"
)

// FailoverPolicy tunes the active monitor. Kernel routing already reacts to a
// link going down; this covers the other half — a link that stays up while the
// path behind it stops working.
type FailoverPolicy struct {
	Enabled bool
	// Interval is how often every candidate link is probed.
	Interval time.Duration
	// Fails is how many consecutive failures demote a link, Recovers how many
	// consecutive successes bring it back. Both exist so one lost packet, or one
	// lucky reply, never moves the default route.
	Fails    int
	Recovers int
	// DemotedMetric is where a failing link is parked in the routing table.
	DemotedMetric uint32
}

func (p FailoverPolicy) withDefaults() FailoverPolicy {
	if p.Interval <= 0 {
		p.Interval = 10 * time.Second
	}
	if p.Fails <= 0 {
		p.Fails = 3
	}
	if p.Recovers <= 0 {
		p.Recovers = 2
	}
	if p.DemotedMetric == 0 {
		p.DemotedMetric = domain.DemotedMetric
	}
	return p
}

// failoverState is the monitor's memory between ticks.
type failoverState struct {
	mu     sync.Mutex
	health map[string]domain.LinkHealth
	// original records the metric a demoted link had before the monitor touched
	// it, which is the only way back to exactly where it was.
	original map[string]uint32
	detail   domain.Message
	at       time.Time
}

// FailoverStatus reports what the monitor currently believes, for the panel.
func (a *Agent) FailoverStatus(ctx context.Context) domain.FailoverStatus {
	policy := a.failoverPolicy.withDefaults()
	status := domain.FailoverStatus{
		Enabled:  a.failoverPolicy.Enabled && a.routes != nil,
		Interval: policy.Interval,
		Fails:    policy.Fails,
		Recovers: policy.Recovers,
		Targets:  a.probeTargets,
		At:       a.clock.Now(),
	}

	a.failover.mu.Lock()
	defer a.failover.mu.Unlock()

	status.Detail = a.failover.detail
	if !a.failover.at.IsZero() {
		status.At = a.failover.at
	}
	for _, health := range a.failover.health {
		status.Links = append(status.Links, health)
	}
	sort.Slice(status.Links, func(i, j int) bool { return status.Links[i].Link < status.Links[j].Link })
	return status
}

// WatchFailover probes every interface that carries a default route and parks
// the ones that stop forwarding, so the kernel picks the next candidate. It
// only ever demotes a link while another one is healthy: an outage that affects
// everything must leave the routing table alone.
func (a *Agent) WatchFailover(ctx context.Context) {
	if !a.failoverPolicy.Enabled {
		return
	}
	if a.routes == nil {
		a.log.Warn("active failover disabled", "err", "no route control")
		return
	}
	if err := a.routes.Available(); err != nil {
		a.log.Warn("active failover disabled", "err", err)
		a.setFailoverDetail(domain.MessageOf(err))
		return
	}

	policy := a.failoverPolicy.withDefaults()
	ticker := time.NewTicker(policy.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		a.checkFailover(ctx, policy)
	}
}

// checkFailover runs one round: probe, then act on what changed.
func (a *Agent) checkFailover(ctx context.Context, policy FailoverPolicy) {
	links, err := a.links.Links(ctx)
	if err != nil {
		a.setFailoverDetail(domain.MessageOf(err))
		return
	}

	candidates := make([]domain.Link, 0, len(links))
	for _, link := range links {
		// Only links that can carry the default route are the monitor's
		// business; the hotspot interface and loopback are not.
		if link.Name == "lo" || link.Gateway == "" {
			continue
		}
		candidates = append(candidates, link)
	}

	now := a.clock.Now()
	updated := make(map[string]domain.LinkHealth, len(candidates))
	healthy := 0
	for _, link := range candidates {
		result := a.prober.ProbeVia(ctx, link)
		health := a.mergeHealth(link, result, now)
		updated[link.Name] = health
		if health.Healthy() {
			healthy++
		}
	}

	changed := a.storeHealth(updated, now)

	for _, link := range candidates {
		health := updated[link.Name]
		switch {
		case !health.Healthy() && !health.Demoted && health.Failures >= policy.Fails:
			// Demoting the last working path would only cut the device off.
			if healthy == 0 {
				continue
			}
			if a.demote(ctx, link, policy) {
				changed = true
			}
		case health.Healthy() && health.Demoted && health.Successes >= policy.Recovers:
			if a.promote(ctx, link) {
				changed = true
			}
		}
	}

	if changed {
		a.pub.Publish(domain.NewEvent(domain.EventFailover, "",
			domain.Msg("Failover status changed"), a.FailoverStatus(ctx)))
	}
}

// mergeHealth folds one probe result into the running counters.
func (a *Agent) mergeHealth(link domain.Link, result domain.ProbeResult, now time.Time) domain.LinkHealth {
	a.failover.mu.Lock()
	previous, seen := a.failover.health[link.Name]
	demoted := previous.Demoted
	a.failover.mu.Unlock()

	health := domain.LinkHealth{
		Link:      link.Name,
		Gateway:   link.Gateway,
		AdminUp:   link.AdminUp,
		OperUp:    link.OperUp,
		Reachable: result.OK,
		Detail:    result.Detail,
		Demoted:   demoted,
		Since:     previous.Since,
		CheckedAt: now,
		Metric:    previous.Metric,
	}
	if result.OK {
		health.Successes = previous.Successes + 1
	} else {
		health.Failures = previous.Failures + 1
	}
	if !seen || previous.Healthy() != health.Healthy() || health.Since.IsZero() {
		health.Since = now
	}
	return health
}

func (a *Agent) storeHealth(updated map[string]domain.LinkHealth, now time.Time) bool {
	a.failover.mu.Lock()
	defer a.failover.mu.Unlock()

	changed := len(a.failover.health) != len(updated)
	for name, health := range updated {
		previous, seen := a.failover.health[name]
		if !seen || previous.Healthy() != health.Healthy() {
			changed = true
		}
	}
	a.failover.health = updated
	a.failover.at = now
	return changed
}

// demote parks a link far down the routing table. The kernel then hands the
// default route to the next candidate on its own.
func (a *Agent) demote(ctx context.Context, link domain.Link, policy FailoverPolicy) bool {
	gateway, metric, ok, err := a.routes.DefaultRoute(ctx, link.Name)
	if err != nil || !ok {
		return false
	}
	if gateway == "" {
		gateway = link.Gateway
	}
	if metric >= policy.DemotedMetric {
		return false
	}
	if err := a.routes.MoveDefault(ctx, link.Name, gateway, metric, policy.DemotedMetric); err != nil {
		a.log.Warn("cannot demote a failing link", "link", link.Name, "err", err)
		return false
	}

	a.log.Info("link demoted by the failover monitor", "link", link.Name, "from", metric, "to", policy.DemotedMetric)
	a.failover.mu.Lock()
	a.failover.original[link.Name] = metric
	if health, seen := a.failover.health[link.Name]; seen {
		health.Demoted = true
		health.Metric = policy.DemotedMetric
		a.failover.health[link.Name] = health
	}
	a.failover.mu.Unlock()
	return true
}

// promote puts a recovered link back exactly where the operator had it.
func (a *Agent) promote(ctx context.Context, link domain.Link) bool {
	a.failover.mu.Lock()
	original, known := a.failover.original[link.Name]
	a.failover.mu.Unlock()
	if !known {
		return false
	}

	gateway, metric, ok, err := a.routes.DefaultRoute(ctx, link.Name)
	if err != nil || !ok {
		return false
	}
	if gateway == "" {
		gateway = link.Gateway
	}
	if err := a.routes.MoveDefault(ctx, link.Name, gateway, metric, original); err != nil {
		a.log.Warn("cannot restore a recovered link", "link", link.Name, "err", err)
		return false
	}

	a.log.Info("link restored by the failover monitor", "link", link.Name, "metric", original)
	a.failover.mu.Lock()
	delete(a.failover.original, link.Name)
	if health, seen := a.failover.health[link.Name]; seen {
		health.Demoted = false
		health.Metric = original
		a.failover.health[link.Name] = health
	}
	a.failover.mu.Unlock()
	return true
}

func (a *Agent) setFailoverDetail(detail domain.Message) {
	a.failover.mu.Lock()
	a.failover.detail = detail
	a.failover.mu.Unlock()
}
