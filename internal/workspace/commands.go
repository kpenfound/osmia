package workspace

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// killWait bounds how long a command that was told to stop is waited on
// before its caller gets control back.
var killWait = 10 * time.Second

// pipeWait bounds the wait for a command's output pipes once it has exited,
// so a descendant that keeps them open does not hold Wait.
const pipeWait = 2 * time.Second

// terminate stops the command and the processes it started.
var terminate = func(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return cmd.Process.Kill()
	}
	return nil
}

// ErrProcessRunning is a command that did not exit after it was told to stop.
var ErrProcessRunning = errors.New("process did not exit after it was stopped")

// ProcessError names a command still running after the context that started
// it ended. The clone it ran under stays locked until it exits.
type ProcessError struct {
	Command string
	PID     int
}

func (e *ProcessError) Error() string {
	return fmt.Sprintf("%s: process %d did not exit after it was stopped; the clone stays locked until it does", e.Command, e.PID)
}
func (e *ProcessError) Is(target error) bool { return target == ErrProcessRunning }

// runCommand starts cmd and waits for it. When ctx ends first it stops the
// command with its process group and waits for the exit, up to killWait. A
// command that is still running then is left to a goroutine that frees the
// clone lock once it exits, and runCommand returns ctx's error joined with a
// ProcessError. A command started under a clone's ownership counts against
// that lock until it exits, so ownership is not released while it runs.
func runCommand(ctx context.Context, lock *cloneLock, cmd *exec.Cmd, name string) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = pipeWait
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if lock != nil {
		lock.started()
	}
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if lock != nil {
			lock.exited()
		}
		exited <- err
	}()
	select {
	case err := <-exited:
		return err
	case <-ctx.Done():
	}
	select {
	case err := <-exited:
		return err
	default:
	}
	_ = terminate(cmd)
	timer := time.NewTimer(killWait)
	defer timer.Stop()
	select {
	case <-exited:
		return ctx.Err()
	case <-timer.C:
		return errors.Join(ctx.Err(), &ProcessError{Command: name, PID: cmd.Process.Pid})
	}
}
