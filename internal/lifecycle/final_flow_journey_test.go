package lifecycle_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func cleanFinalReport(summary string) func(context.Context, *mcp.ClientSession) error {
	return func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": summary, "findings": []any{}})
		return err
	}
}

func blockingFinalReport(ctx context.Context, tools *mcp.ClientSession) error {
	_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Dedupe is not wired in.", "findings": []any{
		map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "The upload path never calls Dedupe.", "action": "Call Dedupe before sending a chunk."}}})
	return err
}

func triageFindingAsCode() briefPlan {
	return briefPlan{Calls: []briefCall{
		{Tool: "decide", Input: map[string]any{"kind": "final-review-triage", "subject": "1:finding-1", "evidence": "The report and the branch agree."}},
		{Tool: "triage_finding", Input: map[string]any{"review": 1, "finding": "finding-1", "decision": "code", "note": "Wire Dedupe into the upload path."}},
	}}
}

func (j *flowJourney) requireDraftPublished(t *testing.T, first delivery.Push) pulls.PullRequest {
	t.Helper()
	if len(j.Pulls.Prs) != 1 || j.Pulls.Creates != 1 || j.pushCount(t) != 1 {
		t.Fatalf("%d pull requests, %d created, %d pushes", len(j.Pulls.Prs), j.Pulls.Creates, j.pushCount(t))
	}
	pr := j.Pulls.Prs[0]
	tip, _ := j.ForkBranch(t)
	if !pr.Draft || pr.State != "open" || pr.Base != "main" || pr.Head != intake.FeatureBranch(j.Stream) || pr.HeadCommit != tip || first.Commit != tip || first.PullRequest != pr.Number || !first.Draft || first.Number != 1 || first.Review != 1 {
		t.Fatalf("pull request %+v, push %+v, branch at %s", pr, first, tip)
	}
	if strings.Contains(pr.Body, "Before you ship") || j.feature(t) != intake.AssembledState {
		t.Fatalf("draft body %q, workstream %s", pr.Body, j.feature(t))
	}
	if docs := intakefixture.StreamDocuments(t, j.live(), j.Stream, delivery.DeliveryDocument); len(docs) != 0 {
		t.Fatalf("an approval was recorded for the draft: %+v", docs)
	}
	return pr
}

func (j *flowJourney) settlePassed(t *testing.T, push delivery.Push) (pulls.CommitStatus, delivery.FinalCheckRun) {
	t.Helper()
	j.host.loaded(push.Commit)
	passed := j.host.completed(push.Commit, "success", 3)
	j.clock.Advance(push.At.Add(delivery.FinalChecksWait + 5*time.Second).Sub(j.clock.Peek()))
	return passed, j.awaitCheckRun(t, push.Review)
}

func TestDraftFirstFlowFromTheLastMergeToDelivery(t *testing.T) {
	ctx := context.Background()
	j := newFlowJourney(t)
	stream := j.Stream
	j.FinalTurn(1, 1, cleanFinalReport("Uploads resume and skip acknowledged chunks."))
	j.manager.plan(1, briefPlan{Text: briefText, Items: []string{briefShipFirst, briefShipNext}})

	j.start(t)
	first := j.awaitPushes(t, 1)[0]
	pr := j.requireDraftPublished(t, first)

	passed, run := j.settlePassed(t, first)
	if run.Status != "passed" || run.Source != "pull_request" || run.PullRequest != pr.Number || run.Push != 1 || run.PushedCommit != first.Commit || run.Commit != first.Reviewed ||
		run.Counts == nil || run.Counts.Total != 3 || run.Counts.Passed != 3 || run.Run == nil || run.Run.ID != passed.ID || run.StatusURL != passed.TargetURL || len(run.Reruns) != 0 {
		t.Fatalf("the check result %+v", run)
	}
	report := j.awaitReport(t, 1)
	if report.Commit != first.Reviewed || len(report.Findings) != 0 {
		t.Fatalf("report %+v", report)
	}

	var presented api.DeliveryPresentation
	intakefixture.Eventually(t, "the delivery card never showed the review's brief and its description", func() bool {
		p, err := j.C.Delivery(ctx, stream)
		fixturekit.Must(t, err)
		presented = p
		return p.Report.Review == 1 && p.Brief != nil && p.Brief.State == api.BriefRecorded && strings.Contains(p.Draft, briefBlock)
	})
	if presented.Checks == nil || presented.Checks.State != api.ChecksStatePassed || presented.Publication == nil || presented.Publication.PullRequest != pr.Number || presented.Brief.Text != briefText {
		t.Fatalf("presentation: checks %+v, publication %+v, brief %+v\n%s", presented.Checks, presented.Publication, presented.Brief, presented.Draft)
	}
	entry := j.awaitEntry(t, stream)
	if !strings.HasPrefix(entry.Question, "Mark ready for review: ") {
		t.Fatalf("the delivery entry asks %q", entry.Question)
	}
	if j.Pulls.Readies != 0 || !j.Pulls.Prs[0].Draft || strings.Contains(j.Pulls.Prs[0].Body, "Before you ship") {
		t.Fatalf("the draft changed before the owner decided: %+v", j.Pulls.Prs[0])
	}
	j.requireNoProblems(t)

	if _, err := j.approvePresented(presented); err != nil {
		t.Fatal(err)
	}
	intakefixture.Eventually(t, "the workstream was never delivered", func() bool { return j.feature(t) == intake.DeliveredState })
	final := j.Pulls.Prs[0]
	if len(j.Pulls.Prs) != 1 || j.Pulls.Creates != 1 || j.Pulls.Readies != 1 || j.Pulls.Updates != 1 || final.Draft || final.State != "open" || !strings.Contains(final.Body, briefBlock) || final.HeadCommit != first.Commit || j.pushCount(t) != 1 || len(j.pushes(t)) != 1 {
		t.Fatalf("pull requests %+v: %d created, %d ready calls, %d updates, %d pushes", j.Pulls.Prs, j.Pulls.Creates, j.Pulls.Readies, j.Pulls.Updates, j.pushCount(t))
	}
	ready, recorded, err := delivery.ReadyAt(j.live(), stream)
	fixturekit.Must(t, err)
	if !recorded || ready.Source != api.ReadyByApproval || ready.PullRequest != pr.Number {
		t.Fatalf("the ready record %+v", ready)
	}

	j.requireNoCheckSlotWait(t)
	if j.checks.calls.Load() != 0 {
		t.Fatalf("the unit check runner was asked %d times", j.checks.calls.Load())
	}
	for _, run := range j.checkRuns(t, 1) {
		if run.Source != "pull_request" {
			t.Fatalf("the final review recorded a check that did not come from its pull request: %+v", run)
		}
	}
	if len(j.rerunCalls(t)) != 0 {
		t.Fatalf("rerun calls %q", j.rerunCalls(t))
	}
}

func TestFinalChecksWaitForTheLoadStatusAndTheCloudRunBeforeSettling(t *testing.T) {
	j := newFlowJourney(t)
	j.FinalTurn(1, 1, cleanFinalReport("Uploads resume and skip acknowledged chunks."))

	j.start(t)
	first := j.awaitPushes(t, 1)[0]
	firstCommit := first.Commit

	j.step(t, 1, 90*time.Second, "no status is read until 2m0s after the push")
	if got := j.host.totalReads(); got != 0 {
		t.Fatalf("%d status reads 90 seconds after the push", got)
	}
	why := j.stepTo(t, 1, first.At.Add(delivery.FinalChecksWait+5*time.Second), "no `load` status")
	if got := j.host.readCount(firstCommit); got != 1 || !strings.Contains(why, "posted no `load` status") {
		t.Fatalf("%d reads of the pushed commit, waiting for %q", got, why)
	}
	if got := j.loadMissingNotices(t, 1, 1); got != 1 {
		t.Fatalf("%d notices that load never ran", got)
	}
	j.awaitHeard(t, "posted no `load` status")

	j.host.loaded(firstCommit)
	j.host.pending(firstCommit)
	j.step(t, 1, flowPoll, "is pending")
	if len(j.checkRuns(t, 1)) != 0 || len(j.rerunCalls(t)) != 0 {
		t.Fatalf("a pending run settled the push: %+v, %d reruns", j.checkRuns(t, 1), len(j.rerunCalls(t)))
	}
	if got := j.loadMissingNotices(t, 1, 1); got != 1 {
		t.Fatalf("%d notices after the load status arrived", got)
	}

	j.restart(t)
	j.step(t, 1, flowPoll, "is pending")
	if j.loadMissingNotices(t, 1, 1) != 1 || j.pushCount(t) != 1 || j.Pulls.Creates != 1 || len(j.pushes(t)) != 1 || len(j.checkRuns(t, 1)) != 0 {
		t.Fatalf("after the restart: %d notices, %d pushes, %d pull requests, runs %+v", j.loadMissingNotices(t, 1, 1), j.pushCount(t), j.Pulls.Creates, j.checkRuns(t, 1))
	}

	j.host.completed(firstCommit, "success", 3)
	j.clock.Advance(flowPoll)
	run := j.awaitCheckRun(t, 1)
	if run.Status != "passed" || run.Push != 1 || run.PushedCommit != firstCommit {
		t.Fatalf("the check result %+v", run)
	}
	if got := j.manager.heardOf("posted no `load` status"); got != 1 {
		t.Fatalf("the chief of staff heard that load never ran %d times", got)
	}
}

func TestFailedCloudChecksAreRerunOnceAndTheFreshRunSettlesTheReview(t *testing.T) {
	j := newFlowJourney(t)
	stream := j.Stream
	j.FinalTurn(1, 1, cleanFinalReport("Uploads resume and skip acknowledged chunks."))

	j.start(t)
	first := j.awaitPushes(t, 1)[0]
	firstCommit := first.Commit
	pr := j.requireDraftPublished(t, first)

	j.host.loaded(firstCommit)
	failedRun := j.host.completed(firstCommit, "failure", 2)
	failedCheck := j.host.failedCheck(firstCommit, "test:resume", "abc123")
	j.stepTo(t, 1, first.At.Add(delivery.FinalChecksWait+5*time.Second), "the failed checks were rerun")
	calls := j.rerunCalls(t)
	if len(calls) != 1 || !strings.HasPrefix(calls[0], fmt.Sprintf("cloud checks rerun --failed --pr %d\n", pr.Number)) || !strings.Contains(calls[0], "pwd="+j.S.Cfg.Root.String()+"/branches/"+string(j.Project)+"/"+string(stream)+"\n") || !strings.HasSuffix(calls[0], "token="+flowToken) {
		t.Fatalf("rerun calls %q", calls)
	}
	requested := j.clock.Peek()

	readsBefore := j.host.readCount(firstCommit)
	why := j.step(t, 1, flowPoll, "posted before the rerun was requested")
	if got := j.host.readCount(firstCommit); got != readsBefore+1 {
		t.Fatalf("%d reads, was %d; waiting for %q", got, readsBefore, why)
	}
	if len(j.checkRuns(t, 1)) != 0 || len(j.rerunCalls(t)) != 1 {
		t.Fatalf("the original failure settled the push: %+v, %d reruns", j.checkRuns(t, 1), len(j.rerunCalls(t)))
	}
	if _, started := j.reportOf(t, 1); started {
		t.Fatal("the final read started on the original failure")
	}

	j.restart(t)
	why = j.step(t, 1, flowPoll, "posted before the rerun was requested")
	if len(j.rerunCalls(t)) != 1 || j.pushCount(t) != 1 || j.Pulls.Creates != 1 || len(j.pushes(t)) != 1 || len(j.checkRuns(t, 1)) != 0 {
		t.Fatalf("after the restart: %d reruns, %d pushes, %d pull requests, runs %+v (waiting for %q)", len(j.rerunCalls(t)), j.pushCount(t), j.Pulls.Creates, j.checkRuns(t, 1), why)
	}

	j.host.pending(firstCommit)
	j.step(t, 1, flowPoll, "is pending")
	fresh := j.host.completed(firstCommit, "success", 2)
	j.host.passedCheck(firstCommit, "test:resume")
	j.clock.Advance(flowPoll)
	run := j.awaitCheckRun(t, 1)
	if run.Status != "passed" || run.Source != "pull_request" || run.PullRequest != pr.Number || run.Push != 1 || run.PushedCommit != firstCommit || run.Commit != first.Reviewed ||
		run.Counts == nil || run.Counts.Total != 3 || run.Counts.Passed != 3 || run.Run == nil || run.Run.ID != fresh.ID || run.StatusURL != fresh.TargetURL ||
		len(run.Flaky) != 1 || run.Flaky[0] != "test:resume" || len(run.Reruns) != 1 || run.Reruns[0].Original.ID != failedRun.ID || run.Reruns[0].Fresh == nil || run.Reruns[0].Fresh.ID != fresh.ID ||
		!run.Reruns[0].RequestedAt.After(failedRun.CreatedAt) || run.Reruns[0].RequestedAt.After(requested) || len(run.Reruns[0].Flakes) != 1 || run.Reruns[0].Flakes[0] != "test:resume" {
		t.Fatalf("the check result %+v (original failure %+v, check %+v)", run, failedRun, failedCheck)
	}

	report := j.awaitReport(t, 1)
	if report.Commit != first.Reviewed || len(report.Findings) != 0 {
		t.Fatalf("report %+v", report)
	}
	reader, err := j.live().Thread(stream, intake.CommitteeAgent(1))
	fixturekit.Must(t, err)
	prompt := reader.Turns[len(reader.Turns)-1].Request.Prompt
	for _, want := range []string{"Dagger Cloud ran the project's checks", "The result is passed.", fmt.Sprintf("pull request #%d, push 1, commit %s", pr.Number, firstCommit), "3 checks, 3 passed, 0 failed, 0 cancelled", "Flaky, failed and then passed on rerun: test:resume", fresh.TargetURL} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the reader's prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, failedRun.TargetURL) {
		t.Fatalf("the reader's prompt carries the original failure:\n%s", prompt)
	}
	if len(j.rerunCalls(t)) != 1 {
		t.Fatalf("rerun calls %q", j.rerunCalls(t))
	}
}

func TestManagerTriagesAFinalReviewFindingWithoutTheOwner(t *testing.T) {
	j := newFlowJourney(t)
	stream := j.Stream
	j.FinalTurn(1, 1, blockingFinalReport)
	j.manager.plan(1, triageFindingAsCode())

	j.start(t)
	first := j.awaitPushes(t, 1)[0]
	j.settlePassed(t, first)
	report := j.awaitReport(t, 1)
	if report.Commit != first.Reviewed || len(report.Findings) != 1 {
		t.Fatalf("report %+v", report)
	}

	intakefixture.Eventually(t, "the chief of staff never triaged the finding", func() bool {
		all, _, err := followup.ReadTriage(j.live(), stream)
		fixturekit.Must(t, err)
		return len(all) == 1
	})
	all, _, err := followup.ReadTriage(j.live(), stream)
	fixturekit.Must(t, err)
	if all[0].Finding != "finding-1" || all[0].Outcome != followup.TriageCode || all[0].ByJev() {
		t.Fatalf("triage %+v (calls %q)", all, j.manager.calls(1))
	}
	if calls := j.manager.calls(1); len(calls) != 2 || !strings.Contains(calls[1], `"recorded":true`) {
		t.Fatalf("the chief of staff's triage calls %q", calls)
	}
	j.requireNoProblems(t)
}

func TestMovedFeatureBranchIsPushedToTheSameDraftAsANewReview(t *testing.T) {
	j := newFlowJourney(t)
	stream := j.Stream
	j.FinalTurn(1, 1, blockingFinalReport)
	j.FinalTurn(2, 1, cleanFinalReport("Uploads resume and skip acknowledged chunks."))
	j.manager.plan(1, triageFindingAsCode())
	j.manager.plan(2, briefPlan{Text: briefText, Items: []string{briefShipFirst, briefShipNext}})

	j.start(t)
	first := j.awaitPushes(t, 1)[0]
	firstCommit := first.Commit
	pr := j.requireDraftPublished(t, first)
	j.settlePassed(t, first)
	j.awaitReport(t, 1)
	intakefixture.Eventually(t, "the chief of staff never triaged the finding", func() bool {
		all, _, err := followup.ReadTriage(j.live(), stream)
		fixturekit.Must(t, err)
		return len(all) == 1
	})
	oldReads := j.host.readCount(firstCommit)

	j.halt(t)
	journey.MoveFeature(t, j.ShedFixture, stream, map[string]string{"internal/trace/upload.go": "package trace\n\n// Upload calls Dedupe before it sends a chunk.\n"})
	j.held = nil
	j.start(t)
	pushes := j.awaitPushes(t, 2)
	second := pushes[1]
	secondCommit := second.Commit
	if second.Number != 2 || second.Review != 2 || second.PullRequest != pr.Number || secondCommit == firstCommit || !second.Draft || j.Pulls.Creates != 1 || len(j.Pulls.Prs) != 1 || j.pushCount(t) != 2 {
		t.Fatalf("second push %+v, %d pull requests created, %d pushes", second, j.Pulls.Creates, j.pushCount(t))
	}
	if tip, _ := j.ForkBranch(t); tip != secondCommit || j.Pulls.Prs[0].Number != pr.Number {
		t.Fatalf("the branch is at %s, the push signed %s", tip, secondCommit)
	}
	if docs := intakefixture.StreamDocuments(t, j.live(), stream, delivery.DeliveryDocument); len(docs) != 0 || j.feature(t) != intake.AssembledState {
		t.Fatalf("the push needed an approval: %+v, workstream %s", docs, j.feature(t))
	}
	j.host.loaded(secondCommit)
	j.host.completed(secondCommit, "success", 3)
	j.step(t, 2, 90*time.Second, "no status is read until 2m0s after the push")
	if j.host.readCount(secondCommit) != 0 {
		t.Fatalf("the new commit's statuses were read before the wait passed")
	}
	j.clock.Advance(second.At.Add(delivery.FinalChecksWait + 5*time.Second).Sub(j.clock.Peek()))
	run := j.awaitCheckRun(t, 2)
	if run.Status != "passed" || run.Push != 2 || run.PushedCommit != secondCommit || run.PullRequest != pr.Number || len(run.Reruns) != 0 || len(run.Flaky) != 0 {
		t.Fatalf("the second check result %+v", run)
	}
	if j.host.readCount(firstCommit) != oldReads || len(j.rerunCalls(t)) != 0 {
		t.Fatalf("the first push's statuses were read again (%d, was %d) or a rerun was made (%d)", j.host.readCount(firstCommit), oldReads, len(j.rerunCalls(t)))
	}
	if got := j.loadMissingNotices(t, 2, 2); got != 0 {
		t.Fatalf("%d load notices for the second push", got)
	}
	if report := j.awaitReport(t, 2); len(report.Findings) != 0 {
		t.Fatalf("report %+v", report)
	}
	intakefixture.Eventually(t, "the delivery card never showed the second review's brief and its description", func() bool {
		p, err := j.C.Delivery(context.Background(), stream)
		fixturekit.Must(t, err)
		return p.Report.Review == 2 && p.Brief != nil && p.Brief.State == api.BriefRecorded && strings.Contains(p.Draft, briefBlock)
	})
}

func (j *flowJourney) reportOf(t *testing.T, review int) (api.FinalReport, bool) {
	t.Helper()
	report, found, err := delivery.LatestFinalReport(j.live(), j.Stream)
	fixturekit.Must(t, err)
	return report, found && report.Review == review
}

func (j *flowJourney) credentialNotices(t *testing.T, review int) int {
	t.Helper()
	entries, err := j.live().Outbox(j.Stream)
	fixturekit.Must(t, err)
	n := 0
	for _, e := range entries {
		if e.Event.ID == trace.EventID(fmt.Sprintf("final-review-%d-credential-notice", review), trace.ManagerKey) {
			n++
		}
	}
	return n
}

func TestAssembledWorkstreamWaitsForAGitHubCredentialThenPublishes(t *testing.T) {
	j := newFlowJourney(t)
	stream := j.Stream
	j.FinalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Uploads resume and skip acknowledged chunks.", "findings": []any{}})
		return err
	})
	j.manager.plan(1, briefPlan{Text: briefText, Items: []string{briefShipFirst}})
	j.Pulls.Mu.Lock()
	j.Pulls.Uncredentialed = true
	j.Pulls.Mu.Unlock()

	j.start(t)
	why := j.awaitWaiting(t, 1, time.Time{}, "GITHUB_TOKEN")
	if !strings.Contains(why, "waits for a GitHub credential") {
		t.Fatalf("final review 1 waits for %q", why)
	}
	for range 3 {
		j.step(t, 1, flowPoll, "GITHUB_TOKEN")
	}
	assertHeld := func() {
		t.Helper()
		j.Pulls.Mu.Lock()
		finds, creates, prs := j.Pulls.Finds, j.Pulls.Creates, len(j.Pulls.Prs)
		j.Pulls.Mu.Unlock()
		if _, pushed := j.ForkBranch(t); pushed || j.pushCount(t) != 0 || finds != 0 || creates != 0 || prs != 0 || len(j.pushes(t)) != 0 {
			t.Fatalf("without a credential: the fork holds the branch %v, %d pushes, %d finds, %d creates, %d pull requests", pushed, j.pushCount(t), finds, creates, prs)
		}
		if runs := j.checkRuns(t, 1); len(runs) != 0 {
			t.Fatalf("without a credential, check records were written: %+v", runs)
		}
		if _, started := j.reportOf(t, 1); started {
			t.Fatal("the final read started without a credential")
		}
		if reads := j.host.totalReads(); reads != 0 {
			t.Fatalf("%d status reads without a credential", reads)
		}
		if entries := j.entries(t, stream); len(entries) != 0 || j.feature(t) != intake.AssembledState {
			t.Fatalf("delivery entries %+v, workstream %s", entries, j.feature(t))
		}
		if reader, err := j.live().Thread(stream, intake.CommitteeAgent(1)); err == nil && len(reader.Turns) != 0 {
			t.Fatalf("the final reader was asked %d times without a credential", len(reader.Turns))
		}
	}
	assertHeld()
	if got := j.credentialNotices(t, 1); got != 1 {
		t.Fatalf("%d notices that the review waits for a credential", got)
	}
	statuses, _, apiErr := journey.InspectForTest(j.S).Statuses()
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	waited := false
	for _, status := range statuses {
		if status.Workstream == stream {
			waited = true
			if !strings.Contains(status.Wait, "GITHUB_TOKEN") {
				t.Fatalf("the workstream's wait is %q", status.Wait)
			}
		}
	}
	if !waited {
		t.Fatal("the workstream is missing from the status")
	}

	j.Pulls.Mu.Lock()
	j.Pulls.Uncredentialed = false
	j.Pulls.Mu.Unlock()
	j.restart(t)
	j.clock.Advance(flowPoll)
	first := j.awaitPushes(t, 1)[0]
	if len(j.Pulls.Prs) != 1 || j.Pulls.Creates != 1 || j.pushCount(t) != 1 || !j.Pulls.Prs[0].Draft || j.Pulls.Prs[0].HeadCommit != first.Commit || first.Review != 1 {
		t.Fatalf("%d pull requests, %d created, %d pushes, push %+v", len(j.Pulls.Prs), j.Pulls.Creates, j.pushCount(t), first)
	}
	if j.credentialNotices(t, 1) != 1 {
		t.Fatalf("%d credential notices after the restart", j.credentialNotices(t, 1))
	}
	j.awaitWaiting(t, 1, first.At, "no status is read until 2m0s after the push")
	if reads := j.host.totalReads(); reads != 0 {
		t.Fatalf("%d status reads right after the push", reads)
	}
	if runs := j.checkRuns(t, 1); len(runs) != 0 {
		t.Fatalf("a check result before the wait passed: %+v", runs)
	}
	if _, started := j.reportOf(t, 1); started {
		t.Fatal("the final read started before the checks completed")
	}
	j.host.loaded(first.Commit)
	passed := j.host.completed(first.Commit, "success", 3)
	j.clock.Advance(flowPoll)
	run := j.awaitCheckRun(t, 1)
	if run.Status != "passed" || run.Source != "pull_request" || run.Push != 1 || run.PushedCommit != first.Commit || run.Run == nil || run.Run.ID != passed.ID {
		t.Fatalf("the check result %+v", run)
	}
	report := j.awaitReport(t, 1)
	if report.Commit != first.Reviewed || j.pushCount(t) != 1 || j.Pulls.Creates != 1 || len(j.pushes(t)) != 1 || len(j.rerunCalls(t)) != 0 {
		t.Fatalf("report %+v, %d pushes, %d pull requests, %d reruns", report, j.pushCount(t), j.Pulls.Creates, len(j.rerunCalls(t)))
	}
}
