package domain

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Mode selects how a link obtains an address. IPv4 accepts dhcp, static and
// disabled; IPv6 accepts auto (RA/SLAAC/DHCPv6), static and disabled.
type Mode string

const (
	ModeDHCP     Mode = "dhcp"
	ModeStatic   Mode = "static"
	ModeAuto     Mode = "auto"
	ModeDisabled Mode = "disabled"
)

// BackendKind identifies which subsystem owns IP configuration on this host.
type BackendKind string

const (
	BackendNetworkd BackendKind = "systemd-networkd"
	BackendNM       BackendKind = "network-manager"
	BackendIfupdown BackendKind = "ifupdown"
	BackendNone     BackendKind = "none"
)

// IPPlan is the desired layer-3 configuration of one link, both families.
type IPPlan struct {
	Link string `json:"link"`

	Mode    Mode   `json:"mode"`
	Address string `json:"address"`
	Gateway string `json:"gateway"`

	Mode6    Mode   `json:"mode6"`
	Address6 string `json:"address6"`
	Gateway6 string `json:"gateway6"`

	// Metric is the route metric of this link's default route, for both
	// families. The kernel prefers the lowest metric, so this is what decides
	// failover between a wired and a wireless link. Zero leaves the backend
	// default in place.
	Metric uint32 `json:"metric"`

	// NoDefaultRoute keeps the link out of the failover order entirely: it gets
	// an address but installs no default route. The zero value means the link
	// does carry one, so plans written before this field existed keep working.
	NoDefaultRoute bool `json:"noDefaultRoute"`

	// DNS may mix both families; backends split it where the syntax requires.
	DNS []string `json:"dns"`
}

// Normalize validates every field and re-serialises it from its parsed form, so
// nothing user supplied can be injected into a generated config file.
func (p IPPlan) Normalize() (IPPlan, error) {
	out := IPPlan{Link: p.Link, Mode: p.Mode, Mode6: p.Mode6, Metric: p.Metric, NoDefaultRoute: p.NoDefaultRoute}
	if err := ValidateLinkName(p.Link); err != nil {
		return out, err
	}

	// Empty modes come from state written before dual-stack support existed.
	if out.Mode == "" {
		out.Mode = ModeDHCP
	}
	if out.Mode6 == "" {
		out.Mode6 = ModeAuto
	}

	switch out.Mode {
	case ModeDHCP, ModeDisabled:
	case ModeStatic:
		prefix, err := parseFamilyPrefix(p.Address, true)
		if err != nil {
			return out, err
		}
		out.Address = prefix.String()

		if gw := strings.TrimSpace(p.Gateway); gw != "" {
			addr, err := netip.ParseAddr(gw)
			if err != nil || !addr.Is4() {
				return out, Invalid("invalid IPv4 gateway: %q", p.Gateway)
			}
			if !prefix.Contains(addr) {
				return out, Invalid("gateway %s is outside subnet %s", addr, prefix)
			}
			out.Gateway = addr.String()
		}
	default:
		return out, Invalid("invalid IPv4 mode: %q", string(out.Mode))
	}

	switch out.Mode6 {
	case ModeAuto, ModeDisabled:
	case ModeStatic:
		prefix, err := parseFamilyPrefix(p.Address6, false)
		if err != nil {
			return out, err
		}
		out.Address6 = prefix.String()

		if gw := strings.TrimSpace(p.Gateway6); gw != "" {
			addr, err := netip.ParseAddr(gw)
			if err != nil || addr.Is4() || addr.Is4In6() {
				return out, Invalid("invalid IPv6 gateway: %q", p.Gateway6)
			}
			// A link-local next hop such as fe80::1 is normal and legitimately
			// falls outside the configured prefix, so containment is not checked.
			out.Gateway6 = addr.String()
		}
	default:
		return out, Invalid("invalid IPv6 mode: %q", string(out.Mode6))
	}

	if out.Mode == ModeDisabled && out.Mode6 == ModeDisabled {
		return out, Invalid("cannot disable both IPv4 and IPv6 on the same interface")
	}

	for _, raw := range p.DNS {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil {
			return out, Invalid("invalid DNS server: %q", raw)
		}
		if value := addr.String(); !slices.Contains(out.DNS, value) {
			out.DNS = append(out.DNS, value)
		}
	}
	if len(out.DNS) > 6 {
		return out, Invalid("at most 6 DNS servers are allowed")
	}
	// A static gateway is the default route, so the two settings cannot both hold.
	if out.NoDefaultRoute {
		out.Gateway, out.Gateway6 = "", ""
	}
	return out, nil
}

func parseFamilyPrefix(raw string, wantV4 bool) (netip.Prefix, error) {
	family := "IPv6"
	if wantV4 {
		family = "IPv4"
	}

	prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
	if err != nil || !prefix.Addr().IsValid() || prefix.Addr().IsUnspecified() {
		return netip.Prefix{}, Invalid("invalid %s address/CIDR: %q", family, raw)
	}
	if prefix.Addr().Is4() != wantV4 || prefix.Addr().Is4In6() {
		return netip.Prefix{}, Invalid("address %q is not %s", raw, family)
	}
	return prefix, nil
}

// DNSByFamily splits the resolver list for backends with per-family keys.
func (p IPPlan) DNSByFamily() (v4, v6 []string) {
	for _, raw := range p.DNS {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			continue
		}
		if addr.Is4() {
			v4 = append(v4, raw)
		} else {
			v6 = append(v6, raw)
		}
	}
	return v4, v6
}

// Equal compares two normalized plans.
func (p IPPlan) Equal(other IPPlan) bool {
	return p.Link == other.Link &&
		p.Mode == other.Mode && p.Address == other.Address && p.Gateway == other.Gateway &&
		p.Mode6 == other.Mode6 && p.Address6 == other.Address6 && p.Gateway6 == other.Gateway6 &&
		p.Metric == other.Metric && p.NoDefaultRoute == other.NoDefaultRoute &&
		slices.Equal(p.DNS, other.DNS)
}

// MetricText renders a metric for the operator; zero means the backend decides.
func MetricText(metric uint32) string {
	if metric == 0 {
		return ""
	}
	return strconv.FormatUint(uint64(metric), 10)
}

func defaultRouteText(none bool) string {
	if none {
		return "off"
	}
	return "on"
}

// Change is one field level difference shown to the operator before applying.
type Change struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// Diff summarises what an apply would do and whether it can cut the session.
type Diff struct {
	Link       string      `json:"link"`
	Backend    BackendKind `json:"backend"`
	Changes    []Change    `json:"changes"`
	Disruptive bool        `json:"disruptive"`
	Warning    Message     `json:"warning"`
}

// disruptiveFields are the ones that can drop the operator's own connection.
// The metric belongs here: lowering it hands the default route to another link,
// which breaks the return path whenever the two links are not on one subnet.
var disruptiveFields = []string{"mode", "address", "gateway", "mode6", "address6", "gateway6", "metric", "defaultRoute"}

// DiffIP builds the change list between the running and the desired plan.
func DiffIP(current, desired IPPlan, backend BackendKind) Diff {
	d := Diff{Link: desired.Link, Backend: backend}
	add := func(field, from, to string) {
		if from != to {
			d.Changes = append(d.Changes, Change{Field: field, From: from, To: to})
		}
	}
	add("mode", string(current.Mode), string(desired.Mode))
	add("address", current.Address, desired.Address)
	add("gateway", current.Gateway, desired.Gateway)
	add("mode6", string(current.Mode6), string(desired.Mode6))
	add("address6", current.Address6, desired.Address6)
	add("gateway6", current.Gateway6, desired.Gateway6)
	add("metric", MetricText(current.Metric), MetricText(desired.Metric))
	add("defaultRoute", defaultRouteText(current.NoDefaultRoute), defaultRouteText(desired.NoDefaultRoute))
	add("dns", strings.Join(current.DNS, ", "), strings.Join(desired.DNS, ", "))

	for _, c := range d.Changes {
		if slices.Contains(disruptiveFields, c.Field) {
			d.Disruptive = true
		}
	}
	if d.Disruptive {
		d.Warning = Msg("This change may cut your connection to %s.", desired.Link)
	}
	return d
}
