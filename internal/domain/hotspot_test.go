package domain

import (
	"testing"
)

func TestHotspotConfigDefaults(t *testing.T) {
	cfg, err := HotspotConfig{}.WithDefaults("dc:a6:32:11:22:33")
	if err != nil {
		t.Fatal(err)
	}

	// Fixed, not derived from the MAC: an operator standing next to a device
	// they cannot reach must recognise the network without being told.
	if cfg.SSID != DefaultHotspotSSID {
		t.Fatalf("wrong default SSID: %s", cfg.SSID)
	}
	if cfg.Passphrase.Reveal() != DefaultHotspotPassphrase {
		t.Fatalf("wrong default passphrase: %s", cfg.Passphrase.Reveal())
	}
	if cfg.Channel != 6 || cfg.Address != DefaultHotspotAddress {
		t.Fatalf("wrong defaults: %+v", cfg)
	}
	// The default is the weakest WPA2 accepts, so it still has to be one it
	// accepts: hostapd refuses to start on anything shorter than eight.
	if err := ValidatePassphrase(cfg.Passphrase); err != nil {
		t.Fatalf("default passphrase is invalid: %v", err)
	}
}

func TestHotspotConfigKeepsOverrides(t *testing.T) {
	cfg, err := HotspotConfig{
		SSID:       "device-setup",
		Passphrase: NewSecret("fixed-passphrase"),
		Channel:    11,
	}.WithDefaults("00:11:22:33:44:55")
	if err != nil {
		t.Fatal(err)
	}

	if cfg.SSID != "device-setup" || cfg.Channel != 11 {
		t.Fatalf("operator supplied settings were lost: %+v", cfg)
	}
	if cfg.Passphrase.Reveal() != "fixed-passphrase" {
		t.Fatal("operator supplied passphrase was overwritten")
	}
}

func TestHotspotConfigRejectsWeakPassphrase(t *testing.T) {
	if _, err := (HotspotConfig{Passphrase: NewSecret("short")}).WithDefaults("00:11:22:33:44:55"); err == nil {
		t.Fatal("a too short passphrase must be rejected")
	}
}

// TestHotspotConfigPassphraseIsRedacted documents the deliberate split: the
// config keeps the passphrase inside Secret, while HotspotStatus exposes it as
// plain text so an operator on the wired side can read it.
func TestHotspotConfigPassphraseIsRedacted(t *testing.T) {
	cfg, err := HotspotConfig{}.WithDefaults("00:11:22:33:44:55")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Passphrase.String() != "[REDACTED]" {
		t.Fatal("the config must not leak the passphrase into logs")
	}
}
