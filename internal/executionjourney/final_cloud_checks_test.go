package executionjourney

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFinalReviewWaitsForCloudChecksThatComplete(t *testing.T) {
	t.Parallel()
	d := newDraftFixture(t, "squash")
	ctx := context.Background()
	var pending atomic.Bool
	pending.Store(true)
	d.pulls.Statuses = func(_ context.Context, repository, commit string) (pulls.CommitStatuses, error) {
		read := fixturekit.PassingStatuses(repository, commit)
		if pending.Load() {
			read.Statuses[1].State, read.Statuses[1].TargetURL = "pending", ""
		}
		return read, nil
	}
	var reads atomic.Int32
	d.f.FinalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		reads.Add(1)
		_, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Uploads resume.", "findings": []any{}})
		return err
	})
	entry := func() (api.InboxEntry, bool) {
		t.Helper()
		state, err := d.repository.Workflow(d.stream, delivery.FinalReviewSubject)
		fixturekit.Must(t, err)
		e, open, err := delivery.FinalChecksEntry(d.repository, trace.WorkstreamStatus{Workstream: d.stream, Subjects: map[string]trace.WorkflowState{delivery.FinalReviewSubject: state}})
		fixturekit.Must(t, err)
		return e, open
	}

	if _, err := d.a.Apply(ctx, d.first); err == nil || !strings.Contains(err.Error(), "final review 1 waits for the checks of push 1") || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("the review did not wait for the pending run: %v", err)
	}
	if n := len(d.pushes(t)); n != 1 || reads.Load() != 0 {
		t.Fatalf("pushes %d, reads %d", n, reads.Load())
	}
	timeout := d.f.S.About(d.repository).Project.CheckTimeout()
	d.f.Clock.At = d.f.Clock.At.Add(timeout + time.Minute)
	for n := 1; n <= 3; n++ {
		if _, open := entry(); open {
			t.Fatalf("the inbox reports %d incomplete runs", n-1)
		}
		if _, err := d.a.Apply(ctx, d.first); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("final review 1 waits for its checks: %d runs in a row", n)) {
			t.Fatalf("run %d: %v", n, err)
		}
		if _, err := d.a.Apply(ctx, d.first); err == nil || !strings.Contains(err.Error(), "runs in a row") {
			t.Fatalf("the checks were read again before the wait passed: %v", err)
		}
		d.f.Clock.At = d.f.Clock.At.Add(delivery.FinalCheckDelay(n) + time.Minute)
	}
	e, open := entry()
	if !open || e.Kind != api.InboxFinalChecks || e.Revision != 1 || !strings.Contains(e.Question, "did not complete 3 times in a row") || !strings.Contains(e.Question, "no completed") || !strings.Contains(e.Blocked, "waiting longer after each run") {
		t.Fatalf("the inbox entry %+v, open %v", e, open)
	}
	if reads.Load() != 0 {
		t.Fatalf("the reader started on incomplete checks")
	}

	pending.Store(false)
	result, err := d.a.Apply(ctx, d.first)
	fixturekit.Must(t, err)
	if result.Outcome != "succeeded" || reads.Load() != 1 {
		t.Fatalf("result %+v after %d reads", result, reads.Load())
	}
	runs := intakefixture.StreamDocuments(t, d.repository, d.stream, delivery.FinalChecksDocument(1))
	var run delivery.FinalCheckRun
	fixturekit.Must(t, json.Unmarshal([]byte(runs[len(runs)-1].Content), &run))
	push := d.pushes(t)[0]
	if len(runs) != 4 || run.Status != checks.ChecksPassed || run.Source != "pull_request" || run.PullRequest != push.PullRequest || run.Push != 1 || run.PushedCommit != push.Commit || run.Commit != push.Reviewed {
		t.Fatalf("the final check run %+v of %d records", run, len(runs))
	}
	reader, err := d.repository.Thread(d.stream, intake.CommitteeAgent(1))
	fixturekit.Must(t, err)
	if prompt := reader.Turns[len(reader.Turns)-1].Request.Prompt; !strings.Contains(prompt, "Dagger Cloud ran the project's checks") || !strings.Contains(prompt, "The result is passed.") || !strings.Contains(prompt, fmt.Sprintf("pull request #%d, push 1, commit %s", push.PullRequest, push.Commit)) {
		t.Fatalf("the reader's prompt lacks the check result:\n%s", prompt)
	}
	if _, open := entry(); open {
		t.Fatal("the inbox still reports the checks once a run completed")
	}
}
