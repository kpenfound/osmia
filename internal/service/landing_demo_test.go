package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// landingPlan has two independent units, resume and audit, and dedupe,
// which depends on resume.
const landingPlan = `{"version": 1, "units": [
  {"id": "resume", "title": "Resume from the last chunk", "addresses": [{"criterion": "spec#1", "proof": {"kind": "new-test", "name": "TestResume"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "audit", "title": "Record acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "acknowledgements are recorded"}}], "depends_on": [], "footprint": ["internal.trace"]},
  {"id": "dedupe", "title": "Skip acknowledged chunks", "addresses": [{"criterion": "spec#2", "proof": {"kind": "reviewer-judgement", "name": "no chunk is sent twice"}}], "depends_on": ["resume"], "footprint": ["internal.trace"]}
]}
`

// landingLearning is what resume's mason reports it learned, and
// landedKnowledge the knowledge base prose the fake librarian folds it into.
const (
	landingLearning = "Trace snapshots require a clean worktree."
	landedKnowledge = "# Internal\n\nPrevious project knowledge.\n\n" + landingLearning + "\n"
)

// landingUnits gives each unit of landingPlan its criterion and the file its
// mason writes besides masonWrote.
var landingUnits = map[string]struct{ criterion, file string }{
	"resume": {"spec#1", ""},
	"audit":  {"spec#2", "internal/trace/audit.go"},
	"dedupe": {"spec#2", "internal/trace/dedupe.go"},
}

// landingDemo records what the fake reviewer and librarian saw.
type landingDemo struct {
	mu        sync.Mutex
	reviews   map[string][]UnitReviewIdentity
	refreshed []string
}

func (d *landingDemo) reviewed(unit string) []UnitReviewIdentity {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.reviews[unit])
}

// review approves the exact candidate a review turn names.
func (d *landingDemo) review(ctx context.Context, unit string, req agent.Request, tools *mcp.ClientSession) (*agent.Result, error) {
	identity, err := reviewIdentityInPrompt(req.Prompt)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.reviews[unit] = append(d.reviews[unit], identity)
	d.mu.Unlock()
	evidence := []ReviewEvidence{{Criterion: landingUnits[unit].criterion, Evidence: "The planned proof holds on the candidate diff"}}
	body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "evidence": evidence, "findings": []ReviewFinding{}})
	if err != nil || !strings.Contains(body, `"recorded":true`) {
		return nil, fmt.Errorf("verdict %s: %v", body, err)
	}
	return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
}

// refresh folds the landed unit's learnings into kb/internal.md, reading
// its landing and report from source/.
func (d *landingDemo) refresh(ctx context.Context, entities string, req agent.Request, tools *mcp.ClientSession) (*agent.Result, error) {
	var landing UnitLanding
	var report UnitReport
	for path, into := range map[string]any{"source/landing.json": &landing, "source/report.json": &report} {
		body, err := readTool(ctx, tools, path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if err := json.Unmarshal([]byte(body), into); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if landing.Unit != report.Unit || landing.Candidate != report.Candidate || !strings.Contains(req.Prompt, landing.Commit) {
		return nil, fmt.Errorf("refresh source landing %+v, report of %s on %s", landing, report.Unit, report.Candidate)
	}
	if landing.Unit == "resume" && !slices.Equal(report.Learnings, []string{landingLearning}) {
		return nil, fmt.Errorf("resume's learnings %v", report.Learnings)
	}
	d.mu.Lock()
	d.refreshed = append(d.refreshed, landing.Unit)
	d.mu.Unlock()
	for path, content := range map[string]string{"output/kb/internal.md": landedKnowledge, "output/kb/entities.json": entities} {
		if _, err := callTool(ctx, tools, "file_write", map[string]any{"path": path, "content": content}); err != nil {
			return nil, err
		}
	}
	return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Refreshed", SessionDir: req.SessionDir, NumTurns: 1}, nil
}

// landingByUnit returns the latest units/<unit>/landing.json of each unit.
func landingByUnit(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) map[string]trace.Document {
	t.Helper()
	docs, err := trace.Read[trace.Document](repository, stream)
	must(t, err)
	out := map[string]trace.Document{}
	for _, d := range docs {
		if d.Unit != "" && d.ID == landingDocument(d.Unit) {
			out[d.Unit] = d
		}
	}
	return out
}

// TestM3LandingDemonstration lands three units of one workstream through
// the local API and trace with fake masons, reviewer and librarian. See
// docs/m3-exit.md.
func TestM3LandingDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := newMasonFixture(t, 1, landingPlan)
	defer f.stop(t)
	f.stop(t)
	configFile, err := os.OpenFile(filepath.Join(f.opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString(librarianContainer)
	must(t, errors.Join(err, configFile.Close()))
	f.opts.Librarian = &Librarian{Engine: f.engine, Hosts: f.opts.Architect.Hosts}
	f.start(t)
	must(t, f.repository().RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: "subsystem-internal", Revision: 1, Project: f.project, At: f.clock.Now(), Actor: librarianActor, Cause: "fixture"}, Path: "kb/internal.md", Content: "# Internal\n\nPrevious project knowledge.\n"}}))
	mapping, err := kb.Load(f.repository())
	must(t, err)
	entities, err := kb.Encode(mapping)
	must(t, err)

	for unit, u := range landingUnits {
		masons.play[masonTurnID(unit)] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
			if u.file != "" {
				if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), u.file), []byte("package trace\n// "+unit+"\n"), 0644); err != nil {
					return err
				}
			}
			args := map[string]any{"outcome": "Built " + unit, "criteria": []any{criterionArgs(CriterionReport{Criterion: u.criterion, Done: "built " + unit, Evidence: "the planned proof holds", Proof: "reviewer judgement"})}}
			if unit == "resume" {
				args["criteria"], args["learnings"] = []any{criterionArgs(resumeReport)}, []string{landingLearning}
			}
			recorded, reason, err := done(ctx, tools, args)
			if err != nil || !recorded {
				return fmt.Errorf("done refused: %q %v", reason, err)
			}
			return nil
		}
	}
	demo := &landingDemo{reviews: map[string][]UnitReviewIdentity{}}
	f.engine.mu.Lock()
	f.engine.turns[masonTurnID("audit")] = masons.turn
	chief := f.engine.turns["*"]
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		for unit := range landingUnits {
			if strings.HasPrefix(req.Name, reviewerAgent(unit)+"-review-") {
				return demo.review(ctx, unit, req, tools)
			}
		}
		if strings.HasPrefix(req.Name, "refresh-") {
			return demo.refresh(ctx, string(entities), req, tools)
		}
		return chief(ctx, req, verified, tools)
	}
	f.engine.mu.Unlock()

	// The first landing is interrupted after its commit is made, until the
	// service stops, so audit is approved on the base resume lands on.
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

	// The service stops between moving the feature branch and recording the
	// landing.
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
	masons.check(t)
	landings := landingByUnit(t, f.repository(), stream)
	decoded := map[string]UnitLanding{}
	for unit, d := range landings {
		var l UnitLanding
		must(t, json.Unmarshal([]byte(d.Content), &l))
		decoded[unit] = l
	}

	// The restart recorded the interrupted landing: one commit, found by its
	// operation trailer, with its provenance.
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

	// The feature branch holds one commit per unit, each on the one before,
	// resume first and dedupe after the unit it depends on.
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

	// dedupe became ready only when resume merged, by the landing.
	transitions := allTransitions(t, f.trace, stream)
	merged := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == trace.UnitSubject("resume")+"-"+UnitMerged })
	ready := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == trace.UnitSubject("dedupe")+"-"+UnitReady })
	started := slices.IndexFunc(transitions, func(tr trace.Transition) bool {
		return tr.Subject == trace.UnitSubject("dedupe") && tr.To == UnitImplementing
	})
	if merged < 0 || ready < merged || started < ready || transitions[ready].Cause != resume.Operation || transitions[merged].Cause != resume.Operation {
		t.Fatalf("resume merged at %d, dedupe ready at %d and started at %d", merged, ready, started)
	}

	// audit's approved workspace was rebased onto resume's landing, and that
	// approval returned to review; the rebased candidate was reviewed again
	// before it landed.
	rebases := unitRebases(t, f.repository(), stream, "audit")
	if len(rebases) == 0 || rebases[0].State != UnitApproved || rebases[0].Onto != resume.Commit || rebases[0].Snapshot != approvedAudit[0].Candidate.Revision || len(rebases[0].Conflicts) != 0 {
		t.Fatalf("audit's rebases %+v", rebases)
	}
	back := slices.IndexFunc(transitions, func(tr trace.Transition) bool { return tr.ID == trace.UnitSubject("audit")+"-reviewing-rebase-1" })
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

	// The librarian folded resume's learning into kb/internal.md; the source
	// ledger links the revision to resume's landing, and dedupe's mason read
	// it in its bundle, which resume's mason did not.
	var refresh trace.OperationRecord
	lib, err := f.repository().Operations(librarianWorkstream(f.project))
	must(t, err)
	for _, op := range lib {
		var in refreshInput
		if op.Operation.Action == RefreshAction && json.Unmarshal(op.Operation.Input, &in) == nil && in.Unit == "resume" {
			refresh = op
			if in.Workstream != stream || in.Commit != resume.Commit || in.Landing != landings["resume"].Revision {
				t.Fatalf("refresh source %+v", in)
			}
		}
	}
	demo.mu.Lock()
	refreshed := slices.Clone(demo.refreshed)
	demo.mu.Unlock()
	if refresh.Result == nil || refresh.Result.Outcome != "succeeded" || len(refreshed) == 0 || refreshed[0] != "resume" {
		t.Fatalf("resume's refresh %+v; refreshed %v", refresh, refreshed)
	}
	docs, err := trace.Read[trace.Document](f.repository(), "")
	must(t, err)
	var ledger, prose trace.Document
	for _, d := range docs {
		switch d.ID {
		case "kb-sources":
			ledger = d
		case "subsystem-internal":
			if d.Cause == refresh.Operation.ID {
				prose = d
			}
		}
	}
	var sources []KnowledgeSource
	must(t, json.Unmarshal([]byte(ledger.Content), &sources))
	i := slices.IndexFunc(sources, func(s KnowledgeSource) bool { return s.Operation == refresh.Operation.ID })
	if prose.Content != landedKnowledge || i < 0 || sources[i].Subsystem != "internal" || sources[i].Revision != prose.Revision || sources[i].Unit != "resume" || sources[i].Commit != resume.Commit || sources[i].Landing != landings["resume"].Revision {
		t.Fatalf("knowledge provenance %+v; prose %+v", sources, prose.Header)
	}
	var resumePrompt, dedupePrompt string
	for _, req := range masons.requests(stream) {
		switch req.Name {
		case masonTurnID("resume"):
			resumePrompt = req.Prompt
		case masonTurnID("dedupe"):
			dedupePrompt = req.Prompt
		}
	}
	if !strings.Contains(dedupePrompt, landingLearning) || resumePrompt == "" || strings.Contains(resumePrompt, landingLearning) {
		t.Fatalf("the refreshed knowledge in the mason bundles: resume %t, dedupe %t", strings.Contains(resumePrompt, landingLearning), strings.Contains(dedupePrompt, landingLearning))
	}

	// The status API shows each unit's landing provenance.
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
