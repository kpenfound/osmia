package executionjourney

import (
	context "context"
	chromedp "github.com/chromedp/chromedp"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	strconv "strconv"
	"strings"
	testing "testing"
)

func TestBrowserFinalReviewChecksStartLiveAfterPreparation(t *testing.T) {
	t.Parallel()
	f, stream, repo, reviewer := newFinalFixture(t, "browser-final-checks")
	_, _ = assembleBoth(t, f, repo, reviewer, stream,
		map[string]string{"internal/trace/resume.go": "package trace\n"},
		map[string]string{"internal/trace/dedupe.go": "package trace\n"})
	advanceUpstream(t, f, map[string]string{"UPSTREAM.md": "upstream\n"})
	turn := f.finalTurn(1, 1, func(ctx context.Context, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "The branch works together.", "findings": []any{}})
		return err
	})
	must(t, repo.Close())
	checks := newHeldChecks()
	preparing, release := make(chan string, 1), make(chan struct{})
	f.opts.boundary = func(ctx context.Context, name string) error {
		if name == "final-rebase-replaying" || name == "final-checks-exporting" {
			preparing <- name
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	allow := startFeedChecks(t, f, checks, FinalReviewAction)
	p := openCheckFeed(t, f, stream)
	const entry = `[data-kind=final_checks][data-review="1"][data-attempt="1"]`
	allow()
	for _, boundary := range []string{"final-rebase-replaying", "final-checks-exporting"} {
		if got := awaitCheck(t, preparing); got != boundary {
			t.Fatalf("preparing %s, want %s", got, boundary)
		}
		if starts := finalCheckStarts(t, f.repository(), stream); len(starts) != 0 {
			t.Fatalf("checks started during %s: %+v", boundary, starts)
		}
		p.Await("no check entry during "+boundary, `!document.querySelector('[data-kind=final_checks]')`)
		release <- struct{}{}
	}
	if ran := awaitCheck(t, checks.entered); len(ran) != 0 {
		t.Fatalf("final checks selected links: %v", ran)
	}
	awaitFeedEvent(p, stream)
	p.Await("the final checks running", `document.querySelector(`+quote(entry)+`)?.dataset.state === 'running'`)
	p.AwaitText(entry+` [data-field=selection]`, "checking every check: the Jev boost is off")
	p.AwaitText(entry+` [data-field=selection-description]`, "every check ran: the Jev boost is off")
	p.Await("no final outcome while checks run", `!document.querySelector('[data-kind=final_review]') && !document.querySelector('[data-kind=session][data-turn='+`+quote(turn)+`+']')`)
	assertFeedAnnotations(t, p, entry, 1)
	checks.release <- checkOutcome{Result: CheckResult{Output: passedReport}}
	awaitFeedEvent(p, stream)
	p.Await("the final checks passed", `document.querySelector(`+quote(entry)+`)?.dataset.state === 'passed'`)
	const outcome = `[data-kind=final_review][data-review="1"]`
	p.AwaitText(outcome+` [data-field=outcome]`, "reviewed")
	p.AwaitText(outcome+` [data-field=blocking]`, "0 blocking findings")
	p.AwaitText(outcome+` [data-field=gaps]`, "0 uncovered criteria")
	p.AwaitText(outcome+` [data-field=turn]`, turn)
	session := `[data-kind=session][data-turn="` + turn + `"]`
	p.Await("the final reader's completed turn", `document.querySelector(`+quote(session)+`)?.dataset.state === 'done'`)
	p.AwaitText(session+` [data-field=profile]`, "default")
	const transition = `[data-kind=transition][data-subject=final-review][data-to=reviewed-1]`
	p.AwaitText(transition+` [data-field=subject]`, "Final review")
	p.AwaitText(transition+` [data-field=from]`, "requested-1")
	p.AwaitText(transition+` [data-field=to]`, "reviewed-1")
	p.AwaitText(transition+` [data-field=reason]`, "final review 1 read")
	assertFeedAnnotations(t, p, entry+", "+outcome+", "+transition, 3)
	// The link identifies the same agent turn shown in the feed.
	var linked bool
	p.Eval(`document.querySelector(`+quote(outcome+` [data-field=turn]`)+`).hash === '#' + document.querySelector(`+quote(session)+`).id`, &linked)
	if !linked {
		t.Fatal("final review does not link its reader turn")
	}
}

func TestBrowserFeedCheckHistoryPreservesResultsAndLegacyCounts(t *testing.T) {
	t.Parallel()
	f := newPageFixture(t)
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(stream)
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
