// Package rpc carries a deliberately narrow, typed contract between the
// unprivileged web tier and the privileged agent over a Unix domain socket.
package rpc

import (
	"encoding/json"
	"time"

	"netcfg/internal/domain"
)

// Method names. Keeping this list short is a security property: it is the
// entire surface the unprivileged process can reach inside the root process.
const (
	MethodLinks         = "links"
	MethodSnapshot      = "snapshot"
	MethodScan          = "scan"
	MethodPlanIP        = "plan_ip"
	MethodApplyIP       = "apply_ip"
	MethodApplyWiFi     = "apply_wifi"
	MethodConfirm       = "confirm"
	MethodRollback      = "rollback"
	MethodPending       = "pending"
	MethodSelectProfile = "select_profile"
	MethodProfileSecret = "profile_secret"
	MethodRemoveProfile = "remove_profile"
	MethodDisconnect    = "disconnect"
	MethodReconnect     = "reconnect"
	MethodHotspotStatus = "hotspot_status"
	MethodHotspotStart  = "hotspot_start"
	MethodHotspotStop   = "hotspot_stop"
	MethodSystemStats   = "system_stats"
	MethodFailover      = "failover_status"
	MethodSSHStatus     = "ssh_status"
	MethodSSHEnable     = "ssh_enable"
	MethodSSHDisable    = "ssh_disable"
	MethodSubscribe     = "subscribe"
)

// Request is one call. Params is opaque here so the codec stays generic.
type Request struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// LogValue keeps request parameters out of logs; they may carry credentials.
func (r Request) LogValue() any {
	return struct {
		ID     uint64
		Method string
	}{r.ID, r.Method}
}

// Response is the reply to a Request.
type Response struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error carries a domain classification across the socket. Msg keeps the
// translatable form so the web tier can localize it for the operator.
type Error struct {
	Code domain.Code    `json:"code"`
	Msg  domain.Message `json:"msg"`
}

func (e *Error) Error() string { return e.Msg.String() }

// ToDomain rebuilds a typed error on the client side.
func (e *Error) ToDomain() error {
	return &domain.Error{Code: e.Code, Msg: e.Msg}
}

// LinkParams addresses a single interface.
type LinkParams struct {
	Link string `json:"link"`
}

// ProfileParams addresses a stored Wi-Fi profile.
type ProfileParams struct {
	Link string `json:"link"`
	ID   int    `json:"id"`
}

// SecretResult carries a stored credential in the clear; it is the one reply
// that must never be logged. Hashed marks a derived key rather than a password.
type SecretResult struct {
	SSID   string `json:"ssid"`
	Value  string `json:"value"`
	Hashed bool   `json:"hashed"`
}

// GenerationParams addresses a pending apply.
type GenerationParams struct {
	Generation domain.Generation `json:"generation"`
}

// ApplyIPParams requests an addressing change.
type ApplyIPParams struct {
	Plan          domain.IPPlan `json:"plan"`
	ConfirmWindow time.Duration `json:"confirmWindow"`
	NoRollback    bool          `json:"noRollback"`
}

// ApplyWiFiParams requests joining a network. The passphrase is a plain string
// here because it must actually cross the socket; it is never logged.
type ApplyWiFiParams struct {
	Link          string          `json:"link"`
	SSID          string          `json:"ssid"`
	Security      domain.Security `json:"security"`
	Hidden        bool            `json:"hidden"`
	Passphrase    string          `json:"passphrase"`
	ConfirmWindow time.Duration   `json:"confirmWindow"`
	NoRollback    bool            `json:"noRollback"`
}

// LinksResult lists interfaces.
type LinksResult struct {
	Links []domain.Link `json:"links"`
}

// ScanResult lists access points.
type ScanResult struct {
	Networks []domain.AccessPoint `json:"networks"`
}

// ApplyResult reports the pending change, if the safety net was armed.
type ApplyResult struct {
	Pending *domain.PendingApply `json:"pending,omitempty"`
	Warning domain.Message       `json:"warning"`
}

// PendingResult reports the change awaiting confirmation.
type PendingResult struct {
	Pending *domain.PendingApply `json:"pending,omitempty"`
}

// OKResult is the reply of commands with no payload.
type OKResult struct {
	OK bool `json:"ok"`
}

// SystemStatsResult reports host health.
type SystemStatsResult struct {
	Stats domain.SystemStats `json:"stats"`
}

// FailoverResult reports what the active failover monitor sees.
type FailoverResult struct {
	Status domain.FailoverStatus `json:"status"`
}

// SSHParams asks for a diagnostic window of a given length.
type SSHParams struct {
	Window time.Duration `json:"window"`
}

// SSHResult reports the state of the SSH server.
type SSHResult struct {
	Status domain.SSHStatus `json:"status"`
}

// HotspotResult reports the fallback access point state.
type HotspotResult struct {
	Status domain.HotspotStatus `json:"status"`
}
