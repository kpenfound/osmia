package executionjourney

import (
	"fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"strconv"
	"testing"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/config"
)

func TestBrowserUnitChecksAppearLiveFromSelectionThroughResult(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"selected", "picks", "narrowed", "full"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			checker := newHeldChecks()
			count, judged, selected := 113, 113, 3
			if mode == "narrowed" {
				count, judged, selected = checkselect.MaxQuestions+1, 2, 1
			}
			for i := range count {
				checker.links = append(checker.links, fmt.Sprintf("dag+check://go/packages/tests/test?go-package=internal/trace&go-test=Test%03d", i))
			}
			if mode == "narrowed" {
				checker.links[count-1] = "dag+check://go/packages/tests/test?go-package=internal/zzz&go-test=TestOther"
			}
			f, stream, repo := newChecksFixture(t, "browser-unit-checks", checker)
			fixturekit.Must(t, repo.Close())
			allow := startFeedChecks(t, f, checker, service.CheckAction)
			if mode != "full" {
				answers := map[string]systemone.Answer{}
				for i := range judged {
					probability := 0.1
					if i < selected && mode != "picks" {
						probability = 0.9
					}
					answers[checkselect.Question(i)] = jevtest.Noul(probability)
				}
				useJevProvider(f.s, &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-test", Answers: answers}}}})
				if mode == "picks" {
					settings := f.s.jev.Config()
					settings.CheckFallbackPicks = selected
					f.s.jev.Config = func() config.Jev { return settings }
				}
			}
			p := openCheckFeed(t, f, stream)
			const entry = `[data-kind=unit_checks][data-unit=resume][data-run="1"]`
			p.Await("checks have not started", `!document.querySelector(`+browsertest.Quote(entry)+`)`)
			allow()
			ran := awaitCheck(t, checker.entered)
			awaitFeedEvent(p, stream)
			p.Await("the running check entry", `document.querySelector(`+browsertest.Quote(entry)+`)?.dataset.state === 'running'`)
			p.AwaitText(entry+` [data-field=checks]`, "Unit resume, check run 1")
			selection := "checking " + strconv.Itoa(selected) + " of " + strconv.Itoa(judged)
			if mode == "full" {
				selection = "checking every check: Jev is unconfigured"
				if len(ran) != 0 {
					t.Fatalf("full run selected links: %v", ran)
				}
			} else if len(ran) != selected {
				t.Fatalf("runner received %v, want %d selected checks", ran, selected)
			}
			if mode == "picks" {
				selection = "no check reached the threshold; checking the highest-scored checks, checking 3 of 113"
			}
			p.AwaitText(entry+` [data-field=selection]`, selection)
			if mode == "narrowed" {
				p.AwaitText(entry+` [data-field=listed]`, fmt.Sprintf("narrowed from %d listed", count))
			}
			assertFeedAnnotations(t, p, entry, 1)

			state := checks.ChecksFailed
			failed := checker.links[0]
			outcome := fixturekit.CheckOutcome{Result: api.CheckResult{ExitCode: 1, Output: fixturekit.FailedReport(failed)}}
			if mode == "full" {
				state = checks.ChecksPassed
				outcome = fixturekit.CheckOutcome{Result: api.CheckResult{Output: fixturekit.PassedReport, Flaky: []string{checker.links[0]}}}
			}
			checker.release <- outcome
			awaitFeedEvent(p, stream)
			p.Await("the check result", `document.querySelector(`+browsertest.Quote(entry)+`)?.dataset.state === `+browsertest.Quote(state))
			if mode == "full" {
				selection = "every check ran: Jev is unconfigured"
				p.AwaitText(entry+` [data-field=flaky-note]`, "Flaky")
				p.AwaitText(entry+` [data-field=flaky]`, checker.links[0])
			} else {
				p.AwaitText(entry+` [data-field=failed]`, failed)
			}
			if mode == "picks" {
				selection = "no check reached the threshold; the highest-scored checks ran, checking 3 of 113"
			}
			p.AwaitText(entry+` [data-field=selection]`, selection)
			p.Await("one entry for the run", `document.querySelectorAll(`+browsertest.Quote(entry)+`).length === 1`)
			assertFeedAnnotations(t, p, entry, 1)
		})
	}
}
