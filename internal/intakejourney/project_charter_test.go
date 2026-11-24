package intakejourney

import (
	context "context"
	errors "errors"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	filepath "path/filepath"
	"reflect"
	sync "sync"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/charter"
	config "github.com/kpenfound/osmia/internal/config"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestProjectCharterRemainsReadableWhileMemberTurnsDrain(t *testing.T) {
	for _, mode := range []string{"removal", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newTwoProjectFixture(t)
			member := f.cfg.For(otherProject).Project
			repo, err := trace.Open(f.cfg.Root, member)
			fixturekit.Must(t, err)
			_, err = trace.EnsureWorkgroups(ctx, repo, []config.WorkstreamID{journey.OtherStream}, f.clock.Now(), intake.ServiceActor, "fixture")
			fixturekit.Must(t, err)
			fixturekit.Must(t, repo.Close())
			fixturekit.SaveProjectSettings(t, f.cfg.Root.String(), member)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			read := make(chan error, 1)
			f.turns.Mu.Lock()
			f.turns.Hooks[intakefixture.Turn(otherProject, "first")] = func(ctx context.Context) {
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
			s, c := journey.StartT(t, f.opts)
			intakefixture.Await(t, "the member's running turn", entered)
			finished := make(chan error, 1)
			if mode == "removal" {
				for _, id := range []config.ProjectID{fixturekit.Project, otherProject} {
					r, err := s.Repository(id)
					fixturekit.Must(t, err)
					state, err := r.Workflow(f.streams[id], trace.FeatureSubject)
					fixturekit.Must(t, err)
					_, err = r.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "resolve-project-reload", Revision: 1, Project: id, Workstream: f.streams[id], At: f.clock.Now(), Actor: intake.OwnerActor, Cause: "owner"}, Subject: trace.FeatureSubject, From: state.Value, To: intake.AbandonedState, Reason: "owner cancelled member"}})
					fixturekit.Must(t, err)
				}

				go func() {
					for _, id := range []config.ProjectID{fixturekit.Project, otherProject} {
						if _, err := c.RemoveProject(ctx, id); err != nil {
							finished <- err
							return
						}
					}
					finished <- nil
				}()
				intakefixture.Soon(t, "both projects draining", func() bool { return !s.Current().Active(fixturekit.Project) && !s.Current().Active(otherProject) })
				unblock()
			} else {
				go func() { finished <- s.Close() }()
			}
			select {
			case err := <-read:
				fixturekit.Must(t, err)
			case <-time.After(fixturekit.DemoTimeout):
				t.Fatal("draining turn could not read the project charter")
			}
			select {
			case err := <-finished:
				fixturekit.Must(t, err)
			case <-time.After(fixturekit.DemoTimeout):
				t.Fatal("projects did not finish draining")
			}
		})
	}
}

func TestProjectChartersAndContextsStayIndependentAcrossRestart(t *testing.T) {
	t.Parallel()
	opts, clone := journey.ProjectFixture(t)
	s, c := journey.StartT(t, opts)
	ctx := context.Background()
	added, err := c.AddProject(ctx, fixturekit.Request(clone))
	fixturekit.Must(t, err)
	second := filepath.Join(filepath.Dir(clone), "client")
	fixturekit.DemoGit(t, filepath.Dir(clone), "init", "-q", second)
	other, err := c.AddProject(ctx, api.ProjectAddRequest{Name: "Client", Upstream: "owner/client", Clone: second})
	fixturekit.Must(t, err)
	policies := map[config.ProjectID]string{added.Project.ID: "1. Server policy.\n", other.Project.ID: "1. Client policy.\n7. Preserve identifiers.\n"}
	for id, text := range policies {
		journey.WriteCharter(t, c, id, text)
	}
	check := func() {
		for id, text := range policies {
			got, err := c.Charter(ctx, id)
			fixturekit.Must(t, err)
			if got.Project != id || got.Content != text {
				t.Fatalf("project policy: %+v", got)
			}
			b, err := s.Context().Assemble(ctx, id, bundle.Scope{Role: intake.ArchitectRole})
			fixturekit.Must(t, err)
			if !reflect.DeepEqual(b.Charter.Rules, charter.Parse(text).Rules) {
				t.Fatalf("project context: %+v", b.Charter)
			}
		}
	}
	check()
	c.Close()
	fixturekit.Must(t, s.Close())
	s, c = journey.StartT(t, opts)
	check()
}
