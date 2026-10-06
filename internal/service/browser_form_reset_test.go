package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// awaitCleared waits until the field selector names shows no value, either
// because its card cleared it after a successful submission or because the
// resolved entry it answered already took the card out of the inbox; it
// fails with the page's text if neither happens.
func (p *page) awaitCleared(selector string) {
	p.t.Helper()
	p.await(selector+" cleared", `(function(){var e=document.querySelector(`+quote(selector)+`);return e===null||e.value==='';})()`)
}

// The new project form clears back to its default values once a
// registration succeeds, so opening it again shows none of what was
// submitted before; a failed registration leaves the owner's entry in place
// and shows the refusal beside it.
func TestBrowserPageResetsTheProjectAddFormAfterSuccessAndKeepsItAfterFailure(t *testing.T) {
	p := openBrowser(t)
	opts, clone := projectFixture(t)
	path := filepath.Join(opts.Config.Root, "config.toml")
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(data, []byte(listenWeb)...), 0600))
	s, _ := start(t, opts)

	values := func() (name, upstream, fork, cloneField, base string) {
		p.eval(`document.querySelector('#project-add [name=name]').value`, &name)
		p.eval(`document.querySelector('#project-add [name=upstream]').value`, &upstream)
		p.eval(`document.querySelector('#project-add [name=fork]').value`, &fork)
		p.eval(`document.querySelector('#project-add [name=clone]').value`, &cloneField)
		p.eval(`document.querySelector('#project-add [name=base_branch]').value`, &base)
		return
	}
	fill := func(name, upstream, fork string) {
		t.Helper()
		p.typeInto("#project-add [name=name]", name)
		p.typeInto("#project-add [name=upstream]", upstream)
		p.typeInto("#project-add [name=fork]", fork)
		p.typeInto("#project-add [name=clone]", clone)
	}

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.openView("project-add")

	// A successful registration clears the form; it shows its defaults, none
	// of what the owner just submitted.
	fill("Documents", "owner/docs", "owner/docs-fork")
	p.click("#project-add [type=submit]")
	p.awaitText("#project-add .result", "Registered Documents")
	if name, upstream, fork, cloneField, base := values(); name != "" || upstream != "" || fork != "" || cloneField != "" || base != "main" {
		t.Fatalf("the new project form after a successful registration: name %q, upstream %q, fork %q, clone %q, base %q", name, upstream, fork, cloneField, base)
	}

	// Registering a different project with the same clone fails, because the
	// clone already belongs to the first registration; the owner's entry
	// stands and the refusal shows.
	fill("Notes", "owner/notes", "")
	p.click("#project-add [type=submit]")
	p.awaitText("#project-add .result", "already owns this registration or clone")
	p.await("an error outcome", `document.getElementById('project-add').querySelector('.result').dataset.outcome === 'error'`)
	if name, upstream, fork, cloneField, _ := values(); name != "Notes" || upstream != "owner/notes" || fork != "" || cloneField != clone {
		t.Fatalf("the new project form after a failed registration: name %q, upstream %q, fork %q, clone %q", name, upstream, fork, cloneField)
	}
}

// The debate-and-abandonment form, which also archives the work it abandons,
// clears back to its default values once an action succeeds, so working on
// it for the next workstream never shows what the owner submitted before; a
// refused action leaves the owner's entry in place and shows the refusal.
func TestBrowserPageResetsTheArchiveFormAfterSuccessAndKeepsItAfterFailure(t *testing.T) {
	p := openBrowser(t)
	f := newPageFixture(t)

	fields := func() (action, note string, archive bool) {
		p.eval(`document.querySelector('#workstream-action [name=action]').value`, &action)
		p.eval(`document.querySelector('#workstream-action [name=note]').value`, &note)
		p.eval(`document.querySelector('#workstream-action [name=archive]').checked`, &archive)
		return
	}

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.selectWorkstream(quiet)
	p.click("[data-tab=documents]")
	p.await("the documents tab", `!document.querySelector('[data-panel=documents]').hidden`)

	// Abandoning and archiving quiet, with a reason, succeeds; the shared
	// form clears back to its defaults once it does.
	p.click("#workstream-menu-button")
	p.click(`[data-workstream-action="abandon-archive"]`)
	p.await("the abandonment chosen", `document.querySelector('#workstream-action [name=action]').value === 'abandon' && document.querySelector('#workstream-action [name=archive]').checked`)
	p.typeInto("#workstream-action [name=note]", "Superseded by the upload design.")
	p.click("#workstream-action [type=submit]")
	p.awaitText("#workstream-action .result", "Workstream abandoned and archived.")
	p.await("the next workstream shown", `document.getElementById('workstream-head').dataset.workstream === `+quote(string(stream)))
	if action, note, archive := fields(); action != "object" || note != "" || archive {
		t.Fatalf("the debate-and-abandonment form after a successful action: action %q, note %q, archive %v", action, note, archive)
	}

	// Abandoning quiet again is refused, because it already is; the owner's
	// entry stands and the refusal shows.
	p.click("#archived summary")
	p.click(`#archived-list [data-select="` + string(quiet) + `"]`)
	p.await("quiet shown", `document.getElementById('workstream-head').dataset.workstream === `+quote(string(quiet)))
	p.setValue("#workstream-action [name=action]", "abandon")
	p.eval(`document.querySelector('#workstream-action [name=archive]').checked = true`, nil)
	p.typeInto("#workstream-action [name=note]", "Trying again.")
	p.click("#workstream-action [type=submit]")
	p.awaitText("#workstream-action .result", "cannot be abandoned")
	if action, note, archive := fields(); action != "abandon" || note != "Trying again." || !archive {
		t.Fatalf("the debate-and-abandonment form after a refused action: action %q, note %q, archive %v", action, note, archive)
	}
}

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
	contested := decisionCard(firstOf(InboxContested))
	amendment := decisionCard(firstOf(InboxAmendment))

	p.run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.selectWorkstream(stream)
	p.awaitText(escalation+"[data-field=kind]", "Question")

	// The escalation's answer clears once it is sent. Answering it resolves
	// the entry, so the page may remove its card, taking the answer with it,
	// before the test next reads it; either outcome leaves no stale answer
	// showing.
	p.typeInto(escalation+"textarea", "State lives in the upload log.")
	p.click(escalation + "button[type=submit]")
	p.awaitText("#inbox-result", "Answered inbox entry")
	p.awaitCleared(escalation + "textarea")

	// The contested unit's ruling clears once it is recorded, and the same
	// card, reused when the unit is contested again, still shows none of it.
	decisionField, noteField := contested+"select", contested+`input[name=note]`
	p.choose(decisionField, "review")
	p.typeInto(noteField, "The reviewer's sandbox failed; review it again.")
	p.click(contested + "button[type=submit]")
	p.awaitText("#inbox-result", "Ruled review on unit index.")
	p.awaitCleared(decisionField)
	p.awaitCleared(noteField)
	p.awaitGone("the ruled unit", contested)
	move(trace.UnitSubject("index"), "index-contested-2", UnitContested, reviewerActor)
	p.awaitText(contested+"[data-field=kind]", "Contested unit")
	var decision, note string
	p.eval(`document.querySelector(`+quote(decisionField)+`).value`, &decision)
	p.eval(`document.querySelector(`+quote(noteField)+`).value`, &note)
	if decision != "" || note != "" {
		t.Fatalf("the reused contested card: decision %q, note %q", decision, note)
	}

	// The amendment's decision clears once it is recorded.
	amendmentDecision, amendmentNote := amendment+"select", amendment+`input[name=note]`
	p.choose(amendmentDecision, AmendmentReject)
	p.typeInto(amendmentNote, "Keep the acknowledged-chunk contract.")
	p.click(amendment + "button[type=submit]")
	p.awaitText("#inbox-result", "Decided reject on amendment 1.")
	p.awaitCleared(amendmentDecision)
	p.awaitCleared(amendmentNote)
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
	faults.check(t)
	var entries []InboxEntry
	soon(t, "the ratification entry", func() bool {
		entries = f.inboxEntries(t, ws, InboxRatification)
		return len(entries) > 0
	})
	card := decisionCard(entries[0])
	decisionField, noteField := card+"select", card+`input[name=note]`
	fields := func() (decision, note string) {
		p.eval(`document.querySelector(`+quote(decisionField)+`).value`, &decision)
		p.eval(`document.querySelector(`+quote(noteField)+`).value`, &note)
		return
	}

	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.selectWorkstream(ws)
	p.awaitText(card+"[data-field=kind]", "Ratification")

	p.choose(decisionField, "overrule "+veto)
	p.typeInto(noteField, "I accept the risk.")
	p.click(card + "button[type=submit]")
	p.awaitText("#inbox-result", "I accept the risk.")
	if decision, note := fields(); decision != "" || note != "" {
		t.Fatalf("the ratification card after the first decision: decision %q, note %q", decision, note)
	}

	// The same card, reused for the next objection, still shows no value
	// from the decision before it.
	p.choose(decisionField, "sustain "+size)
	p.typeInto(noteField, "Split the resume unit.")
	p.click(card + "button[type=submit]")
	p.awaitText(card+`[data-objection="`+size+`"] [data-field=standing]`, "blocking, sustained")
	if decision, note := fields(); decision != "" || note != "" {
		t.Fatalf("the ratification card after the second decision: decision %q, note %q", decision, note)
	}
}

// The delivery card's drafted description and commit messages clear back to
// blank as soon as the approval they carry is recorded.
func TestBrowserPageResetsTheDeliveryFormAfterSuccess(t *testing.T) {
	p := openBrowser(t)
	f, ws, repository, _ := deliveryFixtureWith(t, listenWeb)
	must(t, repository.Close())
	f.start(t)
	t.Cleanup(func() { f.stop(t) })
	entries := entriesOf(t, f.c, InboxDelivery)
	if len(entries) != 1 {
		t.Fatalf("delivery entries %+v", entries)
	}
	card := decisionCard(entries[0])
	description := card + `textarea[name="description"]`

	p.run(chromedp.Navigate("http://" + f.s.WebAddr() + "/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.selectWorkstream(ws)
	p.awaitText(card+"[data-field=kind]", "Delivery")
	p.await("the drafted description", `document.querySelector(`+quote(description)+`).value !== ''`)
	p.click(card + "button[type=submit]")
	p.awaitText("#inbox-result", "Approved the delivery of final review 1")
	// Approving delivers the workstream, so the card may also leave the
	// inbox before the test next reads it, taking the drafted description
	// with it; either outcome leaves no stale description showing.
	p.awaitCleared(description)
}
