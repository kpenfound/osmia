package intakejourney

import (
	context "context"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	testing "testing"
	time "time"

	cdppage "github.com/chromedp/cdproto/page"
	chromedp "github.com/chromedp/chromedp"
	seal "github.com/kpenfound/osmia/internal/seal"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
)

func TestBrowserPageResetsEscalationContestedAndAmendmentFormsAfterSuccess(t *testing.T) {
	t.Parallel()
	p := browsertest.OpenBrowser(t)
	f := journey.NewPageFixture(t)
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

	fixturekit.Must(t, live.CreateThread(ctx, trace.Agent{Header: header("osmia.trace.agent", fixturekit.DemoAgent, intake.OwnerActor), Role: fixturekit.DemoRole, ThreadID: fixturekit.DemoThread}))
	asked, err := live.Ask(ctx, fixturekit.DemoAgent, fixturekit.ClaimTurn(t, live, t.TempDir(), fixturekit.Stream, fixturekit.DemoAgent, fixturekit.DemoThread, "build_"+fixturekit.DemoAgent, at), "Where does upload state live?", at)
	fixturekit.Must(t, err)
	_, err = live.EscalateQuestions(ctx, trace.ChiefOfStaff, f.Chief, trace.EscalationRequest{Questions: []string{asked.ID}, Rephrasing: "Where should upload state live?",
		Blocked: "The upload unit.", Recommendation: "Keep it in files."}, at)
	fixturekit.Must(t, err)
	plantReport(t, live, fixturekit.Stream, "index", at)
	move(trace.UnitSubject("index"), "index-reviewing", intake.UnitReviewing, service.TestForemanActor)
	move(trace.UnitSubject("index"), "index-contested-1", intake.UnitContested, execution.ReviewerActor)
	journey.RaiseFixtureContest(t, live, fixturekit.Stream, "index")
	sealed, sealDoc, _, err := seal.Latest(live, fixturekit.Stream)
	fixturekit.Must(t, err)
	_, err = live.FileBudgetAmendment(ctx, fixturekit.Stream, trace.AmendmentRequest{Citations: []string{"spec#1"}, Change: "Raise the per-unit budget.", Reason: "The upload unit needs more turns.",
		Seal: sealed.Seal, SealRevision: sealDoc.Revision, SpecHash: sealed.SpecHash}, at)
	fixturekit.Must(t, err)
	packet := trace.Document{Header: header("osmia.trace.document", "amendment-1-presented-packet", intake.ShedActor), Path: intake.AmendmentPacketPath("1"),
		Content: `{"round":1,"recommendation":"approve: no objection stands"}` + "\n"}
	move(intake.AmendmentSubject("1"), "amendment-1-presented", intake.AmendmentPresented, intake.ShedActor, packet)

	firstOf := func(kind string) api.InboxEntry {
		t.Helper()
		var entries []api.InboxEntry
		intakefixture.Soon(t, "the "+kind+" inbox entry", func() bool {
			entries = intakefixture.EntriesOf(t, f.C, kind)
			return len(entries) > 0
		})
		return entries[0]
	}
	escalation := intakefixture.DecisionCard(firstOf(api.InboxEscalation))
	contestedEntry := firstOf(api.InboxContested)
	contested := intakefixture.DecisionCard(contestedEntry)
	amendment := intakefixture.DecisionCard(firstOf(api.InboxAmendment))

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
						if (!body.entries.some(e => e.kind === 'contested' && e.workstream === ` + browsertest.Quote(string(fixturekit.Stream)) + ` && e.unit === 'index')) {
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
	p.Run(chromedp.EmulateViewport(1280, 800), chromedp.Navigate("http://"+f.S.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(fixturekit.Stream)
	p.AwaitText(escalation+"[data-field=kind]", "Question")

	p.TypeInto(escalation+"textarea", "State lives in the upload log.")
	p.Click(escalation + "button[type=submit]")
	p.AwaitOutcome("Answered inbox entry")
	p.AwaitCleared(escalation + "textarea")

	noteField := contested + `input[name=note]`
	p.TypeInto(noteField, "The reviewer's sandbox failed; review it again.")
	p.Click(contested + `[data-decision-choice="review"]`)
	p.AwaitOutcome("Ruled review on unit index.")
	p.AwaitCleared(noteField)
	p.AwaitGone("the ruled unit", contested)
	intakefixture.Soon(t, "the ruled contest to leave the inbox API", func() bool {
		for _, entry := range intakefixture.EntriesOf(t, f.C, api.InboxContested) {
			if entry.Workstream == fixturekit.Stream && entry.Unit == "index" {
				return false
			}
		}
		return true
	})
	p.Await("the page to process the ruled contest's removal", `window.ruledContestRead === true`)
	move(trace.UnitSubject("index"), "index-contested-2", intake.UnitContested, execution.ReviewerActor)
	journey.RaiseFixtureContest(t, live, fixturekit.Stream, "index")
	p.AwaitText(contested+"[data-field=kind]", "Contested unit")
	var note string
	p.Eval(`document.querySelector(`+browsertest.Quote(noteField)+`).value`, &note)
	if note != "" {
		t.Fatalf("the reused contested card note: %q", note)
	}

	amendmentNote := amendment + `input[name=note]`
	p.TypeInto(amendmentNote, "Keep the acknowledged-chunk contract.")
	p.Click(amendment + `[data-decision-choice="reject"]`)
	p.AwaitOutcome("Decided reject on amendment 1.")
	p.AwaitCleared(amendmentNote)
}

func TestBrowserPageResetsTheRatificationFormAfterEachSuccessfulDecision(t *testing.T) {
	t.Parallel()
	p := browsertest.OpenBrowser(t)
	f := journey.NewDebateFixtureWith(t, 2, 1, servicetest.ListenWeb, nil)
	t.Cleanup(func() { f.Stop(t) })
	faults := &intakefixture.Faults{}
	veto := shed.ObjectionID(1, intake.CommitteeAgent(1), 1)
	size := shed.ObjectionID(1, intake.CommitteeAgent(2), 1)
	f.Member(1, 1, 1, intakefixture.Objects(faults, shed.Charter, "spec#2", "charter#1"))
	f.Member(1, 2, 1, intakefixture.Objects(faults, shed.Size, "plan#resume", "spec#1"))
	f.Script(intake.ReplyTurnID(1, 1), nil, intakefixture.Answers(faults, "Kept as it is.", veto, size))
	ws := f.HandIn(t, "design", fixturekit.HandedDesign)
	f.AwaitShed(t, ws, "concluded-1")
	faults.Check(t)
	f.ChiefDecides(t, ws, veto, shed.Escalate, "The veto cites the small-changes rule.")
	f.ChiefDecides(t, ws, size, shed.Escalate, "Splitting the unit is a judgement call.")
	var entries []api.InboxEntry
	intakefixture.Soon(t, "the ratification entry", func() bool {
		entries = f.InboxEntries(t, ws, api.InboxRatification)
		return len(entries) > 0
	})
	card := intakefixture.DecisionCard(entries[0])
	noteField := card + `input[name=note]`

	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.S.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.SelectWorkstream(ws)
	p.AwaitText(card+"[data-field=kind]", "Ratification")

	p.TypeInto(noteField, "I accept the risk.")
	p.Click(card + `[data-decision-choice="overrule ` + veto + `"]`)
	p.AwaitOutcome("I accept the risk.")
	p.AwaitCleared(noteField)

	p.TypeInto(noteField, "Split the resume unit.")
	p.Click(card + `[data-decision-choice="sustain ` + size + `"]`)
	p.AwaitText(card+`[data-objection="`+size+`"] [data-field=standing]`, "blocking, sustained")
	p.AwaitCleared(noteField)
}
