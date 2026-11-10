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

// Concurrency model: each zone's loop goroutine owns every source's state
// and the ramp. It decides when each source is due, starts the read in a
// goroutine of its own and gets the result back on a channel; the read
// goroutine touches nothing else. So there is no lock on source state, at
// most one live read per source is ever in flight (plus at most one stuck
// one it gave up on), and every result is evaluated as it lands. Fan writes run in a separate writer goroutine
// (a hung BMC must not stall polling), fed through a mailbox of one.
//
// Time: everything reads time.Now(). Tests run under testing/synctest, so
// they get the same code paths on a virtual clock.

type sourceState int

const (
	statePending  sourceState = iota // no good reading yet: zone uses startup_duty
	stateOK                          // demand comes from the curve
	stateIdle                        // nothing to measure (all disks asleep): no demand
	stateFailsafe                    // fail_after errors in a row, or a stuck read: max_duty
)

// source is a sensor plus the state the zone loop keeps for it. Only the
// loop goroutine (or step, inline) touches these fields.
type source struct {
	cfg    Source
	sensor Sensor

	avg      window
	failures int
	state    sourceState
	demand   float64 // duty from readings: the curve, or a partial reading's lower bound
	held     float64 // temperature after hysteresis
	hasHeld  bool
	frag     string // last reading, for logs

	// For metrics, also loop-owned; publish copies them.
	label      string  // sensor name, numbered if the zone has several of a type
	temp       float64 // latest temperature, if hasTemp
	hasTemp    bool
	reads      uint64 // completed reads
	readErrors uint64 // failed or partial
	stuckReads uint64
	failsafes  uint64
	readTime   time.Duration // the latest completed read

	// Poll scheduling, also loop-owned.
	// A read is stuck once it is in flight past stuckAt; that is derived,
	// never stored, so it can't drift out of step with these.
	next      time.Time // when the next poll is due; zero = at once
	inFlight  bool
	startedAt time.Time
	gen       int                // numbers reads, so an abandoned one's result is known
	cancel    context.CancelFunc // cancels the in-flight read
	abandoned bool               // a stuck read given up on is still running
}

// apply folds one read into the source's state. Pure apart from logging:
// no I/O, no clock, no locks.
//
//	read          | effect
//	--------------+--------------------------------------------------------
//	failed (err)  | keep last demand; ends idle (back to startup floor)
//	partial       | may only raise demand; ends idle; never releases startup;
//	              | when OK and hotter than held, also enters average and hold
//	idle          | no demand
//	ok            | demand from the curve, after smoothing and hysteresis
//
// Failed and partial reads count towards fail_after; once failed safe, only
// a good (idle or ok) read leaves it, so the logged reason is kept.
func (s *source) apply(r Reading, err error) {
	name := s.sensor.Name()
	problem := err
	if problem == nil {
		problem = r.Partial
	}
	s.reads++
	if problem != nil {
		s.readErrors++
		s.failures++
		log.Printf("%s: read error (%d/%d): %v", name, s.failures, s.cfg.FailAfter, problem)
	} else {
		s.failures = 0
	}
	if s.failures >= s.cfg.FailAfter {
		s.failsafe(name + "=FAILSAFE")
		return
	}
	switch {
	case problem != nil && s.state == stateFailsafe:
		// Still failed safe (e.g. a stuck read returning an error).
	case err != nil:
		// No data: keep the previous demand; fail_after bounds how long.
		s.endIdle()
		s.hasTemp = false
		s.frag = name + "=err"
	case r.Partial != nil:
		// The unreadable part could be the hottest: only raise. A reading
		// above what is held also enters the average and the hold, so the
		// next full reading carries on from it rather than from before.
		s.endIdle()
		if s.state == stateOK && r.Temp > s.held {
			s.held = math.Max(s.held, s.avg.add(r.Temp))
		}
		s.demand = math.Max(s.demand, Interpolate(s.cfg.Curve, r.Temp))
		s.temp, s.hasTemp = r.Temp, true
		s.frag = fmt.Sprintf("%s=%.1f°C→%d%%(partial)", name, r.Temp, int(math.Round(s.demand)))
	case r.Idle:
		s.state, s.demand = stateIdle, 0
		s.avg.reset()
		s.hasHeld = false
		s.hasTemp = false
		s.frag = fmt.Sprintf("%s=idle(%s)", name, r.Detail)
	default:
		t := s.avg.add(r.Temp)
		// Hysteresis: follow rises at once, follow drops only once they
		// exceed the hysteresis. Stops 40↔41 °C flicker moving the fans.
		if s.hasHeld && t < s.held && s.held-t <= s.cfg.Hysteresis {
			t = s.held
		}
		s.held, s.hasHeld = t, true
		s.state, s.demand = stateOK, Interpolate(s.cfg.Curve, t)
		s.temp, s.hasTemp = r.Temp, true
		s.frag = fmt.Sprintf("%s=%.1f°C→%d%%", name, t, int(math.Round(s.demand)))
		if r.Detail != "" {
			s.frag += "(" + r.Detail + ")"
		}
	}
}

// endIdle: "nothing to measure" no longer holds once a read fails (a disk
// that just stopped answering may be hot), so back to the startup floor.
func (s *source) endIdle() {
	if s.state == stateIdle {
		s.state, s.demand = statePending, 0
	}
}

// failsafe demands max_duty until a good reading. Unlike the startup
// floor this is real demand: the fans hold and ramp down from max_duty
// after recovery, so a flaky sensor can't make them hunt. The average and
// hysteresis are cleared, so the first reading after recovery isn't mixed
// with pre-failure ones.
func (s *source) failsafe(frag string) {
	if s.state != stateFailsafe {
		s.failsafes++
	}
	s.state, s.frag = stateFailsafe, frag
	s.avg.reset()
	s.hasHeld = false
	s.hasTemp = false
}

// want is what this source asks of zone z: real demand (which the ramp
// holds and ramps down from) and a floor (startup duty while pending,
// which never enters the ramp).
func (s *source) want(z *Zone) (real, floor float64) {
	switch s.state {
	case stateFailsafe:
		return float64(z.MaxDuty), 0
	case stateIdle:
		return 0, 0
	case statePending:
		return s.demand, float64(*z.StartupDuty)
	default:
		return s.demand, 0
	}
}

// stuckAt is when the in-flight read counts as stuck. checkStuck and
// nextWake both use it, so the loop always wakes when a read turns stuck.
func (s *source) stuckAt() time.Time { return s.startedAt.Add(s.stuckLimit()) }

// stuckLimit is the longest one read of this source can legitimately take.
func (s *source) stuckLimit() time.Duration {
	if b, ok := s.sensor.(interface{ maxReadTime() time.Duration }); ok {
		return b.maxReadTime() // e.g. disks: batches of smartctl
	}
	return readBudget(1)
}

// ramp is the zone's real duty: up at once, down only after holding for
// delay since the drop was first seen, then at most rate % per minute.
// Pure: the caller supplies the time.
type ramp struct {
	level  float64
	dropAt time.Time // when target first fell below level; zero = not ramping
	last   time.Time // previous step; zero = never stepped
}

func (r *ramp) step(now time.Time, target float64, delay time.Duration, rate float64) float64 {
	if now.Before(r.last) {
		now = r.last // never let the ramp see time run backwards
	}
	switch {
	case r.last.IsZero() || target >= r.level:
		r.level, r.dropAt = target, time.Time{}
	default:
		if r.dropAt.IsZero() {
			r.dropAt = now
		}
		if now.Sub(r.dropAt) < delay {
			break // hold
		}
		from := r.dropAt.Add(delay)
		if r.last.After(from) {
			from = r.last
		}
		r.level = math.Max(target, r.level-rate*now.Sub(from).Minutes())
	}
	r.last = now
	return r.level
}

// order is what evaluate wants on the fans.
type order struct {
	valid  bool
	duty   int
	target int      // where the ramp is heading
	parts  []string // per-source readings, for the log
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

// bmcState is what the writer knows about the fans. Its methods are pure:
// the caller does the write and supplies the time.
type bmcState struct {
	duty       int // last duty the BMC accepted, -1 = none yet
	lastSent   time.Time
	failures   int       // failed writes in a row
	retryAt    time.Time // when the last failed write may be retried
	failedDuty int       // what it tried to send
}

// decide says whether o should be sent now, or else how long until it is
// worth looking again (0 = only when a new order comes).
//
// A duty is sent when it moves at least min_change, hits min or max,
// settles on the target at the end of a ramp-down, or reapply is due, all
// keyed on the last duty the BMC accepted, so a failed write is retried.
// After a failure it waits out the backoff, except to jump to max_duty.
func (b *bmcState) decide(o order, z *Zone, reapply time.Duration, now time.Time) (send bool, wait time.Duration) {
	if !o.valid {
		return false, 0
	}
	diff := o.duty - b.duty
	if diff < 0 {
		diff = -diff
	}
	changed := o.duty != b.duty
	reapplyIn := b.lastSent.Add(reapply).Sub(now)
	worth := b.duty < 0 || reapplyIn <= 0 ||
		(changed && (diff >= z.MinChange ||
			o.duty == z.MaxDuty || o.duty == z.MinDuty ||
			(o.duty < b.duty && o.duty == o.target)))
	if !worth {
		return false, reapplyIn
	}
	urgent := o.duty == z.MaxDuty && b.failedDuty < z.MaxDuty
	if wait := b.retryAt.Sub(now); b.failures > 0 && !urgent && wait > 0 {
		return false, wait
	}
	return true, 0
}

// sent records a successful write at now.
func (b *bmcState) sent(duty int, now time.Time) {
	b.duty, b.lastSent, b.failures = duty, now, 0
}

// failed records a failed write that ended at now, and returns the backoff.
func (b *bmcState) failed(duty int, now time.Time) time.Duration {
	b.failures++
	d := retryDelay(b.failures)
	b.retryAt, b.failedDuty = now.Add(d), duty
	return d
}

// zoneCtl runs one fan zone; see the concurrency model at the top.
type zoneCtl struct {
	cfg     Zone
	sources []*source
	ipmi    BMC
	reapply time.Duration
	tick    time.Duration  // longest the loop sleeps without evaluating; 0 = evalTick
	wg      sync.WaitGroup // reads and the writer

	ramp     ramp      // owned by the loop
	lastEval time.Time // owned by the loop: when evaluate last ran

	// The mailbox from evaluate to the writer.
	mu     sync.Mutex
	posted order
	kick   chan struct{} // wakes the writer goroutine; nil = write inline

	bmc bmcState // owned by the writer

	metrics *zoneMetrics // has its own lock; see metrics.go
}

func newZoneCtl(cfg Zone, ipmi BMC, reapply time.Duration) *zoneCtl {
	return &zoneCtl{cfg: cfg, ipmi: ipmi, reapply: reapply, bmc: bmcState{duty: -1}, metrics: newZoneMetrics(cfg.Name)}
}

func (z *zoneCtl) addSource(s *source) {
	name := s.sensor.Name()
	s.frag = name + "=pending" // until the first poll
	// Two sources of one type (say, disks with different curves) need
	// telling apart in metrics: the second is "disks_2".
	n := 1
	for _, o := range z.sources {
		if o.sensor.Name() == name {
			n++
		}
	}
	s.label = name
	if n > 1 {
		s.label = fmt.Sprintf("%s_%d", name, n)
	}
	z.sources = append(z.sources, s)
}

// step reads every source once, inline, then evaluates and writes inline:
// --once and tests.
func (z *zoneCtl) step(ctx context.Context) {
	for _, s := range z.sources {
		start := time.Now()
		r, err := s.sensor.Read(ctx)
		if ctx.Err() != nil {
			return // a cancelled read is not a sensor failure
		}
		s.readTime = time.Since(start)
		s.apply(r, err)
	}
	z.evaluate(ctx, time.Now())
}

// evaluate moves the fans toward the highest demand: the ramp follows real
// demand, and the startup floor sits on top of it, so entering startup can
// only raise the fans and leaving it goes straight to wherever the ramp
// has got to.
func (z *zoneCtl) evaluate(ctx context.Context, now time.Time) {
	real, floor := float64(z.cfg.MinDuty), 0.0
	parts := make([]string, 0, len(z.sources))
	for _, s := range z.sources {
		r, f := s.want(&z.cfg)
		real, floor = math.Max(real, r), math.Max(floor, f)
		parts = append(parts, s.frag)
	}
	lo, hi := z.cfg.MinDuty, z.cfg.MaxDuty
	level := z.ramp.step(now, float64(clampRound(real, lo, hi)), z.cfg.RampDownDelay.Duration, *z.cfg.RampDownRate)
	o := order{
		valid:  true,
		duty:   clampRound(math.Max(level, floor), lo, hi),
		target: clampRound(math.Max(real, floor), lo, hi),
		parts:  parts,
	}
	z.publish(o)
	z.post(ctx, o)
}

// post hands o to the writer, or writes it inline without one.
func (z *zoneCtl) post(ctx context.Context, o order) {
	z.mu.Lock()
	z.posted = o
	z.mu.Unlock()
	if z.kick == nil {
		z.flush(ctx)
		return
	}
	select {
	case z.kick <- struct{}{}:
	default: // the writer already has a wakeup pending
	}
}

// flush sends the posted duty if bmcState.decide says so, and returns how
// long until it should look again (0 = only when a new duty is posted).
func (z *zoneCtl) flush(ctx context.Context) time.Duration {
	z.mu.Lock()
	o := z.posted
	z.mu.Unlock()
	if ctx.Err() != nil {
		return 0
	}
	send, wait := z.bmc.decide(o, &z.cfg, z.reapply, time.Now())
	if !send {
		return wait
	}
	err := z.ipmi.SetDuty(ctx, z.cfg.IpmiZone, o.duty)
	// The clock is read after the call: a hung call took its whole timeout,
	// and both the backoff and the reapply interval start when it ended.
	now := time.Now()
	if err != nil {
		if ctx.Err() != nil {
			return 0 // shutting down
		}
		log.Printf("zone %s: %v", z.cfg.Name, err)
		z.metrics.wrote(o.duty, err)
		return z.bmc.failed(o.duty, now)
	}
	z.metrics.wrote(o.duty, nil)
	if o.duty != z.bmc.duty {
		log.Printf("zone %s: %d%% (target %d%%) [%s]", z.cfg.Name, o.duty, o.target, strings.Join(o.parts, " "))
	}
	z.bmc.sent(o.duty, now)
	return z.reapply
}

// writer runs flush whenever a new duty is posted, a retry is due or the
// reapply interval comes round.
func (z *zoneCtl) writer(ctx context.Context) {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		if wait := z.flush(ctx); wait > 0 {
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

// shutdownWait bounds how long loop waits for its goroutines on exit. A
// read stuck in the kernel (a disk in D state) never returns, and waiting
// for it would keep main from restoring the BMC fan mode.
const shutdownWait = 2 * time.Second

// evalTick is the longest the loop goes without evaluating, so holds and
// ramp-downs move on time even in a zone whose sources poll every minute.
// Otherwise it evaluates only when a source's state changed.
const evalTick = time.Second

func (z *zoneCtl) evalEvery() time.Duration {
	if z.tick == 0 {
		return evalTick
	}
	return z.tick
}

// minWait is the shortest the loop sleeps between passes.
const minWait = time.Millisecond

// pollResult carries a finished read back to the loop.
type pollResult struct {
	s   *source
	gen int
	r   Reading
	err error
}

func (z *zoneCtl) loop(ctx context.Context) {
	z.kick = make(chan struct{}, 1)
	results := make(chan pollResult)
	z.wg.Add(1)
	go func() { defer z.wg.Done(); z.writer(ctx) }()
	defer z.waitWorkers(shutdownWait)
	timer := time.NewTimer(0)
	defer timer.Stop()
	changed := true // the first pass puts the startup duty on at once
	for {
		// However the loop woke (a tick or a result can race the cancel),
		// a pass never starts at shutdown: no more reads or writes.
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		if z.checkStuck(now) {
			changed = true
		}
		if changed || !now.Before(z.lastEval.Add(z.evalEvery())) {
			z.evaluate(ctx, now)
			z.lastEval, changed = now, false
		}
		for _, s := range z.claimDue(now) {
			rctx, cancel := context.WithCancel(ctx)
			s.gen++
			s.cancel = cancel
			gen := s.gen
			z.wg.Add(1)
			go func() {
				defer z.wg.Done()
				r, err := s.sensor.Read(rctx)
				select {
				case results <- pollResult{s, gen, r, err}:
				case <-ctx.Done():
				}
			}()
		}
		// nextWake is always after now here (TestNextWakeIsAhead); the
		// floor only keeps a future bug from becoming a busy spin.
		timer.Reset(max(z.nextWake(now).Sub(now), minWait))
		select {
		case <-ctx.Done():
		case res := <-results:
			if z.land(ctx, res) {
				changed = true
			}
		case <-timer.C:
		}
	}
}

// land takes a finished read: the live one is applied, an abandoned one
// only frees its slot. Reports whether the source's state may have changed.
func (z *zoneCtl) land(ctx context.Context, res pollResult) bool {
	s := res.s
	if res.gen != s.gen || !s.inFlight {
		// The stuck read given up on: its result is stale. With nothing
		// else in flight, poll again at once.
		s.abandoned = false
		log.Printf("%s: stuck read ended after all", s.sensor.Name())
		if !s.inFlight {
			s.next = time.Time{}
		}
		return false
	}
	s.inFlight = false
	s.cancel()
	if ctx.Err() != nil { // a cancelled read is not a sensor failure
		return false
	}
	s.readTime = time.Since(s.startedAt)
	s.apply(res.r, res.err)
	return true
}

// claimDue marks each source that is due and not already being read as in
// flight from now, advances its schedule, and returns them for the loop to
// read. Polls keep their interval's cadence; one that overran its slot
// runs again as soon as it lands.
func (z *zoneCtl) claimDue(now time.Time) []*source {
	var due []*source
	for _, s := range z.sources {
		if s.inFlight || now.Before(s.next) {
			continue
		}
		s.inFlight, s.startedAt = true, now
		s.next = nextSlot(s.next, now, s.cfg.Interval.Duration)
		due = append(due, s)
	}
	return due
}

// nextSlot is the poll slot after the one due at prev (zero = first poll,
// at now), skipping slots already missed.
func nextSlot(prev, now time.Time, interval time.Duration) time.Time {
	if prev.IsZero() {
		prev = now
	}
	next := prev.Add(interval)
	if !next.After(now) {
		next = now.Add(interval)
	}
	return next
}

// checkStuck fails safe any source whose in-flight read has outlived its
// stuck limit: failures only count when a read returns, and a hung one
// never does. The limit is looked up each time, so disks detected during
// the read extend it. Reports whether any source failed safe.
//
// The stuck read is then given up on (cancelled, and its result ignored if
// it ever comes), so the next poll can start on schedule and the source can
// recover even if that read never returns. Only one at a time: while one
// is still running, a second stuck read is waited for, so a disk that
// hangs every read can't pile up hung processes.
func (z *zoneCtl) checkStuck(now time.Time) bool {
	failed := false
	for _, s := range z.sources {
		if !s.inFlight || now.Before(s.stuckAt()) {
			continue
		}
		// Already failed safe (by this read or by fail_after): it stays so
		// until a read lands, so the logged reason is kept.
		if s.state != stateFailsafe {
			s.failsafe(s.sensor.Name() + "=STUCK")
			log.Printf("%s: read running for %s, failing safe", s.sensor.Name(), now.Sub(s.startedAt).Round(time.Second))
			failed = true
		}
		if !s.abandoned {
			s.stuckReads++
			s.abandoned, s.inFlight = true, false
			if s.cancel != nil {
				s.cancel()
			}
		}
	}
	return failed
}

// nextWake is when the loop must next look: the next poll due, the next
// read that could turn stuck, or the evaluate tick, whichever is first.
// A read already past its stuck deadline has nothing more to wake for
// (one that can't be given up on yet waits for the abandoned one to land).
func (z *zoneCtl) nextWake(now time.Time) time.Time {
	wake := z.lastEval.Add(z.evalEvery())
	for _, s := range z.sources {
		at := s.next
		if s.inFlight {
			if at = s.stuckAt(); !at.After(now) {
				continue
			}
		}
		if at.Before(wake) {
			wake = at
		}
	}
	return wake
}

// waitWorkers waits for running reads and the writer, at most d. Reports
// whether all ended.
func (z *zoneCtl) waitWorkers(d time.Duration) bool {
	done := make(chan struct{})
	go func() { z.wg.Wait(); close(done) }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		log.Printf("zone %s: polls still running at shutdown, not waiting", z.cfg.Name)
		return false
	}
}
