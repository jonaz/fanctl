package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Prometheus metrics, in the text exposition format, written by hand so
// the binary keeps no dependencies.
//
// The zone loop and the writer own their state (see controller.go), so
// the HTTP handler never reads it: the loop copies what it shows into the
// zone's zoneMetrics at the end of each evaluate (at least once a second),
// the writer after each write, both under zoneMetrics' own lock.

// sourceMetrics is a copy of one source's state, taken by the loop.
type sourceMetrics struct {
	label    string
	state    sourceState
	temp     float64 // NaN = no current temperature
	demand   float64 // what the source asks of the zone, before min/max
	reads    uint64
	errors   uint64 // failed or partial reads
	stuck    uint64
	failsafe uint64 // times it entered failsafe
	readTime float64
}

// zoneSnap is what /metrics shows for one zone.
type zoneSnap struct {
	name    string
	sources []sourceMetrics // from the loop
	duty    int             // wanted now, from the loop; -1 = not yet
	target  int
	// From the writer.
	applied     int // last duty the BMC accepted; -1 = none yet
	writes      uint64
	writeErrors uint64
}

// zoneMetrics is a zoneSnap the loop and the writer update and the
// handler copies, under mu.
type zoneMetrics struct {
	mu sync.Mutex
	zoneSnap
}

func newZoneMetrics(name string) *zoneMetrics {
	return &zoneMetrics{zoneSnap: zoneSnap{name: name, duty: -1, target: -1, applied: -1}}
}

// publish copies the loop's view of the zone. Loop goroutine only.
func (z *zoneCtl) publish(o order) {
	snap := make([]sourceMetrics, len(z.sources))
	for i, s := range z.sources {
		real, _ := s.want(&z.cfg)
		temp := math.NaN()
		if s.hasTemp {
			temp = s.temp
		}
		snap[i] = sourceMetrics{
			label: s.label, state: s.state, temp: temp, demand: real,
			reads: s.reads, errors: s.readErrors, stuck: s.stuckReads, failsafe: s.failsafes,
			readTime: s.readTime.Seconds(),
		}
	}
	m := z.metrics
	m.mu.Lock()
	m.sources, m.duty, m.target = snap, o.duty, o.target
	m.mu.Unlock()
}

// wrote records a write attempt. Writer goroutine only.
func (m *zoneMetrics) wrote(duty int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.writeErrors++
		return
	}
	m.writes++
	m.applied = duty
}

var stateNames = []struct {
	st   sourceState
	name string
}{{statePending, "pending"}, {stateOK, "ok"}, {stateIdle, "idle"}, {stateFailsafe, "failsafe"}}

// writeMetrics writes every zone's metrics in the text exposition format.
func writeMetrics(w io.Writer, zones []*zoneCtl) {
	snaps := make([]zoneSnap, len(zones))
	for i, z := range zones {
		m := z.metrics
		m.mu.Lock()
		snaps[i] = m.zoneSnap // sources is replaced, never changed in place
		m.mu.Unlock()
	}

	var b strings.Builder
	family := func(name, typ, help string, rows func(add func(labels string, v float64))) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		rows(func(labels string, v float64) {
			fmt.Fprintf(&b, "%s{%s} %s\n", name, labels, formatValue(v))
		})
	}
	zl := func(z zoneSnap) string { return `zone="` + escapeLabel(z.name) + `"` }
	sl := func(z zoneSnap, s sourceMetrics) string {
		return zl(z) + `,source="` + escapeLabel(s.label) + `"`
	}
	perZone := func(f func(z zoneSnap, add func(string, float64))) func(func(string, float64)) {
		return func(add func(string, float64)) {
			for _, z := range snaps {
				f(z, add)
			}
		}
	}
	perSource := func(f func(z zoneSnap, s sourceMetrics, add func(string, float64))) func(func(string, float64)) {
		return perZone(func(z zoneSnap, add func(string, float64)) {
			for _, s := range z.sources {
				f(z, s, add)
			}
		})
	}
	known := func(v int, labels string, add func(string, float64)) {
		if v >= 0 {
			add(labels, float64(v))
		}
	}

	family("fanctl_build_info", "gauge", "fanctl version; always 1.", func(add func(string, float64)) {
		add(`version="`+escapeLabel(version)+`"`, 1)
	})
	family("fanctl_zone_duty_percent", "gauge", "Last duty the BMC accepted for the zone.",
		perZone(func(z zoneSnap, add func(string, float64)) { known(z.applied, zl(z), add) }))
	family("fanctl_zone_wanted_duty_percent", "gauge", "Duty the zone wants now, after the ramp and the startup floor.",
		perZone(func(z zoneSnap, add func(string, float64)) { known(z.duty, zl(z), add) }))
	family("fanctl_zone_target_duty_percent", "gauge", "Duty the zone is ramping towards.",
		perZone(func(z zoneSnap, add func(string, float64)) { known(z.target, zl(z), add) }))
	family("fanctl_zone_writes_total", "counter", "Duty writes the BMC accepted.",
		perZone(func(z zoneSnap, add func(string, float64)) { add(zl(z), float64(z.writes)) }))
	family("fanctl_zone_write_errors_total", "counter", "Duty writes that failed.",
		perZone(func(z zoneSnap, add func(string, float64)) { add(zl(z), float64(z.writeErrors)) }))

	family("fanctl_source_temperature_celsius", "gauge", "Latest temperature read (the hottest part; a lower bound after a partial read). Absent while pending, idle, failed or failed safe.",
		perSource(func(z zoneSnap, s sourceMetrics, add func(string, float64)) {
			if !math.IsNaN(s.temp) {
				add(sl(z, s), s.temp)
			}
		}))
	family("fanctl_source_demand_percent", "gauge", "Duty the source asks of its zone (max duty when failed safe, 0 when idle).",
		perSource(func(z zoneSnap, s sourceMetrics, add func(string, float64)) { add(sl(z, s), s.demand) }))
	family("fanctl_source_state", "gauge", "Source state: 1 for the current one of pending, ok, idle, failsafe.",
		perSource(func(z zoneSnap, s sourceMetrics, add func(string, float64)) {
			for _, st := range stateNames {
				v := 0.0
				if s.state == st.st {
					v = 1
				}
				add(sl(z, s)+`,state="`+st.name+`"`, v)
			}
		}))
	family("fanctl_source_reads_total", "counter", "Reads that completed, good or not.",
		perSource(func(z zoneSnap, s sourceMetrics, add func(string, float64)) { add(sl(z, s), float64(s.reads)) }))
	family("fanctl_source_read_errors_total", "counter", "Reads that failed or were partial.",
		perSource(func(z zoneSnap, s sourceMetrics, add func(string, float64)) { add(sl(z, s), float64(s.errors)) }))
	family("fanctl_source_stuck_reads_total", "counter", "Reads given up on as stuck.",
		perSource(func(z zoneSnap, s sourceMetrics, add func(string, float64)) { add(sl(z, s), float64(s.stuck)) }))
	family("fanctl_source_failsafes_total", "counter", "Times the source failed safe to max duty.",
		perSource(func(z zoneSnap, s sourceMetrics, add func(string, float64)) { add(sl(z, s), float64(s.failsafe)) }))
	family("fanctl_source_read_duration_seconds", "gauge", "How long the latest completed read took.",
		perSource(func(z zoneSnap, s sourceMetrics, add func(string, float64)) {
			if s.reads > 0 {
				add(sl(z, s), s.readTime)
			}
		}))
	_, _ = io.WriteString(w, b.String())
}

func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return fmt.Sprint(v)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }

// metricsServer serves /metrics. It listens at once, so a bad address
// stops fanctl before it touches the BMC; after that a failing server is
// only logged, never a reason to stop controlling the fans.
type metricsServer struct {
	srv  *http.Server
	ln   net.Listener
	done chan struct{}
}

func startMetrics(addr string, zones []*zoneCtl) (*metricsServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeMetrics(w, zones)
	})
	m := &metricsServer{
		srv: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout: 10 * time.Second, IdleTimeout: time.Minute},
		ln:   ln,
		done: make(chan struct{}),
	}
	go func() {
		defer close(m.done)
		if err := m.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics: %v", err)
		}
	}()
	log.Printf("metrics on http://%s/metrics", ln.Addr())
	return m, nil
}

func (m *metricsServer) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = m.srv.Shutdown(ctx)
	<-m.done
}
