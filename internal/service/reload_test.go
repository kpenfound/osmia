package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// reloadFiles are the top-level and project configuration files of a fixture
// with their original contents.
type reloadFiles struct {
	top, project         string
	topText, projectText string
}

func configFiles(t *testing.T, opts Options) reloadFiles {
	t.Helper()
	f := reloadFiles{top: filepath.Join(opts.Config.Root, "config.toml"), project: filepath.Join(opts.Config.Root, "projects", string(project), "config.toml")}
	top, err := os.ReadFile(f.top)
	must(t, err)
	p, err := os.ReadFile(f.project)
	must(t, err)
	f.topText, f.projectText = string(top), string(p)
	return f
}

func (f reloadFiles) write(t *testing.T, top, project string) {
	t.Helper()
	must(t, os.WriteFile(f.top, []byte(top), 0600))
	must(t, os.WriteFile(f.project, []byte(project), 0600))
}

func hasCode(ds []Diagnostic, field string, code Code) bool {
	for _, d := range ds {
		if d.Field == field && d.Code == code {
			return true
		}
	}
	return false
}

func reloadFails(t *testing.T, c *Client, path, field string) {
	t.Helper()
	_, err := c.Reload(context.Background())
	var api *APIError
	if !errors.As(err, &api) || api.Code != Validation || !strings.Contains(api.Message, path+": "+field+": ") {
		t.Fatalf("reload error: %v, want validation naming %s and %s", err, path, field)
	}
}

// A reload validates the top-level file and the project's file as one
// candidate: a bad file in either leaves the loaded configuration and its
// digest as they were, and the failure stays in the configuration view until
// a reload succeeds.
func TestReloadValidatesEveryFileBeforeApplying(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	files := configFiles(t, opts)
	_, c := start(t, opts)
	before, err := c.Configuration(ctx)
	must(t, err)

	// A valid top-level change is not applied while the project file fails.
	files.write(t, files.topText+"[capacity]\nmasons = 7\n", files.projectText+"landing = \"sideways\"\n")
	reloadFails(t, c, files.project, "landing")
	now, err := c.Configuration(ctx)
	must(t, err)
	if now.Digest != before.Digest || !reflect.DeepEqual(now.Effective, before.Effective) || now.Effective.Capacity.Masons != 4 {
		t.Fatalf("a failed reload changed the loaded configuration: %+v", now)
	}
	if e := now.LastError; e == nil || e.Path != files.project || e.Field != "landing" || e.At.IsZero() {
		t.Fatalf("last error: %+v", now.LastError)
	}
	if !hasCode(now.Diagnostics, "configuration", Validation) {
		t.Fatalf("diagnostics: %+v", now.Diagnostics)
	}

	// Fixing the file on disk leaves the last error until a reload succeeds.
	files.write(t, files.topText+"[capacity]\nmasons = 7\n", files.projectText)
	now, err = c.Configuration(ctx)
	must(t, err)
	if now.LastError == nil || now.Digest != before.Digest || !hasCode(now.Diagnostics, "configuration", ReloadRequired) {
		t.Fatalf("after the fix on disk: %+v", now)
	}

	// A bad top-level file fails the same way; the error names it instead.
	files.write(t, files.topText+"[capacity]\nmasons = 0\n", files.projectText)
	reloadFails(t, c, files.top, "capacity.masons")
	// TOML that does not parse names the file and line, never its text.
	files.write(t, files.topText+"credential = 'secret-reload-text'\n[broken", files.projectText)
	_, err = c.Reload(ctx)
	if err == nil || strings.Contains(err.Error(), "secret-reload-text") || !strings.Contains(err.Error(), files.top+": ") {
		t.Fatalf("unparsable reload: %v", err)
	}
	now, err = c.Configuration(ctx)
	must(t, err)
	encoded, _ := json.Marshal(now)
	if now.LastError == nil || now.LastError.Path != files.top || now.Digest != before.Digest || bytes.Contains(encoded, []byte("secret-reload-text")) {
		t.Fatalf("after an unparsable reload: %s", encoded)
	}

	files.write(t, files.topText+"[capacity]\nmasons = 7\n", files.projectText)
	reloaded, err := c.Reload(ctx)
	must(t, err)
	now, err = c.Configuration(ctx)
	must(t, err)
	if reloaded.Digest == before.Digest || reloaded.Digest != now.Digest || len(reloaded.RestartRequired) != 0 {
		t.Fatalf("reload: %+v, configuration digest %s", reloaded, now.Digest)
	}
	if now.LastError != nil || len(now.Diagnostics) != 0 || now.Effective.Capacity.Masons != 7 {
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
	files := configFiles(t, opts)
	s, c := start(t, opts)
	socket := s.Socket()

	files.write(t, files.topText+"[listen]\nsocket = \"local.sock\"\n[capacity]\nreviewers = 5\n", files.projectText)
	reloaded, err := c.Reload(ctx)
	must(t, err)
	if !reflect.DeepEqual(reloaded.RestartRequired, []string{"listen.socket"}) {
		t.Fatalf("restart required: %+v", reloaded)
	}
	now, err := c.Configuration(ctx)
	must(t, err)
	if now.Effective.Listen.Socket != socket || now.Effective.Capacity.Reviewers != 5 || now.Digest != reloaded.Digest {
		t.Fatalf("after reload: %+v", now)
	}
	if !hasCode(now.Diagnostics, "configuration", RestartRequired) || hasCode(now.Diagnostics, "configuration", ReloadRequired) {
		t.Fatalf("diagnostics: %+v", now.Diagnostics)
	}

	// The loaded project stays when the disk stops listing it, and its file
	// is still validated.
	unlisted := strings.Replace(files.topText, `active_projects = ["`+string(project)+`"]`, "active_projects = []", 1)
	files.write(t, unlisted, files.projectText+"landing = \"sideways\"\n")
	reloadFails(t, c, files.project, "landing")
	files.write(t, unlisted, files.projectText+"landing = \"squash\"\n")
	reloaded, err = c.Reload(ctx)
	must(t, err)
	now, err = c.Configuration(ctx)
	must(t, err)
	if !reflect.DeepEqual(reloaded.RestartRequired, []string{"active_projects"}) || now.Effective.Project.ID != project || now.Effective.Project.Landing != "squash" || now.Effective.Listen.Socket != socket {
		t.Fatalf("unlisted project: %+v %+v", reloaded, now.Effective)
	}
}

// A reload resolves the runtime state against the new configuration without
// rewriting it: pauses, priorities and profile overrides survive, and an
// override the new configuration cannot honour is only set aside until a
// configuration names its profile again.
func TestReloadPreservesRuntimeState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	files := configFiles(t, opts)
	_, c := start(t, opts)
	mutation(t, c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Reason: "travel", Source: "owner"})
	mutation(t, c, "PUT", "priority", PriorityRequest{Project: project, Workstreams: []config.WorkstreamID{stream}})
	mutation(t, c, "PUT", "profile", ProfileRequest{"mason", "other"})
	before, err := c.Runtime(ctx)
	must(t, err)
	stored, err := os.ReadFile(filepath.Join(opts.Config.Root, "runtime.json"))
	must(t, err)
	unchanged := func(t *testing.T) {
		t.Helper()
		disk, err := os.ReadFile(filepath.Join(opts.Config.Root, "runtime.json"))
		must(t, err)
		if !bytes.Equal(disk, stored) {
			t.Fatalf("reload rewrote runtime.json:\n%s", disk)
		}
	}

	files.write(t, files.topText+"[roles.reviewer]\nprofile = \"other\"\n", files.projectText)
	_, err = c.Reload(ctx)
	must(t, err)
	unchanged(t)
	now, err := c.Runtime(ctx)
	must(t, err)
	if !reflect.DeepEqual(now.Effective.Pauses, before.Effective.Pauses) || !reflect.DeepEqual(now.Effective.Priorities, before.Effective.Priorities) {
		t.Fatalf("runtime controls changed: %+v", now.Effective)
	}
	if now.Profiles["mason"] != (EffectiveProfile{"other", "owner_override"}) || now.Profiles["reviewer"] != (EffectiveProfile{"other", "configuration"}) {
		t.Fatalf("profiles: %+v", now.Profiles)
	}

	// Without the overridden profile the override is excluded, not dropped.
	withoutOther := strings.Replace(files.topText, "[profiles.other]\nagent = \"codex\"\nmodel = \"other\"\n", "", 1)
	files.write(t, withoutOther, files.projectText)
	_, err = c.Reload(ctx)
	must(t, err)
	unchanged(t)
	now, err = c.Runtime(ctx)
	must(t, err)
	if now.Profiles["mason"] != (EffectiveProfile{"default", "configuration"}) || !hasCode(now.Diagnostics, "profiles", Validation) {
		t.Fatalf("stale override: %+v %+v", now.Profiles, now.Diagnostics)
	}
	files.write(t, files.topText, files.projectText)
	_, err = c.Reload(ctx)
	must(t, err)
	unchanged(t)
	now, err = c.Runtime(ctx)
	must(t, err)
	if !reflect.DeepEqual(now.Effective, before.Effective) || !reflect.DeepEqual(now.Profiles, before.Profiles) {
		t.Fatalf("runtime after restoring the profile: %+v, want %+v", now, before)
	}
}

// A reload while a turn runs leaves that turn on the configuration it started
// with; the project's next pass rebuilds dispatch from the reloaded
// configuration, so the next turn runs on it with the reloaded role binding.
func TestReloadAppliesToNextTurnsAndKeepsRunningOnes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, cfg := conversationFixture(t, "rl-")
	files := configFiles(t, opts)
	turns := &chiefTurns{hold: make(chan struct{}), entered: make(chan struct{})}
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
		return thread.Dispatcher{Runner: thread.Runner{Store: r, Turns: turns, Now: opts.Reconciliation.Now},
			Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
				mu.Lock()
				prepared[in.Turn] = binding
				mu.Unlock()
				return coreadapter.PreparedTurn{SessionDirectory: filepath.Join(cfg.Root.String(), "sessions", in.Agent, in.Turn)}, nil
			}}, nil
	}
	s, c := start(t, opts)
	first, err := c.Send(ctx, stream, "first")
	must(t, err)
	select {
	case <-turns.entered:
	case <-time.After(demoTimeout):
		t.Fatal("the first message did not run")
	}

	files.write(t, files.topText+"[roles.chief_of_staff]\nprofile = \"other\"\n[capacity]\nmasons = 7\n", files.projectText)
	_, err = c.Reload(ctx)
	must(t, err)
	second, err := c.Send(ctx, stream, "second")
	must(t, err)
	close(turns.hold)
	awaitConversation(t, c, func(l ConversationResponse) bool {
		return len(l.Entries) == 4 && l.Entries[3].State == TurnDone
	})

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
	if got := s.active.pipeline.current.Load().cfg; got != s.current() {
		t.Fatal("the running project's passes do not use the reloaded configuration")
	}
}
