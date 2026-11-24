package executionjourney

import (
	context "context"
	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	strings "strings"
	testing "testing"
	time "time"
)

func newDebateFixtureWith(t *testing.T, members, rounds int, extra string, prepare func(*Options)) *shedFixture {
	t.Helper()
	opts, clone, engine, sessions, clock := newArchitectOptions(t)
	intakefixture.ConfigureDebate(t, opts.Config.Root, members, rounds, extra)
	opts.Committee = &intake.Committee{Engine: engine, Hosts: opts.Architect.Hosts}
	if prepare != nil {
		prepare(&opts)
	}
	f := &architectFixture{opts: opts, clone: clone, engine: engine, sessions: sessions, clock: clock}
	f.start(t)
	f.project, f.trace = intakefixture.AddArchitectProject(t, f.c, clone, intakefixture.ShedCharter, func(t *testing.T, p config.ProjectID, text string) { writeCharter(t, f.c, p, text) })
	f.s.cfg = f.s.current()
	f.scenario().ScriptDebate(rounds)
	return &shedFixture{architectFixture: f, members: members}
}

type shedFixture struct {
	*architectFixture
	members int
}

func newDebateFixture(t *testing.T, members, rounds int) *shedFixture {
	return newDebateFixtureWith(t, members, rounds, "", nil)
}

func (f *shedFixture) answer(id string, run fixturekit.FakeTurn) string {
	return intakefixture.Answer(f.engine, id, run)
}

func (f *shedFixture) rule(t *testing.T, id string) {
	t.Helper()
	intakefixture.Rule(t, f.c, id)
}

func (f *shedFixture) thread(t *testing.T, stream config.WorkstreamID, member string) trace.Thread {
	t.Helper()
	return intakefixture.Thread(t, f.repository(), stream, member)
}

func (f *shedFixture) question(t *testing.T, stream config.WorkstreamID, id string) trace.QuestionState {
	t.Helper()
	return intakefixture.Question(t, f.repository(), stream, id)
}

func (f *shedFixture) awaitQuestion(t *testing.T, stream config.WorkstreamID, id, state string) {
	t.Helper()
	intakefixture.AwaitQuestion(t, f.repository(), stream, id, state)
}

func (f *shedFixture) awaitTurn(t *testing.T, stream config.WorkstreamID, member, turn string) trace.QueuedTurn {
	t.Helper()
	return intakefixture.AwaitNamedTurn(t, f.repository(), stream, member, turn)
}

func (f *shedFixture) inboxEntries(t *testing.T, stream config.WorkstreamID, kind string) []api.InboxEntry {
	t.Helper()
	return f.scenario().InboxEntries(t, stream, kind)
}

const independentPlan = `{"version": 2, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "task": "Resume from the last chunk", "acceptance": ["spec#1 holds, shown by TestResume (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "task": "Skip acknowledged chunks", "acceptance": ["spec#2 holds, shown by no chunk is sent twice (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": [], "footprint": ["internal.trace"]}
]}
`

func (f *shedFixture) builtAs(t *testing.T, key string) (config.WorkstreamID, trace.OperationRecord) {
	t.Helper()
	return f.scenario().BuiltAs(t, key)
}

func (f *architectFixture) checkUnits(t *testing.T, stream config.WorkstreamID, want []api.UnitStatus) {
	t.Helper()
	wait, close := serviceChanges(t, f.s)
	t.Cleanup(close)
	intakefixture.CheckUnits(t, f.c, f.checkStatusError, wait, stream, want)
}

func (f *architectFixture) checkStatusError(t *testing.T, err error, elapsed time.Duration) {
	t.Helper()
	intakefixture.CheckStatusError(t, intakefixture.StatusService{Done: f.s.done, Wait: f.s.Wait, Socket: f.s.Socket, Ready: f.s.ready.Load}, err, elapsed)
}

func landingReason(t *testing.T, lands *foreman, repository *trace.Repository, stream config.WorkstreamID, unit string) string {
	t.Helper()
	approval := &foreman{masons: &masons{repository: repository}}
	return intakefixture.LandingReason(t, stream, unit, approval.approval,
		func(result service.UnitReviewResult) service.UnitReviewIdentity { return result.Identity },
		func(ctx context.Context, stream config.WorkstreamID, in intakefixture.LandingApproval, result service.UnitReviewResult) (string, error) {
			return lands.current(ctx, stream, execution.LandInput{Unit: in.Unit, Review: in.Review, Candidate: in.Candidate, Base: in.Base}, result)
		})
}

func (f *shedFixture) builtPaused(t *testing.T, target runtime.Target, keys ...string) []config.WorkstreamID {
	t.Helper()
	return intakefixture.BuiltPaused(t, target, keys, f.shedAs, func() {
		mutation(t, f.c, "PUT", "pause", api.PauseRequest{Target: target, Mode: "soft", Source: "owner"})
	}, f.ratifiedBuild)
}

func (f *shedFixture) shedAs(t *testing.T, key string) config.WorkstreamID {
	t.Helper()
	return f.scenario().ShedAs(t, key)
}

func (f *shedFixture) ratifiedBuild(t *testing.T, stream config.WorkstreamID) trace.OperationRecord {
	t.Helper()
	return f.scenario().RatifiedBuild(t, stream)
}

func (f *shedFixture) raisedContests(t *testing.T, stream config.WorkstreamID) []api.InboxEntry {
	t.Helper()
	var entries []api.InboxEntry
	intakefixture.Soon(t, "a contest to reach the inbox", func() bool {
		entries = f.inboxEntries(t, stream, api.InboxContested)
		return len(entries) != 0
	})
	return entries
}

func (f *shedFixture) awaitDeferral(t *testing.T, stream config.WorkstreamID, unit, reason string) {
	t.Helper()
	wait, close := serviceChanges(t, f.s)
	defer close()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	want := execution.DispatchDeferred + " " + reason
	for {
		decisions := f.dispatches(t, stream, unit)
		if len(decisions) > 0 && decisions[len(decisions)-1] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unit %s dispatches %q, want %q", unit, decisions, want)
		}
		wait()
	}
}

func (f *shedFixture) awaitCompleted(t *testing.T, stream config.WorkstreamID, agent, turn string) trace.QueuedTurn {
	t.Helper()
	return intakefixture.AwaitCompleted(t, f.repository(), stream, agent, turn)
}

func newReviewFixture(t *testing.T, key string) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f, _ := newMasonFixture(t, 1, independentPlan)
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	seedReview(t, f, repository, stream, "resume", resumeReport)
	return f, stream, repository
}

func (f *shedFixture) seedBuildingPaused(t *testing.T, target runtime.Target, graph string, keys ...string) []config.WorkstreamID {
	t.Helper()
	mutation(t, f.c, "PUT", "pause", api.PauseRequest{Target: target, Mode: "soft", Source: "owner"})
	var streams []config.WorkstreamID
	for _, key := range keys {
		streams = append(streams, f.seedBuilding(t, key, graph))
	}
	return streams
}

func moveUnitNow(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, unit, to string) {
	t.Helper()
	intakefixture.MoveUnitNow(t, f.clock.Now(), execution.ForemanActor, repository, stream, unit, to)
}

func reloadFails(t *testing.T, c *Client, path, field string) {
	t.Helper()
	intakefixture.ReloadFails(t, c.Reload, path, field)
}
