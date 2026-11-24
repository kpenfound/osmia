package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/engine"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/service"
)

const readinessEnv = "OSMIA_SERVICE_READY_FD"

const detachedReadinessTimeout = engine.ActionTimeout + 30*time.Second

var detachedCommand = func(root string) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if root == "" {
		return exec.Command(executable, "serve"), nil
	}
	return exec.Command(executable, "serve", "--root", root), nil
}

func rootFlag(root config.Root) string {
	if root.SelfContained() {
		return root.String()
	}
	return ""
}

var baseServiceOptions = func(root config.Root, logger *slog.Logger) service.Options {
	return service.Options{Config: root.Options(""), Build: build(), Logger: logger}
}

func runService(ctx context.Context, root config.Root, detached bool, stdout io.Writer, logger *slog.Logger) error {
	if err := os.MkdirAll(root.String(), 0700); err != nil {
		return err
	}
	if detached {
		return detach(ctx, root, stdout)
	}
	restore := installGlobalLog(logger)
	defer restore()
	ready := os.Getenv(readinessEnv) == "3"
	os.Unsetenv(readinessEnv)
	var pipe *os.File
	if ready {
		pipe = os.NewFile(3, "service-readiness")
		defer pipe.Close()
	}
	s, err := service.Start(ctx, service.Enforce(baseServiceOptions(root, logger), enforcement(root)))
	if err != nil {
		return err
	}
	if pipe != nil {
		if _, err := io.WriteString(pipe, "ready\n"); err != nil {
			return errors.Join(err, s.Close())
		}
		pipe.Close()
	}
	if err := s.Wait(); err != nil {
		return stoppedError{err}
	}
	return nil
}

type stoppedError struct{ err error }

func (e stoppedError) Error() string { return e.err.Error() }
func (e stoppedError) Unwrap() error { return e.err }

func reportServeFailure(stderr io.Writer, err error, at time.Time) {
	if stopped := (stoppedError{}); errors.As(err, &stopped) {
		fmt.Fprintf(stderr, "%s service stopped after a failure: %s\n", at.Format(time.RFC3339), failureText(stopped.err))
		fmt.Fprintln(stderr, "fix the cause above, then start the service again")
		return
	}
	fmt.Fprintln(stderr, "service startup failed; run osmia doctor to find the cause, and stop any existing owner before starting another")
}

func logServeFailure(logger *slog.Logger, err error) {
	if stopped := (stoppedError{}); errors.As(err, &stopped) {
		logger.Error("service stopped after a failure", "error", failureText(stopped.err), "guidance", "fix the cause above, then start the service again")
		return
	}
	logger.Error("service startup failed", "guidance", "run osmia doctor to find the cause, and stop any existing owner before starting another")
}

func newServiceLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(redactingWriter{w}, nil))
}

var globalLogMu sync.Mutex

func installGlobalLog(logger *slog.Logger) (restore func()) {
	if !globalLogMu.TryLock() {
		return func() {}
	}
	previousDefault := slog.Default()
	previousOutput := log.Writer()
	previousFlags := log.Flags()
	slog.SetDefault(logger)
	return func() {
		slog.SetDefault(previousDefault)
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
		globalLogMu.Unlock()
	}
}

type redactingWriter struct{ w io.Writer }

func (r redactingWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(r.w, redact.Secrets(string(p), redact.KnownSecrets()...)); err != nil {
		return 0, err
	}
	return len(p), nil
}

var credentialURL = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@]+@`)

const maxFailureText = 4000

func failureText(err error) string {
	text := credentialURL.ReplaceAllString(err.Error(), "${1}[redacted]@")
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		text = strings.ReplaceAll(text, token, "[redacted]")
	}
	text = strings.ReplaceAll(text, "\n", "; ")
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	if len(text) > maxFailureText {
		cut := maxFailureText
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}

func detach(ctx context.Context, root config.Root, stdout io.Writer) error {
	return detachWithin(ctx, root, stdout, detachedReadinessTimeout)
}

func detachWithin(ctx context.Context, root config.Root, stdout io.Writer, timeout time.Duration) error {
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
	cmd, err := detachedCommand(rootFlag(root))
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
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	stop := func() { cmd.Process.Kill(); <-exited }
	select {
	case ok := <-ready:
		if !ok {
			stop()
			return errors.New("detached service did not become ready")
		}
		command := "osmia stop"
		if flag := rootFlag(root); flag != "" {
			command += " --root " + flag
		}
		fmt.Fprintf(stdout, "Osmia is ready; log: %s\nStop with %s\n", logPath, command)
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
