package execution

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/servicetest/daggerfixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestDaggerFixtureProcess(t *testing.T) { daggerfixture.Process() }

// serial: real unit selection, runner, persisted evidence and reviewer prompt
// against an executable that refuses different filters in one command.
func TestUnitSelectedLinksReachDagger(t *testing.T) {
	for _, picks := range []int{0, 2} {
		t.Run(string(rune('0'+picks)), func(t *testing.T) {
			links := []string{daggerfixture.PackageA, daggerfixture.TestB}
			cli := daggerfixture.Install(t, daggerfixture.Config{Links: append(slices.Clone(links), daggerfixture.Version)})
			f, stream, repo := newChecksFixture(t, "selected-links", checkrun.DaggerChecks{})
			score := .9
			if picks > 0 {
				score = .4
			}
			p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "stub", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(score), "check-1": jevtest.Noul(score - .1), "check-2": jevtest.Noul(.01)}}}}}
			settings := config.Jev{Enabled: true, URL: config.DefaultJevURL, Model: config.DefaultJevModel, APIKeyEnv: config.DefaultJevAPIKeyEnv, Timeout: "1s", CheckThreshold: .5, CheckFallbackPicks: picks}
			f.s.Jev = &jev.Judge{Config: func() config.Jev { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "key" }, Now: f.s.Now}
			runChecks(t, f.s, repo, stream)
			run := onlyRun(t, repo, stream)
			if run.Status != ChecksPassed || !slices.Equal(run.Selection.Links, links) || run.Selection.FallbackPicks != picks || run.Selection.Judgment == "" || len(p.Requests()) != 1 {
				t.Fatalf("run %+v selection %+v", run, run.Selection)
			}
			cli.AssertRun(t, links)
			docs, err := trace.Read[trace.Document](repo, stream)
			must(t, err)
			captured := ""
			results := 0
			for _, d := range docs {
				if d.Path == run.OutputPath {
					captured = d.Content
				}
				if d.Path == checkPath("resume", run.Run) {
					var saved UnitCheckRun
					must(t, json.Unmarshal([]byte(d.Content), &saved))
					results++
					if saved.Status != ChecksPassed {
						t.Fatalf("saved %+v", saved)
					}
				}
			}
			if results != 1 || strings.Count(captured, "$ dagger check") != 2 || strings.Count(run.Output, "CHECKS") != 2 {
				t.Fatalf("results %d output %s summary %s", results, captured, run.Output)
			}
			prompt := reviewTurn(t, f, repo, stream).Prompt
			for _, link := range links {
				if !strings.Contains(prompt, link) || !strings.Contains(captured, link) {
					t.Fatalf("missing %s in evidence", link)
				}
			}
			if !slices.Equal(run.Command, checkrun.Command(links)) {
				t.Fatalf("commands %q", run.Command)
			}
		})
	}
}

// serial: the start keeps the picks while the result describes the full run.
func TestUnitPicksLateRefusalRecordsFullRun(t *testing.T) {
	links := []string{daggerfixture.PackageA, daggerfixture.TestB}
	a1 := daggerfixture.PackageA + "&go-test=T1"
	cli := daggerfixture.Install(t, daggerfixture.Config{Links: links, Attempts: []daggerfixture.Attempt{
		{Fail: a1}, {}, {}, {Refusal: daggerfixture.Refusal}, {},
	}})
	f, stream, repo := newChecksFixture(t, "late-refusal", checkrun.DaggerChecks{})
	p := &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "stub", Answers: map[string]systemone.Answer{"check-0": jevtest.Noul(.4), "check-1": jevtest.Noul(.3)}}}}}
	settings := config.Jev{Enabled: true, URL: config.DefaultJevURL, Model: config.DefaultJevModel, APIKeyEnv: config.DefaultJevAPIKeyEnv, Timeout: "1s", CheckThreshold: .5, CheckFallbackPicks: 2}
	f.s.Jev = &jev.Judge{Config: func() config.Jev { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "key" }, Now: f.s.Now}
	runChecks(t, f.s, repo, stream)
	run := onlyRun(t, repo, stream)
	if run.Status != ChecksPassed || run.Selection.Mode != checkrun.SelectionFull || run.Selection.Reason != checkrun.SelectionRefused || run.Selection.FallbackPicks != 0 || len(run.Selection.Links) != 0 || !slices.Equal(run.Command, checkrun.Command(nil)) || !slices.Equal(run.Flaky, []string{a1}) {
		t.Fatalf("run %+v", run)
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	starts, results := 0, 0
	captured := ""
	for _, d := range docs {
		switch d.Path {
		case checkSelectionPath("resume", run.Run):
			starts++
			var start UnitCheckSelection
			must(t, json.Unmarshal([]byte(d.Content), &start))
			if start.Selection.FallbackPicks != 2 || !slices.Equal(start.Selection.Links, links) || start.Selection.Judgment != run.Selection.Judgment {
				t.Fatalf("start %+v", start)
			}
		case checkPath("resume", run.Run):
			results++
		case run.OutputPath:
			captured = d.Content
		}
	}
	if starts != 1 || results != 1 || strings.Count(captured, "$ dagger check") != 5 || !strings.Contains(captured, daggerfixture.Refusal) || !strings.Contains(captured, "✔ 5 passed") {
		t.Fatalf("starts %d results %d output %s", starts, results, captured)
	}
	prompt := reviewTurn(t, f, repo, stream).Prompt
	if !strings.Contains(prompt, "Dagger refused") || !strings.Contains(prompt, run.OutputPath) {
		t.Fatalf("review evidence %s", prompt)
	}
	cli.AssertRun(t, nil)
}
