package executionjourney

import (
	context "context"
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	network "github.com/chromedp/cdproto/network"
	chromedp "github.com/chromedp/chromedp"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	os "os"
	filepath "path/filepath"
	sync "sync"
	testing "testing"
	time "time"
)

// listenWeb is the configuration that opens the web listener on a free
// loopback port.
const listenWeb = "[listen]\nweb = \"127.0.0.1:0\"\n"

var decisionCard = intakefixture.DecisionCard

var entriesOf = intakefixture.EntriesOf

// The page shows a delivery with its final report's criteria and the drafted
// description, and offers Approve only while the report and draft the
// entry pins are the ones it shows: not before the presentation is read,
// not while a read of it fails, and not while a newer report's read is
// pending. A draft left as it is is approved as drafted, and an edited one
// with the description the owner wrote, as osmia approve with and without a
// description file records them; each approval takes the entry out.
func TestBrowserPageApprovesADeliveryAsShown(t *testing.T) {
	t.Parallel()
	f, ws, repository, report := deliveryFixtureWith(t, listenWeb)
	must(t, repository.Close())
	f.start(t)
	t.Cleanup(func() { f.stop(t) })
	p := openBrowser(t)
	ctx := context.Background()
	// Keep the stream's last events for a failed delivery wait. The service
	// currently sends no SSE id, so retain the transport request and kind too.
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
		presented, err := f.c.Delivery(read, ws)
		t.Logf("delivery at failure: review=%d revision=%d draft=%s approval=%+v error=%v", presented.Report.Review, presented.ReviewRevision, presented.DraftHash, presented.Approval, err)
		inbox, err := f.c.Inbox(read)
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
	// Publication waits while the workstream is paused.
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: ws}, Mode: "soft", Reason: "Hold the publication", Source: runtime.PauseOwner})
	// recordReport records the next revision of the final report, keeping
	// its criteria and commit, and returns its presentation. Neither the
	// title nor the drafted body is derived from the final report's
	// summary (spec#1, spec#3), so a later revision of the same report
	// presents the same title and draft; what the entry pins is the new
	// review revision.
	recordReport := func(revision int) DeliveryPresentation {
		t.Helper()
		content, err := json.Marshal(report)
		must(t, err)
		h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: finalReportDocument, Revision: revision, Project: f.project, Workstream: ws, At: time.Now().UTC(), Actor: finalReviewActor, Cause: "final-review"}
		must(t, f.repository().RecordDocuments(ctx, []trace.Document{{Header: h, Path: finalReportPath, Content: string(content)}}))
		out, err := f.c.Delivery(ctx, ws)
		must(t, err)
		if out.ReviewRevision != revision {
			t.Fatalf("presentation of report revision %d: %+v", revision, out)
		}
		return out
	}
	first, err := f.c.Delivery(ctx, ws)
	must(t, err)
	entries := entriesOf(t, f.c, InboxDelivery)
	if len(entries) != 1 {
		t.Fatalf("delivery entries %+v", entries)
	}
	card := decisionCard(entries[0])
	description := card + `textarea[name="description"]`
	approve := card + "button[type=submit]"
	pins := func(revision int, presented DeliveryPresentation) string {
		return fmt.Sprintf("final review 1 · report revision %d · commit %s · draft %s", revision, report.Commit[:12], presented.DraftHash[:12])
	}
	drafted := func(presented DeliveryPresentation) {
		t.Helper()
		p.Await("the drafted description", `document.querySelector(`+quote(description)+`).value === `+quote(presented.Draft))
		p.Await("Approve enabled", `!document.querySelector(`+quote(approve)+`).disabled`)
	}
	disabled := func() {
		t.Helper()
		var off bool
		p.Eval(`document.querySelector(`+quote(approve)+`).disabled`, &off)
		if !off {
			t.Fatal("Approve is offered while the page does not show the draft it pins")
		}
	}
	// resync reconnects the page, whose new stream reads the inbox again.
	link := newLinkProxy(t, f.s.WebAddr())
	resync := func() {
		t.Helper()
		link.Cut()
		p.Await("the lost connection", `document.body.dataset.connection === 'lost'`)
		link.Restore()
		p.Await("the live connection again", `document.body.dataset.connection === 'live'`)
	}
	// sent is the body of the page's last approval.
	sent := func() map[string]any {
		t.Helper()
		var out map[string]any
		p.Eval(`window.approvals[window.approvals.length - 1]`, &out)
		return out
	}

	// The presentation cannot be read at first.
	gate := p.Gate("*" + Prefix + "/delivery/*")
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
	p.AwaitText(card+"[data-field=kind]", "Delivery")
	p.AwaitText(card+"[data-field=question]", "Deliver Resumable uploads? Final review 1 of commit "+report.Commit)
	p.AwaitText(card+"[data-field=options]", "Options: approve")
	p.AwaitText(card+"[data-field=pins]", pins(1, first))
	p.AwaitText(card+"[data-field=reading]", "The final report is unavailable.")
	disabled()
	// The next read of the inbox reads the presentation again.
	gate.Set(false)
	resync()
	p.AwaitText(card+`[data-criterion="spec#2"]`, "dedupe.go and TestDedupe")
	// The presentation shows the title to be published alongside the
	// drafted body (spec#7).
	p.AwaitText(card+"[data-field=title]", "Pull request title: "+first.Title)
	drafted(first)

	// A new report is recorded while its presentation cannot be read: the
	// entry pins its new review revision, and the page still holds the
	// earlier one.
	gate.Set(true)
	second := recordReport(2)
	if second.ReviewRevision != 2 || second.DraftHash != first.DraftHash {
		t.Fatalf("report revision 2 presented %+v", second)
	}
	p.AwaitText(card+"[data-field=pins]", pins(2, second))
	p.AwaitText(card+"[data-field=reading]", "The final report is unavailable.")
	disabled()
	var shown string
	p.Eval(`document.querySelector(`+quote(description)+`).value`, &shown)
	if shown != first.Draft {
		t.Fatalf("the description changed without a read of the new draft: %q", shown)
	}
	gate.Set(false)
	resync()
	drafted(second)

	// The draft as it stands is approved as drafted.
	p.Click(approve)
	p.AwaitText("#inbox-result", "Approved the delivery of final review 1")
	p.AwaitGone("the approved delivery", card)
	if body := sent(); body["description"] != second.Draft || body["review_revision"] != 2.0 || body["draft_hash"] != second.DraftHash {
		t.Fatalf("the approval the page sent: %v", body)
	}
	after, err := f.c.Delivery(ctx, ws)
	must(t, err)
	if after.Approval == nil || after.Approval.Description != second.Draft || after.Approval.ReviewRevision != 2 || after.Approval.DraftHash != second.DraftHash {
		t.Fatalf("the approval of the draft %+v", after.Approval)
	}

	// A submitted card disappears optimistically. Observe the accepted
	// approval in the page's inbox before the fixture records another report
	// with the same decision identity.
	resync()
	p.Await("the processed approval in the live inbox", `window.deliveryInboxReads?.at(-1)?.entries &&
		!window.deliveryInboxReads.at(-1).entries.some(e => e.kind === 'delivery' && e.workstream === `+quote(string(ws))+`)`)
	eventMu.Lock()
	deliveryEvents = nil
	eventMu.Unlock()
	// A newer report asks for approval again; the owner edits its draft.
	third := recordReport(3)
	p.AwaitText(card+"[data-field=pins]", pins(3, third))
	drafted(third)
	edited := third.Draft + "\nReviewed on the phone.\n"
	p.SetValue(description, edited)
	p.SetValue(card+`textarea[name="commit-message"]`, "Owner edited commit message")
	p.Click(approve)
	p.AwaitText("#inbox-result", "Approved the delivery of final review 1")
	p.AwaitGone("the approved delivery", card)
	if body := sent(); body["description"] != edited || body["review_revision"] != 3.0 {
		t.Fatalf("the edited approval the page sent: %v", body)
	}
	after, err = f.c.Delivery(ctx, ws)
	must(t, err)
	if after.Approval == nil || after.Approval.Description != edited || after.Approval.Messages[0].Message != "Owner edited commit message" || after.Approval.Review != 1 || after.Approval.ReviewRevision != 3 || after.Approval.DraftHash != third.DraftHash {
		t.Fatalf("the edited approval %+v", after.Approval)
	}
	p.Await("the same document", `window.notReloaded === true`)
}

// The page lists a publication the host keeps refusing with its last
// failure and what it waits on, and offers no answer: it closes once an
// attempt publishes.
func TestBrowserPageShowsAFailingPublication(t *testing.T) {
	t.Parallel()
	f, ws, repository, _ := deliveryFixtureWith(t, listenWeb)
	ctx := context.Background()
	presented, api := f.s.deliveryPresentation(ctx, string(ws))
	if api != nil {
		t.Fatal(api)
	}
	if _, api := f.s.approveDelivery(ctx, string(ws), DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash}); api != nil {
		t.Fatal(api)
	}
	must(t, repository.Close())
	home := filepath.Dir(f.clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	demoGit(t, home, "init", "--quiet", "--bare", fork)
	demoGit(t, home, "-C", f.clone, "remote", "add", "origin", fork)
	refusal := "GitHub answered 403: Resource not accessible by personal access token"
	f.opts.PullRequests = &fakePulls{FindErr: errors.New(refusal), Fork: func() string { return "" }}
	f.start(t)
	t.Cleanup(func() { f.stop(t) })
	p := openBrowser(t)
	var entry InboxEntry
	for deadline := time.Now().Add(browserTimeout); ; time.Sleep(100 * time.Millisecond) {
		if entries := entriesOf(t, f.c, InboxPublication); len(entries) == 1 {
			entry = entries[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the failing publication never reached the inbox")
		}
	}
	card := decisionCard(entry)
	p.Run(chromedp.Navigate("http://" + f.s.WebAddr() + "/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(ws)
	p.AwaitText(card+"[data-field=kind]", "Publication failing")
	p.AwaitText(card+"[data-field=question]", "Last failure: Effect returned error: ")
	p.AwaitText(card+"[data-field=question]", refusal)
	p.AwaitText(card+"[data-field=blocked]", "Waiting on it: Publishing the pull request.")
	p.AwaitText(card+"[data-field=options]", "Options: none until what blocks it is resolved")
	var answerable bool
	p.Eval(`document.querySelector(`+quote(card+"[data-field=answer]")+`) !== null || document.querySelector(`+quote(card+"[data-field=pins]")+`) !== null`, &answerable)
	if answerable {
		t.Fatal("the page offers an answer to a failing publication")
	}
}
