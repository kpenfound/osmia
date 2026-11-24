package executionjourney

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type heldScopedChecks struct {
	*fixturekit.ScopedFakeChecks
	held *heldChecks
}

func (c *heldScopedChecks) Check(ctx context.Context, dir string, identity api.CheckIdentity, links []string) (api.CheckResult, error) {
	return c.held.Check(ctx, dir, identity, links)
}

func TestServiceSmartCheckStagesSurviveRestart(t *testing.T) {
	t.Parallel()
	checker := &heldScopedChecks{ScopedFakeChecks: fixturekit.SmartChecks(), held: newHeldChecks()}
	f, stream, repo := newChecksFixture(t, "smart-stage-restart", checker)
	provider := &jevtest.Provider{Results: []jevtest.Result{
		{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"breadth-0": jevtest.Noul(.9), "breadth-1": jevtest.Noul(.01), "breadth-2": jevtest.Noul(.9)}}},
		{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"depth-0": jevtest.Choice("go-package", .8, .8, "go-test")}}},
		{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(.1), "check-1": jevtest.Noul(.9), "check-2": jevtest.Noul(.1), "check-3": jevtest.Noul(.1)}}},
	}}
	useJevProvider(f.s, provider)
	c, op := requestUnitCheck(t, f.s, repo, stream)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := intakefixture.ApplyCheck(ctx, c, op)
	links := awaitCheck(t, checker.held.entered)
	if !slices.Equal(links, []string{fixturekit.SmartPackageA}) {
		t.Fatalf("runner got %q", links)
	}
	requests := provider.Requests()
	if len(requests) != 3 || !strings.Contains(string(mustJSON(t, requests[0])), fixturekit.SmartTestRoot) || !strings.Contains(string(mustJSON(t, requests[1])), fixturekit.SmartTestRoot) {
		t.Fatalf("stage requests %+v", requests)
	}
	wantRanking := []string{fixturekit.SmartStandalone, fixturekit.SmartPackageA, fixturekit.SmartPackageB, fixturekit.SmartUncovered}
	slices.Sort(wantRanking)
	for i, link := range wantRanking {
		q := requests[2].Questions[[]string{"check-0", "check-1", "check-2", "check-3"}[i]]
		if q.Instructions.(map[string]any)["link"] != link {
			t.Fatalf("ranking question %+v want %s", q, link)
		}
	}
	starts := intakefixture.StreamDocuments(t, repo, stream, execution.CheckSelectionDocument("resume", 1))
	if len(starts) != 1 {
		t.Fatalf("start records %+v", starts)
	}
	var start execution.UnitCheckSelection
	fixturekit.Must(t, json.Unmarshal([]byte(starts[0].Content), &start))
	s := start.Selection
	if s.BreadthJudgment == "" || s.DepthJudgment == "" || s.Judgment == "" || s.Roots == nil || *s.Roots != 3 || len(s.KeptRoots) != 2 || s.Levels[fixturekit.SmartTestRoot] != "go-package" {
		t.Fatalf("selection evidence %+v", s)
	}
	cancel()
	if err := awaitCheck(t, finished); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted check: %v", err)
	}
	fixturekit.Must(t, repo.Close())
	reopened, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { reopened.Close() })

	checker.ListErr = errors.New("listings unavailable after restart")
	replacement := &jevtest.Provider{}
	useJevProvider(f.s, replacement)
	c = &checkers{masons: newMasonController(f.s, reopened)}
	if observed, err := c.Inspect(t.Context(), op); err != nil || observed.State != coreadapter.EffectAbsent {
		t.Fatalf("interrupted observation %+v %v", observed, err)
	}
	finished = intakefixture.ApplyCheck(t.Context(), c, op)
	if again := awaitCheck(t, checker.held.entered); !slices.Equal(again, links) {
		t.Fatalf("resumed links %q", again)
	}
	checker.held.release <- fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport}}
	fixturekit.Must(t, awaitCheck(t, finished))
	run := onlyRun(t, reopened, stream)
	if run.Status != checks.ChecksPassed || !reflect.DeepEqual(*run.Selection, s) || len(replacement.Requests()) != 0 || len(provider.Requests()) != 3 {
		t.Fatalf("resumed run %+v selection %+v", run, run.Selection)
	}
	if after := intakefixture.StreamDocuments(t, reopened, stream, execution.CheckSelectionDocument("resume", 1)); !reflect.DeepEqual(after, starts) {
		t.Fatal("restart rewrote the selection record")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	fixturekit.Must(t, err)
	return data
}
