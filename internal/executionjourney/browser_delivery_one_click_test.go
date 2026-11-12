package executionjourney

import (
	context "context"
	reflect "reflect"
	testing "testing"

	chromedp "github.com/chromedp/chromedp"
	delivery "github.com/kpenfound/osmia/internal/delivery"
)

// recordApprovals makes the page keep the body of every delivery approval it
// sends in window.approvals.
func recordApprovals(p *page) {
	p.Eval(`(() => {
		window.approvals = [];
		const send = window.fetch;
		window.fetch = (url, init) => {
			if (init && init.method === 'POST' && String(url).includes('/delivery/')) {
				window.approvals.push(JSON.parse(init.body));
			}
			return send(url, init);
		};
	})()`, nil)
}

// approvalsSent returns the bodies of the approvals the page sent.
func approvalsSent(p *page) []map[string]any {
	var out []map[string]any
	p.Eval(`window.approvals`, &out)
	return out
}

// previewOf reads the plain text of the card's "What will be published"
// section: the description and each commit message.
func previewOf(p *page, card string) (description string, messages []string) {
	p.Eval(`document.querySelector(`+quote(card+"[data-field=preview] [data-field=description]")+`).textContent`, &description)
	p.Eval(`[...document.querySelectorAll(`+quote(card+"[data-field=preview] [data-field=commit-message]")+`)].map(n => n.textContent)`, &messages)
	return description, messages
}

// awaitOneClickCard waits until the page shows the delivery card ready to
// approve, with no editor, and returns its label.
func awaitOneClickCard(t *testing.T, p *page, card string) string {
	t.Helper()
	approve := card + "button[type=submit]"
	p.Await("the delivery card ready to approve", `document.querySelector(`+quote(approve)+`) && !document.querySelector(`+quote(approve)+`).disabled && document.querySelector(`+quote(card+"[data-field=preview] [data-field=description]")+`) !== null`)
	var editors int
	p.Eval(`document.querySelector(`+quote(card)+`).querySelectorAll('textarea, input').length`, &editors)
	if editors != 0 {
		t.Fatalf("the delivery card has %d editable fields", editors)
	}
	var label string
	p.Eval(`document.querySelector(`+quote(approve)+`).textContent`, &label)
	return label
}

// A delivery with no earlier approval is approved with one click on the
// page. The page sends only the identity the entry pins, and the service
// records an approval whose description and commit messages equal the draft
// the preview showed.
func TestBrowserPageApprovesTheDraftInOneClick(t *testing.T) {
	p := openBrowser(t)
	f, ws, repository, _ := deliveryFixtureWith(t, listenWeb)
	must(t, repository.Close())
	f.start(t)
	t.Cleanup(func() { f.stop(t) })
	ctx := context.Background()
	entries := entriesOf(t, f.c, InboxDelivery)
	if len(entries) != 1 {
		t.Fatalf("delivery entries %+v", entries)
	}
	card := decisionCard(entries[0])
	presented, err := f.c.Delivery(ctx, ws)
	must(t, err)
	if presented.Approval != nil || len(presented.Messages) == 0 {
		t.Fatalf("the presentation %+v", presented)
	}

	p.Run(chromedp.Navigate("http://" + f.s.WebAddr() + "/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(ws)
	p.AwaitText(card+"[data-field=kind]", "Delivery")
	if label := awaitOneClickCard(t, p, card); label != "Open pull request" {
		t.Fatalf("the button reads %q", label)
	}
	description, messages := previewOf(p, card)
	var drafted []string
	for _, m := range presented.Messages {
		drafted = append(drafted, m.Message)
	}
	if description != presented.Draft || !reflect.DeepEqual(messages, drafted) {
		t.Fatalf("the preview %q %q, the draft %q %q", description, messages, presented.Draft, drafted)
	}
	recordApprovals(p)
	p.Click(card + "button[type=submit]")
	p.AwaitOutcome("Approved the delivery of final review 1")
	p.AwaitGone("the approved delivery", card)
	sent := approvalsSent(p)
	if len(sent) != 1 {
		t.Fatalf("approvals sent: %v", sent)
	}
	for _, key := range []string{"description", "messages"} {
		if _, ok := sent[0][key]; ok {
			t.Fatalf("the page sent %q: %v", key, sent[0])
		}
	}
	after, err := f.c.Delivery(ctx, ws)
	must(t, err)
	if after.Approval == nil || after.Approval.Description != description || after.Approval.DraftHash != presented.DraftHash || len(after.Approval.Messages) != len(messages) {
		t.Fatalf("the recorded approval %+v", after.Approval)
	}
	for i, m := range after.Approval.Messages {
		if m.Message != messages[i] || m.Commit != presented.Messages[i].Commit {
			t.Fatalf("recorded commit message %d %+v, previewed %q", i, m, messages[i])
		}
	}
}

// A delivery approved through the API with an edited description and
// commit messages, whose publication was refused, is offered again. The page
// previews the earlier edited text, and approving from it sends no text, so
// the recorded approval holds the text the preview showed. The refusal is
// recorded the way a refused publication records it, before the service
// starts.
func TestBrowserPageReapprovesTheEditedTextOfARefusedPublication(t *testing.T) {
	p := openBrowser(t)
	f, ws, repository, _ := deliveryFixtureWith(t, listenWeb)
	ctx := context.Background()
	presented, api := f.s.deliveryPresentation(ctx, string(ws))
	if api != nil {
		t.Fatal(api)
	}
	edited := "Edited description of the pull request.\n"
	messages := append(presented.Messages[:0:0], presented.Messages...)
	for i := range messages {
		messages[i].Message = "Edited commit message " + messages[i].Commit[:7] + "\n"
	}
	first, api := f.s.approveDelivery(ctx, string(ws), DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash, Description: &edited, Messages: messages})
	if api != nil {
		t.Fatal(api)
	}
	if first.Description != edited || edited == presented.Draft {
		t.Fatalf("the edited approval %+v", first)
	}
	driver := (&publisher{s: f.s, repository: repository}).driver()
	if _, err := driver.Refuse(ctx, ws, delivery.PublishInput{Approval: 1}, "the fork is gone"); err != nil {
		t.Fatal(err)
	}
	must(t, repository.Close())
	f.start(t)
	t.Cleanup(func() { f.stop(t) })
	entries := entriesOf(t, f.c, InboxDelivery)
	if len(entries) != 1 {
		t.Fatalf("delivery entries %+v", entries)
	}
	card := decisionCard(entries[0])
	offered, err := f.c.Delivery(ctx, ws)
	must(t, err)
	if offered.Approval == nil || offered.Approval.Description != edited {
		t.Fatalf("the reoffered presentation %+v", offered.Approval)
	}

	p.Run(chromedp.Navigate("http://" + f.s.WebAddr() + "/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(ws)
	p.AwaitText(card+"[data-field=kind]", "Delivery")
	awaitOneClickCard(t, p, card)
	description, previewed := previewOf(p, card)
	var want []string
	for _, m := range messages {
		want = append(want, m.Message)
	}
	if description != edited || !reflect.DeepEqual(previewed, want) {
		t.Fatalf("the preview %q %q, the earlier approval's text %q %q", description, previewed, edited, want)
	}
	recordApprovals(p)
	p.Click(card + "button[type=submit]")
	p.AwaitOutcome("Approved the delivery of final review 1")
	p.AwaitGone("the approved delivery", card)
	sent := approvalsSent(p)
	if len(sent) != 1 {
		t.Fatalf("approvals sent: %v", sent)
	}
	for _, key := range []string{"description", "messages"} {
		if _, ok := sent[0][key]; ok {
			t.Fatalf("the page sent %q: %v", key, sent[0])
		}
	}
	after, err := f.c.Delivery(ctx, ws)
	must(t, err)
	if after.Approval == nil || after.Approval.Description != description || after.Approval.DraftHash != presented.DraftHash {
		t.Fatalf("the recorded approval %+v", after.Approval)
	}
	if !reflect.DeepEqual(after.Approval.Messages, messages) {
		t.Fatalf("the recorded messages %+v, the earlier approval's %+v", after.Approval.Messages, messages)
	}
	if after.Approval.Description == offered.Draft {
		t.Fatalf("the approval holds the draft %q", after.Approval.Description)
	}
}
