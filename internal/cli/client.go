package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/kpenfound/osmia/internal/fleet/client"
)

const clientUsage = `Usage: osmia client --server URL --slots N [--dir PATH]
  --server URL  the server's tailnet API, such as https://osmia.example.ts.net
  --slots N     how many turns this machine runs at once
  --dir PATH    where the client keeps its clones, sessions and unsent files
                (default: osmia-client under the machine's user data directory)
osmia client runs in the foreground and takes flags only: it reads no config file
and has no background mode. Keep it running with the machine's own tools.
The first Ctrl+C or SIGTERM drains it: it takes no new turns and exits once its
running turns are sent. A second cancels them and still sends their files.
`

type clientOptions struct {
	server, dir string
	slots       int
	help        bool
}

// foregroundOnly is why a client refuses a flag that asks for a background run
// or a configuration file.
func foregroundOnly(flag string) error {
	return fmt.Errorf("osmia client runs in the foreground and takes flags only; %s is not supported (it has no background mode and reads no config file)", flag)
}

func parseClient(args []string) (clientOptions, error) {
	var o clientOptions
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		key, value, has := strings.Cut(args[i], "=")
		if !strings.HasPrefix(key, "-") || key == "-" {
			return o, fmt.Errorf("unexpected argument %q; osmia client takes flags only", args[i])
		}
		switch key {
		case "--detach", "--detached", "--background", "--daemon", "--daemonize", "--bg", "-d", "-b",
			"--config", "--config-file", "--configfile", "--conf", "-c", "--root", "--profile", "--profiles", "--socket":
			return o, foregroundOnly(key)
		case "--help", "-h":
			o.help = true
			continue
		case "--server", "--slots", "--dir":
		default:
			return o, fmt.Errorf("unknown flag %s", key)
		}
		if seen[key] {
			return o, fmt.Errorf("duplicate flag %s", key)
		}
		seen[key] = true
		if !has {
			i++
			if i >= len(args) {
				return o, fmt.Errorf("%s needs a value", key)
			}
			value = args[i]
		}
		if value == "" {
			return o, fmt.Errorf("%s needs a value", key)
		}
		switch key {
		case "--server":
			o.server = value
		case "--dir":
			o.dir = value
		case "--slots":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return o, errors.New("--slots must be a whole number of at least 1")
			}
			o.slots = n
		}
	}
	return o, nil
}

// clientDirectory is where a client keeps its files when --dir is not given.
func clientDirectory() (string, error) {
	if runtime.GOOS != "darwin" {
		if data := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(data) {
			return filepath.Join(data, "osmia-client"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "osmia-client"), nil
	}
	return filepath.Join(home, ".local", "share", "osmia-client"), nil
}

// runClientProcess and clientInterrupts are what a test replaces: the client
// itself, and the Ctrl+C and SIGTERM it is stopped by.
var (
	runClientProcess = client.Run
	clientInterrupts = osInterrupts
)

// osInterrupts delivers one value for each Ctrl+C or SIGTERM until stop is
// called.
func osInterrupts() (<-chan struct{}, func()) {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	out := make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-signals:
				select {
				case out <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	return out, func() { signal.Stop(signals); close(done) }
}

// runClient runs `osmia client`: a foreground client of the server named by
// --server. It reads no configuration, opens no trace and logs to stderr only.
func runClient(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseClient(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, "use osmia client --help")
		return 2
	}
	if o.help {
		fmt.Fprint(stdout, clientUsage)
		return 0
	}
	if o.server == "" || o.slots == 0 {
		fmt.Fprintln(stderr, "osmia client needs --server and --slots; use osmia client --help")
		return 2
	}
	if o.dir == "" {
		if o.dir, err = clientDirectory(); err != nil {
			fmt.Fprintln(stderr, "cannot choose a directory for the client; pass --dir:", err)
			return 2
		}
	}
	interrupts, stop := clientInterrupts()
	defer stop()
	logger := newServiceLogger(stderr)
	err = runClientProcess(context.WithoutCancel(ctx), client.Options{Server: o.server, Slots: o.slots, Dir: o.dir, Interrupts: interrupts, Logger: logger})
	if err != nil {
		logger.Error("the client stopped", "error", err.Error())
		return 1
	}
	return 0
}
