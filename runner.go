package main

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// Runner runs external commands; swapped out in tests.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// cmdWaitDelay bounds how long a command's output is waited for after it
// exits or is killed, in case a child it forked still holds the pipes.
const cmdWaitDelay = time.Second

type execRunner struct {
	timeout   time.Duration
	waitDelay time.Duration // 0 = cmdWaitDelay; tests shorten it
}

// Run runs the command in a process group of its own, and kills the whole
// group on timeout and once the command is done, so a child it forked can
// neither hang the read nor outlive it. (A process stuck in the kernel can
// still hang Wait; the controller's stuck-read check covers that.)
func (r execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = cmdWaitDelay
	if r.waitDelay != 0 {
		cmd.WaitDelay = r.waitDelay
	}
	out, err := cmd.Output()
	if cmd.Process != nil {
		// Leftover children. The group id can't be reused while any of
		// them lives; with none left this is a harmless ESRCH.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The command itself exited 0; only a child held the pipes.
		err = nil
	}
	return out, err
}
