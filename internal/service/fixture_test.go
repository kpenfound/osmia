package service

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const unreachableGitHub = "http://127.0.0.1:1"

func testOptions(cfg config.Options) Options {
	return Options{Config: cfg, Build: Identity{Version: "test", Commit: "abc"}, Workstreams: []config.WorkstreamID{fixturekit.Stream}, ShutdownTimeout: 100 * time.Millisecond,
		ErrorReporter: ErrorReporterOptions{BaseURL: unreachableGitHub}}
}

func awaitConversation(t *testing.T, c *Client, done func(ConversationResponse) bool) ConversationResponse {
	t.Helper()
	return intakefixture.AwaitView(t, func() (ConversationResponse, error) {
		return c.Conversation(context.Background(), fixturekit.Stream)
	}, done, "conversation did not settle: %+v")
}

func (f *twoProjectFixture) block(id config.ProjectID, name string) (entered, release chan struct{}) {
	return f.turns.Block(intakefixture.Turn(id, name))
}

func conversationFixture(t *testing.T, prefix string) (Options, *config.Config) {
	t.Helper()
	opts, cfg := fixturekit.Conversation(t, prefix)
	o := testOptions(opts)
	o.Reconciliation.Now = (&fixturekit.DemoClock{At: fixturekit.DemoStart}).Now
	return o, cfg
}

func newTwoProjectFixture(t *testing.T) *twoProjectFixture {
	t.Helper()
	data := fixturekit.NewTwoProjectScenario(t)
	opts := testOptions(data.Config)
	opts.Workstreams, opts.Threads, opts.Reconciliation.Now = nil, data.Threads, data.Clock.Now
	return &twoProjectFixture{opts: opts, cfg: data.Loaded, clock: data.Clock, streams: data.Streams, turns: data.Turns, scenario: data}
}

const (
	otherProject config.ProjectID    = "p_fedcba9876543210fedcba9876543210"
	otherStream  config.WorkstreamID = "w_fedcba9876543210fedcba9876543210"
)

func projectFixture(t *testing.T) (Options, string) {
	t.Helper()
	cfg, clone := fixturekit.ProjectConfig(t)
	o := testOptions(cfg)
	o.Workstreams = nil
	o.reviewChecks = fixturekit.PassingChecks
	return o, clone
}

func (f *twoProjectFixture) repository(id config.ProjectID) *trace.Repository {
	return f.scenario.Repository(id)
}

type twoProjectFixture struct {
	opts     Options
	cfg      *config.Config
	clock    *fixturekit.DemoClock
	turns    *fixturekit.BlockingTurns
	streams  map[config.ProjectID]config.WorkstreamID
	scenario *fixturekit.TwoProjectScenario
}

const reviewSummary = "The candidate does the task and its acceptance holds"
