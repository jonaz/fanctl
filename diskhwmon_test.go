package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDiskSysfs builds a sysfs tree with:
//
//	hwmon0 coretemp            (not a disk, ignored)
//	hwmon1 drivetemp → sda     41 °C
//	hwmon2 drivetemp → sdb     45 °C
//	hwmon3 nvme → nvme0n1      38 °C
//	hwmon4 nvme (multipath) → nvme1c1n1, i.e. /dev/nvme1n1, 36 °C
func fakeDiskSysfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	sysRoot = root
	t.Cleanup(func() { sysRoot = "/" })
	write := func(p, s string) {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	hw := func(n, name, temp string) string {
		d := "sys/class/hwmon/" + n
		write(d+"/name", name+"\n")
		write(d+"/temp1_input", temp+"\n")
		return d
	}
	hw("hwmon0", "coretemp", "90000")
	mkdir(hw("hwmon1", "drivetemp", "41000") + "/device/block/sda")
	mkdir(hw("hwmon2", "drivetemp", "45000") + "/device/block/sdb")
	d := hw("hwmon3", "nvme", "38000")
	mkdir(d + "/device/nvme0n1")
	mkdir(d + "/device/ng0n1") // char device node, not a namespace
	mkdir(hw("hwmon4", "nvme", "36000") + "/device/nvme1c1n1")
	return root
}

func TestScanDiskHwmon(t *testing.T) {
	fakeDiskSysfs(t)
	var got []string
	for _, h := range scanDiskHwmon() {
		got = append(got, filepath.Base(h.dir)+"="+h.label())
	}
	want := "hwmon1=/dev/sda hwmon2=/dev/sdb hwmon3=/dev/nvme0n1 hwmon4=/dev/nvme1n1"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
}

func TestHwmonDiskSensorMax(t *testing.T) {
	fakeDiskSysfs(t)
	r, err := (&hwmonDiskSensor{}).Read(context.Background())
	if err != nil || r.Temp != 45 || r.Detail != "hottest /dev/sdb" || r.Partial != nil {
		t.Fatalf("got %+v, %v; want 45 from /dev/sdb", r, err)
	}
}

func TestHwmonDiskSensorDevicesFilter(t *testing.T) {
	fakeDiskSysfs(t)
	r, err := (&hwmonDiskSensor{devices: []string{"/dev/sda", "/dev/nvme0n1", "/dev/nvme1n1"}}).Read(context.Background())
	if err != nil || r.Temp != 41 || r.Detail != "hottest /dev/sda" || r.Partial != nil {
		t.Fatalf("got %+v, %v; want 41 from /dev/sda (sdb not listed)", r, err)
	}
}

func TestHwmonDiskSensorMissingDevice(t *testing.T) {
	fakeDiskSysfs(t)
	r, err := (&hwmonDiskSensor{devices: []string{"/dev/sda", "/dev/sdz"}}).Read(context.Background())
	if err != nil || r.Temp != 41 || r.Partial == nil || !strings.Contains(r.Partial.Error(), "/dev/sdz: no hwmon") {
		t.Fatalf("got %+v, %v; want 41 with /dev/sdz partial", r, err)
	}
	// Only missing disks: no data at all.
	if _, err := (&hwmonDiskSensor{devices: []string{"/dev/sdz"}}).Read(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "/dev/sdz") {
		t.Fatalf("got %v, want an error naming /dev/sdz", err)
	}
}

func TestHwmonDiskSensorBadRead(t *testing.T) {
	root := fakeDiskSysfs(t)
	os.WriteFile(filepath.Join(root, "sys/class/hwmon/hwmon2/temp1_input"), []byte("garbage\n"), 0o644)
	r, err := (&hwmonDiskSensor{}).Read(context.Background())
	if err != nil || r.Temp != 41 || r.Partial == nil || !strings.Contains(r.Partial.Error(), "/dev/sdb") {
		t.Fatalf("got %+v, %v; want 41 with /dev/sdb partial", r, err)
	}
}

func TestHwmonDiskSensorNoHwmon(t *testing.T) {
	sysRoot = t.TempDir()
	defer func() { sysRoot = "/" }()
	if _, err := (&hwmonDiskSensor{}).Read(context.Background()); err == nil {
		t.Fatal("want an error with no disk hwmon")
	}
}

func TestDisksBackendConfig(t *testing.T) {
	ok := [][2]float64{{1, 1}, {2, 2}}
	cfg := func(typ, backend string) Config {
		c := Config{Zones: []Zone{{Name: "a", Sources: []Source{{Type: typ, Backend: backend, Curve: ok}}}}}
		c.applyDefaults()
		return c
	}
	c := cfg("disks", "")
	if err := c.Validate(); err != nil || c.Zones[0].Sources[0].Backend != "smartctl" {
		t.Fatalf("default backend %q, err %v; want smartctl", c.Zones[0].Sources[0].Backend, err)
	}
	for _, b := range []struct{ typ, backend, want string }{
		{"disks", "hwmon", ""},
		{"disks", "sysfs", "want smartctl or hwmon"},
		{"cpu", "hwmon", "only for disks"},
	} {
		c := cfg(b.typ, b.backend)
		err := c.Validate()
		if (b.want == "" && err != nil) || (b.want != "" && (err == nil || !strings.Contains(err.Error(), b.want))) {
			t.Errorf("%s backend %q: got %v, want %q", b.typ, b.backend, err, b.want)
		}
	}
}

// The hwmon backend doesn't need smartctl, and fails at startup without
// any disk hwmon unless the source is optional.
func TestBuildZonesHwmonDisks(t *testing.T) {
	c, err := LoadConfig("fanctl.example.json")
	if err != nil {
		t.Fatal(err)
	}
	c.Zones[1].Sources[0].Backend = "hwmon"
	look := func(bin string) (string, error) {
		if bin == "smartctl" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + bin, nil
	}
	fakeDiskSysfs(t)
	zones, err := buildZones(c, &fakeRunner{}, newIPMI(nil, false), look)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := zones[1].sources[0].sensor.(*hwmonDiskSensor); !ok {
		t.Fatalf("disks sensor is %T, want *hwmonDiskSensor", zones[1].sources[0].sensor)
	}

	sysRoot = t.TempDir() // no hwmon at all
	if _, err := buildZones(c, &fakeRunner{}, newIPMI(nil, false), look); err == nil ||
		!strings.Contains(err.Error(), "modprobe drivetemp") {
		t.Fatalf("got %v, want a modprobe drivetemp hint", err)
	}
	c.Zones[1].Sources[0].Optional = true
	zones, err = buildZones(c, &fakeRunner{}, newIPMI(nil, false), look)
	if err != nil {
		t.Fatal(err)
	}
	if got := zones[1].sources[0].sensor.Name(); got == "disks" {
		t.Fatal("optional hwmon disks without hwmon should be skipped")
	}
}
