package thread

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestParseClassification(t *testing.T) {
	for _, bad := range []string{`{}`, `{"class":"claims_done"}`, `{"class":"other","evidence":"x"}`, `{"class":"unclear","evidence":""}`, `{"class":"unclear","evidence":"x","extra":1}`, `{"class":"unclear","class":"claims_done","evidence":"x"}`, `{"class":"unclear","evidence":"x"} {}`} {
		if _, ok := parseClassification(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
	if got, ok := parseClassification(`{"class":"claims_done","evidence":"completion stated"}`); !ok || got.Class != "claims_done" {
		t.Fatalf("valid: %+v %t", got, ok)
	}
}

func TestClassifierFallbackAndBound(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outputs  []string
		fail     bool
		want     string
		attempts int
	}{
		{"valid", []string{`{"class":"claims_done","evidence":"work finished"}`}, false, "claims_done", 1},
		{"invalid then valid", []string{`not json`, `{"class":"gave_up","evidence":"cannot proceed"}`}, false, "gave_up", 2},
		{"invalid", []string{`not json`, `not json`}, false, "unclear", 2},
		{"failed", nil, true, "unclear", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := coreadapter.Profile{Backend: "fake", Timeout: time.Minute}
			calls := 0
			r := Runner{ClassifierProfile: &profile, Classifier: func(_ context.Context, input coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
				if input.Scope.Role != "classifier" || input.Profile.Timeout != 30*time.Second {
					t.Fatalf("classifier boundary: %+v", input)
				}
				calls++
				out := coreadapter.SessionResult{StartedAt: time.Now(), Usage: coreadapter.Usage{CostKnown: true, CostUSD: .01, Turns: 1}}
				if tc.fail {
					return out, errors.New("provider unavailable")
				}
				out.FinalResponse = tc.outputs[calls-1]
				return out, nil
			}}
			response := trace.TurnResponse{Classification: &trace.TurnClassification{Class: "unclear", Evidence: "no phrase", Window: "edited files"}}
			r.classify(context.Background(), coreadapter.PreparedTurn{Scope: coreadapter.Scope{Turn: "turn"}, SessionDirectory: t.TempDir()}, &response)
			if calls != tc.attempts || response.Classification.Class != tc.want || len(response.ClassifierUsage) != calls {
				t.Fatalf("calls %d response %+v", calls, response)
			}
		})
	}
}
