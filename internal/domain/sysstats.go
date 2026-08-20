package domain

import "time"

// Usage is a consumed / total pair in bytes.
type Usage struct {
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Available uint64  `json:"available"`
	Percent   float64 `json:"percent"`
}

// NewUsage fills in the derived percentage. The denominator is the capacity a
// caller can actually use, so a filesystem matches what df prints: ext4 keeps
// ~5% reserved for root, which df excludes from Use% and so does this.
func NewUsage(total, used, available uint64) Usage {
	u := Usage{Total: total, Used: used, Available: available}
	if usable := used + available; usable > 0 {
		u.Percent = float64(used) / float64(usable) * 100
	}
	return u
}

// Filesystem is one mounted volume backed by a real block device.
type Filesystem struct {
	Device     string `json:"device"`
	Mountpoint string `json:"mountpoint"`
	FSType     string `json:"fstype"`
	Usage      Usage  `json:"usage"`
}

// SensorKind lets the interface pick a unit and a sensible number of decimals
// without hard coding which sensors a given board happens to have.
type SensorKind string

const (
	SensorTemperature SensorKind = "temperature"
	SensorFan         SensorKind = "fan"
	SensorVoltage     SensorKind = "voltage"
	SensorCurrent     SensorKind = "current"
	SensorPower       SensorKind = "power"
	SensorEnergy      SensorKind = "energy"
	SensorHumidity    SensorKind = "humidity"
	SensorFrequency   SensorKind = "frequency"
	SensorCharge      SensorKind = "charge"
	SensorState       SensorKind = "state"
)

// Sensor is one reading. Text carries non-numeric readings such as a battery
// reporting "Discharging"; Value is meaningless when Text is set.
type Sensor struct {
	Label    string     `json:"label"`
	Kind     SensorKind `json:"kind"`
	Value    float64    `json:"value"`
	Unit     string     `json:"unit,omitempty"`
	Text     string     `json:"text,omitempty"`
	High     float64    `json:"high,omitempty"`
	Critical float64    `json:"critical,omitempty"`
}

// SensorGroup is one physical chip or power supply and everything it exposes.
type SensorGroup struct {
	Name    string   `json:"name"`
	Sensors []Sensor `json:"sensors"`
}

// Host identifies the machine itself rather than its load. Every field is best
// effort: a board that publishes no model name is still a usable host.
type Host struct {
	Hostname string `json:"hostname,omitempty"`
	OS       string `json:"os,omitempty"`
	Kernel   string `json:"kernel,omitempty"`
	Arch     string `json:"arch,omitempty"`
	Model    string `json:"model,omitempty"`
	CPUModel string `json:"cpuModel,omitempty"`
}

// SystemStats is the host health snapshot shown on the metrics panel. Every
// field degrades independently: a machine without thermal sensors still
// reports CPU and memory.
type SystemStats struct {
	At            time.Time     `json:"at"`
	Host          Host          `json:"host"`
	UptimeSeconds int64         `json:"uptimeSeconds"`
	CPUCount      int           `json:"cpuCount"`
	CPUPercent    float64       `json:"cpuPercent"`
	Load          [3]float64    `json:"load"`
	Memory        Usage         `json:"memory"`
	Swap          Usage         `json:"swap"`
	Filesystems   []Filesystem  `json:"filesystems"`
	Sensors       []SensorGroup `json:"sensors"`
}
