package executionjourney

import (
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	strconv "strconv"
	"strings"
	testing "testing"

	chromedp "github.com/chromedp/chromedp"
	checkrun "github.com/kpenfound/osmia/internal/checks"
)

func TestBrowserFeedCheckHistoryPreservesResultsAndSelectionsWithoutAListedTotal(t *testing.T) {
	t.Parallel()
	f := newPageFixture(t)
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(fixturekit.Stream)
	p.OpenActivity()
	repo := f.s.sole().repository
	at := f.s.now()
	selection := &api.CheckSelection{Mode: checkrun.SelectionSelected, Links: []string{"dag://go/check"}, Candidates: 113}
	for i, state := range []string{checkrun.ChecksPassed, checkrun.ChecksIncomplete, checkrun.ChecksSuperseded} {
		run := i + 1
		errorText := ""
		if state == checkrun.ChecksIncomplete {
			errorText = "check runner lost its connection"
		}
		if state == checkrun.ChecksSuperseded {
			errorText = "the candidate was replaced"
		}
		fixturekit.FeedDocument(t, repo, fixturekit.Stream, execution.CheckDocumentID("upload", run), execution.CheckPath("upload", run), 1, at,
			execution.UnitCheckRun{Unit: "upload", Run: run, Status: state, Selection: selection, Error: errorText, StartedAt: at, FinishedAt: at})
		entry := `[data-kind=unit_checks][data-unit=upload][data-run="` + strconv.Itoa(run) + `"]`
		p.AwaitText(entry+` [data-field=state]`, checkStateLabel(state))
		p.AwaitText(entry+` [data-field=selection]`, "checking 1 of 113")
		p.Await("no invented listed total", `!document.querySelector(`+browsertest.Quote(entry+` [data-field=listed]`)+`)`)
		if errorText != "" {
			p.AwaitText(entry+` [data-field=error]`, errorText)
		}
	}

	fixturekit.FeedDocument(t, repo, fixturekit.Stream, delivery.FinalChecksDocument(1), delivery.FinalChecksPath(1), 1, at,
		delivery.FinalCheckRun{Review: 1, Status: checkrun.ChecksIncomplete, Error: "engine unavailable", FinishedAt: at})
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, delivery.FinalCheckAttemptDocument(1, 2), delivery.FinalCheckAttemptPath(1, 2), 1, at,
		delivery.FinalCheckAttempt{Review: 1, Attempt: 2, StartedAt: at})
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, delivery.FinalReportDocument, delivery.FinalReportPath, 1, at,
		api.FinalReport{Review: 1, Outcome: delivery.FinalFailed, Failure: "reader did not produce a report"})
	p.AwaitText(`[data-kind=final_checks][data-attempt=""] [data-field=state]`, "not completed")
	p.AwaitText(`[data-kind=final_checks][data-attempt=""] [data-field=error]`, "engine unavailable")
	p.AwaitText(`[data-kind=final_checks][data-attempt="2"] [data-field=state]`, "running")
	p.AwaitText(`[data-kind=final_review] [data-field=failure]`, "reader did not produce a report")
	listed := 214
	selected := &api.CheckSelection{Mode: checkrun.SelectionSelected, Links: []string{"dag://go/check"}, Candidates: 37, Listed: &listed, Judgment: "judgment_final"}
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, delivery.FinalCheckAttemptDocument(2, 1), delivery.FinalCheckAttemptPath(2, 1), 1, at,
		delivery.FinalCheckAttempt{Review: 2, Attempt: 1, Selection: selected, StartedAt: at})
	const finalEntry = `[data-kind=final_checks][data-review="2"][data-attempt="1"]`
	p.AwaitText(finalEntry+` [data-field=selection]`, "checking 1 of 37")
	p.AwaitText(finalEntry+` [data-field=listed]`, "narrowed from 214 listed")
	p.AwaitText(finalEntry+` [data-field=selection-description]`, "Jev selected 1 of 37 check links for the changed files in judgment judgment_final")
	fixturekit.FeedDocument(t, repo, fixturekit.Stream, delivery.FinalChecksDocument(2), delivery.FinalChecksPath(2), 1, at,
		delivery.FinalCheckRun{Review: 2, Attempt: 1, Selection: selected, Status: checkrun.ChecksPassed, StartedAt: at, FinishedAt: at})
	p.AwaitText(finalEntry+` [data-field=state]`, "passed")
	p.AwaitText(finalEntry+` [data-field=selection-description]`, checkrun.DescribeSelection(*selected))
	var selectedText string
	p.Eval(`document.querySelector(`+browsertest.Quote(finalEntry)+`).textContent`, &selectedText)
	if strings.Contains(selectedText, "every check") {
		t.Fatalf("selected final entry claims full run: %s", selectedText)
	}
	assertFeedAnnotations(t, p, `[data-kind=unit_checks], [data-kind=final_checks], [data-kind=final_review]`, 7)
}

func checkStateLabel(state string) string {
	if state == checkrun.ChecksIncomplete {
		return "not completed"
	}
	return state
}
