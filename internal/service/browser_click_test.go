package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func TestBrowserClickReselectsPauseReplacedDuringMouseDispatch(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/v1/runtime/pause" {
			requests.Add(1)
			w.Write([]byte(`{"ok":true}`))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<div id="pause-list"></div><div id="result"></div><script>
		window.presses = 0; window.clicks = 0; window.trusted = false;
		function render() {
		  const button = document.createElement('button');
		  button.dataset.field = 'resume'; button.textContent = 'Resume';
		  button.addEventListener('click', async e => {
		    window.clicks++; window.trusted = e.isTrusted;
		    document.getElementById('pause-list').replaceChildren();
		    await fetch('/v1/runtime/pause', {method: 'DELETE'});
		    document.getElementById('result').textContent = 'resumed';
		  });
		  document.getElementById('pause-list').replaceChildren(button);
		}
		render();
		document.addEventListener('mousedown', () => {
		  window.presses++;
		  if (window.presses === 1) render();
		}, true);
		</script>`))
	}))
	t.Cleanup(server.Close)
	p := openBrowser(t)
	p.run(chromedp.Navigate(server.URL))
	p.click("[data-field=resume]")
	var got struct {
		Presses, Clicks int
		Trusted         bool
	}
	p.eval(`({presses: window.presses, clicks: window.clicks, trusted: window.trusted})`, &got)
	if got.Presses != 2 || got.Clicks != 1 || !got.Trusted {
		t.Fatalf("pause replacement during mouse dispatch: %+v; DELETE requests = %d", got, requests.Load())
	}
	p.awaitText("#result", "resumed")
	if got := requests.Load(); got != 1 {
		t.Fatalf("DELETE requests = %d, want exactly one", got)
	}
}

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
