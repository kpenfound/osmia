// Package jevfixture is test support for fixtures whose chief of staff turns
// call the outcome tools of subjects Jev decides: it records the decide result
// those tools require.
package jevfixture

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/trace"
)

func settings() config.Jev {
	return config.Jev{URL: config.DefaultJevURL, Model: "jev-test", APIKeyEnv: "OSMIA_TEST_JEV_KEY", Timeout: "1s", CheckThreshold: config.DefaultJevCheckThreshold, DecisionThreshold: config.DefaultJevDecisionThreshold}
}

// Unconfigured is Jev without its API key: every judgment falls back.
func Unconfigured(now func() time.Time) *jev.Judge {
	s := settings()
	return &jev.Judge{Config: func() config.Jev { return s }, Getenv: func(string) string { return "" }, Now: now}
}

// Answering is Jev with a key whose requests the provider answers.
func Answering(now func() time.Time, p *jevtest.Provider) *jev.Judge {
	s := settings()
	return &jev.Judge{Config: func() config.Jev { return s }, Provider: p.Factory(), Getenv: func(string) string { return "key" }, Now: now}
}

// Fallback records the decide result of the occurrence as a fallback, which
// lets the outcome tool of its subject record any outcome valid today. It is
// what the decide tool leaves while Jev is unconfigured.
func Fallback(repo *trace.Repository, project config.ProjectID, stream config.WorkstreamID, kind jev.DecisionKind, key string) jev.Result {
	return Unconfigured(time.Now).Decide(context.Background(), repo, jev.DecisionRequest{
		Scope: coreadapter.Scope{Project: string(project), Workstream: string(stream)},
		Kind:  kind, Occurrence: key, State: "fixture", Options: []jev.DecisionOption{{Name: "a", Description: "a"}, {Name: "b", Description: "b"}}})
}

// Pick is the provider result that answers a decision request by picking the
// option with the confidence.
func Pick(option string, confidence float64, others ...string) jevtest.Result {
	return jevtest.Result{Response: systemone.Response{
		Model:   "jev-test-1",
		Answers: map[string]systemone.Answer{"decision": jevtest.Choice(option, confidence, confidence, others...)},
		Usage:   systemone.Usage{InputTokens: 100, OutputTokens: 5, CostUSD: 0.000004, CostKnown: true},
	}}
}

// Decide asks the judge for the occurrence's decision with the options, as the
// decide tool does, and returns the result.
func Decide(j *jev.Judge, repo *trace.Repository, project config.ProjectID, stream config.WorkstreamID, kind jev.DecisionKind, key string, options ...string) jev.Result {
	req := jev.DecisionRequest{Scope: coreadapter.Scope{Project: string(project), Workstream: string(stream)}, Kind: kind, Occurrence: key, State: "fixture"}
	for _, o := range options {
		req.Options = append(req.Options, jev.DecisionOption{Name: o, Description: o})
	}
	return j.Decide(context.Background(), repo, req)
}

// DecideQuestion leaves the decide result of the open question id of the chief of
// staff's workstream host (or of the member a qualified id names) as a
// fallback, unless a result is recorded. It reports the error that kept it
// from doing so.
func DecideQuestion(repo *trace.Repository, host config.WorkstreamID, id string) error {
	stream := host
	if ws, local, ok := strings.Cut(id, ":"); ok {
		stream, id = config.WorkstreamID(ws), local
	}
	target, err := repo.ProjectFor(stream)
	if err != nil {
		return err
	}
	asked, err := target.Questions(stream)
	if err != nil || !slices.ContainsFunc(asked, func(q trace.QuestionState) bool { return q.Asked.ID == id && q.State == trace.QuestionOpen }) {
		return err
	}
	key := jev.QuestionKey(id)
	if res, err := jev.Recorded(target, stream, jev.KindQuestionRouting, key); err != nil || res.Status != jev.None {
		return err
	}
	Fallback(target, target.Project(), stream, jev.KindQuestionRouting, key)
	return nil
}

// AutoDecide wraps the chief of staff's question tools so that a call first
// leaves a fallback decide result for each question it names, as a chief of
// staff turn does while Jev is unconfigured. Fixtures whose scripted turns call
// the outcome tools directly use it; tests of the decide requirement itself do
// not.
func AutoDecide(repo *trace.Repository, host config.WorkstreamID, list []coreadapter.Tool) []coreadapter.Tool {
	out := make([]coreadapter.Tool, len(list))
	for i, tool := range list {
		out[i] = tool
		switch tool.Name {
		case "answer", "escalate", "route_amendment":
			handle := tool.Handle
			out[i].Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
				var in struct {
					Question  string   `json:"question"`
					Questions []string `json:"questions"`
				}
				_ = json.Unmarshal(raw, &in)
				for _, id := range append(in.Questions, in.Question) {
					if id != "" {
						if err := DecideQuestion(repo, host, id); err != nil {
							return nil, err
						}
					}
				}
				return handle(ctx, raw)
			}
		}
	}
	return out
}
