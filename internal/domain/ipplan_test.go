package domain

import "testing"

func TestIPPlanNormalize(t *testing.T) {
	tests := []struct {
		name    string
		plan    IPPlan
		wantErr bool
		want    IPPlan
	}{
		{
			name: "dhcp discards leftover address fields",
			plan: IPPlan{Link: "wlan0", Mode: ModeDHCP, Address: "junk", Gateway: "junk"},
			want: IPPlan{Link: "wlan0", Mode: ModeDHCP, Mode6: ModeAuto},
		},
		{
			name: "valid static IPv4",
			plan: IPPlan{Link: "eth0", Mode: ModeStatic, Address: "192.168.1.50/24", Gateway: "192.168.1.1", DNS: []string{" 1.1.1.1 ", ""}},
			want: IPPlan{Link: "eth0", Mode: ModeStatic, Address: "192.168.1.50/24", Gateway: "192.168.1.1", Mode6: ModeAuto, DNS: []string{"1.1.1.1"}},
		},
		{name: "missing prefix length", plan: IPPlan{Link: "eth0", Mode: ModeStatic, Address: "192.168.1.50"}, wantErr: true},
		{name: "gateway outside subnet", plan: IPPlan{Link: "eth0", Mode: ModeStatic, Address: "192.168.1.50/24", Gateway: "10.0.0.1"}, wantErr: true},
		{name: "gateway from the wrong family", plan: IPPlan{Link: "eth0", Mode: ModeStatic, Address: "192.168.1.50/24", Gateway: "fe80::1"}, wantErr: true},
		{name: "IPv6 in the IPv4 field", plan: IPPlan{Link: "eth0", Mode: ModeStatic, Address: "2001:db8::1/64"}, wantErr: true},
		{name: "IPv4 in the IPv6 field", plan: IPPlan{Link: "eth0", Mode6: ModeStatic, Address6: "192.168.1.5/24"}, wantErr: true},
		{name: "both families disabled", plan: IPPlan{Link: "eth0", Mode: ModeDisabled, Mode6: ModeDisabled}, wantErr: true},
		{name: "invalid DNS server", plan: IPPlan{Link: "eth0", Mode: ModeDHCP, DNS: []string{"not-an-ip"}}, wantErr: true},
		{name: "path traversal in link name", plan: IPPlan{Link: "../../etc/passwd", Mode: ModeDHCP}, wantErr: true},
		{name: "unknown mode", plan: IPPlan{Link: "eth0", Mode: "manual"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.plan.Normalize()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDiffIPMarksDisruptiveChanges(t *testing.T) {
	current := IPPlan{Link: "eth0", Mode: ModeDHCP, Mode6: ModeAuto}
	desired := IPPlan{Link: "eth0", Mode: ModeStatic, Address: "192.168.1.50/24", Mode6: ModeAuto}

	diff := DiffIP(current, desired, BackendNetworkd)
	if !diff.Disruptive {
		t.Fatal("switching from DHCP to static must count as connection breaking")
	}
	if diff.Warning.Empty() {
		t.Fatal("a disruptive change must carry a warning")
	}

	dnsOnly := DiffIP(
		IPPlan{Link: "eth0", Mode: ModeDHCP, Mode6: ModeAuto},
		IPPlan{Link: "eth0", Mode: ModeDHCP, Mode6: ModeAuto, DNS: []string{"1.1.1.1"}},
		BackendNetworkd,
	)
	if dnsOnly.Disruptive {
		t.Fatal("changing DNS alone cannot break the session")
	}
	if len(dnsOnly.Changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(dnsOnly.Changes))
	}

	v6Only := DiffIP(
		IPPlan{Link: "eth0", Mode: ModeDHCP, Mode6: ModeAuto},
		IPPlan{Link: "eth0", Mode: ModeDHCP, Mode6: ModeStatic, Address6: "2001:db8::10/64"},
		BackendNetworkd,
	)
	if !v6Only.Disruptive {
		t.Fatal("an IPv6 change can break the session too")
	}
}

func TestIPv6StaticNormalize(t *testing.T) {
	got, err := IPPlan{
		Link:     "eth0",
		Mode:     ModeDisabled,
		Mode6:    ModeStatic,
		Address6: "2001:db8::10/64",
		// A link-local next hop is normal and lies outside the prefix.
		Gateway6: "fe80::1",
		DNS:      []string{"2606:4700:4700::1111", "1.1.1.1"},
	}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if got.Address6 != "2001:db8::10/64" || got.Gateway6 != "fe80::1" {
		t.Fatalf("IPv6 normalization is wrong: %+v", got)
	}

	v4, v6 := got.DNSByFamily()
	if len(v4) != 1 || len(v6) != 1 {
		t.Fatalf("DNS split by family is wrong: v4=%v v6=%v", v4, v6)
	}
}

func TestSecretNeverLeaks(t *testing.T) {
	secret := NewSecret("top-secret")

	if got := secret.String(); got != "[REDACTED]" {
		t.Fatalf("String() leaked the secret: %q", got)
	}
	data, err := secret.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"[REDACTED]"` {
		t.Fatalf("JSON leaked the secret: %s", data)
	}
	if secret.Reveal() != "top-secret" {
		t.Fatal("Reveal must return the original value")
	}
}

func TestWPAPSKMatchesKnownVector(t *testing.T) {
	// IEEE 802.11i Annex H.4 test vector: SSID "IEEE", passphrase "password".
	got := WPAPSK("IEEE", NewSecret("password"))
	want := "f42c6fc52df0ebef9ebb4b90b38a5f902e83fe1b135a70e23aed762e9710a12e"
	if got != want {
		t.Fatalf("PSK = %s, want %s", got, want)
	}
}

func TestValidateSSID(t *testing.T) {
	if err := ValidateSSID(""); err == nil {
		t.Fatal("an empty SSID must be rejected")
	}
	if err := ValidateSSID(string(make([]byte, 33))); err == nil {
		t.Fatal("an SSID longer than 32 bytes must be rejected")
	}
	if err := ValidateSSID("home\nnetwork"); err == nil {
		t.Fatal("an SSID containing a newline must be rejected")
	}
	if err := ValidateSSID("My Home 5G"); err != nil {
		t.Fatalf("a valid SSID was rejected: %v", err)
	}
}
