package browsertest

import (
	"context"
	"testing"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
)

func OpenCheckFeed(t *testing.T, address string, stream config.WorkstreamID) *Page {
	t.Helper()
	p := OpenBrowser(t)
	p.Run(chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(`
   window.feedEvents = [];
   window.sameFeedDocument = true;
   window.setInterval = () => 0;
   const ServiceEvents = window.EventSource;
   window.EventSource = class extends ServiceEvents {
    constructor(...args) {
     super(...args);
     this.addEventListener('workstream', event => feedEvents.push(JSON.parse(event.data)));
    }
   };`).Do(ctx)
		return err
	}))
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile), chromedp.Navigate("http://"+address+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(stream)
	p.AwaitText(`[data-workstream="`+string(stream)+`"] [data-kind=transition][data-subject=feature]`, "Workstream")
	p.Eval(`window.feedEvents.length = 0`, nil)
	return p
}
