package ifupdown

import (
	"strings"
	"testing"

	"netcfg/internal/domain"
)

func mustNormalize(t *testing.T, plan domain.IPPlan) domain.IPPlan {
	t.Helper()
	out, err := plan.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRenderStaticDualStack(t *testing.T) {
	plan := mustNormalize(t, domain.IPPlan{
		Link:     "eth0",
		Mode:     domain.ModeStatic,
		Address:  "192.168.1.50/24",
		Gateway:  "192.168.1.1",
		Mode6:    domain.ModeStatic,
		Address6: "2001:db8::10/64",
		Gateway6: "fe80::1",
		DNS:      []string{"1.1.1.1"},
	})

	got := string(render(plan))
	for _, want := range []string{
		"iface eth0 inet static",
		"address 192.168.1.50",
		"netmask 255.255.255.0",
		"gateway 192.168.1.1",
		"dns-nameservers 1.1.1.1",
		"iface eth0 inet6 static",
		"address 2001:db8::10/64",
		"gateway fe80::1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

// TestRenderIPv6UsesPrefixNotNetmask guards the bug where the IPv4 netmask
// helper was applied to an IPv6 address and silently produced 255.255.255.255.
func TestRenderIPv6UsesPrefixNotNetmask(t *testing.T) {
	plan := mustNormalize(t, domain.IPPlan{
		Link: "eth0", Mode: domain.ModeDisabled,
		Mode6: domain.ModeStatic, Address6: "2001:db8::10/64",
	})

	got := string(render(plan))
	if strings.Contains(got, "255.255.255.255") {
		t.Fatalf("IPv6 must not be written with an IPv4 netmask:\n%s", got)
	}
	if !strings.Contains(got, "iface eth0 inet manual") {
		t.Fatal("disabling IPv4 must emit an inet manual stanza")
	}
}

func TestRenderAutoIPv6(t *testing.T) {
	plan := mustNormalize(t, domain.IPPlan{Link: "wlan0", Mode: domain.ModeDHCP})

	got := string(render(plan))
	if !strings.Contains(got, "iface wlan0 inet dhcp") {
		t.Fatal("missing the IPv4 DHCP stanza")
	}
	if !strings.Contains(got, "iface wlan0 inet6 auto") {
		t.Fatal("IPv6 must default to auto")
	}
}

func TestMaskRoundTrip(t *testing.T) {
	for _, bits := range []int{8, 16, 22, 24, 30} {
		mask := bitsToMask(bits)
		got, err := maskToBits(mask)
		if err != nil {
			t.Fatal(err)
		}
		if got != bits {
			t.Fatalf("/%d -> %s -> /%d", bits, mask, got)
		}
	}
}
