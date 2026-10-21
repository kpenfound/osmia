package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	cdppage "github.com/chromedp/cdproto/page"
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

// decisionCard is the selector of the page's card of an inbox entry.
func decisionCard(e InboxEntry) string {
	return fmt.Sprintf(`[data-decision="%s"] `, strings.Join([]string{e.Kind, string(e.Project), string(e.Workstream), strconv.Itoa(e.Number), e.Unit, e.Amendment}, ":"))
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
	t.Parallel()
	f := newPageFixture(t)
	p := openBrowser(t)
	ctx := context.Background()
	live := f.s.sole().repository
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
	raiseFixtureContest(t, live, stream, "index")
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
	// Hold inbox responses, including reads already in flight, while the
	// server advances the packet. Submit releases them after the handler
	// has copied the displayed revision into its request.
	p.Run(chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(`(() => {
			const gate = window.inboxGate = { closed: false, waiting: [] };
			const original = window.fetch;
			window.fetch = async (...args) => {
				const response = await original(...args);
				if (String(args[0]).endsWith('/inbox')) {
					const json = response.json.bind(response);
					response.json = async () => {
						const body = await json();
						if (gate.closed) await new Promise(resolve => gate.waiting.push(resolve));
						return body;
					};
				}
				return response;
			};
			document.addEventListener('submit', event => {
				if (!gate.closed || !event.target.closest('[data-kind="amendment"]')) return;
				gate.closed = false;
				gate.waiting.splice(0).forEach(resolve => resolve());
			});
		})()`).Do(ctx)
		return err
	}))
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+link.Listener.Addr().String()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Eval(`window.notReloaded = true`, nil)
	// The list counts what waits on each workstream; the entries show with
	// their workstream.
	p.AwaitText(`[data-select="`+string(stream)+`"] [data-field=needs]`, "4")
	p.Await("nothing waits on the quiet workstream", `document.querySelector(`+quote(`[data-select="`+string(quiet)+`"] [data-field=needs]`)+`) === null`)
	p.SelectWorkstream(stream)
	for selector, want := range map[string]string{
		accepted + "[data-field=kind]":            "Question",
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
		p.AwaitText(selector, want)
	}
	// The card leads with the rephrasing; what the role asked in its own
	// words starts collapsed.
	var collapsed bool
	p.Eval(`document.querySelector(`+quote(accepted+"details.asked")+`)?.open === false`, &collapsed)
	if !collapsed {
		t.Fatal("the asked questions are not collapsed under the rephrasing")
	}
	// The API offers no quick reply on the second question, so the page
	// offers no acceptance.
	var acceptable bool
	p.Eval(`document.querySelector(`+quote(written+"[data-field=accept]")+`) !== null`, &acceptable)
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
	p.Click(written + "button[type=submit]")
	p.AwaitText("#inbox-result", "Write an answer first.")
	if r := ruling(second.ID); r != nil {
		t.Fatalf("an empty answer was recorded: %+v", r)
	}
	p.TypeInto(written+"textarea", "Keep the history; add a fixup commit.")
	p.Click(written + "button[type=submit]")
	p.AwaitText("#inbox-result", "Answered inbox entry 2.")
	p.AwaitGone("the written answer's entry", written)
	p.AwaitText(`[data-select="`+string(stream)+`"] [data-field=needs]`, "3")
	if r := ruling(second.ID); r == nil || r.OwnerResponse != "Keep the history; add a fixup commit." || r.Actor != ownerActor {
		t.Fatalf("the written answer's ruling: %+v", r)
	}

	// An option fills the answer; the one-tap acceptance sends the quick
	// reply itself.
	p.Click(accepted + "[data-field=option]:nth-of-type(2)")
	p.Await("the option in the answer", `document.querySelector(`+quote(accepted+"textarea")+`).value === 'A database'`)
	p.Click(accepted + "[data-field=accept]")
	p.AwaitText("#inbox-result", "Accepted the recommendation on inbox entry 1.")
	p.AwaitGone("the accepted entry", accepted)
	if r := ruling(first.ID); r == nil || r.OwnerResponse != "Keep it in files." || r.Actor != ownerActor {
		t.Fatalf("the accepted ruling: %+v", r)
	}

	// A contested unit is ruled on with a decision and a note.
	p.Click(contest + "button[type=submit]")
	p.AwaitText("#inbox-result", "Choose a decision.")
	p.Choose(contest+"select", "review")
	p.Click(contest + "button[type=submit]")
	p.AwaitText("#inbox-result", "Give a note for the ruling.")
	p.TypeInto(contest+"input[name=note]", "The reviewer's sandbox failed; review it again.")
	p.Click(contest + "button[type=submit]")
	p.AwaitText("#inbox-result", "Ruled review on unit index.")
	p.AwaitGone("the contested unit", contest)
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
	gate := p.Gate("*" + Prefix + "/events")
	gate.Set(true)
	p.Eval(`window.inboxGate.closed = true`, nil)
	link.Cut()
	p.Await("the lost connection", `document.body.dataset.connection === 'lost'`)
	link.Restore()
	must(t, live.RecordDocuments(ctx, []trace.Document{packet(2, "reject: the budget holds")}))
	p.AwaitText(amendment+"[data-field=pins]", "packet revision 1")
	p.Choose(amendment+"select", AmendmentReject)
	p.Click(amendment + "button[type=submit]")
	p.AwaitText("#inbox-result", "packet revision 1 of amendment 1 is not the latest; revision 2 is presented")
	p.AwaitText(amendment+"[data-field=pins]", "packet revision 2")
	p.AwaitText(amendment+"[data-field=recommendation]", "reject: the budget holds")
	if out, err := f.c.Amendment(ctx, stream, "1"); err != nil || out.State != amendmentPresented || out.Decision != nil {
		t.Fatalf("the refused decision: %+v %v", out, err)
	}
	gate.Set(false)
	p.Await("the live connection again", `document.body.dataset.connection === 'live'`)
	p.Click(amendment + "button[type=submit]")
	p.AwaitText("#inbox-result", "Decided reject on amendment 1.")
	p.AwaitGone("the decided amendment", amendment)
	if out, err := f.c.Amendment(ctx, stream, "1"); err != nil || out.Decision == nil || out.Decision.Decision != AmendmentReject || out.Decision.Packet != 2 {
		t.Fatalf("the decided amendment: %+v %v", out, err)
	}
	p.Await("the same document", `window.notReloaded === true`)
	// The page sent the refused decision once and never again.
	decisions := 0
	for _, path := range p.Paths() {
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
	t.Parallel()
	f := newDebateFixtureWith(t, 2, 1, listenWeb, nil)
	t.Cleanup(func() { f.stop(t) })
	p := openBrowser(t)
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
	f.chiefDecides(t, ws, veto, shed.Escalate, "The veto cites the small-changes rule.")
	f.chiefDecides(t, ws, size, shed.Escalate, "Splitting the unit is a judgement call.")
	var entries []InboxEntry
	soon(t, "the ratification entry", func() bool {
		entries = f.inboxEntries(t, ws, InboxRatification)
		return len(entries) > 0
	})
	if len(entries) != 1 {
		t.Fatalf("ratification entries %+v", entries)
	}
	card := decisionCard(entries[0])
	choices := func() []string {
		var out []string
		p.Eval(`[...document.querySelector(`+quote(card+"select")+`).options].map((o) => o.value)`, &out)
		return out
	}

	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Eval(`window.notReloaded = true`, nil)
	p.SelectWorkstream(ws)
	p.AwaitText(card+"[data-field=kind]", "Ratification")
	p.AwaitText(card+"[data-field=question]", "Ratify spec.md revision 1 and plan.json revision 1? Debate ended after round 1")
	p.AwaitText(card+"[data-field=recommendation]", "do not ratify yet: ratification is blocked by 1 objection")
	p.AwaitText(card+"[data-field=options]", "Options: none until what blocks it is resolved")
	p.AwaitText(card+"[data-field=pins]", "spec revision 1 · plan revision 1")
	p.AwaitText(card+`[data-objection="`+veto+`"] [data-field=standing]`, "blocking")
	p.AwaitText(card+`[data-objection="`+veto+`"]`, "charter by "+committeeAgent(1)+" in round 1 on spec#2")
	p.AwaitText(card+`[data-objection="`+size+`"] [data-field=argument]`, "It does not hold.")
	p.AwaitText(card+`[data-objection="`+veto+`"] [data-field=escalation]`, "Chief of staff: The veto cites the small-changes rule.")
	// Nothing blocked may be ratified.
	if got := choices(); slices.Contains(got, "ratify") || !slices.Contains(got, "overrule "+veto) || !slices.Contains(got, "sustain "+size) || !slices.Contains(got, "redraft") {
		t.Fatalf("the decisions offered while objections block: %v", got)
	}

	p.Choose(card+"select", "overrule "+veto)
	p.TypeInto(card+"input[name=note]", "I accept the risk.")
	p.Click(card + "button[type=submit]")
	p.AwaitText("#inbox-result", "I accept the risk.")
	p.AwaitText(card+`[data-objection="`+veto+`"] [data-field=standing]`, "advice, overruled")
	p.Choose(card+"select", "sustain "+size)
	p.TypeInto(card+"input[name=note]", "Split the resume unit.")
	p.Click(card + "button[type=submit]")
	p.AwaitText(card+`[data-objection="`+size+`"] [data-field=standing]`, "blocking, sustained")
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
	p.Choose(card+"select", "redraft")
	p.Click(card + "button[type=submit]")
	p.AwaitText("#inbox-result", "Say what the redraft should change.")
	if asked, err := shed.Redrafts(f.repository(), ws); err != nil || len(asked) != 0 {
		t.Fatalf("a redraft without a note was recorded: %+v %v", asked, err)
	}
	p.TypeInto(card+"input[name=note]", "Split the resume unit into what it addresses.")
	p.Click(card + "button[type=submit]")
	// The redraft is debated, and the packet of the round that concludes it
	// replaces the one the page showed.
	p.AwaitText(card+"[data-field=pins]", "spec revision 1 · plan revision 2")
	p.AwaitText(card+"[data-field=recommendation]", "ratify: no objection stands")
	asked, err := shed.Redrafts(f.repository(), ws)
	must(t, err)
	if len(asked) != 1 || asked[0].Round != 1 || asked[0].Note != "Split the resume unit into what it addresses." {
		t.Fatalf("the recorded redraft %+v", asked)
	}

	p.Choose(card+"select", "ratify")
	p.Click(card + "button[type=submit]")
	p.AwaitText("#inbox-result", "the owner ratified spec.md revision 1 and plan.json revision 2 after round 2")
	p.AwaitGone("the ratified packet", card)
	if record := f.ratification(t, ws, 2); record.Revision != (shed.Pin{Spec: 1, Plan: 2}) || record.Round != 2 {
		t.Fatalf("the recorded ratification %+v", record)
	}
	if moves := f.ownerMoves(t, ws); !slices.Equal(moves, []string{"overruled-1", "ruled-1", "redraft-1", "ratified-2"}) {
		t.Fatalf("owner subject went %v", moves)
	}
	p.Await("the same document", `window.notReloaded === true`)
	faults.check(t)
}

// The ratification card shows the owner the objections the chief of staff
// escalated, with its note, and folds those it dismissed below them with its
// reasons: a dismissed charter veto blocks nothing and takes no ruling there.
func TestBrowserPageFoldsObjectionsTheChiefOfStaffDismissed(t *testing.T) {
	t.Parallel()
	f := newDebateFixtureWith(t, 2, 1, listenWeb, nil)
	t.Cleanup(func() { f.stop(t) })
	p := openBrowser(t)
	faults := &faults{}
	veto := shed.ObjectionID(1, committeeAgent(1), 1)
	size := shed.ObjectionID(1, committeeAgent(2), 1)
	f.member(1, 1, 1, objects(faults, shed.Charter, "spec#2", "charter#1"))
	f.member(1, 2, 1, objects(faults, shed.Size, "plan#resume", "spec#1"))
	f.script(replyTurnID(1, 1), nil, answers(faults, "Kept as it is.", veto, size))
	ws := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, ws, "concluded-1")
	faults.check(t)
	f.chiefDecides(t, ws, veto, shed.Dismiss, "Each unit is one small change with its own test.")
	f.chiefDecides(t, ws, size, shed.Escalate, "Splitting the unit is a judgement call.")
	var entries []InboxEntry
	soon(t, "the ratification entry", func() bool {
		entries = f.inboxEntries(t, ws, InboxRatification)
		return len(entries) > 0
	})
	card := decisionCard(entries[0])

	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(ws)
	p.AwaitText(card+"[data-field=kind]", "Ratification")
	p.AwaitText(card+`[data-objection="`+size+`"] [data-field=escalation]`, "Chief of staff: Splitting the unit is a judgement call.")
	p.AwaitText(card+"[data-field=dismissed] summary", "Dismissed by the chief of staff (1)")
	p.AwaitText(card+`[data-field=dismissed] [data-objection="`+veto+`"] [data-field=note]`, "Chief of staff: Each unit is one small change with its own test.")
	var choices []string
	p.Eval(`[...document.querySelector(`+quote(card+"select")+`).options].map((o) => o.value)`, &choices)
	if !slices.Contains(choices, "ratify") || !slices.Contains(choices, "overrule "+size) || slices.ContainsFunc(choices, func(c string) bool { return strings.HasSuffix(c, " "+veto) }) {
		t.Fatalf("the decisions offered with the veto dismissed: %v", choices)
	}
}

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
