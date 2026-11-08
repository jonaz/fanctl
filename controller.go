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
	cfg     Source
	sensor  Sensor
	maxDuty int // the zone's limits, for want()
	startup int

	next     time.Time   // when to poll next; zero = poll now (zone loop only)
	started  time.Time   // when the running async poll began (zone loop only)
	inFlight atomic.Bool // an async poll is running

	mu       sync.Mutex // guards the fields below
	peak     float64    // highest demand since want() last ran; -1 = none
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
	// Latch the demand until the zone has seen it, so a quick follow-up
	// poll can't replace a hot reading before it reaches the fans. Only
	// real readings: startup/failsafe demand is already in the state.
	if w, forced := s.current(); !forced && w > s.peak {
		s.peak = w
	}
}

// markStuck fails a source safe whose poll has been running too long:
// failures are only counted when a read returns, and a hung one never does.
func (s *source) markStuck(running time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == stateFailsafe {
		return
	}
	s.state = stateFailsafe
	s.frag = s.sensor.Name() + "=STUCK"
	log.Printf("%s: read running for %s, failing safe", s.sensor.Name(), running.Round(time.Second))
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
		if s.state == stateIdle {
			// "Nothing to measure" no longer holds: a disk that just
			// stopped answering may be hot. Back to startup duty.
			s.state, s.demand = statePending, 0
		}
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
			// Coming out of "all asleep" with a disk unreadable: that
			// disk may be hot, so the startup floor applies again.
			s.state, s.demand = statePending, d
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

// want returns the duty this source asks for: its current demand, or a
// higher one from a poll the zone hasn't seen yet. forced is true when the
// demand comes from startup or failsafe rather than a reading.
func (s *source) want() (w float64, forced bool, frag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, forced = s.current()
	if s.peak > w {
		w, forced = s.peak, false
	}
	s.peak = -1
	return w, forced, s.frag
}

func (s *source) current() (float64, bool) {
	switch s.state {
	case stateOK:
		return s.demand, false
	case stateIdle:
		return 0, false
	case stateFailsafe:
		return float64(s.maxDuty), true
	default: // pending: partial readings may already ask for more
		return math.Max(float64(s.startup), s.demand), s.demand <= float64(s.startup)
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
	iv := s.cfg.Interval.Duration
	for s.next = s.next.Add(iv); !s.next.After(now); {
		s.next = s.next.Add(iv)
	}
}

type zoneCtl struct {
	cfg     Zone
	sources []*source
	ipmi    *IPMI
	reapply time.Duration
	tick    time.Duration    // loop period: the shortest source interval
	async   bool             // poll in goroutines (set by loop); else inline
	clock   func() time.Time // nil = time.Now; tests set a fake
	// stuckMin is the least time a poll may run before it is failed safe:
	// at least the command timeout, so one slow read just counts as one
	// error instead of forcing max_duty.
	stuckMin time.Duration
	wg       sync.WaitGroup
	done     chan struct{} // an async poll finished; nil = nobody listens

	startup   int
	rampDelay time.Duration
	rampRate  float64 // % per minute

	duty   int       // last duty sent, -1 = none yet
	level  float64   // internal duty; ramps down smoothly between whole percents
	dropAt time.Time // when demand first fell below level; zero = not ramping
	forced bool      // level came from startup/failsafe, not a reading
	// realLevel is the level real readings call for while forced, so
	// leaving startup/failsafe resumes from it (hold and ramp included).
	realLevel float64
	lastStep  time.Time
	lastSent  time.Time
}

func (z *zoneCtl) addSource(s *source) {
	s.maxDuty, s.startup, s.peak = z.cfg.MaxDuty, z.startup, -1
	z.sources = append(z.sources, s)
}

// step polls the sources that are due and updates the fans.
//
// Inline (tests, --once) it polls first, then evaluates. In the loop it
// evaluates first and polls in the background: a slow sensor (a hung disk)
// must not delay the others or the fan writes, and each finished poll wakes
// the loop to evaluate again.
func (z *zoneCtl) step(ctx context.Context, now time.Time) {
	if !z.async {
		for _, s := range z.sources {
			if s.due(now, z.tick) {
				s.poll(ctx)
				s.schedule(now)
			}
		}
		z.evaluate(ctx, now)
		return
	}
	for _, s := range z.sources {
		limit := max(time.Duration(s.cfg.FailAfter)*s.cfg.Interval.Duration, z.stuckMin)
		if s.inFlight.Load() && now.Sub(s.started) >= limit {
			s.markStuck(now.Sub(s.started))
		}
	}
	z.evaluate(ctx, now)
	launched := z.now() // after evaluate: a slow fan write isn't read time
	for _, s := range z.sources {
		if !s.due(now, z.tick) || !s.inFlight.CompareAndSwap(false, true) {
			continue // not due, or the previous poll is still running
		}
		s.started = launched
		z.wg.Add(1)
		go func(s *source) {
			defer z.wg.Done()
			s.poll(ctx)
			s.inFlight.Store(false)
			if z.done != nil {
				select {
				case z.done <- struct{}{}:
				default: // a wakeup is already pending
				}
			}
		}(s)
		s.schedule(now)
	}
}

// evaluate takes the highest demand and moves the fans toward it: up at
// once, down only after ramp_down_delay and then at most ramp_down_rate %
// per minute. Leaving startup or failsafe isn't a drop in heat, so it
// goes straight to the target.
func (z *zoneCtl) evaluate(ctx context.Context, now time.Time) {
	if now.Before(z.lastStep) {
		now = z.lastStep // never let the ramp see time run backwards
	}
	want, forcedWant, readWant := float64(z.cfg.MinDuty), 0.0, 0.0
	parts := make([]string, 0, len(z.sources))
	for _, s := range z.sources {
		w, forced, frag := s.want()
		want = math.Max(want, w)
		if forced {
			forcedWant = math.Max(forcedWant, w)
		} else {
			readWant = math.Max(readWant, w)
		}
		parts = append(parts, frag)
	}
	target := float64(clampRound(want, z.cfg.MinDuty, z.cfg.MaxDuty))
	forced := forcedWant > readWant && forcedWant > float64(z.cfg.MinDuty)

	if forced {
		if !z.forced { // entering startup/failsafe: remember the real level
			z.realLevel = 0
			if z.duty >= 0 {
				z.realLevel = z.level
			}
		}
		z.realLevel = math.Max(z.realLevel, readWant)
	}

	switch {
	case z.forced && !forced:
		// Leaving startup/failsafe isn't a drop in heat: go to what the
		// readings call for, and hold and ramp only from a real level.
		z.level = math.Max(target, z.realLevel)
		z.dropAt = time.Time{}
		if z.level > target {
			z.dropAt = now
		}
	case z.duty < 0 || target >= z.level || forced:
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
	z.forced = forced
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

func (z *zoneCtl) now() time.Time {
	if z.clock != nil {
		return z.clock()
	}
	return time.Now()
}

// shutdownWait bounds how long loop waits for running polls on exit. A
// read stuck in the kernel (a disk in D state) never returns, and waiting
// for it would keep main from restoring the BMC fan mode.
const shutdownWait = 2 * time.Second

func (z *zoneCtl) loop(ctx context.Context) {
	z.async = true
	z.done = make(chan struct{}, 1)
	defer z.waitPolls(shutdownWait)
	z.step(ctx, z.now())
	t := time.NewTicker(z.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Not the tick's own timestamp: a buffered tick can be older
			// than a completion already handled, and time must not go
			// backwards for the ramp.
			z.step(ctx, z.now())
		case <-z.done:
			z.evaluate(ctx, z.now())
		}
	}
}

// waitPolls waits for running polls, at most d. Reports whether all ended.
func (z *zoneCtl) waitPolls(d time.Duration) bool {
	done := make(chan struct{})
	go func() { z.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		log.Printf("zone %s: polls still running at shutdown, not waiting", z.cfg.Name)
		return false
	}
}
