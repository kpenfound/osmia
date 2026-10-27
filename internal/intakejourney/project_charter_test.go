package intakejourney

import (
	context "context"
	errors "errors"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/charter"
	os "os"
	filepath "path/filepath"
	"reflect"
	strings "strings"
	sync "sync"
	testing "testing"
	time "time"

	config "github.com/kpenfound/osmia/internal/config"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestProjectCharterRemainsReadableWhileMemberTurnsDrain(t *testing.T) {
	for _, mode := range []string{"reload", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newTwoProjectFixture(t)
			member := f.cfg.For(otherProject).Project
			repo, err := trace.Open(f.cfg.Root, member)
			must(t, err)
			_, err = trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{otherStream}, f.clock.Now(), serviceActor, "fixture")
			must(t, err)
			must(t, repo.Close())
			path, err := f.cfg.Root.ProjectConfig(member.ID)
			must(t, err)
			must(t, config.WriteProjectConfig(path, member))
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			read := make(chan error, 1)
			f.turns.Mu.Lock()
			f.turns.Hooks[turn(otherProject, "first")] = func(ctx context.Context) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
				doc, err := f.repository(otherProject).Charter(context.WithoutCancel(ctx))
				if err == nil && doc.Project != otherProject {
					err = errors.New("draining turn read another project policy")
				}
				read <- err
			}
			f.turns.Mu.Unlock()
			s, c := start(t, f.opts)
			await(t, "the member's running turn", entered)
			finished := make(chan error, 1)
			if mode == "reload" {
				for _, id := range []config.ProjectID{project, otherProject} {
					r, err := s.repository(id)
					must(t, err)
					state, err := r.Workflow(f.streams[id], trace.FeatureSubject)
					must(t, err)
					_, err = r.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "resolve-project-reload", Revision: 1, Project: id, Workstream: f.streams[id], At: f.clock.Now(), Actor: ownerActor, Cause: "owner"}, Subject: trace.FeatureSubject, From: state.Value, To: AbandonedState, Reason: "owner cancelled member"}})
					must(t, err)
				}

				path := filepath.Join(f.opts.Config.Root, "config.toml")
				top, err := os.ReadFile(path)
				must(t, err)
				line := "active_projects = [\"" + string(project) + "\", \"" + string(otherProject) + "\"]"
				updated := strings.Replace(string(top), line, "active_projects = []", 1)
				if updated == string(top) {
					t.Fatal("fixture did not remove both project registrations")
				}
				must(t, os.WriteFile(path, []byte(updated), 0600))
				go func() { _, err := c.Reload(ctx); finished <- err }()
				soon(t, "both projects draining", func() bool { return !s.current().Active(project) && !s.current().Active(otherProject) })
				unblock()
			} else {
				go func() { finished <- s.Close() }()
			}
			select {
			case err := <-read:
				must(t, err)
			case <-time.After(demoTimeout):
				t.Fatal("draining turn could not read the project charter")
			}
			select {
			case err := <-finished:
				must(t, err)
			case <-time.After(demoTimeout):
				t.Fatal("projects did not finish draining")
			}
		})
	}
}

func TestProjectChartersAndContextsStayIndependentAcrossRestart(t *testing.T) {
	t.Parallel()
	opts, clone := projectFixture(t)
	s, c := start(t, opts)
	ctx := context.Background()
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	second := filepath.Join(filepath.Dir(clone), "client")
	demoGit(t, filepath.Dir(clone), "init", "-q", second)
	other, err := c.AddProject(ctx, ProjectAddRequest{Name: "Client", Upstream: "owner/client", Clone: second})
	must(t, err)
	policies := map[config.ProjectID]string{added.Project.ID: "1. Server policy.\n", other.Project.ID: "1. Client policy.\n7. Preserve identifiers.\n"}
	for id, text := range policies {
		writeCharter(t, c, id, text)
	}
	check := func() {
		for id, text := range policies {
			got, err := c.Charter(ctx, id)
			must(t, err)
			if got.Project != id || got.Content != text {
				t.Fatalf("project policy: %+v", got)
			}
			b, err := s.Context().Assemble(ctx, id, bundle.Scope{Role: architectRole})
			must(t, err)
			if !reflect.DeepEqual(b.Charter.Rules, charter.Parse(text).Rules) {
				t.Fatalf("project context: %+v", b.Charter)
			}
		}
	}
	check()
	c.Close()
	must(t, s.Close())
	s, c = start(t, opts)
	check()
}
