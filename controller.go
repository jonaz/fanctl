package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"
)

type sourceState int

const (
	statePending  sourceState = iota // no good reading yet: zone uses startup_duty
	stateOK                          // demand comes from the curve
	stateIdle                        // nothing to measure (all disks asleep): no demand
	stateFailsafe                    // fail_after errors in a row: demand max_duty
)

// source is a sensor plus the state the controller keeps for it.
// Each source is polled on its own interval; between polls the zone
// keeps using its last demand.
type source struct {
	cfg      Source
	sensor   Sensor
	interval time.Duration
	avg      window

	next     time.Time // when to poll next; zero = poll now
	failures int
	state    sourceState
	demand   float64 // duty from the curve, valid in stateOK
	held     float64 // temperature after hysteresis
	hasHeld  bool
	frag     string // last reading, for logs
}

// poll reads the sensor once and updates the source's state.
func (s *source) poll(ctx context.Context) {
	name := s.sensor.Name()
	r, err := s.sensor.Read(ctx)
	if err != nil {
		s.failures++
		log.Printf("%s: read error (%d/%d): %v", name, s.failures, s.cfg.FailAfter, err)
	} else {
		s.failures = 0
	}
	if s.failures >= s.cfg.FailAfter {
		s.state = stateFailsafe
		s.frag = name + "=FAILSAFE"
		return
	}
	// A failed read with no usable data keeps the previous state (or the
	// startup duty if there is none yet); fail_after above bounds how long.
	// Partial readings, e.g. one disk failed, are still used.
	if err != nil && r.Temp == 0 && !r.Idle {
		s.frag = name + "=err"
		return
	}
	if r.Idle {
		s.state = stateIdle
		s.avg.reset()
		s.hasHeld = false
		s.frag = fmt.Sprintf("%s=idle(%s)", name, r.Detail)
		return
	}
	t := s.avg.add(r.Temp)
	// Hysteresis: follow rises at once, follow drops only once they exceed
	// the hysteresis. Stops 40↔41 °C flicker from moving the fans.
	if s.hasHeld && t < s.held && s.held-t <= s.cfg.Hysteresis {
		t = s.held
	}
	s.held, s.hasHeld = t, true
	s.demand = Interpolate(s.cfg.Curve, t)
	s.state = stateOK
	s.frag = fmt.Sprintf("%s=%.1f°C→%d%%", name, t, int(math.Round(s.demand)))
	if r.Detail != "" {
		s.frag += "(" + r.Detail + ")"
	}
}

// want returns the duty this source currently asks for.
func (s *source) want(maxDuty, startupDuty int) float64 {
	switch s.state {
	case stateOK:
		return s.demand
	case stateIdle:
		return 0
	case stateFailsafe:
		return float64(maxDuty)
	default:
		return float64(startupDuty)
	}
}

type zoneCtl struct {
	cfg     Zone
	sources []*source
	ipmi    *IPMI
	reapply time.Duration
	tick    time.Duration // loop period: the shortest source interval

	startup   int
	rampDelay time.Duration
	rampRate  float64 // % per minute

	duty     int     // last duty sent, -1 = none yet
	level    float64 // internal duty; ramps down smoothly between whole percents
	lastHigh time.Time
	lastStep time.Time
	lastSent time.Time
}

// step polls the sources that are due, takes the highest demand and moves
// the fans toward it: up at once, down only after ramp_down_delay and then
// at most ramp_down_rate % per minute.
func (z *zoneCtl) step(ctx context.Context, now time.Time) {
	for _, s := range z.sources {
		// Half a tick of slack so ticker jitter never skips a poll.
		if s.next.IsZero() || !now.Before(s.next.Add(-z.tick/2)) {
			s.poll(ctx)
			s.next = now.Add(s.interval)
		}
	}

	want := float64(z.cfg.MinDuty)
	parts := make([]string, 0, len(z.sources))
	for _, s := range z.sources {
		if w := s.want(z.cfg.MaxDuty, z.startup); w > want {
			want = w
		}
		parts = append(parts, s.frag)
	}
	target := float64(clampRound(want, z.cfg.MinDuty, z.cfg.MaxDuty))

	rampingDown := false
	switch {
	case z.duty < 0 || target >= z.level:
		z.level = target
		z.lastHigh = now
	case now.Sub(z.lastHigh) < z.rampDelay:
		// hold: demand only just dropped
	default:
		from := z.lastHigh.Add(z.rampDelay)
		if z.lastStep.After(from) {
			from = z.lastStep
		}
		z.level = math.Max(target, z.level-z.rampRate*now.Sub(from).Minutes())
		rampingDown = true
	}
	z.lastStep = now

	duty := clampRound(z.level, z.cfg.MinDuty, z.cfg.MaxDuty)
	diff := duty - z.duty
	if diff < 0 {
		diff = -diff
	}
	changed := duty != z.duty
	send := z.duty < 0 ||
		(changed && (diff >= z.cfg.MinChange ||
			duty == z.cfg.MaxDuty || duty == z.cfg.MinDuty ||
			(rampingDown && float64(duty) == target))) ||
		now.Sub(z.lastSent) >= z.reapply
	if !send {
		return
	}
	if err := z.ipmi.SetDuty(ctx, z.cfg.IpmiZone, duty); err != nil {
		log.Printf("zone %s: %v", z.cfg.Name, err)
		return
	}
	if changed {
		log.Printf("zone %s: %d%% (target %d%%) [%s]", z.cfg.Name, duty, int(target), strings.Join(parts, " "))
	}
	z.duty, z.lastSent = duty, now
}

func (z *zoneCtl) loop(ctx context.Context) {
	z.step(ctx, time.Now())
	t := time.NewTicker(z.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			z.step(ctx, now)
		}
	}
}
