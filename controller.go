package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
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
func (s *source) poll(ctx context.Context) { s.pollWatched(ctx, nil) }

// pollWatched is poll for a background read guarded by watch: finished
// (nil = unwatched) is closed under the lock as the result lands, so that
// read's watchdog can never fail safe a poll that has delivered, nor the
// one started after it.
func (s *source) pollWatched(ctx context.Context, finished chan struct{}) {
	r, err := s.sensor.Read(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if finished != nil {
		defer close(finished)
	}
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
func (s *source) watch(finished chan struct{}, wake func()) {
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
			s.markStuck(finished, time.Since(start))
			wake()
			return
		}
	}
}

// markStuck fails a source safe whose poll (identified by its finished
// channel) has been running too long.
func (s *source) markStuck(finished chan struct{}, running time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-finished:
		return // that poll delivered meanwhile
	default:
	}
	if s.state == stateFailsafe {
		return // already failed safe
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
	if err != nil && s.state == stateFailsafe {
		return // still failed safe (e.g. a stuck read returning an error): keep the reason
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

// zoneCtl runs one fan zone. Three kinds of goroutine share it in the loop:
//
//   - the loop itself owns the ramp (evaluate), run every tick and whenever
//     a poll lands;
//   - one goroutine per source polls it on its own ticker, so a slow sensor
//     (a hung disk) never delays the others, and wakes the loop;
//   - the writer puts the duty on the fans, so a slow or hung BMC never
//     stalls polling or the ramp. evaluate posts the duty it wants into a
//     mailbox of one; the writer always sends the latest.
//
// Inline (tests, --once) step polls and writes on the caller's goroutine.
type zoneCtl struct {
	cfg     Zone
	sources []*source
	ipmi    *IPMI
	reapply time.Duration
	tick    time.Duration    // how often loop evaluates without a poll; 0 = evalTick
	clock   func() time.Time // nil = time.Now; tests set a fake
	wg      sync.WaitGroup   // pollers and the writer
	done    chan struct{}    // a poll landed or got stuck; nil = nobody listens

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
	retryAt    time.Time // when the last failed write may be retried
	failedDuty int       // what it tried to send
}

// order is what evaluate wants on the fans.
type order struct {
	valid  bool
	duty   int
	target int      // where the ramp is heading
	parts  []string // per-source readings, for the log
}

func (z *zoneCtl) addSource(s *source) {
	s.peak = -1
	s.frag = s.sensor.Name() + "=pending" // until the first poll
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

// step polls every source once, inline, then evaluates: --once and tests.
func (z *zoneCtl) step(ctx context.Context, now time.Time) {
	for _, s := range z.sources {
		s.poll(ctx)
	}
	z.evaluate(ctx, now)
}

// pollEvery polls s on its own ticker until ctx ends, under a watchdog,
// waking the loop after each poll. A poll running past the next tick
// delays it (time.Ticker drops the missed ones), so polls never overlap.
func (z *zoneCtl) pollEvery(ctx context.Context, s *source) {
	t := time.NewTicker(s.cfg.Interval.Duration)
	defer t.Stop()
	for {
		finished := make(chan struct{})
		go s.watch(finished, z.wake)
		s.pollWatched(ctx, finished)
		z.wake()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// startPolling starts one pollEvery goroutine per source.
func (z *zoneCtl) startPolling(ctx context.Context) {
	for _, s := range z.sources {
		z.wg.Add(1)
		go func() { defer z.wg.Done(); z.pollEvery(ctx, s) }()
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
			floor = math.Max(floor, float64(*z.cfg.StartupDuty))
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
		delay, rate := z.cfg.RampDownDelay.Duration, *z.cfg.RampDownRate
		if now.Sub(z.dropAt) < delay {
			break // hold: demand only just dropped
		}
		from := z.dropAt.Add(delay)
		if z.lastStep.After(from) {
			from = z.lastStep
		}
		z.level = math.Max(realTarget, z.level-rate*now.Sub(from).Minutes())
	}
	z.lastStep = now

	z.mu.Lock()
	z.posted = order{
		valid:  true,
		duty:   clampRound(math.Max(z.level, floor), z.cfg.MinDuty, z.cfg.MaxDuty),
		target: target,
		parts:  parts,
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
// hung BMC isn't hammered.
const (
	retryMin = time.Second
	retryMax = 30 * time.Second
)

// retryDelay is the wait after n failed writes in a row (n >= 1). Doubling
// stops at retryMax, so no number of failures can overflow it.
func retryDelay(n int) time.Duration {
	d := retryMin
	for i := 1; i < n && d < retryMax; i++ {
		d *= 2
	}
	return min(d, retryMax)
}

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
	// Only a jump to max_duty (a failsafe, say) skips the wait: a merely
	// rising demand would otherwise retry a hung BMC on every evaluate.
	urgent := o.duty == z.cfg.MaxDuty && z.failedDuty < z.cfg.MaxDuty
	if wait := z.retryAt.Sub(now); z.failures > 0 && !urgent && wait > 0 {
		return wait
	}
	start := time.Now()
	if err := z.ipmi.SetDuty(ctx, z.cfg.IpmiZone, o.duty); err != nil {
		if ctx.Err() != nil {
			return 0 // shutting down
		}
		log.Printf("zone %s: %v", z.cfg.Name, err)
		z.failures++
		// From the end of the attempt: a hung call took its whole timeout.
		z.retryAt = now.Add(time.Since(start) + retryDelay(z.failures))
		z.failedDuty = o.duty
		return retryDelay(z.failures) // the writer's timer starts after the call
	}
	z.failures = 0
	if changed {
		log.Printf("zone %s: %d%% (target %d%%) [%s]", z.cfg.Name, o.duty, o.target, strings.Join(o.parts, " "))
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

// evalTick is how often the loop evaluates between polls, so holds and
// ramp-downs move on time even in a zone whose sources poll every minute.
// Evaluating is cheap: the writer only talks to the BMC when it must.
const evalTick = time.Second

func (z *zoneCtl) loop(ctx context.Context) {
	z.done = make(chan struct{}, 1)
	z.kick = make(chan struct{}, 1)
	z.wg.Add(1)
	go func() { defer z.wg.Done(); z.writer(ctx) }()
	defer z.waitWorkers(shutdownWait)
	z.evaluate(ctx, z.now()) // startup duty at once
	z.startPolling(ctx)
	tick := z.tick
	if tick == 0 {
		tick = evalTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-z.done: // a poll landed, or one is stuck
		}
		// Not the tick's own timestamp: a buffered tick can be older than a
		// wakeup already handled, and time must not go backwards for the ramp.
		if ctx.Err() != nil {
			return // woken at shutdown: no more writes
		}
		z.evaluate(ctx, z.now())
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
