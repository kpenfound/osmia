package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/climb"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	kindreg "github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// climbJourney is a climb fixture whose project delivers through a fake pull
// request host: the final review rebases nothing a second time, publishes the
// draft pull request to a local fork, and the owner's approval marks it ready.
type climbJourney struct {
	*climbFixture
	pulls *fixturekit.FakePulls
	fork  string
	mu    sync.Mutex
	named config.WorkstreamID
	// reports counts the final reviews' report calls.
	reports int
}

func (j *climbJourney) workstream() config.WorkstreamID {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.named
}

func newClimbJourney(t *testing.T, engine *evaluatorEngine, spec string) *climbJourney {
	t.Helper()
	pulls := &fixturekit.FakePulls{SyncHead: true}
	j := &climbJourney{pulls: pulls}
	j.climbFixture = newClimbFixtureHosted(t, engine, false, nil, spec, pulls)
	t.Cleanup(func() { j.stop(t) })
	fork, _ := fixturekit.PublicationFork(t, j.clone, func() string { return "" })
	j.fork = fork
	pulls.Fork = func() string {
		tip, _ := fixturekit.PublicationForkTip(fork, intake.FeatureBranch(j.workstream()))
		return tip
	}
	fixturekit.ConfigureDeliverySigner(t, j.clone)
	intakefixture.FinalTurn(j.shedFixture.engine, delivery.FinalTurnID(1, committeeAgent(1), 1), func(ctx context.Context, tools *mcp.ClientSession) error {
		j.mu.Lock()
		j.reports++
		j.mu.Unlock()
		_, err := fixturekit.CallTool(ctx, tools, FinalReportTool, map[string]any{"summary": "The kept attempts serve the goal and leave the evaluator alone.", "findings": []any{}})
		return err
	})
	return j
}

// calibrated hands in the climb, has its climb spec drafted, debated once and
// ratified, builds and calibrates the evaluator and waits for the owner's
// approval card.
func (j *climbJourney) calibrated(t *testing.T) config.WorkstreamID {
	t.Helper()
	stream := j.ratified(t)
	j.mu.Lock()
	j.named = stream
	j.mu.Unlock()
	j.diagnose(t, stream)
	j.awaitInner(t, stream, climb.InnerAwaitingApproval)
	return stream
}

// approve is the owner's explicit decision on the evaluator approval card.
func (j *climbJourney) approve(t *testing.T, stream config.WorkstreamID) {
	t.Helper()
	card, open := j.card(t)
	if !open {
		t.Fatal("no approval card")
	}
	if _, err := j.c.ApproveClimb(context.Background(), stream, api.ClimbApprovalRequest{Decision: "approve", Commit: card.Answer.Body["commit"].(string)}); err != nil {
		t.Fatal(err)
	}
}

// crashAt makes the service fail at the named step and every step after it,
// as a process killed there would, until it is restarted.
func (j *climbJourney) crashAt(point string) *atomic.Bool {
	var dead atomic.Bool
	j.s.setBoundary(func(name string) error {
		if dead.Load() || name == point && dead.CompareAndSwap(false, true) {
			return errors.New("the service crashed at " + point)
		}
		return nil
	})
	return &dead
}

func (j *climbJourney) feature(t *testing.T, stream config.WorkstreamID) string {
	t.Helper()
	state, err := j.repository().Workflow(stream, trace.FeatureSubject)
	fixturekit.Must(t, err)
	return state.Value
}

func (j *climbJourney) awaitFeature(t *testing.T, stream config.WorkstreamID, want string) {
	t.Helper()
	within(t, fmt.Sprintf("the workstream to be %s (it is %s)", want, j.feature(t, stream)), func() bool { return j.feature(t, stream) == want })
}

func (j *climbJourney) conclusion(t *testing.T, stream config.WorkstreamID) climb.Conclusion {
	t.Helper()
	rec, found, err := climb.ReadConclusion(j.repository(), stream)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("the climb recorded no conclusion")
	}
	return rec
}

func (j *climbJourney) awaitConcluded(t *testing.T, stream config.WorkstreamID) climb.Conclusion {
	t.Helper()
	within(t, "the conclusion to record both finals", func() bool {
		rec, found, err := climb.ReadConclusion(j.repository(), stream)
		fixturekit.Must(t, err)
		return found && rec.Concluded()
	})
	return j.conclusion(t, stream)
}

// presented waits for the delivery presentation of the reviewed branch.
func (j *climbJourney) presented(t *testing.T, stream config.WorkstreamID) api.DeliveryPresentation {
	t.Helper()
	var presentation api.DeliveryPresentation
	within(t, "the delivery presentation", func() bool {
		p, err := j.c.Delivery(context.Background(), stream)
		if err != nil {
			return false
		}
		presentation = p
		return p.Report.Review > 0 && p.Report.Outcome == delivery.FinalReviewed
	})
	return presentation
}

func (j *climbJourney) attemptsEndpoint(t *testing.T, stream config.WorkstreamID) ClimbAttemptsResponse {
	t.Helper()
	var out ClimbAttemptsResponse
	fixturekit.Must(t, j.c.Do(context.Background(), "GET", api.Prefix+"/climb/"+url.PathEscape(string(stream))+"/attempts", nil, &out))
	return out
}

func (j *climbJourney) progressEndpoint(t *testing.T, stream config.WorkstreamID) ClimbProgressResponse {
	t.Helper()
	var out ClimbProgressResponse
	fixturekit.Must(t, j.c.Do(context.Background(), "GET", api.Prefix+"/climb/"+url.PathEscape(string(stream))+"/progress", nil, &out))
	return out
}

func (j *climbJourney) treeOf(t *testing.T, commit string) string {
	t.Helper()
	return j.git(t, "rev-parse", commit+"^{tree}")
}

func (j *climbJourney) isAncestor(t *testing.T, ancestor, of string) bool {
	t.Helper()
	home := filepath.Dir(j.clone)
	cmd := exec.Command("git", "-C", j.clone, "merge-base", "--is-ancestor", ancestor, of)
	cmd.Dir = home
	return cmd.Run() == nil
}

// scriptThreeAttempts scripts a kept attempt, one that touches a protected
// path and one that does not improve.
func (j *climbJourney) scriptThreeAttempts() {
	j.playAttempt(1, map[string]string{"src/parse.go": "package src\n// faster\n"}, "cache the parsed corpus")
	j.playAttempt(2, map[string]string{"src/other.go": "package src\n", "testdata/corpus/a.txt": "doctored\n"}, "shrink the corpus")
	j.playAttempt(3, map[string]string{"src/third.go": "package src\n// third\n"}, "batch the reads")
}

func finalPromptOf(t *testing.T, j *climbJourney, stream config.WorkstreamID) (system, prompt string) {
	t.Helper()
	th, err := j.repository().Thread(stream, committeeAgent(1))
	fixturekit.Must(t, err)
	for _, q := range th.Turns {
		if strings.HasPrefix(q.Request.TurnID, delivery.FinalTurnPrefix(1, committeeAgent(1))) {
			return q.Request.SystemPrompt, q.Request.Prompt
		}
	}
	t.Fatalf("the committee member has no final review turn: %+v", th.Turns)
	return "", ""
}

func TestAHillClimbWorkstreamIsDrivenFromHandInToDeliveryThroughTheRunningService(t *testing.T) {
	t.Parallel()
	engine := newEvaluatorEngine(100, 103, 101, 90, 95, 91)
	j := newClimbJourney(t, engine, climbLoopSpec)
	j.scriptThreeAttempts()
	ctx := context.Background()

	stream := j.calibrated(t)
	moved := intakefixture.AdvanceUpstream(t, j.clone, map[string]string{"UPSTREAM.md": "upstream moved before the owner approved\n"})
	j.approve(t, stream)
	j.awaitClimb(t, stream, climb.InnerConcluding)
	rec := j.awaitConcluded(t, stream)

	attempts := j.attemptsRecorded(t, stream)
	if len(attempts) != 3 || attempts[0].Outcome != climb.OutcomeKept || attempts[1].Cause != climb.CauseProtected || attempts[2].Cause != climb.CauseNoImprovement {
		t.Fatalf("attempts %+v", attempts)
	}
	keptCommit := attempts[0].Commit
	branch := intake.FeatureBranch(stream)

	if rec.Target.Commit != moved || rec.Target.Branch != "main" || rec.Before != keptCommit || rec.Rebased == "" || rec.Rebased == keptCommit {
		t.Fatalf("the climb branch was not rebased once onto the upstream commit pinned at conclusion: %+v", rec)
	}
	if tip := j.git(t, "rev-parse", branch); tip != rec.Rebased {
		t.Fatalf("the climb branch is at %s, not at the rebased commit %s", tip, rec.Rebased)
	}
	if !j.isAncestor(t, moved, rec.Rebased) || j.isAncestor(t, keptCommit, rec.Rebased) {
		t.Fatalf("the rebased commit %s does not sit on %s alone", rec.Rebased, moved)
	}
	if rec.Baseline != 101 || rec.Pre.Score != 90 || rec.Pre.Commit != keptCommit || rec.Pre.MeasuredOn != attempts[0].Candidate || rec.Post == nil || rec.Post.Score != 91 || rec.Post.Commit != rec.Rebased || rec.Post.MeasuredOn != rec.Rebased || len(rec.Kept) != 1 {
		t.Fatalf("the finals %+v / %+v", rec.Pre, rec.Post)
	}
	calls := engine.recorded()
	if len(calls) != 6 {
		t.Fatalf("the evaluator ran %d times: 3 calibration runs, 2 measured attempts and the rebased commit", len(calls))
	}

	j.awaitFeature(t, stream, intake.AssembledState)
	presentation := j.presented(t, stream)
	report := presentation.Report
	if report.Plan != 0 || report.Spec < 1 || len(report.Criteria) != 0 || len(report.Findings) != 0 || report.Commit != rec.Rebased || report.Upstream == nil || report.Upstream.Commit != moved {
		t.Fatalf("the final report %+v", report)
	}
	rebase, rebased, err := intakefixtureRebase(j, stream, report.Review)
	fixturekit.Must(t, err)
	if !rebased || rebase.Commit != rec.Rebased || rebase.Before != keptCommit || rebase.Upstream.Commit != moved {
		t.Fatalf("the final review's rebase %+v is not the conclusion's", rebase)
	}
	system, prompt := finalPromptOf(t, j, stream)
	for _, want := range []string{"Hill Climb", "against the sealed climb spec", "never decide whether a score improved"} {
		if !strings.Contains(system, want) {
			t.Errorf("the final reader's instructions lack %q:\n%s", want, system)
		}
	}
	for _, want := range []string{"climb-spec.json", "climb/attempts/<n>.json", "Results the pull request reports", "rebased once onto", "Final after the rebase"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the final review prompt lacks %q:\n%s", want, prompt)
		}
	}
	if j.reports != 1 {
		t.Fatalf("%d final reviews ran", j.reports)
	}

	if len(j.pulls.Prs) != 1 || j.pulls.Creates != 1 {
		t.Fatalf("pull requests %+v, %d created", j.pulls.Prs, j.pulls.Creates)
	}
	pr := j.pulls.Prs[0]
	for _, want := range []string{
		"Make the parser faster on the corpus.",
		"## Hill Climb results",
		"| Baseline | `",
		"| 101 ms |",
		"| Final before the rebase | `" + keptCommit[:10] + "` | 90 ms | \u221211 (\u221210.9%) |",
		"| Final after the rebase | `" + rec.Rebased[:10] + "` | 91 ms | \u221210 (\u22129.9%) |",
		"Baseline \u2192 final: 101 \u2192 90 before the rebase, 101 \u2192 91 after it, rebased once onto `main` at `" + moved[:10] + "`.",
		"| Attempt | Commit | Hypothesis | Score | Delta |",
		"| 1 | `" + keptCommit[:10] + "` | cache the parsed corpus | 90 ms | \u221211 (\u221210.9%) |",
	} {
		if !strings.Contains(pr.Body, want) {
			t.Errorf("the draft pull request lacks %q:\n%s", want, pr.Body)
		}
	}
	if strings.Contains(pr.Body, "| 2 |") || strings.Contains(pr.Body, "| 3 |") || strings.Contains(pr.Body, "only the evaluator") {
		t.Errorf("the draft lists attempts that were not kept:\n%s", pr.Body)
	}
	if !pr.Draft || pr.State != "open" || pr.Base != "main" || pr.Head != branch {
		t.Fatalf("pull request %+v", pr)
	}
	if j.treeOf(t, pr.HeadCommit) != j.treeOf(t, rec.Rebased) {
		t.Fatalf("the pull request's head %s does not hold the tree the evaluator measured in %s", pr.HeadCommit, rec.Rebased)
	}
	if !j.isAncestor(t, moved, pr.HeadCommit) {
		t.Fatalf("the pull request's head %s is not rebased onto %s", pr.HeadCommit, moved)
	}
	if presentation.Publication == nil || presentation.Publication.PullRequest != pr.Number || !strings.Contains(presentation.Draft, "## Hill Climb results") {
		t.Fatalf("the presentation %+v\n%s", presentation.Publication, presentation.Draft)
	}

	decision := api.DeliveryDecision{Review: report.Review, ReviewRevision: presentation.ReviewRevision, Commit: report.Commit, DraftHash: presentation.DraftHash}
	if _, err := j.c.DeclineDelivery(ctx, stream, api.DeliveryDecline{Review: report.Review, ReviewRevision: presentation.ReviewRevision, Commit: report.Commit}); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("declining a pull request that holds a kept attempt: %v", err)
	}
	if _, err := j.c.ApproveDelivery(ctx, stream, decision); err != nil {
		t.Fatal(err)
	}
	j.awaitFeature(t, stream, intake.DeliveredState)
	if final := j.pulls.Prs[0]; len(j.pulls.Prs) != 1 || j.pulls.Creates != 1 || j.pulls.Readies != 1 || final.Draft || final.HeadCommit != pr.HeadCommit || !strings.Contains(final.Body, "## Hill Climb results") {
		t.Fatalf("pull requests %+v: %d created, %d ready", j.pulls.Prs, j.pulls.Creates, j.pulls.Readies)
	}

	log := j.attemptsEndpoint(t, stream)
	if log.Workstream != stream || len(log.Attempts) != 3 {
		t.Fatalf("the attempt log %+v", log)
	}
	for i, want := range []struct {
		outcome climb.Outcome
		cause   climb.Cause
		score   float64
		hyp     string
	}{{climb.OutcomeKept, "", 90, "cache the parsed corpus"}, {climb.OutcomeDiscarded, climb.CauseProtected, 0, "shrink the corpus"}, {climb.OutcomeDiscarded, climb.CauseNoImprovement, 95, "batch the reads"}} {
		got := log.Attempts[i]
		if got.Number != i+1 || got.Outcome != want.outcome || got.Cause != want.cause || got.Hypothesis != want.hyp || (want.score != 0) != (got.Scores != nil) || got.Scores != nil && got.Scores.Primary != want.score {
			t.Errorf("attempt %d of the log: %+v", i+1, got)
		}
	}
	if log.Attempts[0].Parent == "" || log.Attempts[0].Commit != keptCommit || log.Attempts[2].Parent != keptCommit {
		t.Fatalf("the log's parents %+v", log.Attempts)
	}

	panel := j.progressEndpoint(t, stream).Progress
	if panel.Summary != "3 attempts \u00b7 best \u221210.9%" || panel.Baseline == nil || *panel.Baseline != 101 || panel.Best == nil || *panel.Best != 90 || panel.BestAttempt != 1 || panel.Unit != "ms" ||
		panel.NoiseBand == nil || *panel.NoiseBand != 3 || len(panel.Points) != 3 || panel.Points[0].Class != climb.ClassKept || panel.Points[1].Class != climb.ClassFailed || panel.Points[2].Class != climb.ClassDiscarded ||
		panel.Budgets.Attempts.Used != 3 || panel.Budgets.Attempts.Max != 3 || panel.Running != nil || len(panel.Spark) != 4 {
		t.Fatalf("the climb panel's data %+v", panel)
	}
	status, err := j.c.Status(ctx, stream)
	fixturekit.Must(t, err)
	if status.WorkKind != kindreg.HillClimb || status.Progress == nil || status.Progress.Panel != "climb" || status.Progress.Summary != panel.Summary || len(status.Progress.Spark) != len(panel.Spark) || status.State == nil || *status.State != intake.DeliveredState {
		t.Fatalf("the status line %+v (state %v)", status.Progress, status.State)
	}
	all, err := j.c.Statuses(ctx)
	fixturekit.Must(t, err)
	listed := false
	for _, w := range all.Workstreams {
		if w.Workstream == stream {
			listed = w.Progress != nil && w.Progress.Summary == panel.Summary
		}
	}
	if !listed {
		t.Fatalf("the workstream list does not show the summary line %q", panel.Summary)
	}
}

func intakefixtureRebase(j *climbJourney, stream config.WorkstreamID, review int) (delivery.FinalRebase, bool, error) {
	return (&delivery.FinalReviewer{S: j.s.delivery(), Repository: j.repository()}).Rebase(stream, review)
}

func TestAClimbThatKeptNothingOffersTheEvaluatorOnlyPullRequestAndDecliningItConcludesWithNoImprovement(t *testing.T) {
	t.Parallel()
	engine := newEvaluatorEngine(100, 103, 101, 99, 98, 97, 100)
	j := newClimbJourney(t, engine, climbLoopSpec)
	for n := 1; n <= 3; n++ {
		j.playAttempt(n, map[string]string{fmt.Sprintf("src/a%d.go", n): "package src\n"}, fmt.Sprintf("idea %d", n))
	}
	ctx := context.Background()

	stream := j.calibrated(t)
	j.approve(t, stream)
	rec := j.awaitConcluded(t, stream)
	if !rec.KeptNone() || rec.Attempts != 3 || rec.Pre.Score != 101 || rec.Pre.Commit != rec.BaselineCommit || rec.Post == nil || rec.Post.Score != 100 {
		t.Fatalf("the conclusion %+v", rec)
	}
	j.awaitFeature(t, stream, intake.AssembledState)
	presentation := j.presented(t, stream)
	pr := j.pulls.Prs[0]
	for _, want := range []string{"## Hill Climb results", "| Baseline | `", "The climb kept no attempt, so this pull request holds only the evaluator", "101 \u2192 101 before the rebase, 101 \u2192 100 after it"} {
		if !strings.Contains(pr.Body, want) {
			t.Errorf("the evaluator-only pull request lacks %q:\n%s", want, pr.Body)
		}
	}
	if strings.Contains(pr.Body, "Kept attempts:") || !pr.Draft {
		t.Fatalf("pull request %+v", pr)
	}
	changed := strings.Fields(j.git(t, "diff", "--name-only", rec.Target.Commit, pr.HeadCommit))
	if !slices.Contains(changed, ".dagger/bench/main.go") {
		t.Fatalf("the pull request changes %v, want the evaluator", changed)
	}
	for _, name := range changed {
		if strings.HasPrefix(name, "src/") {
			t.Fatalf("the pull request holds %s, which no kept attempt wrote: %v", name, changed)
		}
	}

	inbox, err := j.c.Inbox(ctx)
	fixturekit.Must(t, err)
	var card api.InboxEntry
	for _, e := range inbox.Entries {
		if e.Kind == api.InboxDelivery && e.Workstream == stream {
			card = e
		}
	}
	if card.Decline == nil || card.Decline.Path != card.Answer.Path+"/decline" || len(card.Options) != 2 || card.Options[0] != "approve" || card.Options[1] != "decline" || !strings.Contains(card.Question, "kept no attempt") {
		t.Fatalf("the delivery card %+v", card)
	}

	stale := api.DeliveryDecline{Review: presentation.Report.Review, ReviewRevision: presentation.ReviewRevision, Commit: "0000000"}
	if _, err := j.c.DeclineDelivery(ctx, stream, stale); !intakefixture.Failed(err, api.Conflict) {
		t.Fatalf("declining a report the owner did not read: %v", err)
	}
	declined, err := j.c.DeclineDelivery(ctx, stream, api.DeliveryDecline{Review: presentation.Report.Review, ReviewRevision: presentation.ReviewRevision, Commit: presentation.Report.Commit, Note: "an evaluator alone is not worth a pull request"})
	fixturekit.Must(t, err)
	if declined.State != intake.ConcludedState || declined.Workstream != stream || declined.PullRequest != pr.Number || !declined.PullRequestClosed {
		t.Fatalf("the decline %+v", declined)
	}
	if got := j.feature(t, stream); got != intake.ConcludedState {
		t.Fatalf("the workstream is %q, want %q", got, intake.ConcludedState)
	}
	if abandoned, err := intake.Abandoned(j.repository(), stream); err != nil || abandoned {
		t.Fatalf("the declined workstream is abandoned: %v %v", abandoned, err)
	}
	closed := j.pulls.Prs[0]
	if closed.State != "closed" || j.pulls.Closes != 1 || len(j.pulls.Comments) != 1 || j.pulls.Readies != 0 || !strings.Contains(j.pulls.Comments[0].Text, "kept no attempt") {
		t.Fatalf("pull request %+v, %d closed, %d comments, %d ready", closed, j.pulls.Closes, len(j.pulls.Comments), j.pulls.Readies)
	}
	if again, err := j.c.DeclineDelivery(ctx, stream, api.DeliveryDecline{Review: presentation.Report.Review, ReviewRevision: presentation.ReviewRevision, Commit: presentation.Report.Commit}); err != nil || again.State != intake.ConcludedState || j.pulls.Closes != 1 || len(j.pulls.Comments) != 1 {
		t.Fatalf("declining twice: %+v %v, %d closed, %d comments", again, err, j.pulls.Closes, len(j.pulls.Comments))
	}
	if _, err := j.c.ApproveDelivery(ctx, stream, api.DeliveryDecision{Review: presentation.Report.Review, ReviewRevision: presentation.ReviewRevision, Commit: presentation.Report.Commit, DraftHash: presentation.DraftHash}); err == nil {
		t.Fatal("a concluded workstream's pull request was approved")
	}
	for _, e := range func() []api.InboxEntry { in, err := j.c.Inbox(ctx); fixturekit.Must(t, err); return in.Entries }() {
		if e.Workstream == stream && e.Kind == api.InboxDelivery {
			t.Fatalf("the delivery card stays open after the decline: %+v", e)
		}
	}

	status, err := j.c.Status(ctx, stream)
	fixturekit.Must(t, err)
	if status.State == nil || *status.State != intake.ConcludedState || status.WorkKind != kindreg.HillClimb {
		t.Fatalf("status %+v", status)
	}
	if panel := j.progressEndpoint(t, stream).Progress; panel.Summary != "3 attempts \u00b7 no improvement yet" || panel.Best == nil || *panel.Best != 101 || panel.BestAttempt != 0 {
		t.Fatalf("the panel %+v", panel)
	}

	archived, err := j.c.Archive(ctx, stream)
	fixturekit.Must(t, err)
	if !archived.Archived {
		t.Fatalf("archive %+v", archived)
	}
	after, err := j.c.Status(ctx, stream)
	fixturekit.Must(t, err)
	if !after.Archived || after.State == nil || *after.State != intake.ConcludedState {
		t.Fatalf("the archived workstream %+v", after)
	}
}

func TestARunInterruptedAfterTheRebaseAndAfterTheDraftOpenedDoesNotRebaseOrOpenAgain(t *testing.T) {
	t.Parallel()
	engine := newEvaluatorEngine(100, 103, 101, 90, 95, 91, 91, 91)
	j := newClimbJourney(t, engine, climbLoopSpec)
	j.scriptThreeAttempts()
	ctx := context.Background()

	stream := j.calibrated(t)
	pinned := intakefixture.AdvanceUpstream(t, j.clone, map[string]string{"UPSTREAM.md": "upstream moved before the owner approved\n"})
	crashed := j.crashAt("climb-conclusion-moved")
	j.approve(t, stream)
	within(t, "the service to crash after the climb branch moved", crashed.Load)
	rec := j.conclusion(t, stream)
	j.stop(t)
	if rec.Phase != climb.ConcludePrepared || rec.Rebased == "" || rec.Target.Commit != pinned {
		t.Fatalf("the conclusion at the crash: %+v", rec)
	}
	if tip := j.git(t, "rev-parse", intake.FeatureBranch(stream)); tip != rec.Rebased {
		t.Fatalf("the climb branch is at %s, not at the rebased commit %s", tip, rec.Rebased)
	}
	newer := intakefixture.AdvanceUpstream(t, j.clone, map[string]string{"NEWER.md": "upstream moved while the service was down\n"})
	if newer == pinned {
		t.Fatal("upstream did not move")
	}
	j.start(t)
	opened := j.crashAt("draft-opened")

	done := j.awaitConcluded(t, stream)
	if done.Target.Commit != pinned || done.Rebased != rec.Rebased || done.Post == nil || done.Post.Commit != rec.Rebased || done.Post.MeasuredOn != rec.Rebased || done.Pre.Commit != rec.Pre.Commit || done.Pre.Score != rec.Pre.Score {
		t.Fatalf("the restarted conclusion moved on: %+v, was %+v", done, rec)
	}
	if j.isAncestor(t, newer, done.Rebased) {
		t.Fatalf("the climb branch was rebased onto the newer upstream %s", newer)
	}
	if tip := j.git(t, "rev-parse", intake.FeatureBranch(stream)); tip != rec.Rebased {
		t.Fatalf("the climb branch is at %s after the restart, not at %s", tip, rec.Rebased)
	}

	within(t, "the service to crash after the draft pull request opened", opened.Load)
	j.pulls.Mu.Lock()
	created, open := j.pulls.Creates, len(j.pulls.Prs)
	j.pulls.Mu.Unlock()
	if created != 1 || open != 1 {
		t.Fatalf("%d pull requests opened, %d exist at the crash", created, open)
	}
	if docs := j.documents(t, stream, delivery.PublicationDocument); len(docs) == 0 {
		t.Fatal("no publication was recorded before the pull request opened")
	} else if last := docs[len(docs)-1]; strings.Contains(last.Content, `"opened"`) {
		t.Fatalf("the pull request was already recorded as opened: %s", last.Content)
	}
	j.stop(t)
	j.start(t)

	presentation := j.presented(t, stream)
	within(t, "the adopted pull request to be recorded", func() bool {
		p, err := j.c.Delivery(ctx, stream)
		return err == nil && p.Publication != nil && p.Publication.PullRequest > 0
	})
	if len(j.pulls.Prs) != 1 || j.pulls.Creates != 1 {
		t.Fatalf("the restart opened another pull request: %+v, %d created", j.pulls.Prs, j.pulls.Creates)
	}
	pr := j.pulls.Prs[0]
	if presentation.Report.Upstream == nil || presentation.Report.Upstream.Commit != pinned || presentation.Report.Commit != rec.Rebased {
		t.Fatalf("the report %+v", presentation.Report)
	}
	published, err := j.c.Delivery(ctx, stream)
	fixturekit.Must(t, err)
	if published.Publication == nil || published.Publication.PullRequest != pr.Number {
		t.Fatalf("the adopted pull request is not the recorded one: %+v", published.Publication)
	}
	for _, want := range []string{"## Hill Climb results", "| Final after the rebase | `" + rec.Rebased[:10] + "`", "rebased once onto `main` at `" + pinned[:10] + "`"} {
		if !strings.Contains(pr.Body, want) {
			t.Errorf("the adopted pull request lacks %q:\n%s", want, pr.Body)
		}
	}
	if j.treeOf(t, pr.HeadCommit) != j.treeOf(t, rec.Rebased) || j.isAncestor(t, newer, pr.HeadCommit) || !j.isAncestor(t, pinned, pr.HeadCommit) {
		t.Fatalf("the pull request's head %s is not the measured tree on the pinned upstream", pr.HeadCommit)
	}
	if _, err := j.c.ApproveDelivery(ctx, stream, api.DeliveryDecision{Review: presentation.Report.Review, ReviewRevision: presentation.ReviewRevision, Commit: presentation.Report.Commit, DraftHash: presentation.DraftHash}); err != nil {
		t.Fatal(err)
	}
	j.awaitFeature(t, stream, intake.DeliveredState)
	if len(j.pulls.Prs) != 1 || j.pulls.Creates != 1 || j.pulls.Readies != 1 || j.pulls.Prs[0].Draft {
		t.Fatalf("pull requests %+v: %d created, %d ready", j.pulls.Prs, j.pulls.Creates, j.pulls.Readies)
	}
	final := j.conclusion(t, stream)
	if final.Rebased != rec.Rebased || final.Target.Commit != pinned {
		t.Fatalf("the conclusion changed after delivery: %+v", final)
	}
	if got := len(j.attemptsRecorded(t, stream)); got != 3 {
		t.Fatalf("%d attempts recorded", got)
	}
}
