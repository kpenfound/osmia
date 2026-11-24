package executionjourney

import (
	context "context"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	lifecycle "github.com/kpenfound/osmia/internal/lifecycle"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
)

func newCloneFixture(t *testing.T) *cloneFixture {
	t.Helper()
	data := fixturekit.NewCloneSetup(t)
	opts := journey.TestOptions(data.Config)
	opts.Workstreams = nil
	opts.ReviewChecks = fixturekit.PassingChecks
	opts.Reconciliation.Now = data.Clock.Now
	return &cloneFixture{opts: opts, clone: data.Clone, engine: data.Engine, sessions: data.Sessions, clock: data.Clock}
}

func ensureWorkgroups(s *journey.Service, ctx context.Context, cfg *config.Config, projects []*journey.ActiveProject, archives []runtime.Archive) error {
	var ps []*lifecycle.Project
	for _, p := range projects {
		ps = append(ps, &lifecycle.Project{ID: p.Id, Repository: p.Repository})
	}
	return s.Lifecycle().EnsureWorkgroups(ctx, cfg, ps, archives)
}

type cloneFixture struct {
	opts     journey.Options
	clone    string
	engine   *fixturekit.DemoEngine
	sessions *fixturekit.DemoSessions
	clock    *fixturekit.DemoClock
}

func notifyDiagnostics(t *testing.T, c *journey.Client) (status, cfg []api.Diagnostic) {
	t.Helper()
	st, err := c.Statuses(context.Background())
	fixturekit.Must(t, err)
	cr, err := c.Configuration(context.Background())
	fixturekit.Must(t, err)
	pick := func(ds []api.Diagnostic) []api.Diagnostic {
		var out []api.Diagnostic
		for _, d := range ds {
			if d.Field == "notify" {
				out = append(out, d)
			}
		}
		return out
	}
	return pick(st.Diagnostics), pick(cr.Diagnostics)
}
