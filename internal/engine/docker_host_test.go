package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

func TestManagedDockerHost(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"tcp://engine.example.ts.net:2375", "ssh://user@engine.example.ts.net", ""} {
		for _, selector := range []string{"", "default", "named"} {
			if host != "" && selector == "named" {
				continue
			}
			t.Run(host+"/"+selector, func(t *testing.T) {
				o, dir := fakeHost(t)
				get := o.Getenv
				o.Getenv = func(name string) string {
					if name == "DOCKER_HOST" {
						return host
					}
					if name == "DOCKER_CONTEXT" {
						return selector
					}
					return get(name)
				}
				o.Command = func(ctx context.Context, path string, env []string, args ...string) (string, error) {
					found := false
					for _, entry := range env {
						if strings.HasPrefix(entry, "DOCKER_HOST=") {
							found = true
							if entry != "DOCKER_HOST="+host {
								t.Fatalf("host changed: %q", entry)
							}
						}
					}
					if found != (host != "") {
						t.Fatalf("Docker host presence=%t, configured=%q", found, host)
					}
					return command(ctx, path, env, args...)
				}
				c := New(managed("v0.20.5"), o)
				must(t, c.Ensure(t.Context()))
				running(t, dir, "v0.20.5", false)
				must(t, c.Ensure(t.Context()))
				must(t, c.Restart(t.Context()))
				must(t, c.Prune(t.Context()))
				status, err := c.Status(t.Context())
				must(t, err)
				if !status.Reachable || status.DockerHost != displayDockerHost(host) {
					t.Fatalf("%+v", status)
				}
				log := read(t, dir, "environments")
				for _, line := range strings.Split(log, "\n") {
					if !strings.HasSuffix(line, "|"+host+"|"+selector) {
						t.Fatalf("incorrect selectors: %s", log)
					}
				}
				for _, cmd := range []string{"docker|container inspect", "docker|run", "docker|start", "docker|stop", "docker|rm", "dagger|core version", "dagger|core engine local-cache prune"} {
					if !strings.Contains(log, cmd) {
						t.Fatalf("missing %s: %s", cmd, log)
					}
				}
			})
		}
	}
}

func TestDockerSelectorConflict(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"managed", "docker-container://explicit"} {
		o, dir := fakeHost(t)
		o.Getenv = func(name string) string {
			switch name {
			case "DOCKER_HOST":
				return "ssh://user:secret@engine.example.ts.net"
			case "DOCKER_CONTEXT":
				return "named"
			}
			return ""
		}
		c := New(config.DaggerConfig{Engine: mode}.Resolve(), o)
		for _, action := range []func(context.Context) error{c.Ensure, c.Restart, c.Prune} {
			err := action(t.Context())
			if err == nil || !strings.Contains(err.Error(), "DOCKER_HOST") || !strings.Contains(err.Error(), "DOCKER_CONTEXT") {
				t.Fatalf("%v", err)
			}
		}
		s, err := c.Status(t.Context())
		must(t, err)
		if s.Reachable || s.DockerHost != "" || !strings.Contains(s.ReachabilityError, "DOCKER_CONTEXT") {
			t.Fatalf("%+v", s)
		}
		if _, err := os.Stat(filepath.Join(dir, "commands")); !os.IsNotExist(err) {
			t.Fatalf("conflict invoked CLIs: %v", err)
		}
	}
}

func TestDockerHostDisplay(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]string{
		"unix:///var/run/docker.sock":                                      "unix:///var/run/docker.sock",
		"ssh://user:secret@engine.example.ts.net:22/private?secret#secret": "ssh://engine.example.ts.net:22",
		"tcp://engine.example.ts.net:2375":                                 "tcp://engine.example.ts.net:2375",
		"tcp://[fd00::1]:2375":                                             "tcp://[fd00::1]:2375",
		"bad%host":                                                         "", "engine.example.ts.net": "", "": "",
	} {
		if got := displayDockerHost(value); got != want {
			t.Fatalf("%q: %q, want %q", value, got, want)
		}
	}
	for _, mode := range []string{"managed", "docker-container://explicit", "tcp://engine:1234", "unix:///run/engine.sock", "cloud", "auto"} {
		o, _ := fakeHost(t)
		get := o.Getenv
		o.Getenv = func(name string) string {
			if name == "DOCKER_HOST" {
				return "ssh://user:secret@host"
			}
			return get(name)
		}
		s, err := New(config.DaggerConfig{Engine: mode}.Resolve(), o).Status(t.Context())
		must(t, err)
		want := ""
		if mode == "managed" || strings.HasPrefix(mode, "docker-container://") {
			want = "ssh://host"
		}
		if s.DockerHost != want {
			t.Fatalf("%s: %+v", mode, s)
		}
	}
}

func TestRemoteDaemonFailureDoesNotCreate(t *testing.T) {
	t.Parallel()
	o, dir := fakeHost(t)
	get := o.Getenv
	o.Getenv = func(name string) string {
		if name == "DOCKER_HOST" {
			return "tcp://offline.example.ts.net:2375"
		}
		return get(name)
	}
	put(t, dir, "docker-error", "yes")
	c := New(managed("v0.20.5"), o)
	s, err := c.Status(t.Context())
	must(t, err)
	if s.Reachable || !strings.Contains(s.ReachabilityError, "daemon unavailable") {
		t.Fatalf("%+v", s)
	}
	if err := c.Ensure(t.Context()); err == nil {
		t.Fatal("unavailable daemon accepted")
	}
	if strings.Contains(read(t, dir, "commands"), "docker run") {
		t.Fatal("created replacement engine")
	}
}

func TestCreationOutlastsProbeDeadline(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"ensure", "restart"} {
		t.Run(action, func(t *testing.T) {
			o, dir := fakeHost(t)
			o.Timeout, o.CreateTimeout, o.ActionTimeout = time.Second, 5*time.Second, 6*time.Second
			put(t, dir, "slow-run", "yes")
			c := New(managed("v0.20.5"), o)
			start := time.Now()
			if action == "ensure" {
				must(t, c.Ensure(t.Context()))
			} else {
				must(t, c.Restart(t.Context()))
			}
			if time.Since(start) <= o.Timeout {
				t.Fatal("creation did not outlast probe deadline")
			}
		})
	}
}

func TestRestartCommandsShareActionDeadline(t *testing.T) {
	t.Parallel()
	for _, slow := range []bool{false, true} {
		t.Run(map[bool]string{false: "finishes", true: "never finishes"}[slow], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				calls := 0
				o := Options{Timeout: time.Second, CreateTimeout: 8 * time.Second, ActionTimeout: 5 * time.Second,
					LookPath: func(name string) (string, error) { return name, nil }, Getenv: func(string) string { return "" },
					Command: func(ctx context.Context, name string, env []string, args ...string) (string, error) {
						calls++
						if args[0] == "run" {
							deadline, _ := ctx.Deadline()
							if deadline != start.Add(5*time.Second) {
								t.Fatalf("creation has independent budget: %v", deadline.Sub(start))
							}
							if slow {
								<-ctx.Done()
								return "", ctx.Err()
							}
							time.Sleep(time.Second)
							return "", nil
						}
						time.Sleep(500 * time.Millisecond)
						if name == "dagger" {
							return "v0.20.5", nil
						}
						if args[0] == "container" {
							return `[{"State":{"Running":true}}]`, nil
						}
						return "", nil
					},
				}
				err := New(managed(""), o).Restart(t.Context())
				if slow {
					if err == nil || !strings.Contains(err.Error(), "deadline exceeded") || time.Since(start) != 5*time.Second {
						t.Fatalf("duration %s: %v", time.Since(start), err)
					}
				} else {
					must(t, err)
					if time.Since(start) != 3*time.Second {
						t.Fatalf("%s", time.Since(start))
					}
				}
				if calls != 5 {
					t.Fatalf("%d calls", calls)
				}
			})
		})
	}
}

func TestStuckCreationEndsAtActionDeadline(t *testing.T) {
	t.Parallel()
	o, dir := fakeHost(t)
	o.Timeout, o.CreateTimeout, o.ActionTimeout = time.Second, 5*time.Second, time.Second
	put(t, dir, "stuck-run", "yes")
	start := time.Now()
	err := New(managed("v0.20.5"), o).Ensure(t.Context())
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") || time.Since(start) > 3*time.Second {
		t.Fatalf("duration %s: %v", time.Since(start), err)
	}
	if !strings.Contains(read(t, dir, "commands"), "docker run") {
		t.Fatal("deadline expired before creation")
	}
	if createTimeout <= commandTimeout || ActionTimeout+time.Second >= 5*time.Minute {
		t.Fatal("production deadlines leave no API response margin")
	}
}
