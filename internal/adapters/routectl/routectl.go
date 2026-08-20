// Package routectl moves a link up or down the kernel routing table. It never
// writes a configuration file: a demotion is meant to last exactly as long as
// the fault, and to vanish on reboot.
package routectl

import (
	"context"
	"strconv"
	"strings"

	"netcfg/internal/adapters/sysexec"
	"netcfg/internal/domain"
)

// Control implements ports.RouteControl through iproute2.
type Control struct{}

func New() *Control { return &Control{} }

func (c *Control) Available() error {
	if !sysexec.Available("ip") {
		return domain.Unavailable("iproute2 is not installed")
	}
	return nil
}

// DefaultRoute reads the default route the kernel currently holds for a link.
func (c *Control) DefaultRoute(ctx context.Context, link string) (string, uint32, bool, error) {
	if err := domain.ValidateLinkName(link); err != nil {
		return "", 0, false, err
	}
	out, err := sysexec.Run(ctx, "ip", "-4", "route", "show", "default", "dev", link)
	if err != nil {
		return "", 0, false, err
	}
	gateway, metric, ok := parseDefault(out)
	return gateway, metric, ok, nil
}

// parseDefault reads the first default route out of "ip route show" output.
func parseDefault(out string) (gateway string, metric uint32, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "default" {
			continue
		}
		for i := 1; i+1 < len(fields); i++ {
			switch fields[i] {
			case "via":
				gateway = fields[i+1]
			case "metric":
				if value, err := strconv.ParseUint(fields[i+1], 10, 32); err == nil {
					metric = uint32(value)
				}
			}
		}
		return gateway, metric, true
	}
	return "", 0, false
}

// MoveDefault re-installs the link's default route at another metric. The
// kernel keys routes by metric, so the new one is added first and the old entry
// removed after: the link is never left without a default route.
func (c *Control) MoveDefault(ctx context.Context, link, gateway string, from, to uint32) error {
	if err := domain.ValidateLinkName(link); err != nil {
		return err
	}
	if gateway == "" {
		return domain.Invalid("%s has no gateway to route through", link)
	}
	if from == to {
		return nil
	}

	if _, err := sysexec.Run(ctx, "ip", "-4", "route", "replace", "default",
		"via", gateway, "dev", link, "metric", metricArg(to)); err != nil {
		return err
	}
	// The old entry may already be gone, for instance because DHCP renewed the
	// lease in between. The move itself succeeded, so that is not an error.
	_, _ = sysexec.Run(ctx, "ip", "-4", "route", "del", "default",
		"via", gateway, "dev", link, "metric", metricArg(from))
	return nil
}

func metricArg(metric uint32) string { return strconv.FormatUint(uint64(metric), 10) }
