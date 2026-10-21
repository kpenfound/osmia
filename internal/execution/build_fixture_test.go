package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// buildFixture drives stopped controllers against a real trace and local workspaces.
// The fake turns, checks, clock and sealed-document seeding are shared primitives.
type buildFixture struct {
	unitsFixture
	s     *Controller
	clock *fixturekit.DemoClock
}

func newBuildFixture(t *testing.T, key string) *buildFixture {
	t.Helper()
	u := newUnitsFixture(t)
	ctx := context.Background()
	clone := u.cfg.Project.Clone
	must(t, os.MkdirAll(filepath.Join(clone, "internal", "trace"), 0700))
	must(t, os.WriteFile(filepath.Join(clone, "internal", "trace", "git.go"), []byte("package trace\n"), 0600))
	demoGit(t, u.home, "-C", clone, "add", "internal")
	demoGit(t, u.home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "trace")
	u.base = strings.TrimSpace(demoGit(t, u.home, "-C", clone, "rev-parse", "HEAD"))
	demoGit(t, u.home, "-C", clone, "branch", "-f", featureBranch(stream), u.base)
	mapping, err := kb.Seed(clone)
	must(t, err)
	must(t, kb.Store(ctx, u.repository, mapping, demoStart, registrationActor, "fixture"))
	fixturekit.RecordCharter(t, u.repository, intakefixture.ShedCharter)
	f := &buildFixture{unitsFixture: u, clock: &fixturekit.DemoClock{At: demoStart}}
	f.s = New(Dependencies{
		RefreshPendingStreams: func(*trace.Repository) (map[config.WorkstreamID]bool, error) {
			return map[config.WorkstreamID]bool{}, nil
		},
		RefreshBudget: func(context.Context, *trace.Repository) error { return nil },
		AcquireCheckSlot: func(context.Context, config.ProjectID, config.WorkstreamID, string) (func(), error) {
			return func() {}, nil
		},
		RefreshPending:       func(*trace.Repository) (bool, error) { return false, nil },
		EdgeWaiting:          func(*trace.Repository, config.WorkstreamID) (bool, error) { return false, nil },
		ChangeRepositoryHeld: func(*trace.Repository, config.WorkstreamID, coreadapter.Operation) (bool, error) { return false, nil },
		WorkgroupEdgePending: func(*trace.Repository, config.WorkstreamID) (bool, error) { return false, nil },
		CarryBase:            func(*trace.Repository, config.WorkstreamID, string) (string, bool, error) { return "", false, nil },
		CarriedOnto: func(*trace.Repository, config.WorkstreamID, string) (trace.UpstreamMove, bool, error) {
			return trace.UpstreamMove{}, false, nil
		},

		Now:       f.clock.Now,
		Effective: func() (runtime.State, []runtime.Diagnostic) { return runtime.State{}, nil },
		About:     func(*trace.Repository) *config.Config { return u.cfg },
		RoleExecution: func(cfg *config.Config, role string) (coreadapter.Profile, coreadapter.ExecutionSettings, error) {
			return cfg.Execution(role, cfg.Roles[role].Profile)
		},
		ContextFor: func(*trace.Repository) bundle.Provider {
			return bundle.Files{Repository: func(config.ProjectID) (*trace.Repository, error) { return u.repository, nil }}
		},
		Step:            func(string) error { return nil },
		ChangeHeld:      func(*trace.Repository, config.WorkstreamID) (bool, error) { return false, nil },
		UnitDiscoveries: func(*trace.Repository, config.WorkstreamID, string) (string, string, error) { return "", "", nil },
		UnitDrift: func(*trace.Repository, config.WorkstreamID, string) (trace.UpstreamMove, bool, error) {
			return trace.UpstreamMove{}, false, nil
		},
		UnitRebaseConflicts: func(*trace.Repository, config.WorkstreamID, string, string) ([]string, error) { return nil, nil },
		Checks:              &fixturekit.FakeChecks{Outcomes: fixturekit.Passing()},
	})
	provider, err := intake.FeatureWorkspaces(u.cfg, u.repository).Of(stream)
	must(t, err)
	acquired, err := provider.Acquire(ctx, vcs.Request{Name: string(stream), Ref: u.base, Branch: featureBranch(stream)})
	must(t, err)
	fixturekit.RecordBuild(t, u.repository, fixturekit.BuildInput{Stream: stream, At: demoStart, Key: key, Graph: independentPlan, Spec: intakefixture.ValidSpec, Branch: featureBranch(stream), Workspace: acquired.Directory(), Base: u.base, BaseBranch: u.cfg.Project.BaseBranch})
	return f
}

func (f *buildFixture) controller() *masons {
	return &masons{s: f.s, cfg: f.cfg, repository: f.repository}
}

func seedChecking(t *testing.T, f *buildFixture, repository *trace.Repository, stream config.WorkstreamID, unit, outcome string) {
	t.Helper()
	m := f.controller()
	turns := intakefixture.TurnScenario{Root: func() config.Root { return f.cfg.Root }, Project: repository.Project(), Now: f.clock.Now, ForemanActor: foremanActor}
	intakefixture.SeedChecking(t, intakefixture.UnitSeedDriver[building]{Read: m.read, Start: m.start, Finish: m.finish, Open: f.units().open, Complete: turns.CompleteMasonTurn}, repository, stream, unit, outcome)
}

func completeMasonTurn(t *testing.T, f *buildFixture, repository *trace.Repository, stream config.WorkstreamID, unit, outcome string) trace.QueuedTurn {
	t.Helper()
	turns := intakefixture.TurnScenario{Root: func() config.Root { return f.cfg.Root }, Project: repository.Project(), Now: f.clock.Now, ForemanActor: foremanActor}
	return turns.CompleteMasonTurn(t, repository, stream, unit, outcome)
}

func attemptOperation(t *testing.T, s *Controller, repository *trace.Repository, stream config.WorkstreamID, op coreadapter.Operation, r coreadapter.Reconciler) (coreadapter.OperationResult, error) {
	t.Helper()
	return intakefixture.AttemptOperation(t, s.Now(), foremanActor, repository, stream, op, r)
}

type fakeChecks = fixturekit.FakeChecks
type checkOutcome = fixturekit.CheckOutcome

var passing = fixturekit.Passing
var failedReport = fixturekit.FailedReport
var streamDocuments = intakefixture.StreamDocuments

const passedReport = fixturekit.PassedReport
const masonWrote = fixturekit.MasonWrote
const resumeReport = "resume from the last chunk"

const independentPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": [], "footprint": ["internal.trace"]}
]}
`

func newMasonController(s *Controller, repository *trace.Repository) *masons {
	return &masons{s: s, cfg: s.About(repository), repository: repository}
}
func newReviewFixture(t *testing.T, key string) (*buildFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f := newBuildFixture(t, key)
	intakefixture.SeedReview(t, f.repository, stream, "resume", resumeReport, UnitReviewing, func(t *testing.T, r *trace.Repository, w config.WorkstreamID, u, o string) {
		seedChecking(t, f, r, w, u, o)
	}, func(t *testing.T, r *trace.Repository, w config.WorkstreamID) { runChecks(t, f.s, r, w) })
	return f, stream, f.repository
}
func approveDirectly(t *testing.T, s *Controller, repository *trace.Repository, stream config.WorkstreamID, unit, criterion string) {
	t.Helper()
	r := &reviewers{masons: newMasonController(s, repository)}
	intakefixture.ApproveDirectly(t, repository, stream, unit, r.prepareUnitReview, func(identity UnitReviewIdentity, turn string) ([]byte, error) {
		return json.Marshal(UnitReviewResult{Identity: identity, Turn: turn, Verdict: UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}})
	}, reviewTurnID, reviewDocument, r.one, UnitApproved)
}
func approvedReview(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) (trace.Document, UnitReviewResult) {
	t.Helper()
	f := &foreman{masons: &masons{repository: repository}}
	return intakefixture.ApprovedReview(t, stream, unit, f.approval)
}
func landOperations(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	t.Helper()
	return intakefixture.LandOperations(t, repository, stream, LandAction)
}
func (f *buildFixture) landedCommits(t *testing.T, stream config.WorkstreamID, base string) []string {
	t.Helper()
	return intakefixture.LandedCommits(t, f.home, f.cfg.Project.Clone, stream, base)
}

var jump = fixturekit.Jump
var allTransitions = intakefixture.AllTransitions
