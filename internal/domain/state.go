package domain

import "time"

// StateVersion is the schema version of the persisted desired state.
const StateVersion = 1

// Generation identifies one committed change set. It only ever increases.
type Generation uint64

// ChangeKind tells the operator what a pending apply touched.
type ChangeKind string

const (
	ChangeIP   ChangeKind = "ip"
	ChangeWiFi ChangeKind = "wifi"
)

// LinkDesired is the configuration the operator asked for on one link.
type LinkDesired struct {
	IP       *IPPlan `json:"ip,omitempty"`
	WiFiSSID string  `json:"wifiSsid,omitempty"`
}

// DesiredState is the single source of truth the reconciler drives towards.
type DesiredState struct {
	Version    int                    `json:"version"`
	Generation Generation             `json:"generation"`
	UpdatedAt  time.Time              `json:"updatedAt"`
	Links      map[string]LinkDesired `json:"links"`
}

// NewDesiredState returns an empty state at the current schema version.
func NewDesiredState() DesiredState {
	return DesiredState{Version: StateVersion, Links: map[string]LinkDesired{}}
}

// Clone deep copies the state so callers cannot mutate a shared map.
func (s DesiredState) Clone() DesiredState {
	out := s
	out.Links = make(map[string]LinkDesired, len(s.Links))
	for name, link := range s.Links {
		copied := link
		if link.IP != nil {
			plan := *link.IP
			plan.DNS = append([]string(nil), link.IP.DNS...)
			copied.IP = &plan
		}
		out.Links[name] = copied
	}
	return out
}

// ProbeResult is the connectivity verdict after an apply.
type ProbeResult struct {
	OK     bool      `json:"ok"`
	Detail Message   `json:"detail"`
	At     time.Time `json:"at"`
}

// PendingApply is a change waiting for operator confirmation. If it is not
// confirmed before Deadline the agent restores the previous configuration.
type PendingApply struct {
	Generation Generation  `json:"generation"`
	Kind       ChangeKind  `json:"kind"`
	Link       string      `json:"link"`
	StartedAt  time.Time   `json:"startedAt"`
	Deadline   time.Time   `json:"deadline"`
	Probe      ProbeResult `json:"probe"`
	Summary    []Change    `json:"summary"`
}

// Remaining is the time left before automatic rollback.
func (p PendingApply) Remaining(now time.Time) time.Duration {
	if d := p.Deadline.Sub(now); d > 0 {
		return d
	}
	return 0
}

// ConfirmWindow bounds how long an operator may take to confirm an apply.
const (
	MinConfirmWindow     = 15 * time.Second
	MaxConfirmWindow     = 10 * time.Minute
	DefaultConfirmWindow = 90 * time.Second
)

// ClampConfirmWindow keeps an operator supplied window inside safe bounds.
func ClampConfirmWindow(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return DefaultConfirmWindow
	case d < MinConfirmWindow:
		return MinConfirmWindow
	case d > MaxConfirmWindow:
		return MaxConfirmWindow
	default:
		return d
	}
}
