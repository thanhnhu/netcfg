package domain

import (
	"time"
)

// HotspotMode records how the radio is shared while the fallback AP is up.
type HotspotMode string

const (
	// HotspotConcurrent uses a virtual AP interface next to the client role.
	HotspotConcurrent HotspotMode = "concurrent"
	// HotspotExclusive takes the radio over, so no client connection is possible
	// until the hotspot stops.
	HotspotExclusive HotspotMode = "exclusive"
)

// HotspotConfig describes the fallback access point.
type HotspotConfig struct {
	Link       string `json:"link"`
	SSID       string `json:"ssid"`
	Passphrase Secret `json:"-"`
	Channel    int    `json:"channel"`
	Country    string `json:"country"`
	Address    string `json:"address"`
}

// HotspotStatus is what the UI and the logs show about the fallback AP.
type HotspotStatus struct {
	Active bool        `json:"active"`
	Link   string      `json:"link"`
	Mode   HotspotMode `json:"mode,omitempty"`
	SSID   string      `json:"ssid,omitempty"`
	// Passphrase is intentionally exposed: an operator on the Ethernet side must
	// be able to read it in order to join the fallback network.
	Passphrase string    `json:"passphrase,omitempty"`
	Address    string    `json:"address,omitempty"`
	PortalURL  string    `json:"portalUrl,omitempty"`
	Since      time.Time `json:"since,omitempty"`
	Clients    int       `json:"clients"`
	Reason     Message   `json:"reason"`
}

// DefaultHotspotAddress is the portal address handed out by the fallback AP.
const DefaultHotspotAddress = "192.168.4.1/24"

// DefaultHotspotSSID is what the fallback AP calls itself. A fixed name is the
// point: an operator standing next to a device they cannot reach has to
// recognise the network without being told.
const DefaultHotspotSSID = "netcfg"

// DefaultHotspotPassphrase is deliberately guessable, for the same reason the
// SSID is: a generated one is unreadable to somebody who cannot open the
// interface, which is exactly when this network matters. It is also the shortest
// WPA2 accepts, so anyone within radio range can join. Pin your own with
// -ap-passphrase on any device where that is not an acceptable trade.
const DefaultHotspotPassphrase = "12345678"

// WithDefaults fills in anything the operator did not configure.
func (c HotspotConfig) WithDefaults(mac string) (HotspotConfig, error) {
	out := c
	if out.SSID == "" {
		out.SSID = DefaultHotspotSSID
	}
	if err := ValidateSSID(out.SSID); err != nil {
		return out, err
	}
	if out.Channel <= 0 {
		out.Channel = 6
	}
	if out.Country == "" {
		out.Country = "VN"
	}
	if out.Address == "" {
		out.Address = DefaultHotspotAddress
	}
	if out.Passphrase.Empty() {
		out.Passphrase = NewSecret(DefaultHotspotPassphrase)
	}
	return out, ValidatePassphrase(out.Passphrase)
}
