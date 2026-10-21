package executionjourney

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/checkselect"
)

func TestBrowserUnitChecksAppearLiveFromSelectionThroughResult(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"selected", "narrowed", "full"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			checks := newHeldChecks()
			count, judged, selected := 113, 113, 3
			if mode == "narrowed" {
				count, judged, selected = checkselect.MaxQuestions+1, 1, 1
			}
			for i := range count {
				checks.links = append(checks.links, fmt.Sprintf("dag+check://go/packages/tests/test?go-package=internal/trace&go-test=Test%03d", i))
			}
			f, stream, repo := newChecksFixture(t, "browser-unit-checks", checks)
			must(t, repo.Close())
			allow := startFeedChecks(t, f, checks, CheckAction)
			if mode != "full" {
				answers := map[string]systemone.Answer{}
				for i := range judged {
					probability := 0.1
					if i < selected {
						probability = 0.9
					}
					answers[checkselect.Question(i)] = jevtest.Noul(probability)
				}
				boostJev(f.s, &jevtest.Provider{Results: []jevtest.Result{{Response: systemone.Response{Model: "jev-test", Answers: answers}}}})
			}
			p := openCheckFeed(t, f, stream)
			const entry = `[data-kind=unit_checks][data-unit=resume][data-run="1"]`
			p.Await("checks have not started", `!document.querySelector(`+quote(entry)+`)`)
			allow()
			ran := awaitCheck(t, checks.entered)
			awaitFeedEvent(p, stream)
			p.Await("the running check entry", `document.querySelector(`+quote(entry)+`)?.dataset.state === 'running'`)
			p.AwaitText(entry+` [data-field=checks]`, "Unit resume · check run 1")
			selection := "checking " + strconv.Itoa(selected) + " of " + strconv.Itoa(judged)
			if mode == "full" {
				selection = "checking every check: the Jev boost is off"
				if len(ran) != 0 {
					t.Fatalf("full run selected links: %v", ran)
				}
			} else if len(ran) != selected {
				t.Fatalf("runner received %v, want %d selected checks", ran, selected)
			}
			p.AwaitText(entry+` [data-field=selection]`, selection)
			if mode == "narrowed" {
				p.AwaitText(entry+` [data-field=listed]`, "narrowed from 129 listed")
			}
			assertFeedAnnotations(t, p, entry, 1)
			// A recorded failure tests both the terminal state and wrapping a long
			// check link on a phone. The full run completes successfully.
			state := ChecksFailed
			failed := checks.links[0]
			outcome := checkOutcome{Result: CheckResult{ExitCode: 1, Output: failedReport(failed)}}
			if mode == "full" {
				state = ChecksPassed
				outcome = checkOutcome{Result: CheckResult{Output: passedReport}}
			}
			checks.release <- outcome
			awaitFeedEvent(p, stream)
			p.Await("the check result", `document.querySelector(`+quote(entry)+`)?.dataset.state === `+quote(state))
			if mode == "full" {
				selection = "every check ran: the Jev boost is off"
			} else {
				p.AwaitText(entry+` [data-field=failed]`, failed)
			}
			p.AwaitText(entry+` [data-field=selection]`, selection)
			p.Await("one entry for the run", `document.querySelectorAll(`+quote(entry)+`).length === 1`)
			assertFeedAnnotations(t, p, entry, 1)
		})
	}
}
