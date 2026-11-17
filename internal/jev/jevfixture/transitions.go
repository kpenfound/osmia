package jevfixture

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	corejev "github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
)

// Ask is a decide-transition request as a fixture reads it: the trigger and the
// default the router named in the summary, the options of the first pick
// question in the order they were asked, and the names of the trigger's own
// questions. Call numbers the requests the fixture has received, from 1.
type Ask struct {
	Trigger string
	Default string
	State   string
	Options []string
	Own     []string
	Call    int
}

// Reply is a fixture's answers to a decide-transition request. PickA and PickB
// default to the default when empty; ConfA and ConfB are the picks'
// confidences. Own holds the trigger's own propositions' probabilities, and a
// question it does not name is answered 0.5.
type Reply struct {
	Fit, Stuck, Intent float64
	PickA, PickB       string
	ConfA, ConfB       float64
	Own                map[string]float64
}

// DefaultFits is the reply that keeps the default: the default fits, the
// subject is not stuck, nothing would change intent, and both orderings pick
// the default with high confidence.
func DefaultFits(a Ask) Reply {
	return Reply{Fit: 0.95, Stuck: 0.05, Intent: 0.05, PickA: a.Default, PickB: a.Default, ConfA: 0.95, ConfB: 0.95}
}

// Departs is the reply that departs from the default to target at confidence p:
// the default does not fit, both orderings pick target at p, and every own
// question of the trigger is answered corroborate.
func Departs(a Ask, target string, p, corroborate float64) Reply {
	own := map[string]float64{}
	for _, name := range a.Own {
		own[name] = corroborate
	}
	return Reply{Fit: 0.1, Stuck: 0.5, Intent: 0.05, PickA: target, PickB: target, ConfA: p, ConfB: p, Own: own}
}

// Transitions is the fixture Jev provider of decide-transition requests. Its
// zero value answers every request that the default fits. Script replaces the
// answers per request, and Fail makes a request fail. A request that is not a
// transition request is answered by Decisions when it is set, and otherwise
// has an answer with nothing in it, which the judge treats as unusable.
type Transitions struct {
	// Script returns the reply to a request; nil answers DefaultFits.
	Script func(Ask) Reply
	// Fail returns the error a request fails with, or nil to answer it. It is
	// given the number of the request, from 1.
	Fail func(call int) error
	// Wait runs before each answer and may block until the request's context
	// ends, as a slow provider does.
	Wait func(context.Context) error
	// Decisions answers the requests that ask for a decision.
	Decisions *Provider
	// Free makes the answers cost nothing, for a fixture whose spend a test
	// reads and Jev's own requests must not change.
	Free bool

	mu       sync.Mutex
	requests []systemone.Request
}

// Evaluate answers one request.
func (p *Transitions) Evaluate(ctx context.Context, r systemone.Request) (systemone.Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, r)
	call := len(p.requests)
	p.mu.Unlock()
	if _, ok := r.Questions[jev.QuestionFit]; !ok {
		if p.Decisions != nil {
			return p.Decisions.Evaluate(ctx, r)
		}
		return systemone.Response{Model: "jev-test"}, nil
	}
	if p.Wait != nil {
		if err := p.Wait(ctx); err != nil {
			return systemone.Response{}, err
		}
	}
	if p.Fail != nil {
		if err := p.Fail(call); err != nil {
			return systemone.Response{}, err
		}
	}
	resp := answerTransition(r, call, p.Script)
	if p.Free {
		resp.Usage = systemone.Usage{}
	}
	return resp, nil
}

// answerTransition answers a transition request with the script's reply, or
// DefaultFits.
func answerTransition(r systemone.Request, call int, script func(Ask) Reply) systemone.Response {
	ask := Ask{Call: call}
	if raw, err := json.Marshal(r.State); err == nil {
		var s struct {
			Trigger string `json:"trigger"`
			Default string `json:"default"`
		}
		_ = json.Unmarshal(raw, &s)
		ask.Trigger, ask.Default, ask.State = s.Trigger, s.Default, string(raw)
	}
	for _, o := range r.Questions[jev.QuestionPickA].Options {
		ask.Options = append(ask.Options, o.Name)
	}
	for id := range r.Questions {
		if name, ok := strings.CutPrefix(id, jev.OwnQuestionPrefix); ok {
			ask.Own = append(ask.Own, name)
		}
	}
	sort.Strings(ask.Own)
	if script == nil {
		script = DefaultFits
	}
	reply := script(ask)
	if reply.PickA == "" {
		reply.PickA = ask.Default
	}
	if reply.PickB == "" {
		reply.PickB = reply.PickA
	}
	answers := map[string]systemone.Answer{
		jev.QuestionFit: jevtest.Noul(reply.Fit), jev.QuestionStuck: jevtest.Noul(reply.Stuck), jev.QuestionIntent: jevtest.Noul(reply.Intent),
		jev.QuestionPickA: choice(r.Questions[jev.QuestionPickA], reply.PickA, reply.ConfA),
		jev.QuestionPickB: choice(r.Questions[jev.QuestionPickB], reply.PickB, reply.ConfB),
	}
	for _, name := range ask.Own {
		prob, ok := reply.Own[name]
		if !ok {
			prob = 0.5
		}
		answers[jev.OwnQuestionPrefix+name] = jevtest.Noul(prob)
	}
	return systemone.Response{Model: "jev-test", Answers: answers, Usage: systemone.Usage{InputTokens: 100, OutputTokens: 5, CostUSD: 0.000004, CostKnown: true}}
}

// choice answers a pick question with the option at the confidence. An option the
// question does not offer is answered as given, so a test can script an
// answer naming a target the router never offered.
func choice(q systemone.Question, option string, confidence float64) systemone.Answer {
	var others []string
	for _, o := range q.Options {
		if o.Name != option {
			others = append(others, o.Name)
		}
	}
	return jevtest.Choice(option, confidence, confidence, others...)
}

// Factory is the service's Options.JevProvider.
func (p *Transitions) Factory() func(corejev.Settings, string) corejev.Provider {
	return func(corejev.Settings, string) corejev.Provider { return p }
}

// Requests returns every request received.
func (p *Transitions) Requests() []systemone.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]systemone.Request(nil), p.requests...)
}

// TransitionRequests returns the requests that ask the narrow questions of a
// transition.
func (p *Transitions) TransitionRequests() []systemone.Request {
	var out []systemone.Request
	for _, r := range p.Requests() {
		if _, ok := r.Questions[jev.QuestionFit]; ok {
			out = append(out, r)
		}
	}
	return out
}

// TransitionJudge is Jev with a key whose requests the provider answers, for a
// test of the transition router.
func TransitionJudge(now func() time.Time, p corejev.Provider) *jev.Judge {
	s := settings()
	return &jev.Judge{Config: func() config.Jev { return s }, Provider: func(corejev.Settings, string) corejev.Provider { return p }, Getenv: func(string) string { return "key" }, Now: now}
}
