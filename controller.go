package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"sync/atomic"
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

	next     time.Time   // when to poll next; zero = poll now (zone loop only)
	inFlight atomic.Bool // an async poll is running

	mu       sync.Mutex // guards the fields below
	avg      window
	failures int
	state    sourceState
	demand   float64 // duty from the curve, valid in stateOK
	held     float64 // temperature after hysteresis
	hasHeld  bool
	frag     string // last reading, for logs
}

// poll reads the sensor once and updates the source's state. The read
// itself runs unlocked, so a slow sensor never blocks the zone.
func (s *source) poll(ctx context.Context) {
	r, err := s.sensor.Read(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apply(r, err)
}

func (s *source) apply(r Reading, err error) {
	name := s.sensor.Name()
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
	if err != nil && r.Temp == 0 && !r.Idle {
		s.frag = name + "=err"
		return
	}
	// A partial reading (e.g. one disk failed) may only raise demand: the
	// unreadable part could be the hottest. It never releases the startup
	// duty and stays out of the average and hysteresis.
	if err != nil {
		d := Interpolate(s.cfg.Curve, r.Temp)
		switch {
		case s.state == stateIdle:
			s.state, s.demand = stateOK, d
		case d > s.demand:
			s.demand = d
		}
		s.frag = fmt.Sprintf("%s=%.1f°C→%d%%(partial)", name, r.Temp, int(math.Round(s.demand)))
		return
	}
	if r.Idle {
		s.state = stateIdle
		s.demand = 0
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

// want returns the duty this source currently asks for, and its log text.
func (s *source) want(maxDuty, startupDuty int) (float64, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case stateOK:
		return s.demand, s.frag
	case stateIdle:
		return 0, s.frag
	case stateFailsafe:
		return float64(maxDuty), s.frag
	default: // pending: partial readings may already ask for more
		return math.Max(float64(startupDuty), s.demand), s.frag
	}
}

// due reports whether the source should be polled at now. Half a tick of
// slack so ticker jitter never skips a poll.
func (s *source) due(now time.Time, tick time.Duration) bool {
	return s.next.IsZero() || !now.Before(s.next.Add(-tick/2))
}

// schedule advances the deadline from the previous one, skipping missed
// slots, so rounding to ticks keeps the configured average interval.
func (s *source) schedule(now time.Time) {
	if s.next.IsZero() {
		s.next = now
	}
	for s.next = s.next.Add(s.interval); !s.next.After(now); {
		s.next = s.next.Add(s.interval)
	}
}

type zoneCtl struct {
	cfg     Zone
	sources []*source
	ipmi    *IPMI
	reapply time.Duration
	tick    time.Duration // loop period: the shortest source interval
	async   bool          // poll in goroutines (set by loop); else inline
	wg      sync.WaitGroup

	startup   int
	rampDelay time.Duration
	rampRate  float64 // % per minute

	duty     int       // last duty sent, -1 = none yet
	level    float64   // internal duty; ramps down smoothly between whole percents
	dropAt   time.Time // when demand first fell below level; zero = not ramping
	lastStep time.Time
	lastSent time.Time
}

// step polls the sources that are due, takes the highest demand and moves
// the fans toward it: up at once, down only after ramp_down_delay and then
// at most ramp_down_rate % per minute.
func (z *zoneCtl) step(ctx context.Context, now time.Time) {
	for _, s := range z.sources {
		if !s.due(now, z.tick) {
			continue
		}
		if z.async {
			// A slow sensor (a hung disk) must not delay the others or the
			// fan writes: poll it in the background and use its last state.
			if !s.inFlight.CompareAndSwap(false, true) {
				continue // previous poll still running; retry next tick
			}
			z.wg.Add(1)
			go func(s *source) {
				defer z.wg.Done()
				defer s.inFlight.Store(false)
				s.poll(ctx)
			}(s)
		} else {
			s.poll(ctx)
		}
		s.schedule(now)
	}

	want := float64(z.cfg.MinDuty)
	parts := make([]string, 0, len(z.sources))
	for _, s := range z.sources {
		w, frag := s.want(z.cfg.MaxDuty, z.startup)
		if w > want {
			want = w
		}
		parts = append(parts, frag)
	}
	target := float64(clampRound(want, z.cfg.MinDuty, z.cfg.MaxDuty))

	switch {
	case z.duty < 0 || target >= z.level:
		z.level = target
		z.dropAt = time.Time{}
	default:
		// The hold starts when the drop is first seen, not at the last
		// high reading, so long ticks can't eat into it.
		if z.dropAt.IsZero() {
			z.dropAt = now
		}
		if now.Sub(z.dropAt) < z.rampDelay {
			break // hold: demand only just dropped
		}
		from := z.dropAt.Add(z.rampDelay)
		if z.lastStep.After(from) {
			from = z.lastStep
		}
		z.level = math.Max(target, z.level-z.rampRate*now.Sub(from).Minutes())
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
			// settling on the target at the end of a ramp-down; keyed on
			// the last duty actually sent, so a failed write is retried
			(z.duty > duty && float64(duty) == target))) ||
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
	z.async = true
	defer z.wg.Wait()
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
