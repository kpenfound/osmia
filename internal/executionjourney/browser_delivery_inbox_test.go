package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	reflect "reflect"
	sync "sync"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	network "github.com/chromedp/cdproto/network"
	chromedp "github.com/chromedp/chromedp"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestBrowserPageApprovesADeliveryAsShown(t *testing.T) {
	t.Parallel()
	f, ws, repository, report := journey.DeliveryFixtureWith(t, journey.ListenWeb)
	fixturekit.Must(t, repository.Close())
	f.Start(t)
	t.Cleanup(func() { f.Stop(t) })
	p := openBrowser(t)
	ctx := context.Background()

	var eventMu sync.Mutex
	var deliveryEvents []string
	chromedp.ListenTarget(p.Ctx, func(event any) {
		if e, ok := event.(*network.EventEventSourceMessageReceived); ok {
			eventMu.Lock()
			deliveryEvents = append(deliveryEvents, fmt.Sprintf("request=%s id=%q kind=%s data=%.300s", e.RequestID, e.EventID, e.EventName, e.Data))
			if len(deliveryEvents) > 64 {
				deliveryEvents = deliveryEvents[len(deliveryEvents)-64:]
			}
			eventMu.Unlock()
		}
	})
	defer func() {
		if !t.Failed() {
			return
		}
		read, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		presented, err := f.C.Delivery(read, ws)
		t.Logf("delivery at failure: review=%d revision=%d draft=%s approval=%+v error=%v", presented.Report.Review, presented.ReviewRevision, presented.DraftHash, presented.Approval, err)
		inbox, err := f.C.Inbox(read)
		t.Logf("inbox read at failure: %v", err)
		for _, entry := range inbox.Entries {
			if entry.Workstream == ws {
				t.Logf("inbox entry at failure: kind=%s revision=%d answer=%+v", entry.Kind, entry.Revision, entry.Answer)
			}
		}
		eventMu.Lock()
		events := append([]string(nil), deliveryEvents...)
		eventMu.Unlock()
		t.Logf("delivery stream events since approval: %v", events)
		var inboxReads any
		p.Eval(`window.deliveryInboxReads`, &inboxReads)
		t.Logf("page inbox reads: %+v", inboxReads)
	}()

	intakefixture.Mutation(t, f.C, "PUT", "pause", api.PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.Project, Workstream: ws}, Mode: "soft", Reason: "Hold the publication", Source: runtime.PauseOwner})

	recordReport := func(revision int) api.DeliveryPresentation {
		t.Helper()
		content, err := json.Marshal(report)
		fixturekit.Must(t, err)
		h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalReportDocument, Revision: revision, Project: f.Project, Workstream: ws, At: time.Now().UTC(), Actor: delivery.FinalReviewActor, Cause: "final-review"}
		fixturekit.Must(t, f.Repository().RecordDocuments(ctx, []trace.Document{{Header: h, Path: delivery.FinalReportPath, Content: string(content)}}))
		out, err := f.C.Delivery(ctx, ws)
		fixturekit.Must(t, err)
		if out.ReviewRevision != revision {
			t.Fatalf("presentation of report revision %d: %+v", revision, out)
		}
		return out
	}
	first, err := f.C.Delivery(ctx, ws)
	fixturekit.Must(t, err)
	entries := intakefixture.EntriesOf(t, f.C, api.InboxDelivery)
	if len(entries) != 1 {
		t.Fatalf("delivery entries %+v", entries)
	}
	card := intakefixture.DecisionCard(entries[0])
	preview := card + "[data-field=preview] [data-field=description]"
	approve := card + "button[type=submit]"
	pins := func(revision int, presented api.DeliveryPresentation) string {
		return fmt.Sprintf("final review 1, report revision %d, commit %s, draft %s", revision, report.Commit[:12], presented.DraftHash[:12])
	}
	drafted := func(presented api.DeliveryPresentation) {
		t.Helper()
		p.Await("the drafted description", `document.querySelector(`+browsertest.Quote(preview)+`)?.textContent === `+browsertest.Quote(presented.Draft))
		p.Await("the approval enabled", `!document.querySelector(`+browsertest.Quote(approve)+`).disabled`)
	}
	disabled := func() {
		t.Helper()
		var off bool
		p.Eval(`document.querySelector(`+browsertest.Quote(approve)+`).disabled`, &off)
		if !off {
			t.Fatal("The approval is offered while the page does not show the draft it pins")
		}
	}

	link := browsertest.NewLinkProxy(t, f.S.WebAddr())
	resync := func() {
		t.Helper()
		link.Cut()
		p.Await("the lost connection", `document.body.dataset.connection === 'lost'`)
		link.Restore()
		p.Await("the live connection again", `document.body.dataset.connection === 'live'`)
	}

	sent := func() map[string]any {
		t.Helper()
		var out map[string]any
		p.Eval(`window.approvals[window.approvals.length - 1]`, &out)
		return out
	}

	gate := p.Gate("*" + service.Prefix + "/delivery/*")
	gate.Set(true)
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+link.Listener.Addr().String()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Eval(`(() => {
		window.notReloaded = true;
		window.approvals = [];
		const send = window.fetch;
		window.fetch = (url, init) => {
			if (init && init.method === 'POST' && String(url).includes('/delivery/')) {
				window.approvals.push(JSON.parse(init.body));
			}
			return send(url, init).then(async (response) => {
				if (String(url).endsWith('/inbox')) {
					const body = await response.clone().json();
					window.deliveryInboxReads = [...(window.deliveryInboxReads || []),
						{status: response.status, entries: body.entries?.map(e => ({
							kind: e.kind, workstream: e.workstream, revision: e.revision, pins: e.answer?.body
						}))}].slice(-16);
				}
				return response;
			});
		};
	})()`, nil)
	p.SelectWorkstream(ws)
	p.AwaitText(card+"[data-field=kind]", "Mark ready for review")
	p.AwaitText(card+"[data-field=question]", "Mark ready for review: Resumable uploads? Final review 1 of commit "+report.Commit)
	p.AwaitText(card+"[data-field=options]", "Mark ready for review")
	p.AwaitText(card+"[data-field=pins]", pins(1, first))
	p.AwaitText(card+"[data-field=reading]", "The final report is unavailable.")
	disabled()

	gate.Set(false)
	resync()
	p.AwaitText(card+`[data-criterion="spec#2"]`, "dedupe.go and TestDedupe")

	p.AwaitText(card+"[data-field=title]", "Pull request title: "+first.Title)
	drafted(first)

	gate.Set(true)
	second := recordReport(2)
	if second.ReviewRevision != 2 || second.DraftHash != first.DraftHash {
		t.Fatalf("report revision 2 presented %+v", second)
	}
	p.AwaitText(card+"[data-field=pins]", pins(2, second))
	p.AwaitText(card+"[data-field=reading]", "The final report is unavailable.")
	disabled()
	var shown string
	p.Eval(`document.querySelector(`+browsertest.Quote(preview)+`).textContent`, &shown)
	if shown != first.Draft {
		t.Fatalf("the description changed without a read of the new draft: %q", shown)
	}
	gate.Set(false)
	resync()
	drafted(second)

	p.Click(approve)
	p.AwaitOutcome("Approved marking final review 1")
	p.AwaitGone("the approved delivery", card)
	pinned := func(revision int, presented api.DeliveryPresentation) map[string]any {
		return map[string]any{"review": 1.0, "review_revision": float64(revision), "commit": report.Commit, "draft_hash": presented.DraftHash}
	}
	if body := sent(); !reflect.DeepEqual(body, pinned(2, second)) {
		t.Fatalf("the approval the page sent: %v", body)
	}
	after, err := f.C.Delivery(ctx, ws)
	fixturekit.Must(t, err)
	if after.Approval == nil || after.Approval.Description != second.Draft || !reflect.DeepEqual(after.Approval.Messages, second.Messages) || after.Approval.ReviewRevision != 2 || after.Approval.DraftHash != second.DraftHash {
		t.Fatalf("the approval of the draft %+v", after.Approval)
	}

	resync()
	p.Await("the processed approval in the live inbox", `window.deliveryInboxReads?.at(-1)?.entries &&
		!window.deliveryInboxReads.at(-1).entries.some(e => e.kind === 'delivery' && e.workstream === `+browsertest.Quote(string(ws))+`)`)
	eventMu.Lock()
	deliveryEvents = nil
	eventMu.Unlock()

	third := recordReport(3)
	p.AwaitText(card+"[data-field=pins]", pins(3, third))
	drafted(third)
	p.Click(approve)
	p.AwaitOutcomes("Approved marking final review 1", 2)
	p.AwaitGone("the approved delivery", card)
	if body := sent(); !reflect.DeepEqual(body, pinned(3, third)) {
		t.Fatalf("the approval the page sent: %v", body)
	}
	after, err = f.C.Delivery(ctx, ws)
	fixturekit.Must(t, err)
	if after.Approval == nil || after.Approval.Description != third.Draft || !reflect.DeepEqual(after.Approval.Messages, third.Messages) || after.Approval.Review != 1 || after.Approval.ReviewRevision != 3 || after.Approval.DraftHash != third.DraftHash {
		t.Fatalf("the approval %+v", after.Approval)
	}
	p.Await("the same document", `window.notReloaded === true`)
}

func TestBrowserPageShowsAFailingPublication(t *testing.T) {
	t.Parallel()
	f := journey.NewPublicationFixtureWith(t, "commit-per-unit", journey.ListenWeb)
	ws, repository := f.Stream, f.Repository
	ctx := context.Background()
	presented, apiErr := f.S.DeliveryPresentation(ctx, string(ws))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if _, apiErr := f.S.ApproveDelivery(ctx, string(ws), api.DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash}); apiErr != nil {
		t.Fatal(apiErr)
	}
	fixturekit.Must(t, repository.Close())
	refusal := "GitHub answered 403: Resource not accessible by personal access token"
	f.Pulls.FindErr = errors.New(refusal)
	f.Opts.PullRequests = f.Pulls
	f.Start(t)
	t.Cleanup(func() { f.Stop(t) })
	p := openBrowser(t)
	var entry api.InboxEntry
	intakefixture.Soon(t, "the failing publication to reach the inbox", func() bool {
		entries := intakefixture.EntriesOf(t, f.C, api.InboxPublication)
		if len(entries) != 1 {
			return false
		}
		entry = entries[0]
		return true
	})
	card := intakefixture.DecisionCard(entry)
	p.Run(chromedp.Navigate("http://" + f.S.WebAddr() + "/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(ws)
	p.AwaitText(card+"[data-field=kind]", "Publication failing")
	p.AwaitText(card+"[data-field=question]", "Last failure: Effect returned error: ")
	p.AwaitText(card+"[data-field=question]", refusal)
	p.AwaitText(card+"[data-field=blocked]", "Waiting on it: Marking the pull request ready for review.")
	p.AwaitText(card+"[data-field=options]", "Options: none until what blocks it is resolved")
	var answerable bool
	p.Eval(`document.querySelector(`+browsertest.Quote(card+"[data-field=answer]")+`) !== null || document.querySelector(`+browsertest.Quote(card+"[data-field=pins]")+`) !== null`, &answerable)
	if answerable {
		t.Fatal("the page offers an answer to a failing publication")
	}
}
