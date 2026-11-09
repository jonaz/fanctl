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
	cfg    Source
	sensor Sensor

	next     time.Time   // when to poll next; zero = poll now
	inFlight atomic.Bool // a background poll is running

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
	// Cleared under the lock, so markStuck can't fail safe a poll that
	// has just delivered its result.
	defer s.inFlight.Store(false)
	if ctx.Err() != nil {
		return // shutting down: a cancelled read is not a sensor failure
	}
	s.apply(r, err)
	// Latch the demand until the zone has seen it, so a quick follow-up
	// poll can't replace a hot reading before it reaches the fans. Only
	// readings: the state itself carries the startup floor. An unseen
	// failsafe isn't latched: the zone only holds max_duty for failsafes
	// it actually acted on.
	if d := s.current(); s.state != stateFailsafe && d > s.peak {
		s.peak = d
	}
}

// failsafe demands max_duty until a good reading. Unlike the startup
// floor this is real demand: the fans hold and ramp down from max_duty
// after recovery, so a flaky sensor can't make them hunt. The average and
// hysteresis are cleared, so the first reading after recovery isn't mixed
// with pre-failure ones.
func (s *source) failsafe(frag string) {
	s.state, s.frag = stateFailsafe, frag
	s.avg.reset()
	s.hasHeld = false
}

// watch fails the source safe if the background read it guards outlives
// stuckLimit: failures are only counted when a read returns, and a hung one
// never does. Runs alongside the read until finished is closed. The limit
// is looked up again when the timer fires, so disks detected during the
// read extend it.
func (s *source) watch(finished <-chan struct{}, wake func()) {
	start := time.Now()
	t := time.NewTimer(s.stuckLimit())
	defer t.Stop()
	for {
		select {
		case <-finished:
			return
		case <-t.C:
			if rem := s.stuckLimit() - time.Since(start); rem > 0 {
				t.Reset(rem) // the limit grew meanwhile
				continue
			}
			s.markStuck(time.Since(start))
			wake()
			return
		}
	}
}

// markStuck fails a source safe whose poll has been running too long.
func (s *source) markStuck(running time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inFlight.Load() || s.state == stateFailsafe {
		return // finished meanwhile, or already failed safe
	}
	s.failsafe(s.sensor.Name() + "=STUCK")
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
		s.failsafe(name + "=FAILSAFE")
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

// want returns what this source asks for: its state, and the demand from
// readings (raised by any poll the zone hasn't seen yet). The zone turns
// failsafe into max_duty and pending into the startup floor.
func (s *source) want() (demand float64, st sourceState, frag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	demand = math.Max(s.current(), s.peak)
	s.peak = -1
	return demand, s.state, s.frag
}

// stuckLimit is the longest one read of this source can legitimately take.
func (s *source) stuckLimit() time.Duration {
	if b, ok := s.sensor.(interface{ maxReadTime() time.Duration }); ok {
		return b.maxReadTime() // e.g. disks: batches of smartctl
	}
	return readBudget(1)
}

// current is the demand from readings: none while idle or failed safe;
// while pending, partial readings may already ask for something.
func (s *source) current() float64 {
	if s.state == stateIdle || s.state == stateFailsafe {
		return 0
	}
	return s.demand
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

// zoneCtl runs one fan zone. Three kinds of goroutine share it in the loop:
//
//   - the loop itself owns the schedule and the ramp (evaluate);
//   - one goroutine per running poll reads a sensor and updates its source;
//   - the writer puts the duty on the fans, so a slow or hung BMC never
//     stalls polling or the ramp. evaluate posts the duty it wants into a
//     mailbox of one; the writer always sends the latest.
//
// Inline (tests, --once) the same code runs on the caller's goroutine.
type zoneCtl struct {
	cfg     Zone
	sources []*source
	ipmi    *IPMI
	reapply time.Duration
	tick    time.Duration    // loop period: the shortest source interval
	async   bool             // poll in the background (set by loop); else inline
	clock   func() time.Time // nil = time.Now; tests set a fake
	wg      sync.WaitGroup   // background polls and the writer
	done    chan struct{}    // a background poll finished; nil = nobody listens

	startup   int
	rampDelay time.Duration
	rampRate  float64 // % per minute

	// The ramp, owned by evaluate.
	level    float64   // internal duty; ramps down smoothly between whole percents
	dropAt   time.Time // when demand first fell below level; zero = not ramping
	lastStep time.Time

	// The mailbox from evaluate to the writer.
	mu     sync.Mutex
	posted order
	kick   chan struct{} // wakes the writer goroutine; nil = write inline

	// Owned by the writer.
	duty       int // last duty the BMC accepted, -1 = none yet
	lastSent   time.Time
	failures   int       // failed writes in a row
	failedAt   time.Time // when the last write failed
	failedDuty int       // what it tried to send
}

// order is what evaluate wants on the fans.
type order struct {
	valid  bool
	duty   int
	target int    // where the ramp is heading
	parts  string // per-source readings, for the log
}

func (z *zoneCtl) addSource(s *source) {
	s.peak = -1
	z.sources = append(z.sources, s)
}

// wake makes loop evaluate again soon; never blocks.
func (z *zoneCtl) wake() {
	if z.done != nil {
		select {
		case z.done <- struct{}{}:
		default: // a wakeup is already pending
		}
	}
}

// step polls the sources that are due and updates the fans.
//
// Inline it polls first, then evaluates. In the background it evaluates
// first and starts the polls: a slow sensor (a hung disk) must not delay
// the others, and each finished poll wakes the loop to evaluate again.
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
	z.evaluate(ctx, now)
	for _, s := range z.sources {
		if !s.due(now, z.tick) || !s.inFlight.CompareAndSwap(false, true) {
			continue // not due, or the previous poll is still running
		}
		z.wg.Add(1)
		go func(s *source) {
			defer z.wg.Done()
			finished := make(chan struct{})
			go s.watch(finished, z.wake)
			s.poll(ctx) // clears inFlight
			close(finished)
			z.wake()
		}(s)
		s.schedule(now)
	}
}

// evaluate moves the fans toward the highest demand.
//
// z.level follows real demand: up at once, down only after ramp_down_delay
// and then at most ramp_down_rate % per minute. A failsafe source counts as
// real max_duty demand, so the fans hold and ramp down after it recovers.
// The startup duty of a pending source is a floor on top of the level,
// never part of the ramp: entering startup can only raise the fans, and
// leaving it goes straight to wherever the real demand's ramp has got to.
func (z *zoneCtl) evaluate(ctx context.Context, now time.Time) {
	if now.Before(z.lastStep) {
		now = z.lastStep // never let the ramp see time run backwards
	}
	readWant, floor := float64(z.cfg.MinDuty), 0.0
	parts := make([]string, 0, len(z.sources))
	for _, s := range z.sources {
		d, st, frag := s.want()
		switch st {
		case stateFailsafe:
			d = float64(z.cfg.MaxDuty)
		case statePending:
			floor = math.Max(floor, float64(z.startup))
		}
		readWant = math.Max(readWant, d)
		parts = append(parts, frag)
	}
	realTarget := float64(clampRound(readWant, z.cfg.MinDuty, z.cfg.MaxDuty))
	target := clampRound(math.Max(readWant, floor), z.cfg.MinDuty, z.cfg.MaxDuty)

	switch {
	case z.lastStep.IsZero() || realTarget >= z.level:
		z.level = realTarget // first evaluate, or a rise
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
		z.level = math.Max(realTarget, z.level-z.rampRate*now.Sub(from).Minutes())
	}
	z.lastStep = now

	z.mu.Lock()
	z.posted = order{
		valid:  true,
		duty:   clampRound(math.Max(z.level, floor), z.cfg.MinDuty, z.cfg.MaxDuty),
		target: target,
		parts:  strings.Join(parts, " "),
	}
	z.mu.Unlock()
	if z.kick == nil {
		z.flush(ctx, now)
		return
	}
	select {
	case z.kick <- struct{}{}:
	default: // the writer already has a wakeup pending
	}
}

// Failed writes are retried after retryMin, doubling up to retryMax, so a
// hung BMC isn't hammered. A higher duty than the one that failed (a
// failsafe, say) is tried at once.
const (
	retryMin = time.Second
	retryMax = 30 * time.Second
)

// flush sends the posted duty if it is worth sending, and returns how long
// until it should look again (0 = only when a new duty is posted).
//
// A duty is sent when it moves at least min_change, hits min or max,
// settles on the target at the end of a ramp-down, or reapply is due. All
// of it keyed on the last duty the BMC accepted, so a failed write is
// retried.
func (z *zoneCtl) flush(ctx context.Context, now time.Time) time.Duration {
	z.mu.Lock()
	o := z.posted
	z.mu.Unlock()
	if !o.valid || ctx.Err() != nil {
		return 0
	}
	diff := o.duty - z.duty
	if diff < 0 {
		diff = -diff
	}
	changed := o.duty != z.duty
	reapplyIn := z.lastSent.Add(z.reapply).Sub(now)
	send := z.duty < 0 || reapplyIn <= 0 ||
		(changed && (diff >= z.cfg.MinChange ||
			o.duty == z.cfg.MaxDuty || o.duty == z.cfg.MinDuty ||
			(o.duty < z.duty && o.duty == o.target)))
	if !send {
		return reapplyIn
	}
	if z.failures > 0 && o.duty <= z.failedDuty {
		backoff := min(retryMin<<(z.failures-1), retryMax)
		if wait := z.failedAt.Add(backoff).Sub(now); wait > 0 {
			return wait
		}
	}
	start := time.Now()
	if err := z.ipmi.SetDuty(ctx, z.cfg.IpmiZone, o.duty); err != nil {
		if ctx.Err() != nil {
			return 0 // shutting down
		}
		log.Printf("zone %s: %v", z.cfg.Name, err)
		z.failures++
		// From the end of the attempt: a hung call took its whole timeout.
		z.failedAt, z.failedDuty = now.Add(time.Since(start)), o.duty
		return min(retryMin<<(z.failures-1), retryMax)
	}
	z.failures = 0
	if changed {
		log.Printf("zone %s: %d%% (target %d%%) [%s]", z.cfg.Name, o.duty, o.target, o.parts)
	}
	z.duty, z.lastSent = o.duty, now
	return z.reapply
}

// writer runs flush whenever a new duty is posted, a retry is due or the
// reapply interval comes round.
func (z *zoneCtl) writer(ctx context.Context) {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		if wait := z.flush(ctx, z.now()); wait > 0 {
			t.Reset(wait)
		} else {
			t.Stop()
		}
		select {
		case <-ctx.Done():
			return
		case <-z.kick:
		case <-t.C:
		}
	}
}

func (z *zoneCtl) now() time.Time {
	if z.clock != nil {
		return z.clock()
	}
	return time.Now()
}

// shutdownWait bounds how long loop waits for its goroutines on exit. A
// read stuck in the kernel (a disk in D state) never returns, and waiting
// for it would keep main from restoring the BMC fan mode.
const shutdownWait = 2 * time.Second

func (z *zoneCtl) loop(ctx context.Context) {
	z.async = true
	z.done = make(chan struct{}, 1)
	z.kick = make(chan struct{}, 1)
	z.wg.Add(1)
	go func() { defer z.wg.Done(); z.writer(ctx) }()
	defer z.waitWorkers(shutdownWait)
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
			if ctx.Err() != nil {
				return // a tick buffered at shutdown: no new polls
			}
			z.step(ctx, z.now())
		case <-z.done:
			if ctx.Err() != nil {
				return // cancelled polls wake us on shutdown
			}
			z.evaluate(ctx, z.now()) // a poll finished, or one is stuck
		}
	}
}

// waitWorkers waits for running polls and the writer, at most d. Reports
// whether all ended.
func (z *zoneCtl) waitWorkers(d time.Duration) bool {
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
