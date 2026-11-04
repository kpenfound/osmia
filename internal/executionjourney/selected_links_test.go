package executionjourney

import (
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/daggerfixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestDaggerFixtureProcess(t *testing.T) { daggerfixture.Process() }

// serial: the session API uses production runner composition and a stub Jev.
func TestSessionSelectedLinksReachDagger(t *testing.T) {
	for _, picks := range []int{0, 2} {
		t.Run(string(rune('0'+picks)), func(t *testing.T) {
			links := []string{daggerfixture.PackageA, daggerfixture.TestB}
			cli := daggerfixture.Install(t, daggerfixture.Config{Links: append(slices.Clone(links), daggerfixture.Version)})
			s, c, _, project, stream, _, _ := newSessionControlsFixture(t)
			inspect := service.InspectForTest(s.core())
			inspect.SetReviewChecks(nil)
			score := .9
			if picks > 0 {
				score = .4
			}
			p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "stub", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(score), "check-1": jevtest.Noul(score - .1), "check-2": jevtest.Noul(.01)}}}}}
			settings := config.Jev{Enabled: true, URL: config.DefaultJevURL, Model: config.DefaultJevModel, APIKeyEnv: config.DefaultJevAPIKeyEnv, Timeout: "1s", CheckThreshold: .5, CheckFallbackPicks: picks}
			inspect.SetJudge(&jev.Judge{Config: func() config.Jev { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "key" }, Now: s.now})
			run, err := c.SessionCheck(t.Context(), stream)
			must(t, err)
			if run.Status != ChecksPassed || run.Kind != "smart" || !slices.Equal(run.Selection.Links, links) || !slices.Equal(run.Checks, links) || run.Selection.FallbackPicks != picks || len(p.Requests()) != 1 {
				t.Fatalf("run %+v selection %+v", run, run.Selection)
			}
			cli.AssertRun(t, links)
			repo, err := s.repository(project)
			must(t, err)
			runs, err := sessionCheckRuns(repo, stream)
			must(t, err)
			docs, err := trace.Read[trace.Document](repo, stream)
			must(t, err)
			captured := ""
			for _, d := range docs {
				if d.Path == run.OutputPath {
					captured = d.Content
				}
			}
			if len(runs) != 1 || strings.Count(captured, "$ dagger check") != 2 || strings.Count(run.Output, "CHECKS") != 2 || !slices.Equal(run.Command, checkrun.Command(links)) {
				t.Fatalf("runs %d output %s commands %q", len(runs), captured, run.Command)
			}
		})
	}
}
