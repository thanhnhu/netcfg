// Package nm configures IP addressing through NetworkManager via nmcli.
package nm

import (
	"context"
	"strconv"
	"strings"

	"netcfg/internal/adapters/sysexec"
	"netcfg/internal/domain"
)

// Backend implements ports.IPBackend.
type Backend struct{}

func New() *Backend { return &Backend{} }

func (b *Backend) Kind() domain.BackendKind { return domain.BackendNM }

// Detect lists the devices NetworkManager reports as managed.
func (b *Backend) Detect(ctx context.Context) ([]string, error) {
	if !sysexec.Available("nmcli") {
		return nil, domain.Unavailable("nmcli is not installed")
	}
	out, err := sysexec.Run(ctx, "nmcli", "-t", "-f", "DEVICE,STATE", "device", "status")
	if err != nil {
		return nil, err
	}

	var links []string
	for _, line := range strings.Split(out, "\n") {
		device, state, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || device == "lo" || state == "unmanaged" {
			continue
		}
		if domain.ValidateLinkName(device) == nil {
			links = append(links, device)
		}
	}
	return links, nil
}

// connectionFor resolves the active connection profile bound to a device.
func (b *Backend) connectionFor(ctx context.Context, link string) (string, error) {
	if err := domain.ValidateLinkName(link); err != nil {
		return "", err
	}
	out, err := sysexec.Run(ctx, "nmcli", "-t", "-f", "NAME,DEVICE", "connection", "show", "--active")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		name, device, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && device == link {
			return name, nil
		}
	}
	return "", domain.NotFound("no active NetworkManager connection found for %s", link)
}

// Current reads the addressing of the active profile for both families.
func (b *Backend) Current(ctx context.Context, link string) (domain.IPPlan, error) {
	plan := domain.IPPlan{Link: link, Mode: domain.ModeDHCP, Mode6: domain.ModeAuto}
	conn, err := b.connectionFor(ctx, link)
	if err != nil {
		return plan, err
	}

	fields := "ipv4.method,ipv4.addresses,ipv4.gateway,ipv4.dns,ipv4.route-metric,ipv4.never-default," +
		"ipv6.method,ipv6.addresses,ipv6.gateway,ipv6.dns"
	out, err := sysexec.Run(ctx, "nmcli", "-t", "-f", fields, "connection", "show", conn)
	if err != nil {
		return plan, err
	}

	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || value == "" || value == "--" {
			continue
		}
		switch key {
		case "ipv4.method":
			plan.Mode = fromNMMethod4(value)
		case "ipv4.addresses":
			plan.Address = strings.TrimSpace(strings.Split(value, ",")[0])
		case "ipv4.gateway":
			plan.Gateway = value
		case "ipv6.method":
			plan.Mode6 = fromNMMethod6(value)
		case "ipv6.addresses":
			plan.Address6 = strings.TrimSpace(strings.Split(value, ",")[0])
		case "ipv6.gateway":
			plan.Gateway6 = value
		case "ipv4.route-metric":
			// nmcli reports -1 when the metric is left to NetworkManager.
			if parsed, err := strconv.ParseUint(value, 10, 32); err == nil {
				plan.Metric = uint32(parsed)
			}
		case "ipv4.never-default":
			plan.NoDefaultRoute = value == "yes"
		case "ipv4.dns", "ipv6.dns":
			for _, dns := range strings.Split(value, ",") {
				if dns = strings.TrimSpace(dns); dns != "" {
					plan.DNS = append(plan.DNS, dns)
				}
			}
		}
	}
	return plan, nil
}

// Apply rewrites the profile and re-activates it.
func (b *Backend) Apply(ctx context.Context, plan domain.IPPlan) error {
	normalized, err := plan.Normalize()
	if err != nil {
		return err
	}
	conn, err := b.connectionFor(ctx, normalized.Link)
	if err != nil {
		return err
	}
	dns4, dns6 := normalized.DNSByFamily()

	args := []string{"connection", "modify", conn}
	switch normalized.Mode {
	case domain.ModeDHCP:
		args = append(args, "ipv4.method", "auto", "ipv4.addresses", "", "ipv4.gateway", "")
	case domain.ModeStatic:
		args = append(args, "ipv4.method", "manual", "ipv4.addresses", normalized.Address,
			"ipv4.gateway", normalized.Gateway)
	case domain.ModeDisabled:
		args = append(args, "ipv4.method", "disabled", "ipv4.addresses", "", "ipv4.gateway", "")
	}

	switch normalized.Mode6 {
	case domain.ModeAuto:
		args = append(args, "ipv6.method", "auto", "ipv6.addresses", "", "ipv6.gateway", "")
	case domain.ModeStatic:
		args = append(args, "ipv6.method", "manual", "ipv6.addresses", normalized.Address6,
			"ipv6.gateway", normalized.Gateway6)
	case domain.ModeDisabled:
		args = append(args, "ipv6.method", "disabled", "ipv6.addresses", "", "ipv6.gateway", "")
	}

	args = append(args,
		"ipv4.dns", strings.Join(dns4, ","), "ipv4.ignore-auto-dns", yesNo(len(dns4) > 0),
		"ipv6.dns", strings.Join(dns6, ","), "ipv6.ignore-auto-dns", yesNo(len(dns6) > 0),
		"ipv4.route-metric", metricArg(normalized.Metric),
		"ipv6.route-metric", metricArg(normalized.Metric),
		"ipv4.never-default", yesNo(normalized.NoDefaultRoute),
		"ipv6.never-default", yesNo(normalized.NoDefaultRoute))

	if _, err := sysexec.Run(ctx, "nmcli", args...); err != nil {
		return err
	}
	_, err = sysexec.Run(ctx, "nmcli", "connection", "up", conn)
	return err
}

func fromNMMethod4(method string) domain.Mode {
	switch method {
	case "manual":
		return domain.ModeStatic
	case "disabled":
		return domain.ModeDisabled
	default:
		return domain.ModeDHCP
	}
}

func fromNMMethod6(method string) domain.Mode {
	switch method {
	case "manual":
		return domain.ModeStatic
	case "disabled", "ignore":
		return domain.ModeDisabled
	default:
		return domain.ModeAuto
	}
}

// metricArg maps zero back onto NetworkManager's "decide for me" value.
func metricArg(metric uint32) string {
	if metric == 0 {
		return "-1"
	}
	return strconv.FormatUint(uint64(metric), 10)
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
