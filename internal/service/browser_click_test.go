package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func TestBrowserClickReselectsReplacedNodeBeforeMouseDispatch(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<button id="resume">Resume</button><script>
		window.clicks = 0; window.trusted = false;
		document.addEventListener('click', e => { window.clicks++; window.trusted = e.isTrusted; });
		</script>`))
	}))
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.Navigate(server.URL))
	lookups := 0
	p.run(clickCurrent("#resume", chromedp.After(func(ctx context.Context, _ cdpruntime.ExecutionContextID, nodes ...*cdp.Node) error {
		lookups++
		if lookups != 1 {
			return nil
		}
		// Replace the selected node after visibility succeeds but before
		// MouseClickNode reads its geometry, as a status refresh can do.
		if err := dom.RemoveNode(nodes[0].NodeID).Do(ctx); err != nil {
			return err
		}
		return chromedp.Evaluate(`document.body.insertAdjacentHTML('afterbegin', '<button id="resume">Resume</button>')`, nil).Do(ctx)
	})))
	if lookups != 2 {
		t.Fatalf("node lookups = %d, want 2", lookups)
	}
	p.await("one trusted mouse click", `window.clicks === 1 && window.trusted`)

	refused := errors.New("click preparation refused")
	lookups = 0
	ctx, cancel := context.WithTimeout(p.ctx, browserTimeout)
	defer cancel()
	err := chromedp.Run(ctx, clickCurrent("#resume", chromedp.After(func(context.Context, cdpruntime.ExecutionContextID, ...*cdp.Node) error {
		lookups++
		return refused
	})))
	if !errors.Is(err, refused) || lookups != 1 {
		t.Fatalf("non-node error was retried: lookups=%d err=%v", lookups, err)
	}
	var clicks int
	p.eval("window.clicks", &clicks)
	if clicks != 1 {
		t.Fatalf("refused click dispatched a mouse event: %d", clicks)
	}
}
