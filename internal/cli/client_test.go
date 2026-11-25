package cli

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/fleet/client"
)

func runClientCommand(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(context.Background(), append([]string{"client"}, args...), strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestClientRunsInTheForegroundAndTakesFlagsOnly(t *testing.T) {
	ran := false
	t.Cleanup(func(old func(context.Context, client.Options) error) func() {
		return func() { runClientProcess = old }
	}(runClientProcess))
	runClientProcess = func(context.Context, client.Options) error { ran = true; return nil }
	for _, flags := range [][]string{{"--detach"}, {"--detached"}, {"--background"}, {"--daemon"}, {"-d"}, {"--bg"},
		{"--config", "client.toml"}, {"--config=client.toml"}, {"--config-file", "client.toml"}, {"--root", "/tmp/osmia"}, {"--profile", "main"}} {
		args := append([]string{"--server", "https://osmia.example.ts.net", "--slots", "2"}, flags...)
		code, out, errOut := runClientCommand(t, args...)
		if code != 2 || !strings.Contains(errOut, "runs in the foreground") || out != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", flags, code, out, errOut)
		}
	}
	for _, args := range [][]string{{}, {"--server", "https://osmia.example.ts.net"}, {"--slots", "2"}, {"--server", "x", "--slots", "0"},
		{"--server", "x", "--slots", "many"}, {"--server", "x", "--slots", "1", "extra"}, {"--server", "x", "--slots", "1", "--unknown"}, {"--server", "x", "--server", "y", "--slots", "1"}} {
		if code, _, errOut := runClientCommand(t, args...); code != 2 || errOut == "" {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
	if ran {
		t.Fatal("a refused command ran a client")
	}
}

func TestClientPassesItsFlagsToTheClientAndLogsToStderrOnly(t *testing.T) {
	var got client.Options
	t.Cleanup(func(run func(context.Context, client.Options) error, interrupts func() (<-chan struct{}, func())) func() {
		return func() { runClientProcess, clientInterrupts = run, interrupts }
	}(runClientProcess, clientInterrupts))
	stopped := false
	runClientProcess = func(_ context.Context, o client.Options) error {
		got = o
		o.Logger.Info("connected")
		return nil
	}
	clientInterrupts = func() (<-chan struct{}, func()) { return make(chan struct{}), func() { stopped = true } }
	dir := t.TempDir()
	code, out, errOut := runClientCommand(t, "--server=https://osmia.example.ts.net", "--slots", "3", "--dir", dir)
	if code != 0 || out != "" || !strings.Contains(errOut, "connected") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got.Server != "https://osmia.example.ts.net" || got.Slots != 3 || got.Dir != dir || got.Interrupts == nil || !stopped {
		t.Fatalf("options %+v, interrupts stopped %v", got, stopped)
	}
	runClientProcess = func(context.Context, client.Options) error { return os.ErrClosed }
	if code, out, errOut := runClientCommand(t, "--server", "https://x", "--slots", "1", "--dir", dir); code != 1 || out != "" || errOut == "" {
		t.Fatalf("a client that cannot run: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestClientHelpAndUsageNameTheCommand(t *testing.T) {
	code, out, _ := runClientCommand(t, "--help")
	if code != 0 || !strings.Contains(out, "--server URL --slots N") || !strings.Contains(out, "foreground") {
		t.Fatalf("exit %d: %q", code, out)
	}
	var usageOut bytes.Buffer
	if Run(context.Background(), []string{"--help"}, strings.NewReader(""), &usageOut, &bytes.Buffer{}) != 0 || !strings.Contains(usageOut.String(), "client --server URL --slots N") {
		t.Fatalf("the usage does not list the client: %q", usageOut.String())
	}
}

func TestTheReleaseShipsNoUnitFiles(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the module root is not at %s: %v", root, err)
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "node_modules") {
			return filepath.SkipDir
		}
		switch filepath.Ext(path) {
		case ".service", ".plist", ".socket", ".timer":
			t.Errorf("%s is a unit file; osmia ships none for the client or the server", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
