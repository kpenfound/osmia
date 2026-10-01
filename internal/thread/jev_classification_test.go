package thread

import (
	"context"
	"errors"
	"reflect"
	"slices"
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

func TestResponseSpans(t *testing.T) {
	got := responseSpans("Edited store.go. Should I keep the cache?\n\n  Tests pass!\nnext steps:  ")
	want := []string{"Edited store.go.", "Should I keep the cache?", "Tests pass!", "next steps:"}
	if !slices.Equal(got, want) {
		t.Fatalf("spans %q, want %q", got, want)
	}
	if got := responseSpans("v1.2 is ready"); !slices.Equal(got, []string{"v1.2 is ready"}) {
		t.Fatalf("a period inside a word split the sentence: %q", got)
	}
	long := responseSpans(strings.Repeat("é", maxSpanRunes+10))
	if len(long) != 2 || len([]rune(long[0])) != maxSpanRunes || len([]rune(long[1])) != 10 {
		t.Fatalf("long span split into %d", len(long))
	}
	var lines []string
	for i := range maxSpans + 5 {
		lines = append(lines, "line "+string(rune('a'+i%26)))
	}
	if many := responseSpans(strings.Join(lines, "\n")); len(many) != maxSpans || many[len(many)-1] != lines[len(lines)-1] {
		t.Fatalf("spans kept %d ending %q", len(many), many[len(many)-1])
	}
	if got := responseSpans(" \n "); len(got) != 0 {
		t.Fatalf("blank response spans %q", got)
	}
}

// classification answers a mason-classification request: class with
// probability p, and for each class but unclear the span or none its
// evidence question selects.
func classification(class string, p float64, evidence map[string]string) jevtest.Result {
	var others []string
	for _, o := range classOptions {
		if o.Name != class {
			others = append(others, o.Name)
		}
	}
	answers := map[string]systemone.Answer{"class": jevtest.Choice(class, p, p, others...)}
	for c := range evidenceQuestions {
		choice := evidence[c]
		if choice == "" {
			choice = noSpan
		}
		answers[evidenceQuestion(c)] = jevtest.Choice(choice, 0.8, 0.8, "span-x")
	}
	return jevtest.Result{Response: systemone.Response{Model: "jev-1.13.0", Answers: answers, Usage: systemone.Usage{InputTokens: 900, CostUSD: 0.00004, CostKnown: true}}}
}

func TestJevClassification(t *testing.T) {
	const final = "Edited store.go. I cannot continue without the schema. Should I wait for it?"
	for _, tc := range []struct {
		name       string
		enabled    bool
		result     jevtest.Result
		classifier bool
		class      string
		evidence   string
		by         string
		outcome    jev.Outcome
		reason     jev.Reason
	}{
		{name: "disabled", result: classification("claims_done", 0.99, map[string]string{"claims_done": "span-1"}), class: "gave_up", evidence: "I cannot continue", by: trace.ClassifiedByRule},
		{name: "disabled with classifier", classifier: true, class: "gave_up", evidence: "schema missing", by: trace.ClassifiedByClassifier},
		{name: "question", enabled: true, result: classification("asked_in_prose", 0.92, map[string]string{"asked_in_prose": "span-3"}), classifier: true, class: "asked_in_prose", evidence: "Should I wait for it?", by: trace.ClassifiedByJev, outcome: jev.Accepted},
		{name: "gave up", enabled: true, result: classification("gave_up", 0.95, map[string]string{"gave_up": "span-2"}), class: "gave_up", evidence: "I cannot continue without the schema.", by: trace.ClassifiedByJev, outcome: jev.Accepted},
		{name: "unclear", enabled: true, result: classification("unclear", 0.81, nil), class: "unclear", evidence: "Jev found no question, completion claim or surrender (probability 0.81)", by: trace.ClassifiedByJev, outcome: jev.Accepted},
		{name: "uncertain gave up", enabled: true, result: classification("gave_up", 0.85, map[string]string{"gave_up": "span-2"}), classifier: true, class: "gave_up", evidence: "schema missing", by: trace.ClassifiedByClassifier, outcome: jev.Fallback, reason: jev.ReasonDeclined},
		{name: "uncertain class", enabled: true, result: classification("claims_done", 0.6, map[string]string{"claims_done": "span-1"}), class: "gave_up", evidence: "I cannot continue", by: trace.ClassifiedByRule, outcome: jev.Fallback, reason: jev.ReasonDeclined},
		{name: "no passage", enabled: true, result: classification("claims_done", 0.97, nil), class: "gave_up", evidence: "I cannot continue", by: trace.ClassifiedByRule, outcome: jev.Fallback, reason: jev.ReasonDeclined},
		{name: "rate limited", enabled: true, result: jevtest.Result{Err: &systemone.Error{Kind: systemone.KindRateLimited, Message: "slow down"}}, classifier: true, class: "gave_up", evidence: "schema missing", by: trace.ClassifiedByClassifier, outcome: jev.Fallback, reason: jev.ReasonRateLimited},
		{name: "unusable", enabled: true, result: jevtest.Result{Err: &systemone.Error{Kind: systemone.KindMalformed, Message: "answer missing"}}, class: "gave_up", evidence: "I cannot continue", by: trace.ClassifiedByRule, outcome: jev.Fallback, reason: jev.ReasonUnusable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _, _ := setup(t)
			req := queue(t, repo, "turn")
			settings := config.Jev{Enabled: tc.enabled, URL: config.DefaultJevURL, Model: "jev-test", APIKeyEnv: "TEST_JEV_KEY", Timeout: "1s"}
			p := &jevtest.Provider{Results: []jevtest.Result{tc.result}}
			now := func() time.Time { return timestamp.Add(time.Second) }
			runner := Runner{Store: repo, Now: now,
				Turns: fakeTurns(func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
					return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "s"}, FinalResponse: final, ToolCounts: map[string]int{"file_write": 1}}, nil
				}),
				Jev: &jev.Judge{Config: func() config.Jev { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "key" }, Now: now},
			}
			classified := 0
			if tc.classifier {
				profile := coreadapter.Profile{Backend: "fake", Timeout: time.Minute}
				runner.ClassifierProfile = &profile
				runner.Classifier = func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
					classified++
					return coreadapter.SessionResult{StartedAt: timestamp, FinalResponse: `{"class":"gave_up","evidence":"schema missing"}`}, nil
				}
			}
			q, err := runner.RunNext(context.Background(), stream, "agent", coreadapter.PreparedTurn{SessionDirectory: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			c := q.Response.Classification
			if c.Class != tc.class || c.Evidence != tc.evidence || c.By != tc.by || c.Window != final || c.ToolCounts["file_write"] != 1 {
				t.Fatalf("classification %+v", c)
			}
			if want := tc.classifier && tc.by == trace.ClassifiedByClassifier; (classified > 0) != want {
				t.Fatalf("classifier ran %d times; Jev classified %t", classified, tc.by == trace.ClassifiedByJev)
			}
			th, err := repo.Thread(stream, "agent")
			if err != nil || !reflect.DeepEqual(th.Turns[0].Response.Classification, c) {
				t.Fatalf("recorded classification %+v %v", th.Turns[0].Response, err)
			}
			requests := p.Requests()
			if !tc.enabled {
				if len(requests) != 0 || c.Judgment != "" {
					t.Fatalf("boost off sent %d requests, judgment %q", len(requests), c.Judgment)
				}
				return
			}
			if len(requests) != 1 {
				t.Fatalf("requests %d", len(requests))
			}
			r := requests[0]
			state, _ := r.State.(map[string]any)
			if state["message"] != final || len(r.Questions) != 4 || len(r.Questions[evidenceQuestion("gave_up")].Options) != 4 {
				t.Fatalf("request %+v", r)
			}
			// The same judgment asked again, as after a restart, is the
			// recorded decision.
			decision := runner.Jev.Evaluate(context.Background(), repo, jev.Judgment{Scope: coreadapter.Scope{Project: string(project), Workstream: string(stream), Thread: "thread", Turn: "turn", Role: "mason"}, Cause: req.ID, Depth: req.Depth, Task: classificationTask, Version: classificationVersion, Request: r})
			if !decision.Recovered || decision.Outcome != tc.outcome || decision.Reason != tc.reason || len(p.Requests()) != 1 {
				t.Fatalf("recorded decision %+v", decision)
			}
			if (c.Judgment == decision.ID) != (tc.outcome == jev.Accepted) || tc.outcome != jev.Accepted && c.Judgment != "" {
				t.Fatalf("classification cites judgment %q, decision %s", c.Judgment, decision.ID)
			}
		})
	}
}

func TestJevClassificationAskedOnceAcrossRestart(t *testing.T) {
	repo, root, p := setup(t)
	req := queue(t, repo, "turn")
	settings := config.Jev{Enabled: true, URL: config.DefaultJevURL, Model: "jev-test", APIKeyEnv: "TEST_JEV_KEY", Timeout: "1s"}
	provider := &jevtest.Provider{Results: []jevtest.Result{classification("claims_done", 0.9, map[string]string{"claims_done": "span-1"})}}
	judge := &jev.Judge{Config: func() config.Jev { return settings }, Provider: provider.Factory(), Getenv: func(string) string { return "key" }, Now: func() time.Time { return timestamp }}
	scope := coreadapter.Scope{Project: string(project), Workstream: string(stream), Thread: "thread", Turn: "turn", Role: "mason"}
	classify := func(repo *trace.Repository) trace.TurnClassification {
		response := trace.TurnResponse{Classification: &trace.TurnClassification{Class: "unclear", Evidence: "no phrase", Window: "All work is complete. Nothing else changed.", By: trace.ClassifiedByRule}}
		if !(Runner{Store: repo, Jev: judge}).judgeClassification(context.Background(), req, scope, &response) {
			t.Fatal("judgment fell back")
		}
		return *response.Classification
	}
	first := classify(repo)
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := trace.Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if second := classify(reopened); !reflect.DeepEqual(second, first) || first.Class != "claims_done" || first.Evidence != "All work is complete." || len(provider.Requests()) != 1 {
		t.Fatalf("classifications %+v then %+v after %d requests", first, second, len(provider.Requests()))
	}
}

func TestJevClassificationSkipsBlankResponses(t *testing.T) {
	repo, _, _ := setup(t)
	provider := &jevtest.Provider{Results: []jevtest.Result{{Err: errors.New("asked")}}}
	settings := config.Jev{Enabled: true, URL: config.DefaultJevURL, Model: "jev-test", APIKeyEnv: "TEST_JEV_KEY", Timeout: "1s"}
	r := Runner{Store: repo, Jev: &jev.Judge{Config: func() config.Jev { return settings }, Provider: provider.Factory(), Getenv: func(string) string { return "key" }}}
	response := trace.TurnResponse{Classification: &trace.TurnClassification{Class: "unclear", Evidence: "no phrase", Window: "  \n", By: trace.ClassifiedByRule}}
	if r.judgeClassification(context.Background(), trace.TurnRequest{}, coreadapter.Scope{}, &response) || len(provider.Requests()) != 0 {
		t.Fatalf("blank response judged: %+v", response.Classification)
	}
}
