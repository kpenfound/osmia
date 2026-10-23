package browsertest

import (
	"context"
	"fmt"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

const browserCheckTrace = "https://dagger.cloud/example/traces/browser123"

func assertCheckTrace(p *page, entry string, linked bool) {
	p.T.Helper()
	p.Await("the check entry", `document.querySelector(`+quote(entry)+`) !== null`)
	if linked {
		p.AwaitText(entry+` [data-field=trace-url]`, "View Dagger Cloud trace")
		p.Await("the stored trace URL is linked", `document.querySelector(`+quote(entry+` a[data-field=trace-url]`)+`)?.href === `+quote(browserCheckTrace))
	} else {
		p.Await("no trace link on the run without one", `document.querySelector(`+quote(entry+` [data-field=trace-url]`)+`) === null`)
	}
}

func TestBrowserUnitAndFinalCheckFeedTraceLinks(t *testing.T) {
	t.Parallel()
	f := newPageFixture(t)
	repo := service.InspectForTest(f.s).SoleRepository()
	for n := 1; n <= 2; n++ {
		url := ""
		if n == 1 {
			url = browserCheckTrace
		}
		fixturekit.FeedDocument(t, repo, stream, fmt.Sprintf("unit-upload-checks-%d", n), fmt.Sprintf("units/upload/checks-%d.json", n), 1, demoStart,
			service.UnitCheckRun{Unit: "upload", Run: n, Status: "passed", TraceURL: url, StartedAt: demoStart, FinishedAt: demoStart})
		fixturekit.FeedDocument(t, repo, stream, fmt.Sprintf("final-checks-%d", n), fmt.Sprintf("final/checks-%d.json", n), 1, demoStart,
			service.FinalCheckRun{Review: n, Status: "passed", TraceURL: url, StartedAt: demoStart, FinishedAt: demoStart})
	}
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.SelectWorkstream(stream)
	for n := 1; n <= 2; n++ {
		assertCheckTrace(p, fmt.Sprintf(`[data-kind=unit_checks][data-unit=upload][data-run="%d"]`, n), n == 1)
		assertCheckTrace(p, fmt.Sprintf(`[data-kind=final_checks][data-review="%d"]`, n), n == 1)
	}
}

func TestBrowserSessionCheckFeedTraceLinks(t *testing.T) {
	t.Parallel()
	s, c, projectID, _, _ := sessionWebFixture(t)
	resp, err := c.OpenSession(context.Background(), SessionOpenRequest{Project: projectID, Profile: "default"})
	must(t, err)
	id := resp.Session.Workstream
	_, _, repo, apiErr := service.InspectForTest(s).SessionTrace(string(id))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	for n := 1; n <= 2; n++ {
		url := ""
		if n == 1 {
			url = browserCheckTrace
		}
		fixturekit.FeedDocument(t, repo, id, fmt.Sprintf("session-checks-%d", n), fmt.Sprintf("session-checks/checks-%d.json", n), 1, demoStart,
			service.SessionCheckRun{Run: n, Kind: "full", Status: "passed", TraceURL: url, StartedAt: demoStart, FinishedAt: demoStart})
	}
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+s.WebAddr()+"/#workstream="+string(id)))
	for n := 1; n <= 2; n++ {
		assertCheckTrace(p, fmt.Sprintf(`#session-feed [data-kind=session_checks][data-run="%d"]`, n), n == 1)
	}
	p.Run(chromedp.Reload())
	for n := 1; n <= 2; n++ {
		assertCheckTrace(p, fmt.Sprintf(`#session-feed [data-kind=session_checks][data-run="%d"]`, n), n == 1)
	}
}
