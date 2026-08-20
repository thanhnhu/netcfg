package domain

import "time"

// DemotedMetric is the kernel metric a failing link is pushed to. It is far
// above anything an operator would configure but still below the maximum, so a
// demoted route stays visible and reversible instead of disappearing.
const DemotedMetric uint32 = 4_000_000_000

// LinkHealth is what the failover monitor knows about one candidate interface.
// It describes the kernel and the path, not the configuration: a link can be
// up, carry an address and still fail to forward.
type LinkHealth struct {
	Link      string  `json:"link"`
	Gateway   string  `json:"gateway,omitempty"`
	Metric    uint32  `json:"metric,omitempty"`
	AdminUp   bool    `json:"adminUp"`
	OperUp    bool    `json:"operUp"`
	Reachable bool    `json:"reachable"`
	Detail    Message `json:"detail"`
	// Failures and Successes count consecutive probe results, which is what the
	// thresholds act on; a single lost packet must not move the default route.
	Failures  int `json:"failures"`
	Successes int `json:"successes"`
	// Demoted marks a link the monitor pushed down the routing table. The change
	// lives in the kernel only, so a reboot undoes it.
	Demoted   bool      `json:"demoted"`
	Since     time.Time `json:"since"`
	CheckedAt time.Time `json:"checkedAt"`
}

// Healthy reports whether the link is usable as a default route right now.
func (h LinkHealth) Healthy() bool { return h.OperUp && h.Reachable }

// FailoverStatus is the monitor's whole view, as shown on the failover panel.
type FailoverStatus struct {
	Enabled  bool          `json:"enabled"`
	Interval time.Duration `json:"interval"`
	Fails    int           `json:"fails"`
	Recovers int           `json:"recovers"`
	Targets  []string      `json:"targets,omitempty"`
	Links    []LinkHealth  `json:"links"`
	Detail   Message       `json:"detail,omitempty"`
	At       time.Time     `json:"at"`
}

// Health returns the record for a link, if the monitor has one.
func (s FailoverStatus) Health(link string) (LinkHealth, bool) {
	for _, h := range s.Links {
		if h.Link == link {
			return h, true
		}
	}
	return LinkHealth{}, false
}
