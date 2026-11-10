package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// fakeDiskSysfs builds a sysfs tree with:
//
//	hwmon0 coretemp            (not a disk, ignored)
//	hwmon1 drivetemp → sda     41 °C
//	hwmon2 drivetemp → sdb     45 °C
//	hwmon3 nvme → nvme0n1      38 °C
//	hwmon4 nvme (multipath) → nvme1c1n1, i.e. /dev/nvme1n1, 36 °C
//	hwmon5 drivetemp → sdd     60 °C, removable (USB), not autodetected
//	hwmon6 nvme (multipath) → nvme1c2n1: a second path to /dev/nvme1n1, 40 °C
//	sdc: a SATA SSD drivetemp found no sensor on
//
// with drivetemp loaded.
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
	mkdir(hw("hwmon5", "drivetemp", "60000") + "/device/block/sdd")
	mkdir(hw("hwmon6", "nvme", "40000") + "/device/nvme1c2n1")
	for _, b := range []string{"sda", "sdb", "sdc", "sdd", "nvme0n1", "nvme1n1"} {
		write("sys/block/"+b+"/removable", "0\n")
	}
	write("sys/block/sdd/removable", "1\n")
	mkdir("sys/module/drivetemp")
	return root
}

func TestScanDiskHwmon(t *testing.T) {
	fakeDiskSysfs(t)
	var got []string
	for _, h := range scanDiskHwmon() {
		got = append(got, filepath.Base(h.dir)+"="+h.label())
	}
	want := "hwmon1=/dev/sda hwmon2=/dev/sdb hwmon3=/dev/nvme0n1 hwmon4=/dev/nvme1n1 hwmon5=/dev/sdd hwmon6=/dev/nvme1n1"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
}

func TestHwmonDiskSensorMax(t *testing.T) {
	fakeDiskSysfs(t)
	r, err := (&hwmonDiskSensor{}).Read(context.Background())
	// sdd (removable, 60 °C) is left out; sdc has no sensor and is skipped.
	if err != nil || r.Temp != 45 || r.Detail != "hottest /dev/sdb, 1 without sensor" || r.Partial != nil {
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
	r, err := (&hwmonDiskSensor{devices: []string{"/dev/sda", "/dev/sdz", "/dev/sdc"}}).Read(context.Background())
	if err != nil || r.Temp != 41 || r.Partial == nil ||
		!strings.Contains(r.Partial.Error(), "/dev/sdz: no such disk") ||
		!strings.Contains(r.Partial.Error(), "/dev/sdc: no drivetemp sensor") {
		t.Fatalf("got %+v, %v; want 41 with /dev/sdz and /dev/sdc partial", r, err)
	}
	// Only missing disks: no data at all.
	if _, err := (&hwmonDiskSensor{devices: []string{"/dev/sdz"}}).Read(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "/dev/sdz") {
		t.Fatalf("got %v, want an error naming /dev/sdz", err)
	}
	// A listed removable disk is read: the list is the user's choice.
	if r, err := (&hwmonDiskSensor{devices: []string{"/dev/sdd"}}).Read(context.Background()); err != nil || r.Temp != 60 {
		t.Fatalf("got %+v, %v; want 60 from listed /dev/sdd", r, err)
	}
}

// Both controllers of a multipath namespace are read: the hotter counts.
func TestHwmonDiskSensorMultipath(t *testing.T) {
	fakeDiskSysfs(t)
	r, err := (&hwmonDiskSensor{devices: []string{"/dev/nvme1n1"}}).Read(context.Background())
	if err != nil || r.Temp != 40 || r.Partial != nil {
		t.Fatalf("got %+v, %v; want 40 from the second controller", r, err)
	}
}

// A sysfs read that never returns is given up on after hwmonReadTimeout,
// and while it hangs the disk is not read again (no goroutine pile-up).
// Once it returns, the disk is read normally.
func TestHwmonDiskSensorHungRead(t *testing.T) {
	fakeDiskSysfs(t)
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var hungReads atomic.Int32
		s := &hwmonDiskSensor{read: func(path string) (float64, error) {
			if strings.Contains(path, "/hwmon2/") { // sdb, the hottest
				hungReads.Add(1)
				<-release
			}
			return readMilliC(path)
		}}
		start := time.Now()
		r, err := s.Read(context.Background())
		if err != nil || r.Temp != 41 || r.Partial == nil || !strings.Contains(r.Partial.Error(), "/dev/sdb: read timed out") {
			t.Fatalf("got %+v, %v; want 41 with /dev/sdb timed out", r, err)
		}
		if d := time.Since(start); d != hwmonReadTimeout {
			t.Fatalf("read took %s, want %s", d, hwmonReadTimeout)
		}
		time.Sleep(time.Minute)
		r, err = s.Read(context.Background())
		if err != nil || r.Partial == nil || !strings.Contains(r.Partial.Error(), "/dev/sdb: read hung since") ||
			!strings.Contains(r.Partial.Error(), "1m10s ago") {
			t.Fatalf("got %+v, %v; want /dev/sdb hung for 1m10s", r, err)
		}
		if time.Since(start) != hwmonReadTimeout+time.Minute {
			t.Fatal("a read with a known hung disk waited for it")
		}
		if n := hungReads.Load(); n != 1 {
			t.Fatalf("hung disk read %d times, want once", n)
		}
		close(release)
		synctest.Wait()
		if r, err := s.Read(context.Background()); err != nil || r.Temp != 45 || r.Partial != nil {
			t.Fatalf("after recovery: got %+v, %v; want 45", r, err)
		}
	})
}

// Without drivetemp no SATA disk has a sensor: each one is a failed read,
// not a disk without a sensor, or the NVMe would quietly decide alone.
func TestHwmonDiskSensorDrivetempNotLoaded(t *testing.T) {
	root := fakeDiskSysfs(t)
	for _, p := range []string{"sys/module/drivetemp", "sys/class/hwmon/hwmon1", "sys/class/hwmon/hwmon2", "sys/class/hwmon/hwmon5"} {
		os.RemoveAll(filepath.Join(root, p))
	}
	r, err := (&hwmonDiskSensor{}).Read(context.Background())
	if err != nil || r.Temp != 40 || r.Partial == nil || strings.Count(r.Partial.Error(), "drivetemp not loaded") != 3 {
		t.Fatalf("got %+v, %v; want 40 from nvme with sda, sdb, sdc failed", r, err)
	}
}

// An autodetected disk whose sensor goes away (drivetemp still loaded, say
// after an unbind) is a failed read, not a disk without a sensor: it may
// have been the hottest. sdc, which never had one, is still left out.
func TestHwmonDiskSensorLostSensor(t *testing.T) {
	root := fakeDiskSysfs(t)
	s := &hwmonDiskSensor{}
	if r, err := s.Read(context.Background()); err != nil || r.Temp != 45 || r.Partial != nil {
		t.Fatalf("got %+v, %v; want 45 from /dev/sdb", r, err)
	}
	os.RemoveAll(filepath.Join(root, "sys/class/hwmon/hwmon2"))
	r, err := s.Read(context.Background())
	if err != nil || r.Temp != 41 || r.Partial == nil || r.Partial.Error() != "/dev/sdb: drivetemp sensor gone" ||
		r.Detail != "hottest /dev/sda, 1 without sensor" {
		t.Fatalf("got %+v, %v; want 41 with only /dev/sdb failed", r, err)
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

	// drivetemp not loaded: the NVMe alone must not pass the startup check.
	root := sysRoot
	for _, p := range []string{"sys/module/drivetemp", "sys/class/hwmon/hwmon1", "sys/class/hwmon/hwmon2", "sys/class/hwmon/hwmon5"} {
		os.RemoveAll(filepath.Join(root, p))
	}
	if _, err := buildZones(c, &fakeRunner{}, newIPMI(nil, false), look); err == nil ||
		!strings.Contains(err.Error(), "modprobe drivetemp") || !strings.Contains(err.Error(), "modules-load.d") {
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
