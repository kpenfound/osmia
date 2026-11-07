package executionjourney

import (
	"context"
	"encoding/json"
	"errors"
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

func (c *heldScopedChecks) Check(ctx context.Context, dir string, identity api.CheckIdentity, links []string) (CheckResult, error) {
	return c.held.Check(ctx, dir, identity, links)
}

func TestServiceSmartCheckStagesSurviveRestart(t *testing.T) {
	t.Parallel()
	checks := &heldScopedChecks{ScopedFakeChecks: fixturekit.SmartChecks(), held: newHeldChecks()}
	f, stream, repo := newChecksFixture(t, "smart-stage-restart", checks)
	provider := &jevtest.Provider{Results: []jevtest.Result{
		{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"breadth-0": jevtest.Noul(.9), "breadth-1": jevtest.Noul(.01), "breadth-2": jevtest.Noul(.9)}}},
		{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"depth-0": jevtest.Choice("go-package", .8, .8, "go-test")}}},
		{Response: systemone.Response{Model: "jev-test", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(.1), "check-1": jevtest.Noul(.9), "check-2": jevtest.Noul(.1), "check-3": jevtest.Noul(.1)}}},
	}}
	boostJev(f.s, provider)
	c, op := requestUnitCheck(t, f.s, repo, stream)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := applyCheck(ctx, c, op)
	links := awaitCheck(t, checks.held.entered)
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
	starts := streamDocuments(t, repo, stream, checkSelectionDocument("resume", 1))
	if len(starts) != 1 {
		t.Fatalf("start records %+v", starts)
	}
	var start UnitCheckSelection
	must(t, json.Unmarshal([]byte(starts[0].Content), &start))
	s := start.Selection
	if s.BreadthJudgment == "" || s.DepthJudgment == "" || s.Judgment == "" || s.Roots == nil || *s.Roots != 3 || len(s.KeptRoots) != 2 || s.Levels[fixturekit.SmartTestRoot] != "go-package" {
		t.Fatalf("selection evidence %+v", s)
	}
	cancel()
	if err := awaitCheck(t, finished); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted check: %v", err)
	}
	must(t, repo.Close())
	reopened, err := trace.Open(f.s.cfg.Root, f.s.cfg.Project)
	must(t, err)
	t.Cleanup(func() { reopened.Close() })
	// With the provider and all listings unavailable, the stored selection still
	// owns the resumed run, including all three judgment identities.
	checks.ListErr = errors.New("listings unavailable after restart")
	replacement := &jevtest.Provider{}
	boostJev(f.s, replacement)
	c = &checkers{masons: newMasonController(f.s, reopened)}
	if observed, err := c.Inspect(t.Context(), op); err != nil || observed.State != coreadapter.EffectAbsent {
		t.Fatalf("interrupted observation %+v %v", observed, err)
	}
	finished = applyCheck(t.Context(), c, op)
	if again := awaitCheck(t, checks.held.entered); !slices.Equal(again, links) {
		t.Fatalf("resumed links %q", again)
	}
	checks.held.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
	must(t, awaitCheck(t, finished))
	run := onlyRun(t, reopened, stream)
	if run.Status != ChecksPassed || !reflect.DeepEqual(*run.Selection, s) || len(replacement.Requests()) != 0 || len(provider.Requests()) != 3 {
		t.Fatalf("resumed run %+v selection %+v", run, run.Selection)
	}
	if after := streamDocuments(t, reopened, stream, checkSelectionDocument("resume", 1)); !reflect.DeepEqual(after, starts) {
		t.Fatal("restart rewrote the selection record")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	must(t, err)
	return data
}
