// Package prober answers whether a link still has usable connectivity after a
// configuration change. A refused TCP connection still proves reachability, so
// it is treated as success.
package prober

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"time"

	"netcfg/internal/domain"
)

const dialTimeout = 1500 * time.Millisecond

// Prober implements ports.Prober.
type Prober struct {
	// Targets are extra host:port probes, checked after the gateway.
	Targets []string
}

func New(targets []string) *Prober { return &Prober{Targets: targets} }

// Probe checks that the link has an address and that its gateway answers.
func (p *Prober) Probe(ctx context.Context, link domain.Link) domain.ProbeResult {
	result := domain.ProbeResult{At: time.Now()}

	if len(link.Addresses) == 0 {
		result.Detail = domain.Msg("%s has no IP address", link.Name)
		return result
	}
	if link.Gateway == "" {
		result.OK = true
		result.Detail = domain.Msg("%s has address %s, no gateway to test", link.Name, link.Addresses[0])
		return result
	}

	for _, port := range []string{"53", "80", "443"} {
		if reachable(ctx, net.JoinHostPort(link.Gateway, port)) {
			result.OK = true
			result.Detail = domain.Msg("gateway %s responded", link.Gateway)
			return result
		}
	}
	for _, target := range p.Targets {
		if reachable(ctx, target) {
			result.OK = true
			result.Detail = domain.Msg("%s responded", target)
			return result
		}
	}

	result.Detail = domain.Msg("gateway %s did not respond", link.Gateway)
	return result
}

// ProbeVia answers the question the failover monitor actually asks: does this
// link forward, whatever the routing table prefers. Configured targets are
// reached through this interface, which is what separates "the gateway is up"
// from "the gateway still carries traffic".
func (p *Prober) ProbeVia(ctx context.Context, link domain.Link) domain.ProbeResult {
	result := domain.ProbeResult{At: time.Now()}

	if !link.OperUp {
		result.Detail = domain.Msg("%s is down", link.Name)
		return result
	}
	if len(link.Addresses) == 0 {
		result.Detail = domain.Msg("%s has no IP address", link.Name)
		return result
	}
	if link.Gateway == "" {
		result.Detail = domain.Msg("%s has no gateway", link.Name)
		return result
	}

	// A target beyond the gateway is the only thing that catches a next hop
	// that answers but no longer forwards, so it decides on its own when set.
	if len(p.Targets) > 0 {
		for _, target := range p.Targets {
			if reachableVia(ctx, link.Name, target) {
				result.OK = true
				result.Detail = domain.Msg("%s responded through %s", target, link.Name)
				return result
			}
		}
		result.Detail = domain.Msg("no target responded through %s", link.Name)
		return result
	}

	for _, port := range []string{"53", "80", "443"} {
		if reachableVia(ctx, link.Name, net.JoinHostPort(link.Gateway, port)) {
			result.OK = true
			result.Detail = domain.Msg("gateway %s responded through %s", link.Gateway, link.Name)
			return result
		}
	}

	result.Detail = domain.Msg("gateway %s did not respond through %s", link.Gateway, link.Name)
	return result
}

// reachable dials target; ECONNREFUSED means the host is alive, which is
// exactly what we want to know.
func reachable(ctx context.Context, target string) bool {
	return dial(ctx, net.Dialer{}, target)
}

// reachableVia dials out of one interface, whatever the routing table would
// have chosen.
func reachableVia(ctx context.Context, link, target string) bool {
	return dial(ctx, net.Dialer{Control: bindToDevice(link)}, target)
}

func dial(ctx context.Context, dialer net.Dialer, target string) bool {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err == nil {
		_ = conn.Close()
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return strings.Contains(err.Error(), "refused")
}
