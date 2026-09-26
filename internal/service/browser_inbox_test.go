package service

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// listenWeb is the configuration that opens the web listener on a free
// loopback port.
const listenWeb = "[listen]\nweb = \"127.0.0.1:0\"\n"

// requestGate fails the page's requests of one kind while it is closed, so
// the page keeps what it last read of them until the gate opens again.
type requestGate struct {
	mu     sync.Mutex
	closed bool
}

func (g *requestGate) set(closed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = closed
}

// gate intercepts the page's requests whose URL matches pattern. A page has
// one gate at a time.
func (p *page) gate(pattern string) *requestGate {
	p.t.Helper()
	g := &requestGate{}
	chromedp.ListenTarget(p.ctx, func(event any) {
		e, ok := event.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		g.mu.Lock()
		closed := g.closed
		g.mu.Unlock()
		go func() {
			ctx := cdp.WithExecutor(p.ctx, chromedp.FromContext(p.ctx).Target)
			if closed {
				fetch.FailRequest(e.RequestID, network.ErrorReasonConnectionRefused).Do(ctx)
			} else {
				fetch.ContinueRequest(e.RequestID).Do(ctx)
			}
		}()
	})
	p.run(fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: pattern}}))
	return g
}

// decisionCard is the selector of the page's card of an inbox entry.
func decisionCard(e InboxEntry) string {
	return fmt.Sprintf(`[data-decision="%s"] `, strings.Join([]string{e.Kind, string(e.Workstream), strconv.Itoa(e.Number), e.Unit, e.Amendment}, ":"))
}

// awaitGone waits until the page shows no card for the entry.
func (p *page) awaitGone(what, card string) {
	p.t.Helper()
	p.await(what+" to leave the inbox", `document.querySelector(`+quote(strings.TrimSpace(card))+`) === null`)
}

// setValue replaces the value of the first field selector matches.
func (p *page) setValue(selector, value string) {
	p.t.Helper()
	p.eval(`document.querySelector(`+quote(selector)+`).value = `+quote(value), nil)
}

// entriesOf returns the listed inbox entries of one kind.
func entriesOf(t *testing.T, c *Client, kind string) []InboxEntry {
	t.Helper()
	inbox, err := c.Inbox(context.Background())
	must(t, err)
	var out []InboxEntry
	for _, e := range inbox.Entries {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// The page lists escalated questions, a contested unit and an amendment
// with their workstream, rephrasing, options, recommendation and what their
// answer pins, and answers each inline through its endpoint: a written
// answer, the one-tap acceptance only a quick reply offers, a ruling on the
// contest and a decision on the amendment. Each answered entry leaves the
// inbox through the event stream. A decision on an amendment packet the
// page no longer shows the latest of is refused; the page shows the refusal
// and the refreshed entry and sends nothing again.
func TestBrowserPageAnswersQuestionsContestsAndAmendments(t *testing.T) {
	p := openBrowser(t)
	f := newPageFixture(t)
	ctx := context.Background()
	live := f.s.active.repository
	at := time.Now().UTC()
	header := func(schema, id string, actor trace.Actor) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1, Project: project, Workstream: stream, At: at, Actor: actor, Cause: "planted"}
	}
	move := func(subject, id, to string, actor trace.Actor, docs ...trace.Document) {
		t.Helper()
		state, err := live.Workflow(stream, subject)
		must(t, err)
		tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: header("osmia.trace.transition", id, actor), Subject: subject, From: state.Value, To: to, Reason: "moved to " + to}}
		if len(docs) > 0 {
			_, err = live.RecordDocumentsWith(ctx, docs, tx)
		} else {
			_, err = live.Transact(ctx, tx)
		}
		must(t, err)
	}

	// Two masons ask a question each, which the chief of staff escalates
	// apart: the first recommends what may be accepted with one tap, the
	// second what may not.
	ask := func(agent, thread, question string) trace.Question {
		t.Helper()
		must(t, live.CreateThread(ctx, trace.Agent{Header: header("osmia.trace.agent", agent, ownerActor), Role: demoRole, ThreadID: thread}))
		asked, err := live.Ask(ctx, agent, claimTurn(t, live, t.TempDir(), stream, agent, thread, "build_"+agent, at), question, at)
		must(t, err)
		return asked
	}
	first := ask(demoAgent, demoThread, "Where does upload state live?")
	second := ask("agent_mason_history", "thread_mason_history", "May I rewrite the branch history?")
	_, err := live.EscalateQuestions(ctx, trace.ChiefOfStaff, f.chief, trace.EscalationRequest{Questions: []string{first.ID}, Rephrasing: "Where should upload state live?",
		Blocked: "The upload unit.", Options: []string{"Files", "A database"}, Recommendation: "Keep it in files."}, at)
	must(t, err)
	_, err = live.EscalateQuestions(ctx, trace.ChiefOfStaff, f.chief, trace.EscalationRequest{Questions: []string{second.ID}, Rephrasing: "May the mason rewrite the branch history?",
		Blocked: "The upload unit.", Recommendation: "Force-push the cleaned branch."}, at)
	must(t, err)
	// A reviewer's turn fails on the index unit.
	move(trace.UnitSubject("index"), "index-reviewing", UnitReviewing, foremanActor)
	move(trace.UnitSubject("index"), "index-contested", UnitContested, reviewerActor)
	// A budget amendment is presented.
	sealed, sealDoc, _, err := seal.Latest(live, stream)
	must(t, err)
	_, err = live.FileBudgetAmendment(ctx, stream, trace.AmendmentRequest{Citations: []string{"spec#1"}, Change: "Raise the per-unit budget.", Reason: "The upload unit needs more turns.",
		Seal: sealed.Seal, SealRevision: sealDoc.Revision, SpecHash: sealed.SpecHash}, at)
	must(t, err)
	packet := func(revision int, recommendation string) trace.Document {
		h := header("osmia.trace.document", "amendment-1-presented-packet", shedActor)
		h.Revision = revision
		return trace.Document{Header: h, Path: amendmentPacketPath("1"), Content: `{"round":1,"recommendation":"` + recommendation + `"}` + "\n"}
	}
	move(amendmentSubject("1"), "amendment-1-presented", amendmentPresented, shedActor, packet(1, "approve: no objection stands"))

	escalations := entriesOf(t, f.c, InboxEscalation)
	contests := entriesOf(t, f.c, InboxContested)
	amendments := entriesOf(t, f.c, InboxAmendment)
	if len(escalations) != 2 || escalations[0].QuickReply != "Keep it in files." || escalations[1].QuickReply != "" || len(contests) != 1 || len(amendments) != 1 {
		t.Fatalf("planted inbox: %+v %+v %+v", escalations, contests, amendments)
	}
	accepted, written, contest, amendment := decisionCard(escalations[0]), decisionCard(escalations[1]), decisionCard(contests[0]), decisionCard(amendments[0])

	link := newLinkProxy(t, f.s.WebAddr())
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+link.listener.Addr().String()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.eval(`window.notReloaded = true`, nil)
	for selector, want := range map[string]string{
		accepted + "[data-field=kind]":            "Question",
		accepted + "[data-field=workstream]":      "Ship resumable uploads.",
		accepted + "[data-field=question]":        "Where should upload state live?",
		accepted + "[data-field=asked]":           "Where does upload state live?",
		accepted + "[data-field=blocked]":         "The upload unit.",
		accepted + "[data-field=options]":         "FilesA database",
		accepted + "[data-field=recommendation]":  "Keep it in files.",
		accepted + "[data-field=pins]":            "inbox entry 1",
		accepted + "[data-field=accept]":          "Accept: Keep it in files.",
		written + "[data-field=question]":         "May the mason rewrite the branch history?",
		written + "[data-field=recommendation]":   "Force-push the cleaned branch.",
		written + "[data-field=pins]":             "inbox entry 2",
		contest + "[data-field=kind]":             "Contested unit",
		contest + "[data-field=question]":         "Unit index is contested: moved to contested",
		contest + "[data-field=options]":          "Options: review",
		contest + "[data-field=pins]":             "unit index",
		amendment + "[data-field=question]":       "Change: Raise the per-unit budget. Reason: The upload unit needs more turns.",
		amendment + "[data-field=options]":        "Options: " + strings.Join(amendments[0].Options, ", "),
		amendment + "[data-field=recommendation]": "approve: no objection stands",
		amendment + "[data-field=pins]":           "amendment 1 · packet revision 1",
	} {
		p.awaitText(selector, want)
	}
	// The API offers no quick reply on the second question, so the page
	// offers no acceptance.
	var acceptable bool
	p.eval(`document.querySelector(`+quote(written+"[data-field=accept]")+`) !== null`, &acceptable)
	if acceptable {
		t.Fatal("the page offers to accept a recommendation the API withholds a quick reply for")
	}

	ruling := func(id string) *trace.Ruling {
		t.Helper()
		questions, err := live.Questions(stream)
		must(t, err)
		i := slices.IndexFunc(questions, func(q trace.QuestionState) bool { return q.Asked.ID == id })
		if i < 0 {
			t.Fatalf("no question %s", id)
		}
		return questions[i].Ruling
	}

	// An answer is written before it is sent.
	p.click(written + "button[type=submit]")
	p.awaitText("#inbox-result", "Write an answer first.")
	if r := ruling(second.ID); r != nil {
		t.Fatalf("an empty answer was recorded: %+v", r)
	}
	p.typeInto(written+"textarea", "Keep the history; add a fixup commit.")
	p.click(written + "button[type=submit]")
	p.awaitText("#inbox-result", "Answered inbox entry 2.")
	p.awaitGone("the written answer's entry", written)
	if r := ruling(second.ID); r == nil || r.OwnerResponse != "Keep the history; add a fixup commit." || r.Actor != ownerActor {
		t.Fatalf("the written answer's ruling: %+v", r)
	}

	// An option fills the answer; the one-tap acceptance sends the quick
	// reply itself.
	p.click(accepted + "[data-field=option]:nth-of-type(2)")
	p.await("the option in the answer", `document.querySelector(`+quote(accepted+"textarea")+`).value === 'A database'`)
	p.click(accepted + "[data-field=accept]")
	p.awaitText("#inbox-result", "Accepted the recommendation on inbox entry 1.")
	p.awaitGone("the accepted entry", accepted)
	if r := ruling(first.ID); r == nil || r.OwnerResponse != "Keep it in files." || r.Actor != ownerActor {
		t.Fatalf("the accepted ruling: %+v", r)
	}

	// A contested unit is ruled on with a decision and a note.
	p.click(contest + "button[type=submit]")
	p.awaitText("#inbox-result", "Choose a decision.")
	p.choose(contest+"select", "review")
	p.click(contest + "button[type=submit]")
	p.awaitText("#inbox-result", "Give a note for the ruling.")
	p.typeInto(contest+"input[name=note]", "The reviewer's sandbox failed; review it again.")
	p.click(contest + "button[type=submit]")
	p.awaitText("#inbox-result", "Ruled review on unit index.")
	p.awaitGone("the contested unit", contest)
	if state, err := live.Workflow(stream, trace.UnitSubject("index")); err != nil || state.Value != UnitReviewing {
		t.Fatalf("the ruled unit: %+v %v", state, err)
	}
	transitions, err := trace.Read[trace.Transition](live, stream)
	must(t, err)
	ruled := slices.DeleteFunc(transitions, func(tr trace.Transition) bool { return tr.Subject != trace.UnitSubject("index") })
	if last := ruled[len(ruled)-1]; last.Actor != ownerActor || !strings.HasSuffix(last.Reason, "The reviewer's sandbox failed; review it again.") {
		t.Fatalf("the contested ruling: %+v", last)
	}

	// The stream is held while the amendment's packet is presented again,
	// so the page still shows the first revision when the owner decides.
	gate := p.gate("*" + Prefix + "/events")
	gate.set(true)
	link.cut()
	p.await("the lost connection", `document.body.dataset.connection === 'lost'`)
	link.restore()
	must(t, live.RecordDocuments(ctx, []trace.Document{packet(2, "reject: the budget holds")}))
	p.awaitText(amendment+"[data-field=pins]", "packet revision 1")
	p.choose(amendment+"select", AmendmentReject)
	p.click(amendment + "button[type=submit]")
	p.awaitText("#inbox-result", "packet revision 1 of amendment 1 is not the latest; revision 2 is presented")
	p.awaitText(amendment+"[data-field=pins]", "packet revision 2")
	p.awaitText(amendment+"[data-field=recommendation]", "reject: the budget holds")
	if out, err := f.c.Amendment(ctx, stream, "1"); err != nil || out.State != amendmentPresented || out.Decision != nil {
		t.Fatalf("the refused decision: %+v %v", out, err)
	}
	gate.set(false)
	p.await("the live connection again", `document.body.dataset.connection === 'live'`)
	p.click(amendment + "button[type=submit]")
	p.awaitText("#inbox-result", "Decided reject on amendment 1.")
	p.awaitGone("the decided amendment", amendment)
	if out, err := f.c.Amendment(ctx, stream, "1"); err != nil || out.Decision == nil || out.Decision.Decision != AmendmentReject || out.Decision.Packet != 2 {
		t.Fatalf("the decided amendment: %+v %v", out, err)
	}
	p.await("the same document", `window.notReloaded === true`)
	// The page sent the refused decision once and never again.
	decisions := 0
	for _, path := range p.paths() {
		if path == Prefix+"/amendment/"+string(stream)+"/1" {
			decisions++
		}
	}
	if decisions != 2 {
		t.Fatalf("the page sent %d decisions on the amendment, want the refused one and the accepted one", decisions)
	}
}

// The page shows a ratification packet with the dissent that stands and
// decides it inline: it overrules one objection and sustains another,
// refuses a redraft without a note, asks for one with a note, and ratifies
// the revisions the redraft's debate concluded at. Each is recorded as the
// matching command's call records it.
func TestBrowserPageDecidesARatificationPacket(t *testing.T) {
	p := openBrowser(t)
	f := newDebateFixtureWith(t, 2, 1, listenWeb, nil)
	defer f.stop(t)
	faults := &faults{}
	veto := shed.ObjectionID(1, committeeAgent(1), 1)
	size := shed.ObjectionID(1, committeeAgent(2), 1)
	f.member(1, 1, 1, objects(faults, shed.Charter, "spec#2", "charter#1"))
	f.member(1, 2, 1, objects(faults, shed.Size, "plan#resume", "spec#1"))
	f.script(replyTurnID(1, 1), nil, answers(faults, "Kept as it is.", veto, size))
	f.script((roundInput{Round: 1, Redraft: true}).turnID(1), map[string]string{plan.PlanPath: splitPlan}, nil)
	f.member(2, 1, 1, silent)
	f.member(2, 2, 1, concedes(faults, size))
	f.script(replyTurnID(2, 1), nil, nil)
	ws := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, ws, "concluded-1")
	faults.check(t)
	entries := f.inboxEntries(t, ws, InboxRatification)
	if len(entries) != 1 {
		t.Fatalf("ratification entries %+v", entries)
	}
	card := decisionCard(entries[0])
	choices := func() []string {
		var out []string
		p.eval(`[...document.querySelector(`+quote(card+"select")+`).options].map((o) => o.value)`, &out)
		return out
	}

	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.eval(`window.notReloaded = true`, nil)
	p.awaitText(card+"[data-field=kind]", "Ratification")
	p.awaitText(card+"[data-field=question]", "Ratify spec.md revision 1 and plan.json revision 1? Debate ended after round 1")
	p.awaitText(card+"[data-field=recommendation]", "do not ratify yet: ratification is blocked by 2 objections")
	p.awaitText(card+"[data-field=options]", "Options: none until what blocks it is resolved")
	p.awaitText(card+"[data-field=pins]", "spec revision 1 · plan revision 1")
	p.awaitText(card+`[data-objection="`+veto+`"] [data-field=standing]`, "blocking")
	p.awaitText(card+`[data-objection="`+veto+`"]`, "charter by "+committeeAgent(1)+" in round 1 on spec#2")
	p.awaitText(card+`[data-objection="`+size+`"] [data-field=argument]`, "It does not hold.")
	// Nothing blocked may be ratified.
	if got := choices(); slices.Contains(got, "ratify") || !slices.Contains(got, "overrule "+veto) || !slices.Contains(got, "sustain "+size) || !slices.Contains(got, "redraft") {
		t.Fatalf("the decisions offered while objections block: %v", got)
	}

	p.choose(card+"select", "overrule "+veto)
	p.typeInto(card+"input[name=note]", "I accept the risk.")
	p.click(card + "button[type=submit]")
	p.awaitText("#inbox-result", "I accept the risk.")
	p.awaitText(card+`[data-objection="`+veto+`"] [data-field=standing]`, "advice, overruled")
	p.choose(card+"select", "sustain "+size)
	p.typeInto(card+"input[name=note]", "Split the resume unit.")
	p.click(card + "button[type=submit]")
	p.awaitText(card+`[data-objection="`+size+`"] [data-field=standing]`, "blocking, sustained")
	entries = f.inboxEntries(t, ws, InboxRatification)
	dissent := f.dissent(t, ws)
	if len(entries) != 1 || len(entries[0].Options) != 0 || len(dissent) != 2 {
		t.Fatalf("after the dispositions: %+v %+v", entries, dissent)
	}
	for _, e := range dissent {
		if e.ID == veto && (e.Disposition != shed.Overruled || e.Note != "I accept the risk.") || e.ID == size && (e.Disposition != shed.Sustained || e.Note != "Split the resume unit.") {
			t.Fatalf("the dispositions %+v", dissent)
		}
	}

	// A redraft says what to change.
	p.choose(card+"select", "redraft")
	p.click(card + "button[type=submit]")
	p.awaitText("#inbox-result", "Say what the redraft should change.")
	if asked, err := shed.Redrafts(f.repository(), ws); err != nil || len(asked) != 0 {
		t.Fatalf("a redraft without a note was recorded: %+v %v", asked, err)
	}
	p.typeInto(card+"input[name=note]", "Split the resume unit into what it addresses.")
	p.click(card + "button[type=submit]")
	// The redraft is debated, and the packet of the round that concludes it
	// replaces the one the page showed.
	p.awaitText(card+"[data-field=pins]", "spec revision 1 · plan revision 2")
	p.awaitText(card+"[data-field=recommendation]", "ratify: no objection stands")
	asked, err := shed.Redrafts(f.repository(), ws)
	must(t, err)
	if len(asked) != 1 || asked[0].Round != 1 || asked[0].Note != "Split the resume unit into what it addresses." {
		t.Fatalf("the recorded redraft %+v", asked)
	}

	p.choose(card+"select", "ratify")
	p.click(card + "button[type=submit]")
	p.awaitText("#inbox-result", "the owner ratified spec.md revision 1 and plan.json revision 2 after round 2")
	p.awaitGone("the ratified packet", card)
	if record := f.ratification(t, ws, 2); record.Revision != (shed.Pin{Spec: 1, Plan: 2}) || record.Round != 2 {
		t.Fatalf("the recorded ratification %+v", record)
	}
	if moves := f.ownerMoves(t, ws); !slices.Equal(moves, []string{"overruled-1", "ruled-1", "redraft-1", "ratified-2"}) {
		t.Fatalf("owner subject went %v", moves)
	}
	p.await("the same document", `window.notReloaded === true`)
	faults.check(t)
}

// The page shows a delivery with its final report's criteria and the drafted
// description, and offers Approve only while the report and draft the
// entry pins are the ones it shows: not before the presentation is read,
// not while a read of it fails, and not while a newer report's read is
// pending. A draft left as it is is approved as drafted, and an edited one
// with the description the owner wrote, as osmia approve with and without a
// description file records them; each approval takes the entry out.
func TestBrowserPageApprovesADeliveryAsShown(t *testing.T) {
	p := openBrowser(t)
	f, ws, repository, report := deliveryFixtureWith(t, listenWeb)
	must(t, repository.Close())
	f.start(t)
	defer f.stop(t)
	ctx := context.Background()
	// Publication waits while the workstream is paused.
	mutation(t, f.c, "PUT", "pause", PauseRequest{Target: runtime.Target{Scope: "workstream", Project: f.project, Workstream: ws}, Mode: "soft", Reason: "Hold the publication", Source: runtime.PauseOwner})
	// recordReport records the next revision of the final report under a new
	// summary, which changes the draft, and returns its presentation.
	recordReport := func(revision int, summary string) DeliveryPresentation {
		t.Helper()
		report.Summary = summary
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
	description := card + "textarea"
	approve := card + "button[type=submit]"
	pins := func(revision int, presented DeliveryPresentation) string {
		return fmt.Sprintf("final review 1 · report revision %d · commit %s · draft %s", revision, report.Commit[:12], presented.DraftHash[:12])
	}
	drafted := func(presented DeliveryPresentation) {
		t.Helper()
		p.await("the drafted description", `document.querySelector(`+quote(description)+`).value === `+quote(presented.Draft))
		p.await("Approve enabled", `!document.querySelector(`+quote(approve)+`).disabled`)
	}
	disabled := func() {
		t.Helper()
		var off bool
		p.eval(`document.querySelector(`+quote(approve)+`).disabled`, &off)
		if !off {
			t.Fatal("Approve is offered while the page does not show the draft it pins")
		}
	}
	// resync reconnects the page, whose new stream reads the inbox again.
	link := newLinkProxy(t, f.s.WebAddr())
	resync := func() {
		t.Helper()
		link.cut()
		p.await("the lost connection", `document.body.dataset.connection === 'lost'`)
		link.restore()
		p.await("the live connection again", `document.body.dataset.connection === 'live'`)
	}
	// sent is the body of the page's last approval.
	sent := func() map[string]any {
		t.Helper()
		var out map[string]any
		p.eval(`window.approvals[window.approvals.length - 1]`, &out)
		return out
	}

	// The presentation cannot be read at first.
	gate := p.gate("*" + Prefix + "/delivery/*")
	gate.set(true)
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+link.listener.Addr().String()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.eval(`(() => {
		window.notReloaded = true;
		window.approvals = [];
		const send = window.fetch;
		window.fetch = (url, init) => {
			if (init && init.method === 'POST' && String(url).includes('/delivery/')) {
				window.approvals.push(JSON.parse(init.body));
			}
			return send(url, init);
		};
	})()`, nil)
	p.awaitText(card+"[data-field=kind]", "Delivery")
	p.awaitText(card+"[data-field=question]", "Deliver Resumable uploads? Final review 1 of commit "+report.Commit)
	p.awaitText(card+"[data-field=options]", "Options: approve")
	p.awaitText(card+"[data-field=pins]", pins(1, first))
	p.awaitText(card+"[data-field=reading]", "The final report is unavailable.")
	disabled()
	// The next read of the inbox reads the presentation again.
	gate.set(false)
	resync()
	p.awaitText(card+`[data-criterion="spec#2"]`, "dedupe.go and TestDedupe")
	drafted(first)

	// A new report is recorded while its presentation cannot be read: the
	// entry pins its draft, and the page still holds the earlier one.
	gate.set(true)
	second := recordReport(2, "Resumable uploads with dedupe")
	if second.DraftHash == first.DraftHash {
		t.Fatal("the new summary left the draft as it was")
	}
	p.awaitText(card+"[data-field=pins]", pins(2, second))
	p.awaitText(card+"[data-field=reading]", "The final report is unavailable.")
	disabled()
	var shown string
	p.eval(`document.querySelector(`+quote(description)+`).value`, &shown)
	if shown != first.Draft {
		t.Fatalf("the description changed without a read of the new draft: %q", shown)
	}
	gate.set(false)
	resync()
	drafted(second)

	// The draft as it stands is approved as drafted.
	p.click(approve)
	p.awaitText("#inbox-result", "Approved the delivery of final review 1")
	p.awaitGone("the approved delivery", card)
	if body := sent(); body["description"] != nil || body["review_revision"] != 2.0 || body["draft_hash"] != second.DraftHash {
		t.Fatalf("the approval the page sent: %v", body)
	}
	after, err := f.c.Delivery(ctx, ws)
	must(t, err)
	if after.Approval == nil || after.Approval.Description != second.Draft || after.Approval.ReviewRevision != 2 || after.Approval.DraftHash != second.DraftHash {
		t.Fatalf("the approval of the draft %+v", after.Approval)
	}

	// A newer report asks for approval again; the owner edits its draft.
	third := recordReport(3, "Resumable uploads, reviewed again")
	p.awaitText(card+"[data-field=pins]", pins(3, third))
	drafted(third)
	edited := third.Draft + "\nReviewed on the phone.\n"
	p.setValue(description, edited)
	p.click(approve)
	p.awaitText("#inbox-result", "Approved the delivery of final review 1")
	p.awaitGone("the approved delivery", card)
	if body := sent(); body["description"] != edited || body["review_revision"] != 3.0 {
		t.Fatalf("the edited approval the page sent: %v", body)
	}
	after, err = f.c.Delivery(ctx, ws)
	must(t, err)
	if after.Approval == nil || after.Approval.Description != edited || after.Approval.Review != 1 || after.Approval.ReviewRevision != 3 || after.Approval.DraftHash != third.DraftHash {
		t.Fatalf("the edited approval %+v", after.Approval)
	}
	p.await("the same document", `window.notReloaded === true`)
}
