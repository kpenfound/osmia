package executionjourney

import (
	strconv "strconv"
	"strings"
	testing "testing"

	chromedp "github.com/chromedp/chromedp"
	checkrun "github.com/kpenfound/osmia/internal/checks"
)

func TestBrowserFeedCheckHistoryPreservesResultsAndLegacyCounts(t *testing.T) {
	t.Parallel()
	f := newPageFixture(t)
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(stream)
	p.OpenActivity()
	repo := f.s.sole().repository
	at := f.s.now()
	selection := &CheckSelection{Mode: checkrun.SelectionSelected, Links: []string{"dag://go/check"}, Candidates: 113}
	for i, state := range []string{ChecksPassed, ChecksIncomplete, ChecksSuperseded} {
		run := i + 1
		errorText := ""
		if state == ChecksIncomplete {
			errorText = "check runner lost its connection"
		}
		if state == ChecksSuperseded {
			errorText = "the candidate was replaced"
		}
		feedDocument(t, repo, stream, checkDocumentID("upload", run), checkPath("upload", run), 1, at,
			UnitCheckRun{Unit: "upload", Run: run, Status: state, Selection: selection, Error: errorText, StartedAt: at, FinishedAt: at})
		entry := `[data-kind=unit_checks][data-unit=upload][data-run="` + strconv.Itoa(run) + `"]`
		p.AwaitText(entry+` [data-field=state]`, checkStateLabel(state))
		p.AwaitText(entry+` [data-field=selection]`, "checking 1 of 113")
		p.Await("no invented listed total", `!document.querySelector(`+quote(entry+` [data-field=listed]`)+`)`)
		if errorText != "" {
			p.AwaitText(entry+` [data-field=error]`, errorText)
		}
	}
	// Legacy final results have no start/attempt evidence; they still render
	// alongside a separately started retry and a failed review outcome.
	feedDocument(t, repo, stream, finalChecksDocument(1), finalChecksPath(1), 1, at,
		FinalCheckRun{Review: 1, Status: ChecksIncomplete, Error: "engine unavailable", FinishedAt: at})
	feedDocument(t, repo, stream, finalCheckAttemptDocument(1, 2), finalCheckAttemptPath(1, 2), 1, at,
		FinalCheckAttempt{Review: 1, Attempt: 2, StartedAt: at})
	feedDocument(t, repo, stream, finalReportDocument, finalReportPath, 1, at,
		FinalReport{Review: 1, Outcome: finalFailed, Failure: "reader did not produce a report"})
	p.AwaitText(`[data-kind=final_checks][data-attempt=""] [data-field=state]`, "not completed")
	p.AwaitText(`[data-kind=final_checks][data-attempt=""] [data-field=error]`, "engine unavailable")
	p.AwaitText(`[data-kind=final_checks][data-attempt="2"] [data-field=state]`, "running")
	p.AwaitText(`[data-kind=final_review] [data-field=failure]`, "reader did not produce a report")
	listed := 214
	selected := &CheckSelection{Mode: checkrun.SelectionSelected, Links: []string{"dag://go/check"}, Candidates: 37, Listed: &listed, Judgment: "judgment_final"}
	feedDocument(t, repo, stream, finalCheckAttemptDocument(2, 1), finalCheckAttemptPath(2, 1), 1, at,
		FinalCheckAttempt{Review: 2, Attempt: 1, Selection: selected, StartedAt: at})
	const finalEntry = `[data-kind=final_checks][data-review="2"][data-attempt="1"]`
	p.AwaitText(finalEntry+` [data-field=selection]`, "checking 1 of 37")
	p.AwaitText(finalEntry+` [data-field=listed]`, "narrowed from 214 listed")
	p.AwaitText(finalEntry+` [data-field=selection-description]`, checkrun.DescribeSelection(*selected))
	feedDocument(t, repo, stream, finalChecksDocument(2), finalChecksPath(2), 1, at,
		FinalCheckRun{Review: 2, Attempt: 1, Selection: selected, Status: ChecksPassed, StartedAt: at, FinishedAt: at})
	p.AwaitText(finalEntry+` [data-field=state]`, "passed")
	p.AwaitText(finalEntry+` [data-field=selection-description]`, checkrun.DescribeSelection(*selected))
	var selectedText string
	p.Eval(`document.querySelector(`+quote(finalEntry)+`).textContent`, &selectedText)
	if strings.Contains(selectedText, "every check") {
		t.Fatalf("selected final entry claims full run: %s", selectedText)
	}
	assertFeedAnnotations(t, p, `[data-kind=unit_checks], [data-kind=final_checks], [data-kind=final_review]`, 7)
}

func checkStateLabel(state string) string {
	if state == ChecksIncomplete {
		return "not completed"
	}
	return state
}
