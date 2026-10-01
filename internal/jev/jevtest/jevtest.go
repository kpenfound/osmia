// Package jevtest supplies a fake Jev provider for tests. No test reaches a
// real model.
package jevtest

import (
	"context"
	"errors"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/systemone"
)

// Result is one scripted provider result: a response, or an error.
type Result struct {
	Response systemone.Response
	Err      error
}

// Provider returns its scripted results in order and repeats the last one.
// Wait, when set, runs before each result and may block until the request's
// context ends, returning its error.
type Provider struct {
	Results []Result
	Wait    func(context.Context) error

	mu       sync.Mutex
	requests []systemone.Request
}

func (p *Provider) Evaluate(ctx context.Context, r systemone.Request) (systemone.Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, r)
	n := len(p.requests)
	p.mu.Unlock()
	if p.Wait != nil {
		if err := p.Wait(ctx); err != nil {
			return systemone.Response{}, err
		}
	}
	if len(p.Results) == 0 {
		return systemone.Response{}, &systemone.Error{Kind: systemone.KindUnavailable, Message: "no scripted result"}
	}
	res := p.Results[min(n, len(p.Results))-1]
	if res.Err != nil {
		return systemone.Response{}, res.Err
	}
	return res.Response, nil
}

// Requests returns every request received.
func (p *Provider) Requests() []systemone.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]systemone.Request(nil), p.requests...)
}

// Factory returns a jev.Judge provider factory that always returns p.
func (p *Provider) Factory() func(config.Jev, string) jev.Provider {
	return func(config.Jev, string) jev.Provider { return p }
}

// BlockUntilDone waits for the request's context to end, as an interrupted
// request does.
func BlockUntilDone(ctx context.Context) error {
	<-ctx.Done()
	return errors.Join(ctx.Err(), &systemone.Error{Kind: systemone.KindCancelled, Message: "request cancelled"})
}

// Choice is a Choice answer that selects choice with probability p and
// spreads the rest evenly over others.
func Choice(choice string, p, confidence float64, others ...string) systemone.Answer {
	probabilities := map[string]float64{choice: p}
	for _, o := range others {
		probabilities[o] = (1 - p) / float64(len(others))
	}
	return systemone.Answer{Kind: systemone.KindChoice, Choice: choice, Probabilities: probabilities, Confidence: confidence}
}

// Noul is a Noul answer.
func Noul(p float64) systemone.Answer { return systemone.Answer{Kind: systemone.KindNoul, Noul: p} }
