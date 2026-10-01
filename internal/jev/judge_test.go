package jev_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevtest"
	"github.com/kpenfound/osmia/internal/systemone"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	projectID config.ProjectID    = "p_00000000000000000000000000000001"
	streamID  config.WorkstreamID = "w_00000000000000000000000000000001"
)

var at = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type fixture struct {
	root    config.Root
	project config.Project
	repo    *trace.Repository
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(base, "target")
	cmd := exec.Command("git", "init", "--quiet", clone)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR="}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	f := &fixture{root: root, project: config.Project{ID: projectID, Clone: clone}}
	owner := trace.Actor{Kind: "owner", ID: "local"}
	if f.repo, err = trace.Create(context.Background(), root, f.project, at, owner); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.CreateWorkstream(context.Background(), streamID, at, owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.repo.Close() })
	return f
}

// reopen closes and reopens the trace, as a restart does.
func (f *fixture) reopen(t *testing.T) {
	t.Helper()
	if err := f.repo.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := trace.Open(f.root, f.project)
	if err != nil {
		t.Fatal(err)
	}
	f.repo = r
}

func (f *fixture) records(t *testing.T, id string) []jev.Record {
	t.Helper()
	docs, err := trace.Read[trace.Document](f.repo, streamID)
	if err != nil {
		t.Fatal(err)
	}
	var out []jev.Record
	for _, d := range docs {
		if d.ID != id {
			continue
		}
		if d.Path != jev.Path(id) || d.Actor != (trace.Actor{Kind: "service", ID: jev.Role}) || d.Cause != "request_1" {
			t.Fatalf("document = %+v", d.Header)
		}
		var r jev.Record
		if err := json.Unmarshal([]byte(d.Content), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (f *fixture) costs(t *testing.T) []trace.Cost {
	t.Helper()
	costs, err := trace.Read[trace.Cost](f.repo, streamID)
	if err != nil {
		t.Fatal(err)
	}
	return costs
}

func enabled() config.Jev {
	return config.Jev{Enabled: true, URL: config.DefaultJevURL, Model: config.DefaultJevModel, APIKeyEnv: "TEST_JEV_KEY", Timeout: "1s"}
}

func judge(settings *config.Jev, p *jevtest.Provider, key string) *jev.Judge {
	return &jev.Judge{
		Config:   func() config.Jev { return *settings },
		Provider: p.Factory(),
		Getenv: func(name string) string {
			if name == "TEST_JEV_KEY" {
				return key
			}
			return ""
		},
		Now: func() time.Time { return at },
	}
}

func judgment(state string) jev.Judgment {
	return jev.Judgment{
		Scope:   coreadapter.Scope{Project: string(projectID), Workstream: string(streamID), Unit: "unit1", Thread: "mason1", Turn: "turn1", Role: "mason"},
		Cause:   "request_1",
		Task:    "mason-classification",
		Version: 1,
		Sources: []jev.Source{{Kind: "turn-response", ID: "response_1", Revision: 1}},
		Request: systemone.Request{State: state, Questions: map[string]systemone.Question{
			"class": systemone.Choice("How did the mason end its turn?", systemone.Option{Name: "claims_done"}, systemone.Option{Name: "gave_up"}),
		}},
		Accept: func(r systemone.Response) string {
			if r.Answers["class"].Confidence < 0.8 {
				return "class confidence below 0.8"
			}
			return ""
		},
	}
}

func answer(confidence float64) jevtest.Result {
	return jevtest.Result{Response: systemone.Response{
		Model:   "jev-1.13.0",
		Answers: map[string]systemone.Answer{"class": jevtest.Choice("claims_done", 0.9, confidence, "gave_up")},
		Usage:   systemone.Usage{InputTokens: 120, OutputTokens: 10, CostUSD: 0.000005, CostKnown: true},
	}}
}

func TestDisabledBoostSendsAndRecordsNothing(t *testing.T) {
	f := newFixture(t)
	settings := enabled()
	settings.Enabled = false
	p := &jevtest.Provider{Results: []jevtest.Result{answer(0.9)}}
	j := judge(&settings, p, "key")
	d := j.Evaluate(context.Background(), f.repo, judgment("Done."))
	if d.Outcome != jev.Fallback || d.Reason != jev.ReasonDisabled || d.ID != "" || len(p.Requests()) != 0 {
		t.Fatalf("decision = %+v, requests = %d", d, len(p.Requests()))
	}
	docs, _ := trace.Read[trace.Document](f.repo, streamID)
	for _, doc := range docs {
		if strings.HasPrefix(doc.Path, "judgments/") {
			t.Fatalf("disabled boost recorded %s", doc.Path)
		}
	}
	if len(f.costs(t)) != 0 {
		t.Fatal("disabled boost recorded a cost")
	}
	if s := j.Status(); s.Mode != jev.ModeDisabled {
		t.Fatalf("status = %+v", s)
	}
}

func TestAcceptedJudgmentIsRecordedWithItsCostAndReusedAfterRestart(t *testing.T) {
	f := newFixture(t)
	settings := enabled()
	p := &jevtest.Provider{Results: []jevtest.Result{answer(0.9)}}
	j := judge(&settings, p, "key")
	d := j.Evaluate(context.Background(), f.repo, judgment("Done."))
	if d.Outcome != jev.Accepted || d.Recovered || d.Response == nil || d.Response.Answers["class"].Choice != "claims_done" || d.Response.Model != "jev-1.13.0" {
		t.Fatalf("decision = %+v", d)
	}
	records := f.records(t, d.ID)
	if len(records) != 2 || records[0].State != "started" || records[1].State != "accepted" {
		t.Fatalf("records = %+v", records)
	}
	r := records[1]
	if r.Task != "mason-classification" || r.Version != 1 || r.Model != config.DefaultJevModel || r.ResolvedModel != "jev-1.13.0" || r.Attempt != 1 || r.Requests != 1 || r.Usage == nil || r.Usage.InputTokens != 120 || len(r.Sources) != 1 || r.Digest == "" || !strings.Contains(string(r.Request), `"state":"Done."`) {
		t.Fatalf("record = %+v", r)
	}
	costs := f.costs(t)
	if len(costs) != 1 || costs[0].Entry.Scope.Role != jev.Role || costs[0].Entry.Scope.Unit != "unit1" || costs[0].Entry.Usage.CostUSD != 0.000005 || !costs[0].Entry.Usage.CostKnown {
		t.Fatalf("costs = %+v", costs)
	}

	f.reopen(t)
	again := judge(&settings, &jevtest.Provider{}, "key")
	recovered := again.Evaluate(context.Background(), f.repo, judgment("Done."))
	if !recovered.Recovered || recovered.Outcome != jev.Accepted || recovered.ID != d.ID || !reflect.DeepEqual(recovered.Response.Answers, d.Response.Answers) {
		t.Fatalf("recovered = %+v", recovered)
	}
	if len(f.records(t, d.ID)) != 2 || len(f.costs(t)) != 1 {
		t.Fatal("reuse recorded again")
	}
}

func TestChangedInputsAreANewJudgment(t *testing.T) {
	f := newFixture(t)
	settings := enabled()
	p := &jevtest.Provider{Results: []jevtest.Result{answer(0.9)}}
	j := judge(&settings, p, "key")
	first := j.Evaluate(context.Background(), f.repo, judgment("Done."))
	second := j.Evaluate(context.Background(), f.repo, judgment("Done, and I also refactored the parser."))
	newer := judgment("Done.")
	newer.Version = 2
	third := j.Evaluate(context.Background(), f.repo, newer)
	settings.Model = "jev-1.13.0"
	fourth := j.Evaluate(context.Background(), f.repo, judgment("Done."))
	ids := map[string]bool{first.ID: true, second.ID: true, third.ID: true, fourth.ID: true}
	if len(ids) != 4 || len(p.Requests()) != 4 || second.Recovered || third.Recovered || fourth.Recovered {
		t.Fatalf("ids = %v, requests = %d", ids, len(p.Requests()))
	}
}

func TestLowConfidenceFallsBackKeepingTheAnswers(t *testing.T) {
	f := newFixture(t)
	settings := enabled()
	j := judge(&settings, &jevtest.Provider{Results: []jevtest.Result{answer(0.5)}}, "key")
	d := j.Evaluate(context.Background(), f.repo, judgment("Maybe done?"))
	if d.Outcome != jev.Fallback || d.Reason != jev.ReasonDeclined || d.Detail != "class confidence below 0.8" || d.Response == nil {
		t.Fatalf("decision = %+v", d)
	}
	if len(f.costs(t)) != 1 {
		t.Fatal("a declined answer still costs")
	}
	f.reopen(t)
	again := judge(&settings, &jevtest.Provider{Results: []jevtest.Result{answer(0.99)}}, "key")
	if r := again.Evaluate(context.Background(), f.repo, judgment("Maybe done?")); !r.Recovered || r.Outcome != jev.Fallback || r.Reason != jev.ReasonDeclined {
		t.Fatalf("a recorded fallback was reinterpreted: %+v", r)
	}
}

func TestFailuresFallBackWithBoundedRetries(t *testing.T) {
	for _, c := range []struct {
		name     string
		err      error
		want     jev.Reason
		requests int
	}{
		{"timeout retried once", &systemone.Error{Kind: systemone.KindTimeout}, jev.ReasonTimeout, 2},
		{"unavailable retried once", &systemone.Error{Kind: systemone.KindUnavailable}, jev.ReasonUnavailable, 2},
		{"other errors are unavailable", errors.New("connection reset"), jev.ReasonUnavailable, 2},
		{"unprocessable not retried", &systemone.Error{Kind: systemone.KindRejected, Message: "HTTP 422"}, jev.ReasonRejected, 1},
		{"unauthorized not retried", &systemone.Error{Kind: systemone.KindUnauthorized}, jev.ReasonRejected, 1},
		{"malformed not retried", &systemone.Error{Kind: systemone.KindMalformed}, jev.ReasonUnusable, 1},
		{"rate limit not retried", &systemone.Error{Kind: systemone.KindRateLimited}, jev.ReasonRateLimited, 1},
	} {
		f := newFixture(t)
		settings := enabled()
		p := &jevtest.Provider{Results: []jevtest.Result{{Err: c.err}}}
		d := judge(&settings, p, "key").Evaluate(context.Background(), f.repo, judgment("Done."))
		want := c.want
		if d.Outcome != jev.Fallback || d.Reason != want || len(p.Requests()) != c.requests {
			t.Errorf("%s: decision = %+v, requests = %d", c.name, d, len(p.Requests()))
			continue
		}
		records := f.records(t, d.ID)
		if last := records[len(records)-1]; last.State != "fallback" || last.Reason != want || last.Requests != c.requests {
			t.Errorf("%s: record = %+v", c.name, last)
		}
		if len(f.costs(t)) != 0 {
			t.Errorf("%s: a failed request recorded a cost", c.name)
		}
	}
}

func TestATransientFailureThenSuccessIsAccepted(t *testing.T) {
	f := newFixture(t)
	settings := enabled()
	p := &jevtest.Provider{Results: []jevtest.Result{{Err: &systemone.Error{Kind: systemone.KindTimeout}}, answer(0.9)}}
	d := judge(&settings, p, "key").Evaluate(context.Background(), f.repo, judgment("Done."))
	if d.Outcome != jev.Accepted || len(p.Requests()) != 2 {
		t.Fatalf("decision = %+v", d)
	}
}

func TestARateLimitCoolsEveryJudgmentDown(t *testing.T) {
	f := newFixture(t)
	settings := enabled()
	p := &jevtest.Provider{Results: []jevtest.Result{{Err: &systemone.Error{Kind: systemone.KindRateLimited, RetryAfter: time.Minute}}}}
	j := judge(&settings, p, "key")
	j.Evaluate(context.Background(), f.repo, judgment("first"))
	d := j.Evaluate(context.Background(), f.repo, judgment("second"))
	if d.Reason != jev.ReasonCoolingDown || len(p.Requests()) != 1 {
		t.Fatalf("decision = %+v, requests = %d", d, len(p.Requests()))
	}
	s := j.Status()
	if s.Mode != jev.ModeDegraded || s.Reason != jev.ReasonRateLimited || s.CoolingUntil == nil || !s.CoolingUntil.Equal(at.Add(time.Minute)) {
		t.Fatalf("status = %+v", s)
	}
	settings.URL = "https://typesafe.example"
	if s := j.Status(); s.Mode != jev.ModeReady {
		t.Fatalf("changed settings kept the cool-down: %+v", s)
	}
}

func TestMissingKeyFallsBackVisibly(t *testing.T) {
	f := newFixture(t)
	settings := enabled()
	p := &jevtest.Provider{Results: []jevtest.Result{answer(0.9)}}
	j := judge(&settings, p, "")
	d := j.Evaluate(context.Background(), f.repo, judgment("Done."))
	if d.Reason != jev.ReasonUnconfigured || !strings.Contains(d.Detail, "TEST_JEV_KEY") || len(p.Requests()) != 0 {
		t.Fatalf("decision = %+v", d)
	}
	if s := j.Status(); s.Mode != jev.ModeUnconfigured {
		t.Fatalf("status = %+v", s)
	}
}

func TestInterruptedAttemptsAreDistinguishedAndBounded(t *testing.T) {
	f := newFixture(t)
	settings := enabled()
	blocking := &jevtest.Provider{Wait: jevtest.BlockUntilDone}
	interrupt := func() jev.Decision {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			for len(blocking.Requests()) == 0 {
				time.Sleep(time.Millisecond)
			}
			cancel()
		}()
		d := judge(&settings, blocking, "key").Evaluate(ctx, f.repo, judgment("Done."))
		blocking = &jevtest.Provider{Wait: jevtest.BlockUntilDone}
		return d
	}
	d := interrupt()
	if d.Reason != jev.ReasonCancelled {
		t.Fatalf("decision = %+v", d)
	}
	if records := f.records(t, d.ID); len(records) != 1 || records[0].State != "started" || records[0].Attempt != 1 {
		t.Fatalf("an interrupted attempt must remain only started: %+v", records)
	}

	f.reopen(t)
	interrupt()
	if records := f.records(t, d.ID); len(records) != 2 || records[1].State != "started" || records[1].Attempt != 2 {
		t.Fatalf("second attempt records = %+v", records)
	}

	f.reopen(t)
	p := &jevtest.Provider{Results: []jevtest.Result{answer(0.9)}}
	final := judge(&settings, p, "key").Evaluate(context.Background(), f.repo, judgment("Done."))
	if final.Outcome != jev.Fallback || final.Reason != jev.ReasonInterrupted || len(p.Requests()) != 0 {
		t.Fatalf("final = %+v, requests = %d", final, len(p.Requests()))
	}
	if records := f.records(t, d.ID); len(records) != 3 || records[2].State != "fallback" {
		t.Fatalf("records = %+v", records)
	}
}

func TestAnInterruptedAttemptIsRetriedOnce(t *testing.T) {
	f := newFixture(t)
	settings := enabled()
	blocking := &jevtest.Provider{Wait: jevtest.BlockUntilDone}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for len(blocking.Requests()) == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	first := judge(&settings, blocking, "key").Evaluate(ctx, f.repo, judgment("Done."))
	f.reopen(t)
	d := judge(&settings, &jevtest.Provider{Results: []jevtest.Result{answer(0.9)}}, "key").Evaluate(context.Background(), f.repo, judgment("Done."))
	if d.ID != first.ID || d.Outcome != jev.Accepted || d.Recovered {
		t.Fatalf("decision = %+v", d)
	}
	if records := f.records(t, d.ID); len(records) != 3 || records[2].Attempt != 2 || records[2].State != "accepted" {
		t.Fatalf("records = %+v", records)
	}
}

func TestInvalidJudgmentsFallBackWithoutARequest(t *testing.T) {
	f := newFixture(t)
	settings := enabled()
	p := &jevtest.Provider{Results: []jevtest.Result{answer(0.9)}}
	j := judge(&settings, p, "key")
	for name, edit := range map[string]func(*jev.Judgment){
		"no turn":     func(x *jev.Judgment) { x.Scope.Turn = "" },
		"no task":     func(x *jev.Judgment) { x.Task = "" },
		"no version":  func(x *jev.Judgment) { x.Version = 0 },
		"no cause":    func(x *jev.Judgment) { x.Cause = "" },
		"no question": func(x *jev.Judgment) { x.Request.Questions = nil },
	} {
		x := judgment("Done.")
		edit(&x)
		if d := j.Evaluate(context.Background(), f.repo, x); d.Reason != jev.ReasonRejected {
			t.Errorf("%s: decision = %+v", name, d)
		}
	}
	if len(p.Requests()) != 0 {
		t.Fatal("an invalid judgment was sent")
	}
}
