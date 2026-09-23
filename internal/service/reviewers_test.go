package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func reviewEvidence() []ReviewEvidence {
	return []ReviewEvidence{{Criterion: "spec#1", Evidence: "The planned proof passes on the candidate diff"}}
}

func TestReviewerVerdictToolAndOutcome(t *testing.T) {
	t.Parallel()
	scope := coreadapter.Scope{Workstream: "stream", Unit: "resume", Thread: "reviewer-resume", Turn: "review-1", Role: reviewerRole}
	reports := &reviewerReports{}
	tool := reports.tool(scope)
	for _, input := range []UnitVerdict{{Decision: "satisfactory"}, {Decision: "material_findings", Evidence: reviewEvidence()}} {
		data, _ := json.Marshal(input)
		out, err := tool.Handle(context.Background(), data)
		if err != nil || !strings.Contains(string(out), `"recorded":false`) {
			t.Fatalf("accepted incomplete verdict %s: %s %v", data, out, err)
		}
	}
	good := UnitVerdict{Decision: "material_findings", Evidence: reviewEvidence(), Findings: []ReviewFinding{{Criterion: "spec#1", Severity: "material", Evidence: "The retry fails", Action: "Handle the retry token"}}}
	data, _ := json.Marshal(good)
	out, err := tool.Handle(context.Background(), data)
	if err != nil || !strings.Contains(string(out), `"recorded":true`) {
		t.Fatalf("verdict refused: %s %v", out, err)
	}
	turns := &verdictTurns{Turns: staticVerdictTurn{}, reports: reports}
	result, err := turns.Run(context.Background(), coreadapter.PreparedTurn{Scope: scope})
	if err != nil || result.Outcome == nil || result.Outcome.Status != verdictOutcome {
		t.Fatalf("outcome %+v %v", result.Outcome, err)
	}
	var got UnitVerdict
	if err := json.Unmarshal([]byte(result.Outcome.Report), &got); err != nil || got.Findings[0].Action != good.Findings[0].Action {
		t.Fatalf("report %+v %v", got, err)
	}
}

type staticVerdictTurn struct{}

func (staticVerdictTurn) Run(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return coreadapter.SessionResult{}, nil
}

func TestReviewerSendBackResubmitAndApprove(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 1, independentPlan)
	defer f.stop(t)
	masons.play[masonTurnID("resume")] = reportDone("Built")
	chief := &chief{p: &faults{}, released: map[string]bool{}, held: map[string]chan struct{}{}}
	var reviews atomic.Int32
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
			listed, err := tools.ListTools(ctx, nil)
			if err != nil {
				return nil, err
			}
			var names []string
			for _, tool := range listed.Tools {
				names = append(names, tool.Name)
			}
			slices.Sort(names)
			if !slices.Equal(names, []string{questions.AskTool, "file_read", verdictTool}) {
				return nil, fmt.Errorf("reviewer tools %+v", listed.Tools)
			}
			n := reviews.Add(1)
			v := UnitVerdict{Decision: "satisfactory", Evidence: reviewEvidence(), Findings: []ReviewFinding{}}
			if n == 1 {
				v.Decision = "material_findings"
				v.Findings = []ReviewFinding{{Criterion: "spec#1", Severity: "material", Evidence: "Retry proof fails", Action: "Handle retry token"}}
			}
			body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": v.Decision, "evidence": v.Evidence, "findings": v.Findings})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		if strings.HasPrefix(req.Name, masonAgent("resume")+"-revise-") {
			if !strings.Contains(req.Prompt, "Handle retry token") {
				return nil, fmt.Errorf("missing actionable finding: %s", req.Prompt)
			}
			if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), masonWrote), []byte("package trace\n// retry token handled\n"), 0600); err != nil {
				return nil, err
			}
			if err := reportDone("Revised")(ctx, req, tools); err != nil {
				return nil, err
			}
			return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Revised", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief.turn(ctx, req, verified, tools)
	}
	f.engine.mu.Unlock()
	stream, _ := f.builtAs(t, "review-cycle")
	deadline := time.Now().Add(demoTimeout)
	for {
		state, err := f.repository().Workflow(stream, trace.UnitSubject("resume"))
		if err != nil {
			t.Fatal(err)
		}
		if state.Value == UnitApproved {
			break
		}
		if time.Now().After(deadline) {
			thread, _ := f.repository().Thread(stream, reviewerAgent("resume"))
			mason, _ := f.repository().Thread(stream, masonAgent("resume"))
			f.engine.mu.Lock()
			runs := append([]string(nil), f.engine.runs...)
			f.engine.mu.Unlock()
			t.Fatalf("unit %s, reviewer status %s turns %d, mason status %s turns %d, runs %v", state.Value, thread.Status, len(thread.Turns), mason.Status, len(mason.Turns), runs)
		}
		time.Sleep(50 * time.Millisecond)
	}
	masons.check(t)
	th, err := f.repository().Thread(stream, reviewerAgent("resume"))
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Turns) != 2 || th.Identity.Role != reviewerRole || reviews.Load() != 2 {
		t.Fatalf("review identity or turns: %+v, runs %d", th, reviews.Load())
	}
	docs, err := trace.Read[trace.Document](f.repository(), stream)
	if err != nil {
		t.Fatal(err)
	}
	var results []UnitReviewResult
	for _, d := range docs {
		if d.ID == reviewDocument("resume") {
			var v UnitReviewResult
			if json.Unmarshal([]byte(d.Content), &v) == nil && v.Turn != "" {
				results = append(results, v)
			}
		}
	}
	if len(results) != 2 || results[0].Identity.Candidate.Revision == results[1].Identity.Candidate.Revision || results[0].Identity.Candidate.SpecRevision == "" || results[1].Identity.Candidate.BaseRevision == "" {
		t.Fatalf("result identities %+v", results)
	}
}

func TestReviewResultReconcilesAfterRestart(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 1, independentPlan)
	stopped := false
	defer func() {
		if !stopped {
			f.stop(t)
		}
	}()
	masons.play[masonTurnID("resume")] = reportDone("Built")
	stream, _ := f.builtAs(t, "recovery")
	f.awaitUnit(t, stream, "resume", UnitReviewing)
	// The service may run a failed fake reviewer turn. Stop it, then persist a
	// complete exact result as if the process stopped before the transition.
	f.stop(t)
	stopped = true
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	r := &reviewers{masons: newMasonController(f.s, repo)}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := r.prepareUnitReview(context.Background(), stream, "resume")
	if err != nil {
		t.Fatal(err)
	}
	turn := reviewTurnID("resume", state.Version)
	result := UnitReviewResult{Identity: identity, Turn: turn, Verdict: UnitVerdict{Decision: "satisfactory", Evidence: reviewEvidence()}}
	data, _ := json.Marshal(result)
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		t.Fatal(err)
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == reviewDocument("resume") {
			latest = d
		}
	}
	latest.Revision++
	latest.Content = string(data) + "\n"
	latest.At = time.Now()
	latest.Cause = turn
	if err := repo.RecordDocuments(context.Background(), []trace.Document{latest}); err != nil {
		t.Fatal(err)
	}
	if err := r.one(context.Background(), stream, "resume", state, false); err != nil {
		t.Fatal(err)
	}
	if err := r.one(context.Background(), stream, "resume", state, false); err != nil {
		t.Fatal(err)
	}
	approved, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	if err != nil || approved.Value != UnitApproved {
		t.Fatalf("recovered %+v %v", approved, err)
	}
}

func TestInterruptedReviewQueuesOneContinuation(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 1, independentPlan)
	stopped := false
	defer func() {
		if !stopped {
			f.stop(t)
		}
	}()
	masons.play[masonTurnID("resume")] = reportDone("Built")
	entered := make(chan struct{})
	chief := &chief{p: &faults{}, released: map[string]bool{}, held: map[string]chan struct{}{}}
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return chief.turn(ctx, req, turn, tools)
	}
	f.engine.mu.Unlock()
	stream, _ := f.builtAs(t, "interrupted-review")
	select {
	case <-entered:
	case <-time.After(demoTimeout):
		t.Fatal("review turn did not start")
	}
	f.stop(t)
	stopped = true
	repo, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	r := &reviewers{masons: newMasonController(f.s, repo)}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.one(context.Background(), stream, "resume", state, false); err != nil {
		t.Fatal(err)
	}
	if err := r.one(context.Background(), stream, "resume", state, false); err != nil {
		t.Fatal(err)
	}
	th, err := repo.Thread(stream, reviewerAgent("resume"))
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Turns) != 2 || th.Turns[0].Status() != "interrupted" || th.Turns[1].CompletedAt != (time.Time{}) {
		t.Fatalf("recovery turns: %+v", th.Turns)
	}
}

func TestReviewerQuestionResumesSameCandidateAfterOwnerAnswer(t *testing.T) {
	t.Parallel()
	p := &faults{}
	f, masons, chief := newAskingMasonFixture(t, 1, independentPlan, p)
	defer f.stop(t)
	masons.play[masonTurnID("resume")] = reportDone("Built")
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
			if err := asks(p, "1")(ctx, req, turn, tools); err != nil {
				return nil, err
			}
			return &agent.Result{ClaudeID: "asked", ResultText: "Asked", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief.turn(ctx, req, turn, tools)
	}
	f.engine.mu.Unlock()
	f.answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, relayedRuling) {
			return fmt.Errorf("missing ruling: %s", req.Prompt)
		}
		body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "evidence": reviewEvidence(), "findings": []ReviewFinding{}})
		if err != nil || !strings.Contains(body, `"recorded":true`) {
			return fmt.Errorf("verdict %s: %v", body, err)
		}
		return nil
	})
	stream, _ := f.builtAs(t, "review-question")
	f.awaitUnit(t, stream, "resume", UnitWaiting)
	docs, err := trace.Read[trace.Document](f.repository(), stream)
	must(t, err)
	var before UnitReviewIdentity
	for _, d := range docs {
		if d.ID == reviewDocument("resume") {
			_ = json.Unmarshal([]byte(d.Content), &before)
		}
	}
	if before.Candidate.Revision == "" {
		t.Fatal("review identity was not recorded")
	}
	f.stop(t)
	f.start(t)
	f.awaitUnit(t, stream, "resume", UnitWaiting)
	f.rule(t, "1")
	f.awaitUnit(t, stream, "resume", UnitApproved)
	r := &reviewers{masons: newMasonController(f.s, f.repository())}
	result, ok, err := r.storedResult(stream, "resume", trace.WorkflowState{Value: UnitApproved})
	must(t, err)
	if !ok || result.Identity.Candidate.Revision != before.Candidate.Revision || result.Turn != questions.TurnID("1") {
		t.Fatalf("review after answer: %+v", result)
	}
	th := f.thread(t, stream, reviewerAgent("resume"))
	if len(th.Turns) != 2 || th.Turns[0].Status() != questions.Waiting {
		t.Fatalf("reviewer turns: %+v", th.Turns)
	}
	p.check(t)
	masons.check(t)
}

func TestContestedReviewRulingSurvivesRestart(t *testing.T) {
	t.Parallel()
	f, masons := newMasonFixture(t, 1, independentPlan)
	defer f.stop(t)
	f.s.cfg.Shed.MaxBounces = 1
	masons.play[masonTurnID("resume")] = reportDone("Built")
	chief := &chief{p: &faults{}, released: map[string]bool{}, held: map[string]chan struct{}{}}
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, reviewerAgent("resume")+"-review-") {
			body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "material_findings", "evidence": reviewEvidence(), "findings": []ReviewFinding{{Criterion: "spec#1", Severity: "material", Evidence: "Retry fails", Action: "Handle retry"}}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: "reviewed", ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief.turn(ctx, req, turn, tools)
	}
	f.engine.mu.Unlock()
	stream, _ := f.builtAs(t, "contested")
	f.awaitUnit(t, stream, "resume", UnitContested)
	r := &reviewers{masons: newMasonController(f.s, f.repository())}
	result, ok, err := r.storedResult(stream, "resume", trace.WorkflowState{Value: UnitContested})
	must(t, err)
	if !ok || result.Bounces != 1 {
		t.Fatalf("bounce count: %+v", result)
	}
	status, err := f.c.Status(context.Background(), stream)
	if err != nil || !slices.Contains(status.Gates, trace.OwnerGate{Kind: UnitContested, Reference: "resume"}) {
		t.Fatalf("contested gate: %+v %v", status.Gates, err)
	}
	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "review", "Check the candidate once more"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "review", "Decide twice"); err == nil {
		t.Fatal("accepted duplicate ruling")
	}
	deadline := time.Now().Add(demoTimeout)
	for {
		result, ok, err = r.storedResult(stream, "resume", trace.WorkflowState{Value: UnitContested})
		must(t, err)
		state, err := f.repository().Workflow(stream, trace.UnitSubject("resume"))
		must(t, err)
		if ok && result.Bounces == 2 && state.Value == UnitContested {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second review did not contest: %+v", result)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := f.c.RuleContested(context.Background(), stream, "resume", "revise", "The mason should fix the retry proof"); err != nil {
		t.Fatal(err)
	}
	f.stop(t)
	f.start(t)
	f.awaitUnit(t, stream, "resume", UnitImplementing)
	deadline = time.Now().Add(demoTimeout)
	var th trace.Thread
	for {
		th = f.thread(t, stream, masonAgent("resume"))
		if len(th.Turns) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("revision turn was not queued: %+v", th.Turns)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(th.Turns) != 2 || (!strings.Contains(th.Turns[1].Request.Prompt, "Handle retry") || !strings.Contains(th.Turns[1].Request.Prompt, "The mason should fix the retry proof")) {
		t.Fatalf("revision turn: %+v", th.Turns)
	}
	if _, found, err := latestContestedRuling(f.repository(), stream, "resume", 2); err != nil || !found {
		t.Fatalf("ruling lost: %v %v", found, err)
	}
	masons.check(t)
}
