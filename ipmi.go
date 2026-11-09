package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// IPMI sets Supermicro X10/X11 fan mode and per-zone duty via ipmitool raw.
type IPMI struct {
	run    Runner
	bin    string
	dryRun bool

	// One raw command at a time: the BMC dislikes concurrent ones. A
	// channel, not a mutex, so waiting can give up after lockWait: an
	// ipmitool stuck in the kernel must not keep shutdown from restoring
	// the fan mode, nor hang the other zone's writer for good.
	busy     chan struct{}
	lockWait time.Duration
}

// ipmiLockWait is how long a raw command waits for the one before it: as
// long as that one can legitimately hold the lock (its timeout, plus the
// wait for its output), so a slow but working call doesn't fail the next.
const ipmiLockWait = ipmiTimeout + cmdWaitDelay

func newIPMI(run Runner, bin string, dryRun bool) *IPMI {
	return &IPMI{run: run, bin: bin, dryRun: dryRun, busy: make(chan struct{}, 1), lockWait: ipmiLockWait}
}

func (i *IPMI) raw(ctx context.Context, bytes ...int) error {
	args := []string{"raw"}
	for _, b := range bytes {
		args = append(args, fmt.Sprintf("0x%02x", b))
	}
	if i.dryRun {
		log.Printf("dry-run: %s %s", i.bin, strings.Join(args, " "))
		return nil
	}
	wait, cancel := context.WithTimeout(ctx, i.lockWait)
	defer cancel()
	select {
	case i.busy <- struct{}{}:
	case <-wait.Done():
		return fmt.Errorf("%s %s: waiting for an earlier ipmitool: %w", i.bin, strings.Join(args, " "), wait.Err())
	}
	defer func() { <-i.busy }()
	out, err := i.run.Run(ctx, i.bin, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %w (%s)", i.bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SetMode: 0 standard, 1 full, 2 optimal, 4 heavy IO.
func (i *IPMI) SetMode(ctx context.Context, mode int) error {
	return i.raw(ctx, 0x30, 0x45, 0x01, mode)
}

// SetDuty sets a zone's duty cycle in percent. Only sticks in Full mode.
func (i *IPMI) SetDuty(ctx context.Context, zone, duty int) error {
	return i.raw(ctx, 0x30, 0x70, 0x66, 0x01, zone, duty)
}
