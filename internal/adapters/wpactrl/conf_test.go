package wpactrl

import (
	"bufio"
	"strings"
	"testing"
)

const sampleConf = `ctrl_interface=DIR=/run/wpa_supplicant GROUP=netdev
update_config=1
country=VN

# a comment
network={
        ssid="Home 2.4G"
        psk="hunter2hunter2"
        priority=5
}

network={
        ssid="Home 5G"
        psk=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
        key_mgmt=WPA-PSK
}

network={
        ssid="Mesh WPA3"
        key_mgmt=SAE
        sae_password="dragon dragon"
}

network={
        ssid="Cafe"
        key_mgmt=NONE
}
`

func TestSecretOf(t *testing.T) {
	blocks := parseConfBlocks(bufio.NewScanner(strings.NewReader(sampleConf)))
	if len(blocks) != 4 {
		t.Fatalf("parsed %d blocks, want 4", len(blocks))
	}

	cases := []struct {
		ssid   string
		value  string
		hashed bool
	}{
		{"Home 2.4G", "hunter2hunter2", false},
		{"Home 5G", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", true},
		{"Mesh WPA3", "dragon dragon", false},
		{"Cafe", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.ssid, func(t *testing.T) {
			got, ok := secretOf(blocks, tc.ssid)
			if !ok {
				t.Fatalf("secretOf(%q) not found", tc.ssid)
			}
			if got.Value.Reveal() != tc.value {
				t.Errorf("value = %q, want %q", got.Value.Reveal(), tc.value)
			}
			if got.Hashed != tc.hashed {
				t.Errorf("hashed = %v, want %v", got.Hashed, tc.hashed)
			}
		})
	}

	if _, ok := secretOf(blocks, "Absent"); ok {
		t.Error("secretOf found a network that is not in the file")
	}
}

func TestSecretIsNotLogged(t *testing.T) {
	blocks := parseConfBlocks(bufio.NewScanner(strings.NewReader(sampleConf)))
	got, _ := secretOf(blocks, "Home 2.4G")
	if s := got.Value.String(); strings.Contains(s, "hunter2") {
		t.Errorf("Secret.String() leaked the passphrase: %q", s)
	}
}
