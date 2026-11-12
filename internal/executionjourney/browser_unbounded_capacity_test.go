package executionjourney

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// While an architect draft is in flight, the capacity view a running service
// serializes in /v1/status shows the architect pool unbounded with its use,
// and the web page rendering that response labels the architect and librarian
// pools unbounded, keeps the header's slot totals numeric over the bounded
// pools' limits alone, and still shows the architect's use.
func TestBrowserShowsUnboundedPoolsFromTheServedStatus(t *testing.T) {
	t.Parallel()
	f := newArchitectFixture(t, func(o *Options) { withListen(t, *o, "web = \"127.0.0.1:0\"\n") })
	held, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	f.script("draft-1-1", map[string]string{plan.SpecPath: validSpec, plan.PlanPath: validPlan}, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
		if calls.Add(1) == 1 {
			close(held)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	defer f.stop(t)
	defer close(release)
	f.handIn(t, "held-draft", handedDesign)
	<-held
	second := f.handIn(t, "independent-draft", handedDesign)
	f.await(t, second, sketched)
	f.await(t, second, func(_, _ string) bool { return InspectForTest(f.s).Pool().Slots(nil).Used[architectRole] == 1 })

	var status json.RawMessage
	must(t, f.c.Do(context.Background(), "GET", Prefix+"/status", nil, &status))
	roles := fixturekit.CapacityWire(t, status)
	var used, limit, architect float64
	for _, r := range roles {
		if l, ok := r["limit"].(float64); ok {
			used += r["used"].(float64)
			limit += l
		}
		if r["role"] == "architect" {
			architect = r["used"].(float64)
		}
	}
	if architect < 1 {
		t.Fatalf("the served status shows no architect use: %s", status)
	}

	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.AwaitText(`#capacity [data-role=architect] [data-field=slots]`, fmt.Sprintf("%d active (unbounded; 1 per workstream)", int(architect)))
	p.AwaitText(`#capacity [data-role=librarian] [data-field=slots]`, "0 active (unbounded; 1 per workstream)")
	for _, role := range []string{"architect", "librarian"} {
		p.AwaitText(`#capacity [data-role=`+role+`] [data-field=note]`, "No factory-wide limit")
		p.AwaitText(`#capacity [data-role=`+role+`] [data-field=note]`, "at most one turn per workstream")
		p.AwaitText(`#capacity [data-role=`+role+`] [data-field=note]`, "no configuration setting")
		p.Await(role+" meter is not full", `document.querySelector('[data-meter=`+role+`]').dataset.full === 'false'`)
	}
	p.Await("the architect meter title", `document.querySelector('[data-meter=architect]').title === `+quote(fmt.Sprintf("architect: %d active (unbounded; 1 per workstream)", int(architect))))
	p.AwaitText(`#meter-total`, fmt.Sprintf("%d/%d", int(used), int(limit)))
	p.Await("the capacity button title", `document.getElementById('capacity-button').title === `+quote(fmt.Sprintf("%d of %d slots in use; %d architect/librarian sessions", int(used), int(limit), int(architect))))
	var text string
	p.Eval(`document.getElementById('capacity').textContent + ' ' + document.getElementById('meter-total').textContent + ' ' + document.getElementById('capacity-button').title`, &text)
	for _, bad := range []string{"NaN", "undefined", "/ 0 slots", "of 0 slots", "0/0", "null"} {
		if strings.Contains(text, bad) {
			t.Fatalf("the capacity display shows %q: %s", bad, text)
		}
	}
}
