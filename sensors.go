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
	"sync"
	"sync/atomic"
	"time"
)

// Reading is one sample from a source.
// Idle means there is nothing to measure right now (e.g. all disks asleep):
// the source makes no demand on the fans.
type Reading struct {
	Temp   float64
	Idle   bool
	Detail string // for logs, e.g. "hottest /dev/sdc"
	// Partial is set when some parts couldn't be read (e.g. one disk of
	// several): Temp covers only the rest, so it is a lower bound.
	Partial error
}

// Sensor reads a source. As usual in Go, a non-nil error means no data:
// a reading that covers only some parts comes back with Partial set.
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

// maxParallelSmartctl caps concurrent smartctl processes on big boxes while
// still keeping one hung disk from holding up the rest.
const maxParallelSmartctl = 8

type diskSensor struct {
	run     Runner
	bin     string
	devices []string     // empty = autodetect each read
	count   atomic.Int64 // disks in the latest (or running) Read
}

// maxReadTime is the longest one Read can legitimately take, plus a
// margin: one command timeout per batch of maxParallelSmartctl disks.
// Based on the latest detection, so disks that appear later count.
func (s *diskSensor) maxReadTime() time.Duration {
	n := max(int64(len(s.devices)), s.count.Load())
	return readBudget(max(1, (n+maxParallelSmartctl-1)/maxParallelSmartctl))
}

// readTimeout bounds each sensor command (smartctl, nvidia-smi).
const readTimeout = 20 * time.Second

// ipmiTimeout bounds each BMC command (ipmitool, or a /dev/ipmi0 request). A raw command takes well under
// a second. Writes run in each zone's writer goroutine, but both zones and
// the exit-mode restore share one IPMI lock, so a hung BMC must give it up
// sooner than a slow disk read would.
const ipmiTimeout = 5 * time.Second

// readBudget is how long a read made of n rounds of commands may take:
// one readTimeout each plus its output wait (cmdWaitDelay), plus a margin.
func readBudget(n int64) time.Duration {
	return time.Duration(n)*(readTimeout+cmdWaitDelay) + 5*time.Second
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
	s.count.Store(int64(len(devs)))
	var (
		best, found = 0.0, false
		hottest     string
		asleep      int
		failed      []string
	)
	// Up to maxParallelSmartctl workers read the disks, so one hung disk
	// costs a single smartctl timeout rather than one per disk queued
	// behind it. Disks beyond the workers wait in line: maxReadTime counts
	// those batches.
	type result struct {
		temp     float64
		sleeping bool
		err      error
	}
	res := make([]result, len(devs))
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(len(devs), maxParallelSmartctl) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				t, sleeping, err := s.readOne(ctx, devs[i])
				res[i] = result{t, sleeping, err}
			}
		}()
	}
	for i := range devs {
		next <- i
	}
	close(next)
	wg.Wait()
	for i, r := range res {
		dev := devs[i]
		switch {
		case r.err != nil:
			failed = append(failed, fmt.Sprintf("%s: %v", dev, r.err))
		case r.sleeping:
			asleep++
		case !found || r.temp > best:
			best, found, hottest = r.temp, true, dev
		}
	}
	var partial error
	if len(failed) > 0 {
		partial = errors.New(strings.Join(failed, "; "))
	}
	if !found {
		switch {
		case partial != nil:
			return Reading{}, partial
		case asleep > 0:
			return Reading{Idle: true, Detail: fmt.Sprintf("%d disks asleep", asleep)}, nil
		default:
			return Reading{}, errors.New("no disk temperatures")
		}
	}
	detail := "hottest " + hottest
	if asleep > 0 {
		detail += fmt.Sprintf(", %d asleep", asleep)
	}
	// A partial failure still returns the readings we have; the controller
	// counts it as a failure, so it fails safe if it persists.
	return Reading{Temp: best, Detail: detail, Partial: partial}, nil
}

func (s *diskSensor) readOne(ctx context.Context, dev string) (temp float64, asleep bool, err error) {
	// smartctl exits non-zero for many harmless reasons (bitmask), so parse
	// the JSON regardless of the exit status, but never output that was cut
	// short: a fragment can parse as a temperature or a standby message.
	out, runErr := s.run.Run(ctx, s.bin, "-n", "standby", "-A", "-j", dev)
	if errors.Is(runErr, errCutShort) {
		return 0, false, runErr
	}
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
