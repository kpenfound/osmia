package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
)

const readinessEnv = "OSMIA_SERVICE_READY_FD"

// detachedCommand is the process boundary; tests launch an idle fake service.
var detachedCommand = func(root string) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(executable, "serve", "--root", root), nil
}

func runService(ctx context.Context, root config.Root, detached bool, stdout io.Writer) error {
	if detached {
		return detach(ctx, root, stdout)
	}
	ready := os.Getenv(readinessEnv) == "3"
	os.Unsetenv(readinessEnv)
	var pipe *os.File
	if ready {
		pipe = os.NewFile(3, "service-readiness")
		defer pipe.Close()
	}
	s, err := service.Start(ctx, service.Enforce(service.Options{Config: config.Options{Root: root.String()}, Build: build()}, enforcement()))
	if err != nil {
		return err
	}
	if pipe != nil {
		if _, err := io.WriteString(pipe, "ready\n"); err != nil {
			return errors.Join(err, s.Close())
		}
		pipe.Close()
	}
	return s.Wait()
}

// detach acknowledges startup only through the child's private readiness pipe.
// Root ownership and socket recovery remain the service's responsibility.
func detach(ctx context.Context, root config.Root, stdout io.Writer) error {
	if err := os.MkdirAll(root.String(), 0700); err != nil {
		return err
	}
	logPath := filepath.Join(root.String(), "service.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	info, err := log.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("service.log must be a private regular file")
	}
	read, write, err := os.Pipe()
	if err != nil {
		return err
	}
	defer read.Close()
	defer write.Close()
	cmd, err := detachedCommand(root.String())
	if err != nil {
		return err
	}
	cmd.Env = nil
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, readinessEnv+"=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, readinessEnv+"=3")
	cmd.ExtraFiles = []*os.File{write}
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	write.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	ready := make(chan bool, 1)
	go func() {
		var data [6]byte
		_, err := io.ReadFull(read, data[:])
		ready <- err == nil && string(data[:]) == "ready\n"
	}()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	stop := func() { cmd.Process.Kill(); <-exited }
	select {
	case ok := <-ready:
		if !ok {
			stop()
			return errors.New("detached service did not become ready")
		}
		fmt.Fprintf(stdout, "Osmia is ready; log: %s\nStop with osmia stop --root %s\n", logPath, root.String())
		return nil
	case err := <-exited:
		return fmt.Errorf("detached service exited before readiness: %w", err)
	case <-ctx.Done():
		stop()
		return ctx.Err()
	case <-timer.C:
		stop()
		return errors.New("detached service readiness timed out")
	}
}
