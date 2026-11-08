package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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
	good.applyDefaults()
	if err := good.Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

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
	if err != nil || r.Temp != 44 || !strings.Contains(r.Detail, "/dev/sdb") || !strings.Contains(r.Detail, "1 asleep") {
		t.Fatalf("got %+v, %v", r, err)
	}

	// All asleep: idle, no error, nothing woken.
	s.devices = []string{"/dev/sdc"}
	r, err = s.Read(context.Background())
	if err != nil || !r.Idle {
		t.Fatalf("all asleep: got %+v, %v", r, err)
	}

	// One disk unreadable: keep the others' max, but report the error.
	f.out[smartKey("/dev/sdx")] = ""
	f.err[smartKey("/dev/sdx")] = errors.New("exit status 2")
	s.devices = []string{"/dev/sda", "/dev/sdx"}
	r, err = s.Read(context.Background())
	if err == nil || r.Temp != 38 {
		t.Fatalf("partial failure: got %+v, %v", r, err)
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

// stubSensor returns a scripted sequence of readings.
type stubSensor struct {
	name string
	seq  []Reading
	errs []error
	i    int
}

func (s *stubSensor) Name() string { return s.name }
func (s *stubSensor) Read(context.Context) (Reading, error) {
	i := s.i
	if i >= len(s.seq) {
		i = len(s.seq) - 1
	}
	s.i++
	var err error
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return s.seq[i], err
}

var testCurve = [][2]float64{{30, 30}, {50, 100}} // 3.5 % per °C

func newSource(st *stubSensor, interval time.Duration, hyst float64) *source {
	return &source{
		cfg:    Source{Curve: testCurve, FailAfter: 3, Hysteresis: hyst, Interval: Duration{interval}},
		sensor: st,
		avg:    window{size: 1},
	}
}

func newZone(f *fakeRunner, srcs ...*source) *zoneCtl {
	z := &zoneCtl{
		cfg:       Zone{Name: "fanwall", IpmiZone: 1, MinDuty: 30, MaxDuty: 100, MinChange: 2},
		ipmi:      &IPMI{run: f, bin: "ipmitool"},
		reapply:   5 * time.Minute,
		tick:      time.Second,
		startup:   70,
		rampDelay: 2 * time.Minute,
		rampRate:  10,
		duty:      -1,
	}
	for _, s := range srcs {
		z.addSource(s)
	}
	return z
}

func temps(ts ...float64) []Reading {
	r := make([]Reading, len(ts))
	for i, t := range ts {
		r[i] = Reading{Temp: t}
	}
	return r
}

func lastDuty(f *fakeRunner) string {
	if len(f.calls) == 0 {
		return ""
	}
	p := strings.Fields(f.calls[len(f.calls)-1])
	return p[len(p)-1]
}

func duty(n int) string { return fmt.Sprintf("0x%02x", n) }

func TestZoneTakesHighestDemand(t *testing.T) {
	f := &fakeRunner{}
	disks := newSource(&stubSensor{name: "disks", seq: temps(35)}, time.Second, 0)
	gpu := newSource(&stubSensor{name: "gpu", seq: temps(45)}, time.Second, 0)
	z := newZone(f, disks, gpu)
	z.step(context.Background(), time.Now())
	// gpu 45 °C → 30 + 15*3.5 = 82.5 → 83 %
	if f.calls[0] != "ipmitool raw 0x30 0x70 0x66 0x01 0x01 0x53" {
		t.Fatalf("raw command = %q", f.calls[0])
	}
}

func TestZoneMinChangeAndReapply(t *testing.T) {
	f := &fakeRunner{}
	z := newZone(f, newSource(&stubSensor{name: "disks", seq: temps(40, 40.2, 42)}, time.Second, 0))
	t0 := time.Now()
	z.step(context.Background(), t0) // 65 %
	z.step(context.Background(), t0.Add(time.Second))
	if len(f.calls) != 1 {
		t.Fatalf("tiny change should not be sent: %v", f.calls)
	}
	z.step(context.Background(), t0.Add(2*time.Second)) // 72 %
	if lastDuty(f) != duty(72) {
		t.Fatalf("calls %v", f.calls)
	}
	z.step(context.Background(), t0.Add(6*time.Minute)) // unchanged, but reapply is due
	if len(f.calls) != 3 || lastDuty(f) != duty(72) {
		t.Fatalf("reapply: %v", f.calls)
	}
}

func TestRampUpFastDownSlow(t *testing.T) {
	f := &fakeRunner{}
	z := newZone(f, newSource(&stubSensor{name: "gpu", seq: temps(40, 50, 30)}, time.Second, 0))
	ctx, t0 := context.Background(), time.Now()
	z.step(ctx, t0) // 65 %
	z.step(ctx, t0.Add(time.Second))
	if lastDuty(f) != duty(100) {
		t.Fatalf("rise should apply at once: %v", f.calls)
	}
	// Demand falls to 30 % from here on.
	z.step(ctx, t0.Add(2*time.Second))
	z.step(ctx, t0.Add(110*time.Second))
	if lastDuty(f) != duty(100) {
		t.Fatalf("should hold during ramp_down_delay: %v", f.calls)
	}
	// The drop was first seen at 2 s, so the hold ends at 2 m 2 s; a
	// minute of ramping at 10 %/min later → 90 %.
	z.step(ctx, t0.Add(2*time.Second+3*time.Minute))
	if lastDuty(f) != duty(90) {
		t.Fatalf("ramp down: got %s, want 90%%; %v", lastDuty(f), f.calls)
	}
	// A rise during the ramp-down applies at once again.
	z.sources[0].sensor = &stubSensor{name: "gpu", seq: temps(50)}
	z.step(ctx, t0.Add(4*time.Minute))
	if lastDuty(f) != duty(100) {
		t.Fatalf("rise during ramp-down: %v", f.calls)
	}
	// Long after: settles exactly on the target, not min_change short of it.
	z.sources[0].sensor = &stubSensor{name: "gpu", seq: temps(30)}
	for m := 5; m < 20; m++ {
		z.step(ctx, t0.Add(time.Duration(m)*time.Minute))
	}
	if lastDuty(f) != duty(30) {
		t.Fatalf("should settle at 30%%: %v", f.calls)
	}
}

func TestHysteresis(t *testing.T) {
	st := &stubSensor{name: "disks", seq: temps(41, 40, 41, 40, 39)}
	s := newSource(st, time.Second, 1)
	want := []float64{41, 41, 41, 41, 39}
	for i, w := range want {
		s.poll(context.Background())
		if s.held != w {
			t.Fatalf("poll %d: held %.0f, want %.0f", i, s.held, w)
		}
	}
}

func TestPerSourceInterval(t *testing.T) {
	disks := &stubSensor{name: "disks", seq: temps(35)}
	gpu := &stubSensor{name: "gpu", seq: temps(40)}
	z := newZone(&fakeRunner{}, newSource(disks, time.Minute, 0), newSource(gpu, 5*time.Second, 0))
	z.tick = 5 * time.Second
	t0 := time.Now()
	for i := 0; i <= 24; i++ { // two minutes, with up to 40 ms of ticker jitter
		jitter := time.Duration(i%3) * 20 * time.Millisecond
		z.step(context.Background(), t0.Add(time.Duration(i)*5*time.Second+jitter))
	}
	if disks.i != 3 || gpu.i != 25 {
		t.Fatalf("reads: disks %d (want 3), gpu %d (want 25)", disks.i, gpu.i)
	}
}

func TestStartupAndFailsafe(t *testing.T) {
	f := &fakeRunner{}
	boom := errors.New("boom")
	st := &stubSensor{name: "gpu",
		seq:  []Reading{{}, {}, {}, {Temp: 30}},
		errs: []error{boom, boom, boom, nil}}
	z := newZone(f, newSource(st, time.Second, 0))
	z.rampDelay, z.rampRate = 0, 1e6 // isolate from ramping
	ctx, t0 := context.Background(), time.Now()
	z.step(ctx, t0)
	if lastDuty(f) != duty(70) {
		t.Fatalf("no reading yet should use startup duty: %v", f.calls)
	}
	z.step(ctx, t0.Add(time.Second))
	z.step(ctx, t0.Add(2*time.Second))
	if lastDuty(f) != duty(100) {
		t.Fatalf("failsafe should go to 100%%: %v", f.calls)
	}
	z.step(ctx, t0.Add(3*time.Second))
	if lastDuty(f) != duty(30) {
		t.Fatalf("after recovery: %v", f.calls)
	}
}

func TestTransientErrorKeepsLastDemand(t *testing.T) {
	f := &fakeRunner{}
	st := &stubSensor{name: "gpu", seq: []Reading{{Temp: 45}, {}}, errs: []error{nil, errors.New("timeout")}}
	z := newZone(f, newSource(st, time.Second, 0))
	z.rampDelay = 0
	t0 := time.Now()
	z.step(context.Background(), t0)
	z.step(context.Background(), t0.Add(time.Minute))
	if lastDuty(f) != duty(83) || len(f.calls) != 1 {
		t.Fatalf("one failed read should keep 83%%: %v", f.calls)
	}
}

func TestIdleDisksGiveMinDuty(t *testing.T) {
	f := &fakeRunner{}
	z := newZone(f, newSource(&stubSensor{name: "disks", seq: []Reading{{Idle: true}}}, time.Second, 0))
	z.step(context.Background(), time.Now())
	if lastDuty(f) != duty(30) {
		t.Fatalf("idle disks should leave min duty: %v", f.calls)
	}
}

func TestSetModeCommand(t *testing.T) {
	f := &fakeRunner{}
	if err := (&IPMI{run: f, bin: "ipmitool"}).SetMode(context.Background(), fanModes["full"]); err != nil {
		t.Fatal(err)
	}
	if f.calls[0] != "ipmitool raw 0x30 0x45 0x01 0x01" {
		t.Fatalf("got %q", f.calls[0])
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
	zones, err := buildZones(c, &fakeRunner{}, &IPMI{}, look)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(zones[1].sources); n != 1 {
		t.Fatalf("optional gpu should be skipped, got %d sources", n)
	}
}

// blockingSensor blocks in Read until release is closed.
type blockingSensor struct {
	started chan struct{}
	release chan struct{}
	reads   atomic.Int32
}

func (b *blockingSensor) Name() string { return "disks" }
func (b *blockingSensor) Read(ctx context.Context) (Reading, error) {
	if b.reads.Add(1) == 1 {
		close(b.started)
	}
	<-b.release
	return Reading{Temp: 30}, nil
}

// signalSensor returns a fixed reading and signals each read.
type signalSensor struct {
	r    Reading
	done chan struct{}
}

func (s *signalSensor) Name() string { return "gpu" }
func (s *signalSensor) Read(context.Context) (Reading, error) {
	defer func() { s.done <- struct{}{} }()
	return s.r, nil
}

// waitPollDone waits for an async poll to finish storing its result.
func waitPollDone(s *source) {
	for s.inFlight.Load() {
		time.Sleep(time.Millisecond)
	}
}

func TestSlowSourceDoesNotBlockZone(t *testing.T) {
	f := &fakeRunner{}
	disk := &blockingSensor{started: make(chan struct{}), release: make(chan struct{})}
	gpu := &signalSensor{r: Reading{Temp: 45}, done: make(chan struct{}, 10)}
	z := newZone(f, newSource(&stubSensor{}, time.Minute, 0), newSource(&stubSensor{}, time.Second, 0))
	z.sources[0].sensor, z.sources[1].sensor = disk, gpu
	z.async = true
	ctx, t0 := context.Background(), time.Now()

	stepped := make(chan struct{})
	go func() {
		z.step(ctx, t0)
		<-disk.started
		<-gpu.done
		waitPollDone(z.sources[1])
		z.step(ctx, t0.Add(time.Second)) // gpu polled again; disk still busy
		<-gpu.done
		waitPollDone(z.sources[1])
		close(stepped)
	}()
	select {
	case <-stepped:
	case <-time.After(2 * time.Second):
		t.Fatal("zone blocked on a slow source")
	}
	z.step(ctx, t0.Add(2*time.Second)) // uses the gpu result from the 1 s poll
	// Disk still pending (startup 70 %), gpu 45 °C → 83 % wins.
	if lastDuty(f) != duty(83) {
		t.Fatalf("gpu demand should apply while disks hang: %v", f.calls)
	}
	z.step(ctx, t0.Add(time.Minute)) // disk due again but still in flight
	if n := disk.reads.Load(); n != 1 {
		t.Fatalf("disk polled %d times while in flight, want 1", n)
	}
	close(disk.release)
	z.wg.Wait()
}

func TestPartialReadingOnlyRaisesDemand(t *testing.T) {
	partial := errors.New("/dev/sdb: exit status 2")
	t.Run("keeps startup duty", func(t *testing.T) {
		f := &fakeRunner{}
		st := &stubSensor{name: "disks", seq: temps(34, 50), errs: []error{partial, partial}}
		z := newZone(f, newSource(st, time.Second, 0))
		ctx, t0 := context.Background(), time.Now()
		z.step(ctx, t0) // 34 °C → 44 %, but the missing disk may be hot
		if lastDuty(f) != duty(70) {
			t.Fatalf("cool partial reading released startup duty: %v", f.calls)
		}
		z.step(ctx, t0.Add(time.Second)) // hotter partial reading still raises
		if lastDuty(f) != duty(100) {
			t.Fatalf("hot partial reading should raise demand: %v", f.calls)
		}
	})
	t.Run("keeps previous demand", func(t *testing.T) {
		f := &fakeRunner{}
		st := &stubSensor{name: "disks", seq: temps(45, 31, 31), errs: []error{nil, partial, nil}}
		z := newZone(f, newSource(st, time.Second, 0))
		z.rampDelay, z.rampRate = 0, 1e6
		ctx, t0 := context.Background(), time.Now()
		for i := 0; i < 2; i++ {
			z.step(ctx, t0.Add(time.Duration(i)*time.Second))
			z.step(ctx, t0.Add(time.Duration(i)*time.Second+time.Millisecond)) // not due: no poll
		}
		if lastDuty(f) != duty(83) || len(f.calls) != 1 {
			t.Fatalf("partial cool reading lowered demand: %v", f.calls)
		}
		z.step(ctx, t0.Add(2*time.Second)) // full reading: drop is allowed
		z.step(ctx, t0.Add(3*time.Second))
		if lastDuty(f) != duty(34) {
			t.Fatalf("full reading should lower demand: %v", f.calls)
		}
	})
}

func TestIntervalNotRoundedToTick(t *testing.T) {
	st := &stubSensor{name: "disks", seq: temps(35)}
	z := newZone(&fakeRunner{}, newSource(st, 7*time.Second, 0))
	z.tick = 5 * time.Second
	t0 := time.Now()
	for i := 0; i <= 14; i++ { // 0…70 s
		z.step(context.Background(), t0.Add(time.Duration(i)*5*time.Second))
	}
	if st.i != 11 { // 70 s / 7 s + the first poll; was 15 (every tick)
		t.Fatalf("polls %d, want 11", st.i)
	}
}

func TestHoldStartsAtFirstDrop(t *testing.T) {
	f := &fakeRunner{}
	z := newZone(f, newSource(&stubSensor{name: "disks", seq: temps(50, 30)}, time.Minute, 0))
	z.tick, z.rampDelay = time.Minute, 30*time.Second
	ctx, t0 := context.Background(), time.Now()
	z.step(ctx, t0) // 100 %
	z.step(ctx, t0.Add(time.Minute))
	if lastDuty(f) != duty(100) || len(f.calls) != 1 {
		t.Fatalf("drop seen at 60 s must be held for 30 s: %v", f.calls)
	}
	// Hold ends at 90 s; 30 s of ramping at 10 %/min → 95 %.
	z.step(ctx, t0.Add(2*time.Minute))
	if lastDuty(f) != duty(95) {
		t.Fatalf("ramp after hold: %v", f.calls)
	}
}

func TestFailedSettleWriteIsRetried(t *testing.T) {
	f := &fakeRunner{err: map[string]error{}}
	// 40.3 °C → 66 %, then 32 °C → 37 %. Ramping 1 %/s with min_change 2
	// sends 66, 64 … 38, and the final 1 % step to 37 via the settle rule.
	z := newZone(f, newSource(&stubSensor{name: "disks", seq: temps(40.3, 32)}, time.Second, 0))
	z.rampDelay, z.rampRate = 0, 60
	set37 := "ipmitool raw 0x30 0x70 0x66 0x01 0x01 0x25"
	f.err[set37] = errors.New("bmc busy")
	ctx, t0 := context.Background(), time.Now()
	for i := 0; i <= 30; i++ {
		z.step(ctx, t0.Add(time.Duration(i)*time.Second))
	}
	if f.calls[len(f.calls)-1] != set37 || z.duty != 38 {
		t.Fatalf("expected a failed write of 37%% after 38%%: duty %d, %v", z.duty, f.calls)
	}
	delete(f.err, set37)
	z.step(ctx, t0.Add(31*time.Second))
	if z.duty != 37 {
		t.Fatalf("failed settle write not retried: duty %d, %v", z.duty, f.calls)
	}
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

func TestLeavingStartupOrFailsafeSkipsRamp(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeRunner{}
	// default hold (2 m) and rate (10 %/min) from newZone
	st := &stubSensor{name: "disks",
		seq:  []Reading{{}, {Temp: 30}, {}, {}, {}, {Temp: 32}},
		errs: []error{boom, nil, boom, boom, boom, nil}}
	z := newZone(f, newSource(st, time.Second, 0))
	ctx, t0 := context.Background(), time.Now()
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	z.step(ctx, at(0))
	if lastDuty(f) != duty(70) {
		t.Fatalf("startup: %v", f.calls)
	}
	z.step(ctx, at(1)) // first good reading: straight to 30 %, no hold
	if lastDuty(f) != duty(30) {
		t.Fatalf("leaving startup should skip the ramp: %v", f.calls)
	}
	for i := 2; i <= 4; i++ {
		z.step(ctx, at(i))
	}
	if lastDuty(f) != duty(100) {
		t.Fatalf("failsafe: %v", f.calls)
	}
	z.step(ctx, at(5)) // recovered at 32 °C → 37 %
	if lastDuty(f) != duty(37) {
		t.Fatalf("recovering from failsafe should skip the ramp: %v", f.calls)
	}
	// A real drop after that still holds.
	z.sources[0].sensor = &stubSensor{name: "disks", seq: temps(30)}
	z.step(ctx, at(6))
	if lastDuty(f) != duty(37) {
		t.Fatalf("real drop should hold: %v", f.calls)
	}
}

func TestHotReadingNotLostToQuickRepoll(t *testing.T) {
	f := &fakeRunner{}
	st := &stubSensor{name: "cpu", seq: temps(30, 50, 30)}
	z := newZone(f, newSource(st, time.Second, 0))
	ctx := context.Background()
	s := z.sources[0]
	s.poll(ctx)
	z.evaluate(ctx, time.Now()) // 30 %
	// Two polls finish before the zone looks: hot, then cool.
	s.poll(ctx)
	s.poll(ctx)
	z.evaluate(ctx, time.Now())
	if lastDuty(f) != duty(100) {
		t.Fatalf("hot reading overwritten before use: %v", f.calls)
	}
}

func TestLoopAppliesPollAsSoonAsItFinishes(t *testing.T) {
	f := &fakeRunner{}
	gate := make(chan struct{})
	gpu := &gatedSensor{gate: gate, r: Reading{Temp: 45}}
	z := newZone(f, newSource(&stubSensor{}, time.Hour, 0))
	z.sources[0].sensor = gpu
	z.tick = time.Hour // no second tick during the test
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { z.loop(ctx); close(stopped) }()
	waitFor(t, f, duty(70)) // startup duty while the first poll runs
	close(gate)
	waitFor(t, f, duty(83)) // applied on completion, not an hour later
	cancel()
	<-stopped
}

func TestStuckPollFailsSafe(t *testing.T) {
	f := &fakeRunner{}
	disk := &blockingSensor{started: make(chan struct{}), release: make(chan struct{})}
	z := newZone(f, newSource(&stubSensor{}, time.Second, 0))
	z.sources[0].sensor = disk
	z.async = true
	ctx, t0 := context.Background(), time.Now()
	z.step(ctx, t0)
	<-disk.started
	z.step(ctx, t0.Add(2*time.Second))
	if lastDuty(f) != duty(70) {
		t.Fatalf("before fail_after × interval: %v", f.calls)
	}
	z.step(ctx, t0.Add(3*time.Second)) // fail_after 3 × 1 s
	if lastDuty(f) != duty(100) {
		t.Fatalf("hung poll should fail safe: %v", f.calls)
	}
	close(disk.release)
	z.wg.Wait()
}

func TestUnreadableDiskEndsIdle(t *testing.T) {
	f := &fakeRunner{}
	st := &stubSensor{name: "disks", seq: []Reading{{Idle: true}, {}}, errs: []error{nil, errors.New("no disk temperatures")}}
	z := newZone(f, newSource(st, time.Second, 0))
	t0 := time.Now()
	z.step(context.Background(), t0)
	z.step(context.Background(), t0.Add(time.Second))
	if lastDuty(f) != duty(70) {
		t.Fatalf("an unreadable disk among sleeping ones should get startup duty: %v", f.calls)
	}
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

// waitFor waits until the last duty sent is d.
func waitFor(t *testing.T, f *fakeRunner, d string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := lastDuty(f)
		f.mu.Unlock()
		if got == d {
			return
		}
		time.Sleep(time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("duty %s never sent: %v", d, f.calls)
}
