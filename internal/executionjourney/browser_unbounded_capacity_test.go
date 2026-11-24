package executionjourney

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBrowserShowsUnboundedPoolsFromTheServedStatus(t *testing.T) {
	t.Parallel()
	f := journey.NewArchitectFixture(t, func(o *journey.Options) { fixturekit.WithListen(t, o.Config.Root, "web = \"127.0.0.1:0\"\n") })
	held, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	f.Script("draft-1-1", map[string]string{plan.SpecPath: fixturekit.ValidSpec, plan.PlanPath: fixturekit.ValidPlan}, func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
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
	defer f.Stop(t)
	defer close(release)
	f.HandIn(t, "held-draft", fixturekit.HandedDesign)
	<-held
	second := f.HandIn(t, "independent-draft", fixturekit.HandedDesign)
	f.Await(t, second, intakefixture.Sketched)
	f.Await(t, second, func(_, _ string) bool {
		return journey.InspectForTest(f.S).Pool().Slots(nil).Used[intake.ArchitectRole] == 1
	})

	var status json.RawMessage
	fixturekit.Must(t, f.C.Do(context.Background(), "GET", service.Prefix+"/status", nil, &status))
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
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.S.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.AwaitText(`#capacity [data-role=architect] [data-field=slots]`, fmt.Sprintf("%d active (unbounded; 1 per workstream)", int(architect)))
	for _, role := range []string{"architect"} {
		p.AwaitText(`#capacity [data-role=`+role+`] [data-field=note]`, "No factory-wide limit")
		p.AwaitText(`#capacity [data-role=`+role+`] [data-field=note]`, "at most one turn per workstream")
		p.AwaitText(`#capacity [data-role=`+role+`] [data-field=note]`, "no configuration setting")
		p.Await(role+" meter is not full", `document.querySelector('[data-meter=`+role+`]').dataset.full === 'false'`)
	}
	p.Await("the architect meter title", `document.querySelector('[data-meter=architect]').title === `+browsertest.Quote(fmt.Sprintf("architect: %d active (unbounded; 1 per workstream)", int(architect))))
	p.AwaitText(`#meter-total`, fmt.Sprintf("%d/%d", int(used), int(limit)))
	p.Await("the capacity button title", `document.getElementById('capacity-button').title === `+browsertest.Quote(fmt.Sprintf("%d of %d slots in use; %d architect sessions", int(used), int(limit), int(architect))))
	var text string
	p.Eval(`document.getElementById('capacity').textContent + ' ' + document.getElementById('meter-total').textContent + ' ' + document.getElementById('capacity-button').title`, &text)
	for _, bad := range []string{"NaN", "undefined", "/ 0 slots", "of 0 slots", "0/0", "null"} {
		if strings.Contains(text, bad) {
			t.Fatalf("the capacity display shows %q: %s", bad, text)
		}
	}
}
