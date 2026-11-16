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
	sync "sync"
	atomic "sync/atomic"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const landingPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": ["resume"], "footprint": ["internal.trace"]}
]}
`

var landingUnits = map[string]struct{ criterion, file string }{
	"resume": {"spec#1", ""},
	"audit":  {"spec#2", "internal/trace/audit.go"},
	"dedupe": {"spec#2", "internal/trace/dedupe.go"},
}

type landingDemo struct {
	mu      sync.Mutex
	reviews map[string][]UnitReviewIdentity
}

func (d *landingDemo) reviewed(unit string) []UnitReviewIdentity {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.reviews[unit])
}

func (d *landingDemo) review(ctx context.Context, unit string, req agent.Request, tools *mcp.ClientSession) (*agent.Result, error) {
	return intakefixture.LandingReview(ctx, unit, req, tools, func(unit string, identity UnitReviewIdentity) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.reviews[unit] = append(d.reviews[unit], identity)
	})
}

func landingByUnit(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) map[string]trace.Document {
	t.Helper()
	return intakefixture.LandingByUnit(t, repository, stream, landingDocument)
}

func TestLandingDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := newMasonFixture(t, 1, landingPlan)
	defer f.stop(t)

	for unit, u := range landingUnits {
		masons.Play[masonTurnID(unit)] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
			if u.file != "" {
				if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), u.file), []byte("package trace\n// "+unit+"\n"), 0644); err != nil {
					return err
				}
			}
			recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Built " + unit})
			if err != nil || !recorded {
				return fmt.Errorf("done refused: %q %v", reason, err)
			}
			return nil
		}
	}
	demo := &landingDemo{reviews: map[string][]UnitReviewIdentity{}}
	f.engine.Mu.Lock()
	f.engine.Turns[masonTurnID("audit")] = masons.Turn
	chief := f.engine.Turns["*"]
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for unit := range landingUnits {
			if strings.HasPrefix(req.Name, reviewerAgent(unit)+"-review-") {
				return demo.review(ctx, unit, req, tools)
			}
		}
		return chief(ctx, req, verified, tools)
	}
	f.engine.Mu.Unlock()

	const (
		held       = "crash: the landing commit was made"
		afterMoved = "crash: the feature branch moved"
	)
	var moving atomic.Bool
	f.s.boundary = func(name string) error {
		switch {
		case name == "land-committed" && !moving.Load():
			return errors.New(held)
		case name == "land-advanced" && moving.Load():
			return errors.New(afterMoved)
		}
		return nil
	}
	stream, _ := f.builtAs(t, "landing-demo")
	base := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "rev-parse", featureBranch(stream)))
	f.awaitUnit(t, stream, "resume", UnitApproved)
	f.awaitUnit(t, stream, "audit", UnitApproved)
	deadline := time.Now().Add(demoTimeout)
	for {
		ops := landOperations(t, f.repository(), stream)
		if len(ops) == 1 && slices.ContainsFunc(ops[0].History, func(a trace.OperationAction) bool { return a.Kind == "retry" }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first landing was never interrupted: %+v", ops)
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.stop(t)
	if got := f.landedCommits(t, stream, base); len(got) != 0 {
		t.Fatalf("an interrupted landing moved the feature branch: %v", got)
	}
	approvedAudit := demo.reviewed("audit")
	if len(approvedAudit) != 1 || approvedAudit[0].Candidate.BaseRevision != base {
		t.Fatalf("audit's reviews before the first landing: %+v", approvedAudit)
	}

	repository, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	ops := landOperations(t, repository, stream)
	first, err := decodeLand(ops[0].Operation)
	must(t, err)
	if first.Unit != "resume" || first.Base != base {
		t.Fatalf("the first landing is %+v, not resume's on %s", first, base)
	}
	moving.Store(true)
	if _, err := (&foreman{masons: newMasonController(f.s, repository)}).Apply(ctx, ops[0].Operation); err == nil || err.Error() != afterMoved {
		t.Fatalf("the interrupted landing: %v", err)
	}
	must(t, repository.Close())
	moved := f.landedCommits(t, stream, base)
	if len(moved) != 1 {
		t.Fatalf("the feature branch holds %v", moved)
	}
	f.start(t)

	for _, unit := range []string{"resume", "audit", "dedupe"} {
		f.awaitMerged(t, stream, unit)
	}
	masons.Check(t)
	landings := landingByUnit(t, f.repository(), stream)
	decoded := map[string]UnitLanding{}
	for unit, d := range landings {
		var l UnitLanding
		must(t, json.Unmarshal([]byte(d.Content), &l))
		decoded[unit] = l
	}

	resume := decoded["resume"]
	interrupted := ops[0].Operation.ID
	ops = landOperations(t, f.repository(), stream)
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
	if message := strings.TrimSpace(demoGit(t, filepath.Dir(f.clone), "-C", f.clone, "log", "-1", "--format=%B", resume.Commit)); message != strings.TrimSpace(resume.Message) || !strings.Contains(message, "Osmia-Candidate: "+resume.Candidate) || !strings.Contains(message, landingTrailer+": "+resume.Operation) {
		t.Fatalf("resume's landing commit message:\n%s", message)
	}

	commits := f.landedCommits(t, stream, base)
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
		if l.Base != parent || l.Branch != featureBranch(stream) || !slices.Equal(l.Criteria, []string{landingUnits[unit].criterion}) {
			t.Fatalf("unit %s landed %+v on %v", unit, l, commits)
		}
		reviews := demo.reviewed(unit)
		if last := reviews[len(reviews)-1]; last.Candidate.Revision != l.Candidate || last.Candidate.BaseRevision != l.Base {
			t.Fatalf("unit %s landed candidate %s from %s; its last review was of %+v", unit, l.Candidate, l.Base, last.Candidate)
		}
	}
	if order["dedupe"] < order["resume"] {
		t.Fatalf("dedupe landed before resume: %v", order)
	}

	transitions := allTransitions(t, f.s.cfg.Root, f.project, stream)
	merged := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == trace.UnitSubject("resume")+"-"+UnitMerged })
	ready := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == trace.UnitSubject("dedupe")+"-"+UnitReady })
	started := slices.IndexFunc(transitions, func(tr trace.Transition) bool {
		return tr.Subject == trace.UnitSubject("dedupe") && tr.To == UnitImplementing
	})
	if merged < 0 || ready < merged || started < ready || transitions[ready].Cause != resume.Operation || transitions[merged].Cause != resume.Operation {
		t.Fatalf("resume merged at %d, dedupe ready at %d and started at %d", merged, ready, started)
	}

	rebases := unitRebases(t, f.repository(), stream, "audit")
	if len(rebases) == 0 || rebases[0].State != UnitApproved || rebases[0].Onto != resume.Commit || rebases[0].Snapshot != approvedAudit[0].Candidate.Revision || len(rebases[0].Conflicts) != 0 {
		t.Fatalf("audit's rebases %+v", rebases)
	}
	back := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == trace.UnitSubject("audit")+"-checking-rebase-1" })
	if back < merged || transitions[back].From != UnitApproved || !strings.HasPrefix(transitions[back].Reason, "the approval no longer holds: ") {
		t.Fatalf("audit's return to review at %d: %+v", back, transitions[max(back, 0)])
	}
	reviews := demo.reviewed("audit")
	if len(reviews) < 2 || staleReview(approvedAudit[0], reviews[1]) == "" || reviews[1].Candidate.Revision != rebases[0].Commit || reviews[1].Candidate.BaseRevision != resume.Commit {
		t.Fatalf("audit's reviews %+v after rebase %+v", reviews, rebases[0])
	}
	if audit := decoded["audit"]; audit.Candidate == approvedAudit[0].Candidate.Revision || audit.Base == base {
		t.Fatalf("audit landed its approval from before the rebase: %+v", audit)
	}

	st, err := f.c.Status(ctx, stream)
	must(t, err)
	if len(st.Units) != 3 {
		t.Fatalf("status units %+v", st.Units)
	}
	for _, u := range st.Units {
		if u.State != UnitMerged || u.Landing == nil || u.Landing.Commit != decoded[u.Unit].Commit || u.Landing.Candidate != decoded[u.Unit].Candidate || u.Landing.Approval != decoded[u.Unit].Approval || !slices.Equal(u.Landing.Criteria, decoded[u.Unit].Criteria) {
			t.Fatalf("status of unit %s: %+v", u.Unit, u)
		}
	}
}
