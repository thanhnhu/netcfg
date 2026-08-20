// Package sysinfo reports host health by reading /proc and /sys. Nothing here
// shells out, so it stays cheap enough to poll every few seconds.
package sysinfo

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"netcfg/internal/domain"
)

// Reader keeps the previous CPU sample so utilisation is reported as a delta
// between polls rather than an average since boot.
type Reader struct {
	procDir string
	sysDir  string
	etcDir  string

	mu      sync.Mutex
	prev    cpuSample
	hasPrev bool
}

type cpuSample struct {
	total, idle uint64
}

func New() *Reader { return NewAt("/proc", "/sys", "/etc") }

// NewAt exists so tests can point at a fixture tree.
func NewAt(procDir, sysDir, etcDir string) *Reader {
	return &Reader{procDir: procDir, sysDir: sysDir, etcDir: etcDir}
}

// Stats gathers everything it can. A subsystem that cannot be read is left at
// its zero value; only a host that yields nothing at all is an error.
func (r *Reader) Stats(ctx context.Context) (domain.SystemStats, error) {
	stats := domain.SystemStats{At: time.Now()}

	load, loadErr := r.loadAverage()
	if loadErr == nil {
		stats.Load = load
	}
	memory, swap, memErr := r.memory()
	if memErr == nil {
		stats.Memory, stats.Swap = memory, swap
	}
	if loadErr != nil && memErr != nil {
		return stats, domain.Unavailable("cannot read host statistics from %s", r.procDir)
	}

	stats.UptimeSeconds = r.uptime()
	stats.CPUCount, stats.CPUPercent = r.cpu()
	stats.Host = r.host()
	stats.Filesystems = r.filesystems()
	stats.Sensors = r.sensors()
	return stats, nil
}

// host describes the machine: what it runs and what it is. Nothing here is
// specific to one board, so a Pi, a mini PC and a VM all answer as far as they
// can and leave the rest empty.
func (r *Reader) host() domain.Host {
	host := domain.Host{
		Arch:     runtime.GOARCH,
		Kernel:   readLine(filepath.Join(r.procDir, "sys", "kernel", "osrelease")),
		OS:       r.osRelease(),
		Model:    r.model(),
		CPUModel: r.cpuModel(),
	}
	host.Hostname, _ = os.Hostname()
	return host
}

// osRelease prefers PRETTY_NAME because it already carries the version, and
// falls back to composing one for the distributions that omit it.
func (r *Reader) osRelease() string {
	values := readKeyValues(filepath.Join(r.etcDir, "os-release"), "=")
	if pretty := values["PRETTY_NAME"]; pretty != "" {
		return pretty
	}
	return strings.TrimSpace(values["NAME"] + " " + values["VERSION"])
}

// model names the machine. A device tree board publishes it directly, an
// Armbian image keeps the marketing name of the box it was flashed for, and a
// PC or a VM only answers over DMI.
func (r *Reader) model() string {
	for _, path := range []string{
		filepath.Join(r.procDir, "device-tree", "model"),
		filepath.Join(r.sysDir, "firmware", "devicetree", "base", "model"),
	} {
		if model := readLine(path); model != "" {
			return model
		}
	}
	if board := r.armbianBoard(); board != "" {
		return board
	}
	return r.dmiModel()
}

// dmiPlaceholders are what a vendor leaves behind when the field was never
// filled in. Treating them as absent is what makes the fallback to the
// motherboard fields worthwhile on a self-built machine.
var dmiPlaceholders = map[string]bool{
	"to be filled by o.e.m.": true,
	"system product name":    true,
	"system manufacturer":    true,
	"default string":         true,
	"not specified":          true,
	"not applicable":         true,
	"product name":           true,
	"o.e.m.":                 true,
	"none":                   true,
	"unknown":                true,
}

func (r *Reader) dmiModel() string {
	field := func(name string) string {
		value := readLine(filepath.Join(r.sysDir, "class", "dmi", "id", name))
		if dmiPlaceholders[strings.ToLower(value)] {
			return ""
		}
		return value
	}

	if product := field("product_name"); product != "" {
		return qualify(field("sys_vendor"), product)
	}
	// A machine assembled from parts identifies itself only by its motherboard.
	return qualify(field("board_vendor"), field("board_name"))
}

// armbianBoard reads the board an Armbian image was built for, which is the
// only place the retail name of a TV box such as "X96 Max+" appears.
func (r *Reader) armbianBoard() string {
	values := readKeyValues(filepath.Join(r.etcDir, "armbian-release"), "=")
	for _, key := range []string{"BOARD_NAME", "BOARD"} {
		if value := values[key]; value != "" {
			return value
		}
	}
	return ""
}

// vendorNoise is the legal boilerplate a DMI vendor string ends with. Dropping
// it turns "ASUSTeK COMPUTER INC." into "ASUSTeK" without losing who built the
// machine.
var vendorNoise = map[string]bool{
	"co": true, "ltd": true, "inc": true, "corp": true, "corporation": true,
	"company": true, "gmbh": true, "llc": true, "computer": true, "computers": true,
}

// qualify prefixes the vendor unless the name already carries it, so a laptop
// reads "MSI Alpha 15" and not "MSI MSI Alpha 15".
func qualify(vendor, name string) string {
	if name == "" || vendor == "" {
		return name
	}

	words := strings.Fields(vendor)
	for len(words) > 1 && vendorNoise[strings.Trim(strings.ToLower(words[len(words)-1]), ".,")] {
		words = words[:len(words)-1]
	}
	vendor = strings.Trim(strings.Join(words, " "), " ,")

	if strings.Contains(strings.ToLower(name), strings.ToLower(words[0])) {
		return name
	}
	return vendor + " " + name
}

// cpuModel takes whichever key this architecture uses: x86 and most arm64
// kernels write "model name", 32-bit arm writes "Hardware". An arm64 SoC often
// writes none of them, and then the device tree names the chip instead.
func (r *Reader) cpuModel() string {
	values := readKeyValues(filepath.Join(r.procDir, "cpuinfo"), ":")
	for _, key := range []string{"model name", "Hardware", "Processor", "cpu model"} {
		if value := values[key]; value != "" {
			return value
		}
	}
	return r.soc()
}

// soc returns the chip from the device tree "compatible" list. The entries run
// from the most specific board to the SoC family, so the last one names the
// silicon: "amlogic,sm1" on an S905X3 box.
func (r *Reader) soc() string {
	for _, path := range []string{
		filepath.Join(r.procDir, "device-tree", "compatible"),
		filepath.Join(r.sysDir, "firmware", "devicetree", "base", "compatible"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		entries := strings.FieldsFunc(string(data), func(r rune) bool { return r == 0 || r == '\n' })
		if len(entries) > 0 {
			return strings.TrimSpace(entries[len(entries)-1])
		}
	}
	return ""
}

// readKeyValues parses a file of "key<sep>value" lines, keeping the first value
// for a repeated key: /proc/cpuinfo repeats every field once per core.
func readKeyValues(path, sep string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	values := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), sep)
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if _, seen := values[key]; !seen {
			values[key] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return values
}

// readLine returns the first line of a file, without the trailing NUL the
// device tree appends to its strings.
func readLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(strings.TrimRight(line, "\x00"))
}

func (r *Reader) loadAverage() ([3]float64, error) {
	var load [3]float64
	data, err := os.ReadFile(filepath.Join(r.procDir, "loadavg"))
	if err != nil {
		return load, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return load, domain.Unavailable("malformed loadavg")
	}
	for i := 0; i < 3; i++ {
		load[i], _ = strconv.ParseFloat(fields[i], 64)
	}
	return load, nil
}

func (r *Reader) uptime() int64 {
	data, err := os.ReadFile(filepath.Join(r.procDir, "uptime"))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	seconds, _ := strconv.ParseFloat(fields[0], 64)
	return int64(seconds)
}

func (r *Reader) memory() (memory, swap domain.Usage, err error) {
	f, err := os.Open(filepath.Join(r.procDir, "meminfo"))
	if err != nil {
		return memory, swap, err
	}
	defer f.Close()

	values := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		if parsed, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
			values[key] = parsed * 1024
		}
	}

	total, available := values["MemTotal"], values["MemAvailable"]
	// MemAvailable already discounts reclaimable cache, which is what an
	// operator means by "free", unlike MemFree.
	memory = domain.NewUsage(total, total-min(available, total), available)

	swapTotal, swapFree := values["SwapTotal"], values["SwapFree"]
	swap = domain.NewUsage(swapTotal, swapTotal-min(swapFree, swapTotal), swapFree)
	return memory, swap, nil
}

func (r *Reader) cpu() (count int, percent float64) {
	f, err := os.Open(filepath.Join(r.procDir, "stat"))
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	var sample cpuSample
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] != "cpu" {
			count++
			continue
		}
		for i, raw := range fields[1:] {
			value, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				continue
			}
			sample.total += value
			// Fields 3 and 4 are idle and iowait; both mean "not working".
			if i == 3 || i == 4 {
				sample.idle += value
			}
		}
	}
	if sample.total == 0 {
		return count, 0
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case r.hasPrev && sample.total > r.prev.total:
		busy := (sample.total - r.prev.total) - (sample.idle - r.prev.idle)
		percent = float64(busy) / float64(sample.total-r.prev.total) * 100
	case !r.hasPrev:
		// No previous poll yet, so report the average since boot rather than
		// showing a misleading zero on the first render.
		percent = float64(sample.total-sample.idle) / float64(sample.total) * 100
	}
	r.prev, r.hasPrev = sample, true
	return count, percent
}

func (r *Reader) filesystems() []domain.Filesystem {
	f, err := os.Open(filepath.Join(r.procDir, "mounts"))
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []domain.Filesystem
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// Only real block devices; this drops tmpfs, cgroup and the overlay
		// mounts a container runtime piles up.
		if len(fields) < 3 || !strings.HasPrefix(fields[0], "/dev/") || seen[fields[1]] {
			continue
		}
		seen[fields[1]] = true

		total, used, available, err := diskUsage(fields[1])
		if err != nil || total == 0 {
			continue
		}
		out = append(out, domain.Filesystem{
			Device:     fields[0],
			Mountpoint: unescapeMount(fields[1]),
			FSType:     fields[2],
			Usage:      domain.NewUsage(total, used, available),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Mountpoint < out[j].Mountpoint })
	return out
}

// unescapeMount decodes the octal escapes /proc/mounts uses for spaces and tabs.
func unescapeMount(path string) string {
	if !strings.Contains(path, `\0`) {
		return path
	}
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(path)
}

// hwmonSpecs maps the kernel's hwmon naming convention onto units. Anything a
// board exposes under one of these prefixes is picked up without this package
// knowing the hardware: a fan, a shunt, an NVMe probe all arrive the same way.
// Scales come from Documentation/hwmon/sysfs-interface.
var hwmonSpecs = []struct {
	prefix string
	kind   domain.SensorKind
	unit   string
	scale  float64
}{
	{"temp", domain.SensorTemperature, "°C", 1e-3},
	{"fan", domain.SensorFan, "RPM", 1},
	{"in", domain.SensorVoltage, "V", 1e-3},
	{"curr", domain.SensorCurrent, "A", 1e-3},
	{"power", domain.SensorPower, "W", 1e-6},
	{"energy", domain.SensorEnergy, "J", 1e-6},
	{"humidity", domain.SensorHumidity, "%", 1e-3},
	{"freq", domain.SensorFrequency, "Hz", 1},
}

// sensors discovers every probe the host exposes. hwmon comes first because it
// carries labels and alarm thresholds; thermal zones then fill in boards whose
// probes are not mirrored into hwmon, and power supplies cover battery or UPS
// hats. Nothing here is specific to one device.
func (r *Reader) sensors() []domain.SensorGroup {
	groups, known := r.hwmonGroups()
	groups = append(groups, r.thermalGroups(known)...)
	groups = append(groups, r.powerSupplyGroups()...)

	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	return groups
}

// hwmonGroups returns one group per chip plus the set of names already covered,
// so thermal zones do not report the same probe a second time.
func (r *Reader) hwmonGroups() ([]domain.SensorGroup, map[string]bool) {
	known := map[string]bool{}
	chips, _ := filepath.Glob(filepath.Join(r.sysDir, "class", "hwmon", "hwmon*"))

	var groups []domain.SensorGroup
	for _, chip := range chips {
		name := readTrimmed(filepath.Join(chip, "name"))
		if name == "" {
			name = filepath.Base(chip)
		}

		var sensors []domain.Sensor
		unlabelled := 0
		for _, spec := range hwmonSpecs {
			inputs, _ := filepath.Glob(filepath.Join(chip, spec.prefix+"*_input"))
			sort.Strings(inputs)

			for _, input := range inputs {
				value, ok := readScaled(input, spec.scale)
				if !ok {
					continue
				}
				base := strings.TrimSuffix(input, "_input")
				sensor := domain.Sensor{
					Label: readTrimmed(base + "_label"),
					Kind:  spec.kind,
					Value: value,
					Unit:  spec.unit,
				}
				if sensor.Label == "" {
					sensor.Label = filepath.Base(base)
					unlabelled++
				}
				sensor.High, _ = readScaled(base+"_max", spec.scale)
				sensor.Critical, _ = readScaled(base+"_crit", spec.scale)
				sensors = append(sensors, sensor)
			}
		}
		if len(sensors) == 0 {
			continue
		}

		// A lone unlabelled probe is better named after its chip than "temp1",
		// which is how most single-sensor SBC boards look.
		if len(sensors) == 1 && unlabelled == 1 {
			sensors[0].Label = name
		}
		known[sensorKey(name)] = true
		for _, sensor := range sensors {
			known[sensorKey(sensor.Label)] = true
		}
		groups = append(groups, domain.SensorGroup{Name: name, Sensors: sensors})
	}
	return groups, known
}

func (r *Reader) thermalGroups(known map[string]bool) []domain.SensorGroup {
	zones, _ := filepath.Glob(filepath.Join(r.sysDir, "class", "thermal", "thermal_zone*"))
	sort.Strings(zones)

	var groups []domain.SensorGroup
	for _, zone := range zones {
		label := readTrimmed(filepath.Join(zone, "type"))
		if label == "" || known[sensorKey(label)] {
			continue
		}
		value, ok := readScaled(filepath.Join(zone, "temp"), 1e-3)
		if !ok {
			continue
		}
		known[sensorKey(label)] = true
		groups = append(groups, domain.SensorGroup{
			Name:    label,
			Sensors: []domain.Sensor{{Label: label, Kind: domain.SensorTemperature, Value: value, Unit: "°C"}},
		})
	}
	return groups
}

// powerSupplyGroups covers batteries, UPS hats and mains adapters.
func (r *Reader) powerSupplyGroups() []domain.SensorGroup {
	supplies, _ := filepath.Glob(filepath.Join(r.sysDir, "class", "power_supply", "*"))
	sort.Strings(supplies)

	readings := []struct {
		file  string
		label string
		kind  domain.SensorKind
		unit  string
		scale float64
	}{
		{"capacity", "capacity", domain.SensorCharge, "%", 1},
		{"voltage_now", "voltage", domain.SensorVoltage, "V", 1e-6},
		{"current_now", "current", domain.SensorCurrent, "A", 1e-6},
		{"power_now", "power", domain.SensorPower, "W", 1e-6},
		{"temp", "temperature", domain.SensorTemperature, "°C", 0.1},
	}

	var groups []domain.SensorGroup
	for _, supply := range supplies {
		name := filepath.Base(supply)
		var sensors []domain.Sensor

		if status := readTrimmed(filepath.Join(supply, "status")); status != "" {
			sensors = append(sensors, domain.Sensor{Label: "status", Kind: domain.SensorState, Text: status})
		}
		for _, reading := range readings {
			if value, ok := readScaled(filepath.Join(supply, reading.file), reading.scale); ok {
				sensors = append(sensors, domain.Sensor{
					Label: reading.label, Kind: reading.kind, Value: value, Unit: reading.unit,
				})
			}
		}
		if len(sensors) > 0 {
			groups = append(groups, domain.SensorGroup{Name: name, Sensors: sensors})
		}
	}
	return groups
}

// sensorKey folds the naming difference between thermal zones (cpu-thermal)
// and hwmon (cpu_thermal) so the same probe is not reported twice.
func sensorKey(label string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(label)), "_", "-")
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readScaled converts a sysfs integer to its natural unit. Disconnected probes
// park at implausible values, so obvious nonsense is dropped rather than shown.
func readScaled(path string, scale float64) (float64, bool) {
	raw := readTrimmed(path)
	if raw == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	value := float64(parsed) * scale
	if value < -1e9 || value > 1e12 {
		return 0, false
	}
	return value, true
}
