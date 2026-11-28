package intakejourney

import (
	context "context"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	cdppage "github.com/chromedp/cdproto/page"
	chromedp "github.com/chromedp/chromedp"
	plan "github.com/kpenfound/osmia/internal/plan"
	seal "github.com/kpenfound/osmia/internal/seal"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestBrowserPageAnswersQuestionsContestsAndAmendments(t *testing.T) {
	t.Parallel()
	f := journey.NewPageFixture(t)
	p := browsertest.OpenBrowser(t)
	ctx := context.Background()
	live := f.S.Sole().Repository
	at := time.Now().UTC()
	header := func(schema, id string, actor trace.Actor) trace.Header {
		return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: 1, Project: fixturekit.Project, Workstream: fixturekit.Stream, At: at, Actor: actor, Cause: "planted"}
	}
	move := func(subject, id, to string, actor trace.Actor, docs ...trace.Document) {
		t.Helper()
		state, err := live.Workflow(fixturekit.Stream, subject)
		fixturekit.Must(t, err)
		tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: header("osmia.trace.transition", id, actor), Subject: subject, From: state.Value, To: to, Reason: "moved to " + to}}
		if len(docs) > 0 {
			_, err = live.RecordDocumentsWith(ctx, docs, tx)
		} else {
			_, err = live.Transact(ctx, tx)
		}
		fixturekit.Must(t, err)
	}

	ask := func(agent, thread, question string) trace.Question {
		t.Helper()
		fixturekit.Must(t, live.CreateThread(ctx, trace.Agent{Header: header("osmia.trace.agent", agent, intake.OwnerActor), Role: fixturekit.DemoRole, ThreadID: thread}))
		asked, err := live.Ask(ctx, agent, fixturekit.ClaimTurn(t, live, t.TempDir(), fixturekit.Stream, agent, thread, "build_"+agent, at), question, at)
		fixturekit.Must(t, err)
		return asked
	}
	first := ask(fixturekit.DemoAgent, fixturekit.DemoThread, "Where does upload state live?")
	second := ask("agent_mason_history", "thread_mason_history", "May I rewrite the branch history?")
	_, err := live.EscalateQuestions(ctx, trace.Manager, f.Manager, trace.EscalationRequest{Questions: []string{first.ID}, Rephrasing: "Where should upload state live?",
		Blocked: "The upload unit.", Options: []string{"Files", "A database"}, Recommendation: "Keep it in files."}, at)
	fixturekit.Must(t, err)
	_, err = live.EscalateQuestions(ctx, trace.Manager, f.Manager, trace.EscalationRequest{Questions: []string{second.ID}, Rephrasing: "May the mason rewrite the branch history?",
		Blocked: "The upload unit.", Recommendation: "Force-push the cleaned branch."}, at)
	fixturekit.Must(t, err)
	plantReport(t, live, fixturekit.Stream, "index", at)
	move(trace.UnitSubject("index"), "index-reviewing", intake.UnitReviewing, execution.ForemanActor)
	move(trace.UnitSubject("index"), "index-contested", intake.UnitContested, execution.ReviewerActor)
	journey.RaiseFixtureContest(t, live, fixturekit.Stream, "index")
	sealed, sealDoc, _, err := seal.Latest(live, fixturekit.Stream)
	fixturekit.Must(t, err)
	_, err = live.FileBudgetAmendment(ctx, fixturekit.Stream, trace.AmendmentRequest{Citations: []string{"spec#1"}, Change: "Raise the per-unit budget.", Reason: "The upload unit needs more turns.",
		Seal: sealed.Seal, SealRevision: sealDoc.Revision, SpecHash: sealed.SpecHash}, at)
	fixturekit.Must(t, err)
	packet := func(revision int, recommendation string) trace.Document {
		h := header("osmia.trace.document", "amendment-1-presented-packet", intake.ShedActor)
		h.Revision = revision
		return trace.Document{Header: h, Path: intake.AmendmentPacketPath("1"), Content: `{"round":1,"recommendation":"` + recommendation + `"}` + "\n"}
	}
	move(intake.AmendmentSubject("1"), "amendment-1-presented", intake.AmendmentPresented, intake.ShedActor, packet(1, "approve: no objection stands"))

	escalations := intakefixture.EntriesOf(t, f.C, api.InboxEscalation)
	contests := intakefixture.EntriesOf(t, f.C, api.InboxContested)
	amendments := intakefixture.EntriesOf(t, f.C, api.InboxAmendment)
	if len(escalations) != 2 || escalations[0].QuickReply != "Keep it in files." || escalations[1].QuickReply != "" || len(contests) != 1 || len(amendments) != 1 {
		t.Fatalf("planted inbox: %+v %+v %+v", escalations, contests, amendments)
	}
	accepted, written, contest, amendment := intakefixture.DecisionCard(escalations[0]), intakefixture.DecisionCard(escalations[1]), intakefixture.DecisionCard(contests[0]), intakefixture.DecisionCard(amendments[0])

	link := browsertest.NewLinkProxy(t, f.S.WebAddr())
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
			document.addEventListener('click', event => {
				if (!gate.closed || !event.target.closest('[data-kind="amendment"] [data-decision-choice]')) return;
				gate.closed = false;
				gate.waiting.splice(0).forEach(resolve => resolve());
			});
		})()`).Do(ctx)
		return err
	}))
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+link.Listener.Addr().String()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Eval(`window.notReloaded = true`, nil)
	p.AwaitText(`[data-select="`+string(fixturekit.Stream)+`"] [data-field=needs]`, "4")
	p.Await("nothing waits on the quiet workstream", `document.querySelector(`+browsertest.Quote(`[data-select="`+string(intakefixture.Quiet)+`"] [data-field=needs]`)+`) === null`)
	p.SelectWorkstream(fixturekit.Stream)
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
		contest + "[data-field=options]":          "Review the unit again",
		contest + "[data-field=pins]":             "unit index",
		amendment + "[data-field=question]":       "Change: Raise the per-unit budget. Reason: The upload unit needs more turns.",
		amendment + "[data-field=recommendation]": "approve: no objection stands",
		amendment + "[data-field=pins]":           "amendment 1, packet revision 1",
	} {
		p.AwaitText(selector, want)
	}
	var offered []string
	p.Eval(`[...document.querySelectorAll(`+browsertest.Quote(amendment+"[data-decision-choice]")+`)].map((b) => b.dataset.decisionChoice)`, &offered)
	if !slices.Equal(offered, amendments[0].Options) {
		t.Fatalf("the amendment's option buttons: %v, want %v", offered, amendments[0].Options)
	}
	var collapsed bool
	p.Eval(`document.querySelector(`+browsertest.Quote(accepted+"details.asked")+`)?.open === false`, &collapsed)
	if !collapsed {
		t.Fatal("the asked questions are not collapsed under the rephrasing")
	}
	var acceptable bool
	p.Eval(`document.querySelector(`+browsertest.Quote(written+"[data-field=accept]")+`) !== null`, &acceptable)
	if acceptable {
		t.Fatal("the page offers to accept a recommendation the API withholds a quick reply for")
	}

	ruling := func(id string) *trace.Ruling {
		t.Helper()
		questions, err := live.Questions(fixturekit.Stream)
		fixturekit.Must(t, err)
		i := slices.IndexFunc(questions, func(q trace.QuestionState) bool { return q.Asked.ID == id })
		if i < 0 {
			t.Fatalf("no question %s", id)
		}
		return questions[i].Ruling
	}

	p.Click(written + "button[type=submit]")
	p.AwaitText("#inbox-result", "Write an answer first.")
	if r := ruling(second.ID); r != nil {
		t.Fatalf("an empty answer was recorded: %+v", r)
	}
	p.TypeInto(written+"textarea", "Keep the history; add a fixup commit.")
	p.Click(written + "button[type=submit]")
	p.AwaitOutcome("Answered inbox entry 2.")
	p.AwaitGone("the written answer's entry", written)
	p.AwaitText(`[data-select="`+string(fixturekit.Stream)+`"] [data-field=needs]`, "3")
	if r := ruling(second.ID); r == nil || r.OwnerResponse != "Keep the history; add a fixup commit." || r.Actor != intake.OwnerActor {
		t.Fatalf("the written answer's ruling: %+v", r)
	}

	p.Click(accepted + "[data-field=option]:nth-of-type(2)")
	p.Await("the option in the answer", `document.querySelector(`+browsertest.Quote(accepted+"textarea")+`).value === 'A database'`)
	p.Click(accepted + "[data-field=accept]")
	p.AwaitOutcome("Accepted the recommendation on inbox entry 1.")
	p.AwaitGone("the accepted entry", accepted)
	if r := ruling(first.ID); r == nil || r.OwnerResponse != "Keep it in files." || r.Actor != intake.OwnerActor {
		t.Fatalf("the accepted ruling: %+v", r)
	}

	p.Click(contest + `[data-decision-choice="review"]`)
	p.AwaitText("#inbox-result", "Give a note for the ruling.")
	p.TypeInto(contest+"input[name=note]", "The reviewer's sandbox failed; review it again.")
	p.Click(contest + `[data-decision-choice="review"]`)
	p.AwaitOutcome("Ruled review on unit index.")
	p.AwaitGone("the contested unit", contest)
	if state, err := live.Workflow(fixturekit.Stream, trace.UnitSubject("index")); err != nil || state.Value != intake.UnitReviewing {
		t.Fatalf("the ruled unit: %+v %v", state, err)
	}
	transitions, err := trace.Read[trace.Transition](live, fixturekit.Stream)
	fixturekit.Must(t, err)
	ruled := slices.DeleteFunc(transitions, func(tr trace.Transition) bool { return tr.Subject != trace.UnitSubject("index") })
	if last := ruled[len(ruled)-1]; last.Actor != intake.OwnerActor || !strings.HasSuffix(last.Reason, "The reviewer's sandbox failed; review it again.") {
		t.Fatalf("the contested ruling: %+v", last)
	}

	gate := p.Gate("*" + api.Prefix + "/events")
	gate.Set(true)
	p.Eval(`window.inboxGate.closed = true`, nil)
	link.Cut()
	p.Await("the lost connection", `document.body.dataset.connection === 'lost'`)
	link.Restore()
	fixturekit.Must(t, live.RecordDocuments(ctx, []trace.Document{packet(2, "ratify: no objection stands")}))
	p.AwaitText(amendment+"[data-field=pins]", "packet revision 1")
	p.Click(amendment + `[data-decision-choice="` + intake.AmendmentReject + `"]`)
	p.AwaitText("#inbox-result", "packet revision 1 of amendment 1 is not the latest; revision 2 is presented")
	p.AwaitText(amendment+"[data-field=pins]", "packet revision 2")
	p.AwaitText(amendment+"[data-field=recommendation]", "approve: no objection stands")
	if out, err := f.C.Amendment(ctx, fixturekit.Stream, "1"); err != nil || out.State != intake.AmendmentPresented || out.Decision != nil {
		t.Fatalf("the refused decision: %+v %v", out, err)
	}
	gate.Set(false)
	p.Await("the live connection again", `document.body.dataset.connection === 'live'`)
	p.Click(amendment + `[data-decision-choice="` + intake.AmendmentReject + `"]`)
	p.AwaitOutcome("Decided reject on amendment 1.")
	p.AwaitGone("the decided amendment", amendment)
	if out, err := f.C.Amendment(ctx, fixturekit.Stream, "1"); err != nil || out.Decision == nil || out.Decision.Decision != intake.AmendmentReject || out.Decision.Packet != 2 {
		t.Fatalf("the decided amendment: %+v %v", out, err)
	}
	p.Await("the same document", `window.notReloaded === true`)
	decisions := 0
	for _, path := range p.Paths() {
		if path == api.Prefix+"/amendment/"+string(fixturekit.Stream)+"/1" {
			decisions++
		}
	}
	if decisions != 2 {
		t.Fatalf("the page sent %d decisions on the amendment, want the refused one and the accepted one", decisions)
	}
}

func TestBrowserPageDecidesARatificationPacket(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixtureWith(t, 2, 1, servicetest.ListenWeb, nil)
	t.Cleanup(func() { f.Stop(t) })
	p := browsertest.OpenBrowser(t)
	faults := &intakefixture.Faults{}
	veto := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	size := shed.ObjectionID(1, intake.CommitteeAgent(2), 1)
	f.Member(1, 1, 1, intakefixture.Objects(faults, shed.Charter, "spec#2", "charter#1"))
	f.Member(1, 2, 1, intakefixture.Objects(faults, shed.Size, "plan#resume", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Answers(faults, "Kept as it is.", veto, size))
	f.Script((intake.RoundInput{Round: 1, Redraft: true}).TurnID(1), map[string]string{plan.PlanPath: splitPlan}, nil)
	f.Member(2, 1, 1, intakefixture.Silent)
	f.Member(2, 2, 1, intakefixture.Concedes(faults, size))
	f.Script(intake.ReplyTurnID(2, 1), nil, nil)
	ws := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, ws, "concluded-1")
	faults.Check(t)
	f.ManagerDecides(t, ws, veto, shed.Escalate, "The veto cites the small-changes rule.")
	f.ManagerDecides(t, ws, size, shed.Escalate, "Splitting the unit is a judgement call.")
	var entries []api.InboxEntry
	intakefixture.Soon(t, "the ratification entry", func() bool {
		entries = f.InboxEntries(t, ws, api.InboxRatification)
		return len(entries) > 0
	})
	if len(entries) != 1 {
		t.Fatalf("ratification entries %+v", entries)
	}
	card := intakefixture.DecisionCard(entries[0])
	choices := func() []string {
		var out []string
		p.Eval(`[...document.querySelectorAll(`+browsertest.Quote(card+"[data-decision-choice]")+`)].map((o) => o.dataset.decisionChoice)`, &out)
		return out
	}

	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.S.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Eval(`window.notReloaded = true`, nil)
	p.SelectWorkstream(ws)
	p.AwaitText(card+"[data-field=kind]", "Ratification")
	p.AwaitText(card+"[data-field=question]", "Ratify spec.md revision 1 and plan.json revision 1? Debate ended after round 1")
	p.AwaitText(card+"[data-field=recommendation]", "do not ratify yet: ratification is blocked by 1 objection")
	p.AwaitText(card+"[data-field=options]", "Options: none until what blocks it is resolved")
	p.AwaitText(card+"[data-field=pins]", "spec revision 1, plan revision 1")
	p.AwaitText(card+`[data-objection="`+veto+`"] [data-field=standing]`, "blocking")
	p.AwaitText(card+`[data-objection="`+veto+`"]`, "charter by "+intake.CommitteeAgent(1)+" in round 1 on spec#2")
	p.AwaitText(card+`[data-objection="`+size+`"] [data-field=argument]`, "It does not hold.")
	p.AwaitText(card+`[data-objection="`+veto+`"] [data-field=escalation]`, "Chief of staff: The veto cites the small-changes rule.")
	if got := choices(); slices.Contains(got, "ratify") || !slices.Contains(got, "overrule "+veto) || !slices.Contains(got, "sustain "+size) || !slices.Contains(got, "redraft") {
		t.Fatalf("the decisions offered while objections block: %v", got)
	}

	p.TypeInto(card+"input[name=note]", "I accept the risk.")
	p.Click(card + `[data-decision-choice="overrule ` + veto + `"]`)
	p.AwaitOutcome("I accept the risk.")
	p.AwaitText(card+`[data-objection="`+veto+`"] [data-field=standing]`, "advice, overruled")
	p.TypeInto(card+"input[name=note]", "Split the resume unit.")
	p.Click(card + `[data-decision-choice="sustain ` + size + `"]`)
	p.AwaitText(card+`[data-objection="`+size+`"] [data-field=standing]`, "blocking, sustained")
	entries = f.InboxEntries(t, ws, api.InboxRatification)
	dissent := f.Dissent(t, ws)
	if len(entries) != 1 || len(entries[0].Options) != 0 || len(dissent) != 2 {
		t.Fatalf("after the dispositions: %+v %+v", entries, dissent)
	}
	for _, e := range dissent {
		if e.ID == veto && (e.Disposition != shed.Overruled || e.Note != "I accept the risk.") || e.ID == size && (e.Disposition != shed.Sustained || e.Note != "Split the resume unit.") {
			t.Fatalf("the dispositions %+v", dissent)
		}
	}

	p.Click(card + `[data-decision-choice="redraft"]`)
	p.AwaitText("#inbox-result", "Say what the redraft should change.")
	if asked, err := shed.Redrafts(f.Repository(), ws); err != nil || len(asked) != 0 {
		t.Fatalf("a redraft without a note was recorded: %+v %v", asked, err)
	}
	p.TypeInto(card+"input[name=note]", "Split the resume unit into what it addresses.")
	p.Click(card + `[data-decision-choice="redraft"]`)
	p.AwaitText(card+"[data-field=pins]", "spec revision 1, plan revision 2")
	p.AwaitText(card+"[data-field=recommendation]", "ratify: no objection stands")
	asked, err := shed.Redrafts(f.Repository(), ws)
	fixturekit.Must(t, err)
	if len(asked) != 1 || asked[0].Round != 1 || asked[0].Note != "Split the resume unit into what it addresses." {
		t.Fatalf("the recorded redraft %+v", asked)
	}

	p.Click(card + `[data-decision-choice="ratify"]`)
	p.AwaitOutcome("the owner ratified spec.md revision 1 and plan.json revision 2 after round 2")
	p.AwaitGone("the ratified packet", card)
	if record := f.Ratification(t, ws, 2); record.Revision != (shed.Pin{Spec: 1, Plan: 2}) || record.Round != 2 {
		t.Fatalf("the recorded ratification %+v", record)
	}
	if moves := f.OwnerMoves(t, ws); !slices.Equal(moves, []string{"overruled-1", "ruled-1", "redraft-1", "ratified-2"}) {
		t.Fatalf("owner subject went %v", moves)
	}
	p.Await("the same document", `window.notReloaded === true`)
	faults.Check(t)
}

func TestBrowserPageFoldsObjectionsTheManagerDismissed(t *testing.T) {
	t.Parallel()
	f := journey.NewDebateFixtureWith(t, 2, 1, servicetest.ListenWeb, nil)
	t.Cleanup(func() { f.Stop(t) })
	p := browsertest.OpenBrowser(t)
	faults := &intakefixture.Faults{}
	veto := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	size := shed.ObjectionID(1, intake.CommitteeAgent(2), 1)
	f.Member(1, 1, 1, intakefixture.Objects(faults, shed.Charter, "spec#2", "charter#1"))
	f.Member(1, 2, 1, intakefixture.Objects(faults, shed.Size, "plan#resume", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Answers(faults, "Kept as it is.", veto, size))
	ws := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, ws, "concluded-1")
	faults.Check(t)
	f.ManagerDecides(t, ws, veto, shed.Dismiss, "Each unit is one small change with its own test.")
	f.ManagerDecides(t, ws, size, shed.Escalate, "Splitting the unit is a judgement call.")
	var entries []api.InboxEntry
	intakefixture.Soon(t, "the ratification entry", func() bool {
		entries = f.InboxEntries(t, ws, api.InboxRatification)
		return len(entries) > 0
	})
	card := intakefixture.DecisionCard(entries[0])

	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.S.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(ws)
	p.AwaitText(card+"[data-field=kind]", "Ratification")
	p.AwaitText(card+`[data-objection="`+size+`"] [data-field=escalation]`, "Chief of staff: Splitting the unit is a judgement call.")
	p.AwaitText(card+"[data-field=dismissed] summary", "Dismissed by the chief of staff (1)")
	p.AwaitText(card+`[data-field=dismissed] [data-objection="`+veto+`"] [data-field=note]`, "Chief of staff: Each unit is one small change with its own test.")
	var choices []string
	p.Eval(`[...document.querySelectorAll(`+browsertest.Quote(card+"[data-decision-choice]")+`)].map((o) => o.dataset.decisionChoice)`, &choices)
	if !slices.Contains(choices, "ratify") || !slices.Contains(choices, "overrule "+size) || slices.ContainsFunc(choices, func(c string) bool { return strings.HasSuffix(c, " "+veto) }) {
		t.Fatalf("the decisions offered with the veto dismissed: %v", choices)
	}
}
