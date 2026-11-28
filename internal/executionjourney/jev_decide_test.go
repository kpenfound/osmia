package executionjourney

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/cli"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/jev/jevfixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const contestEvidence = "The reviewer sent the candidate back three times with the same finding and the mason has not changed it."

func withJev(t *testing.T, p *jevfixture.Provider) func(*journey.Options) {
	t.Helper()
	key := jevfixture.Key(t, true)
	return func(o *journey.Options) {
		o.JevProvider = p.Factory()
		file, err := os.OpenFile(filepath.Join(o.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
		fixturekit.Must(t, err)
		_, err = file.WriteString(jevfixture.Config(key))
		fixturekit.Must(t, err)
		fixturekit.Must(t, file.Close())
	}
}

func (b *bounceFixture) managerTurn(match string, script func(ctx context.Context, tools *mcp.ClientSession) error) {
	b.f.Engine.Mu.Lock()
	defer b.f.Engine.Mu.Unlock()
	next := b.f.Engine.Turns["*"]
	var once atomic.Bool
	b.f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		if !strings.Contains(req.Prompt, match) || !once.CompareAndSwap(false, true) {
			return next(ctx, req, turn, tools)
		}
		err := script(ctx, tools)
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Recorded", SessionDir: req.SessionDir, NumTurns: 1}, err
	}
}

func rawTool(ctx context.Context, tools *mcp.ClientSession, name string, args map[string]any) (string, error) {
	result, err := tools.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, content := range result.Content {
		if c, ok := content.(*mcp.TextContent); ok {
			text.WriteString(c.Text)
		}
	}
	if result.IsError {
		return "", errors.New(text.String())
	}
	return text.String(), nil
}

func callDecideContest(ctx context.Context, tools *mcp.ClientSession) (string, error) {
	return rawTool(ctx, tools, "decide", map[string]any{"kind": "contested-ruling", "subject": bounceUnit, "evidence": contestEvidence})
}

func TestAConfidentDecidePickBindsTheManagersContestedRuling(t *testing.T) {
	t.Parallel()
	provider := &jevfixture.Provider{Confidence: 0.96, Pick: func(options []string) string {
		if slices.Contains(options, "review") {
			return "review"
		}
		return options[0]
	}}
	b := newBounceFixture(t, "reviews", withJev(t, provider))
	defer b.stop(t)
	ctx := context.Background()

	contest, contested, err := execution.UnitContest(b.f.Repository(), b.stream, bounceUnit)
	fixturekit.Must(t, err)
	if !contested {
		t.Fatal("the unit is not contested")
	}
	type outcome struct{ before, decided, against, picked string }
	done := make(chan outcome, 1)
	b.managerTurn("jev rules bounce now", func(ctx context.Context, tools *mcp.ClientSession) error {
		var o outcome
		var err error
		if o.before, err = rawTool(ctx, tools, execution.ResolveContestedTool, map[string]any{"unit": bounceUnit, "decision": "review", "note": bounceNote}); err != nil {
			return err
		}
		if o.decided, err = callDecideContest(ctx, tools); err != nil {
			return err
		}
		if o.against, err = rawTool(ctx, tools, execution.ResolveContestedTool, map[string]any{"unit": bounceUnit, "decision": "escalate", "note": "The owner should look."}); err != nil {
			return err
		}
		o.picked, err = rawTool(ctx, tools, execution.ResolveContestedTool, map[string]any{"unit": bounceUnit, "decision": "review", "note": bounceNote})
		done <- o
		return err
	})
	_, err = b.f.C.Send(ctx, b.stream, "jev rules bounce now")
	fixturekit.Must(t, err)
	got := intakefixture.AwaitCheck(t, done)
	if !strings.Contains(got.before, `"recorded":false`) || !strings.Contains(got.before, "decide") {
		t.Fatalf("the ruling before decide: %s", got.before)
	}
	if !strings.Contains(got.decided, `"decision":"binding"`) || !strings.Contains(got.decided, `"pick":"review"`) {
		t.Fatalf("decide: %s", got.decided)
	}
	if !strings.Contains(got.against, `"recorded":false`) || !strings.Contains(got.against, "Jev") {
		t.Fatalf("the ruling Jev did not pick: %s", got.against)
	}
	if !strings.Contains(got.picked, `"recorded":true`) {
		t.Fatalf("the picked ruling: %s", got.picked)
	}

	if req := intakefixture.AwaitCheck(t, b.delivered); !strings.HasPrefix(req.Name, intake.ReviewerAgent(bounceUnit)+"-review-") {
		t.Fatalf("the turn after the ruling: %s", req.Name)
	}

	requests := provider.Decisions()
	if len(requests) != 1 {
		t.Fatalf("%d decision requests", len(requests))
	}
	for _, option := range []string{"review", "revise", "escalate"} {
		if !slices.Contains(jevfixture.Options(requests[0]), option) {
			t.Fatalf("Jev was not offered %q: %v", option, jevfixture.Options(requests[0]))
		}
	}
	unit, err := b.f.C.TraceUnit(ctx, b.stream, bounceUnit)
	fixturekit.Must(t, err)
	var ruling *api.TraceRuling
	for i := range unit.Rulings {
		if unit.Rulings[i].Kind == "contested" {
			ruling = &unit.Rulings[i]
		}
	}
	if ruling == nil || ruling.Decision != "review" || ruling.DecidedBy != trace.DecidedByJev || ruling.Judgment == "" {
		t.Fatalf("trace rulings: %+v", unit.Rulings)
	}
	res, err := jev.Recorded(b.f.Repository(), b.stream, jev.KindContestedRuling, jev.ContestKey(contest.ID))
	fixturekit.Must(t, err)
	if res.Status != jev.Binding || res.Pick != "review" || res.Judgment != ruling.Judgment {
		t.Fatalf("recorded result %+v", res)
	}
	byJev := 0
	for _, tr := range intakefixture.AllTransitions(t, b.f.S.Cfg.Root, b.f.Project, b.stream) {
		if tr.Actor == trace.JevActor && tr.Subject == trace.UnitSubject(bounceUnit) {
			byJev++
		}
	}
	if byJev != 1 {
		t.Fatalf("%d transitions of the unit by Jev", byJev)
	}
	root, err := config.ResolveRoot(b.f.Opts.Config.Root, "")
	fixturekit.Must(t, err)
	var out, diag bytes.Buffer
	code := cli.Run(ctx, []string{"--root", root.String(), "trace", string(b.stream), "unit", bounceUnit}, strings.NewReader(""), &out, &diag)
	if code != 0 || diag.Len() != 0 || !strings.Contains(out.String(), "contested: review (decided by Jev)\n") {
		t.Fatalf("the CLI's trace: code=%d stderr=%s\n%s", code, diag.String(), out.String())
	}
}

func TestWithJevUnconfiguredTheManagerDecidesAndTheContestReachesTheOwner(t *testing.T) {
	t.Parallel()
	b := newBounceFixture(t, "reviews")
	defer b.stop(t)
	ctx := context.Background()

	done := make(chan [2]string, 1)
	const note = "The retry needs the owner's decision."
	b.managerTurn("escalate after decide", func(ctx context.Context, tools *mcp.ClientSession) error {
		decided, err := callDecideContest(ctx, tools)
		if err != nil {
			return err
		}
		escalated, err := rawTool(ctx, tools, execution.ResolveContestedTool, map[string]any{"unit": bounceUnit, "decision": "escalate", "note": note})
		done <- [2]string{decided, escalated}
		return err
	})
	_, err := b.f.C.Send(ctx, b.stream, "escalate after decide")
	fixturekit.Must(t, err)
	got := intakefixture.AwaitCheck(t, done)
	if !strings.Contains(got[0], `"decision":"manager"`) || strings.Contains(got[0], `"decision":"binding"`) {
		t.Fatalf("decide: %s", got[0])
	}
	if !strings.Contains(got[1], `"recorded":true`) {
		t.Fatalf("escalate: %s", got[1])
	}
	entries := raisedContests(b.f, t, b.stream)
	if len(entries) != 1 || entries[0].Unit != bounceUnit || !strings.Contains(entries[0].Recommendation, note) {
		t.Fatalf("the contested card: %+v", entries)
	}
	if runtime, err := b.f.C.Runtime(ctx); err != nil || runtime.Jev.Mode != jev.ModeUnconfigured {
		t.Fatalf("Jev in the status: %+v %v", runtime.Jev, err)
	}
}
