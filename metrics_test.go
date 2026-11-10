package main

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func scrape(zones ...*zoneCtl) string {
	var b strings.Builder
	writeMetrics(&b, zones)
	return b.String()
}

// wellFormed checks the exposition format as Prometheus reads it: every
// sample belongs to the family declared just before it, and no series
// appears twice.
var (
	sampleRe = regexp.MustCompile(`^([a-z_]+)\{((?:[a-z_]+="(?:[^"\\]|\\.)*",?)*)\} (\S+)$`)
	typeRe   = regexp.MustCompile(`^# TYPE ([a-z_]+) (gauge|counter)$`)
)

func wellFormed(t *testing.T, out string) {
	t.Helper()
	family, seen := "", map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if m := typeRe.FindStringSubmatch(line); m != nil {
			family = m[1]
			continue
		}
		if strings.HasPrefix(line, "# HELP ") {
			continue
		}
		m := sampleRe.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("malformed line %q", line)
			continue
		}
		if m[1] != family {
			t.Errorf("%q outside its family (in %s)", line, family)
		}
		series := m[1] + "{" + m[2] + "}"
		if seen[series] {
			t.Errorf("duplicate series %s", series)
		}
		seen[series] = true
	}
}

func hasLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out, "\n"+l+"\n") {
			t.Errorf("missing %q in:\n%s", l, out)
		}
	}
}

func lacks(t *testing.T, out string, prefixes ...string) {
	t.Helper()
	for _, p := range prefixes {
		if strings.Contains(out, "\n"+p) {
			t.Errorf("unexpected %q in:\n%s", p, out)
		}
	}
}

func TestMetricsBeforeFirstEvaluate(t *testing.T) {
	z := newZone(&fakeRunner{}, newSource(stub("cpu", ok(40)), time.Second, 0))
	out := scrape(z)
	wellFormed(t, out)
	hasLines(t, out, `fanctl_zone_writes_total{zone="fanwall"} 0`)
	lacks(t, out, "fanctl_zone_duty_percent{", "fanctl_zone_wanted_duty_percent{",
		"fanctl_source_state{", "fanctl_source_read_duration_seconds{")
}

func TestMetricsAfterStep(t *testing.T) {
	f := &fakeRunner{}
	z := newZone(f,
		newSource(stub("disks", ok(40)), time.Second, 0), // 65 %
		newSource(stub("disks", idle()), time.Second, 0),
		newSource(stub("gpu", fail()), time.Second, 0),
	)
	z.step(t.Context())
	out := scrape(z)
	wellFormed(t, out)
	hasLines(t, out,
		`fanctl_build_info{version="dev"} 1`,
		// gpu is still pending after one failed read: startup duty.
		`fanctl_zone_duty_percent{zone="fanwall"} 70`,
		`fanctl_zone_wanted_duty_percent{zone="fanwall"} 70`,
		`fanctl_zone_target_duty_percent{zone="fanwall"} 70`,
		`fanctl_zone_writes_total{zone="fanwall"} 1`,
		`fanctl_zone_write_errors_total{zone="fanwall"} 0`,
		`fanctl_source_temperature_celsius{zone="fanwall",source="disks"} 40`,
		`fanctl_source_demand_percent{zone="fanwall",source="disks"} 65`,
		`fanctl_source_demand_percent{zone="fanwall",source="disks_2"} 0`,
		`fanctl_source_state{zone="fanwall",source="disks",state="ok"} 1`,
		`fanctl_source_state{zone="fanwall",source="disks",state="pending"} 0`,
		`fanctl_source_state{zone="fanwall",source="disks_2",state="idle"} 1`,
		`fanctl_source_state{zone="fanwall",source="gpu",state="pending"} 1`,
		`fanctl_source_reads_total{zone="fanwall",source="gpu"} 1`,
		`fanctl_source_read_errors_total{zone="fanwall",source="gpu"} 1`,
		`fanctl_source_read_errors_total{zone="fanwall",source="disks"} 0`,
		`fanctl_source_failsafes_total{zone="fanwall",source="gpu"} 0`,
	)
	// No temperature for an idle or failed source.
	lacks(t, out, `fanctl_source_temperature_celsius{zone="fanwall",source="disks_2"}`,
		`fanctl_source_temperature_celsius{zone="fanwall",source="gpu"}`)
}

func TestMetricsFailsafeCountedOnce(t *testing.T) {
	z := newZone(&fakeRunner{}, newSource(stub("gpu", ok(40), fail()), time.Second, 0))
	for range 6 {
		z.step(t.Context())
	}
	out := scrape(z)
	wellFormed(t, out)
	hasLines(t, out,
		`fanctl_source_state{zone="fanwall",source="gpu",state="failsafe"} 1`,
		`fanctl_source_demand_percent{zone="fanwall",source="gpu"} 100`,
		`fanctl_source_failsafes_total{zone="fanwall",source="gpu"} 1`,
		`fanctl_source_reads_total{zone="fanwall",source="gpu"} 6`,
		`fanctl_source_read_errors_total{zone="fanwall",source="gpu"} 5`,
		`fanctl_zone_duty_percent{zone="fanwall"} 100`,
	)
	lacks(t, out, `fanctl_source_temperature_celsius{`) // the old reading is not current
}

func TestMetricsWriteErrors(t *testing.T) {
	f := &fakeRunner{err: map[string]error{setDutyCmd(70): errors.New("bmc says no")}}
	z := newZone(f, newSource(stub("cpu", fail()), time.Second, 0))
	z.step(t.Context())
	out := scrape(z)
	hasLines(t, out,
		`fanctl_zone_write_errors_total{zone="fanwall"} 1`,
		`fanctl_zone_writes_total{zone="fanwall"} 0`,
		`fanctl_zone_wanted_duty_percent{zone="fanwall"} 70`,
	)
	lacks(t, out, "fanctl_zone_duty_percent{") // the BMC never accepted one
}

func TestMetricsCountStuckReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		disk := &hangFirst{gate: make(chan struct{})}
		defer close(disk.gate)
		z := newZone(&fakeRunner{}, newSource(disk, time.Second, 0))
		noRamp(z)
		stop := run(t, z)
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		out := scrape(z) // scraped while the loop runs: -race checks the handoff
		hasLines(t, out,
			`fanctl_source_state{zone="fanwall",source="disks",state="failsafe"} 1`,
			`fanctl_source_stuck_reads_total{zone="fanwall",source="disks"} 1`,
			`fanctl_source_failsafes_total{zone="fanwall",source="disks"} 1`,
			`fanctl_zone_duty_percent{zone="fanwall"} 100`,
		)
		time.Sleep(2 * time.Second) // recovers while the stuck read hangs
		synctest.Wait()
		out = scrape(z)
		wellFormed(t, out)
		hasLines(t, out,
			`fanctl_source_state{zone="fanwall",source="disks",state="ok"} 1`,
			`fanctl_source_temperature_celsius{zone="fanwall",source="disks"} 40`,
			`fanctl_source_stuck_reads_total{zone="fanwall",source="disks"} 1`,
			`fanctl_zone_duty_percent{zone="fanwall"} 65`,
		)
		stop()
	})
}

func TestMetricsEscapeLabels(t *testing.T) {
	z := newZone(&fakeRunner{}, newSource(stub("cpu", ok(40)), time.Second, 0))
	z.cfg.Name, z.metrics.name = `a"b\c`+"\n", `a"b\c`+"\n"
	z.step(t.Context())
	out := scrape(z)
	wellFormed(t, out)
	hasLines(t, out, `fanctl_zone_writes_total{zone="a\"b\\c\n"} 1`)
}

func TestMetricsServer(t *testing.T) {
	z := newZone(&fakeRunner{}, newSource(stub("cpu", ok(40)), time.Second, 0))
	z.step(t.Context())
	m, err := startMetrics("127.0.0.1:0", []*zoneCtl{z})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	resp, err := http.Get("http://" + m.ln.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	hasLines(t, string(body), `fanctl_source_temperature_celsius{zone="fanwall",source="cpu"} 40`)

	if _, err := startMetrics(m.ln.Addr().String(), nil); err == nil {
		t.Fatal("second server on the same port started")
	}
}

func TestMetricsListenValidated(t *testing.T) {
	c := Config{MetricsListen: "9101", Zones: []Zone{{Name: "a", Sources: []Source{{Type: "cpu", Curve: [][2]float64{{1, 1}, {2, 2}}}}}}}
	c.applyDefaults()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "metrics_listen") {
		t.Fatalf("got %v, want a metrics_listen error", err)
	}
	c.MetricsListen = ":9101"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
