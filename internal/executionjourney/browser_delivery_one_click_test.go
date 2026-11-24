package executionjourney

import (
	context "context"
	reflect "reflect"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	chromedp "github.com/chromedp/chromedp"
	delivery "github.com/kpenfound/osmia/internal/delivery"
)

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

func approvalsSent(p *page) []map[string]any {
	var out []map[string]any
	p.Eval(`window.approvals`, &out)
	return out
}

func previewOf(p *page, card string) (description string, messages []string) {
	p.Eval(`document.querySelector(`+browsertest.Quote(card+"[data-field=preview] [data-field=description]")+`).textContent`, &description)
	p.Eval(`[...document.querySelectorAll(`+browsertest.Quote(card+"[data-field=preview] [data-field=commit-message]")+`)].map(n => n.textContent)`, &messages)
	return description, messages
}

func awaitOneClickCard(t *testing.T, p *page, card string) string {
	t.Helper()
	approve := card + "button[type=submit]"
	p.Await("the delivery card ready to approve", `document.querySelector(`+browsertest.Quote(approve)+`) && !document.querySelector(`+browsertest.Quote(approve)+`).disabled && document.querySelector(`+browsertest.Quote(card+"[data-field=preview] [data-field=description]")+`) !== null`)
	var editors int
	p.Eval(`document.querySelector(`+browsertest.Quote(card)+`).querySelectorAll('textarea, input').length`, &editors)
	if editors != 0 {
		t.Fatalf("the delivery card has %d editable fields", editors)
	}
	var label string
	p.Eval(`document.querySelector(`+browsertest.Quote(approve)+`).textContent`, &label)
	return label
}

func TestBrowserPageApprovesTheDraftInOneClick(t *testing.T) {
	t.Parallel()
	p := openBrowser(t)
	f, ws, repository, _ := journey.DeliveryFixtureWith(t, journey.ListenWeb)
	fixturekit.Must(t, repository.Close())
	f.Start(t)
	t.Cleanup(func() { f.Stop(t) })
	ctx := context.Background()
	entries := intakefixture.EntriesOf(t, f.C, api.InboxDelivery)
	if len(entries) != 1 {
		t.Fatalf("delivery entries %+v", entries)
	}
	card := intakefixture.DecisionCard(entries[0])
	presented, err := f.C.Delivery(ctx, ws)
	fixturekit.Must(t, err)
	if presented.Approval != nil || len(presented.Messages) == 0 {
		t.Fatalf("the presentation %+v", presented)
	}

	p.Run(chromedp.Navigate("http://" + f.S.WebAddr() + "/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(ws)
	p.AwaitText(card+"[data-field=kind]", "Mark ready for review")
	if label := awaitOneClickCard(t, p, card); label != "Mark ready for review" {
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
	p.AwaitOutcome("Approved marking final review 1")
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
	after, err := f.C.Delivery(ctx, ws)
	fixturekit.Must(t, err)
	if after.Approval == nil || after.Approval.Description != description || after.Approval.DraftHash != presented.DraftHash || len(after.Approval.Messages) != len(messages) {
		t.Fatalf("the recorded approval %+v", after.Approval)
	}
	for i, m := range after.Approval.Messages {
		if m.Message != messages[i] || m.Commit != presented.Messages[i].Commit {
			t.Fatalf("recorded commit message %d %+v, previewed %q", i, m, messages[i])
		}
	}
}

func TestBrowserPageReapprovesTheEditedTextOfARefusedPublication(t *testing.T) {
	t.Parallel()
	p := openBrowser(t)
	f, ws, repository, _ := journey.DeliveryFixtureWith(t, journey.ListenWeb)
	ctx := context.Background()
	presented, apiErr := f.S.DeliveryPresentation(ctx, string(ws))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	edited := "Edited description of the pull request.\n"
	messages := presented.Messages
	first, apiErr := f.S.ApproveDelivery(ctx, string(ws), api.DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash, Description: &edited})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if first.Description != edited || edited == presented.Draft {
		t.Fatalf("the edited approval %+v", first)
	}
	driver := (&journey.Publisher{S: f.S, Repository: repository}).Driver()
	if _, err := driver.Refuse(ctx, ws, delivery.PublishInput{Approval: 1}, "the fork is gone"); err != nil {
		t.Fatal(err)
	}
	fixturekit.Must(t, repository.Close())
	f.Start(t)
	t.Cleanup(func() { f.Stop(t) })
	entries := intakefixture.EntriesOf(t, f.C, api.InboxDelivery)
	if len(entries) != 1 {
		t.Fatalf("delivery entries %+v", entries)
	}
	card := intakefixture.DecisionCard(entries[0])
	offered, err := f.C.Delivery(ctx, ws)
	fixturekit.Must(t, err)
	if offered.Approval == nil || offered.Approval.Description != edited {
		t.Fatalf("the reoffered presentation %+v", offered.Approval)
	}

	p.Run(chromedp.Navigate("http://" + f.S.WebAddr() + "/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(ws)
	p.AwaitText(card+"[data-field=kind]", "Mark ready for review")
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
	p.AwaitOutcome("Approved marking final review 1")
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
	after, err := f.C.Delivery(ctx, ws)
	fixturekit.Must(t, err)
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
