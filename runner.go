package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

// Runner runs external commands; swapped out in tests.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// errCutShort marks a run whose output may be incomplete: the command was
// killed, or something it left behind held stdout past the wait delay.
// Callers must not use that output, even if it happens to parse.
var errCutShort = errors.New("output cut short")

// cmdWaitDelay bounds how long a command's output is waited for after it
// exits or is killed, in case a child it forked still holds stdout.
const cmdWaitDelay = time.Second

type execRunner struct {
	timeout   time.Duration
	waitDelay time.Duration // 0 = cmdWaitDelay; tests shorten it
}

// Run runs the command in a process group of its own, so a child it forked
// can neither hang the read nor outlive it: the group is killed on timeout,
// and once the command exits (any child still running is a leftover).
//
// Both kills happen while the command has exited at most as a zombie, not
// yet reaped: it is only reaped (cmd.Wait) after them, so the group id
// is still ours and the kill can't reach anyone else's group. (A process
// stuck in the kernel can still hang the run; the controller's stuck-read
// check covers that.)
//
// stdout is a pipe of our own rather than os/exec's, so a cut-short run is
// told apart from a plain non-zero exit (which smartctl's exit bitmask
// makes routine): it returns an error wrapping errCutShort.
func (r execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	waitDelay := r.waitDelay
	if waitDelay == 0 {
		waitDelay = cmdWaitDelay
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	cmd := exec.Command(name, args...)
	cmd.Stdout = pw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	pw.Close() // the child has its own copy
	if err != nil {
		return nil, err
	}
	pgid := cmd.Process.Pid

	var out bytes.Buffer
	eof := make(chan struct{})
	go func() { _, _ = out.ReadFrom(pr); close(eof) }()
	exited := make(chan struct{})
	go func() { waitExited(pgid); close(exited) }()

	var cut error
	select {
	case <-exited:
	case <-ctx.Done():
		cut = fmt.Errorf("%w: killed: %w", errCutShort, ctx.Err())
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-exited
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL) // leftover children
	werr := cmd.Wait()

	t := time.NewTimer(waitDelay)
	defer t.Stop()
	select {
	case <-eof:
	case <-t.C: // a child out of reach (stuck in the kernel) holds stdout
		pr.Close()
		<-eof
		if cut == nil {
			cut = fmt.Errorf("%w: %w", errCutShort, exec.ErrWaitDelay)
		}
	}
	if cut != nil {
		return out.Bytes(), cut
	}
	return out.Bytes(), werr
}

// waitExited blocks until process pid has exited, without reaping it
// (waitid with WNOWAIT, as os.Process does internally).
func waitExited(pid int) {
	var info [128]byte // siginfo_t
	for {
		_, _, e := syscall.Syscall6(syscall.SYS_WAITID, 1 /* P_PID */, uintptr(pid),
			uintptr(unsafe.Pointer(&info)), syscall.WEXITED|syscall.WNOWAIT, 0, 0)
		if e != syscall.EINTR {
			return
		}
	}
}
