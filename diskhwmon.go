package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- Disks: hwmon (drivetemp for SATA, nvme for NVMe) ----

// diskHwmonDrivers are the hwmon names that belong to one disk each:
// drivetemp has one per SATA disk, nvme one per controller.
var diskHwmonDrivers = []string{"drivetemp", "nvme"}

// hwmonReadTimeout bounds one temp1_input read. drivetemp sends the disk an
// ATA command, which a failing disk may never answer.
const hwmonReadTimeout = 10 * time.Second

// diskHwmon is one disk temperature sensor in sysfs.
type diskHwmon struct {
	dir   string   // /sys/class/hwmon/hwmonN
	disks []string // block devices it covers, e.g. /dev/sda; may be empty
}

// label names the hwmon in logs: its disks, or the hwmon itself.
func (h diskHwmon) label() string {
	if len(h.disks) == 0 {
		return filepath.Base(h.dir)
	}
	return strings.Join(h.disks, ",")
}

type hwmonDiskSensor struct {
	devices []string // empty = autodetect
	// read reads one temp1_input; nil = readMilliC. Tests make it block.
	read func(path string) (float64, error)

	mu   sync.Mutex
	busy map[string]time.Time // hwmon dirs with a read running, since when
	// monitored holds the disks that have had a sensor. One that loses it
	// is a failed read, not a disk without a sensor.
	monitored map[string]bool
}

func (s *hwmonDiskSensor) Name() string { return "disks" }

var (
	nvmeNamespace = regexp.MustCompile(`^nvme\d+n\d+$`)
	// With native NVMe multipath the controller lists its paths
	// (nvme<subsys>c<ctrl>n<ns>); the block device is nvme<subsys>n<ns>.
	nvmePath = regexp.MustCompile(`^(nvme\d+)c\d+(n\d+)$`)
)

// scanDiskHwmon lists the disk hwmon devices and the block devices each
// one covers.
func scanDiskHwmon() []diskHwmon {
	var out []diskHwmon
	for _, hd := range hwmonDirs(diskHwmonDrivers) {
		d := hd.dir
		var disks []string
		switch hd.name {
		case "drivetemp":
			// hwmonN/device is the SCSI device H:C:T:L; its block/ holds sdX.
			entries, _ := os.ReadDir(filepath.Join(d, "device", "block"))
			for _, e := range entries {
				disks = append(disks, "/dev/"+e.Name())
			}
		case "nvme":
			// hwmonN/device is the controller nvmeN; its namespaces are
			// subdirectories.
			entries, _ := os.ReadDir(filepath.Join(d, "device"))
			for _, e := range entries {
				n := e.Name()
				if nvmeNamespace.MatchString(n) {
					disks = append(disks, "/dev/"+n)
				} else if m := nvmePath.FindStringSubmatch(n); m != nil {
					disks = append(disks, "/dev/"+m[1]+m[2])
				}
			}
		}
		sort.Strings(disks)
		out = append(out, diskHwmon{dir: d, disks: disks})
	}
	return out
}

// diskName reduces a configured device to its kernel name, following
// symlinks such as /dev/disk/by-id/... when they resolve.
func diskName(dev string) string {
	if p, err := filepath.EvalSymlinks(dev); err == nil {
		dev = p
	}
	return "/dev/" + filepath.Base(dev)
}

func drivetempLoaded() bool {
	_, err := os.Stat(filepath.Join(sysRoot, "sys/module/drivetemp"))
	return err == nil
}

// diskPlan is what one read covers.
type diskPlan struct {
	hw       []diskHwmon // to read, each once
	covered  []string    // disks with a sensor in hw
	failed   []string    // disks that should have a sensor but don't
	noSensor []string    // autodetected SATA disks drivetemp found no sensor on
}

// planDisks matches the disks to read (devices, or the autodetected
// non-removable ones) with their hwmon. Without drivetemp loaded no SATA
// disk has one, so each counts as failed. With it loaded, an autodetected
// SATA disk without one has no sensor drivetemp can use (some older SSDs,
// SAS disks) and is left out, unless it is in monitored (it had one: its
// sensor went away); a listed one is always an error.
func planDisks(devices []string, monitored map[string]bool) diskPlan {
	// With NVMe multipath several controllers (hwmons) share one disk:
	// all of them are read, so the hottest counts.
	byDisk := map[string][]diskHwmon{}
	for _, h := range scanDiskHwmon() {
		for _, d := range h.disks {
			byDisk[d] = append(byDisk[d], h)
		}
	}
	loaded := drivetempLoaded()
	var p diskPlan
	names := detectDisks()
	labels := names // for errors: the device as configured
	if len(devices) > 0 {
		names, labels = nil, nil
		for _, dev := range devices {
			name := diskName(dev)
			if _, err := os.Stat(filepath.Join(sysRoot, "sys/block", filepath.Base(name))); err != nil {
				p.failed = append(p.failed, dev+": no such disk")
				continue
			}
			names, labels = append(names, name), append(labels, dev)
		}
	}
	picked := map[string]bool{}
	for i, name := range names {
		if hs := byDisk[name]; len(hs) > 0 {
			p.covered = append(p.covered, name)
			for _, h := range hs {
				if !picked[h.dir] {
					picked[h.dir] = true
					p.hw = append(p.hw, h)
				}
			}
			continue
		}
		label := labels[i]
		isSD := strings.HasPrefix(filepath.Base(name), "sd")
		switch {
		case isSD && !loaded:
			p.failed = append(p.failed, label+": drivetemp not loaded (modprobe drivetemp)")
		case isSD && len(devices) == 0 && monitored[name]:
			p.failed = append(p.failed, label+": drivetemp sensor gone")
		case isSD && len(devices) == 0:
			p.noSensor = append(p.noSensor, name)
		case isSD:
			p.failed = append(p.failed, label+": no drivetemp sensor (SAS, no SCT/SMART temperature, or an HBA without ATA passthrough)")
		default:
			p.failed = append(p.failed, label+": no nvme hwmon")
		}
	}
	return p
}

func (s *hwmonDiskSensor) Read(ctx context.Context) (Reading, error) {
	s.mu.Lock()
	plan := planDisks(s.devices, s.monitored)
	if s.monitored == nil {
		s.monitored = map[string]bool{}
	}
	for _, d := range plan.covered {
		s.monitored[d] = true
	}
	s.mu.Unlock()
	hw, failed := plan.hw, plan.failed
	if len(hw) == 0 && len(failed) == 0 {
		return Reading{}, errors.New("no disk with a temperature sensor")
	}

	type result struct {
		temp float64
		err  error
	}
	read := s.read
	if read == nil {
		read = readMilliC
	}
	res := make([]chan result, len(hw))
	for i, h := range hw {
		res[i] = make(chan result, 1)
		s.mu.Lock()
		if s.busy == nil {
			s.busy = map[string]time.Time{}
		}
		since, hung := s.busy[h.dir]
		if !hung {
			s.busy[h.dir] = time.Now()
		}
		s.mu.Unlock()
		if hung {
			// A sysfs read can't be cancelled. Don't pile up goroutines
			// behind one that never returns.
			res[i] <- result{err: fmt.Errorf("read hung since %s (%s ago); the disk isn't answering",
				since.Format("15:04:05"), time.Since(since).Round(time.Second))}
			continue
		}
		go func() {
			t, err := read(filepath.Join(h.dir, "temp1_input"))
			s.mu.Lock()
			delete(s.busy, h.dir)
			s.mu.Unlock()
			res[i] <- result{t, err}
		}()
	}

	timer := time.NewTimer(hwmonReadTimeout)
	defer timer.Stop()
	expired := false
	timedOut := result{err: fmt.Errorf("read timed out after %s", hwmonReadTimeout)}
	best, found, hottest := 0.0, false, ""
	for i, h := range hw {
		var r result
		if expired { // the rest get only what is already there
			select {
			case r = <-res[i]:
			default:
				r = timedOut
			}
		} else {
			select {
			case r = <-res[i]:
			case <-timer.C:
				expired, r = true, timedOut
			case <-ctx.Done():
				return Reading{}, ctx.Err()
			}
		}
		switch {
		case r.err != nil:
			failed = append(failed, fmt.Sprintf("%s: %v", h.label(), r.err))
		case !found || r.temp > best:
			best, found, hottest = r.temp, true, h.label()
		}
	}
	var partial error
	if len(failed) > 0 {
		partial = errors.New(strings.Join(failed, "; "))
	}
	if !found {
		if partial != nil {
			return Reading{}, partial
		}
		return Reading{}, errors.New("no disk temperatures")
	}
	detail := "hottest " + hottest
	if n := len(plan.noSensor); n > 0 {
		detail += fmt.Sprintf(", %d without sensor", n)
	}
	return Reading{Temp: best, Detail: detail, Partial: partial}, nil
}

type hwmonDir struct{ dir, name string }

// hwmonDirs lists the hwmon devices whose driver name is in names.
func hwmonDirs(names []string) []hwmonDir {
	dirs, _ := filepath.Glob(filepath.Join(sysRoot, "sys/class/hwmon/hwmon*"))
	var out []hwmonDir
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "name"))
		if name := strings.TrimSpace(string(b)); err == nil && contains(names, name) {
			out = append(out, hwmonDir{d, name})
		}
	}
	return out
}

// readMilliC reads a hwmon temperature file (millidegrees) in °C.
func readMilliC(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	milli, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("bad temperature %q", strings.TrimSpace(string(b)))
	}
	return float64(milli) / 1000, nil
}
