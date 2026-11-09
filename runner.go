package main

import (
	"context"
	"errors"
	"os"
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

// Run runs the command in a process group of its own, so a child it forked
// can neither hang the read nor outlive it: the group is killed on timeout,
// and after a child kept the output pipes open past the command's exit.
// The group is only signalled while its id is known to be in use (the
// command unreaped, or a child just seen holding the pipes), never after
// it may have been freed and reused. (A process stuck in the kernel can
// still hang Wait; the controller's stuck-read check covers that.)
//
// exec.ErrWaitDelay is returned as is: the pipes were closed before EOF,
// so the output may be incomplete and must not be trusted.
func (r execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Wait may already have reaped the command (it does so before it
		// looks at the context); then its pid no longer names our group.
		if err := cmd.Process.Signal(syscall.Signal(0)); errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = cmdWaitDelay
	if r.waitDelay != 0 {
		cmd.WaitDelay = r.waitDelay
	}
	out, err := cmd.Output()
	if errors.Is(err, exec.ErrWaitDelay) {
		// A child held the pipes until just now, so the group still has a
		// member and its id can't have been reused: kill what is left.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return out, err
}
