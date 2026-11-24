package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const landingPlan = `{"version": 2, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "task": "Resume from the last chunk", "acceptance": ["spec#1 holds, shown by TestResume (new-test)"], "criteria": ["spec#1"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "audit", "title": "Record acknowledged chunks", "task": "Record acknowledged chunks", "acceptance": ["spec#2 holds, shown by acknowledgements are recorded (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "task": "Skip acknowledged chunks", "acceptance": ["spec#2 holds, shown by no chunk is sent twice (reviewer-judgement)"], "criteria": ["spec#2"], "depends_on": ["resume"], "footprint": ["internal.trace"]}
]}
`

var landingUnits = map[string]struct{ criterion, file string }{
	"resume": {"spec#1", ""},
	"audit":  {"spec#2", "internal/trace/audit.go"},
	"dedupe": {"spec#2", "internal/trace/dedupe.go"},
}

func TestLandingDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := journey.NewMasonFixture(t, 1, landingPlan)
	defer f.Stop(t)

	for unit, u := range landingUnits {
		masons.Play[execution.MasonTurnID(unit)] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
			if u.file != "" {
				if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), u.file), []byte("package trace\n// "+unit+"\n"), 0644); err != nil {
					return err
				}
			}
			recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Built " + unit})
			if err != nil || !recorded {
				return fmt.Errorf("done refused: %q %v", reason, err)
			}
			return nil
		}
	}
	demo := &journey.LandingDemo{Reviews: map[string][]service.UnitReviewIdentity{}}
	f.Engine.Mu.Lock()
	f.Engine.Turns[execution.MasonTurnID("audit")] = masons.Turn
	chief := f.Engine.Turns["*"]
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for unit := range landingUnits {
			if strings.HasPrefix(req.Name, intake.ReviewerAgent(unit)+"-review-") {
				return demo.Review(ctx, unit, req, tools)
			}
		}
		return chief(ctx, req, verified, tools)
	}
	f.Engine.Mu.Unlock()

	const (
		held       = "crash: the landing commit was made"
		afterMoved = "crash: the feature branch moved"
	)
	var moving atomic.Bool
	f.S.Boundary = func(name string) error {
		switch {
		case name == "land-committed" && !moving.Load():
			return errors.New(held)
		case name == "land-advanced" && moving.Load():
			return errors.New(afterMoved)
		}
		return nil
	}
	stream, _ := f.BuiltAs(t, "landing-demo")
	base := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "rev-parse", intake.FeatureBranch(stream)))
	f.AwaitUnit(t, stream, "resume", intake.UnitApproved)
	f.AwaitUnit(t, stream, "audit", intake.UnitApproved)
	intakefixture.Soon(t, "the first landing to be interrupted", func() bool {
		ops := journey.LandOperations(t, f.Repository(), stream)
		return len(ops) == 1 && slices.ContainsFunc(ops[0].History, func(a trace.OperationAction) bool { return a.Kind == "retry" })
	})
	f.Stop(t)
	if got := landedCommits(f, t, stream, base); len(got) != 0 {
		t.Fatalf("an interrupted landing moved the feature branch: %v", got)
	}
	approvedAudit := demo.Reviewed("audit")
	if len(approvedAudit) != 1 || approvedAudit[0].Candidate.BaseRevision != base {
		t.Fatalf("audit's reviews before the first landing: %+v", approvedAudit)
	}

	repository, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	ops := journey.LandOperations(t, repository, stream)
	first, err := execution.DecodeLand(ops[0].Operation)
	fixturekit.Must(t, err)
	if first.Unit != "resume" || first.Base != base {
		t.Fatalf("the first landing is %+v, not resume's on %s", first, base)
	}
	moving.Store(true)
	if _, err := (&journey.Foreman{Masons: journey.NewMasonController(f.S, repository)}).Apply(ctx, ops[0].Operation); err == nil || err.Error() != afterMoved {
		t.Fatalf("the interrupted landing: %v", err)
	}
	fixturekit.Must(t, repository.Close())
	moved := landedCommits(f, t, stream, base)
	if len(moved) != 1 {
		t.Fatalf("the feature branch holds %v", moved)
	}
	f.Start(t)

	for _, unit := range []string{"resume", "audit", "dedupe"} {
		f.AwaitMerged(t, stream, unit)
	}
	masons.Check(t)
	landings := journey.LandingByUnit(t, f.Repository(), stream)
	decoded := map[string]api.UnitLanding{}
	for unit, d := range landings {
		var l api.UnitLanding
		fixturekit.Must(t, json.Unmarshal([]byte(d.Content), &l))
		decoded[unit] = l
	}

	resume := decoded["resume"]
	interrupted := ops[0].Operation.ID
	ops = journey.LandOperations(t, f.Repository(), stream)
	ops = slices.DeleteFunc(ops, func(o trace.OperationRecord) bool { return o.Operation.ID != interrupted })
	if len(ops) != 1 || resume.Commit != moved[0] || resume.Operation != interrupted || resume.Candidate != first.Candidate || resume.Base != base || !slices.Equal(resume.Criteria, []string{"spec#1"}) {
		t.Fatalf("resume's landing %+v, want commit %s of operation %s", resume, moved[0], interrupted)
	}
	if ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
		t.Fatalf("the first landing's result %+v", ops[0].Result)
	}
	for _, a := range ops[0].History {
		if a.Kind == "retry" && !strings.Contains(a.Failure, held) {
			t.Fatalf("the first landing's retry %+v", a)
		}
	}
	if message := strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(f.Clone), "-C", f.Clone, "log", "-1", "--format=%B", resume.Commit)); message != strings.TrimSpace(resume.Message) || !strings.Contains(message, "Osmia-Candidate: "+resume.Candidate) || !strings.Contains(message, execution.LandingTrailer+": "+resume.Operation) {
		t.Fatalf("resume's landing commit message:\n%s", message)
	}

	commits := landedCommits(f, t, stream, base)
	slices.Reverse(commits)
	if len(commits) != 3 || commits[0] != resume.Commit {
		t.Fatalf("the feature branch holds %v; resume landed as %s", commits, resume.Commit)
	}
	order := map[string]int{}
	for unit, l := range decoded {
		order[unit] = slices.Index(commits, l.Commit)
		if order[unit] < 0 {
			t.Fatalf("unit %s landed as %s, which the feature branch %v lacks", unit, l.Commit, commits)
		}
		parent := base
		if order[unit] > 0 {
			parent = commits[order[unit]-1]
		}
		if l.Base != parent || l.Branch != intake.FeatureBranch(stream) || !slices.Equal(l.Criteria, []string{landingUnits[unit].criterion}) {
			t.Fatalf("unit %s landed %+v on %v", unit, l, commits)
		}
		reviews := demo.Reviewed(unit)
		if last := reviews[len(reviews)-1]; last.Candidate.Revision != l.Candidate || last.Candidate.BaseRevision != l.Base {
			t.Fatalf("unit %s landed candidate %s from %s; its last review was of %+v", unit, l.Candidate, l.Base, last.Candidate)
		}
	}
	if order["dedupe"] < order["resume"] {
		t.Fatalf("dedupe landed before resume: %v", order)
	}

	transitions := intakefixture.AllTransitions(t, f.S.Cfg.Root, f.Project, stream)
	merged := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == trace.UnitSubject("resume")+"-"+intake.UnitMerged })
	ready := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == trace.UnitSubject("dedupe")+"-"+intake.UnitReady })
	started := slices.IndexFunc(transitions, func(tr trace.Transition) bool {
		return tr.Subject == trace.UnitSubject("dedupe") && tr.To == intake.UnitImplementing
	})
	if merged < 0 || ready < merged || started < ready || transitions[ready].Cause != resume.Operation || transitions[merged].Cause != resume.Operation {
		t.Fatalf("resume merged at %d, dedupe ready at %d and started at %d", merged, ready, started)
	}

	rebases := journey.UnitRebases(t, f.Repository(), stream, "audit")
	if len(rebases) == 0 || rebases[0].State != intake.UnitApproved || rebases[0].Onto != resume.Commit || rebases[0].Snapshot != approvedAudit[0].Candidate.Revision || len(rebases[0].Conflicts) != 0 {
		t.Fatalf("audit's rebases %+v", rebases)
	}
	back := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == trace.UnitSubject("audit")+"-checking-rebase-1" })
	if back < merged || transitions[back].From != intake.UnitApproved || !strings.HasPrefix(transitions[back].Reason, "the approval no longer holds: ") {
		t.Fatalf("audit's return to review at %d: %+v", back, transitions[max(back, 0)])
	}
	reviews := demo.Reviewed("audit")
	if len(reviews) < 2 || execution.StaleReview(approvedAudit[0], reviews[1]) == "" || reviews[1].Candidate.Revision != rebases[0].Commit || reviews[1].Candidate.BaseRevision != resume.Commit {
		t.Fatalf("audit's reviews %+v after rebase %+v", reviews, rebases[0])
	}
	if audit := decoded["audit"]; audit.Candidate == approvedAudit[0].Candidate.Revision || audit.Base == base {
		t.Fatalf("audit landed its approval from before the rebase: %+v", audit)
	}

	st, err := f.C.Status(ctx, stream)
	fixturekit.Must(t, err)
	if len(st.Units) != 3 {
		t.Fatalf("status units %+v", st.Units)
	}
	for _, u := range st.Units {
		if u.State != intake.UnitMerged || u.Landing == nil || u.Landing.Commit != decoded[u.Unit].Commit || u.Landing.Candidate != decoded[u.Unit].Candidate || u.Landing.Approval != decoded[u.Unit].Approval || !slices.Equal(u.Landing.Criteria, decoded[u.Unit].Criteria) {
			t.Fatalf("status of unit %s: %+v", u.Unit, u)
		}
	}
}
