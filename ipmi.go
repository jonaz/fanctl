package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
)

// IPMI sets Supermicro X10/X11 fan mode and per-zone duty via ipmitool raw.
type IPMI struct {
	run    Runner
	bin    string
	dryRun bool
	mu     sync.Mutex // the BMC dislikes concurrent raw commands
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
	i.mu.Lock()
	defer i.mu.Unlock()
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
