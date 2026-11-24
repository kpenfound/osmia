package journey

import (
	context "context"
	fmt "fmt"
	sync "sync"
	testing "testing"
	time "time"

	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type QuestionFixture struct {
	Opts     Options
	Cfg      *config.Config
	Clock    *fixturekit.FixedClock
	Engine   *fixturekit.DemoEngine
	Lives    chan *trace.Repository
	Ticks    chan time.Time
	Mu       sync.Mutex
	Results  map[string][]string
	Problems []string
}

func NewQuestionFixture(t *testing.T, prefix, global, charter string, askers []intakefixture.QuestionAsker) *QuestionFixture {
	t.Helper()
	f := &QuestionFixture{Results: map[string][]string{}}
	var inputs []intakefixture.QuestionAsker
	for _, a := range askers {
		inputs = append(inputs, intakefixture.QuestionAsker{Stream: a.Stream, Agent: a.Agent, Thread: a.Thread, Role: a.Role, Turn: a.Turn})
	}
	data := intakefixture.NewQuestionSetup(t, prefix, global, charter, inputs, func(t *testing.T, home string) config.Options {
		f.Opts = FixtureAt(t, home)
		return f.Opts.Config
	})
	f.Cfg, f.Clock, f.Engine, f.Lives, f.Ticks = data.Config, data.Clock, data.Engine, data.Lives, data.Ticks
	f.Opts.Threads, f.Opts.Reconciliation.Now, f.Opts.Reconciliation.Ticks = data.Threads, data.Clock.Now, data.Ticks
	return f
}

func (f *QuestionFixture) Settle(t *testing.T, s *Service) {
	t.Helper()
	intakefixture.SettleTicks(t, f.Ticks, f.Clock.Now, s.Close)
}

func (f *QuestionFixture) Problem(format string, args ...any) {
	f.Problems = append(f.Problems, fmt.Sprintf(format, args...))
}

func (f *QuestionFixture) Use(ctx context.Context, tools *mcp.ClientSession, step, name string, args map[string]any) error {
	out, err := fixturekit.CallTool(ctx, tools, name, args)
	if err != nil {
		return err
	}
	f.Results[step] = append(f.Results[step], out)
	return nil
}
