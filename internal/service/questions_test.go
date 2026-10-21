package service

import (
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	config "github.com/kpenfound/osmia/internal/config"
	trace "github.com/kpenfound/osmia/internal/trace"

	testing "testing"
	time "time"
)

var toolNames = intakefixture.ToolNames

// questionAsker is one agent of a question fixture with its first queued turn.
type questionAsker struct {
	stream                    config.WorkstreamID
	agent, thread, role, turn string
}

// questionFixture is a project whose askers and chiefs of staff are fake
// agents holding the real question tools over MCP. The clock stands still, so
// the event window closes only when a test advances it. Fake turns lock mu
// around results and problems.
type questionFixture struct {
	opts   Options
	cfg    *config.Config
	clock  *fixedClock
	engine *demoEngine
	lives  chan *trace.Repository
	ticks  chan time.Time

	results map[string][]string
}

// newQuestionFixture creates the trace with the charter, each asker's
// workstream and thread, and one queued turn per asker. global is appended to
// the root's config.toml.
func newQuestionFixture(t *testing.T, prefix, global, charter string, askers []questionAsker) *questionFixture {
	t.Helper()
	f := &questionFixture{results: map[string][]string{}}
	var inputs []intakefixture.QuestionAsker
	for _, a := range askers {
		inputs = append(inputs, intakefixture.QuestionAsker{Stream: a.stream, Agent: a.agent, Thread: a.thread, Role: a.role, Turn: a.turn})
	}
	data := intakefixture.NewQuestionSetup(t, prefix, global, charter, inputs, func(t *testing.T, home string) config.Options { f.opts = fixtureAt(t, home); return f.opts.Config })
	f.cfg, f.clock, f.engine, f.lives, f.ticks = data.Config, data.Clock, data.Engine, data.Lives, data.Ticks
	f.opts.Threads, f.opts.Reconciliation.Now, f.opts.Reconciliation.Ticks = data.Threads, data.Clock.Now, data.Ticks
	return f
}

// settle sends several ticks. A tick is received only between passes, so
// afterwards every pass an earlier one caused has finished.
func (f *questionFixture) settle(t *testing.T, s *Service) {
	t.Helper()
	intakefixture.SettleTicks(t, f.ticks, f.clock.Now, s.Close)
}

var questionResult = intakefixture.QuestionResult
