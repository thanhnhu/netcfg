package domain

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"netcfg/internal/kdf"
)

// Security is the authentication suite of a Wi-Fi network.
type Security string

const (
	SecOpen   Security = "open"
	SecWEP    Security = "wep"
	SecPSK    Security = "psk"
	SecSAE    Security = "sae"
	SecPSKSAE Security = "psk-sae"
)

// WiFiBackendKind identifies which subsystem drives the radio on this host.
// The value is free-form so a new backend can name itself without this file
// having to know about it.
type WiFiBackendKind string

const (
	WiFiBackendWPA  WiFiBackendKind = "wpa_supplicant"
	WiFiBackendNM   WiFiBackendKind = "network-manager"
	WiFiBackendNone WiFiBackendKind = "none"
)

// NeedsPassphrase reports whether a credential must be supplied.
func (s Security) NeedsPassphrase() bool {
	return s == SecPSK || s == SecSAE || s == SecPSKSAE
}

// Supported excludes suites we refuse to configure.
func (s Security) Supported() bool {
	switch s {
	case SecOpen, SecPSK, SecSAE, SecPSKSAE:
		return true
	default:
		return false
	}
}

// SecurityFromFlags maps a wpa_supplicant scan flag string to a suite.
func SecurityFromFlags(flags string) Security {
	hasSAE := strings.Contains(flags, "SAE")
	hasPSK := strings.Contains(flags, "PSK")
	switch {
	case hasSAE && hasPSK:
		return SecPSKSAE
	case hasSAE:
		return SecSAE
	case hasPSK:
		return SecPSK
	case strings.Contains(flags, "WEP"):
		return SecWEP
	default:
		return SecOpen
	}
}

// AccessPoint is one scan result.
type AccessPoint struct {
	SSID     string   `json:"ssid"`
	BSSID    string   `json:"bssid"`
	Freq     int      `json:"freq"`
	Band     string   `json:"band"`
	Signal   int      `json:"signal"`
	Quality  int      `json:"quality"`
	Security Security `json:"security"`
	Flags    string   `json:"flags"`
}

// Profile is a network stored inside wpa_supplicant.
type Profile struct {
	ID      int    `json:"id"`
	SSID    string `json:"ssid"`
	BSSID   string `json:"bssid"`
	Flags   string `json:"flags"`
	Current bool   `json:"current"`
	Enabled bool   `json:"enabled"`
}

// ProfileSecret is the credential stored for a saved network. Hashed marks the
// derived 256-bit key, from which the original passphrase cannot be recovered.
type ProfileSecret struct {
	SSID   string
	Value  Secret
	Hashed bool
}

// WiFiStatus is the live association state of a wireless link.
type WiFiStatus struct {
	State      string `json:"state"`
	SSID       string `json:"ssid"`
	BSSID      string `json:"bssid"`
	Freq       int    `json:"freq"`
	Signal     int    `json:"signal"`
	ProfileID  int    `json:"profileId"`
	Associated bool   `json:"associated"`
}

// WiFiRequest is an operator request to join a network.
type WiFiRequest struct {
	Link       string   `json:"link"`
	SSID       string   `json:"ssid"`
	Security   Security `json:"security"`
	Hidden     bool     `json:"hidden"`
	Passphrase Secret   `json:"-"`
}

// Validate enforces the 802.11 limits before anything touches the supplicant.
func (r WiFiRequest) Validate() error {
	if err := ValidateLinkName(r.Link); err != nil {
		return err
	}
	if err := ValidateSSID(r.SSID); err != nil {
		return err
	}
	if !r.Security.Supported() {
		if r.Security == SecWEP {
			return Invalid("WEP is not supported because it is cryptographically broken")
		}
		return Invalid("invalid security type: %q", string(r.Security))
	}
	if r.Security.NeedsPassphrase() {
		return ValidatePassphrase(r.Passphrase)
	}
	return nil
}

// ValidateSSID enforces the 32 octet limit and rejects control characters.
func ValidateSSID(ssid string) error {
	switch {
	case ssid == "":
		return Invalid("SSID must not be empty")
	case len(ssid) > 32:
		return Invalid("SSID exceeds 32 bytes")
	case !utf8.ValidString(ssid):
		return Invalid("SSID is not valid UTF-8")
	case strings.ContainsAny(ssid, "\x00\r\n"):
		return Invalid("SSID contains control characters")
	}
	return nil
}

// ValidatePassphrase checks the WPA-Personal length range.
func ValidatePassphrase(p Secret) error {
	v := p.Reveal()
	if len(v) < 8 || len(v) > 63 {
		return Invalid("Wi-Fi passphrase must be 8-63 characters")
	}
	if strings.ContainsAny(v, "\x00\r\n") {
		return Invalid("passphrase contains control characters")
	}
	return nil
}

// WPAPSK derives the 256-bit pre-shared key defined by IEEE 802.11i so the
// plaintext passphrase never has to reach wpa_supplicant.
func WPAPSK(ssid string, passphrase Secret) string {
	return hex.EncodeToString(kdf.Key([]byte(passphrase.Reveal()), []byte(ssid), 4096, 32, sha1.New))
}

// Band names the frequency range of a channel.
func Band(freq int) string {
	switch {
	case freq >= 2400 && freq < 2500:
		return "2.4 GHz"
	case freq >= 4900 && freq < 5900:
		return "5 GHz"
	case freq >= 5900 && freq < 7200:
		return "6 GHz"
	default:
		return ""
	}
}

// Quality maps dBm onto a 0-100 scale for the signal bar.
func Quality(dbm int) int {
	switch {
	case dbm >= -50:
		return 100
	case dbm <= -100:
		return 0
	default:
		return 2 * (dbm + 100)
	}
}
