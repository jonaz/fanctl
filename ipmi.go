package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// BMC is what the zones need from the board: Supermicro X10/X11 fan mode
// and per-zone duty.
type BMC interface {
	SetMode(ctx context.Context, mode int) error
	SetDuty(ctx context.Context, zone, duty int) error
}

// transport sends one raw IPMI request to the BMC and fails unless the BMC
// completes it. The backends are ipmitool and the kernel device /dev/ipmi0.
type transport interface {
	name() string // for logs: "ipmitool", "/dev/ipmi0"
	raw(ctx context.Context, netfn, cmd byte, data []byte) error
}

// IPMI implements BMC on top of a transport, with what both backends share:
// the Supermicro OEM commands, one command at a time, and dry-run logging.
type IPMI struct {
	t      transport // nil only in dry-run, where it is never used
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
// wait for ipmitool's output), so a slow but working call doesn't fail the
// next.
const ipmiLockWait = ipmiTimeout + cmdWaitDelay

func newIPMI(t transport, dryRun bool) *IPMI {
	return &IPMI{t: t, dryRun: dryRun, busy: make(chan struct{}, 1), lockWait: ipmiLockWait}
}

// rawString formats a request the way ipmitool takes it: "raw 0x30 0x45 ...".
func rawString(netfn, cmd byte, data []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "raw 0x%02x 0x%02x", netfn, cmd)
	for _, d := range data {
		fmt.Fprintf(&b, " 0x%02x", d)
	}
	return b.String()
}

func (i *IPMI) raw(ctx context.Context, netfn, cmd byte, data ...byte) error {
	req := rawString(netfn, cmd, data)
	if i.dryRun {
		log.Printf("dry-run: %s", req) // the same bytes whichever backend
		return nil
	}
	wait, cancel := context.WithTimeout(ctx, i.lockWait)
	defer cancel()
	select {
	case i.busy <- struct{}{}:
	case <-wait.Done():
		return fmt.Errorf("%s %s: waiting for an earlier IPMI command: %w", i.t.name(), req, wait.Err())
	}
	defer func() { <-i.busy }()
	// select picks at random when both are ready: a cancelled call may
	// still have got the lock, and must not then reach the BMC.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s %s: %w", i.t.name(), req, err)
	}
	if err := i.t.raw(ctx, netfn, cmd, data); err != nil {
		return fmt.Errorf("%s %s: %w", i.t.name(), req, err)
	}
	return nil
}

// SetMode: 0 standard, 1 full, 2 optimal, 4 heavy IO.
func (i *IPMI) SetMode(ctx context.Context, mode int) error {
	return i.raw(ctx, 0x30, 0x45, 0x01, byte(mode))
}

// SetDuty sets a zone's duty cycle in percent. Only sticks in Full mode.
func (i *IPMI) SetDuty(ctx context.Context, zone, duty int) error {
	return i.raw(ctx, 0x30, 0x70, 0x66, 0x01, byte(zone), byte(duty))
}

// ipmitool is the transport that shells out to `ipmitool raw`.
type ipmitool struct {
	run Runner
	bin string
}

func (t ipmitool) name() string { return t.bin }

func (t ipmitool) raw(ctx context.Context, netfn, cmd byte, data []byte) error {
	args := strings.Fields(rawString(netfn, cmd, data))
	out, err := t.run.Run(ctx, t.bin, args...)
	if err != nil {
		return fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// completionError is a BMC response whose completion code isn't 0x00.
type completionError byte

var completionCodes = map[byte]string{
	0xc0: "node busy",
	0xc1: "invalid command",
	0xc2: "invalid command for LUN",
	0xc3: "timeout",
	0xc4: "out of space",
	0xc5: "reservation cancelled",
	0xc6: "request data truncated",
	0xc7: "request data length invalid",
	0xc8: "request data field length limit exceeded",
	0xc9: "parameter out of range",
	0xca: "cannot return requested number of bytes",
	0xcb: "requested sensor, data or record not present",
	0xcc: "invalid data field in request",
	0xcd: "command illegal for sensor or record type",
	0xce: "response could not be provided",
	0xcf: "duplicated request",
	0xd0: "SDR repository in update mode",
	0xd1: "device in firmware update mode",
	0xd2: "BMC initialization in progress",
	0xd3: "destination unavailable",
	0xd4: "insufficient privilege level",
	0xd5: "not supported in present state",
	0xd6: "parameter is illegal because sub-function is disabled",
	0xff: "unspecified error",
}

func (c completionError) Error() string {
	if s, ok := completionCodes[byte(c)]; ok {
		return fmt.Sprintf("completion code 0x%02x (%s)", byte(c), s)
	}
	return fmt.Sprintf("completion code 0x%02x", byte(c))
}
