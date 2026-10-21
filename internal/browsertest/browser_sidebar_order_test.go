package browsertest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

// sidebarFixture opens a fresh project trace with a web listener, without
// starting a service, so the caller can seed workstreams and activity before
// the first start.
func sidebarFixture(t *testing.T) (Options, *trace.Repository) {
	t.Helper()
	opts, _, repo := activityFixture(t)
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString("[listen]\nweb = \"127.0.0.1:0\"\n")
	must(t, err)
	must(t, configFile.Close())
	return opts, repo
}

// The sidebar the web UI renders lists workstreams by last activity, newest
// first, regardless of the order they were created in.
func TestBrowserSidebarOrdersWorkstreamsByActivity(t *testing.T) {
	t.Parallel()
	opts, repo := sidebarFixture(t)
	ctx := context.Background()
	first := config.WorkstreamID("w_00000000000000000000000000000001")
	second := config.WorkstreamID("w_00000000000000000000000000000002")
	third := config.WorkstreamID("w_00000000000000000000000000000003")
	// Created oldest to newest, but given activity in the opposite order, so
	// an order that followed creation instead of activity would disagree.
	must(t, repo.CreateWorkstream(ctx, first, demoStart, ownerActor))
	must(t, repo.CreateWorkstream(ctx, second, demoStart.Add(time.Second), ownerActor))
	must(t, repo.CreateWorkstream(ctx, third, demoStart.Add(2*time.Second), ownerActor))
	recordActivity(t, repo, third, "third-old", demoStart.Add(3*time.Second))
	recordActivity(t, repo, second, "second-mid", demoStart.Add(10*time.Second))
	recordActivity(t, repo, first, "first-new", demoStart.Add(20*time.Second))
	must(t, repo.Close())

	s, _ := start(t, opts)
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.AwaitSidebarOrder(first, second, third)
}

// Opening, selecting or refreshing a workstream, including reloading the
// page afterwards with localStorage kept, never changes its position in the
// sidebar.
func TestBrowserSelectingWorkstreamDoesNotReorderSidebar(t *testing.T) {
	t.Parallel()
	opts, repo := sidebarFixture(t)
	ctx := context.Background()
	first := config.WorkstreamID("w_00000000000000000000000000000011")
	second := config.WorkstreamID("w_00000000000000000000000000000012")
	third := config.WorkstreamID("w_00000000000000000000000000000013")
	must(t, repo.CreateWorkstream(ctx, first, demoStart, ownerActor))
	must(t, repo.CreateWorkstream(ctx, second, demoStart.Add(time.Second), ownerActor))
	must(t, repo.CreateWorkstream(ctx, third, demoStart.Add(2*time.Second), ownerActor))
	recordActivity(t, repo, third, "third-old", demoStart.Add(3*time.Second))
	recordActivity(t, repo, second, "second-mid", demoStart.Add(10*time.Second))
	recordActivity(t, repo, first, "first-new", demoStart.Add(20*time.Second))
	must(t, repo.Close())

	s, _ := start(t, opts)
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.AwaitSidebarOrder(first, second, third)

	// Selecting the workstream last in activity, and the one after it, must
	// not move either one.
	p.SelectWorkstream(third)
	p.AwaitSidebarOrder(first, second, third)
	p.SelectWorkstream(second)
	p.AwaitSidebarOrder(first, second, third)

	// A reload, with localStorage kept, leaves the order unchanged.
	p.Run(chromedp.Reload())
	p.Await("the live connection after the reload", `document.body.dataset.connection === 'live'`)
	p.AwaitSidebarOrder(first, second, third)
}

// After a new activity record is written for a workstream that is not
// currently first, it appears first in the sidebar the next time the list
// loads.
func TestBrowserNewActivityPromotesWorkstreamInSidebar(t *testing.T) {
	t.Parallel()
	opts, repo := sidebarFixture(t)
	ctx := context.Background()
	first := config.WorkstreamID("w_00000000000000000000000000000021")
	second := config.WorkstreamID("w_00000000000000000000000000000022")
	third := config.WorkstreamID("w_00000000000000000000000000000023")
	must(t, repo.CreateWorkstream(ctx, first, demoStart, ownerActor))
	must(t, repo.CreateWorkstream(ctx, second, demoStart.Add(time.Second), ownerActor))
	must(t, repo.CreateWorkstream(ctx, third, demoStart.Add(2*time.Second), ownerActor))
	recordActivity(t, repo, third, "third-old", demoStart.Add(3*time.Second))
	recordActivity(t, repo, second, "second-mid", demoStart.Add(10*time.Second))
	recordActivity(t, repo, first, "first-new", demoStart.Add(20*time.Second))
	must(t, repo.Close())

	s, _ := start(t, opts)
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.AwaitSidebarOrder(first, second, third)

	// New activity on third, which is currently last, is written through the
	// running service, not a separate repository.
	recordActivity(t, service.InspectForTest(s).SoleRepository(), third, "third-new", demoStart.Add(30*time.Second))

	p.Run(chromedp.Reload())
	p.Await("the live connection after the reload", `document.body.dataset.connection === 'live'`)
	p.AwaitSidebarOrder(third, first, second)
}

// Selecting a workstream in the sidebar opens it and marks it with the
// current-selection marker, while other workstreams carry none.
func TestBrowserSelectedWorkstreamIsMarkedCurrent(t *testing.T) {
	t.Parallel()
	opts, repo := sidebarFixture(t)
	ctx := context.Background()
	first := config.WorkstreamID("w_00000000000000000000000000000031")
	second := config.WorkstreamID("w_00000000000000000000000000000032")
	must(t, repo.CreateWorkstream(ctx, first, demoStart, ownerActor))
	must(t, repo.CreateWorkstream(ctx, second, demoStart.Add(time.Second), ownerActor))
	recordActivity(t, repo, second, "second-activity", demoStart.Add(10*time.Second))
	recordActivity(t, repo, first, "first-activity", demoStart.Add(20*time.Second))
	must(t, repo.Close())

	s, _ := start(t, opts)
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.AwaitSidebarOrder(first, second)
	p.Await("first carries the current marker", `document.querySelector('[data-select="`+string(first)+`"]').getAttribute('aria-current') === 'true'`)

	p.SelectWorkstream(second)
	p.Await("second opened", `document.getElementById('workstream-head').dataset.workstream === `+quote(string(second)))
	p.Await("second carries the current marker", `document.querySelector('[data-select="`+string(second)+`"]').getAttribute('aria-current') === 'true'`)
	p.Await("first no longer carries the current marker", `document.querySelector('[data-select="`+string(first)+`"]').getAttribute('aria-current') === 'false'`)
	// Selecting did not reorder the sidebar.
	p.AwaitSidebarOrder(first, second)
}

// The page no longer has a Beekeeper sidebar item or chat panel: the
// removed feature leaves nothing behind in the sidebar or the settings
// menu's views.
func TestBrowserPageHasNoBeekeeperSidebarItemOrPanel(t *testing.T) {
	t.Parallel()
	opts, repo := sidebarFixture(t)
	must(t, repo.Close())
	s, _ := start(t, opts)
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	var hasButton, hasPanel bool
	p.Eval(`document.getElementById('beekeeper-button') !== null`, &hasButton)
	p.Eval(`document.querySelector('[data-view="beekeeper"]') !== null`, &hasPanel)
	if hasButton || hasPanel {
		t.Fatalf("the page still has a beekeeper sidebar item (%v) or panel (%v)", hasButton, hasPanel)
	}
}
