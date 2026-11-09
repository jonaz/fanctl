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
	devices []string // empty = every disk hwmon

	mu   sync.Mutex
	busy map[string]bool // hwmon dirs with a read still running
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
	dirs, _ := filepath.Glob(filepath.Join(sysRoot, "sys/class/hwmon/hwmon*"))
	var out []diskHwmon
	for _, d := range dirs {
		name, err := os.ReadFile(filepath.Join(d, "name"))
		if err != nil {
			continue
		}
		var disks []string
		switch strings.TrimSpace(string(name)) {
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
		default:
			continue
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

func (s *hwmonDiskSensor) Read(ctx context.Context) (Reading, error) {
	all := scanDiskHwmon()
	if len(all) == 0 {
		return Reading{}, errors.New("no drivetemp/nvme hwmon devices")
	}
	var (
		hw     []diskHwmon
		failed []string
	)
	if len(s.devices) == 0 {
		hw = all
	} else {
		// Each configured disk must have a hwmon; a hwmon is read once even
		// if several of its disks (NVMe namespaces) are listed.
		picked := map[string]bool{}
		for _, dev := range s.devices {
			name, found := diskName(dev), false
			for _, h := range all {
				if contains(h.disks, name) {
					found = true
					if !picked[h.dir] {
						picked[h.dir] = true
						hw = append(hw, h)
					}
					break
				}
			}
			if !found {
				failed = append(failed, dev+": no hwmon (modprobe drivetemp?)")
			}
		}
	}

	type result struct {
		temp float64
		err  error
	}
	res := make([]chan result, len(hw))
	for i, h := range hw {
		res[i] = make(chan result, 1)
		s.mu.Lock()
		if s.busy == nil {
			s.busy = map[string]bool{}
		}
		hung := s.busy[h.dir]
		s.busy[h.dir] = true
		s.mu.Unlock()
		if hung {
			// A sysfs read can't be cancelled. Don't pile up goroutines
			// behind one that never returns.
			res[i] <- result{err: errors.New("previous read still hung")}
			continue
		}
		go func() {
			t, err := readMilliC(filepath.Join(h.dir, "temp1_input"))
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
	return Reading{Temp: best, Detail: "hottest " + hottest, Partial: partial}, nil
}

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
