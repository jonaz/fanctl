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
	return exec.CommandContext(ctx, name, args...).Output()
}
