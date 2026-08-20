package sysinfo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"netcfg/internal/domain"
)

// write creates one sysfs attribute file.
func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func groupNames(groups []domain.SensorGroup) []string {
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		names = append(names, g.Name)
	}
	return names
}

func findGroup(t *testing.T, groups []domain.SensorGroup, name string) domain.SensorGroup {
	t.Helper()
	for _, g := range groups {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("group %q not found in %v", name, groupNames(groups))
	return domain.SensorGroup{}
}

// TestSensorsAreDiscoveredNotHardcoded builds a tree that mixes an x86 style
// labelled chip, an SBC style single probe, a fan, a shunt and a battery. None
// of these names appear in the adapter.
func TestSensorsAreDiscoveredNotHardcoded(t *testing.T) {
	sys := t.TempDir()

	// Labelled multi-probe chip, as coretemp reports on x86.
	coretemp := filepath.Join(sys, "class", "hwmon", "hwmon0")
	write(t, filepath.Join(coretemp, "name"), "coretemp")
	write(t, filepath.Join(coretemp, "temp1_label"), "Package id 0")
	write(t, filepath.Join(coretemp, "temp1_input"), "48000")
	write(t, filepath.Join(coretemp, "temp1_crit"), "100000")
	write(t, filepath.Join(coretemp, "temp2_label"), "Core 0")
	write(t, filepath.Join(coretemp, "temp2_input"), "46000")

	// Single unlabelled probe, as an SBC reports.
	sbc := filepath.Join(sys, "class", "hwmon", "hwmon1")
	write(t, filepath.Join(sbc, "name"), "cpu_thermal")
	write(t, filepath.Join(sbc, "temp1_input"), "56200")

	// A chip that is not a thermometer at all.
	psu := filepath.Join(sys, "class", "hwmon", "hwmon2")
	write(t, filepath.Join(psu, "name"), "ina219")
	write(t, filepath.Join(psu, "in1_input"), "5040")
	write(t, filepath.Join(psu, "curr1_input"), "1250")
	write(t, filepath.Join(psu, "power1_input"), "6300000")
	write(t, filepath.Join(psu, "fan1_input"), "2400")

	// Two thermal zones: one mirrors hwmon1, the other is unique to this tree.
	write(t, filepath.Join(sys, "class", "thermal", "thermal_zone0", "type"), "cpu-thermal")
	write(t, filepath.Join(sys, "class", "thermal", "thermal_zone0", "temp"), "56200")
	write(t, filepath.Join(sys, "class", "thermal", "thermal_zone1", "type"), "ddr-thermal")
	write(t, filepath.Join(sys, "class", "thermal", "thermal_zone1", "temp"), "57200")

	battery := filepath.Join(sys, "class", "power_supply", "BAT0")
	write(t, filepath.Join(battery, "status"), "Discharging")
	write(t, filepath.Join(battery, "capacity"), "82")
	write(t, filepath.Join(battery, "voltage_now"), "11400000")

	groups := NewAt(t.TempDir(), sys, t.TempDir()).sensors()

	t.Run("labelled probes keep their labels", func(t *testing.T) {
		g := findGroup(t, groups, "coretemp")
		if len(g.Sensors) != 2 {
			t.Fatalf("got %d sensors, want 2", len(g.Sensors))
		}
		if g.Sensors[0].Label != "Package id 0" || g.Sensors[0].Value != 48 {
			t.Errorf("got %+v", g.Sensors[0])
		}
		if g.Sensors[0].Critical != 100 {
			t.Errorf("critical threshold = %v, want 100", g.Sensors[0].Critical)
		}
	})

	t.Run("a lone unlabelled probe takes the chip name", func(t *testing.T) {
		g := findGroup(t, groups, "cpu_thermal")
		if g.Sensors[0].Label != "cpu_thermal" {
			t.Errorf("label = %q, want the chip name", g.Sensors[0].Label)
		}
	})

	t.Run("non thermal probes are scaled to their natural unit", func(t *testing.T) {
		g := findGroup(t, groups, "ina219")
		want := map[domain.SensorKind]struct {
			value float64
			unit  string
		}{
			domain.SensorVoltage: {5.04, "V"},
			domain.SensorCurrent: {1.25, "A"},
			domain.SensorPower:   {6.3, "W"},
			domain.SensorFan:     {2400, "RPM"},
		}
		for _, sensor := range g.Sensors {
			expected, ok := want[sensor.Kind]
			if !ok {
				t.Errorf("unexpected sensor kind %q", sensor.Kind)
				continue
			}
			if sensor.Value != expected.value || sensor.Unit != expected.unit {
				t.Errorf("%s = %v %s, want %v %s", sensor.Kind, sensor.Value, sensor.Unit, expected.value, expected.unit)
			}
			delete(want, sensor.Kind)
		}
		if len(want) != 0 {
			t.Errorf("missing sensor kinds: %v", want)
		}
	})

	t.Run("a thermal zone mirroring hwmon is not reported twice", func(t *testing.T) {
		for _, name := range groupNames(groups) {
			if name == "cpu-thermal" {
				t.Fatalf("cpu-thermal duplicates the cpu_thermal chip: %v", groupNames(groups))
			}
		}
	})

	t.Run("a thermal zone with no hwmon twin still appears", func(t *testing.T) {
		g := findGroup(t, groups, "ddr-thermal")
		if g.Sensors[0].Value != 57.2 {
			t.Errorf("value = %v, want 57.2", g.Sensors[0].Value)
		}
	})

	t.Run("power supplies report state and charge", func(t *testing.T) {
		g := findGroup(t, groups, "BAT0")
		if g.Sensors[0].Text != "Discharging" {
			t.Errorf("first sensor = %+v, want the status text", g.Sensors[0])
		}
		var capacity, voltage float64
		for _, s := range g.Sensors {
			switch s.Kind {
			case domain.SensorCharge:
				capacity = s.Value
			case domain.SensorVoltage:
				voltage = s.Value
			}
		}
		if capacity != 82 || voltage != 11.4 {
			t.Errorf("capacity = %v, voltage = %v; want 82 and 11.4", capacity, voltage)
		}
	})
}

// TestStatsSurvivesAnEmptyHost proves the panel degrades instead of failing on
// a machine that exposes no sensor at all.
func TestStatsSurvivesAnEmptyHost(t *testing.T) {
	proc := t.TempDir()
	write(t, filepath.Join(proc, "loadavg"), "0.10 0.20 0.30 1/2 3")
	write(t, filepath.Join(proc, "meminfo"), "MemTotal:  1000 kB\nMemAvailable:  400 kB")

	stats, err := NewAt(proc, t.TempDir(), t.TempDir()).Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Sensors) != 0 {
		t.Errorf("got %d sensor groups, want none", len(stats.Sensors))
	}
	if stats.Memory.Percent != 60 {
		t.Errorf("memory = %v%%, want 60", stats.Memory.Percent)
	}
}

// TestHostIsReadFromTheFixtureTree checks the identity fields come from the
// files a board actually publishes, not from the machine running the test.
func TestHostIsReadFromTheFixtureTree(t *testing.T) {
	proc, sys, etc := t.TempDir(), t.TempDir(), t.TempDir()
	write(t, filepath.Join(proc, "loadavg"), "0.10 0.20 0.30 1/2 3")
	write(t, filepath.Join(proc, "meminfo"), "MemTotal:  1000 kB\nMemAvailable:  400 kB")
	write(t, filepath.Join(proc, "sys", "kernel", "osrelease"), "6.12.0-rpi")
	write(t, filepath.Join(proc, "cpuinfo"), "processor\t: 0\nmodel name\t: Cortex-A76\n")
	// The device tree NUL terminates its strings.
	write(t, filepath.Join(proc, "device-tree", "model"), "Raspberry Pi 5 Model B\x00")
	write(t, filepath.Join(etc, "os-release"), `PRETTY_NAME="Debian GNU/Linux 13 (trixie)"`)

	stats, err := NewAt(proc, sys, etc).Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := stats.Host
	if got.OS != "Debian GNU/Linux 13 (trixie)" || got.Kernel != "6.12.0-rpi" {
		t.Errorf("os = %q, kernel = %q", got.OS, got.Kernel)
	}
	if got.Model != "Raspberry Pi 5 Model B" || got.CPUModel != "Cortex-A76" {
		t.Errorf("model = %q, cpu = %q", got.Model, got.CPUModel)
	}
}

func TestStatsFailsWhenProcIsUnreadable(t *testing.T) {
	if _, err := NewAt(filepath.Join(t.TempDir(), "absent"), t.TempDir(), t.TempDir()).Stats(context.Background()); err == nil {
		t.Fatal("a host with no /proc must report an error")
	}
}

// TestModelFallsBackAcrossHardwareStyles covers the three kinds of machine this
// runs on: a box whose retail name only Armbian knows, a PC that answers over
// DMI, and one whose vendor left the product fields unfilled.
func TestModelFallsBackAcrossHardwareStyles(t *testing.T) {
	t.Run("armbian box without a device tree model", func(t *testing.T) {
		proc, sys, etc := t.TempDir(), t.TempDir(), t.TempDir()
		write(t, filepath.Join(etc, "armbian-release"), `BOARD_NAME="X96 Max+"`)
		// An arm64 SoC names no CPU in cpuinfo, so the chip comes from the tree.
		write(t, filepath.Join(proc, "cpuinfo"), "processor\t: 0\nCPU implementer\t: 0x41")
		write(t, filepath.Join(proc, "device-tree", "compatible"), "amlogic,x96-max-plus\x00amlogic,sm1\x00")

		r := NewAt(proc, sys, etc)
		if got := r.model(); got != "X96 Max+" {
			t.Errorf("model = %q, want X96 Max+", got)
		}
		if got := r.cpuModel(); got != "amlogic,sm1" {
			t.Errorf("cpu = %q, want amlogic,sm1", got)
		}
	})

	t.Run("PC with DMI", func(t *testing.T) {
		sys := t.TempDir()
		write(t, filepath.Join(sys, "class", "dmi", "id", "sys_vendor"), "Micro-Star International Co., Ltd.")
		write(t, filepath.Join(sys, "class", "dmi", "id", "product_name"), "MS-7C56")

		want := "Micro-Star International MS-7C56"
		if got := NewAt(t.TempDir(), sys, t.TempDir()).model(); got != want {
			t.Errorf("model = %q, want %q", got, want)
		}
	})

	t.Run("vendor already in the product name", func(t *testing.T) {
		sys := t.TempDir()
		write(t, filepath.Join(sys, "class", "dmi", "id", "sys_vendor"), "ASUSTeK COMPUTER INC.")
		write(t, filepath.Join(sys, "class", "dmi", "id", "product_name"), "ASUSTeK Vivobook 15")

		if got := NewAt(t.TempDir(), sys, t.TempDir()).model(); got != "ASUSTeK Vivobook 15" {
			t.Errorf("model = %q, want the product name without a repeated vendor", got)
		}
	})

	t.Run("motherboard only", func(t *testing.T) {
		sys := t.TempDir()
		write(t, filepath.Join(sys, "class", "dmi", "id", "sys_vendor"), "To Be Filled By O.E.M.")
		write(t, filepath.Join(sys, "class", "dmi", "id", "product_name"), "To Be Filled By O.E.M.")
		write(t, filepath.Join(sys, "class", "dmi", "id", "board_vendor"), "ASUSTeK COMPUTER INC.")
		write(t, filepath.Join(sys, "class", "dmi", "id", "board_name"), "PRIME B450M-A")

		want := "ASUSTeK PRIME B450M-A"
		if got := NewAt(t.TempDir(), sys, t.TempDir()).model(); got != want {
			t.Errorf("model = %q, want %q", got, want)
		}
	})
}
