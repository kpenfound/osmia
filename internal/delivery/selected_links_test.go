package delivery

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/servicetest/daggerfixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestDaggerFixtureProcess(t *testing.T) { daggerfixture.Process() }

// serial: final review follows the real selector and runner into the CLI.
func TestFinalSelectedLinksReachDagger(t *testing.T) {
	for _, picks := range []int{0, 2} {
		t.Run(string(rune('0'+picks)), func(t *testing.T) {
			links := []string{daggerfixture.PackageA, daggerfixture.TestB}
			cli := daggerfixture.Install(t, daggerfixture.Config{Links: append(slices.Clone(links), daggerfixture.Version)})
			reviewer, cfg, report := finalChecksFixture(t, nil)
			reviewer.S.Checks = checkrun.DaggerChecks{}
			score := .9
			if picks > 0 {
				score = .4
			}
			p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "stub", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(score), "check-1": jevtest.Noul(score - .1), "check-2": jevtest.Noul(.01)}}}}}
			settings := config.Jev{Enabled: true, URL: config.DefaultJevURL, Model: config.DefaultJevModel, APIKeyEnv: config.DefaultJevAPIKeyEnv, Timeout: "1s", CheckThreshold: .5, CheckFallbackPicks: picks}
			reviewer.S.Jev = &jev.Judge{Config: func() config.Jev { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "key" }}
			run, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != checkrun.ChecksPassed || !slices.Equal(run.Selection.Links, links) || run.Selection.FallbackPicks != picks || run.Selection.Judgment == "" || len(p.Requests()) != 1 {
				t.Fatalf("run %+v selection %+v", run, run.Selection)
			}
			cli.AssertRun(t, links)
			docs, err := trace.Read[trace.Document](reviewer.Repository, stream)
			if err != nil {
				t.Fatal(err)
			}
			captured := ""
			results := 0
			for _, d := range docs {
				if d.Path == run.OutputPath {
					captured = d.Content
				}
				if d.Path == FinalChecksPath(report.Review) {
					results++
				}
			}
			if results != 1 || strings.Count(captured, "$ dagger check") != 2 || strings.Count(run.Output, "CHECKS") != 2 {
				t.Fatalf("results %d output %s summary %s", results, captured, run.Output)
			}
			for _, link := range links {
				if !strings.Contains(FinalCheckEvidence(run), link) {
					t.Fatalf("missing %s in evidence", link)
				}
			}
			if !slices.Equal(run.Command, checkrun.Command(links)) {
				t.Fatalf("commands %q", run.Command)
			}
		})
	}
}

// serial: checks_timeout covers the complete selected final run, including a
// completed first command; it is not restarted for the second command.
func TestFinalSelectedRunSharesChecksTimeout(t *testing.T) {
	links := []string{daggerfixture.PackageA, daggerfixture.TestB}
	cli := daggerfixture.Install(t, daggerfixture.Config{Links: links, Delay: time.Second})
	reviewer, cfg, report := finalChecksFixture(t, nil)
	cfg.Project.ChecksTimeout = "1900ms"
	reviewer.S.Checks = checkrun.DaggerChecks{}
	p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "stub", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(.9), "check-1": jevtest.Noul(.9)}}}}}
	settings := config.Jev{Enabled: true, URL: config.DefaultJevURL, Model: config.DefaultJevModel, APIKeyEnv: config.DefaultJevAPIKeyEnv, Timeout: "1s", CheckThreshold: .5}
	reviewer.S.Jev = &jev.Judge{Config: func() config.Jev { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "key" }}
	run, err := reviewer.FinalChecks(t.Context(), cfg, stream, report)
	if err == nil || !strings.Contains(err.Error(), "checks_timeout 1.9s") {
		t.Fatalf("incomplete review must wait: %v", err)
	}
	if run.Status != checkrun.ChecksIncomplete || !strings.Contains(run.Error, "checks_timeout 1.9s") || !slices.Equal(run.Selection.Links, links) {
		t.Fatalf("run %+v", run)
	}
	events := cli.Events(t)
	if len(events) != 3 || events[0].Kind != "start" || events[1].Kind != "end" || events[2].Kind != "start" {
		t.Fatalf("events %+v", events)
	}
	if !strings.Contains(run.Output, "✔ 2 passed") || len(run.Failed) > 0 {
		t.Fatalf("run %+v", run)
	}
}
