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
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestExactReviewDemonstration exercises review through the local API and
// trace. Mason and reviewer turns are played by a scripted fake model
// session here, not a live one, so a real model's judgment of the candidate
// or its findings is left unverified; the test checks the service's own
// handling of identities, retries and the question.
func TestExactReviewDemonstration(t *testing.T) {
	t.Parallel()
	p := &faults{}
	f, masons, chief := newAskingMasonFixture(t, 1, independentPlan, p)
	defer f.stop(t)
	masons.play[masonTurnID("resume")] = reportDone("Initial candidate")
	var reviews atomic.Int32
	var old, revised UnitReviewIdentity
	f.engine.mu.Lock()
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, masonAgent("resume")+"-revise-") {
			if !strings.Contains(req.Prompt, "Handle retry token") {
				return nil, fmt.Errorf("revision lost the finding: %s", req.Prompt)
			}
			if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), masonWrote), []byte("package trace\n// retry token handled\n"), 0600); err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Join(req.Workspace.Directory(), "docs"), 0700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "docs", "proof.md"), []byte("Retry proof\n"), 0600); err != nil {
				return nil, err
			}
			if err := reportDone("Revised candidate")(ctx, req, tools); err != nil {
				return nil, err
			}
			return &agent.Result{ClaudeID: req.Name, ResultText: "Revised", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
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
			if !slices.Equal(names, []string{questions.AskTool, "file_read", verdictTool, workstreamDiffTool}) {
				return nil, fmt.Errorf("reviewer tools %+v", listed.Tools)
			}
			identity, err := reviewIdentityInPrompt(req.Prompt)
			if err != nil {
				return nil, err
			}
			if body, err := callTool(ctx, tools, workstreamDiffTool, map[string]any{"paths": []string{masonWrote}}); err != nil || !strings.Contains(body, identity.Candidate.Revision) || !strings.Contains(body, "+++ b/"+masonWrote) {
				return nil, fmt.Errorf("workstream_diff %s: %v", body, err)
			}
			n := reviews.Add(1)
			if n == 1 {
				old = identity
			} else {
				revised = identity
			}
			if n == 2 {
				if err := asks(p, "1")(ctx, req, turn, tools); err != nil {
					return nil, err
				}
				return &agent.Result{ClaudeID: req.Name, ResultText: "Asked", SessionDir: req.SessionDir, NumTurns: 1}, nil
			}
			v := UnitVerdict{Decision: "satisfactory", Summary: reviewSummary, Findings: []ReviewFinding{}}
			if n == 1 {
				v.Decision = "material_findings"
				v.Findings = []ReviewFinding{{Severity: "material", Evidence: "Retry proof fails", Action: "Handle retry token"}}
			}
			body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": v.Decision, "summary": v.Summary, "findings": v.Findings})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief.turn(ctx, req, turn, tools)
	}
	f.engine.mu.Unlock()
	f.answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, relayedRuling) {
			return fmt.Errorf("reviewer did not receive the ruling")
		}
		v := UnitVerdict{Decision: "satisfactory", Summary: reviewSummary, Findings: []ReviewFinding{}}
		body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": v.Decision, "summary": v.Summary, "findings": v.Findings})
		if err != nil || !strings.Contains(body, `"recorded":true`) {
			return fmt.Errorf("answer verdict %s: %v", body, err)
		}
		return nil
	})
	stream, _ := f.builtAs(t, "exact-review")
	mapping, err := kb.Load(f.repository())
	must(t, err)
	mapping.Entities = append(mapping.Entities, kb.Entity{ID: "proof-docs", Name: "Proof documents", Paths: []string{"docs"}})
	must(t, kb.Store(context.Background(), f.repository(), mapping, f.clock.Now(), reviewerActor, "review-context"))
	deadline := time.Now().Add(demoTimeout)
	for {
		state, err := f.repository().Workflow(stream, trace.UnitSubject("resume"))
		must(t, err)
		if state.Value == UnitWaiting {
			break
		}
		if time.Now().After(deadline) {
			thread, _ := f.repository().Thread(stream, reviewerAgent("resume"))
			f.engine.mu.Lock()
			runs := append([]string(nil), f.engine.runs...)
			f.engine.mu.Unlock()
			t.Fatalf("waiting for reviewer question: state %s, reviews %d, thread %+v, runs %v", state.Value, reviews.Load(), thread.Turns, runs)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if reviews.Load() != 2 || old.Candidate.Revision == revised.Candidate.Revision || old.Candidate.BaseRevision != revised.Candidate.BaseRevision {
		t.Fatalf("reviewed candidates: old %+v, revised %+v, turns %d", old, revised, reviews.Load())
	}
	f.stop(t)
	f.start(t)
	f.awaitUnit(t, stream, "resume", UnitWaiting)
	f.rule(t, "1")
	f.awaitUnit(t, stream, "resume", UnitMerged)
	if reviews.Load() != 2 {
		t.Fatalf("review turns: %d", reviews.Load())
	}
	if reason := staleReview(old, revised); !strings.HasPrefix(reason, "stale candidate revision") {
		t.Fatalf("old candidate accepted: %q", reason)
	}
	var results []UnitReviewResult
	docs, err := trace.Read[trace.Document](f.repository(), stream)
	must(t, err)
	for _, d := range docs {
		if d.ID == reviewDocument("resume") {
			var result UnitReviewResult
			if json.Unmarshal([]byte(d.Content), &result) == nil && result.Turn != "" {
				results = append(results, result)
			}
		}
	}
	if len(results) != 2 || results[0].Verdict.Decision != "material_findings" || results[1].Verdict.Decision != "satisfactory" || results[1].Verdict.Summary != reviewSummary {
		t.Fatalf("review trace: %+v", results)
	}
	if results[1].Identity != revised || results[1].Identity.Candidate.SpecRevision == "" || results[1].Identity.Candidate.PlanRevision == "" {
		t.Fatalf("approved identity: %+v", results[1].Identity)
	}
	// The revision also changed docs/proof.md, outside the unit's footprint;
	// the reviewer's satisfactory verdict approves it as it stands.
	var refreshed, approved bool
	for _, move := range allTransitions(t, f.trace, stream) {
		if move.Subject != trace.UnitSubject("resume") {
			continue
		}
		refreshed = refreshed || move.From == UnitReviewing && move.To == UnitReviewing
		approved = approved || move.To == UnitApproved && strings.Contains(move.Reason, revised.Candidate.Revision)
	}
	if refreshed || !approved || f.question(t, stream, "1").State != trace.QuestionAnswered {
		t.Fatalf("missing exact approval or ruling: refreshed %t, approved %t", refreshed, approved)
	}
	thread := f.thread(t, stream, reviewerAgent("resume"))
	if thread.Identity.Role != reviewerRole || len(thread.Turns) != 3 || thread.Turns[1].Status() != questions.Waiting {
		t.Fatalf("reviewer requests and responses: %+v", thread.Turns)
	}
	masons.check(t)
	p.check(t)
}
