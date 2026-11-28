package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"

	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFinalReviewSettlesTheStatusesOfThePushedCommit(t *testing.T) {
	cases := []struct {
		name        string
		post, again func(j *flowJourney, commit string)
		status      string
		detail      string
		failed      string
	}{
		{name: "every check passed", status: "passed", post: func(j *flowJourney, commit string) {
			j.host.loaded(commit)
			j.host.completed(commit, "success", 3)
		}},
		{name: "the run failed", status: "failed", post: func(j *flowJourney, commit string) {
			j.host.loaded(commit)
			j.host.completed(commit, "failure", 3)
		}, again: func(j *flowJourney, commit string) {
			j.host.completed(commit, "failure", 3)
		}},
		{name: "a check failed", status: "failed", failed: "test:resume", post: func(j *flowJourney, commit string) {
			j.host.loaded(commit)
			j.host.completed(commit, "success", 2)
			j.host.failedCheck(commit, "test:resume", "abc123")
		}, again: func(j *flowJourney, commit string) {
			j.host.completed(commit, "success", 2)
			j.host.failedCheck(commit, "test:resume", "abc123")
		}},
		{name: "the run reports no checks", status: "incomplete", detail: "the run reports no checks", post: func(j *flowJourney, commit string) {
			j.host.loaded(commit)
			j.host.completed(commit, "success", 0)
		}},
		{name: "a check is pending", status: "incomplete", detail: "1 of 3 checks have no result", post: func(j *flowJourney, commit string) {
			j.host.loaded(commit)
			j.host.completed(commit, "success", 2)
			j.host.post(commit, pulls.CommitStatus{Context: "test:slow", State: "pending"})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := newFlowJourney(t)
			stream := j.Stream
			var mu sync.Mutex
			var viewed, tracePath, traceViewed string
			var viewErr, traceErr error
			tracePath = "final/checks-1-traces/" + checkrun.TraceDocumentID("abc123", "test:resume") + ".json"
			j.FinalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
				content, err := fixturekit.CallTool(ctx, tools, "file_read", map[string]any{"path": delivery.FinalChecksPath(1)})
				var traced string
				var terr error
				if c.failed != "" {
					traced, terr = fixturekit.CallTool(ctx, tools, "file_read", map[string]any{"path": tracePath})
				}
				mu.Lock()
				viewed, viewErr, traceViewed, traceErr = content, err, traced, terr
				mu.Unlock()
				_, err = fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Uploads resume and skip acknowledged chunks.", "findings": []any{}})
				return err
			})
			j.manager.plan(1, briefPlan{Text: briefText, Items: []string{briefShipFirst}})

			j.start(t)
			first := j.awaitPushes(t, 1)[0]
			j.step(t, 1, 90*time.Second, "no status is read until 2m0s after the push")
			c.post(j, first.Commit)
			at := first.At.Add(delivery.FinalChecksWait + 5*time.Second)

			if c.status == "incomplete" {
				j.stepTo(t, 1, at, "the completed run is not a pass: "+c.detail)
				for _, run := range j.checkRuns(t, 1) {
					if run.Status != delivery.ChecksIncomplete || run.Source != "pull_request" || !strings.Contains(run.Error, c.detail) || run.Counts == nil || run.Counts.Cancelled != 0 {
						t.Fatalf("the check record %+v", run)
					}
				}
				if runs := j.checkRuns(t, 1); len(runs) == 0 {
					t.Fatal("no incomplete record was written")
				}
				if _, started := j.reportOf(t, 1); started || len(j.rerunCalls(t)) != 0 {
					t.Fatalf("a result that is not settled started the final read (reruns %q)", j.rerunCalls(t))
				}
				if reader, err := j.live().Thread(stream, intake.CommitteeAgent(1)); err == nil && len(reader.Turns) != 0 {
					t.Fatalf("the reader was asked %d times without a result", len(reader.Turns))
				}
				return
			}

			if c.status == "failed" {
				j.stepTo(t, 1, at, "the failed checks were rerun")
				if len(j.rerunCalls(t)) != 1 {
					t.Fatalf("rerun calls %q", j.rerunCalls(t))
				}
				c.again(j, first.Commit)
				j.clock.Advance(flowPoll)
			} else {
				j.clock.Advance(at.Sub(j.clock.Peek()))
			}
			run := j.awaitCheckRun(t, 1)
			if run.Status != c.status || run.Source != "pull_request" || run.Push != 1 || run.PushedCommit != first.Commit || run.Counts == nil || run.Counts.Total != 3 || run.Counts.Cancelled != 0 {
				t.Fatalf("the check record %+v", run)
			}
			if c.status == "failed" {
				if len(run.Reruns) != 1 || len(j.rerunCalls(t)) != 1 {
					t.Fatalf("reruns %+v, calls %q", run.Reruns, j.rerunCalls(t))
				}
				if c.failed != "" && (len(run.FailedChecks) != 1 || run.FailedChecks[0].Name != c.failed || run.FailedChecks[0].TraceID != "abc123") {
					t.Fatalf("failed checks %+v", run.FailedChecks)
				}
			}
			j.awaitReport(t, 1)

			reader, err := j.live().Thread(stream, intake.CommitteeAgent(1))
			fixturekit.Must(t, err)
			prompt := reader.Turns[len(reader.Turns)-1].Request.Prompt
			section, view, criteria := strings.Index(prompt, "Check result:\n"), strings.Index(prompt, "Your view holds:"), strings.Index(prompt, "Every criterion and the merged units")
			if section < 0 || view < 0 || criteria < 0 || section > criteria {
				t.Fatalf("check result at %d, view list at %d, criteria at %d:\n%s", section, view, criteria, prompt)
			}
			for _, want := range []string{fmt.Sprintf("The result is %s.", c.status), fmt.Sprintf("push 1, commit %s", first.Commit), "- " + delivery.FinalChecksPath(1) + ":"} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("the reader's prompt lacks %q:\n%s", want, prompt)
				}
			}
			if c.failed != "" && !strings.Contains(prompt, "dagger trace abc123") {
				t.Fatalf("the reader's prompt lacks the failed check's trace:\n%s", prompt)
			}
			if run.StatusURL == "" || !strings.Contains(prompt, run.StatusURL) {
				t.Fatalf("the reader's prompt lacks the status link %q:\n%s", run.StatusURL, prompt)
			}
			if tail := prompt[criteria:]; strings.Contains(tail, "Dagger Cloud ran the project's checks") {
				t.Fatalf("the check result is at the end of the prompt:\n%s", prompt)
			}

			wantCalls := 0
			if c.failed != "" {
				wantCalls = 1
			}
			if calls := j.traceCalls(t); len(calls) != wantCalls || (wantCalls == 1 && calls[0] != "cloud traces view --check=test:resume abc123") {
				t.Fatalf("the fake dagger fetched traces %q", calls)
			}
			if c.failed != "" {
				docs := intakefixture.StreamDocuments(t, j.live(), stream, checkrun.TraceDocumentID("abc123", "test:resume"))
				var rec checkrun.TraceRecord
				if len(docs) != 1 || docs[0].Path != checkrun.TraceDocumentPath("abc123", "test:resume") || json.Unmarshal([]byte(docs[0].Content), &rec) != nil ||
					!strings.Contains(rec.Output, "trace output of: cloud traces view --check=test:resume abc123") || strings.Contains(rec.Output, flowToken) {
					t.Fatalf("the recorded trace output %+v", docs)
				}
				mu.Lock()
				var shown string
				if traceErr != nil || json.Unmarshal([]byte(traceViewed), &shown) != nil || shown != docs[0].Content {
					t.Fatalf("the reader's view holds the trace output %q (%v), the record is %q", traceViewed, traceErr, docs[0].Content)
				}
				mu.Unlock()
				if !strings.Contains(prompt, "final/checks-1-traces/") {
					t.Fatalf("the reader's prompt does not say where the trace output is:\n%s", prompt)
				}
			}

			recorded := intakefixture.StreamDocuments(t, j.live(), stream, delivery.FinalChecksDocument(1))
			mu.Lock()
			defer mu.Unlock()
			var content string
			if viewErr != nil || json.Unmarshal([]byte(viewed), &content) != nil || content != recorded[len(recorded)-1].Content {
				t.Fatalf("the reader's view holds %q (%v), the record is %q", viewed, viewErr, recorded[len(recorded)-1].Content)
			}
		})
	}
}
