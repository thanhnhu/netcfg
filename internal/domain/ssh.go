package domain

import "time"

// DefaultSSHWindow is how long remote access stays open when the operator does
// not choose otherwise.
const DefaultSSHWindow = 30 * time.Minute

const (
	minSSHWindow = 5 * time.Minute
	maxSSHWindow = 12 * time.Hour
)

// SSHStatus describes the device's SSH server. Running and EnabledAtBoot are
// separate: a server started for a diagnostic session must not be confused with
// one the operator wants back after a reboot.
type SSHStatus struct {
	Available     bool   `json:"available"`
	Unit          string `json:"unit,omitempty"`
	Running       bool   `json:"running"`
	EnabledAtBoot bool   `json:"enabledAtBoot"`
	Port          int    `json:"port,omitempty"`
	// Firewall names the packet filter found, empty when there is none.
	Firewall string `json:"firewall,omitempty"`
	// FirewallBlocks means the filter is active and does not permit the port.
	FirewallBlocks bool `json:"firewallBlocks"`
	// StopsAt is set only while netcfgd holds a timer to close access again.
	StopsAt time.Time `json:"stopsAt,omitempty"`
	Detail  Message   `json:"detail,omitempty"`
}

// ClampSSHWindow keeps the auto-close timer within a sane range.
func ClampSSHWindow(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return DefaultSSHWindow
	case d < minSSHWindow:
		return minSSHWindow
	case d > maxSSHWindow:
		return maxSSHWindow
	default:
		return d
	}
}
