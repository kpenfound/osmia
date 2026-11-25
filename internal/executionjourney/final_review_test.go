package executionjourney

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/plan"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func hostRunningService(t *testing.T, f *journey.ShedFixture, stream func() config.WorkstreamID) (string, *fixturekit.FakePulls) {
	t.Helper()
	fork, pulls := journey.HostService(t, f, stream)
	stopQuestionJourney(t, f)
	f.Start(t)
	return fork, pulls
}

type assembledFinal struct {
	f          *journey.ShedFixture
	stream     config.WorkstreamID
	repository *trace.Repository
	a          *journey.FinalReviewer
	head       string
	op         coreadapter.Operation
	charter    trace.Document
}

func assembleFinal(t *testing.T, key string) *assembledFinal {
	t.Helper()
	f, stream, repository, a := journey.NewFinalFixture(t, key)
	head, op := journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n\n// Resume resumes.\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n\n// Dedupe skips.\n"})
	charter, err := repository.Charter(context.Background())
	fixturekit.Must(t, err)
	return &assembledFinal{f: f, stream: stream, repository: repository, a: a, head: head, op: op, charter: charter}
}

func (a *assembledFinal) worktree() string {
	return filepath.Join(a.f.Opts.Config.Root, intake.BranchesDirectory, string(a.f.Project), string(a.stream))
}

func (a *assembledFinal) git(t *testing.T, args ...string) string {
	t.Helper()
	return strings.TrimSpace(fixturekit.DemoGit(t, filepath.Dir(a.f.Clone), args...))
}

type reviewedFinal struct {
	*assembledFinal
	upstream string
	landed   []string
	tip      string
	turn     string
	result   coreadapter.OperationResult
}

func recordFinalReport(ctx context.Context, tools *mcp.ClientSession) (string, error) {
	return fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Uploads resume and skip acknowledged chunks.", "findings": []any{
		map[string]any{"severity": "advisory", "criteria": []any{"spec#2"}, "finding": "Dedupe keeps acknowledged chunk IDs in memory only.", "action": "Persist them if restarts matter."}}})
}

func reportOnly(ctx context.Context, tools *mcp.ClientSession) error {
	_, err := recordFinalReport(ctx, tools)
	return err
}

func (a *assembledFinal) review(t *testing.T, script func(ctx context.Context, tools *mcp.ClientSession) error) *reviewedFinal {
	t.Helper()
	ctx := context.Background()
	landed := strings.Split(a.git(t, "-C", a.f.Clone, "log", "--format=%H %s", "-2", a.head), "\n")
	upstream := journey.AdvanceUpstream(t, a.f, map[string]string{"UPSTREAM.md": "upstream moved\n"})
	turn := a.f.FinalTurn(1, 1, script)
	result, err := a.a.Apply(ctx, a.op)
	fixturekit.Must(t, err)
	tip, _, err := intake.Workspaces(a.f.S.Cfg, intake.BranchesDirectory, config.WorkspacesGit).Branch(ctx, intake.FeatureBranch(a.stream))
	fixturekit.Must(t, err)
	return &reviewedFinal{assembledFinal: a, upstream: upstream, landed: landed, tip: tip, turn: turn, result: result}
}

func TestAssemblingEveryUnitMovesTheWorkstreamToAssembledAndAsksForTheFinalReview(t *testing.T) {
	t.Parallel()
	a := assembleFinal(t, "final")
	assembled := intakefixture.TransitionByID(t, a.repository, a.stream, intake.AssembledState)
	wantAssembled := fmt.Sprintf("every unit of the sealed plan has merged onto %s: resume, dedupe; the feature branch is rebased onto upstream and read against the sealed spec and the charter next", intake.FeatureBranch(a.stream))
	if assembled.From != intake.BuildingState || assembled.To != intake.AssembledState || assembled.Actor != execution.ForemanActor || assembled.Cause != "unit-dedupe-merged" || assembled.Reason != wantAssembled {
		t.Fatalf("the move to assembled %+v", assembled)
	}
	if body := intakefixture.NoticeOf(t, a.repository, a.stream, intake.AssembledState); body != "Workstream state changed from building to assembled: "+wantAssembled {
		t.Fatalf("the notice %q", body)
	}
	in, err := delivery.DecodeFinalReview(a.op)
	fixturekit.Must(t, err)
	if in != (delivery.FinalReviewInput{Review: 1, Commit: a.head, Seal: 1, Spec: 1, Plan: 1, Charter: a.charter.Revision}) {
		t.Fatalf("final review input %+v", in)
	}
}

func TestFinalReviewReadsTheRebasedBranchAsAWhole(t *testing.T) {
	t.Parallel()
	a := assembleFinal(t, "final")
	ctx := context.Background()
	var problems []error
	r := a.review(t, func(ctx context.Context, tools *mcp.ClientSession) error {
		for path, want := range map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan, "charter.md": intakefixture.ShedCharter,
			"branch/internal/trace/dedupe.go": "package trace\n\n// Dedupe skips.\n", "branch/UPSTREAM.md": "upstream moved\n"} {
			if got, err := intakefixture.ReadTool(ctx, tools, path); err != nil || got != want {
				problems = append(problems, fmt.Errorf("%s holds %q: %v", path, got, err))
			}
		}
		diff, err := fixturekit.CallTool(ctx, tools, reviewtools.DiffToolName, map[string]any{})
		if err != nil || !strings.Contains(diff, "internal/trace/resume.go") || !strings.Contains(diff, "internal/trace/dedupe.go") || strings.Contains(diff, "UPSTREAM.md") {
			problems = append(problems, fmt.Errorf("branch diff %q: %v", diff, err))
		}
		if listed, err := fixturekit.CallTool(ctx, tools, reviewtools.DiffToolName, map[string]any{"files_only": true, "paths": []string{"internal/trace/dedupe.go"}}); err != nil || !strings.Contains(listed, `"path":"internal/trace/dedupe.go"`) || strings.Contains(listed, "resume.go") {
			problems = append(problems, fmt.Errorf("branch files %q: %v", listed, err))
		}
		if report, err := intakefixture.ReadTool(ctx, tools, "units/resume/report.json"); err == nil {
			problems = append(problems, fmt.Errorf("a unit that never reported shows a report %q", report))
		}
		if got, err := recordFinalReport(ctx, tools); err != nil || !strings.Contains(got, `"recorded":true`) {
			problems = append(problems, fmt.Errorf("the report was not recorded: %s %v", got, err))
		}
		return nil
	})
	if err := errors.Join(problems...); err != nil {
		t.Fatal(err)
	}
	if r.result.Outcome != "succeeded" {
		t.Fatalf("final review result %+v", r.result)
	}

	g := intake.Workspaces(a.f.S.Cfg, intake.BranchesDirectory, config.WorkspacesGit)
	if on, err := g.Ancestor(ctx, r.upstream, r.tip); err != nil || !on || r.tip == a.head {
		t.Fatalf("the feature branch at %s is not rebased onto upstream %s: %v", r.tip, r.upstream, err)
	}
	replayed := strings.Split(a.git(t, "-C", a.f.Clone, "log", "--format=%H %s", r.upstream+".."+r.tip), "\n")
	if len(replayed) != len(r.landed) || slices.ContainsFunc(replayed, func(line string) bool { return slices.Contains(r.landed, line) }) {
		t.Fatalf("the rebased branch holds %v, landed %v", replayed, r.landed)
	}
	for i := range replayed {
		_, got, _ := strings.Cut(replayed[i], " ")
		_, was, _ := strings.Cut(r.landed[i], " ")
		if got != was {
			t.Fatalf("replayed %q, landed %q", replayed[i], r.landed[i])
		}
	}
	if at := a.git(t, "-C", a.worktree(), "rev-parse", "HEAD"); at != r.tip {
		t.Fatalf("the feature worktree is at %s, not %s", at, r.tip)
	}
	if status := a.git(t, "-C", a.worktree(), "status", "--porcelain"); status != "" {
		t.Fatalf("the feature worktree after the rebase:\n%s", status)
	}
	var rebase delivery.FinalRebase
	rebases := intakefixture.StreamDocuments(t, a.repository, a.stream, delivery.FinalRebaseDocument)
	if len(rebases) != 1 || rebases[0].Path != delivery.FinalRebasePath || json.Unmarshal([]byte(rebases[0].Content), &rebase) != nil ||
		rebase != (delivery.FinalRebase{Review: 1, Operation: a.op.ID, Branch: intake.FeatureBranch(a.stream), Upstream: seal.Base{Remote: "upstream", Branch: "main", Commit: r.upstream}, Before: a.head, Commit: r.tip}) {
		t.Fatalf("final rebase documents %+v", rebases)
	}
}

func TestFinalReportToolRefusesAFindingOfUnknownSeverityOrCriterion(t *testing.T) {
	t.Parallel()
	a := assembleFinal(t, "final")
	var problems []error
	r := a.review(t, func(ctx context.Context, tools *mcp.ClientSession) error {
		if got, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "x", "findings": []any{map[string]any{"severity": "minor", "finding": "x"}}}); err == nil || strings.Contains(got, `"recorded":true`) {
			problems = append(problems, fmt.Errorf("a finding of unknown severity was not refused: %s %v", got, err))
		}
		if got, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "x", "findings": []any{map[string]any{"severity": "advisory", "criteria": []any{"spec#9"}, "finding": "x"}}}); err != nil || !strings.Contains(got, `\"spec#9\" is not a criterion of the sealed spec`) {
			problems = append(problems, fmt.Errorf("a finding citing an unknown criterion was not refused: %s %v", got, err))
		}
		return reportOnly(ctx, tools)
	})
	if err := errors.Join(problems...); err != nil {
		t.Fatal(err)
	}
	if reports := intakefixture.StreamDocuments(t, a.repository, a.stream, delivery.FinalReportDocument); len(reports) != 1 || r.result.Outcome != "succeeded" {
		t.Fatalf("the refused reports left %+v, result %+v", reports, r.result)
	}
}

func TestFinalReviewRecordsItsReportAndTellsTheOwner(t *testing.T) {
	t.Parallel()
	a := assembleFinal(t, "final")
	ctx := context.Background()
	r := a.review(t, reportOnly)
	want := api.FinalReport{Review: 1, Operation: a.op.ID, Outcome: delivery.FinalReviewed, Branch: intake.FeatureBranch(a.stream), Before: a.head, Commit: r.tip,
		Upstream: &seal.Base{Remote: "upstream", Branch: "main", Commit: r.upstream}, Seal: 1, SpecHash: seal.SpecHash(fixturekit.ValidSpec), Spec: 1, Plan: 1, Charter: a.charter.Revision,
		Reader: intake.CommitteeAgent(1), Turn: r.turn, Summary: "Uploads resume and skip acknowledged chunks.", Criteria: []api.FinalCriterion{
			{Criterion: "spec#1", Text: "An interrupted upload resumes from the last acknowledged chunk.", Evidence: "served by merged units resume, each reviewed against its acceptance"},
			{Criterion: "spec#2", Text: "Acknowledged chunks are never sent again.", Evidence: "served by merged units dedupe, each reviewed against its acceptance"}},
		Findings: []api.FinalFinding{{Severity: delivery.FindingAdvisory, Criteria: []string{"spec#2"}, Finding: "Dedupe keeps acknowledged chunk IDs in memory only.", Action: "Persist them if restarts matter."}}}
	report, found, err := delivery.LatestFinalReport(a.repository, a.stream)
	if err != nil || !found || !reflect.DeepEqual(report, want) {
		t.Fatalf("final report %+v, %v, %v; want %+v", report, found, err, want)
	}
	reports := intakefixture.StreamDocuments(t, a.repository, a.stream, delivery.FinalReportDocument)
	if len(reports) != 1 || reports[0].Path != delivery.FinalReportPath || reports[0].Cause != a.op.ID || reports[0].Actor != delivery.FinalReviewActor {
		t.Fatalf("final report documents %+v", reports)
	}
	reviewed := intakefixture.TransitionByID(t, a.repository, a.stream, "final-review-1-reviewed")
	wantReason := fmt.Sprintf("final review 1 read %s at %s against seal 1 (spec revision 1, plan revision 1, charter revision %d): 0 blocking and 1 advisory; report final/report.json revision 1", intake.FeatureBranch(a.stream), r.tip, a.charter.Revision)
	if reviewed.Subject != delivery.FinalReviewSubject || reviewed.From != "requested-1" || reviewed.To != "reviewed-1" || reviewed.Cause != a.op.ID || reviewed.Reason != wantReason || r.result.Evidence != wantReason {
		t.Fatalf("the reviewed transition %+v, result %+v", reviewed, r.result)
	}
	if body := intakefixture.NoticeOf(t, a.repository, a.stream, reviewed.ID); body != "The final review of the assembled branch is recorded: 0 blocking, 1 advisory.\nAdvisory: spec#2: Dedupe keeps acknowledged chunk IDs in memory only. Action: Persist them if restarts matter.\nNothing blocks delivery; the owner decides it with the advisory findings in view.\n"+delivery.BriefNotice(report, reports[0].Revision) {
		t.Fatalf("the notice %q", body)
	}
	if _, reason, err := a.a.FinalGate(ctx, a.stream); err != nil || reason != "" {
		t.Fatalf("the current report does not authorise: %q %v", reason, err)
	}
}

func TestFinalReviewReaderIsToldTheResultOfTheChecksOnThePushedCommit(t *testing.T) {
	t.Parallel()
	a := assembleFinal(t, "final")
	a.review(t, reportOnly)
	if runs := intakefixture.StreamDocuments(t, a.repository, a.stream, delivery.FinalChecksDocument(1)); len(runs) != 1 || !strings.Contains(runs[0].Content, `"source": "pull_request"`) && !strings.Contains(runs[0].Content, `"source":"pull_request"`) {
		t.Fatalf("the pull request check result of the pushed commit: %+v", runs)
	}
	reader, err := a.repository.Thread(a.stream, intake.CommitteeAgent(1))
	fixturekit.Must(t, err)
	assertFinalCheckReviewInstructions(t, reader.Turns[len(reader.Turns)-1].Request.SystemPrompt)
	if prompt := reader.Turns[len(reader.Turns)-1].Request.Prompt; !strings.Contains(prompt, "Dagger Cloud ran the project's checks") || !strings.Contains(prompt, "The result is passed.") || strings.Index(prompt, "Check result:\n") > strings.Index(prompt, "Every criterion and the merged units") || !strings.Contains(prompt, "- final/checks-1.json:") ||
		!strings.Contains(prompt, "- spec#1: served by merged units resume, each reviewed against its acceptance\n- spec#2: served by merged units dedupe") || strings.Contains(prompt, "change since") {
		t.Fatalf("the reader's prompt lacks the check run:\n%s", prompt)
	}
}

func TestFinalReviewRunsOnceAcrossARepeatedApplyAndARestart(t *testing.T) {
	t.Parallel()
	a := assembleFinal(t, "final")
	ctx := context.Background()
	r := a.review(t, reportOnly)
	report, _, err := delivery.LatestFinalReport(a.repository, a.stream)
	fixturekit.Must(t, err)

	again, err := a.a.Apply(ctx, a.op)
	if err != nil || !reflect.DeepEqual(again, r.result) {
		t.Fatalf("applied again %+v: %v", again, err)
	}
	fixturekit.Must(t, a.a.Pass(ctx))
	if ops := journey.FinalOperations(t, a.repository, a.stream); len(ops) != 1 || len(intakefixture.StreamDocuments(t, a.repository, a.stream, delivery.FinalReportDocument)) != 1 {
		t.Fatalf("final review operations after the review %+v", ops)
	}

	fixturekit.Must(t, a.repository.Close())
	f := a.f
	f.Start(t)
	live := &journey.FinalReviewer{S: f.S, Repository: f.Repository()}
	if after, _, err := delivery.LatestFinalReport(f.Repository(), a.stream); err != nil || !reflect.DeepEqual(after, report) {
		t.Fatalf("the report after a restart %+v: %v", after, err)
	}
	if _, reason, err := live.FinalGate(ctx, a.stream); err != nil || reason != "" {
		t.Fatalf("the report after a restart does not authorise: %q %v", reason, err)
	}
	if ops := fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord {
		return journey.FinalOperations(t, f.Repository(), a.stream)
	}); len(ops) != 1 || !reflect.DeepEqual(ops[0].Result, &r.result) {
		t.Fatalf("final review operations after a restart %+v", ops)
	}
	f.Stop(t)
	repository, err := trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repository.Close()
	if ops := journey.FinalOperations(t, repository, a.stream); len(ops) != 1 {
		t.Fatalf("the restarted service asked for another final review: %+v", ops)
	}
}

func TestFinalReviewIsStaleWhenTheFeatureBranchMoves(t *testing.T) {
	t.Parallel()
	a := assembleFinal(t, "final")
	ctx := context.Background()
	r := a.review(t, reportOnly)
	journey.SettleOperation(t, a.f.S, a.repository, a.stream, a.op, a.a)
	moved := journey.MoveFeature(t, a.f, a.stream, map[string]string{"internal/trace/late.go": "package trace\n"})
	if _, reason, err := a.a.FinalGate(ctx, a.stream); err != nil || reason != fmt.Sprintf("final review 1 is stale: it read feature branch commit %s, and %s is current; a new final review is required", r.tip, moved) {
		t.Fatalf("a moved branch: %q %v", reason, err)
	}
	fixturekit.Must(t, a.a.Pass(ctx))
	ops := journey.FinalOperations(t, a.repository, a.stream)
	if len(ops) != 2 {
		t.Fatalf("final review operations after the branch moved %+v", ops)
	}
	if in, err := delivery.DecodeFinalReview(ops[1].Operation); err != nil || in != (delivery.FinalReviewInput{Review: 2, Commit: moved, Seal: 1, Spec: 1, Plan: 1, Charter: a.charter.Revision}) {
		t.Fatalf("the second review %+v: %v", in, err)
	}
	fixturekit.DemoGit(t, filepath.Dir(a.f.Clone), "-C", a.worktree(), "reset", "--hard", "--quiet", r.tip)
	if _, reason, err := a.a.FinalGate(ctx, a.stream); err != nil || reason != "" {
		t.Fatalf("the branch back at the reviewed commit: %q %v", reason, err)
	}
}

func TestFinalReviewIsStaleWhenTheCharterChanges(t *testing.T) {
	t.Parallel()
	a := assembleFinal(t, "final")
	a.review(t, reportOnly)
	fixturekit.RecordCharter(t, a.repository, intakefixture.ShedCharter+"3. Document every change.\n")
	if _, reason, err := a.a.FinalGate(context.Background(), a.stream); err != nil || reason != fmt.Sprintf("final review 1 is stale: it read charter revision %d, and %d is current; a new final review is required", a.charter.Revision, a.charter.Revision+1) {
		t.Fatalf("a changed charter: %q %v", reason, err)
	}
}

func TestFinalReviewIsStaleWhenTheSpecChanges(t *testing.T) {
	t.Parallel()
	a := assembleFinal(t, "final")
	ctx := context.Background()
	a.review(t, reportOnly)
	specs := intakefixture.StreamDocuments(t, a.repository, a.stream, plan.SpecDocument)
	amended := specs[len(specs)-1]
	amended.Revision++
	amended.At = time.Now().UTC()
	amended.Content = strings.Replace(fixturekit.ValidSpec, "never sent again", "never sent twice", 1)
	fixturekit.Must(t, a.repository.RecordDocuments(ctx, []trace.Document{amended}))
	if _, reason, err := a.a.FinalGate(ctx, a.stream); err != nil || reason != "final review 1 is stale: it read spec revision 1, and 2 is current; a new final review is required" {
		t.Fatalf("a changed spec: %q %v", reason, err)
	}
}

func TestBlockingFinalFindingsWaitForTriageOrOwnerAcceptance(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := journey.NewFinalFixture(t, "blocking")
	ctx := context.Background()
	_, op := journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	f.FinalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Dedupe is not wired in.", "findings": []any{
			map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "The upload path never calls Dedupe.", "action": "Call Dedupe before sending a chunk."}}})
		return err
	})
	if result := journey.SettleOperation(t, f.S, repository, stream, op, a); result.Outcome != "succeeded" {
		t.Fatalf("the first review %+v", result)
	}
	blocking := []string{"spec#2: The upload path never calls Dedupe. Action: Call Dedupe before sending a chunk."}
	if added, err := followup.Read(repository, stream); err != nil || len(added) != 0 {
		t.Fatalf("a blocking finding built follow-up units %+v: %v", added, err)
	}
	if plans := intakefixture.StreamDocuments(t, repository, stream, plan.PlanDocument); len(plans) != 1 || plans[0].Content != fixturekit.ValidPlan {
		t.Fatalf("a blocking finding changed the ratified plan: %+v", plans)
	}
	if feature, err := repository.Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.AssembledState {
		t.Fatalf("feature after a blocking finding %+v: %v", feature, err)
	}
	if body := intakefixture.NoticeOf(t, repository, stream, "final-review-1-reviewed"); !strings.Contains(body, "1 blocking, 0 advisory.\nBlocking: "+blocking[0]) || !strings.Contains(body, "through route_amendment") {
		t.Fatalf("the notice %q", body)
	}
	fixturekit.Must(t, a.Pass(ctx))
	if ops := journey.FinalOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("the review was asked for again with nothing changed: %+v", ops)
	}

	journey.MoveFeature(t, f, stream, map[string]string{"internal/trace/upload.go": "package trace\n"})
	fixturekit.Must(t, a.Pass(ctx))
	ops := journey.FinalOperations(t, repository, stream)
	if len(ops) != 2 {
		t.Fatalf("no review of the changed branch: %+v", ops)
	}
	var problems []error
	f.FinalTurn(2, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		if since, err := fixturekit.CallTool(ctx, tools, reviewtools.DiffToolName, map[string]any{"change": "since", "files_only": true}); err != nil || !strings.Contains(since, "internal/trace/upload.go") || strings.Contains(since, "dedupe.go") {
			problems = append(problems, fmt.Errorf("the change since review 1 %q: %v", since, err))
		}
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Dedupe is still not wired in.", "findings": []any{
			map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "The upload path never calls Dedupe.", "action": "Call Dedupe before sending a chunk."}}})
		return err
	})
	result := journey.SettleOperation(t, f.S, repository, stream, ops[1].Operation, a)
	if err := errors.Join(problems...); err != nil {
		t.Fatal(err)
	}
	reader, err := repository.Thread(stream, intake.CommitteeAgent(1))
	fixturekit.Must(t, err)
	if prompt := reader.Turns[len(reader.Turns)-1].Request.Prompt; !strings.Contains(prompt, "Final review 1 read ") || !strings.Contains(prompt, "- blocking: "+blocking[0]) || !strings.Contains(prompt, "change since for what changed after final review 1") {
		t.Fatalf("the second reader's prompt lacks the first review:\n%s", prompt)
	}
	if report, reason, err := a.FinalGate(ctx, stream); err != nil || reason != "" || result.Outcome != "succeeded" || report.Review != 2 || !slices.Equal(delivery.ReportBlocking(report), blocking) {
		t.Fatalf("the second review %+v, reason %q: %v", report, reason, err)
	}

	f.S.SetSole(&journey.ActiveProject{Repository: repository})
	p, apiErr := f.S.DeliveryPresentation(ctx, string(stream))
	if apiErr != nil || !slices.Equal(delivery.ReportBlocking(p.Report), blocking) {
		t.Fatalf("presentation %+v: %v", p.Report, apiErr)
	}
	decision := api.DeliveryDecision{Review: p.Report.Review, ReviewRevision: p.ReviewRevision, Commit: p.Report.Commit, DraftHash: p.DraftHash}
	if _, apiErr := f.S.ApproveDelivery(ctx, string(stream), decision); apiErr == nil || apiErr.Code != api.Conflict || !strings.Contains(apiErr.Message, "approve with accept_blocking") {
		t.Fatalf("approved without accepting the blocking finding: %v", apiErr)
	}
	decision.AcceptBlocking = true
	approval, apiErr := f.S.ApproveDelivery(ctx, string(stream), decision)
	if apiErr != nil || !slices.Equal(approval.AcceptedBlocking, blocking) {
		t.Fatalf("the accepting approval %+v: %v", approval, apiErr)
	}
	if _, reason, err := f.S.DeliveryGate(ctx, repository, stream, approval.Description); err != nil || reason != "" {
		t.Fatalf("the accepting approval does not authorise delivery: %q %v", reason, err)
	}
}

func TestRepeatedBlockingFinalReviewsLeaveRevisionsToTheOwner(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := journey.NewFinalFixture(t, "rounds")
	ctx := context.Background()
	journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	record := func(review int, severity string) {
		content, err := json.Marshal(api.FinalReport{Review: review, Outcome: delivery.FinalReviewed, Findings: []api.FinalFinding{{Severity: severity, Finding: "The upload path never calls Dedupe."}}})
		fixturekit.Must(t, err)
		fixturekit.Must(t, repository.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalReportDocument, Revision: review, Project: repository.Project(), Workstream: stream, At: time.Now().UTC(), Actor: delivery.FinalReviewActor, Cause: "final-review"}, Path: delivery.FinalReportPath, Content: string(content)}}))
	}
	chief := func() *api.APIError {
		return f.S.Intake().EngineeringRevision(repository, stream, intake.AmendmentCase{}, seal.Seal{}, nil, "Groups the findings into one unit.")
	}
	const owners = "final reviews have found blocking problems 3 times; further revisions of the assembled plan require an owner decision"
	record(1, delivery.FindingBlocking)
	record(2, delivery.FindingAdvisory)
	record(3, delivery.FindingBlocking)
	if apiErr := chief(); apiErr != nil && apiErr.Message == owners {
		t.Fatalf("two blocking reviews left the revision to the owner: %v", apiErr)
	}
	record(4, delivery.FindingBlocking)
	if apiErr := chief(); apiErr == nil || apiErr.Message != owners {
		t.Fatalf("three blocking reviews left the revision to the chief of staff: %v", apiErr)
	}
	if body := delivery.FinalReviewNotice(api.FinalReport{}, []string{"x"}, intake.MaxFinalRounds, false); !strings.Contains(body, "Further revisions of the assembled plan are the owner's decision") {
		t.Fatalf("the notice %q", body)
	}
}

func TestFinalReviewThatCannotRebaseFailsAndAuthorisesNothing(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := journey.NewFinalFixture(t, "conflicted")
	ctx := context.Background()
	head, op := journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"CODEOWNERS": "/internal/ @feature\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	upstream := journey.AdvanceUpstream(t, f, map[string]string{"CODEOWNERS": "/internal/ @upstream\n"})
	runs := len(f.Runs())
	result, err := a.Apply(ctx, op)
	fixturekit.Must(t, err)
	if result.Outcome != "failed" || !strings.Contains(result.Evidence, "CODEOWNERS conflicted") {
		t.Fatalf("final review result %+v", result)
	}
	if tip, _, err := intake.Workspaces(f.S.Cfg, intake.BranchesDirectory, config.WorkspacesGit).Branch(ctx, intake.FeatureBranch(stream)); err != nil || tip != head {
		t.Fatalf("a conflicted rebase moved the feature branch to %s: %v", tip, err)
	}
	if ran := f.Runs()[runs:]; len(ran) != 0 {
		t.Fatalf("a committee member ran %v on a branch that did not rebase", ran)
	}
	report, found, err := delivery.LatestFinalReport(repository, stream)
	if err != nil || !found || report.Outcome != delivery.FinalFailed || report.Commit != "" || report.Before != head || !slices.Equal(report.Conflicts, []string{"CODEOWNERS"}) ||
		report.Upstream == nil || report.Upstream.Commit != upstream || len(report.Criteria) != 0 ||
		report.Failure != fmt.Sprintf("feature branch %s does not rebase cleanly onto upstream/main at %s: CODEOWNERS conflicted; the branch is left at %s", intake.FeatureBranch(stream), upstream, head) {
		t.Fatalf("the failed report %+v: %v", report, err)
	}
	if len(intakefixture.StreamDocuments(t, repository, stream, delivery.FinalRebaseDocument)) != 0 {
		t.Fatal("a conflicted rebase recorded a rebase")
	}
	failed := intakefixture.TransitionByID(t, repository, stream, "final-review-1-failed")
	if failed.To != "failed-1" || failed.Reason != result.Evidence {
		t.Fatalf("the failed transition %+v", failed)
	}
	if body := intakefixture.NoticeOf(t, repository, stream, failed.ID); !strings.HasPrefix(body, "The final review of the assembled branch failed: feature branch ") || !strings.HasSuffix(body, "The failed review authorises nothing; a new review runs once the branch or its governing documents change, or once the workstream is moved to assembled with move_workstream.") {
		t.Fatalf("the notice %q", body)
	}
	if _, reason, err := a.FinalGate(ctx, stream); err != nil || reason != "final review 1 failed: "+report.Failure {
		t.Fatalf("a failed review authorises: %q %v", reason, err)
	}

	repository.Close()
	f.Start(t)
	if ops := fixturekit.AwaitAcknowledged(t, func(t *testing.T) []trace.OperationRecord { return journey.FinalOperations(t, f.Repository(), stream) }); len(ops) != 1 || !reflect.DeepEqual(ops[0].Result, &result) {
		t.Fatalf("final review operations after a restart %+v", ops)
	}
	f.Stop(t)
	repository, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
	fixturekit.Must(t, err)
	defer repository.Close()
	a = &journey.FinalReviewer{S: f.S, Repository: repository}
	fixturekit.Must(t, a.Pass(ctx))
	if ops := journey.FinalOperations(t, repository, stream); len(ops) != 1 {
		t.Fatalf("the failed review was asked for again with nothing changed: %+v", ops)
	}
	if after, _, err := delivery.LatestFinalReport(repository, stream); err != nil || !reflect.DeepEqual(after, report) {
		t.Fatalf("the failed report after a restart %+v: %v", after, err)
	}
}

func TestInterruptedFinalReviewKeepsItsRecordedRebase(t *testing.T) {
	t.Parallel()
	f, stream, repository, a := journey.NewFinalFixture(t, "interrupted")
	ctx := context.Background()
	head, op := journey.AssembleBoth(t, f, repository, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	first := journey.AdvanceUpstream(t, f, map[string]string{"FIRST.md": "first\n"})
	f.S.Boundary = func(name string) error {
		if name == "final-rebase-recorded" {
			return errors.New("the service stopped")
		}
		return nil
	}
	if _, err := a.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "the service stopped") {
		t.Fatalf("the interrupted review: %v", err)
	}
	f.S.Boundary = nil
	g := intake.Workspaces(f.S.Cfg, intake.BranchesDirectory, config.WorkspacesGit)
	if tip, _, err := g.Branch(ctx, intake.FeatureBranch(stream)); err != nil || tip != head {
		t.Fatalf("the branch moved to %s before the review resumed: %v", tip, err)
	}
	var rebase delivery.FinalRebase
	rebases := intakefixture.StreamDocuments(t, repository, stream, delivery.FinalRebaseDocument)
	if len(rebases) != 1 || json.Unmarshal([]byte(rebases[0].Content), &rebase) != nil || rebase.Before != head || rebase.Upstream.Commit != first {
		t.Fatalf("the recorded rebase %+v", rebases)
	}
	journey.AdvanceUpstream(t, f, map[string]string{"SECOND.md": "second\n"})
	var problems []error
	f.FinalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		if got, err := intakefixture.ReadTool(ctx, tools, "branch/FIRST.md"); err != nil || got != "first\n" {
			problems = append(problems, fmt.Errorf("FIRST.md holds %q: %v", got, err))
		}
		if got, err := intakefixture.ReadTool(ctx, tools, "branch/SECOND.md"); err == nil {
			problems = append(problems, fmt.Errorf("the reader sees upstream's later commit: %q", got))
		}
		return nil
	})
	result, err := a.Apply(ctx, op)
	fixturekit.Must(t, err)
	if err := errors.Join(problems...); err != nil {
		t.Fatal(err)
	}
	if tip, _, err := g.Branch(ctx, intake.FeatureBranch(stream)); err != nil || tip != rebase.Commit {
		t.Fatalf("the branch is at %s, not the recorded rebase %s: %v", tip, rebase.Commit, err)
	}
	report, _, err := delivery.LatestFinalReport(repository, stream)
	fixturekit.Must(t, err)
	if result.Outcome != "failed" || report.Outcome != delivery.FinalFailed || report.Commit != rebase.Commit || report.Failure != "the committee member ended its turn without recording a report" {
		t.Fatalf("result %+v, report %+v", result, report)
	}
	if _, reason, err := a.FinalGate(ctx, stream); err != nil || reason != "final review 1 failed: the committee member ended its turn without recording a report" {
		t.Fatalf("a failed review authorises: %q %v", reason, err)
	}
}

func TestInterruptedFinalRebaseIsReconciled(t *testing.T) {
	t.Parallel()
	evidence := map[string]string{
		"final-rebase-replaying": "has recorded no rebase",
		"final-rebase-recorded":  "the branch moves to it without replaying again",
		"final-rebase-moved":     "the review reads it without replaying again",
	}
	for _, point := range []string{"final-rebase-replaying", "final-rebase-recorded", "final-rebase-moved"} {
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			f, stream, repository, a := journey.NewFinalFixture(t, point)
			ctx := context.Background()
			head, op := journey.AssembleBoth(t, f, repository, a, stream,
				map[string]string{"internal/trace/resume.go": "package trace\n"},
				map[string]string{"internal/trace/dedupe.go": "package trace\n"})
			upstream := journey.AdvanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
			replays, stop := 0, true
			f.S.Boundary = func(name string) error {
				if name == point && stop {
					stop = false
					return errors.New("the service stopped")
				}
				if name == "final-rebase-replaying" {
					replays++
				}
				return nil
			}
			if _, err := a.Apply(ctx, op); err == nil || !strings.Contains(err.Error(), "the service stopped") {
				t.Fatalf("the review never reached %s: %v", point, err)
			}
			if observed, err := a.Inspect(ctx, op); err != nil || observed.State != coreadapter.EffectAbsent || !strings.Contains(observed.Evidence, evidence[point]) {
				t.Fatalf("inspection after an interruption at %s: %+v %v", point, observed, err)
			}
			var problems []error
			f.FinalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
				got, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Uploads resume and skip acknowledged chunks.", "findings": []any{}})
				if err != nil || !strings.Contains(got, `"recorded":true`) {
					problems = append(problems, fmt.Errorf("the report was not recorded: %s %v", got, err))
				}
				return nil
			})
			result := journey.SettleOperation(t, f.S, repository, stream, op, a)
			if err := errors.Join(problems...); err != nil {
				t.Fatal(err)
			}
			tip, _, err := intake.Workspaces(f.S.Cfg, intake.BranchesDirectory, config.WorkspacesGit).Branch(ctx, intake.FeatureBranch(stream))
			fixturekit.Must(t, err)
			var rebase delivery.FinalRebase
			rebases := intakefixture.StreamDocuments(t, repository, stream, delivery.FinalRebaseDocument)
			if replays != 1 || len(rebases) != 1 || json.Unmarshal([]byte(rebases[0].Content), &rebase) != nil || rebase.Before != head || rebase.Upstream.Commit != upstream || rebase.Commit != tip {
				t.Fatalf("%d replays; the recorded rebases %+v; the branch is at %s", replays, rebases, tip)
			}
			home := filepath.Dir(f.Clone)
			landed := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", f.Clone, "rev-list", "--count", strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", f.Clone, "merge-base", head, upstream))+".."+head))
			if count := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", f.Clone, "rev-list", "--count", upstream+".."+tip)); count != landed {
				t.Fatalf("the rebased branch holds %s commits on upstream, %s landed", count, landed)
			}
			report, _, err := delivery.LatestFinalReport(repository, stream)
			fixturekit.Must(t, err)
			if result.Outcome != "succeeded" || report.Outcome != delivery.FinalReviewed || report.Commit != tip {
				t.Fatalf("result %+v, report %+v", result, report)
			}
			if ops := journey.FinalOperations(t, repository, stream); len(ops) != 1 || ops[0].Result == nil || ops[0].Result.Outcome != "succeeded" {
				t.Fatalf("final review operations %+v", ops)
			}
		})
	}
}

func TestIncompleteFinalChecksCountTowardTheRetryDelay(t *testing.T) {
	t.Parallel()
	f, stream, repo, a := journey.NewFinalFixture(t, "final-checks-retry-delay")
	at := f.Clock.Now()

	for revision := 1; revision <= 2; revision++ {
		content := fmt.Sprintf(`{"review":1,"operation":"review","commit":"candidate","status":"incomplete","exit_code":-1,"error":"offline","finished_at":%q}`, at.Format(time.RFC3339Nano))
		fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalChecksDocument(1), Revision: revision, Project: repo.Project(), Workstream: stream, At: at, Actor: execution.ChecksActor, Cause: "checks"}, Path: delivery.FinalChecksPath(1), Content: content}}))
	}
	n, latest, err := a.Driver().IncompleteChecks(stream, 1, "candidate")
	fixturekit.Must(t, err)
	if n != 2 || delivery.FinalCheckDelay(n) != 2*execution.CheckRetryDelay {
		t.Fatalf("incomplete count %d: %+v", n, latest)
	}

}
