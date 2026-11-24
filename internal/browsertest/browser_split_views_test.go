package browsertest

import (
	"context"
	"github.com/kpenfound/osmia/internal/service"
	"testing"

	"github.com/chromedp/chromedp"
)

func splitLayout(p *page, feed string, side ...string) (wide, stacked bool) {
	p.T.Helper()
	sides := "["
	for _, s := range side {
		sides += Quote(s) + ","
	}
	sides += "]"
	p.Eval(`(() => {
		const box = (s) => document.querySelector(s).getBoundingClientRect();
		const feed = box(`+Quote(feed)+`);
		const column = box('.view:not([hidden]) .split-side');
		const inside = `+sides+`.every((s) => { const r = box(s); return r.left >= column.left - 1 && r.right <= column.right + 1 && r.width > 0; });
		return inside && feed.right <= column.left && column.top < feed.bottom;
	})()`, &wide)
	p.Eval(`(() => {
		const box = (s) => document.querySelector(s).getBoundingClientRect();
		const thread = box('.view:not([hidden]) .thread');
		const column = box('.view:not([hidden]) .split-side');
		return column.top >= thread.bottom - 1 && Math.abs(column.left - thread.left) <= 2;
	})()`, &stacked)
	return
}

func TestBrowserSessionViewIsTwoColumnsOnWideScreens(t *testing.T) {
	t.Parallel()
	s, c, project, _, _ := sessionWebFixture(t)
	opened, err := c.OpenSession(context.Background(), service.SessionOpenRequest{Project: project, Profile: "default", Title: "Explore the layout"})
	must(t, err)
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectSession(opened.Session.Workstream)

	controls := []string{"#session-diff", "#session-commit-form", "#session-check", "#session-push", "#session-destroy"}
	if wide, stacked := splitLayout(p, "#session-feed", controls...); !wide || stacked {
		t.Fatalf("wide viewport: two columns %v, stacked %v", wide, stacked)
	}
	p.Run(chromedp.EmulateViewport(390, 844))
	p.Await("the phone layout", `window.innerWidth <= 400`)
	if wide, stacked := splitLayout(p, "#session-feed", controls...); wide || !stacked {
		t.Fatalf("phone viewport: two columns %v, stacked %v", wide, stacked)
	}
	var ordered bool
	p.Eval(`(() => {
		const tops = ['#session-feed', '#session-form', '#session-stop', '#session-diff', '#session-commit-form', '#session-check', '#session-push'].map((s) => document.querySelector(s).getBoundingClientRect().top);
		return tops.every((top, i) => i === 0 || top >= tops[i - 1]);
	})()`, &ordered)
	if !ordered {
		t.Fatal("the phone layout does not stack the conversation, composer and controls in order")
	}
}

func TestBrowserDesignViewIsTwoColumnsOnWideScreens(t *testing.T) {
	t.Parallel()
	s, _, project, _ := newDesignEnforcementFixture(t, true)
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.OpenNewItem("#sidebar-new-design")
	p.Choose("#design-add-form [name=project]", string(project))
	p.Click("#design-add-form [type=submit]")
	p.AwaitText("#design-add-form .result", "Opened design session")
	p.Await("the design view", `!document.querySelector('[data-view=design]').hidden`)

	controls := []string{"#design-brief", "#design-start", "#design-destroy"}
	if wide, stacked := splitLayout(p, "#design-feed", controls...); !wide || stacked {
		t.Fatalf("wide viewport: two columns %v, stacked %v", wide, stacked)
	}
	p.Run(chromedp.EmulateViewport(390, 844))
	p.Await("the phone layout", `window.innerWidth <= 400`)
	if wide, stacked := splitLayout(p, "#design-feed", controls...); wide || !stacked {
		t.Fatalf("phone viewport: two columns %v, stacked %v", wide, stacked)
	}
	var targets bool
	p.Eval(`['#design-start', '#design-destroy'].every((s) => document.querySelector(s).getBoundingClientRect().height >= 43.5)`, &targets)
	if !targets {
		t.Fatal("a design control is under 44px tall on a phone")
	}
}
