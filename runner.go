package main

import (
	"context"
	"os/exec"
	"time"
)

// Runner runs external commands; swapped out in tests.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{ timeout time.Duration }

func (r execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// After the kill, don't wait forever for pipes a forked child still
	// holds open. (A process stuck in the kernel can still hang Wait; the
	// controller's stuck-read check covers that.)
	cmd.WaitDelay = time.Second
	return cmd.Output()
}
