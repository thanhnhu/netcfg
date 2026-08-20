package domain

import "time"

// EventType names a push notification sent to the browser over SSE.
type EventType string

const (
	EventLinks          EventType = "links"
	EventWiFiState      EventType = "wifi_state"
	EventScanResults    EventType = "scan_results"
	EventApplyPending   EventType = "apply_pending"
	EventApplyConfirmed EventType = "apply_confirmed"
	EventApplyReverted  EventType = "apply_reverted"
	EventProbe          EventType = "probe"
	EventHotspot        EventType = "hotspot"
	EventFailover       EventType = "failover"
	EventLog            EventType = "log"
)

// Event is a state change broadcast by the agent. Data must never carry secrets.
type Event struct {
	Type EventType `json:"type"`
	Link string    `json:"link,omitempty"`
	Text Message   `json:"message"`
	Data any       `json:"data,omitempty"`
	At   time.Time `json:"at"`
}

// NewEvent stamps an event with the current time.
func NewEvent(t EventType, link string, text Message, data any) Event {
	return Event{Type: t, Link: link, Text: text, Data: data, At: time.Now()}
}
