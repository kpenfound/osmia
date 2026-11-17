package jevfixture

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	corejev "github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/jev"
)

// Provider is the fake Jev provider of a running service. It answers each
// decision request with the option Pick chooses, at Confidence, and each
// decide-transition request with the default fits unless Transition scripts
// it, and records the requests it received. A request that is neither gets an
// answer with nothing in it, which the judge treats as unusable.
type Provider struct {
	Confidence float64
	// Pick chooses among the option names; it defaults to the first.
	Pick func(options []string) string
	// Transition scripts the answers to a decide-transition request; nil
	// answers every one that the default fits.
	Transition func(Ask) Reply
	// Fail returns the error a request fails with, or nil to answer it. It is
	// given the number of the request, from 1.
	Fail func(call int) error

	mu       sync.Mutex
	requests []systemone.Request
}

// Evaluate answers one request.
func (p *Provider) Evaluate(_ context.Context, r systemone.Request) (systemone.Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, r)
	call := len(p.requests)
	p.mu.Unlock()
	if p.Fail != nil {
		if err := p.Fail(call); err != nil {
			return systemone.Response{}, err
		}
	}
	if _, ok := r.Questions[jev.QuestionFit]; ok {
		return answerTransition(r, call, p.Transition), nil
	}
	question, ok := r.Questions["decision"]
	if !ok || len(question.Options) == 0 {
		return systemone.Response{Model: "jev-test"}, nil
	}
	var options []string
	for _, o := range question.Options {
		options = append(options, o.Name)
	}
	choice := options[0]
	if p.Pick != nil {
		choice = p.Pick(options)
	}
	var others []string
	for _, o := range options {
		if o != choice {
			others = append(others, o)
		}
	}
	return systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"decision": jevtest.Choice(choice, p.Confidence, p.Confidence, others...)},
		Usage: systemone.Usage{InputTokens: 100, OutputTokens: 5, CostUSD: 0.000004, CostKnown: true}}, nil
}

// Factory is the service's Options.JevProvider.
func (p *Provider) Factory() func(corejev.Settings, string) corejev.Provider {
	return func(corejev.Settings, string) corejev.Provider { return p }
}

// Requests returns every request received.
func (p *Provider) Requests() []systemone.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]systemone.Request(nil), p.requests...)
}

// Decisions returns the requests asking for a decision.
func (p *Provider) Decisions() []systemone.Request {
	var out []systemone.Request
	for _, r := range p.Requests() {
		if _, ok := r.Questions["decision"]; ok {
			out = append(out, r)
		}
	}
	return out
}

// Options returns the option names of a decision request.
func Options(r systemone.Request) []string {
	var names []string
	for _, o := range r.Questions["decision"].Options {
		names = append(names, o.Name)
	}
	return names
}

var keys atomic.Int64

// Key names an environment variable that holds Jev's API key, set when on. A
// test that leaves it unset runs its service with Jev unconfigured.
func Key(t *testing.T, on bool) string {
	t.Helper()
	name := fmt.Sprintf("OSMIA_JOURNEY_JEV_KEY_%d", keys.Add(1))
	t.Cleanup(func() { os.Unsetenv(name) })
	if on {
		os.Setenv(name, "journey-"+name)
	}
	return name
}

// Config is the [jev] table that reads its API key from the variable.
func Config(key string) string {
	return fmt.Sprintf("[jev]\napi_key_env = %q\ntimeout = \"20s\"\n", key)
}
