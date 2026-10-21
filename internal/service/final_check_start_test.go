package service

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"slices"

	"reflect"

	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"

	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFinalCheckAttemptsStartAfterPreparationAndRetryDelay(t *testing.T) {
	t.Parallel()
	f, stream, repo, a := newFinalFixture(t, "attempt-start")
	_, op := assembleBoth(t, f, repo, a, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	clock := &fixedClock{At: f.clock.Now()}
	f.s.options.Reconciliation.Now = clock.Now
	checks := newHeldChecks()
	checks.links = []string{"dag+check://go/test"}
	provider := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(0.9)}}}}}
	boostJev(f.s, provider)
	f.s.options.reviewChecks = checks
	preparing, release := make(chan string, 1), make(chan struct{})
	defer close(release)
	f.s.boundary = func(name string) error {
		if name == "final-rebase-replaying" || name == "final-checks-exporting" {
			preparing <- name
			<-release
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Each retry uses admission's committee reservation, including after
	// reopening the trace with an existing judgment and unfinished attempt.
	apply := func(ctx context.Context) <-chan error {
		t.Helper()
		release, admitted := f.s.modelOperationLimit(a.repository, nil)(op)
		if !admitted {
			t.Fatal("review did not acquire committee capacity")
		}
		finished := make(chan error, 1)
		go func() {
			_, err := a.Apply(ctx, op)
			release()
			finished <- err
		}()
		return finished
	}
	finished := apply(ctx)
	for _, point := range []string{"final-rebase-replaying", "final-checks-exporting"} {
		if got := awaitCheck(t, preparing); got != point {
			t.Fatalf("preparation boundary %s, want %s", got, point)
		}
		if starts := finalCheckStarts(t, repo, stream); len(starts) != 0 {
			t.Fatalf("checks started during %s: %+v", point, starts)
		}
		release <- struct{}{}
	}
	if links := awaitCheck(t, checks.entered); !slices.Equal(links, checks.links) {
		t.Fatalf("runner links %v, want %v", links, checks.links)
	}
	starts := finalCheckStarts(t, repo, stream)
	if len(starts) != 1 || starts[0].Attempt != 1 || starts[0].Review != 1 || starts[0].Operation != op.ID || starts[0].Commit == "" || starts[0].StartedAt.IsZero() {
		t.Fatalf("held check start: %+v", starts)
	}
	if docs := streamDocuments(t, repo, stream, finalChecksDocument(1)); len(docs) != 0 {
		t.Fatalf("held checks already have a result: %+v", docs)
	}
	checks.release <- checkOutcome{Result: CheckResult{ExitCode: -1}, Err: errors.New("engine unreachable")}
	if err := awaitCheck(t, finished); err == nil {
		t.Fatal("incomplete check attempt succeeded")
	}
	f.s.boundary = nil
	if len(provider.Requests()) != 1 || starts[0].Selection == nil || starts[0].Selection.Mode != checkrun.SelectionSelected {
		t.Fatalf("initial judgment: %d requests, starts %+v", len(provider.Requests()), starts)
	}
	docs := streamDocuments(t, repo, stream, finalChecksDocument(1))
	var incomplete FinalCheckRun
	if len(docs) != 1 {
		t.Fatalf("incomplete results: %+v", docs)
	}
	must(t, json.Unmarshal([]byte(docs[0].Content), &incomplete))
	if !reflect.DeepEqual(incomplete.Selection, starts[0].Selection) || incomplete.Status != ChecksIncomplete || incomplete.Attempt != starts[0].Attempt {
		t.Fatalf("attempt result: %+v", incomplete)
	}
	// Just before the original ten-minute delay ends, retry records no start.
	clock.Advance(incomplete.FinishedAt.Add(finalCheckDelay(1) - time.Nanosecond).Sub(clock.Now()))
	if err := awaitCheck(t, apply(ctx)); err == nil {
		t.Fatal("retry ran before its delay")
	}
	if got := finalCheckStarts(t, repo, stream); !reflect.DeepEqual(got, starts) {
		t.Fatalf("retry waiting recorded a start: %+v", got)
	}
	clock.Advance(time.Nanosecond)
	finished = apply(ctx)
	if links := awaitCheck(t, checks.entered); !slices.Equal(links, checks.links) {
		t.Fatalf("runner links %v, want %v", links, checks.links)
	}
	starts = finalCheckStarts(t, repo, stream)
	if len(starts) != 2 || starts[1].Attempt != 2 || !starts[1].StartedAt.Equal(clock.Now()) {
		t.Fatalf("retry start: %+v", starts)
	}
	if got := streamDocuments(t, repo, stream, finalChecksDocument(1)); !reflect.DeepEqual(got, docs) {
		t.Fatalf("running retry overwrote incomplete result: %+v", got)
	}
	if len(provider.Requests()) != 1 || !reflect.DeepEqual(starts[0].Selection, starts[1].Selection) {
		t.Fatalf("retry did not reuse judgment: %d requests, starts %+v", len(provider.Requests()), starts)
	}
	// Stop the retry while it is running, then reopen the trace. It keeps
	// a start and no result rather than inheriting the earlier attempt's failure.
	cancel()
	if err := awaitCheck(t, finished); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted final checks: %v", err)
	}
	must(t, repo.Close())
	reopened, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	a = &finalReviewer{s: f.s, repository: reopened}
	// A fresh judge has no in-memory decision; recovery must use the trace.
	restartedProvider := &jevtest.Provider{}
	boostJev(f.s, restartedProvider)
	if got := finalCheckStarts(t, reopened, stream); !reflect.DeepEqual(got, starts) {
		t.Fatalf("starts lost on restart: %+v", got)
	}
	if observed, err := a.Inspect(context.Background(), op); err != nil || observed.State != coreadapter.EffectAbsent || observed.Result != nil {
		t.Fatalf("interrupted review completed: %+v %v", observed, err)
	}
	f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Checks completed.", "findings": []any{}})
		return err
	})
	finished = apply(context.Background())
	if links := awaitCheck(t, checks.entered); !slices.Equal(links, checks.links) {
		t.Fatalf("runner links %v, want %v", links, checks.links)
	}
	starts = finalCheckStarts(t, reopened, stream)
	if len(starts) != 3 || starts[2].Attempt != 3 {
		t.Fatalf("restarted attempt: %+v", starts)
	}
	if len(restartedProvider.Requests()) != 0 || !reflect.DeepEqual(starts[0].Selection, starts[2].Selection) {
		t.Fatalf("restart asked Jev again or changed selection: %d requests, starts %+v", len(restartedProvider.Requests()), starts)
	}
	checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
	must(t, awaitCheck(t, finished))
	docs = streamDocuments(t, reopened, stream, finalChecksDocument(1))
	var passed FinalCheckRun
	if len(docs) != 2 {
		t.Fatalf("final results: %+v", docs)
	}
	must(t, json.Unmarshal([]byte(docs[1].Content), &passed))
	if !reflect.DeepEqual(passed.Selection, starts[0].Selection) || passed.Status != ChecksPassed || passed.Attempt != 3 || !passed.StartedAt.Equal(starts[2].StartedAt) {
		t.Fatalf("finished attempt: %+v", passed)
	}
}
