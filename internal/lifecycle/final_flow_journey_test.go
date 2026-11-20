package lifecycle_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A workstream whose last unit has merged goes through the whole draft-first
// flow on a started service, with a fake pull request host that shows Dagger
// Cloud statuses and a fake `dagger` on the search path:
//
//   - the service assembles it, signs and pushes the branch and opens the pull
//     request as a draft, with no approval;
//   - no status is read for 2 minutes after the push, the first read finds no
//     `load`, and the chief of staff gets one notice;
//   - the statuses arrive late, the run fails, and one `dagger cloud checks
//     rerun --failed` is made from the workstream's checkout; the original
//     failure keeps showing across polls and across a restart without being
//     taken as the rerun's result, and only the rerun's own posting settles
//     it, recording the failed check as a flake;
//   - the final read starts on that fresh result and finds a blocking problem
//     that the chief of staff triages as needing code;
//   - the revision's commit is pushed to the same pull request with no
//     approval, a new cycle passes, and the chief of staff records the brief;
//   - the owner approves "Mark ready for review", which adds the "Before you
//     ship" items to the draft, marks it ready and delivers the workstream.
//
// The final stage takes no check slot, because every slot is held for the
// whole journey, and runs no unit check selection.
func TestDraftFirstFlowFromTheLastMergeToDelivery(t *testing.T) {
	ctx := context.Background()
	j := newFlowJourney(t)
	stream := j.stream

	// The reader of final review 1 finds one blocking problem; the reader of
	// final review 2 finds none. The chief of staff triages the first review's
	// finding as needing code and records the brief of the second.
	j.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Dedupe is not wired in.", "findings": []any{
			map[string]any{"severity": "blocking", "criteria": []any{"spec#2"}, "finding": "The upload path never calls Dedupe.", "action": "Call Dedupe before sending a chunk."}}})
		return err
	})
	j.finalTurn(2, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Uploads resume and skip acknowledged chunks.", "findings": []any{}})
		return err
	})
	j.chief.plan(1, briefPlan{Calls: []briefCall{
		{Tool: "decide", Input: map[string]any{"kind": "final-review-triage", "subject": "1:finding-1", "evidence": "The report and the branch agree."}},
		{Tool: "triage_finding", Input: map[string]any{"review": 1, "finding": "finding-1", "decision": "code", "note": "Wire Dedupe into the upload path."}},
	}})
	j.chief.plan(2, briefPlan{Text: briefText, Items: []string{briefShipFirst, briefShipNext}})

	// The last unit merged: the service assembles the workstream, rebases it,
	// pushes it and opens the draft with nobody's approval.
	j.start(t)
	first := j.awaitPushes(t, 1)[0]
	if len(j.pulls.Prs) != 1 || j.pulls.Creates != 1 || j.pushCount(t) != 1 {
		t.Fatalf("%d pull requests, %d created, %d pushes", len(j.pulls.Prs), j.pulls.Creates, j.pushCount(t))
	}
	pr := j.pulls.Prs[0]
	tip, _ := j.forkBranch(t)
	if !pr.Draft || pr.State != "open" || pr.Base != "main" || pr.Head != featureBranch(stream) || pr.HeadCommit != tip || first.Commit != tip || first.PullRequest != pr.Number || !first.Draft || first.Number != 1 || first.Review != 1 {
		t.Fatalf("pull request %+v, push %+v, branch at %s", pr, first, tip)
	}
	if strings.Contains(pr.Body, "Before you ship") || j.feature(t) != AssembledState {
		t.Fatalf("draft body %q, workstream %s", pr.Body, j.feature(t))
	}
	if docs := streamDocuments(t, j.live(), stream, delivery.DeliveryDocument); len(docs) != 0 {
		t.Fatalf("an approval was recorded for the draft: %+v", docs)
	}
	firstCommit := first.Commit

	// No status is read before the 2 minutes after the push have passed.
	j.step(t, 1, 90*time.Second, "no status is read until 2m0s after the push")
	if got := j.host.totalReads(); got != 0 {
		t.Fatalf("%d status reads 90 seconds after the push", got)
	}
	// The first read, just after the wait, finds nothing posted yet: the chief
	// of staff gets one notice for the push, and the watch goes on.
	why := j.stepTo(t, 1, first.At.Add(delivery.FinalChecksWait+5*time.Second), "no `load` status")
	if got := j.host.readCount(firstCommit); got != 1 || !strings.Contains(why, "posted no `load` status") {
		t.Fatalf("%d reads of the pushed commit, waiting for %q", got, why)
	}
	if got := j.loadMissingNotices(t, 1, 1); got != 1 {
		t.Fatalf("%d notices that load never ran", got)
	}
	j.awaitHeard(t, "posted no `load` status")

	// The statuses arrive late: the module loaded and the run is pending.
	j.host.loaded(firstCommit)
	j.host.pending(firstCommit)
	j.step(t, 1, flowPoll, "is pending")
	if len(j.checkRuns(t, 1)) != 0 || len(j.rerunCalls(t)) != 0 {
		t.Fatalf("a pending run settled the push: %+v, %d reruns", j.checkRuns(t, 1), len(j.rerunCalls(t)))
	}

	// The run completes with one failed check: one rerun of the failed checks
	// is requested, from the workstream's checkout, with the Cloud token.
	failedRun := j.host.completed(firstCommit, "failure", 2)
	failedCheck := j.host.failedCheck(firstCommit, "test:resume", "abc123")
	j.step(t, 1, flowPoll, "the failed checks were rerun")
	calls := j.rerunCalls(t)
	if len(calls) != 1 || !strings.HasPrefix(calls[0], fmt.Sprintf("cloud checks rerun --failed --pr %d\n", pr.Number)) || !strings.Contains(calls[0], "pwd="+j.s.cfg.Root.String()+"/branches/"+string(j.project)+"/"+string(stream)+"\n") || !strings.HasSuffix(calls[0], "token="+flowToken) {
		t.Fatalf("rerun calls %q", calls)
	}
	if got := j.loadMissingNotices(t, 1, 1); got != 1 {
		t.Fatalf("%d notices after the rerun", got)
	}
	requested := j.clock.Peek()

	// The host still shows the original failure on the next poll. It is not
	// the rerun's result: nothing is recorded, nothing is read and no second
	// rerun is made.
	readsBefore := j.host.readCount(firstCommit)
	why = j.step(t, 1, flowPoll, "posted before the rerun was requested")
	if got := j.host.readCount(firstCommit); got != readsBefore+1 {
		t.Fatalf("%d reads, was %d; waiting for %q", got, readsBefore, why)
	}
	if len(j.checkRuns(t, 1)) != 0 || len(j.rerunCalls(t)) != 1 {
		t.Fatalf("the original failure settled the push: %+v, %d reruns", j.checkRuns(t, 1), len(j.rerunCalls(t)))
	}
	if _, started := j.reportOf(t, 1); started {
		t.Fatal("the final read started on the original failure")
	}

	// The service restarts while it watches. Nothing is pushed, rerun or
	// noticed again, and the original failure is still not the rerun's result.
	j.restart(t)
	why = j.step(t, 1, flowPoll, "posted before the rerun was requested")
	if len(j.rerunCalls(t)) != 1 || j.loadMissingNotices(t, 1, 1) != 1 || j.pushCount(t) != 1 || j.pulls.Creates != 1 || len(j.pushes(t)) != 1 || len(j.checkRuns(t, 1)) != 0 {
		t.Fatalf("after the restart: %d reruns, %d notices, %d pushes, %d pull requests, runs %+v (waiting for %q)", len(j.rerunCalls(t)), j.loadMissingNotices(t, 1, 1), j.pushCount(t), j.pulls.Creates, j.checkRuns(t, 1), why)
	}

	// The rerun's own posting starts to arrive, pending; then it completes and
	// the check that failed shows as passed again.
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
	if run.Selection != nil || run.Attempt != 0 || len(run.Command) != 0 || run.Engines != nil {
		t.Fatalf("the check result carries a local run: %+v", run)
	}

	// The final read ran on the fresh result.
	report := j.awaitReport(t, 1)
	if report.Commit != first.Reviewed || len(report.Findings) != 1 {
		t.Fatalf("report %+v", report)
	}
	reader, err := j.live().Thread(stream, committeeAgent(1))
	must(t, err)
	prompt := reader.Turns[len(reader.Turns)-1].Request.Prompt
	for _, want := range []string{"Dagger Cloud ran the project's checks", "The result is passed.", fmt.Sprintf("pull request #%d, push 1, commit %s", pr.Number, firstCommit), "3 checks, 3 passed, 0 failed, 0 cancelled", "Flaky, failed and then passed on rerun: test:resume", fresh.TargetURL} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the reader's prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, failedRun.TargetURL) {
		t.Fatalf("the reader's prompt carries the original failure:\n%s", prompt)
	}

	// The chief of staff triages the blocking finding as needing code.
	intakefixture.Eventually(t, "the chief of staff never triaged the finding", func() bool {
		all, _, err := followup.ReadTriage(j.live(), stream)
		must(t, err)
		return len(all) == 1
	})
	all, _, err := followup.ReadTriage(j.live(), stream)
	must(t, err)
	if all[0].Finding != "finding-1" || all[0].Outcome != followup.TriageCode || all[0].ByJev() {
		t.Fatalf("triage %+v (calls %q)", all, j.chief.calls(1))
	}
	if calls := j.chief.calls(1); len(calls) != 2 || !strings.Contains(calls[1], `"recorded":true`) {
		t.Fatalf("the chief of staff's triage calls %q", calls)
	}
	j.requireNoProblems(t)

	// The revision's commit lands on the feature branch while the service is
	// stopped. The service pushes it to the same pull request without an
	// approval and starts a new cycle: its statuses are read only 2 minutes
	// after that push, and the old commit's are not read again.
	j.halt(t)
	moveFeature(t, j.shedFixture, stream, map[string]string{"internal/trace/upload.go": "package trace\n\n// Upload calls Dedupe before it sends a chunk.\n"})
	j.held = nil
	j.start(t)
	pushes := j.awaitPushes(t, 2)
	second := pushes[1]
	secondCommit := second.Commit
	if second.Number != 2 || second.Review != 2 || second.PullRequest != pr.Number || secondCommit == firstCommit || !second.Draft || j.pulls.Creates != 1 || len(j.pulls.Prs) != 1 || j.pushCount(t) != 2 {
		t.Fatalf("second push %+v, %d pull requests created, %d pushes", second, j.pulls.Creates, j.pushCount(t))
	}
	if tip, _ := j.forkBranch(t); tip != secondCommit || j.pulls.Prs[0].Number != pr.Number {
		t.Fatalf("the branch is at %s, the push signed %s", tip, secondCommit)
	}
	if docs := streamDocuments(t, j.live(), stream, delivery.DeliveryDocument); len(docs) != 0 || j.feature(t) != AssembledState {
		t.Fatalf("the push needed an approval: %+v, workstream %s", docs, j.feature(t))
	}
	oldReads := j.host.readCount(firstCommit)
	j.host.loaded(secondCommit)
	j.host.completed(secondCommit, "success", 3)
	j.step(t, 2, 90*time.Second, "no status is read until 2m0s after the push")
	if j.host.readCount(secondCommit) != 0 {
		t.Fatalf("the new commit's statuses were read before the wait passed")
	}
	j.clock.Advance(second.At.Add(delivery.FinalChecksWait + 5*time.Second).Sub(j.clock.Peek()))
	run = j.awaitCheckRun(t, 2)
	if run.Status != "passed" || run.Push != 2 || run.PushedCommit != secondCommit || run.PullRequest != pr.Number || len(run.Reruns) != 0 || len(run.Flaky) != 0 || run.Selection != nil {
		t.Fatalf("the second check result %+v", run)
	}
	if j.host.readCount(firstCommit) != oldReads || len(j.rerunCalls(t)) != 1 {
		t.Fatalf("the first push's statuses were read again (%d, was %d) or a rerun was made (%d)", j.host.readCount(firstCommit), oldReads, len(j.rerunCalls(t)))
	}
	report = j.awaitReport(t, 2)
	if len(report.Findings) != 0 {
		t.Fatalf("report %+v", report)
	}

	// The chief of staff records the brief, and the owner's card appears with
	// the findings, the check result and the brief.
	// A presentation read while the brief was being recorded can show the brief
	// without its items in the description; the owner reads it again.
	var presented api.DeliveryPresentation
	intakefixture.Eventually(t, "the delivery card never showed the second review's brief and its description", func() bool {
		p, err := j.c.Delivery(ctx, stream)
		must(t, err)
		presented = p
		return p.Report.Review == 2 && p.Brief != nil && p.Brief.State == api.BriefRecorded && strings.Contains(p.Draft, briefBlock)
	})
	if presented.Checks == nil || presented.Checks.State != api.ChecksStatePassed || presented.Publication == nil || presented.Publication.PullRequest != pr.Number || presented.Brief.Text != briefText {
		t.Fatalf("presentation: checks %+v, publication %+v, brief %+v\n%s", presented.Checks, presented.Publication, presented.Brief, presented.Draft)
	}
	entry := j.awaitEntry(t, stream)
	if !strings.HasPrefix(entry.Question, "Mark ready for review: ") {
		t.Fatalf("the delivery entry asks %q", entry.Question)
	}
	if j.pulls.Readies != 0 || !j.pulls.Prs[0].Draft || strings.Contains(j.pulls.Prs[0].Body, "Before you ship") {
		t.Fatalf("the draft changed before the owner decided: %+v", j.pulls.Prs[0])
	}
	j.requireNoProblems(t)

	// The owner approves. The service puts the items on the draft, marks it
	// ready and delivers the workstream; it opens nothing and pushes nothing.
	if _, err := j.approvePresented(presented); err != nil {
		t.Fatal(err)
	}
	intakefixture.Eventually(t, "the workstream was never delivered", func() bool { return j.feature(t) == DeliveredState })
	final := j.pulls.Prs[0]
	if len(j.pulls.Prs) != 1 || j.pulls.Creates != 1 || j.pulls.Readies != 1 || j.pulls.Updates != 1 || final.Draft || final.State != "open" || !strings.Contains(final.Body, briefBlock) || final.HeadCommit != secondCommit || j.pushCount(t) != 2 || len(j.pushes(t)) != 2 {
		t.Fatalf("pull requests %+v: %d created, %d ready calls, %d updates, %d pushes", j.pulls.Prs, j.pulls.Creates, j.pulls.Readies, j.pulls.Updates, j.pushCount(t))
	}
	ready, recorded, err := delivery.ReadyAt(j.live(), stream)
	must(t, err)
	if !recorded || ready.Source != api.ReadyByApproval || ready.PullRequest != pr.Number {
		t.Fatalf("the ready record %+v", ready)
	}

	// The final stage took no check slot and made no check selection or local
	// run, and none of its records carries one.
	j.requireNoCheckSlotWait(t)
	if j.checks.calls.Load() != 0 {
		t.Fatalf("the unit check runner was asked %d times", j.checks.calls.Load())
	}
	for _, review := range []int{1, 2} {
		for _, run := range j.checkRuns(t, review) {
			if run.Selection != nil || run.Attempt != 0 || len(run.Command) != 0 || run.Engines != nil || run.Source != "pull_request" {
				t.Fatalf("final review %d recorded a local check: %+v", review, run)
			}
		}
		for _, d := range streamDocuments(t, j.live(), stream, delivery.FinalCheckAttemptDocument(review, 1)) {
			t.Fatalf("final review %d recorded a local attempt: %+v", review, d)
		}
	}
	if got := j.loadMissingNotices(t, 2, 2); got != 0 {
		t.Fatalf("%d load notices for the second push", got)
	}
	if len(j.rerunCalls(t)) != 1 {
		t.Fatalf("rerun calls %q", j.rerunCalls(t))
	}
	if got := j.chief.heardOf("posted no `load` status"); got != 1 {
		t.Fatalf("the chief of staff heard that load never ran %d times", got)
	}
}

// reportOf returns final review k's recorded report, if the reader
// has recorded one.
func (j *flowJourney) reportOf(t *testing.T, review int) (FinalReport, bool) {
	t.Helper()
	report, found, err := latestFinalReport(j.live(), j.stream)
	must(t, err)
	return report, found && report.Review == review
}

// credentialNotices counts the chief of staff's notices that final review k
// waits for a GitHub credential.
func (j *flowJourney) credentialNotices(t *testing.T, review int) int {
	t.Helper()
	entries, err := j.live().Outbox(j.stream)
	must(t, err)
	n := 0
	for _, e := range entries {
		if e.Event.ID == trace.EventID(fmt.Sprintf("final-review-%d-credential-notice", review), "chief") {
			n++
		}
	}
	return n
}

// A service started without a GitHub credential takes the assembled
// workstream through its final rebase and holds it there: nothing is pushed,
// no pull request is opened or read, no check record is written, the final
// read does not start and no delivery card appears. The wait names the
// missing GITHUB_TOKEN in the workstream's status and in one notice to the
// chief of staff. Restarted with a credential, the same review signs and
// pushes the commit and opens the draft once, reads no status for 2 minutes,
// watches the statuses to a completed result and only then starts the final
// read.
func TestAssembledWorkstreamWaitsForAGitHubCredentialThenPublishes(t *testing.T) {
	j := newFlowJourney(t)
	stream := j.stream
	j.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Uploads resume and skip acknowledged chunks.", "findings": []any{}})
		return err
	})
	j.chief.plan(1, briefPlan{Text: briefText, Items: []string{briefShipFirst}})
	j.pulls.Mu.Lock()
	j.pulls.Uncredentialed = true
	j.pulls.Mu.Unlock()

	j.start(t)
	why := j.awaitWaiting(t, 1, time.Time{}, "GITHUB_TOKEN")
	if !strings.Contains(why, "waits for a GitHub credential") {
		t.Fatalf("final review 1 waits for %q", why)
	}
	// Time passing changes nothing: the review keeps waiting, one attempt
	// after another, and touches neither the fork nor the host.
	for range 3 {
		j.step(t, 1, flowPoll, "GITHUB_TOKEN")
	}
	assertHeld := func() {
		t.Helper()
		j.pulls.Mu.Lock()
		finds, creates, prs := j.pulls.Finds, j.pulls.Creates, len(j.pulls.Prs)
		j.pulls.Mu.Unlock()
		if _, pushed := j.forkBranch(t); pushed || j.pushCount(t) != 0 || finds != 0 || creates != 0 || prs != 0 || len(j.pushes(t)) != 0 {
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
		if entries := j.entries(t, stream); len(entries) != 0 || j.feature(t) != AssembledState {
			t.Fatalf("delivery entries %+v, workstream %s", entries, j.feature(t))
		}
		if reader, err := j.live().Thread(stream, committeeAgent(1)); err == nil && len(reader.Turns) != 0 {
			t.Fatalf("the final reader was asked %d times without a credential", len(reader.Turns))
		}
	}
	assertHeld()
	if got := j.credentialNotices(t, 1); got != 1 {
		t.Fatalf("%d notices that the review waits for a credential", got)
	}
	statuses, _, apiErr := j.s.statuses()
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

	// A restart with a credential retries the review through the draft
	// publication.
	j.pulls.Mu.Lock()
	j.pulls.Uncredentialed = false
	j.pulls.Mu.Unlock()
	j.restart(t)
	j.clock.Advance(flowPoll)
	first := j.awaitPushes(t, 1)[0]
	if len(j.pulls.Prs) != 1 || j.pulls.Creates != 1 || j.pushCount(t) != 1 || !j.pulls.Prs[0].Draft || j.pulls.Prs[0].HeadCommit != first.Commit || first.Review != 1 {
		t.Fatalf("%d pull requests, %d created, %d pushes, push %+v", len(j.pulls.Prs), j.pulls.Creates, j.pushCount(t), first)
	}
	if j.credentialNotices(t, 1) != 1 {
		t.Fatalf("%d credential notices after the restart", j.credentialNotices(t, 1))
	}
	// The attempt that published waits out the 2 minutes after the push.
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
	if report.Commit != first.Reviewed || j.pushCount(t) != 1 || j.pulls.Creates != 1 || len(j.pushes(t)) != 1 || len(j.rerunCalls(t)) != 0 {
		t.Fatalf("report %+v, %d pushes, %d pull requests, %d reruns", report, j.pushCount(t), j.pulls.Creates, len(j.rerunCalls(t)))
	}
}
