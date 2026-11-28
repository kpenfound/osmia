package service

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func hasCode(ds []Diagnostic, field string, code Code) bool {
	for _, d := range ds {
		if d.Field == field && d.Code == code {
			return true
		}
	}
	return false
}

// A reload validates the top-level file before applying it: a bad file leaves
// the loaded configuration and its digest as they were, and the failure stays
// in the configuration view until a reload succeeds.
func TestReloadValidatesTheTopLevelFileBeforeApplying(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	files := fixturekit.ConfigFiles(t, opts.Config.Root)
	_, c := start(t, opts)
	before, err := c.Configuration(ctx)
	fixturekit.Must(t, err)

	files.Write(t, files.TopText+"[capacity]\nmasons = 0\n")
	intakefixture.ReloadFails(t, c.Reload, files.Top, "capacity.masons")
	now, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if now.Digest != before.Digest || !reflect.DeepEqual(now.Effective, before.Effective) || now.Effective.Capacity.Masons != 4 {
		t.Fatalf("a failed reload changed the loaded configuration: %+v", now)
	}
	if e := now.LastError; e == nil || e.Path != files.Top || e.Field != "capacity.masons" || e.At.IsZero() {
		t.Fatalf("last error: %+v", now.LastError)
	}
	if !hasCode(now.Diagnostics, "configuration", Validation) {
		t.Fatalf("diagnostics: %+v", now.Diagnostics)
	}

	files.Write(t, files.TopText+"[capacity]\nmasons = 7\n")
	now, err = c.Configuration(ctx)
	fixturekit.Must(t, err)
	if now.LastError == nil || now.Digest != before.Digest || !hasCode(now.Diagnostics, "configuration", ReloadRequired) {
		t.Fatalf("after the fix on disk: %+v", now)
	}

	files.Write(t, files.TopText+"credential = 'secret-reload-text'\n[broken")
	_, err = c.Reload(ctx)
	if err == nil || strings.Contains(err.Error(), "secret-reload-text") || !strings.Contains(err.Error(), files.Top+": ") {
		t.Fatalf("unparsable reload: %v", err)
	}
	now, err = c.Configuration(ctx)
	fixturekit.Must(t, err)
	encoded, _ := json.Marshal(now)
	if now.LastError == nil || now.LastError.Path != files.Top || now.Digest != before.Digest || bytes.Contains(encoded, []byte("secret-reload-text")) {
		t.Fatalf("after an unparsable reload: %s", encoded)
	}

	files.Write(t, files.TopText+"[capacity]\nmasons = 7\n")
	reloaded, err := c.Reload(ctx)
	fixturekit.Must(t, err)
	now, err = c.Configuration(ctx)
	fixturekit.Must(t, err)
	if reloaded.Digest == before.Digest || reloaded.Digest != now.Digest || len(reloaded.RestartRequired) != 0 {
		t.Fatalf("reload: %+v, configuration digest %s", reloaded, now.Digest)
	}
	if now.LastError != nil || len(now.Diagnostics) != 0 || now.Effective.Capacity.Masons != 7 || now.Effective.Project.Landing != "commit-per-unit" {
		t.Fatalf("after a successful reload: %+v", now)
	}
}

// Settings that need a restart keep their loaded values through a reload,
// which applies the rest and names them; the configuration view keeps
// reporting them until the service restarts.
func TestReloadReportsRestartRequiredSettings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	files := fixturekit.ConfigFiles(t, opts.Config.Root)
	s, c := start(t, opts)
	socket := s.Socket()

	files.Write(t, files.TopText+"[listen]\nsocket = \"local.sock\"\n[capacity]\nreviewers = 5\n")
	reloaded, err := c.Reload(ctx)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(reloaded.RestartRequired, []string{"listen.socket"}) {
		t.Fatalf("restart required: %+v", reloaded)
	}
	now, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if now.Effective.Listen.Socket != socket || now.Effective.Capacity.Reviewers != 5 || now.Digest != reloaded.Digest {
		t.Fatalf("after reload: %+v", now)
	}
	if !hasCode(now.Diagnostics, "configuration", RestartRequired) || hasCode(now.Diagnostics, "configuration", ReloadRequired) {
		t.Fatalf("diagnostics: %+v", now.Diagnostics)
	}
}

func TestReloadKeepsTheRunningProjectsAndTheirEffectiveSettings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := newOrderRoot(t, 2, nil)
	s, c := start(t, root.opts)
	first, second := root.ids[0], root.ids[1]
	store := projectstore.Open(root.cfg.Root)
	current, err := store.Current(first)
	fixturekit.Must(t, err)
	newer, err := store.Replace(first, current.Pin, append(current.Data, "checks_timeout = \"30m\"\n"...))
	fixturekit.Must(t, err)
	newcomer := config.ProjectID("p_cccccccccccccccccccccccccccccccc")
	_, err = store.WriteFirst(newcomer, []byte("version = 1\nupstream = \"upstream/new\"\nclone = \""+filepath.Join(t.TempDir(), "new")+"\"\n"))
	fixturekit.Must(t, err)
	fixturekit.Must(t, store.Add(newcomer))
	fixturekit.Must(t, store.Remove(second))
	files := fixturekit.ConfigFiles(t, root.opts.Config.Root)
	files.Write(t, files.TopText+"[capacity]\nper_workstream = 5\n")

	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)

	loaded := s.current()
	if got := loaded.ProjectIDs(); !slices.Equal(got, root.ids) {
		t.Fatalf("projects after the reload %v, want the running set %v", got, root.ids)
	}
	if got := loaded.For(first).Project.ChecksTimeout; got != config.DefaultChecksTimeout {
		t.Fatalf("the reload adopted the saved revision: checks_timeout %q", got)
	}
	read, err := c.ProjectConfig(ctx, first)
	fixturekit.Must(t, err)
	timeout := settingOf(t, read, "checks_timeout")
	if read.Pin != newer.Pin || !timeout.Pending || timeout.Value != "30m" || timeout.Effective != config.DefaultChecksTimeout {
		t.Fatalf("the saved revision is not pending: %+v", timeout)
	}
	for _, id := range root.ids {
		if got := loaded.For(id).Project.Capacity.PerWorkstream; got != 5 {
			t.Fatalf("project %s: the reloaded inherited per_workstream is %d, want 5", id, got)
		}
	}
}

// Every [error_reports] setting applies on reload: turning the reporter on and
// off and changing its repository, labels, caps and sources never needs a
// restart.
func TestReloadAppliesErrorReports(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	files := fixturekit.ConfigFiles(t, opts.Config.Root)
	_, c := start(t, opts)
	before, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if e := before.Effective.ErrorReports; e.Enabled || e.Repository != "kpenfound/osmia" {
		t.Fatalf("default effective error reports: %+v", e)
	}

	files.Write(t, files.TopText+`[error_reports]
enabled = true
repository = "owner/other"
labels = ["bug"]
max_issues_per_hour = 1
max_issues_per_day = 2
max_comments_per_day = 3
sources = ["github"]
`)
	reloaded, err := c.Reload(ctx)
	fixturekit.Must(t, err)
	now, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	want := config.ErrorReports{Enabled: true, Repository: "owner/other", Labels: []string{"bug"}, MaxIssuesPerHour: 1, MaxIssuesPerDay: 2, MaxCommentsPerDay: 3, Sources: []string{"github"}}
	if len(reloaded.RestartRequired) != 0 || !reflect.DeepEqual(now.Effective.ErrorReports, want) || hasCode(now.Diagnostics, "configuration", RestartRequired) || reloaded.Digest == before.Digest {
		t.Fatalf("reload %+v, effective %+v, diagnostics %+v", reloaded, now.Effective.ErrorReports, now.Diagnostics)
	}

	files.Write(t, files.TopText)
	reloaded, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	now, err = c.Configuration(ctx)
	fixturekit.Must(t, err)
	if len(reloaded.RestartRequired) != 0 || !reflect.DeepEqual(now.Effective.ErrorReports, before.Effective.ErrorReports) {
		t.Fatalf("after removing the section: %+v %+v", reloaded, now.Effective.ErrorReports)
	}

	files.Write(t, files.TopText+"[error_reports]\nrepository = \"not a repository\"\n")
	intakefixture.ReloadFails(t, c.Reload, files.Top, "error_reports.repository")
}

func TestReloadPreservesRuntimeState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	files := fixturekit.ConfigFiles(t, opts.Config.Root)
	_, c := start(t, opts)
	intakefixture.Mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Reason: "travel", Source: "owner"})
	intakefixture.Mutation(t, c, "PUT", "priority", PriorityRequest{Project: fixturekit.Project, Workstreams: []config.WorkstreamID{fixturekit.Stream}})
	intakefixture.Mutation(t, c, "PUT", "profile", ProfileRequest{Role: "mason", Profile: "other"})
	before, err := c.Runtime(ctx)
	fixturekit.Must(t, err)
	stored, err := os.ReadFile(filepath.Join(opts.Config.Root, "runtime.json"))
	fixturekit.Must(t, err)
	unchanged := func(t *testing.T) {
		t.Helper()
		disk, err := os.ReadFile(filepath.Join(opts.Config.Root, "runtime.json"))
		fixturekit.Must(t, err)
		if !bytes.Equal(disk, stored) {
			t.Fatalf("reload rewrote runtime.json:\n%s", disk)
		}
	}

	files.Write(t, files.TopText+"[roles.reviewer]\nprofile = \"other\"\n")
	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	unchanged(t)
	now, err := c.Runtime(ctx)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(now.Effective.Pauses, before.Effective.Pauses) || !reflect.DeepEqual(now.Effective.Priorities, before.Effective.Priorities) {
		t.Fatalf("runtime controls changed: %+v", now.Effective)
	}
	if now.Profiles["mason"] != (EffectiveProfile{Name: "other", Source: "owner_override"}) || now.Profiles["reviewer"] != (EffectiveProfile{Name: "other", Source: "configuration"}) {
		t.Fatalf("profiles: %+v", now.Profiles)
	}

	withoutOther := strings.Replace(files.TopText, "[profiles.other]\nagent = \"codex\"\nmodel = \"other\"\n", "", 1)
	files.Write(t, withoutOther)
	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	unchanged(t)
	now, err = c.Runtime(ctx)
	fixturekit.Must(t, err)
	if now.Profiles["mason"] != (EffectiveProfile{Name: "default", Source: "configuration"}) || !hasCode(now.Diagnostics, "profiles", Validation) {
		t.Fatalf("stale override: %+v %+v", now.Profiles, now.Diagnostics)
	}
	files.Write(t, files.TopText)
	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	unchanged(t)
	now, err = c.Runtime(ctx)
	fixturekit.Must(t, err)
	if !reflect.DeepEqual(now.Effective, before.Effective) || !reflect.DeepEqual(now.Profiles, before.Profiles) {
		t.Fatalf("runtime after restoring the profile: %+v, want %+v", now, before)
	}
}

// A reload leaves a running turn on its original binding. Conversations adopt
// the new binding directly, and scheduling adopts it on its next pass.
func TestReloadAppliesToNextTurnsAndKeepsRunningOnes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "rl-")
	files := fixturekit.ConfigFiles(t, opts.Config.Root)
	turns := &fixturekit.ChiefTurns{Hold: make(chan struct{}), Entered: make(chan struct{})}
	var (
		mu       sync.Mutex
		bound    []*config.Config
		prepared = map[string]int{}
	)
	opts.Threads = func(r *trace.Repository, loaded *config.Config) (coreadapter.Reconciler, error) {
		mu.Lock()
		bound = append(bound, loaded)
		binding := len(bound) - 1
		mu.Unlock()
		return thread.Dispatcher{Runner: thread.Runner{Jev: fixturekit.UnconfiguredJudge(nil), Store: r, Turns: turns, Now: opts.Reconciliation.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				mu.Lock()
				prepared[in.Turn] = binding
				mu.Unlock()
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(cfg.Root.String(), "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	s, c := start(t, opts)
	first, err := c.Send(ctx, fixturekit.Stream, "first")
	fixturekit.Must(t, err)
	select {
	case <-turns.Entered:
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("the first message did not run")
	}

	files.Write(t, files.TopText+"[roles.manager]\nprofile = \"other\"\n[capacity]\nmasons = 7\n")
	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	second, err := c.Send(ctx, fixturekit.Stream, "second")
	fixturekit.Must(t, err)
	close(turns.Hold)
	intakefixture.AwaitConversation(t, c, func(l ConversationResponse) bool {
		return len(l.Entries) == 4 && l.Entries[3].State == TurnDone
	})
	fixturekit.ConversationAwait(t, func() bool { return s.sole().pipeline.current.Load().cfg == s.current() })

	mu.Lock()
	defer mu.Unlock()
	if len(bound) != 2 || bound[1] != s.current() || bound[1].Capacity.Masons != 7 {
		t.Fatalf("thread bindings: %d, reloaded binding current=%t", len(bound), len(bound) == 2 && bound[1] == s.current())
	}
	if prepared[first.Turn] != 0 || prepared[second.Turn] != 1 {
		t.Fatalf("bindings that prepared the turns: %v", prepared)
	}
	calls := turns.Calls()
	if len(calls) != 2 || calls[0].Profile.Name != "default" || calls[1].Profile.Name != "other" {
		t.Fatalf("turn profiles: %+v", calls)
	}
	if got := s.sole().pipeline.current.Load().cfg; got != s.current() {
		t.Fatal("the running project's passes do not use the reloaded configuration")
	}
}

func TestDefaultRootReloadsXDGConfig(t *testing.T) {
	ctx := context.Background()
	opts := fixture(t)
	home := filepath.Dir(opts.Config.Root)
	data, conf := filepath.Join(home, "data"), filepath.Join(home, "conf")
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("XDG_CONFIG_HOME", conf)
	fixturekit.Must(t, os.MkdirAll(data, 0700))
	fixturekit.Must(t, os.MkdirAll(filepath.Join(conf, "osmia"), 0700))
	root := filepath.Join(data, "osmia")
	fixturekit.Must(t, os.Rename(opts.Config.Root, root))
	top := filepath.Join(conf, "osmia", "config.toml")
	fixturekit.Must(t, os.Rename(filepath.Join(root, "config.toml"), top))
	opts.Config = config.Options{}
	_, c := start(t, opts)
	text, err := os.ReadFile(top)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(top, append(text, "[capacity]\nmasons = 7\n"...), 0600))
	now, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if len(now.Drift.Files) == 0 || now.Drift.Files[0].Path != top || now.Drift.Files[0].State != ConfigChanged {
		t.Fatalf("drift: %+v", now.Drift)
	}
	if _, err := c.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	now, err = c.Configuration(ctx)
	fixturekit.Must(t, err)
	if now.Effective.Capacity.Masons != 7 {
		t.Fatalf("after reload: %+v", now.Effective.Capacity)
	}
}
