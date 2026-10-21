package intakejourney

import (
	context "context"
	cdppage "github.com/chromedp/cdproto/page"
	testing "testing"
	time "time"

	chromedp "github.com/chromedp/chromedp"
	seal "github.com/kpenfound/osmia/internal/seal"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
)

// The escalation, contested-unit and amendment cards clear their decision
// back to its default value as soon as their submission succeeds. The
// contested card is reused when its unit is contested again, and still
// shows no value from the ruling before it.
func TestBrowserPageResetsEscalationContestedAndAmendmentFormsAfterSuccess(t *testing.T) {
	p := openBrowser(t)
	f := newPageFixture(t)
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

	// An escalated question, a contested unit and a presented amendment are
	// all open at once.
	must(t, live.CreateThread(ctx, trace.Agent{Header: header("osmia.trace.agent", demoAgent, ownerActor), Role: demoRole, ThreadID: demoThread}))
	asked, err := live.Ask(ctx, demoAgent, claimTurn(t, live, t.TempDir(), stream, demoAgent, demoThread, "build_"+demoAgent, at), "Where does upload state live?", at)
	must(t, err)
	_, err = live.EscalateQuestions(ctx, trace.ChiefOfStaff, f.chief, trace.EscalationRequest{Questions: []string{asked.ID}, Rephrasing: "Where should upload state live?",
		Blocked: "The upload unit.", Recommendation: "Keep it in files."}, at)
	must(t, err)
	move(trace.UnitSubject("index"), "index-reviewing", UnitReviewing, foremanActor)
	move(trace.UnitSubject("index"), "index-contested-1", UnitContested, reviewerActor)
	raiseFixtureContest(t, live, stream, "index")
	sealed, sealDoc, _, err := seal.Latest(live, stream)
	must(t, err)
	_, err = live.FileBudgetAmendment(ctx, stream, trace.AmendmentRequest{Citations: []string{"spec#1"}, Change: "Raise the per-unit budget.", Reason: "The upload unit needs more turns.",
		Seal: sealed.Seal, SealRevision: sealDoc.Revision, SpecHash: sealed.SpecHash}, at)
	must(t, err)
	// Filing the amendment only records its request; the inbox shows it once
	// the shed presents its packet, as the shed debate does in practice.
	packet := trace.Document{Header: header("osmia.trace.document", "amendment-1-presented-packet", shedActor), Path: amendmentPacketPath("1"),
		Content: `{"round":1,"recommendation":"approve: no objection stands"}` + "\n"}
	move(amendmentSubject("1"), "amendment-1-presented", amendmentPresented, shedActor, packet)

	// Each entry is read through the same inbox the page polls, so the test
	// waits for it to show up rather than assuming it is there already.
	firstOf := func(kind string) InboxEntry {
		t.Helper()
		var entries []InboxEntry
		soon(t, "the "+kind+" inbox entry", func() bool {
			entries = entriesOf(t, f.c, kind)
			return len(entries) > 0
		})
		return entries[0]
	}
	escalation := decisionCard(firstOf(InboxEscalation))
	contestedEntry := firstOf(InboxContested)
	contested := decisionCard(contestedEntry)
	amendment := decisionCard(firstOf(InboxAmendment))

	// Observe the page consuming an inbox that no longer lists this contest.
	// pruneInbox maps the entries during render; the microtask runs after
	// that synchronous render has cleared the card's dismissal.
	p.Run(chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := cdppage.AddScriptToEvaluateOnNewDocument(`(() => {
			window.ruledContestRead = false;
			const send = window.fetch;
			window.fetch = async (...args) => {
				const response = await send(...args);
				if (String(args[0]).endsWith('/inbox') && response.ok) {
					const json = response.json.bind(response);
					response.json = async () => {
						const body = await json();
						if (!body.entries.some(e => e.kind === 'contested' && e.workstream === ` + quote(string(stream)) + ` && e.unit === 'index')) {
							const map = body.entries.map.bind(body.entries);
							body.entries.map = (...args) => {
								const result = map(...args);
								queueMicrotask(() => { window.ruledContestRead = true; });
								return result;
							};
						}
						return body;
					};
				}
				return response;
			};
		})()`).Do(ctx)
		return err
	}))
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(stream)
	p.AwaitText(escalation+"[data-field=kind]", "Question")

	// The escalation's answer clears once it is sent. Answering it resolves
	// the entry, so the page may remove its card, taking the answer with it,
	// before the test next reads it; either outcome leaves no stale answer
	// showing.
	p.TypeInto(escalation+"textarea", "State lives in the upload log.")
	p.Click(escalation + "button[type=submit]")
	p.AwaitText("#inbox-result", "Answered inbox entry")
	p.AwaitCleared(escalation + "textarea")

	// The contested unit's ruling clears once it is recorded, and the same
	// card, reused when the unit is contested again, still shows none of it.
	decisionField, noteField := contested+"select", contested+`input[name=note]`
	p.Choose(decisionField, "review")
	p.TypeInto(noteField, "The reviewer's sandbox failed; review it again.")
	p.Click(contested + "button[type=submit]")
	p.AwaitText("#inbox-result", "Ruled review on unit index.")
	p.AwaitCleared(decisionField)
	p.AwaitCleared(noteField)
	p.AwaitGone("the ruled unit", contested)
	// Optimistic removal alone does not clear the dismissal. The server
	// must resolve this contest and the page must read that resolved inbox
	// before another contest can reuse the same card identity.
	soon(t, "the ruled contest to leave the inbox API", func() bool {
		for _, entry := range entriesOf(t, f.c, InboxContested) {
			if entry.Workstream == stream && entry.Unit == "index" {
				return false
			}
		}
		return true
	})
	p.Await("the page to process the ruled contest's removal", `window.ruledContestRead === true`)
	move(trace.UnitSubject("index"), "index-contested-2", UnitContested, reviewerActor)
	raiseFixtureContest(t, live, stream, "index")
	p.AwaitText(contested+"[data-field=kind]", "Contested unit")
	var decision, note string
	p.Eval(`document.querySelector(`+quote(decisionField)+`).value`, &decision)
	p.Eval(`document.querySelector(`+quote(noteField)+`).value`, &note)
	if decision != "" || note != "" {
		t.Fatalf("the reused contested card: decision %q, note %q", decision, note)
	}

	// The amendment's decision clears once it is recorded.
	amendmentDecision, amendmentNote := amendment+"select", amendment+`input[name=note]`
	p.Choose(amendmentDecision, AmendmentReject)
	p.TypeInto(amendmentNote, "Keep the acknowledged-chunk contract.")
	p.Click(amendment + "button[type=submit]")
	p.AwaitText("#inbox-result", "Decided reject on amendment 1.")
	p.AwaitCleared(amendmentDecision)
	p.AwaitCleared(amendmentNote)
}

// The ratification card may be decided more than once as objections are
// disposed of; it clears its decision and note back to their defaults as
// soon as each decision succeeds, so deciding the next objection on the same
// card never shows a value from the decision before it.
func TestBrowserPageResetsTheRatificationFormAfterEachSuccessfulDecision(t *testing.T) {
	p := openBrowser(t)
	f := newDebateFixtureWith(t, 2, 1, listenWeb, nil)
	t.Cleanup(func() { f.stop(t) })
	faults := &faults{}
	veto := shed.ObjectionID(1, committeeAgent(1), 1)
	size := shed.ObjectionID(1, committeeAgent(2), 1)
	f.member(1, 1, 1, objects(faults, shed.Charter, "spec#2", "charter#1"))
	f.member(1, 2, 1, objects(faults, shed.Size, "plan#resume", "spec#1"))
	f.script(replyTurnID(1, 1), nil, answers(faults, "Kept as it is.", veto, size))
	ws := f.handIn(t, "design", handedDesign)
	f.awaitShed(t, ws, "concluded-1")
	faults.Check(t)
	f.chiefDecides(t, ws, veto, shed.Escalate, "The veto cites the small-changes rule.")
	f.chiefDecides(t, ws, size, shed.Escalate, "Splitting the unit is a judgement call.")
	var entries []InboxEntry
	soon(t, "the ratification entry", func() bool {
		entries = f.inboxEntries(t, ws, InboxRatification)
		return len(entries) > 0
	})
	card := decisionCard(entries[0])
	decisionField, noteField := card+"select", card+`input[name=note]`
	fields := func() (decision, note string) {
		p.Eval(`document.querySelector(`+quote(decisionField)+`).value`, &decision)
		p.Eval(`document.querySelector(`+quote(noteField)+`).value`, &note)
		return
	}

	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(ws)
	p.AwaitText(card+"[data-field=kind]", "Ratification")

	p.Choose(decisionField, "overrule "+veto)
	p.TypeInto(noteField, "I accept the risk.")
	p.Click(card + "button[type=submit]")
	p.AwaitText("#inbox-result", "I accept the risk.")
	if decision, note := fields(); decision != "" || note != "" {
		t.Fatalf("the ratification card after the first decision: decision %q, note %q", decision, note)
	}

	// The same card, reused for the next objection, still shows no value
	// from the decision before it.
	p.Choose(decisionField, "sustain "+size)
	p.TypeInto(noteField, "Split the resume unit.")
	p.Click(card + "button[type=submit]")
	p.AwaitText(card+`[data-objection="`+size+`"] [data-field=standing]`, "blocking, sustained")
	if decision, note := fields(); decision != "" || note != "" {
		t.Fatalf("the ratification card after the second decision: decision %q, note %q", decision, note)
	}
}
