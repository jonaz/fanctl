package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// Tests that involve time or goroutines run in a synctest bubble: the
// production code's time.Now, timers and sleeps run on a virtual clock that
// only moves when every goroutine in the bubble is blocked, so they are
// exact and instant.

// fakeRunner returns canned output per command line and records calls.
// Safe for concurrent use; the maps must not be changed during a Run.
type fakeRunner struct {
	out   map[string]string
	err   map[string]error
	mu    sync.Mutex
	calls []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	return []byte(f.out[key]), f.err[key]
}

// last is the duty byte of the latest command, "" if none.
func (f *fakeRunner) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	p := strings.Fields(f.calls[len(f.calls)-1])
	return p[len(p)-1]
}

func (f *fakeRunner) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func duty(n int) string { return fmt.Sprintf("0x%02x", n) }

func setDutyCmd(n int) string { return "ipmitool raw 0x30 0x70 0x66 0x01 0x01 " + duty(n) }

// timeline returns a function that sleeps until d after now: in a bubble,
// "at(d)" moves the virtual clock to that point.
func timeline() func(time.Duration) {
	t0 := time.Now()
	return func(d time.Duration) { time.Sleep(time.Until(t0.Add(d))) }
}

// ---- config, curve, sensors ----

func TestInterpolate(t *testing.T) {
	c := [][2]float64{{30, 20}, {40, 60}, {50, 100}}
	cases := map[float64]float64{10: 20, 30: 20, 35: 40, 40: 60, 45: 80, 50: 100, 90: 100}
	for in, want := range cases {
		if got := Interpolate(c, in); got != want {
			t.Errorf("Interpolate(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestExampleConfigLoads(t *testing.T) {
	c, err := LoadConfig("fanctl.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Zones) != 2 || c.Zones[1].Sources[0].Interval.Duration != time.Minute {
		t.Fatalf("unexpected config: %+v", c.Zones)
	}
}

func TestValidateRejects(t *testing.T) {
	ok := [][2]float64{{1, 1}, {2, 2}}
	zone := func(name string, ipmi int, typ string, curve [][2]float64) Zone {
		return Zone{Name: name, IpmiZone: ipmi, Sources: []Source{{Type: typ, Curve: curve}}}
	}
	bad := []struct {
		want  string
		zones []Zone
	}{
		{"no zones", nil},
		{"at least 2 points", []Zone{zone("a", 0, "cpu", [][2]float64{{40, 20}})}},
		{"must increase", []Zone{zone("a", 0, "cpu", [][2]float64{{40, 20}, {30, 50}})}},
		{"unknown source type", []Zone{zone("a", 0, "fan", ok)}},
		{"both use ipmi_zone", []Zone{zone("a", 0, "cpu", ok), zone("b", 0, "cpu", ok)}},
		{"two zones named \"a\"", []Zone{zone("a", 0, "cpu", ok), zone("a", 1, "cpu", ok)}},
		{"two zones named \"\"", []Zone{zone("", 0, "cpu", ok), zone("", 1, "cpu", ok)}},
		{"fail_after", []Zone{{Name: "a", Sources: []Source{{Type: "cpu", Curve: ok, FailAfter: -1}}}}},
		{"startup_duty must be between min_duty", []Zone{{Name: "a", MinDuty: 30, StartupDuty: ptr(20), Sources: []Source{{Type: "cpu", Curve: ok}}}}},
		{"startup_duty must be between min_duty", []Zone{{Name: "a", MaxDuty: 60, StartupDuty: ptr(80), Sources: []Source{{Type: "cpu", Curve: ok}}}}},
		{"interval must be at least 1s", []Zone{{Name: "a", Interval: Duration{time.Millisecond}, Sources: []Source{{Type: "cpu", Curve: ok}}}}},
		{"source cpu: interval must be at least 1s", []Zone{{Name: "a", Sources: []Source{{Type: "cpu", Curve: ok, Interval: Duration{500 * time.Millisecond}}}}}},
	}
	for i, b := range bad {
		// Defaults first, as LoadConfig does, so each case reaches its check.
		c := Config{Zones: b.zones}
		c.applyDefaults()
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("config %d: got %v, want error containing %q", i, err, b.want)
		}
	}
	good := Config{Zones: []Zone{zone("a", 0, "cpu", ok), zone("b", 1, "gpu", ok)}}
	good.Zones[1].MinDuty = 80 // default startup_duty follows min_duty up
	good.applyDefaults()
	if err := good.Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
	if got := *good.Zones[1].StartupDuty; got != 80 {
		t.Errorf("default startup_duty with min_duty 80 = %d, want 80", got)
	}
	capped := Config{Zones: []Zone{zone("a", 0, "cpu", ok)}}
	capped.Zones[0].MaxDuty = 60 // default startup_duty (70) is capped
	capped.applyDefaults()
	if err := capped.Validate(); err != nil || *capped.Zones[0].StartupDuty != 60 {
		t.Errorf("max_duty 60: startup_duty %d, err %v; want 60, nil", *capped.Zones[0].StartupDuty, err)
	}
}

func TestReapplyMustBeAtLeastOneSecond(t *testing.T) {
	c := Config{Reapply: Duration{50 * time.Millisecond}, Zones: []Zone{{Name: "a", Sources: []Source{{Type: "cpu", Curve: [][2]float64{{1, 1}, {2, 2}}}}}}}
	c.applyDefaults()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "reapply must be at least") {
		t.Fatalf("got %v, want a reapply error", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestCPUSensor(t *testing.T) {
	root := t.TempDir()
	sysRoot = root
	defer func() { sysRoot = "/" }()
	write := func(p, s string) {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(s), 0o644)
	}
	write("sys/class/hwmon/hwmon0/name", "acpitz\n")
	write("sys/class/hwmon/hwmon0/temp1_input", "90000\n") // not the CPU, ignored
	write("sys/class/hwmon/hwmon1/name", "coretemp\n")
	write("sys/class/hwmon/hwmon1/temp1_input", "52000\n")
	write("sys/class/hwmon/hwmon1/temp2_input", "48000\n")

	r, err := (&cpuSensor{drivers: []string{"coretemp"}}).Read(context.Background())
	if err != nil || r.Temp != 52 {
		t.Fatalf("got %+v, %v; want 52", r, err)
	}
	if _, err := (&cpuSensor{drivers: []string{"k10temp"}}).Read(context.Background()); err == nil {
		t.Fatal("expected error when driver missing")
	}
}

const smartOK = `{"smartctl":{"exit_status":0},"temperature":{"current":%d}}`
const smartSleep = `{"smartctl":{"exit_status":2,"messages":[{"string":"Device is in STANDBY mode, exit(2)","severity":"information"}]}}`

func smartKey(dev string) string { return "smartctl -n standby -A -j " + dev }

func TestDiskSensor(t *testing.T) {
	f := &fakeRunner{out: map[string]string{
		smartKey("/dev/sda"): fmt.Sprintf(smartOK, 38),
		smartKey("/dev/sdb"): fmt.Sprintf(smartOK, 44),
		smartKey("/dev/sdc"): smartSleep,
	}, err: map[string]error{
		// smartctl's exit bitmask is often non-zero with valid data
		smartKey("/dev/sdb"): errors.New("exit status 64"),
		smartKey("/dev/sdc"): errors.New("exit status 2"),
	}}
	s := &diskSensor{run: f, bin: "smartctl", devices: []string{"/dev/sda", "/dev/sdb", "/dev/sdc"}}
	r, err := s.Read(context.Background())
	if err != nil || r.Partial != nil || r.Temp != 44 || !strings.Contains(r.Detail, "/dev/sdb") || !strings.Contains(r.Detail, "1 asleep") {
		t.Fatalf("got %+v, %v", r, err)
	}

	// All asleep: idle, no error, nothing woken.
	s.devices = []string{"/dev/sdc"}
	r, err = s.Read(context.Background())
	if err != nil || !r.Idle {
		t.Fatalf("all asleep: got %+v, %v", r, err)
	}

	// One disk unreadable: keep the others' max, flagged partial.
	f.out[smartKey("/dev/sdx")] = ""
	f.err[smartKey("/dev/sdx")] = errors.New("exit status 2")
	s.devices = []string{"/dev/sda", "/dev/sdx"}
	r, err = s.Read(context.Background())
	if err != nil || r.Partial == nil || !strings.Contains(r.Partial.Error(), "/dev/sdx") || r.Temp != 38 {
		t.Fatalf("partial failure: got %+v, %v", r, err)
	}

	// Output cut short is not trusted, even when it parses.
	f.out[smartKey("/dev/sdy")] = fmt.Sprintf(smartOK, 90)
	f.err[smartKey("/dev/sdy")] = fmt.Errorf("%w: %w", errCutShort, exec.ErrWaitDelay)
	f.out[smartKey("/dev/sdz")] = smartSleep
	f.err[smartKey("/dev/sdz")] = fmt.Errorf("%w: killed: %w", errCutShort, context.DeadlineExceeded)
	s.devices = []string{"/dev/sda", "/dev/sdy", "/dev/sdz"}
	r, err = s.Read(context.Background())
	if err != nil || r.Partial == nil || r.Temp != 38 || strings.Contains(r.Detail, "asleep") {
		t.Fatalf("cut-short output: got %+v, %v; want sda only, partial", r, err)
	}

	// Asleep plus unreadable: no data, and not idle (the unreadable one may be hot).
	s.devices = []string{"/dev/sdc", "/dev/sdx"}
	if r, err = s.Read(context.Background()); err == nil || r.Idle {
		t.Fatalf("asleep + unreadable: got %+v, %v; want an error", r, err)
	}
}

func TestGPUSensor(t *testing.T) {
	f := &fakeRunner{out: map[string]string{
		"nvidia-smi --query-gpu=temperature.gpu --format=csv,noheader,nounits": "61\n",
	}}
	r, err := (&gpuSensor{run: f, bin: "nvidia-smi"}).Read(context.Background())
	if err != nil || r.Temp != 61 {
		t.Fatalf("got %+v, %v", r, err)
	}
}

// gone reports whether pid has exited (and been reaped, or is a zombie).
func gone(pid string) bool {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return true
	}
	f := strings.Fields(string(b))
	return len(f) > 2 && f[2] == "Z"
}

// waitGone waits up to a second for pid to exit.
func waitGone(t *testing.T, pid string) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if gone(pid) {
			return
		}
	}
	t.Fatalf("child %s outlived its command", pid)
}

func TestRunnerKillsLingeringChild(t *testing.T) {
	r := execRunner{timeout: 5 * time.Second, waitDelay: time.Second}
	// The command exits at once, leaving a child that holds stdout. The
	// child is killed as the command exits, so its output is complete and
	// the exit status is the command's own.
	for _, c := range []struct {
		exit    string
		wantErr func(error) bool
	}{
		{"exit 0", func(err error) bool { return err == nil }},
		{"exit 1", func(err error) bool { var e *exec.ExitError; return errors.As(err, &e) }}, // like smartctl's bitmask
	} {
		start := time.Now()
		out, err := r.Run(context.Background(), "sh", "-c", "sleep 300 & echo $!; "+c.exit)
		if !c.wantErr(err) || errors.Is(err, errCutShort) {
			t.Fatalf("%s: got %v (%T)", c.exit, err, err)
		}
		if took := time.Since(start); took > 500*time.Millisecond {
			t.Fatalf("%s: returned after %s, want at once", c.exit, took)
		}
		waitGone(t, strings.TrimSpace(string(out)))
	}
}

func TestRunnerCutShortOutput(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid")
	}
	r := execRunner{timeout: 5 * time.Second, waitDelay: 50 * time.Millisecond}
	// A child out of the group's reach holds stdout: the output can't be
	// known to be complete, even after a non-zero exit.
	out, err := r.Run(context.Background(), "sh", "-c", "setsid sleep 300 & echo $!; sleep 0.2; exit 1")
	pid := strings.TrimSpace(string(out))
	if n, perr := strconv.Atoi(pid); perr == nil {
		defer syscall.Kill(n, syscall.SIGKILL)
	}
	if !errors.Is(err, errCutShort) || !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("got %v, want a cut-short error", err)
	}
}

func TestRunnerKillsGroupOnTimeout(t *testing.T) {
	r := execRunner{timeout: 100 * time.Millisecond, waitDelay: 50 * time.Millisecond}
	start := time.Now()
	out, err := r.Run(context.Background(), "sh", "-c", "sleep 300 & echo $!; sleep 300")
	if !errors.Is(err, errCutShort) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a command past its timeout: got %v, want a cut-short timeout", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("timed-out command returned after %s", took)
	}
	waitGone(t, strings.TrimSpace(string(out)))
}

// barrierRunner only answers once n calls are running at the same time.
type barrierRunner struct {
	n       int
	mu      sync.Mutex
	running int
	all     chan struct{}
}

func (b *barrierRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	b.mu.Lock()
	if b.running++; b.running == b.n {
		close(b.all)
	}
	b.mu.Unlock()
	select {
	case <-b.all:
		return []byte(fmt.Sprintf(smartOK, 30+len(args[len(args)-1]))), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestDiskSensorReadsInParallel(t *testing.T) {
	devs := []string{"/dev/sda", "/dev/sdb", "/dev/nvme0n1"}
	b := &barrierRunner{n: len(devs), all: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := (&diskSensor{run: b, bin: "smartctl", devices: devs}).Read(ctx)
	// 30 + len("/dev/nvme0n1") = 42: the hottest, last in order.
	if err != nil || r.Temp != 42 || !strings.Contains(r.Detail, "/dev/nvme0n1") {
		t.Fatalf("disks not read in parallel: %+v, %v", r, err)
	}
}

func TestStuckLimitCoversSmartctlBatches(t *testing.T) {
	devs := make([]string, 12)
	for i := range devs {
		devs[i] = fmt.Sprintf("/dev/sd%c", 'a'+i)
	}
	if got, want := (&diskSensor{devices: devs}).maxReadTime(), readBudget(2); got != want {
		t.Fatalf("12 disks: %s, want %s", got, want)
	}

	// Autodetect: the limit follows the disks found by the latest read.
	root := t.TempDir()
	sysRoot = root
	defer func() { sysRoot = "/" }()
	f := &fakeRunner{out: map[string]string{}}
	for _, d := range devs[:4] {
		os.MkdirAll(filepath.Join(root, "sys/block", filepath.Base(d)), 0o755)
		f.out[smartKey(d)] = fmt.Sprintf(smartOK, 35)
	}
	s := &diskSensor{run: f, bin: "smartctl"}
	s.Read(context.Background())
	if got, want := s.maxReadTime(), readBudget(1); got != want {
		t.Fatalf("4 disks: %s, want %s", got, want)
	}
	for _, d := range devs[4:] { // hotplugged after start
		os.MkdirAll(filepath.Join(root, "sys/block", filepath.Base(d)), 0o755)
		f.out[smartKey(d)] = fmt.Sprintf(smartOK, 35)
	}
	s.Read(context.Background())
	if got, want := s.maxReadTime(), readBudget(2); got != want {
		t.Fatalf("12 disks after hotplug: %s, want %s", got, want)
	}
}

func TestReadBudgetCoversWorstCase(t *testing.T) {
	// Every batch may run to its timeout and then wait out its pipes.
	for n := int64(1); n <= 20; n++ {
		if worst := time.Duration(n) * (readTimeout + cmdWaitDelay); readBudget(n) <= worst {
			t.Fatalf("%d batches: budget %s, worst case %s", n, readBudget(n), worst)
		}
	}
}

func TestStuckLimitDefaultsToReadBudget(t *testing.T) {
	s := newSource(&stubSensor{name: "gpu"}, time.Second, 0)
	if got := s.stuckLimit(); got != readBudget(1) || got <= readTimeout {
		t.Fatalf("stuck limit %s: want one command timeout plus margin", got)
	}
}

func TestBuildZonesOptionalGPU(t *testing.T) {
	c, err := LoadConfig("fanctl.example.json")
	if err != nil {
		t.Fatal(err)
	}
	c.Zones[1].Sources[0].Devices = []string{"/dev/sda"}
	look := func(bin string) (string, error) {
		if bin == "nvidia-smi" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + bin, nil
	}
	zones, err := buildZones(c, &fakeRunner{}, newIPMI(nil, false), look)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range zones[1].sources {
		names = append(names, s.sensor.Name())
	}
	if got := strings.Join(names, ","); got != "disks,cpu" {
		t.Fatalf("optional gpu should be skipped, got sources %s", got)
	}
}

// ---- IPMI ----

func TestSetModeCommand(t *testing.T) {
	f := &fakeRunner{}
	if err := (newIPMI(ipmitool{run: f, bin: "ipmitool"}, false)).SetMode(context.Background(), fanModes["full"]); err != nil {
		t.Fatal(err)
	}
	if f.calls[0] != "ipmitool raw 0x30 0x45 0x01 0x01" {
		t.Fatalf("got %q", f.calls[0])
	}
}

// A dry run logs the same raw bytes whichever backend, and sends nothing.
func TestDryRunLogsRawBytes(t *testing.T) {
	var buf strings.Builder
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	f := &fakeRunner{}
	for _, cfg := range []Config{{IPMI: "ipmitool", Ipmitool: "ipmitool"}, {IPMI: "device", IpmiDevice: t.TempDir() + "/ipmi0"}} {
		buf.Reset()
		ipmi, closeBMC, err := newBMC(&cfg, true)
		if err != nil {
			t.Fatalf("%s: %v", cfg.IPMI, err)
		}
		if cfg.IPMI == "ipmitool" {
			ipmi.t = ipmitool{run: f, bin: "ipmitool"}
		}
		if err := ipmi.SetDuty(context.Background(), 1, 83); err != nil {
			t.Fatal(err)
		}
		closeBMC()
		if !strings.Contains(buf.String(), "dry-run: raw 0x30 0x70 0x66 0x01 0x01 0x53\n") {
			t.Errorf("%s: logged %q", cfg.IPMI, buf.String())
		}
	}
	if f.n() != 0 {
		t.Fatalf("dry run ran %v", f.calls)
	}
	if !strings.Contains(buf.String(), "warning:") {
		t.Errorf("dry run with no device didn't warn: %q", buf.String())
	}
}

func TestIPMIConfig(t *testing.T) {
	ok := [][2]float64{{1, 1}, {2, 2}}
	c := Config{Zones: []Zone{{Name: "a", Sources: []Source{{Type: "cpu", Curve: ok}}}}}
	c.applyDefaults()
	if err := c.Validate(); err != nil || c.IPMI != "device" || c.IpmiDevice != "/dev/ipmi0" {
		t.Fatalf("defaults: ipmi %q, ipmi_device %q, err %v", c.IPMI, c.IpmiDevice, err)
	}
	c.IPMI = "freeipmi"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "want device or ipmitool") {
		t.Fatalf("ipmi freeipmi: got %v", err)
	}
}

// hungBMC blocks every ipmitool call until released or cancelled.
type hungBMC struct {
	calls   atomic.Int32
	release chan struct{}
}

func (h *hungBMC) Run(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	h.calls.Add(1)
	select {
	case <-h.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestIPMIWaitForLockHonoursContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bmc := &hungBMC{release: make(chan struct{})}
		defer close(bmc.release)
		ipmi := newIPMI(ipmitool{run: bmc, bin: "ipmitool"}, false)
		go ipmi.SetDuty(context.Background(), 1, 50) // stuck in the kernel, holding the lock
		synctest.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := ipmi.SetMode(ctx, fanModes["optimal"]); err == nil {
			t.Fatal("SetMode succeeded while another ipmitool held the lock")
		}
		if waited := time.Since(start); waited != 50*time.Millisecond {
			t.Fatalf("SetMode waited %s for the lock, want its 50ms context", waited)
		}
	})
}

func TestIPMILockWaitIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bmc := &hungBMC{release: make(chan struct{})}
		defer close(bmc.release)
		ipmi := newIPMI(ipmitool{run: bmc, bin: "ipmitool"}, false)
		go ipmi.SetDuty(context.Background(), 0, 50) // zone A: stuck in the kernel
		synctest.Wait()
		start := time.Now()
		if err := ipmi.SetDuty(context.Background(), 1, 100); err == nil { // zone B: no deadline of its own
			t.Fatal("write went through while another ipmitool held the lock")
		}
		if waited := time.Since(start); waited != ipmiLockWait || ipmiLockWait < ipmiTimeout+cmdWaitDelay {
			t.Fatalf("zone B waited %s for the lock, want %s, as long as a call can hold it", waited, ipmiLockWait)
		}
	})
}

// ---- test sensors and zones ----

// stubSensor returns a scripted sequence of results, repeating the last.
type stubSensor struct {
	name string
	seq  []result
	i    int
}

type result struct {
	r   Reading
	err error
}

func (s *stubSensor) Name() string { return s.name }
func (s *stubSensor) Read(context.Context) (Reading, error) {
	i := min(s.i, len(s.seq)-1)
	s.i++
	return s.seq[i].r, s.seq[i].err
}

func ok(t float64) result { return result{r: Reading{Temp: t}} }
func partial(t float64) result {
	return result{r: Reading{Temp: t, Partial: errors.New("/dev/sdb: exit status 2")}}
}
func idle() result { return result{r: Reading{Idle: true}} }
func fail() result { return result{err: errors.New("boom")} }

func stub(name string, seq ...result) *stubSensor { return &stubSensor{name: name, seq: seq} }

var testCurve = [][2]float64{{30, 30}, {50, 100}} // 3.5 % per °C

func newSource(sen Sensor, interval time.Duration, hyst float64) *source {
	return &source{
		cfg:    Source{Curve: testCurve, FailAfter: 3, Hysteresis: hyst, Interval: Duration{interval}},
		sensor: sen,
		avg:    window{size: 1},
	}
}

// newZone: min 30, max 100, startup 70, hold 2 m, then 10 %/min.
func newZone(f Runner, srcs ...*source) *zoneCtl {
	z := newZoneCtl(Zone{Name: "fanwall", IpmiZone: 1, MinDuty: 30, MaxDuty: 100, MinChange: 2,
		StartupDuty: ptr(70), RampDownDelay: &Duration{2 * time.Minute}, RampDownRate: ptr(10.0)},
		newIPMI(ipmitool{run: f, bin: "ipmitool"}, false), 5*time.Minute)
	z.tick = time.Second
	for _, s := range srcs {
		z.addSource(s)
	}
	return z
}

// noRamp makes drops apply at once, to isolate other behaviour.
func noRamp(z *zoneCtl) { z.cfg.RampDownDelay, z.cfg.RampDownRate = &Duration{0}, ptr(1e6) }

// ---- source state machine (pure) ----

func TestSourceTransitions(t *testing.T) {
	// One row per (state before, read): the state after and its demand.
	setup := map[sourceState]func(*source){
		statePending:  func(*source) {},
		stateOK:       func(s *source) { s.apply(ok(40).r, nil) }, // 65 %
		stateIdle:     func(s *source) { s.apply(idle().r, nil) },
		stateFailsafe: func(s *source) { s.failsafe("x=FAILSAFE") },
	}
	reads := map[string]result{"fail": fail(), "partial31": partial(31), "partial48": partial(48), "idle": idle(), "ok45": ok(45)}
	type want struct {
		st     sourceState
		demand float64 // ignored in failsafe
	}
	cases := map[sourceState]map[string]want{
		statePending: {"fail": {statePending, 0}, "partial31": {statePending, 33.5}, "partial48": {statePending, 93},
			"idle": {stateIdle, 0}, "ok45": {stateOK, 82.5}},
		stateOK: {"fail": {stateOK, 65}, "partial31": {stateOK, 65}, "partial48": {stateOK, 93},
			"idle": {stateIdle, 0}, "ok45": {stateOK, 82.5}},
		stateIdle: {"fail": {statePending, 0}, "partial31": {statePending, 33.5}, "partial48": {statePending, 93},
			"idle": {stateIdle, 0}, "ok45": {stateOK, 82.5}},
		stateFailsafe: {"fail": {stateFailsafe, 0}, "partial31": {stateFailsafe, 0}, "partial48": {stateFailsafe, 0},
			"idle": {stateIdle, 0}, "ok45": {stateOK, 82.5}},
	}
	for from, row := range cases {
		for read, w := range row {
			s := newSource(stub("disks"), time.Second, 0)
			s.cfg.FailAfter = 10
			setup[from](s)
			s.apply(reads[read].r, reads[read].err)
			if s.state != w.st || (w.st != stateFailsafe && math.Abs(s.demand-w.demand) > 1e-9) {
				t.Errorf("state %d + %s: got state %d demand %.1f, want state %d demand %.1f", from, read, s.state, s.demand, w.st, w.demand)
			}
			if w.st == stateFailsafe && s.frag != "x=FAILSAFE" {
				t.Errorf("state %d + %s: failsafe reason lost: %q", from, read, s.frag)
			}
		}
	}
}

func TestFailAfterCountsPartialReads(t *testing.T) {
	s := newSource(stub("disks"), time.Second, 0)
	s.apply(ok(35).r, nil)
	for _, r := range []result{fail(), partial(40), fail()} {
		s.apply(r.r, r.err)
	}
	if real, _ := s.want(&newZone(nil).cfg); s.state != stateFailsafe || real != 100 || s.frag != "disks=FAILSAFE" {
		t.Fatalf("after 3 bad reads: state %d, real %.0f, log %q", s.state, real, s.frag)
	}
}

func TestHysteresis(t *testing.T) {
	s := newSource(stub("disks"), time.Second, 1)
	for i, c := range []struct{ in, held float64 }{{41, 41}, {40, 41}, {41, 41}, {40, 41}, {39, 39}} {
		s.apply(Reading{Temp: c.in}, nil)
		if s.held != c.held {
			t.Fatalf("read %d: held %.0f, want %.0f", i, s.held, c.held)
		}
	}
}

func TestHotPartialReadingEntersAverage(t *testing.T) {
	s := newSource(stub("disks"), time.Second, 2)
	s.avg = window{size: 2}
	s.apply(ok(40).r, nil)
	s.apply(ok(40).r, nil)
	s.apply(partial(55).r, nil) // the missing disk may be hotter still
	if s.demand != 100 {
		t.Fatalf("hot partial reading: demand %.1f, want 100", s.demand)
	}
	s.apply(ok(41).r, nil) // averaged with the 55 °C, not with the 40s before it
	if want := Interpolate(testCurve, 48); s.demand != want {
		t.Fatalf("after a hot partial reading: demand %.1f, want %.1f", s.demand, want)
	}
	s.apply(partial(30).r, nil) // a cooler one stays out
	if s.held != 48 || len(s.avg.vals) != 2 || s.avg.vals[1] != 41 {
		t.Fatalf("cool partial reading entered the average: held %.1f, window %v", s.held, s.avg.vals)
	}
}

func TestRecoveryReadingNotAveragedWithOldOnes(t *testing.T) {
	s := newSource(stub("disks"), time.Second, 0)
	s.avg = window{size: 3}
	for _, r := range []result{ok(38), ok(38), fail(), fail(), fail(), ok(44)} {
		s.apply(r.r, r.err)
	}
	// 44 °C alone → 79 %; averaged with the old 38s it would be 40 °C → 65 %.
	if got := math.Round(s.demand); got != 79 {
		t.Fatalf("recovery demand %.0f%%, want 79%% (not mixed with pre-failure average)", got)
	}
}

func TestStuckReasonSurvivesErrors(t *testing.T) {
	z := newZone(&fakeRunner{}, newSource(stub("disks"), time.Second, 0))
	s, now := z.sources[0], time.Now()
	s.inFlight, s.startedAt = true, now.Add(-time.Hour)
	z.checkStuck(now)
	s.inFlight = false
	s.apply(Reading{}, errors.New("timeout")) // the stuck read finally fails
	if s.state != stateFailsafe || s.frag != "disks=STUCK" {
		t.Fatalf("state %d, log %q: want failsafe with the STUCK reason", s.state, s.frag)
	}
}

func TestUnpolledSourceLogsPending(t *testing.T) {
	z := newZone(&fakeRunner{}, newSource(stub("gpu", ok(45)), time.Second, 0))
	if frag := z.sources[0].frag; frag != "gpu=pending" {
		t.Fatalf("log for an unpolled source %q, want gpu=pending", frag)
	}
}

// ---- ramp, schedule and write decisions (pure) ----

func TestRampNeverSeesTimeRunBackwards(t *testing.T) {
	var r ramp
	t0 := time.Now()
	r.step(t0, 100, 0, 60)
	r.step(t0.Add(time.Second), 30, 0, 60) // drop seen
	at10 := r.step(t0.Add(10*time.Second), 30, 0, 60)
	if got := r.step(t0.Add(7*time.Second), 30, 0, 60); got > at10 {
		t.Fatalf("level rose from %.1f to %.1f on an older timestamp", at10, got)
	}
}

func TestNextSlot(t *testing.T) {
	t0 := time.Now()
	s := func(d time.Duration) time.Time { return t0.Add(d) }
	for _, c := range []struct {
		prev, now, want time.Time
	}{
		{time.Time{}, s(0), s(7 * time.Second)},                               // first poll
		{s(7 * time.Second), s(7500 * time.Millisecond), s(14 * time.Second)}, // late start keeps the cadence
		{s(7 * time.Second), s(30 * time.Second), s(37 * time.Second)},        // overran: next from now
	} {
		if got := nextSlot(c.prev, c.now, 7*time.Second); !got.Equal(c.want) {
			t.Errorf("nextSlot(%s, %s) = %s, want %s", c.prev.Sub(t0), c.now.Sub(t0), got.Sub(t0), c.want.Sub(t0))
		}
	}
}

func TestRetryDelayNeverOverflows(t *testing.T) {
	for n, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 5: 16 * time.Second, 6: retryMax, 35: retryMax, 100: retryMax} {
		if got := retryDelay(n); got != want {
			t.Errorf("retryDelay(%d) = %s, want %s", n, got, want)
		}
	}
	b := bmcState{duty: 50}
	now := time.Now()
	for range 100 {
		b.failed(83, now)
	}
	if send, wait := b.decide(order{valid: true, duty: 83, target: 83}, &newZone(nil).cfg, time.Hour, now); send || wait != retryMax {
		t.Fatalf("after 100 failures: send %v, wait %s; want a %s wait", send, wait, retryMax)
	}
}

// ---- zone, inline (step) ----

func TestZoneTakesHighestDemand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		z := newZone(f, newSource(stub("disks", ok(35)), time.Second, 0), newSource(stub("gpu", ok(45)), time.Second, 0))
		z.step(t.Context())
		// gpu 45 °C → 30 + 15*3.5 = 82.5 → 83 %
		if f.calls[0] != setDutyCmd(83) {
			t.Fatalf("raw command = %q", f.calls[0])
		}
	})
}

func TestZoneMinChangeAndReapply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		z := newZone(f, newSource(stub("disks", ok(40), ok(40.2), ok(42)), time.Second, 0))
		at, ctx := timeline(), t.Context()
		z.step(ctx) // 65 %
		at(time.Second)
		z.step(ctx)
		if f.n() != 1 {
			t.Fatalf("tiny change should not be sent: %v", f.calls)
		}
		at(2 * time.Second)
		z.step(ctx) // 72 %
		if f.last() != duty(72) {
			t.Fatalf("calls %v", f.calls)
		}
		at(6 * time.Minute)
		z.step(ctx) // unchanged, but reapply is due
		if f.n() != 3 || f.last() != duty(72) {
			t.Fatalf("reapply: %v", f.calls)
		}
	})
}

func TestRampUpFastDownSlow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		gpu := stub("gpu", ok(40), ok(50), ok(30))
		z := newZone(f, newSource(gpu, time.Second, 0))
		at, ctx := timeline(), t.Context()
		z.step(ctx) // 65 %
		at(time.Second)
		z.step(ctx)
		if f.last() != duty(100) {
			t.Fatalf("rise should apply at once: %v", f.calls)
		}
		// Demand falls to 30 % from here on.
		at(2 * time.Second)
		z.step(ctx)
		at(110 * time.Second)
		z.step(ctx)
		if f.last() != duty(100) {
			t.Fatalf("should hold during ramp_down_delay: %v", f.calls)
		}
		// The drop was first seen at 2 s, so the hold ends at 2 m 2 s; a
		// minute of ramping at 10 %/min later → 90 %.
		at(2*time.Second + 3*time.Minute)
		z.step(ctx)
		if f.last() != duty(90) {
			t.Fatalf("ramp down: got %s, want 90%%; %v", f.last(), f.calls)
		}
		// A rise during the ramp-down applies at once again.
		gpu.seq, gpu.i = []result{ok(50)}, 0
		at(4 * time.Minute)
		z.step(ctx)
		if f.last() != duty(100) {
			t.Fatalf("rise during ramp-down: %v", f.calls)
		}
		// Long after: settles exactly on the target, not min_change short of it.
		gpu.seq, gpu.i = []result{ok(30)}, 0
		for m := 5; m < 20; m++ {
			at(time.Duration(m) * time.Minute)
			z.step(ctx)
		}
		if f.last() != duty(30) {
			t.Fatalf("should settle at 30%%: %v", f.calls)
		}
	})
}

func TestHoldStartsAtFirstDrop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		z := newZone(f, newSource(stub("disks", ok(50), ok(30)), time.Minute, 0))
		z.cfg.RampDownDelay = &Duration{30 * time.Second}
		at, ctx := timeline(), t.Context()
		z.step(ctx) // 100 %
		at(time.Minute)
		z.step(ctx)
		if f.last() != duty(100) || f.n() != 1 {
			t.Fatalf("drop seen at 60 s must be held for 30 s: %v", f.calls)
		}
		// Hold ends at 90 s; 30 s of ramping at 10 %/min → 95 %.
		at(2 * time.Minute)
		z.step(ctx)
		if f.last() != duty(95) {
			t.Fatalf("ramp after hold: %v", f.calls)
		}
	})
}

func TestStartupAndFailsafe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		z := newZone(f, newSource(stub("gpu", fail(), fail(), fail(), ok(30)), time.Second, 0))
		noRamp(z)
		at, ctx := timeline(), t.Context()
		z.step(ctx)
		if f.last() != duty(70) {
			t.Fatalf("no reading yet should use startup duty: %v", f.calls)
		}
		at(time.Second)
		z.step(ctx)
		at(2 * time.Second)
		z.step(ctx)
		if f.last() != duty(100) {
			t.Fatalf("failsafe should go to 100%%: %v", f.calls)
		}
		// Recovery is a drop from real 100 %: no delay here, so the (huge)
		// ramp takes effect on the step after the drop is first seen.
		at(3 * time.Second)
		z.step(ctx)
		at(4 * time.Second)
		z.step(ctx)
		if f.last() != duty(30) {
			t.Fatalf("after recovery: %v", f.calls)
		}
	})
}

func TestStartupSnapsFailsafeRamps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		// default hold (2 m) and rate (10 %/min) from newZone
		z := newZone(f, newSource(stub("gpu", fail(), ok(30), fail(), fail(), fail(), ok(32)), time.Second, 0))
		at, ctx := timeline(), t.Context()
		z.step(ctx)
		if f.last() != duty(70) {
			t.Fatalf("startup: %v", f.calls)
		}
		at(time.Second)
		z.step(ctx) // first good reading: straight to 30 %, no hold
		if f.last() != duty(30) {
			t.Fatalf("leaving startup should skip the ramp: %v", f.calls)
		}
		for i := 2; i <= 4; i++ {
			at(time.Duration(i) * time.Second)
			z.step(ctx)
		}
		if f.last() != duty(100) {
			t.Fatalf("failsafe: %v", f.calls)
		}
		// Recovered at 32 °C (37 %), but failsafe was real 100 %: hold, then
		// ramp, so a flaky sensor can't make the fans hunt.
		at(5 * time.Second)
		z.step(ctx)
		at(5*time.Second + time.Minute)
		z.step(ctx)
		if f.last() != duty(100) {
			t.Fatalf("leaving failsafe should hold: %v", f.calls)
		}
		at(5*time.Second + 3*time.Minute) // 2 m hold, then 1 m at 10 %/min
		z.step(ctx)
		if f.last() != duty(90) {
			t.Fatalf("leaving failsafe should ramp after the hold: %v", f.calls)
		}
	})
}

func TestTransientErrorKeepsLastDemand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		z := newZone(f, newSource(stub("gpu", ok(45), fail()), time.Second, 0))
		z.cfg.RampDownDelay = &Duration{}
		at := timeline()
		z.step(t.Context())
		at(time.Minute)
		z.step(t.Context())
		if f.last() != duty(83) || f.n() != 1 {
			t.Fatalf("one failed read should keep 83%%: %v", f.calls)
		}
	})
}

func TestIdleDisksGiveMinDuty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		z := newZone(f, newSource(stub("disks", idle()), time.Second, 0))
		z.step(t.Context())
		if f.last() != duty(30) {
			t.Fatalf("idle disks should leave min duty: %v", f.calls)
		}
	})
}

func TestUnreadableDiskEndsIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		z := newZone(f, newSource(stub("disks", idle(), fail()), time.Second, 0))
		at := timeline()
		z.step(t.Context())
		at(time.Second)
		z.step(t.Context())
		if f.last() != duty(70) {
			t.Fatalf("an unreadable disk among sleeping ones should get startup duty: %v", f.calls)
		}
	})
}

func TestPartialReadingOnlyRaisesDemand(t *testing.T) {
	t.Run("keeps startup duty", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := &fakeRunner{}
			z := newZone(f, newSource(stub("disks", partial(34), partial(50)), time.Second, 0))
			at := timeline()
			z.step(t.Context()) // 34 °C → 44 %, but the missing disk may be hot
			if f.last() != duty(70) {
				t.Fatalf("cool partial reading released startup duty: %v", f.calls)
			}
			at(time.Second)
			z.step(t.Context()) // hotter partial reading still raises
			if f.last() != duty(100) {
				t.Fatalf("hot partial reading should raise demand: %v", f.calls)
			}
		})
	})
	t.Run("keeps previous demand", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := &fakeRunner{}
			z := newZone(f, newSource(stub("disks", ok(45), partial(31), ok(31)), time.Second, 0))
			noRamp(z)
			at, ctx := timeline(), t.Context()
			z.step(ctx)
			at(time.Second)
			z.step(ctx)
			at(2 * time.Second)
			z.evaluate(ctx, time.Now()) // no poll
			if f.last() != duty(83) || f.n() != 1 {
				t.Fatalf("partial cool reading lowered demand: %v", f.calls)
			}
			z.step(ctx) // full reading: drop is allowed
			at(3 * time.Second)
			z.evaluate(ctx, time.Now())
			if f.last() != duty(34) {
				t.Fatalf("full reading should lower demand: %v", f.calls)
			}
		})
	})
	t.Run("after idle uses startup floor", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := &fakeRunner{}
			z := newZone(f, newSource(stub("disks", idle(), partial(34), partial(50)), time.Second, 0))
			at, ctx := timeline(), t.Context()
			z.step(ctx)
			at(time.Second)
			z.step(ctx) // 34 °C survivor → 44 %, but floor is 70 %
			if f.last() != duty(70) {
				t.Fatalf("partial reading after idle skipped the startup floor: %v", f.calls)
			}
			at(2 * time.Second)
			z.step(ctx) // a hotter survivor still raises it
			if f.last() != duty(100) {
				t.Fatalf("hot partial reading should raise demand: %v", f.calls)
			}
		})
	})
}

func TestFloorDoesNotBypassRealHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		// Disks pending (startup 70 %) for two polls, then cool.
		disks := newSource(stub("disks", fail(), fail(), ok(30)), time.Second, 0)
		gpu := newSource(stub("gpu", ok(45), ok(30)), time.Second, 0) // 83 %, then 30 %
		z := newZone(f, disks, gpu)
		at, ctx := timeline(), t.Context()
		z.step(ctx)
		if f.last() != duty(83) {
			t.Fatalf("start: %v", f.calls)
		}
		at(time.Second)
		z.step(ctx) // gpu dropped: hold 83, don't fall to the 70 % floor
		if f.last() != duty(83) {
			t.Fatalf("startup floor bypassed the hold: %v", f.calls)
		}
		at(2 * time.Second)
		z.step(ctx) // disks leave pending
		if f.last() != duty(83) || f.n() != 1 {
			t.Fatalf("leaving startup changed the duty: %v", f.calls)
		}
		// Hold from the drop at 1 s ends at 2 m 1 s; a minute at 10 %/min → 73 %.
		at(time.Second + 3*time.Minute)
		z.step(ctx)
		if f.last() != duty(73) {
			t.Fatalf("ramp after hold: %v", f.calls)
		}
	})
}

func TestFloorEnteredMidHoldOnlyRaises(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		// Disks idle, then a failed read (back to pending, 70 %), then idle again.
		disks := newSource(stub("disks", idle(), idle(), fail(), idle()), time.Second, 0)
		cpu := newSource(stub("cpu", ok(48), ok(34)), time.Second, 0) // 93 %, then 44 %
		z := newZone(f, disks, cpu)
		at, ctx := timeline(), t.Context()
		z.step(ctx)
		at(time.Second)
		z.step(ctx) // cpu drop: holding 93
		at(2 * time.Second)
		z.step(ctx)
		if f.last() != duty(93) {
			t.Fatalf("entering startup mid-hold dropped the fans: %v", f.calls)
		}
		at(3 * time.Second)
		z.step(ctx) // idle again
		if f.last() != duty(93) || f.n() != 1 {
			t.Fatalf("leaving startup should not move the fans: %v", f.calls)
		}
	})
}

func TestCancelledReadIsNotAFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		z := newZone(&fakeRunner{}, newSource(stub("gpu", result{err: context.Canceled}), time.Second, 0))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		z.step(ctx)
		if s := z.sources[0]; s.failures != 0 || s.frag != "gpu=pending" {
			t.Fatalf("cancelled read counted: failures %d frag %q", s.failures, s.frag)
		}
	})
}

// ---- writes: backoff and retries (inline) ----

func TestFailedSettleWriteIsRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{err: map[string]error{}}
		// 40.3 °C → 66 %, then 32 °C → 37 %. Ramping 1 %/s with min_change 2
		// sends 66, 64 … 38, and the final 1 % step to 37 via the settle rule.
		z := newZone(f, newSource(stub("disks", ok(40.3), ok(32)), time.Second, 0))
		z.cfg.RampDownDelay, z.cfg.RampDownRate = &Duration{0}, ptr(60.0)
		f.err[setDutyCmd(37)] = errors.New("bmc busy")
		at, ctx := timeline(), t.Context()
		for i := 0; i <= 30; i++ {
			at(time.Duration(i) * time.Second)
			z.step(ctx)
		}
		if f.calls[len(f.calls)-1] != setDutyCmd(37) || z.bmc.duty != 38 {
			t.Fatalf("expected a failed write of 37%% after 38%%: duty %d, %v", z.bmc.duty, f.calls)
		}
		delete(f.err, setDutyCmd(37))
		at(30*time.Second + retryMax) // past any backoff
		z.step(ctx)
		if z.bmc.duty != 37 {
			t.Fatalf("failed settle write not retried: duty %d, %v", z.bmc.duty, f.calls)
		}
	})
}

func TestWriteRetriesBackOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{err: map[string]error{setDutyCmd(83): errors.New("bmc hung")}}
		z := newZone(f, newSource(stub("gpu", ok(45)), time.Second, 0)) // 83 %
		at, ctx := timeline(), t.Context()
		z.step(ctx) // fails: next try after 1 s
		for _, c := range []struct {
			at   time.Duration
			want int
		}{
			{500 * time.Millisecond, 1},
			{1100 * time.Millisecond, 2}, // fails again: 2 s
			{2500 * time.Millisecond, 2},
			{3200 * time.Millisecond, 3}, // 4 s
			{6 * time.Second, 3},
			{7300 * time.Millisecond, 4},
		} {
			at(c.at)
			z.evaluate(ctx, time.Now())
			if f.n() != c.want {
				t.Fatalf("at %s: %d attempts, want %d: %v", c.at, f.n(), c.want, f.calls)
			}
		}
		delete(f.err, setDutyCmd(83))
		at(time.Minute)
		z.evaluate(ctx, time.Now())
		if z.bmc.duty != 83 || z.bmc.failures != 0 {
			t.Fatalf("write not retried after the backoff: duty %d, %v", z.bmc.duty, f.calls)
		}
	})
}

func TestHigherDutySkipsWriteBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{err: map[string]error{setDutyCmd(83): errors.New("bmc hiccup")}}
		z := newZone(f, newSource(stub("gpu", ok(45), ok(50)), time.Second, 0)) // 83 %, then 100 %
		z.step(t.Context())
		time.Sleep(100 * time.Millisecond) // well inside the backoff
		z.step(t.Context())
		if f.last() != duty(100) || z.bmc.duty != 100 {
			t.Fatalf("max duty held back by the write backoff: %v", f.calls)
		}
	})
}

func TestRisingDutyStillBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{err: map[string]error{setDutyCmd(83): errors.New("bmc hung"), setDutyCmd(90): errors.New("bmc hung")}}
		z := newZone(f, newSource(stub("gpu", ok(45), ok(47)), time.Second, 0))
		z.step(t.Context()) // 83 % fails
		time.Sleep(100 * time.Millisecond)
		z.step(t.Context()) // 90 %: higher, but not max
		if f.n() != 1 {
			t.Fatalf("a rising duty skipped the backoff: %v", f.calls)
		}
	})
}

// failingRunner fails every command while fail is set.
type failingRunner struct {
	fakeRunner
	fail atomic.Bool
}

func (r *failingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := r.fakeRunner.Run(ctx, name, args...)
	if r.fail.Load() {
		return nil, errors.New("bmc rejects writes")
	}
	return out, err
}

func TestFailsafeHoldSurvivesRejectedWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &failingRunner{}
		r.fail.Store(true)
		src := newSource(stub("gpu", fail(), ok(32)), time.Second, 0)
		src.cfg.FailAfter = 1
		z := newZone(r, src)
		at, ctx := timeline(), t.Context()
		z.step(ctx) // failsafe: 100 %, write rejected
		at(retryMin)
		z.step(ctx) // recovered at 32 °C (37 %): hold 100, write rejected
		r.fail.Store(false)
		at(3 * retryMin)
		z.step(ctx)
		if r.last() != duty(100) {
			t.Fatalf("failsafe hold lost while no write had succeeded: %v", r.calls)
		}
	})
}

// slowRunner fails every command after d.
type slowRunner struct{ d time.Duration }

func (r *slowRunner) Run(context.Context, string, ...string) ([]byte, error) {
	time.Sleep(r.d)
	return nil, errors.New("bmc timeout")
}

func TestBackoffStartsWhenTheCallEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		z := newZone(&slowRunner{d: 5 * time.Second}, newSource(stub("gpu", ok(45)), time.Second, 0))
		start := time.Now()
		z.evaluate(t.Context(), start) // posts 83 % and tries once: 5 s to fail
		if got, want := z.bmc.retryAt.Sub(start), 5*time.Second+retryDelay(1); got != want {
			t.Fatalf("retry at +%s, want +%s (call time plus backoff, counted once)", got, want)
		}
		if wait := z.flush(t.Context()); wait != retryDelay(1) {
			t.Fatalf("writer waits %s after a slow failed call, want %s", wait, retryDelay(1))
		}
	})
}

// ---- the loop: real goroutines on the virtual clock ----

// captureLog sends the log to a buffer for the rest of the test.
func captureLog(t *testing.T) *syncBuffer {
	b := &syncBuffer{}
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return b
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// countSensor counts reads; safe for concurrent use.
type countSensor struct {
	name string
	n    atomic.Int32
	took time.Duration // each read sleeps this long
}

func (c *countSensor) Name() string { return c.name }
func (c *countSensor) Read(context.Context) (Reading, error) {
	c.n.Add(1)
	time.Sleep(c.took)
	return Reading{Temp: 40}, nil
}

// run starts z.loop and returns a function that stops it and waits. It
// also stops at cleanup: a test that fails with the loop still running
// would otherwise spin through virtual time for ever.
func run(t *testing.T, z *zoneCtl) (stop func()) {
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { z.loop(ctx); close(stopped) }()
	stop = sync.OnceFunc(func() { cancel(); <-stopped })
	t.Cleanup(stop)
	return stop
}

func TestPerSourceInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		disks, gpu := &countSensor{name: "disks"}, &countSensor{name: "gpu"}
		z := newZone(&fakeRunner{}, newSource(disks, 200*time.Millisecond, 0), newSource(gpu, 20*time.Millisecond, 0))
		stop := run(t, z)
		time.Sleep(450 * time.Millisecond)
		stop()
		// 0, 200, 400 ms for the disks; 0, 20 … 440 ms for the gpu.
		if d, g := disks.n.Load(), gpu.n.Load(); d != 3 || g != 23 {
			t.Fatalf("reads: disks %d (want 3), gpu %d (want 23)", d, g)
		}
	})
}

func TestIntervalNotRoundedToTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &countSensor{name: "disks"}
		z := newZone(&fakeRunner{}, newSource(c, 7*time.Second, 0))
		z.tick = 5 * time.Second
		stop := run(t, z)
		time.Sleep(69 * time.Second)
		stop()
		if n := c.n.Load(); n != 10 { // 0, 7 … 63 s
			t.Fatalf("%d reads in 69 s at a 7 s interval, want 10", n)
		}
	})
}

func TestOverrunPollRunsAgainAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &countSensor{name: "disks", took: 15 * time.Second}
		z := newZone(&fakeRunner{}, newSource(c, 10*time.Second, 0))
		stop := run(t, z)
		time.Sleep(31 * time.Second)
		stop()
		if n := c.n.Load(); n != 3 { // started at 0, 15, 30 s; never two at once
			t.Fatalf("%d reads, want 3", n)
		}
		time.Sleep(c.took) // let the last read end with the bubble
	})
}

// blockingSensor blocks in Read until released.
type blockingSensor struct {
	release func() // idempotent; also runs at cleanup
	gate    chan struct{}
	reads   atomic.Int32
	budget  atomic.Int64 // max read time in ns; 0 = the default
}

func newBlocking(t *testing.T) *blockingSensor {
	b := &blockingSensor{gate: make(chan struct{})}
	b.release = sync.OnceFunc(func() { close(b.gate) })
	t.Cleanup(b.release)
	return b
}

func (b *blockingSensor) Name() string { return "disks" }
func (b *blockingSensor) maxReadTime() time.Duration {
	if d := time.Duration(b.budget.Load()); d != 0 {
		return d
	}
	return readBudget(1)
}
func (b *blockingSensor) Read(context.Context) (Reading, error) {
	b.reads.Add(1)
	<-b.gate
	return Reading{Temp: 30}, nil
}

func TestSlowSourceDoesNotBlockZone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		disk := newBlocking(t)
		gpu := &countSensor{name: "gpu"}
		z := newZone(f, newSource(disk, time.Second, 0), newSource(gpu, 10*time.Millisecond, 0))
		stop := run(t, z)
		time.Sleep(95 * time.Millisecond)
		synctest.Wait()
		// Disk still pending (startup 70 %), gpu 40 °C → 65 %: the floor wins
		// but the gpu keeps polling.
		if n := gpu.n.Load(); n != 10 {
			t.Fatalf("gpu polled %d times in 95 ms while the disk hangs, want 10", n)
		}
		if n := disk.reads.Load(); n != 1 {
			t.Fatalf("disk polled %d times while its read hangs, want 1", n)
		}
		disk.release()
		stop()
	})
}

// gatedSensor answers once gate is closed.
type gatedSensor struct {
	gate chan struct{}
	r    Reading
}

func (g *gatedSensor) Name() string { return "gpu" }
func (g *gatedSensor) Read(context.Context) (Reading, error) {
	<-g.gate
	return g.r, nil
}

func TestLoopAppliesPollAsSoonAsItFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		gpu := &gatedSensor{gate: make(chan struct{}), r: Reading{Temp: 45}}
		z := newZone(f, newSource(gpu, time.Hour, 0))
		z.tick = time.Hour
		stop := run(t, z)
		synctest.Wait()
		if f.last() != duty(70) {
			t.Fatalf("startup duty not applied while the first poll runs: %v", f.calls)
		}
		start := time.Now()
		close(gpu.gate)
		synctest.Wait()
		if f.last() != duty(83) || time.Since(start) != 0 {
			t.Fatalf("result not applied on landing: %v after %s", f.calls, time.Since(start))
		}
		stop()
	})
}

func TestStuckReadFailsSafeAtItsLimit(t *testing.T) {
	logs := captureLog(t)
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		disk := newBlocking(t)
		disk.budget.Store(int64(100 * time.Millisecond))
		z := newZone(f, newSource(disk, time.Hour, 0)) // fail_after 3 × 1 h; no tick either
		z.tick = time.Hour
		stop := run(t, z)
		time.Sleep(99 * time.Millisecond)
		synctest.Wait()
		if f.last() != duty(70) {
			t.Fatalf("failed safe before the read budget: %v", f.calls)
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if f.last() != duty(100) || z.sources[0].frag != "disks=STUCK" {
			t.Fatalf("stuck read not failed safe at its budget: %v", f.calls)
		}
		time.Sleep(3 * time.Hour) // three more ticks, still stuck: logged once
		synctest.Wait()
		if n := strings.Count(logs.String(), "failing safe"); n != 1 {
			t.Fatalf("stuck read logged %d times, want once:\n%s", n, logs.String())
		}
		disk.release()
		stop()
	})
}

// hangFirst hangs its first read for good, ignoring its context like a
// disk in D state; later reads answer 40 °C.
type hangFirst struct {
	gate  chan struct{}
	reads atomic.Int32
}

func (h *hangFirst) Name() string               { return "disks" }
func (h *hangFirst) maxReadTime() time.Duration { return 100 * time.Millisecond }
func (h *hangFirst) Read(context.Context) (Reading, error) {
	if h.reads.Add(1) == 1 {
		<-h.gate
	}
	return Reading{Temp: 40}, nil
}

func TestStuckReadIsGivenUpOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		disk := &hangFirst{gate: make(chan struct{})}
		defer close(disk.gate)
		f := &fakeRunner{}
		z := newZone(f, newSource(disk, time.Second, 0))
		noRamp(z)
		stop := run(t, z)
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		if f.last() != duty(100) {
			t.Fatalf("not failed safe on a stuck read: %v", f.calls)
		}
		time.Sleep(2 * time.Second) // the next poll runs, though the stuck one never returns
		synctest.Wait()
		if f.last() != duty(65) || disk.reads.Load() < 2 {
			t.Fatalf("no recovery while the stuck read hangs: %v after %d reads", f.calls, disk.reads.Load())
		}
		stop()
	})
}

func TestOnlyOneStuckReadGivenUpOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		disk := newBlocking(t) // every read hangs
		disk.budget.Store(int64(100 * time.Millisecond))
		f := &fakeRunner{}
		z := newZone(f, newSource(disk, time.Second, 0))
		noRamp(z)
		stop := run(t, z)
		time.Sleep(time.Hour)
		synctest.Wait()
		if n := disk.reads.Load(); n != 2 {
			t.Fatalf("%d hung reads piled up, want 2: the one given up on and the next", n)
		}
		disk.release() // both land: the stale one frees the slot, the live one recovers
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if f.last() != duty(30) { // the blocking sensor reads 30 °C
			t.Fatalf("no recovery once the reads land: %v", f.calls)
		}
		stop()
	})
}

func TestLoopEvaluatesOnlyOnChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &countSensor{name: "gpu", took: 5 * time.Millisecond}
		z := newZone(&fakeRunner{}, newSource(c, 10*time.Millisecond, 0))
		z.tick = time.Hour
		start := time.Now()
		stop := run(t, z)
		time.Sleep(12 * time.Millisecond) // reads land at 5 and 15 ms; one starts at 10
		stop()
		if got := z.lastEval.Sub(start); got != 5*time.Millisecond {
			t.Fatalf("last evaluate at %s, want 5ms: a pass that only starts a read changes nothing", got)
		}
		time.Sleep(c.took)
	})
}

func TestStuckLimitFollowsDisksFoundDuringRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		disk := newBlocking(t)
		disk.budget.Store(int64(100 * time.Millisecond))
		z := newZone(f, newSource(disk, time.Hour, 0))
		z.tick = time.Hour
		stop := run(t, z)
		time.Sleep(50 * time.Millisecond)
		disk.budget.Store(int64(500 * time.Millisecond)) // autodetect found more disks
		time.Sleep(450*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		if f.last() != duty(70) {
			t.Fatalf("failed safe at the old limit: %v", f.calls)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if f.last() != duty(100) {
			t.Fatalf("not failed safe at the new limit: %v", f.calls)
		}
		disk.release()
		stop()
	})
}

func TestShutdownDoesNotWaitForHungPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		disk := newBlocking(t) // ignores its context, like a disk in D state
		z := newZone(&fakeRunner{}, newSource(disk, time.Second, 0))
		stop := run(t, z)
		synctest.Wait()
		start := time.Now()
		stop()
		if waited := time.Since(start); waited != shutdownWait {
			t.Fatalf("shutdown took %s with a hung read, want %s", waited, shutdownWait)
		}
		disk.release() // let the bubble end
	})
}

func TestHungBMCDoesNotStallPolling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bmc := &hungBMC{release: make(chan struct{})}
		gpu := &countSensor{name: "gpu"}
		z := newZone(bmc, newSource(gpu, 10*time.Millisecond, 0))
		z.tick = 10 * time.Millisecond
		stop := run(t, z)
		time.Sleep(95 * time.Millisecond)
		synctest.Wait()
		if n := gpu.n.Load(); n != 10 {
			t.Fatalf("polling stalled behind a hung fan write: %d polls", n)
		}
		if n := bmc.calls.Load(); n != 1 {
			t.Fatalf("%d ipmitool calls, want the one hung write", n)
		}
		start := time.Now()
		stop() // the writer's call is cancelled, so shutdown is prompt
		if time.Since(start) != 0 {
			t.Fatalf("shutdown waited %s", time.Since(start))
		}
	})
}

func TestWriterReappliesOnItsOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		z := newZone(f, newSource(stub("gpu", ok(45)), time.Hour, 0))
		z.tick, z.reapply = time.Hour, 20*time.Millisecond // no evaluate after the first
		stop := run(t, z)
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		// 83 % at 0 (the startup 70 % may or may not go out first: the read
		// lands in the same instant), then reapplied at 20 and 40 ms.
		f.mu.Lock()
		n := strings.Count(strings.Join(f.calls, "\n"), setDutyCmd(83))
		f.mu.Unlock()
		if n != 3 {
			t.Fatalf("duty not reapplied without an evaluate: %v", f.calls)
		}
		stop()
	})
}

// thenBlock returns its readings in turn, then blocks until cancelled.
type thenBlock struct {
	seq []Reading
	i   atomic.Int32
}

func (b *thenBlock) Name() string { return "disks" }
func (b *thenBlock) Read(ctx context.Context) (Reading, error) {
	if i := int(b.i.Add(1)) - 1; i < len(b.seq) {
		return b.seq[i], nil
	}
	<-ctx.Done()
	return Reading{}, ctx.Err()
}

func TestLoopRampsBetweenPolls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{}
		z := newZone(f, newSource(&thenBlock{seq: []Reading{{Temp: 50}, {Temp: 30}}}, 10*time.Millisecond, 0))
		z.cfg.RampDownDelay, z.cfg.RampDownRate = &Duration{0}, ptr(6000.0) // 100 %/s
		stop := run(t, z)
		time.Sleep(2 * time.Second) // 100 % → 30 % takes 0.7 s; evaluate ticks every 1 s
		synctest.Wait()
		if f.last() != duty(30) {
			t.Fatalf("ramp did not move between polls: %v", f.calls)
		}
		stop()
	})
}

func TestNoPassAfterShutdown(t *testing.T) {
	// A wakeup (tick or result) can race the cancel; whichever select
	// picks, the next pass must not run. A loop entered cancelled shows it.
	synctest.Test(t, func(t *testing.T) {
		f, c := &fakeRunner{}, &countSensor{name: "gpu"}
		z := newZone(f, newSource(c, time.Second, 0))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		z.loop(ctx)
		if c.n.Load() != 0 || f.n() != 0 {
			t.Fatalf("pass ran after cancel: %d reads, writes %v", c.n.Load(), f.calls)
		}
	})
}

// TestNextWakeIsAhead: after a loop pass (checkStuck, claimDue) the next
// wakeup is always in the future, whatever state the sources are in, so
// the loop can never spin.
func TestNextWakeIsAhead(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	now := time.Now()
	for i := range 10000 {
		disk := &blockingSensor{}
		disk.budget.Store(int64(time.Duration(1+rng.IntN(60)) * time.Second))
		s := newSource(disk, time.Duration(1+rng.IntN(60))*time.Second, 0)
		z := newZone(&fakeRunner{}, s)
		z.tick = time.Duration(1+rng.IntN(5000)) * time.Millisecond
		if rng.IntN(2) == 0 {
			s.next = now.Add(time.Duration(rng.IntN(120)-60) * time.Second)
		}
		if rng.IntN(2) == 0 {
			s.inFlight, s.startedAt = true, now.Add(-time.Duration(rng.IntN(120))*time.Second)
		}
		if rng.IntN(4) == 0 {
			s.failsafe("disks=FAILSAFE")
		}
		z.lastEval = now.Add(-time.Duration(rng.Int64N(int64(z.tick)))) // the loop evaluates when the tick is due
		z.checkStuck(now)
		z.claimDue(now)
		if w := z.nextWake(now); !w.After(now) {
			t.Fatalf("case %d: next wake %s is not after now (in flight %v, started %s, next %s)",
				i, w.Sub(now), s.inFlight, s.startedAt.Sub(now), s.next.Sub(now))
		}
	}
}

// ---- randomized invariants ----

// TestInvariants drives zones with random readings at random times and
// checks, after every evaluate, the rules the controller promises:
//
//   - the duty is within min..max, at least the real demand and the
//     startup floor, and max_duty whenever a source has failed safe;
//   - the ramp never falls faster than ramp_down_rate, and never before the
//     real demand has been below it for ramp_down_delay;
//   - with steady good readings, it settles exactly on the target, and so
//     do the fans.
func TestInvariants(t *testing.T) {
	for seed := range uint64(20) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { checkInvariants(t, rand.New(rand.NewPCG(seed, 1))) })
		})
	}
}

func checkInvariants(t *testing.T, rng *rand.Rand) {
	f := &fakeRunner{}
	var srcs []*source
	for i := range 1 + rng.IntN(3) {
		s := newSource(stub(fmt.Sprintf("s%d", i)), time.Second, float64(rng.IntN(3)))
		s.avg = window{size: 1 + rng.IntN(3)}
		s.cfg.FailAfter = 1 + rng.IntN(3)
		srcs = append(srcs, s)
	}
	z := newZone(f, srcs...)
	delay := time.Duration(rng.IntN(180)) * time.Second
	rate := 1 + rng.Float64()*49
	z.cfg.RampDownDelay, z.cfg.RampDownRate = &Duration{delay}, &rate
	z.cfg.MinChange = 1 + rng.IntN(4)
	cfg, ctx := &z.cfg, t.Context()

	randomRead := func() result {
		switch rng.IntN(10) {
		case 0:
			return fail()
		case 1:
			return partial(25 + rng.Float64()*30)
		case 2:
			return idle()
		default:
			return ok(25 + rng.Float64()*30)
		}
	}
	var prevLevel float64
	var prevAt, belowSince time.Time
	check := func(step int) {
		now := time.Now()
		real, floor, failsafe := float64(cfg.MinDuty), 0.0, false
		for _, s := range z.sources {
			r, fl := s.want(cfg)
			real, floor = math.Max(real, r), math.Max(floor, fl)
			failsafe = failsafe || s.state == stateFailsafe
		}
		realTarget := float64(clampRound(real, cfg.MinDuty, cfg.MaxDuty))
		if step > 0 && realTarget < prevLevel {
			if belowSince.IsZero() {
				belowSince = now
			}
		} else {
			belowSince = time.Time{}
		}
		z.evaluate(ctx, now)
		o, level := z.posted, z.ramp.level
		fail := func(format string, a ...any) {
			t.Helper()
			t.Fatalf("step %d: "+format+" (level %.2f, real %.0f, floor %.0f, order %+v)", append([]any{step}, append(a, level, real, floor, o)...)...)
		}
		switch {
		case o.duty < cfg.MinDuty || o.duty > cfg.MaxDuty:
			fail("duty out of range")
		case float64(o.duty) < realTarget || float64(o.duty) < floor:
			fail("duty below demand or floor")
		case failsafe && o.duty != cfg.MaxDuty:
			fail("failsafe source but duty %d", o.duty)
		}
		if step > 0 && level > prevLevel && level != realTarget {
			fail("level rose from %.2f, but not to the demand", prevLevel)
		}
		if step > 0 && level < prevLevel {
			if held := now.Sub(belowSince); held < delay {
				fail("ramped down after holding %s, want %s", held, delay)
			}
			// The ramp runs from the end of the hold, or the previous step.
			from := belowSince.Add(delay)
			if prevAt.After(from) {
				from = prevAt
			}
			if drop, most := prevLevel-level, rate*now.Sub(from).Minutes(); drop > most+1e-9 {
				fail("ramped down %.2f in %s, at most %.2f allowed", drop, now.Sub(from), most)
			}
		}
		// The fans (writes never fail here) follow: at max when asked, and
		// never left above a target the ramp has settled on.
		if o.duty == cfg.MaxDuty && z.bmc.duty != o.duty {
			fail("fans at %d, not max", z.bmc.duty)
		}
		if o.duty == o.target && z.bmc.duty > o.duty {
			fail("ramp settled on %d but fans left at %d", o.duty, z.bmc.duty)
		}
		prevLevel, prevAt = level, now
	}

	for step := range 500 {
		time.Sleep(time.Duration(rng.IntN(30000)) * time.Millisecond)
		for range rng.IntN(3) {
			s := z.sources[rng.IntN(len(z.sources))]
			if rng.IntN(20) == 0 { // a read that hangs past its limit
				s.inFlight, s.startedAt = true, time.Now().Add(-s.stuckLimit())
				z.checkStuck(time.Now())
				s.inFlight = false
				continue
			}
			r := randomRead()
			s.apply(r.r, r.err)
		}
		check(step)
	}

	// Steady good readings: settle on the target exactly.
	for _, s := range z.sources {
		for range s.avg.size {
			s.apply(Reading{Temp: 35}, nil) // 47.5 %
		}
	}
	// Long enough to hold, ramp the full range, and reapply a small rise.
	settled := time.Now().Add(delay + time.Duration(100/rate*float64(time.Minute)) + 2*z.reapply)
	for i := 500; time.Now().Before(settled); i++ {
		time.Sleep(30 * time.Second)
		check(i)
	}
	if o := z.posted; o.duty != o.target || z.bmc.duty != o.target {
		t.Fatalf("did not settle: order %+v, fans %d", o, z.bmc.duty)
	}
}
