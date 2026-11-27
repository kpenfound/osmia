package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verification"
	"github.com/kpenfound/osmia/internal/verifytool"
	"github.com/kpenfound/osmia/internal/workspace"
)

const checkoutMap = `---
feature: Trace
paths:
  - internal/trace/**
flows: [checkout, coupon]
---

# Trace
`

const mixedReport = `{"version":1,"claims":[
 {"name":"cart total","status":"pass","detail":"shows 3","evidence":["shots/cart.png","logs/console.txt"],"flow":"checkout"},
 {"name":"coupon field","status":"fail","detail":"no coupon field","evidence":["logs/console.txt"],"flow":"checkout"},
 {"name":"banner","status":"inconclusive","detail":"slow page","evidence":[]}]}`

const untaggedReport = `{"version":1,"claims":[{"name":"page loads","status":"pass","detail":"ok","evidence":["logs/console.txt"]}]}`

const inconclusiveReport = `{"version":1,"claims":[{"name":"coupon field","status":"inconclusive","detail":"the page never settled","evidence":["logs/console.txt"],"flow":"coupon"}]}`

var evidencePNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)

type leverFake struct{ log string }

func installLever(t *testing.T, report, list string) leverFake {
	t.Helper()
	bin, work := t.TempDir(), t.TempDir()
	out := filepath.Join(work, "out")
	files := map[string][]byte{"report.json": []byte(report), "shots/cart.png": evidencePNG, "logs/console.txt": []byte("console: ready\n")}
	for name, data := range files {
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(out, name)), 0o700))
		must(t, os.WriteFile(filepath.Join(out, name), data, 0o600))
	}
	f := leverFake{log: filepath.Join(work, "log")}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$*" in
  *" list") printf '%%s' %q; exit 0;;
esac
for last; do :; done
cp -R %q/. "$last"
`, f.log, list, out)
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

func (f leverFake) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.log)
	if os.IsNotExist(err) {
		return nil
	}
	must(t, err)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func (f leverFake) runs(t *testing.T) []string {
	t.Helper()
	return slices.DeleteFunc(f.calls(t), func(c string) bool { return strings.HasSuffix(c, " list") })
}

func verifyingFixture(t *testing.T, key string, declared [][]string, withMap bool) (*buildFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	f := newBuildFixture(t, key)
	f.cfg.Project.Verification = verification.Setting{Call: []string{"lever"}, MapDir: verification.DefaultMapDir, LeverPaths: []string{"lever/**"}}
	m := f.controller()
	turns := intakefixture.TurnScenario{Root: func() config.Root { return f.cfg.Root }, Project: f.repository.Project(), Now: f.clock.Now, ForemanActor: foremanActor}
	intakefixture.SeedChecking(t, intakefixture.UnitSeedDriver[building]{Read: m.read, Start: m.start, Finish: m.finish,
		Open: func(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, error) {
			w, base, err := f.units().open(ctx, stream, unit)
			if err == nil && withMap {
				must(t, os.MkdirAll(filepath.Join(w.Path, "docs", "verification"), 0o755))
				must(t, os.WriteFile(filepath.Join(w.Path, "docs", "verification", "trace.md"), []byte(checkoutMap), 0o644))
			}
			return w, base, err
		},
		Complete: func(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, unit, outcome string) trace.QueuedTurn {
			content, err := json.Marshal(doneReport{MasonReport: MasonReport{Outcome: outcome, Invocations: declared}})
			must(t, err)
			return turns.CaptureTurn(t, repo, stream, masonAgent(unit), &coreadapter.Outcome{Status: "done", Report: string(content), Card: &intakefixture.ExampleCard})
		}}, f.repository, stream, "resume", resumeReport)
	return f, stream, f.repository
}

func flowCall(name string) []string { return []string{"flow", "--name=" + name} }

func sevenInvocations() [][]string {
	var declared [][]string
	for i := 1; i <= 7; i++ {
		declared = append(declared, flowCall(fmt.Sprintf("a%d", i)))
	}
	return declared
}

func TestReportedInvocationsAreRerunUpToFiveInOrderOnTheCandidateAndTheRestAreRecordedAsSkipped(t *testing.T) {
	lever := installLever(t, mixedReport, `["checkout","coupon"]`)
	f, stream, repo := verifyingFixture(t, "rerun-seven", sevenInvocations(), true)
	runChecks(t, f.s, repo, stream)

	report := currentReport(t, repo, stream, "resume")
	if len(report.Invocations) != 7 {
		t.Fatalf("the report holds %d invocations", len(report.Invocations))
	}
	runs := lever.runs(t)
	if len(runs) != 5 {
		t.Fatalf("%d lever runs: %q", len(runs), runs)
	}
	reruns, err := verifytool.Reruns(repo, stream, "resume", report.Candidate)
	must(t, err)
	if len(reruns) != 7 {
		t.Fatalf("%d rerun records", len(reruns))
	}
	seen := map[string]bool{}
	for i, op := range reruns {
		if op.Rerun.Index != i+1 || !slices.Equal(op.Args, report.Invocations[i]) || op.Commit != report.Candidate {
			t.Fatalf("rerun %d: %+v", i+1, op)
		}
		if i >= 5 {
			if op.Rerun.Disposition != verifytool.RerunSkipped || op.Rerun.Reason == "" || op.RunID != "" || op.Outcome != nil {
				t.Fatalf("rerun %d was not recorded as skipped with a reason: %+v", i+1, op)
			}
			continue
		}
		if op.Rerun.Disposition != verifytool.RerunRan || op.RunID == "" || seen[op.RunID] || op.Outcome == nil || op.Outcome.Limitation != "" {
			t.Fatalf("rerun %d: %+v", i+1, op)
		}
		seen[op.RunID] = true
		if !strings.Contains(runs[i], "--run-id "+op.RunID) || !strings.Contains(runs[i], fmt.Sprintf("--name=a%d", i+1)) {
			t.Fatalf("lever run %d is %q for %+v", i+1, runs[i], op)
		}
		kept, ok, err := verifytool.Retained(repo, stream, op.RunID)
		must(t, err)
		if !ok || !slices.Contains(kept.Files, "report.json") || !slices.Contains(kept.Files, "shots/cart.png") || !slices.Contains(kept.Images, "shots/cart.png") || !slices.Contains(kept.Files, "logs/console.txt") {
			t.Fatalf("rerun %d kept %+v", i+1, kept)
		}
	}
}

func TestAnInvocationTheArgumentRulesRefuseIsRecordedAsRefusedAndStartsNoRun(t *testing.T) {
	lever := installLever(t, mixedReport, `["checkout"]`)
	declared := [][]string{flowCall("a1"), {"flow", "--run-id", "mine"}, flowCall("a3"), {"--progress=plain", "flow"}}
	f, stream, repo := verifyingFixture(t, "rerun-refused", declared, true)
	runChecks(t, f.s, repo, stream)

	report := currentReport(t, repo, stream, "resume")
	reruns, err := verifytool.Reruns(repo, stream, "resume", report.Candidate)
	must(t, err)
	var dispositions []string
	for _, op := range reruns {
		dispositions = append(dispositions, op.Rerun.Disposition)
		if op.Rerun.Disposition == verifytool.RerunRefused && (op.Rerun.Reason == "" || op.RunID != "" || op.Outcome != nil) {
			t.Fatalf("a refused invocation carries a run: %+v", op)
		}
	}
	if !slices.Equal(dispositions, []string{"ran", "refused", "ran", "refused"}) {
		t.Fatalf("%v", dispositions)
	}
	if runs := lever.runs(t); len(runs) != 2 {
		t.Fatalf("%d lever runs: %q", len(runs), runs)
	}
}

func TestRerunningTheChecksAgainRepeatsNoRerunThatFinished(t *testing.T) {
	lever := installLever(t, mixedReport, `["checkout"]`)
	f, stream, repo := verifyingFixture(t, "rerun-idempotent", [][]string{flowCall("a1"), flowCall("a2")}, true)
	runChecks(t, f.s, repo, stream)
	report := currentReport(t, repo, stream, "resume")
	g, err := f.units().Of(stream)
	must(t, err)
	c := &checkers{masons: f.controller()}
	in := checkInput{Workstream: stream, Unit: "resume", Run: 9, Candidate: report.Candidate, Base: report.Base, Report: 1}
	must(t, c.rerunDeclared(context.Background(), in, g))
	if runs := lever.runs(t); len(runs) != 2 {
		t.Fatalf("a repeat started more runs: %q", runs)
	}
}

func reviewItem(t *testing.T, f *buildFixture, stream config.WorkstreamID, source string) string {
	t.Helper()
	req, _, err := (&reviewers{masons: f.controller()}).prepareUnitReview(context.Background(), stream, "resume")
	must(t, err)
	for _, item := range req.Context {
		if item.Source == source {
			return item.Content
		}
	}
	return ""
}

const verificationSource = "verification (service-recorded)"

func TestTheReviewBundleShowsServiceRerunsApartFromTheMasonAndAFlowWithAPassAndAFailAsFail(t *testing.T) {
	installLever(t, mixedReport, `["checkout","coupon"]`)
	f, stream, repo := verifyingFixture(t, "bundle-mixed", [][]string{flowCall("checkout")}, true)
	runChecks(t, f.s, repo, stream)
	section := reviewItem(t, f, stream, verificationSource)
	runID := mustRerun(t, repo, stream, 1).RunID
	for _, want := range []string{"service-recorded", "not evidence", runID, "status: fail", "- Trace (docs/verification/trace.md)", "flow checkout: fail", "flow coupon: no service-recorded run covered it", ".osmia-verify/" + runID + "/shots/cart.png", "Untagged claims", "banner"} {
		if !strings.Contains(section, want) {
			t.Fatalf("the verification section lacks %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "flow checkout: pass") {
		t.Fatalf("a flow with a failed claim is shown as passing:\n%s", section)
	}
}

func TestTheRerunsRetainedEvidenceIsPlacedInTheReviewersViewWithoutRunningTheLeverAgain(t *testing.T) {
	lever := installLever(t, mixedReport, `["checkout","coupon"]`)
	f, stream, repo := verifyingFixture(t, "bundle-view", [][]string{flowCall("checkout")}, true)
	runChecks(t, f.s, repo, stream)
	before := len(lever.calls(t))
	report := currentReport(t, repo, stream, "resume")
	view, err := (isolation.Views{Directory: t.TempDir()}).Create(context.Background(), coreadapter.Workspace{Directory: t.TempDir(), Access: coreadapter.ReadWrite}, nil)
	must(t, err)
	t.Cleanup(func() { view.Release(context.Background()) })
	must(t, verifytool.PlaceReruns(repo, stream, "resume", report.Candidate, view))
	runID := mustRerun(t, repo, stream, 1).RunID
	for name, want := range map[string][]byte{verifytool.Ref(runID, "shots/cart.png"): evidencePNG, verifytool.Ref(runID, "logs/console.txt"): []byte("console: ready\n"), verifytool.ImagePath(runID, "shots/cart.png"): evidencePNG} {
		got, err := view.Read(name)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s in the reviewer's view: %q %v", name, got, err)
		}
	}
	if got := len(lever.calls(t)); got != before {
		t.Fatalf("placing the evidence ran the lever %d more times", got-before)
	}
}

func mustRerun(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, index int) verifytool.Operation {
	t.Helper()
	ops, _, err := verifytool.Operations(repo, stream)
	must(t, err)
	for _, op := range ops {
		if op.Rerun != nil && op.Rerun.Index == index {
			return op
		}
	}
	t.Fatalf("no rerun %d", index)
	return verifytool.Operation{}
}

func TestAnInconclusiveRerunIsShownAsInconclusiveAndNeverAsPassing(t *testing.T) {
	installLever(t, inconclusiveReport, `["checkout","coupon"]`)
	f, stream, repo := verifyingFixture(t, "bundle-inconclusive", [][]string{flowCall("coupon")}, true)
	runChecks(t, f.s, repo, stream)
	section := reviewItem(t, f, stream, verificationSource)
	for _, want := range []string{"status: inconclusive", "[inconclusive] coupon field", "flow coupon: inconclusive"} {
		if !strings.Contains(section, want) {
			t.Fatalf("the verification section lacks %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "status: pass") || strings.Contains(section, "[pass]") || strings.Contains(section, ": pass") {
		t.Fatalf("an inconclusive rerun reads as a pass:\n%s", section)
	}
}

func TestARerunWithOnlyUntaggedPassingClaimsLeavesTheFlowsUncovered(t *testing.T) {
	installLever(t, untaggedReport, `["checkout","coupon"]`)
	f, stream, repo := verifyingFixture(t, "bundle-untagged", [][]string{flowCall("checkout")}, true)
	runChecks(t, f.s, repo, stream)
	section := reviewItem(t, f, stream, verificationSource)
	for _, want := range []string{"flow checkout: no service-recorded run covered it", "flow coupon: no service-recorded run covered it", "Untagged claims", "page loads"} {
		if !strings.Contains(section, want) {
			t.Fatalf("the verification section lacks %q:\n%s", want, section)
		}
	}
}

func TestAMapHeaderThatDoesNotParseAndAFlowTheListLacksAreNotesAndTheReviewProceeds(t *testing.T) {
	installLever(t, mixedReport, `["checkout"]`)
	f, stream, repo := verifyingFixture(t, "bundle-notes", [][]string{flowCall("checkout")}, true)
	runChecks(t, f.s, repo, stream)
	section := reviewItem(t, f, stream, verificationSource)
	for _, want := range []string{`flow "coupon" is not in the lever's list`, "Structure check", "block nothing"} {
		if !strings.Contains(section, want) {
			t.Fatalf("the verification section lacks %q:\n%s", want, section)
		}
	}
	if state, err := repo.Workflow(stream, trace.UnitSubject("resume")); err != nil || state.Value != UnitReviewing {
		t.Fatalf("the unit is %s: %v", state.Value, err)
	}
}

func TestADiffThatTouchesTheMapDirectoryIsFlaggedAndNotHeld(t *testing.T) {
	installLever(t, mixedReport, `["checkout","coupon"]`)
	f, stream, repo := verifyingFixture(t, "bundle-flag", nil, true)
	runChecks(t, f.s, repo, stream)
	section := reviewItem(t, f, stream, verificationSource)
	if !strings.Contains(section, "Flag: the diff touches the feature map directory docs/verification") {
		t.Fatalf("no flag:\n%s", section)
	}
	if state, err := repo.Workflow(stream, trace.UnitSubject("resume")); err != nil || state.Value != UnitReviewing {
		t.Fatalf("the unit is %s: %v", state.Value, err)
	}
}

func TestAProjectWithoutTheSettingHasNoVerificationInTheReviewBundleOrTheDoneReport(t *testing.T) {
	f, stream, repo := newReviewFixture(t, "bundle-plain")
	req, _, err := (&reviewers{masons: f.controller()}).prepareUnitReview(context.Background(), stream, "resume")
	must(t, err)
	for _, item := range req.Context {
		if strings.Contains(item.Source, "verification") || strings.Contains(item.Content, "service-recorded") || strings.Contains(item.Content, "invocations") {
			t.Fatalf("verification content in %s", item.Source)
		}
	}
	mason, err := f.controller().Bundle(context.Background(), stream, "resume")
	must(t, err)
	if mason.Verification != nil || strings.Contains(mason.Render(), "Verification") {
		t.Fatal("the mason bundle carries verification content")
	}
	if report := currentReport(t, repo, stream, "resume"); report.Invocations != nil {
		t.Fatalf("%v", report.Invocations)
	}
	tool := (&masonReports{}).tool(repo, coreadapter.Scope{})
	if strings.Contains(string(tool.InputSchema), "invocations") || strings.Contains(tool.Description, "invocations") {
		t.Fatal("the done tool offers invocations")
	}
	enabled := (&masonReports{Verification: func() bool { return true }}).tool(repo, coreadapter.Scope{})
	if !strings.Contains(string(enabled.InputSchema), "invocations") {
		t.Fatal("the done tool of a verifying project does not offer invocations")
	}
}

func TestTheMasonBundleNamesTheFeatureFilesOfItsFootprintBeforeAnyCandidateExists(t *testing.T) {
	f := newBuildFixture(t, "pointers")
	f.cfg.Project.Verification = verification.Setting{Call: []string{"lever"}, MapDir: verification.DefaultMapDir}
	clone := f.cfg.Project.Clone
	must(t, os.MkdirAll(filepath.Join(clone, "docs", "verification"), 0o755))
	must(t, os.WriteFile(filepath.Join(clone, "docs", "verification", "trace.md"), []byte(checkoutMap), 0o644))
	demoGit(t, f.home, "-C", clone, "add", "docs")
	demoGit(t, f.home, "-C", clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", "map")
	tip := strings.TrimSpace(demoGit(t, f.home, "-C", clone, "rev-parse", "HEAD"))
	demoGit(t, f.home, "-C", clone, "update-ref", "refs/heads/"+featureBranch(stream), tip)

	m := f.controller()
	mason, err := m.Bundle(context.Background(), stream, "resume")
	must(t, err)
	if mason.Verification == nil || len(mason.Verification.Features) != 1 || mason.Verification.Features[0].File != "docs/verification/trace.md" {
		t.Fatalf("%+v", mason.Verification)
	}
	for _, want := range []string{"docs/verification/trace.md", "same change", "invocations"} {
		if !strings.Contains(mason.Render(), want) {
			t.Fatalf("the mason bundle lacks %q:\n%s", want, mason.Render())
		}
	}
	for name, test := range map[string]struct {
		footprint []string
		want      int
	}{
		"a glob for a directory that does not exist yet": {[]string{"internal/trace/newpkg/**"}, 1},
		"a footprint with no overlap":                    {[]string{"cmd/other/**"}, 0},
		"the whole repository":                           {[]string{"repo"}, 1},
		"an entity ID":                                   {[]string{"internal.trace"}, 1},
	} {
		brief, err := m.verificationBrief(context.Background(), stream, "resume", test.footprint)
		must(t, err)
		if len(brief.Features) != test.want {
			t.Fatalf("%s names %+v", name, brief.Features)
		}
	}
}

func TestTheMasonBundleOnReworkNamesTheFeatureFileOfTheCandidatesChangedFiles(t *testing.T) {
	installLever(t, mixedReport, `["checkout","coupon"]`)
	f, stream, repo := verifyingFixture(t, "pointers-rework", nil, true)
	runChecks(t, f.s, repo, stream)
	m := f.controller()
	brief, err := m.verificationBrief(context.Background(), stream, "resume", []string{"cmd/other/**"})
	must(t, err)
	if len(brief.Features) != 1 || brief.Features[0].File != "docs/verification/trace.md" {
		t.Fatalf("%+v", brief.Features)
	}
	r := &reviewers{masons: m}
	_, identity, err := r.prepareUnitReview(context.Background(), stream, "resume")
	must(t, err)
	result := UnitReviewResult{Identity: identity, Turn: ReviewTurnID("resume", 1), Verdict: UnitVerdict{Decision: "material_findings", Summary: reviewSummary, Findings: []ReviewFinding{{Severity: "material", Evidence: "e", Action: "a"}}}, Bounces: 1}
	must(t, r.enqueueFindings(context.Background(), stream, "resume", result))
	th, err := repo.Thread(stream, masonAgent("resume"))
	must(t, err)
	if prompt := th.Turns[len(th.Turns)-1].Request.Prompt; !strings.Contains(prompt, "docs/verification/trace.md") {
		t.Fatalf("the rework prompt does not name the feature file:\n%s", prompt)
	}
}

func TestAnApprovedUnitsFeatureAssociationIsRecordedInTheTrace(t *testing.T) {
	installLever(t, mixedReport, `["checkout","coupon"]`)
	f, stream, repo := verifyingFixture(t, "association", nil, true)
	runChecks(t, f.s, repo, stream)
	approveDirectly(t, f.s, repo, stream, "resume", "spec#1")
	association, ok, err := ReadFeatureAssociation(repo, stream, "resume")
	must(t, err)
	report := currentReport(t, repo, stream, "resume")
	if !ok || association.Candidate != report.Candidate || len(association.Features) != 1 || association.Features[0].Feature != "Trace" || !slices.Equal(association.Flows, []string{"checkout", "coupon"}) || !association.Touched.Map || association.Touched.Lever {
		t.Fatalf("%+v %v", association, ok)
	}
	if state, err := repo.Workflow(stream, trace.UnitSubject("resume")); err != nil || state.Value != UnitApproved {
		t.Fatalf("the unit is %s: %v", state.Value, err)
	}
}

func TestAProjectWithoutTheSettingRecordsNoFeatureAssociation(t *testing.T) {
	f, stream, repo := newReviewFixture(t, "association-none")
	approveDirectly(t, f.s, repo, stream, "resume", "spec#1")
	if _, ok, err := ReadFeatureAssociation(repo, stream, "resume"); err != nil || ok {
		t.Fatalf("%v %v", ok, err)
	}
}

