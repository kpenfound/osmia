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

func sidebarFixture(t *testing.T) (service.Options, *trace.Repository) {
	t.Helper()
	opts, _, repo := activityFixture(t)
	configFile, err := os.OpenFile(filepath.Join(opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString("[listen]\nweb = \"127.0.0.1:0\"\n")
	must(t, err)
	must(t, configFile.Close())
	return opts, repo
}

func TestBrowserSidebarOrdersWorkstreamsByActivity(t *testing.T) {
	t.Parallel()
	opts, repo := sidebarFixture(t)
	ctx := context.Background()
	first := config.WorkstreamID("w_00000000000000000000000000000001")
	second := config.WorkstreamID("w_00000000000000000000000000000002")
	third := config.WorkstreamID("w_00000000000000000000000000000003")
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

	p.SelectWorkstream(third)
	p.AwaitSidebarOrder(first, second, third)
	p.SelectWorkstream(second)
	p.AwaitSidebarOrder(first, second, third)

	p.Run(chromedp.Reload())
	p.Await("the live connection after the reload", `document.body.dataset.connection === 'live'`)
	p.AwaitSidebarOrder(first, second, third)
}

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

	recordActivity(t, service.InspectForTest(s).SoleRepository(), third, "third-new", demoStart.Add(30*time.Second))

	p.Run(chromedp.Reload())
	p.Await("the live connection after the reload", `document.body.dataset.connection === 'live'`)
	p.AwaitSidebarOrder(third, first, second)
}

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
	p.AwaitSidebarOrder(first, second)
}
