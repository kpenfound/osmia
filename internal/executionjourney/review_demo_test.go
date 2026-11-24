package executionjourney

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"

	agent "github.com/kpenfound/agentcore/agent"
	kb "github.com/kpenfound/osmia/internal/kb"
	questions "github.com/kpenfound/osmia/internal/questions"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestExactReviewDemonstration(t *testing.T) {
	t.Parallel()
	p := &intakefixture.Faults{}
	f, masons, chief := newAskingMasonFixture(t, 1, independentPlan, p)
	defer f.stop(t)
	masons.Play[service.TestMasonTurnID("resume")] = intakefixture.ReportDone("Initial candidate")

	f.engine.Mu.Lock()
	for i := 1; i <= 8; i++ {
		f.engine.Turns[fmt.Sprintf("%s-continue-%d", intake.MasonAgent("dedupe"), i)] = masons.Turn
		f.engine.Turns[fmt.Sprintf("%s-recover-%d", intake.MasonAgent("dedupe"), i)] = masons.Turn
	}
	f.engine.Mu.Unlock()
	var reviews atomic.Int32
	var old, revised service.UnitReviewIdentity
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if strings.HasPrefix(req.Name, intake.MasonAgent("resume")+"-revise-") {
			if !strings.Contains(req.Prompt, "Handle retry token") {
				return nil, fmt.Errorf("revision lost the finding: %s", req.Prompt)
			}
			if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), fixturekit.MasonWrote), []byte("package trace\n// retry token handled\n"), 0600); err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Join(req.Workspace.Directory(), "docs"), 0700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "docs", "proof.md"), []byte("Retry proof\n"), 0600); err != nil {
				return nil, err
			}
			if err := intakefixture.ReportDone("Revised candidate")(ctx, req, tools); err != nil {
				return nil, err
			}
			return &agent.Result{ClaudeID: req.Name, ResultText: "Revised", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		if strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-review-") {
			listed, err := tools.ListTools(ctx, nil)
			if err != nil {
				return nil, err
			}
			var names []string
			for _, tool := range listed.Tools {
				names = append(names, tool.Name)
			}
			slices.Sort(names)
			if !slices.Equal(names, []string{questions.AskTool, "factory_context", "file_read", "project_context", "record_discovery", execution.VerdictTool, reviewtools.DiffToolName}) {
				return nil, fmt.Errorf("reviewer tools %+v", listed.Tools)
			}
			identity, err := reviewtools.IdentityInPrompt(req.Prompt)
			if err != nil {
				return nil, err
			}
			if body, err := fixturekit.CallTool(ctx, tools, reviewtools.DiffToolName, map[string]any{"paths": []string{fixturekit.MasonWrote}}); err != nil || !strings.Contains(body, identity.Candidate.Revision) || !strings.Contains(body, "+++ b/"+fixturekit.MasonWrote) {
				return nil, fmt.Errorf("workstream_diff %s: %v", body, err)
			}
			n := reviews.Add(1)
			if n == 1 {
				old = identity
			} else {
				revised = identity
			}
			if n == 2 {
				if err := intakefixture.Asks(p, "1")(ctx, req, turn, tools); err != nil {
					return nil, err
				}
				return &agent.Result{ClaudeID: req.Name, ResultText: "Asked", SessionDir: req.SessionDir, NumTurns: 1}, nil
			}
			v := service.UnitVerdict{Decision: "satisfactory", Summary: reviewSummary, Findings: []api.ReviewFinding{}}
			if n == 1 {
				v.Decision = "material_findings"
				v.Findings = []api.ReviewFinding{{Severity: "material", Evidence: "Retry proof fails", Action: "Handle retry token"}}
			}
			body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": v.Decision, "summary": v.Summary, "findings": v.Findings})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				return nil, fmt.Errorf("verdict %s: %v", body, err)
			}
			return &agent.Result{ClaudeID: req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
		}
		return chief.Turn(ctx, req, turn, tools)
	}
	f.engine.Mu.Unlock()
	f.answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		if !strings.Contains(req.Prompt, intakefixture.RelayedRuling) {
			return fmt.Errorf("reviewer did not receive the ruling")
		}
		v := service.UnitVerdict{Decision: "satisfactory", Summary: reviewSummary, Findings: []api.ReviewFinding{}}
		body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": v.Decision, "summary": v.Summary, "findings": v.Findings})
		if err != nil || !strings.Contains(body, `"recorded":true`) {
			return fmt.Errorf("answer verdict %s: %v", body, err)
		}
		return nil
	})
	stream, _ := f.builtAs(t, "exact-review")
	mapping, err := kb.Load(f.repository())
	fixturekit.Must(t, err)
	mapping.Entities = append(mapping.Entities, kb.Entity{ID: "proof-docs", Name: "Proof documents", Paths: []string{"docs"}})
	fixturekit.Must(t, kb.Store(context.Background(), f.repository(), mapping, f.clock.Now(), execution.ReviewerActor, "review-context"))
	f.awaitUnit(t, stream, "resume", intake.UnitWaiting)
	intakefixture.Soon(t, "the reviewer's question turn to wait", func() bool {
		turns := f.thread(t, stream, intake.ReviewerAgent("resume")).Turns
		return len(turns) >= 2 && turns[1].Status() == questions.Waiting
	})
	if reviews.Load() != 2 || old.Candidate.Revision == revised.Candidate.Revision || old.Candidate.BaseRevision != revised.Candidate.BaseRevision {
		t.Fatalf("reviewed candidates: old %+v, revised %+v, turns %d", old, revised, reviews.Load())
	}
	f.stop(t)
	f.start(t)
	f.awaitUnit(t, stream, "resume", intake.UnitWaiting)
	f.rule(t, "1")
	f.awaitUnit(t, stream, "resume", intake.UnitMerged)
	if reviews.Load() != 2 {
		t.Fatalf("review turns: %d", reviews.Load())
	}
	if reason := execution.StaleReview(old, revised); !strings.HasPrefix(reason, "stale candidate revision") {
		t.Fatalf("old candidate accepted: %q", reason)
	}
	var results []service.UnitReviewResult
	docs, err := trace.Read[trace.Document](f.repository(), stream)
	fixturekit.Must(t, err)
	for _, d := range docs {
		if d.ID == service.TestReviewDocument("resume") {
			var result service.UnitReviewResult
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

	var refreshed, approved bool
	for _, move := range intakefixture.AllTransitions(t, f.s.cfg.Root, f.project, stream) {
		if move.Subject != trace.UnitSubject("resume") {
			continue
		}
		refreshed = refreshed || move.From == intake.UnitReviewing && move.To == intake.UnitReviewing
		approved = approved || move.To == intake.UnitApproved && strings.Contains(move.Reason, revised.Candidate.Revision)
	}
	if refreshed || !approved || f.question(t, stream, "1").State != trace.QuestionAnswered {
		t.Fatalf("missing exact approval or ruling: refreshed %t, approved %t", refreshed, approved)
	}
	thread := f.thread(t, stream, intake.ReviewerAgent("resume"))
	if thread.Identity.Role != intake.ReviewerRole || len(thread.Turns) != 3 || thread.Turns[1].Status() != questions.Waiting {
		t.Fatalf("reviewer requests and responses: %+v", thread.Turns)
	}
	masons.Check(t)
	p.Check(t)
}
