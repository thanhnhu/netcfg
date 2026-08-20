package app

import (
	"context"
	"net/netip"
	"time"

	"netcfg/internal/domain"
)

// HotspotPolicy controls the fallback access point.
type HotspotPolicy struct {
	// Enabled turns automatic fallback on. Manual start always works.
	Enabled bool
	// After is how long the device may stay without connectivity before the
	// fallback AP comes up.
	After time.Duration
	// CheckEvery is the connectivity poll interval.
	CheckEvery time.Duration
	// AutoStop tears the AP down once real connectivity returns.
	AutoStop bool
	// Config carries operator overrides such as a fixed SSID or passphrase.
	Config domain.HotspotConfig
}

// HotspotStatus reports the fallback AP state.
func (a *Agent) HotspotStatus(ctx context.Context) domain.HotspotStatus {
	if a.hotspot == nil {
		return domain.HotspotStatus{Reason: domain.Msg("the fallback access point is not configured")}
	}
	return a.hotspot.Status(ctx)
}

// StartHotspot brings the fallback AP up on request.
func (a *Agent) StartHotspot(ctx context.Context, link string) error {
	if a.hotspot == nil {
		return domain.Unavailable("the fallback access point is unavailable")
	}
	cfg, err := a.hotspotConfig(ctx, link)
	if err != nil {
		return err
	}
	if err := a.hotspot.Start(ctx, cfg, domain.Msg("started on request")); err != nil {
		return err
	}
	a.setHotspotAuto(false)
	a.pub.Publish(domain.NewEvent(domain.EventHotspot, cfg.Link, domain.Msg("Fallback access point started"), a.hotspot.Status(ctx)))
	return nil
}

// StopHotspot tears the fallback AP down on request.
func (a *Agent) StopHotspot(ctx context.Context) error {
	if a.hotspot == nil {
		return domain.Unavailable("the fallback access point is unavailable")
	}
	if err := a.hotspot.Stop(ctx); err != nil {
		return err
	}
	a.setHotspotAuto(false)
	a.pub.Publish(domain.NewEvent(domain.EventHotspot, "", domain.Msg("Fallback access point stopped"), domain.HotspotStatus{}))
	return nil
}

func (a *Agent) setHotspotAuto(auto bool) {
	a.hotspotMu.Lock()
	a.hotspotAuto = auto
	a.hotspotMu.Unlock()
}

func (a *Agent) hotspotIsAuto() bool {
	a.hotspotMu.Lock()
	defer a.hotspotMu.Unlock()
	return a.hotspotAuto
}

func (a *Agent) hotspotConfig(ctx context.Context, link string) (domain.HotspotConfig, error) {
	cfg := a.hotspotPolicy.Config
	if link != "" {
		cfg.Link = link
	}

	links, err := a.links.Links(ctx)
	if err != nil {
		return cfg, err
	}
	if cfg.Link == "" {
		for _, l := range links {
			if l.Wireless {
				cfg.Link = l.Name
				break
			}
		}
	}
	if cfg.Link == "" {
		return cfg, domain.NotFound("no Wi-Fi interface available for the fallback access point")
	}

	target, err := domain.FindLink(links, cfg.Link)
	if err != nil {
		return cfg, err
	}
	if !target.Wireless {
		return cfg, domain.Invalid("%s is not a wireless interface", target.Name)
	}
	return cfg.WithDefaults(target.MAC)
}

// WatchConnectivity is the last-resort safety net: when the device has had no
// usable network for long enough it publishes its own access point so an
// operator can walk up and reconfigure it.
func (a *Agent) WatchConnectivity(ctx context.Context) {
	if a.hotspot == nil || !a.hotspotPolicy.Enabled {
		return
	}
	if err := a.hotspot.Available(); err != nil {
		a.log.Warn("automatic fallback access point disabled", "err", err)
		return
	}

	interval := a.hotspotPolicy.CheckEvery
	if interval <= 0 {
		interval = 20 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lastSeen := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		status := a.hotspot.Status(ctx)
		if a.hasConnectivity(ctx, status) {
			lastSeen = time.Now()
			if status.Active && a.hotspotPolicy.AutoStop && a.hotspotIsAuto() {
				a.log.Info("connectivity is back, stopping the fallback access point")
				if err := a.StopHotspot(ctx); err != nil {
					a.log.Warn("cannot stop the fallback access point", "err", err)
				}
			}
			continue
		}

		if status.Active || time.Since(lastSeen) < a.hotspotPolicy.After {
			continue
		}

		cfg, err := a.hotspotConfig(ctx, "")
		if err != nil {
			a.log.Warn("cannot start the fallback access point", "err", err)
			lastSeen = time.Now() // do not retry every tick
			continue
		}
		reason := domain.Msg("no connectivity for %s", a.hotspotPolicy.After)
		if err := a.hotspot.Start(ctx, cfg, reason); err != nil {
			a.log.Error("starting the fallback access point failed", "err", err)
			lastSeen = time.Now()
			continue
		}
		a.setHotspotAuto(true)
		a.pub.Publish(domain.NewEvent(domain.EventHotspot, cfg.Link,
			domain.Msg("Fallback access point started automatically after losing connectivity"), a.hotspot.Status(ctx)))
	}
}

// hasConnectivity ignores the hotspot's own interface, whose address would
// otherwise make an isolated device look healthy.
func (a *Agent) hasConnectivity(ctx context.Context, hotspot domain.HotspotStatus) bool {
	links, err := a.links.Links(ctx)
	if err != nil {
		return true // cannot tell; do not disrupt anything
	}

	for _, link := range links {
		if hotspot.Active && link.Name == hotspot.Link {
			continue
		}
		if link.Gateway != "" {
			return true
		}
		for _, raw := range link.Addresses {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				continue
			}
			addr := prefix.Addr()
			if addr.IsGlobalUnicast() && !addr.IsLinkLocalUnicast() && !addr.IsPrivate() {
				return true
			}
			// A private address from DHCP still means someone can reach us.
			if addr.IsPrivate() && link.OperUp {
				return true
			}
		}
	}
	return false
}
