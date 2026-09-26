package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/workspace"
)

// providerOf returns the provider of the workstream's workspaces.
func providerOf(t *testing.T, w streamWorkspaces, stream config.WorkstreamID) workspace.Provider {
	t.Helper()
	g, err := w.of(stream)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// fakeJJ stands in for the check of the jj on PATH: it reports version, and
// err when set.
type fakeJJ struct {
	mu      sync.Mutex
	version string
	err     error
	bounded bool
}

func (j *fakeJJ) set(version string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.version, j.err = version, err
}

func (j *fakeJJ) check(ctx context.Context) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	deadline, ok := ctx.Deadline()
	j.bounded = ok && time.Until(deadline) <= jjCheckTimeout
	return j.version, j.err
}

// lastBounded reports whether the latest check had a deadline no later than
// jjCheckTimeout from when it ran.
func (j *fakeJJ) lastBounded() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.bounded
}

var (
	jjMissing = fmt.Errorf("%w: exec: \"jj\": executable file not found in $PATH", workspace.ErrJJMissing)
	jjTooOld  = fmt.Errorf("%w: found jj 0.44.0, Osmia needs %s or later", workspace.ErrJJTooOld, workspace.MinimumJJ)
)

// workspacesFixture is a hand-in fixture whose configuration sets
// workspaces to setting and whose jj check is jj.
type workspacesFixture struct {
	*handInFixture
	opts Options
	jj   *fakeJJ
	top  string
}

func newWorkspacesFixture(t *testing.T, setting string) *workspacesFixture {
	t.Helper()
	opts, clone := projectFixture(t)
	jj := &fakeJJ{version: "0.45.1"}
	opts.checkJJ = jj.check
	top := filepath.Join(opts.Config.Root, "config.toml")
	f := &workspacesFixture{opts: opts, jj: jj, top: top}
	f.setting(t, setting)
	s, c := start(t, opts)
	added, err := c.AddProject(context.Background(), request(clone))
	must(t, err)
	must(t, os.WriteFile(added.Project.Charter, []byte("1. Keep changes small.\n"), 0600))
	f.handInFixture = &handInFixture{s: s, c: c, project: added.Project.ID, trace: added.Project.Trace, charter: added.Project.Charter, home: filepath.Dir(clone)}
	return f
}

// setting writes workspaces = setting to the top-level configuration file.
func (f *workspacesFixture) setting(t *testing.T, setting string) {
	t.Helper()
	text, err := os.ReadFile(f.top)
	must(t, err)
	lines := strings.Split(string(text), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "workspaces = ") {
			lines[i] = fmt.Sprintf("workspaces = %q", setting)
		}
	}
	must(t, os.WriteFile(f.top, []byte(strings.Join(lines, "\n")), 0600))
}

// handInKey hands in a design under key and returns the result.
func (f *workspacesFixture) handInKey(key string) (HandInResponse, error) {
	text := "# " + key + "\n"
	return f.c.HandIn(context.Background(), HandInRequest{Project: f.project, Key: key, Stdin: &text})
}

// backend reports the workstream's backend as status, the recorded
// manifest and every kind of workspace the service reaches see it.
func (f *workspacesFixture) backend(t *testing.T, stream config.WorkstreamID) string {
	t.Helper()
	st, err := f.c.Status(context.Background(), stream)
	must(t, err)
	recorded, err := f.repository().Workspaces(stream)
	must(t, err)
	if st.Workspaces != recorded {
		t.Fatalf("status reports workstream %s on %q; its manifest records %q", stream, st.Workspaces, recorded)
	}
	cfg := f.s.current()
	for name, w := range map[string]streamWorkspaces{
		"feature": featureWorkspaces(cfg, f.repository()),
		"unit":    newUnitWorkspaces(cfg, f.repository()).streamWorkspaces,
		"drift":   driftWorkspaces(cfg, f.repository()),
	} {
		var got string
		switch providerOf(t, w, stream).(type) {
		case *workspace.Git:
			got = config.WorkspacesGit
		case *workspace.Jujutsu:
			got = config.WorkspacesJujutsu
		}
		if got != recorded {
			t.Fatalf("the %s workspaces of workstream %s are %q; it records %q", name, stream, got, recorded)
		}
	}
	return recorded
}

// Each workspaces value, with jj supported, missing or too old, gives a new
// workstream the backend it names, and service status reports what new
// workstreams get. Only workspaces = "jujutsu" without a supported jj
// refuses to start one, naming the problem and the version found in the
// refusal and in status.
func TestWorkspacesKeyPicksTheBackendOfANewWorkstream(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, setting string
		jj            error
		version       string
		want          string
		problem       string
	}{
		{"git with jj", config.WorkspacesGit, nil, "0.45.1", config.WorkspacesGit, ""},
		{"git without jj", config.WorkspacesGit, jjMissing, "", config.WorkspacesGit, ""},
		{"auto with jj", config.WorkspacesAuto, nil, "0.45.1", config.WorkspacesJujutsu, ""},
		{"auto without jj", config.WorkspacesAuto, jjMissing, "", config.WorkspacesGit, ""},
		{"auto with an old jj", config.WorkspacesAuto, jjTooOld, "0.44.0", config.WorkspacesGit, ""},
		{"jujutsu with jj", config.WorkspacesJujutsu, nil, "0.45.1", config.WorkspacesJujutsu, ""},
		{"jujutsu without jj", config.WorkspacesJujutsu, jjMissing, "", "", "jj is missing"},
		{"jujutsu with an old jj", config.WorkspacesJujutsu, jjTooOld, "0.44.0", "", "found jj 0.44.0, Osmia needs 0.45.0 or later"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newWorkspacesFixture(t, c.setting)
			f.jj.set(c.version, c.jj)
			ctx := context.Background()
			all, err := f.c.Statuses(ctx)
			must(t, err)
			ws := all.Workspaces
			jj := c.version
			if c.setting == config.WorkspacesGit {
				jj = ""
			}
			if ws.Setting != c.setting || ws.Backend != c.want || ws.JJ != jj || !strings.Contains(ws.Problem, c.problem) || (c.problem == "") != (ws.Problem == "") {
				t.Fatalf("service status workspaces %+v", ws)
			}
			var diagnostic *Diagnostic
			for _, d := range all.Diagnostics {
				if d.Field == "workspaces" {
					diagnostic = &d
				}
			}
			if c.problem == "" && diagnostic != nil || c.problem != "" && (diagnostic == nil || diagnostic.Code != Unavailable || diagnostic.Message != ws.Problem) {
				t.Fatalf("service status diagnostics %+v", all.Diagnostics)
			}

			out, err := f.handInKey("design")
			if c.problem != "" {
				var api *APIError
				if !errors.As(err, &api) || api.Code != Unavailable || !strings.Contains(api.Message, c.problem) || !strings.Contains(api.Message, `workspaces is "jujutsu"`) {
					t.Fatalf("a hand-in with no supported jj: %v", err)
				}
				if streams := f.streams(t); len(streams) != 0 {
					t.Fatalf("a refused hand-in left workstreams %v", streams)
				}
				return
			}
			must(t, err)
			if got := f.backend(t, out.Workstream); got != c.want {
				t.Fatalf("workstream on %q, want %q", got, c.want)
			}
		})
	}
}

// A workstream keeps the backend it was created on through a configuration
// change, a reload, jj going away and a restart; the change applies to new
// workstreams alone.
func TestWorkstreamKeepsItsBackendAcrossARestartAfterTheKeyChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newWorkspacesFixture(t, config.WorkspacesAuto)
	first, err := f.handInKey("first")
	must(t, err)
	if got := f.backend(t, first.Workstream); got != config.WorkspacesJujutsu {
		t.Fatalf("the first workstream is on %q", got)
	}

	f.setting(t, config.WorkspacesGit)
	_, err = f.c.Reload(ctx)
	must(t, err)
	second, err := f.handInKey("second")
	must(t, err)
	if got := f.backend(t, second.Workstream); got != config.WorkspacesGit {
		t.Fatalf("the workstream created after the change is on %q", got)
	}
	if got := f.backend(t, first.Workstream); got != config.WorkspacesJujutsu {
		t.Fatalf("after the reload the first workstream is on %q", got)
	}

	must(t, f.s.Close())
	f.jj.set("", jjMissing)
	f.setting(t, config.WorkspacesAuto)
	s, c := start(t, f.opts)
	f.s, f.c = s, c
	for stream, want := range map[config.WorkstreamID]string{first.Workstream: config.WorkspacesJujutsu, second.Workstream: config.WorkspacesGit} {
		if got := f.backend(t, stream); got != want {
			t.Fatalf("after the restart workstream %s is on %q, want %q", stream, got, want)
		}
	}
	third, err := f.handInKey("third")
	must(t, err)
	if got := f.backend(t, third.Workstream); got != config.WorkspacesGit {
		t.Fatalf("a workstream created without jj under auto is on %q", got)
	}
}

// A workstream whose manifest records no backend, as one created before
// backends were recorded, is on Git worktrees.
func TestWorkstreamWithoutARecordedBackendIsOnGit(t *testing.T) {
	t.Parallel()
	f := newWorkspacesFixture(t, config.WorkspacesJujutsu)
	must(t, f.repository().CreateWorkstream(context.Background(), stream, time.Now().UTC(), ownerActor))
	if got := f.backend(t, stream); got != config.WorkspacesGit {
		t.Fatalf("a workstream without a recorded backend is on %q", got)
	}
}

// Service status checks jj with a deadline, so a jj that hangs cannot hold
// up a status read.
func TestStatusChecksJJWithADeadline(t *testing.T) {
	t.Parallel()
	f := newWorkspacesFixture(t, config.WorkspacesAuto)
	_, err := f.c.Statuses(context.Background())
	must(t, err)
	if !f.jj.lastBounded() {
		t.Fatal("status checked jj without a deadline")
	}
}
