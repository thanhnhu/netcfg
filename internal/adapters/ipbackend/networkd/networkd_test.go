package networkd

import (
	"context"
	"os"
	"path/filepath"
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

func TestRenderStaticIPv4(t *testing.T) {
	plan := mustNormalize(t, domain.IPPlan{
		Link:    "wlan0",
		Mode:    domain.ModeStatic,
		Address: "192.168.1.50/24",
		Gateway: "192.168.1.1",
		DNS:     []string{"1.1.1.1", "8.8.8.8"},
	})

	want := header + `[Match]
Name=wlan0

[Network]
DHCP=no
IPv6AcceptRA=yes
Address=192.168.1.50/24
Gateway=192.168.1.1
DNS=1.1.1.1
DNS=8.8.8.8
`
	if got := string(render(plan)); got != want {
		t.Fatalf("unit file differs:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderDualStack(t *testing.T) {
	plan := mustNormalize(t, domain.IPPlan{
		Link:     "eth0",
		Mode:     domain.ModeStatic,
		Address:  "192.168.1.50/24",
		Gateway:  "192.168.1.1",
		Mode6:    domain.ModeStatic,
		Address6: "2001:db8::10/64",
		Gateway6: "fe80::1",
	})

	got := string(render(plan))
	for _, want := range []string{
		"Address=192.168.1.50/24",
		"Address=2001:db8::10/64",
		"Gateway=192.168.1.1",
		"Gateway=fe80::1",
		"IPv6AcceptRA=no",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderDisabledIPv6(t *testing.T) {
	plan := mustNormalize(t, domain.IPPlan{Link: "eth0", Mode: domain.ModeDHCP, Mode6: domain.ModeDisabled})

	got := string(render(plan))
	if !strings.Contains(got, "LinkLocalAddressing=ipv4") {
		t.Fatal("disabling IPv6 must also drop the link-local address")
	}
	if !strings.Contains(got, "IPv6AcceptRA=no") {
		t.Fatal("disabling IPv6 must also disable router advertisements")
	}
}

func TestRenderDHCPWithOverriddenDNS(t *testing.T) {
	plan := mustNormalize(t, domain.IPPlan{Link: "eth0", Mode: domain.ModeDHCP, DNS: []string{"9.9.9.9"}})

	got := string(render(plan))
	if !strings.Contains(got, "DHCP=ipv4") {
		t.Fatal("missing DHCP=ipv4")
	}
	if !strings.Contains(got, "UseDNS=no") {
		t.Fatal("manual DNS servers must override the ones offered by DHCP")
	}
}

func TestRenderMetricOnDHCP(t *testing.T) {
	plan := mustNormalize(t, domain.IPPlan{Link: "wlan0", Mode: domain.ModeDHCP, Mode6: domain.ModeAuto, Metric: 600})

	got := string(render(plan))
	for _, want := range []string{"[DHCPv4]\nRouteMetric=600", "[IPv6AcceptRA]\nRouteMetric=600"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

// A metered static default route cannot use [Network] Gateway=, which carries
// no metric; it has to become a [Route] section instead.
func TestRenderMetricMovesStaticGatewayIntoRoute(t *testing.T) {
	plan := mustNormalize(t, domain.IPPlan{
		Link: "eth0", Mode: domain.ModeStatic,
		Address: "192.168.1.50/24", Gateway: "192.168.1.1", Metric: 100,
	})

	got := string(render(plan))
	if strings.Contains(got, "\nGateway=192.168.1.1\nDNS") || strings.Contains(got, "[Network]\nDHCP=no\nIPv6AcceptRA=yes\nAddress=192.168.1.50/24\nGateway=") {
		t.Fatalf("gateway must not stay in [Network]:\n%s", got)
	}
	if !strings.Contains(got, "[Route]\nGateway=192.168.1.1\nMetric=100") {
		t.Fatalf("missing metered route:\n%s", got)
	}
}

func TestCurrentReadsMetric(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "10-eth0.network", "[Match]\nName=eth*\n\n[Network]\nDHCP=yes\n\n[DHCPv4]\nRouteMetric=100\n")

	plan, err := New(dir).Current(context.Background(), "eth0")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Metric != 100 {
		t.Fatalf("metric = %d, want 100", plan.Metric)
	}
}

// A link may already be governed by a file that does not follow the
// 10-<link>.network convention. Writing a new file would shadow it and quietly
// drop whatever it configured, so the existing one must be reused.
func TestPathReusesTheFileThatAlreadyMatches(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "20-wlan0.network", "[Match]\nName=wlan0\n\n[Network]\nDHCP=yes\n")
	write(t, dir, "10-eth0.network", "[Match]\nName=eth*\n\n[Network]\nDHCP=yes\n")

	b := New(dir)
	for link, want := range map[string]string{
		"wlan0": "20-wlan0.network",
		"eth0":  "10-eth0.network",
		"eth1":  "10-eth0.network", // the eth* glob covers it too
		"usb0":  "10-usb0.network", // nothing matches, so a new file is created
	} {
		got, err := b.path(link)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(got) != want {
			t.Errorf("path(%q) = %q, want %q", link, filepath.Base(got), want)
		}
	}
}

func TestPathIgnoresNegatedMatch(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "05-not-eth0.network", "[Match]\nName=!eth0 eth*\n\n[Network]\nDHCP=yes\n")

	got, err := New(dir).path("eth0")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "10-eth0.network" {
		t.Fatalf("path = %q, want a new 10-eth0.network", filepath.Base(got))
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRenderCannotBeInjected proves that hostile input is rejected during
// normalization and therefore never reaches the generated unit file.
func TestRenderCannotBeInjected(t *testing.T) {
	hostile := []domain.IPPlan{
		{Link: "eth0", Mode: domain.ModeStatic, Address: "192.168.1.1/24\nExecStart=/bin/sh"},
		{Link: "eth0", Mode: domain.ModeStatic, Address: "192.168.1.1/24", Gateway: "192.168.1.2\n[Service]"},
		{Link: "eth0", Mode6: domain.ModeStatic, Address6: "2001:db8::1/64\nDNS=evil"},
		{Link: "eth0", Mode: domain.ModeDHCP, DNS: []string{"1.1.1.1\nDNS=evil"}},
		{Link: "eth0\nName=*", Mode: domain.ModeDHCP},
	}
	for _, plan := range hostile {
		if _, err := plan.Normalize(); err == nil {
			t.Fatalf("hostile input was accepted: %+v", plan)
		}
	}
}
