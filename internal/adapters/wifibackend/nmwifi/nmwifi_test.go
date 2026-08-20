package nmwifi

import (
	"testing"

	"netcfg/internal/domain"
)

// TestSplitEscaped covers nmcli's terse format, where a colon inside a value is
// backslash escaped. Splitting naively on ":" corrupts every MAC address.
func TestSplitEscaped(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{
			name: "a BSSID keeps its escaped colons",
			line: `MyNet:AA\:BB\:CC\:DD\:EE\:FF:72:5180 MHz:WPA2`,
			want: []string{"MyNet", "AA:BB:CC:DD:EE:FF", "72", "5180 MHz", "WPA2"},
		},
		{
			name: "an SSID containing a colon survives",
			line: `Cafe\: Free:AA\:BB:60`,
			want: []string{"Cafe: Free", "AA:BB", "60"},
		},
		{
			name: "empty fields are preserved",
			line: `::x`,
			want: []string{"", "", "x"},
		},
		{
			name: "a plain line splits normally",
			line: `wlan0:wifi:connected`,
			want: []string{"wlan0", "wifi", "connected"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitEscaped(tc.line)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d fields %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("field %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSecurityFromNM(t *testing.T) {
	tests := map[string]domain.Security{
		"":            domain.SecOpen,
		"WPA2":        domain.SecPSK,
		"WPA1 WPA2":   domain.SecPSK,
		"WPA2 WPA3":   domain.SecSAE,
		"WPA3":        domain.SecSAE,
		"WPA2 SAE":    domain.SecPSKSAE,
		"WEP":         domain.SecWEP,
		"802.1X WPA2": domain.SecPSK,
	}
	for flags, want := range tests {
		if got := securityFromNM(flags); got != want {
			t.Errorf("securityFromNM(%q) = %q, want %q", flags, got, want)
		}
	}
}

// TestQualityMapsToDbm checks the conversion from nmcli's 0-100 bar into the
// dBm figure the rest of the interface renders.
func TestQualityMapsToDbm(t *testing.T) {
	tests := map[int]int{0: -100, 50: -75, 100: -50, -5: -100, 150: -50}
	for percent, want := range tests {
		if got := quality(percent); got != want {
			t.Errorf("quality(%d) = %d, want %d", percent, got, want)
		}
	}
}

func TestStateWordUnwrapsTheCode(t *testing.T) {
	tests := map[string]string{
		"100 (connected)":   "connected",
		"30 (disconnected)": "disconnected",
		"connected":         "connected",
		"":                  "",
	}
	for value, want := range tests {
		if got := stateWord(value); got != want {
			t.Errorf("stateWord(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestParseFreqDropsTheUnit(t *testing.T) {
	tests := map[string]int{"5180 MHz": 5180, "2412 MHz": 2412, "": 0, "bogus": 0}
	for value, want := range tests {
		if got := parseFreq(value); got != want {
			t.Errorf("parseFreq(%q) = %d, want %d", value, got, want)
		}
	}
}

// TestProfileIDIsStableAndNonNegative matters because the API speaks in the
// numeric ids wpa_supplicant uses, and the UI round-trips them.
func TestProfileIDIsStableAndNonNegative(t *testing.T) {
	const uuid = "8f2a1c44-1e0b-4a3f-9b7c-2b0d6a5e1f33"

	first := profileID(uuid)
	if first != profileID(uuid) {
		t.Fatal("the same UUID must always map to the same id")
	}
	if first < 0 {
		t.Fatalf("id = %d, must not be negative", first)
	}
	if profileID(uuid) == profileID("a different uuid") {
		t.Fatal("distinct UUIDs must not collide here")
	}
	if profileID("") != 0 {
		t.Fatal("an absent UUID must map to 0")
	}
}
