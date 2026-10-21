package fixturekit

import (
	"context"
	"errors"
	"sync"

	"github.com/kpenfound/osmia/internal/issues"
)

// FakeIssues is a fake remote issue tracker: it serves issue text from
// memory and counts fetches, standing in for the real network call and the
// GitHub issues API's own authentication, rate limiting and formatting,
// which this leaves unverified. Its token stands in for a credential that
// Must never leave the service.
type FakeIssues struct {
	Mu      sync.Mutex
	Token   string
	Text    map[issues.Ref]string
	Fetches int
	// block, when set, holds the first fetch until it is closed; started
	// is signalled when that fetch begins.
	Block, Started chan struct{}
}

func (f *FakeIssues) Fetch(_ context.Context, ref issues.Ref) (string, error) {
	f.Mu.Lock()
	block, started := f.Block, f.Started
	f.Block = nil
	f.Mu.Unlock()
	if block != nil {
		close(started)
		<-block
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Fetches++
	text, ok := f.Text[ref]
	if !ok {
		return "", errors.New("not found with token " + f.Token)
	}
	return text, nil
}
