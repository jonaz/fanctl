package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Reading is one sample from a source.
// Idle means there is nothing to measure right now (e.g. all disks asleep):
// the source makes no demand on the fans.
type Reading struct {
	Temp   float64
	Idle   bool
	Detail string // for logs, e.g. "hottest /dev/sdc"
}

type Sensor interface {
	Read(ctx context.Context) (Reading, error)
	Name() string
}

// sysRoot is "/" in production; tests point it at a fake tree.
var sysRoot = "/"

// ---- CPU: hwmon (coretemp / k10temp) ----

type cpuSensor struct{ drivers []string }

func (s *cpuSensor) Name() string { return "cpu" }

func (s *cpuSensor) Read(_ context.Context) (Reading, error) {
	dirs, _ := filepath.Glob(filepath.Join(sysRoot, "sys/class/hwmon/hwmon*"))
	best, found := 0.0, false
	for _, d := range dirs {
		name, err := os.ReadFile(filepath.Join(d, "name"))
		if err != nil || !contains(s.drivers, strings.TrimSpace(string(name))) {
			continue
		}
		inputs, _ := filepath.Glob(filepath.Join(d, "temp*_input"))
		for _, in := range inputs {
			b, err := os.ReadFile(in)
			if err != nil {
				continue
			}
			milli, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				continue
			}
			if t := float64(milli) / 1000; !found || t > best {
				best, found = t, true
			}
		}
	}
	if !found {
		return Reading{}, fmt.Errorf("no hwmon temperatures for %v", s.drivers)
	}
	return Reading{Temp: best}, nil
}

// ---- Disks: smartctl, without waking sleeping drives ----

type diskSensor struct {
	run     Runner
	bin     string
	devices []string // empty = autodetect each read
}

func (s *diskSensor) Name() string { return "disks" }

type smartJSON struct {
	Temperature *struct {
		Current *float64 `json:"current"`
	} `json:"temperature"`
	Smartctl struct {
		Messages []struct {
			String string `json:"string"`
		} `json:"messages"`
	} `json:"smartctl"`
}

func (s *diskSensor) Read(ctx context.Context) (Reading, error) {
	devs := s.devices
	if len(devs) == 0 {
		devs = detectDisks()
		if len(devs) == 0 {
			return Reading{}, errors.New("no disks found")
		}
	}
	var (
		best, found = 0.0, false
		hottest     string
		asleep      int
		failed      []string
	)
	for _, dev := range devs {
		t, sleeping, err := s.readOne(ctx, dev)
		switch {
		case err != nil:
			failed = append(failed, fmt.Sprintf("%s: %v", dev, err))
		case sleeping:
			asleep++
		case !found || t > best:
			best, found, hottest = t, true, dev
		}
	}
	var err error
	if len(failed) > 0 {
		err = errors.New(strings.Join(failed, "; "))
	}
	if !found {
		if asleep > 0 && len(failed) == 0 {
			return Reading{Idle: true, Detail: fmt.Sprintf("%d disks asleep", asleep)}, nil
		}
		if err == nil {
			err = errors.New("no disk temperatures")
		}
		return Reading{}, err
	}
	detail := "hottest " + hottest
	if asleep > 0 {
		detail += fmt.Sprintf(", %d asleep", asleep)
	}
	// A partial failure still returns the readings we have, plus the error,
	// so the controller can count failures and fail safe if they persist.
	return Reading{Temp: best, Detail: detail}, err
}

func (s *diskSensor) readOne(ctx context.Context, dev string) (temp float64, asleep bool, err error) {
	// smartctl exits non-zero for many harmless reasons (bitmask), so parse
	// the JSON regardless of the exit status.
	out, runErr := s.run.Run(ctx, s.bin, "-n", "standby", "-A", "-j", dev)
	var j smartJSON
	if jerr := json.Unmarshal(out, &j); jerr != nil {
		if runErr != nil {
			return 0, false, runErr
		}
		return 0, false, fmt.Errorf("bad smartctl output: %w", jerr)
	}
	if j.Temperature != nil && j.Temperature.Current != nil {
		return *j.Temperature.Current, false, nil
	}
	for _, m := range j.Smartctl.Messages {
		up := strings.ToUpper(m.String)
		if strings.Contains(up, "STANDBY") || strings.Contains(up, "SLEEP") {
			return 0, true, nil
		}
	}
	if runErr != nil {
		return 0, false, runErr
	}
	return 0, false, errors.New("no temperature in smartctl output")
}

// detectDisks lists non-removable SATA/SAS (sd*) and NVMe namespace devices.
func detectDisks() []string {
	entries, _ := os.ReadDir(filepath.Join(sysRoot, "sys/block"))
	var devs []string
	for _, e := range entries {
		n := e.Name()
		isSD := strings.HasPrefix(n, "sd")
		isNVMe := strings.HasPrefix(n, "nvme") // /sys/block lists namespaces only
		if !isSD && !isNVMe {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(sysRoot, "sys/block", n, "removable")); err == nil &&
			strings.TrimSpace(string(b)) == "1" {
			continue
		}
		devs = append(devs, "/dev/"+n)
	}
	sort.Strings(devs)
	return devs
}

// ---- GPU: nvidia-smi ----

type gpuSensor struct {
	run Runner
	bin string
}

func (s *gpuSensor) Name() string { return "gpu" }

func (s *gpuSensor) Read(ctx context.Context) (Reading, error) {
	out, err := s.run.Run(ctx, s.bin, "--query-gpu=temperature.gpu", "--format=csv,noheader,nounits")
	if err != nil {
		return Reading{}, fmt.Errorf("nvidia-smi: %w", err)
	}
	best, found := 0.0, false
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		t, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
		if err != nil {
			continue
		}
		if !found || t > best {
			best, found = t, true
		}
	}
	if !found {
		return Reading{}, fmt.Errorf("no GPU temperature in %q", strings.TrimSpace(string(out)))
	}
	return Reading{Temp: best}, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
