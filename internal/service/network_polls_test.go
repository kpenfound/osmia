package service

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

func TestNetworkObserversShareGitCapacityWithoutWorkflowOrdering(t *testing.T) {
	t.Parallel()
	_, repo, _, refresh, _ := stopFixture(t)
	s := refresh.s
	var releases []func()
	for i := 0; i < config.DefaultGitOperations; i++ {
		release, admitted := s.tryGitOperation(repo)
		if !admitted {
			t.Fatal("capacity unexpectedly unavailable")
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	entered := make(chan struct{}, 1)
	stage := &stages{polls: []scheduleHook{{"fixture", func(ctx context.Context) error {
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}}}
	p := &pipeline{}
	p.current.Store(stage)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.runNetworkPolls(ctx, repo, p); close(done) }()
	select {
	case <-entered:
		t.Fatal("poll exceeded Git capacity")
	default:
	}
	releases[0]()
	s.conversationSignal.wake()
	fixturekit.AwaitPollSignal(t, entered)
	if _, admitted := s.tryGitOperation(repo); admitted {
		t.Fatal("poll did not retain its Git permit")
	}
	cancel()
	fixturekit.AwaitPollSignal(t, done)
}
