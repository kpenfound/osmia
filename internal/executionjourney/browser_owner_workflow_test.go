package executionjourney

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	atomic "sync/atomic"
	testing "testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/browsertest"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	chromedp "github.com/chromedp/chromedp"
	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	plan "github.com/kpenfound/osmia/internal/plan"
	questions "github.com/kpenfound/osmia/internal/questions"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	shed "github.com/kpenfound/osmia/internal/shed"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBrowserOwnerWorkflowDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := journey.NewMasonFixture(t, 1, fixturekit.ValidPlan)
	t.Cleanup(func() { f.Stop(t) })

	f.Stop(t)
	configFile, err := os.OpenFile(filepath.Join(f.Opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	fixturekit.Must(t, err)
	_, err = configFile.WriteString(journey.ListenWeb)
	fixturekit.Must(t, err)
	fixturekit.Must(t, configFile.Close())

	var stream config.WorkstreamID
	home := filepath.Dir(f.Clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	fixturekit.Must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	fixturekit.DemoGit(t, home, "init", "--quiet", "--bare", fork)
	fixturekit.DemoGit(t, home, "-C", f.Clone, "remote", "add", "origin", fork)
	forkBranch := func() string {
		return strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", fork, "for-each-ref", "--format=%(objectname)", "refs/heads/"+intake.FeatureBranch(stream)))
	}
	prs := &fixturekit.FakePulls{Fork: forkBranch, SyncHead: true}
	f.Opts.PullRequests = prs

	var passed atomic.Int64
	f.Opts.SchedulePassed = func() { passed.Add(1) }

	f.Script("amend-1-1", map[string]string{plan.SpecPath: intakefixture.AmendedSpec}, nil)
	f.Script("amend-1-round-1-"+intake.CommitteeAgent(1)+"-1", nil, nil)
	f.Script("amend-1-reply-1", nil, nil)

	problems := &demoProblems{}
	faults := &intakefixture.Faults{}
	rulingTurn := intake.AmendmentRulingTurn("1")
	masons.Mu.Lock()
	masons.Response = map[string]string{execution.MasonTurnID("dedupe"): "I cannot complete this work."}
	masons.Play[rulingTurn] = intakefixture.ReportDone("Uploads resume")
	for i := 1; i <= 3; i++ {
		masons.Play[fmt.Sprintf("%s-owner-revise-%d", intake.MasonAgent("dedupe"), i)] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
			if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/dedupe.go"), []byte("package trace\n// Skip acknowledged chunks.\n"), 0600); err != nil {
				return err
			}
			if recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Acknowledged chunks are skipped"}); err != nil || !recorded {
				return fmt.Errorf("done refused: %q %v", reason, err)
			}
			return nil
		}
	}
	masons.Mu.Unlock()

	masons.Chief.Route("2", []string{"spec#1"}, "Resume from a durable checkpoint", "Acknowledgements are not durable")
	f.Answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": "Acknowledgements are not durable, so resuming from the last one loses chunks. Should the spec say a durable checkpoint?"})
		return err
	})

	f.Engine.Mu.Lock()

	f.Engine.Turns[execution.MasonTurnID("resume")] = masons.Asking(faults, "", "1")
	f.Engine.Turns[rulingTurn] = masons.Turn
	for i := 1; i <= 3; i++ {
		f.Engine.Turns[fmt.Sprintf("%s-owner-revise-%d", intake.MasonAgent("dedupe"), i)] = masons.Turn
	}
	chief := f.Engine.Turns["*"]
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		switch {
		case strings.HasPrefix(req.Name, "reviewer-") || strings.HasPrefix(req.Name, "reviewer_"):
			body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "satisfactory", "summary": fixturekit.ReviewSummary, "findings": []api.ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				problems.report("review %s: %s: %v", req.Name, body, err)
			}
		case strings.HasPrefix(req.Name, "final-"):
			body, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "Resumable uploads", "findings": []any{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				problems.report("final report %s: %s: %v", req.Name, body, err)
			}
		default:
			return chief(ctx, req, turn, tools)
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.Engine.Mu.Unlock()
	f.Start(t)

	entry := func(kind string) api.InboxEntry {
		t.Helper()
		var out api.InboxEntry
		intakefixture.Eventually(t, "the inbox never listed a "+kind+" entry", func() bool {
			entries := f.InboxEntries(t, stream, kind)
			if len(entries) == 1 {
				out = entries[0]
			}
			return len(entries) == 1
		})
		return out
	}

	ownerDocument := func(id string) trace.Document {
		t.Helper()
		docs := f.Documents(t, stream, id)
		if len(docs) == 0 {
			t.Fatalf("no document %s", id)
		}
		doc := docs[len(docs)-1]
		if doc.Actor != intake.OwnerActor {
			t.Fatalf("document %s was recorded by %+v", id, doc.Actor)
		}
		return doc
	}

	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.S.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Eval(`window.notReloaded = true`, nil)

	stream = f.ShedAs(t, "web-release")
	ws := `[data-workstream="` + string(stream) + `"] `
	p.SelectWorkstream(stream)
	p.AwaitText(ws+"[data-field=state]", intake.InShedState)
	card := intakefixture.DecisionCard(entry(api.InboxRatification))
	p.AwaitText(card+"[data-field=recommendation]", "ratify: no objection stands")
	p.Click(card + `[data-decision-choice="ratify"]`)
	p.AwaitOutcome("the owner ratified spec.md revision 1 and plan.json revision 1")
	p.AwaitGone("the ratified packet", card)
	if record := f.Ratification(t, stream, 1); record.Revision != (shed.Pin{Spec: 1, Plan: 1}) || record.Round != 1 {
		t.Fatalf("the recorded ratification %+v", record)
	}
	if moves := f.OwnerMoves(t, stream); !slices.Equal(moves, []string{"skipped", "ratified-1"}) {
		t.Fatalf("owner subject went %v", moves)
	}
	f.AwaitFeature(t, stream, intake.BuildingState)
	p.AwaitText(ws+"[data-field=state]", intake.BuildingState)

	card = intakefixture.DecisionCard(entry(api.InboxEscalation))
	p.AwaitText(card+"[data-field=question]", "Is the store durable across restarts?")
	p.TypeInto(card+"textarea", intakefixture.OwnerRuling)
	p.Click(card + "button[type=submit]")
	p.AwaitOutcome("Answered inbox entry")
	p.AwaitGone("the answered question", card)

	rulings, err := trace.Read[trace.Ruling](f.Repository(), stream)
	fixturekit.Must(t, err)
	var r *trace.Ruling
	for i, ruling := range rulings {
		if ruling.QuestionID == "1" && ruling.Revision == 1 {
			r = &rulings[i]
		}
	}
	if r == nil || r.OwnerResponse != intakefixture.OwnerRuling || r.Actor != intake.OwnerActor {
		t.Fatalf("the owner's ruling on question 1: %+v", r)
	}

	f.AwaitAmendment(t, stream, intake.AmendmentPresented)
	card = intakefixture.DecisionCard(entry(api.InboxAmendment))
	p.AwaitText(card+"[data-field=question]", "Change: Resume from a durable checkpoint")
	p.TypeInto(card+"input[name=note]", "Keep the acknowledged-chunk contract.")
	p.Click(card + `[data-decision-choice="` + intake.AmendmentReject + `"]`)
	p.AwaitOutcome("Decided reject on amendment 1.")
	p.AwaitGone("the decided amendment", card)
	f.AwaitAmendment(t, stream, intake.AmendmentRuled)
	var decided api.AmendmentDecision
	fixturekit.Must(t, json.Unmarshal([]byte(ownerDocument(intake.AmendmentDecisionID("1")).Content), &decided))
	if decided.Decision != intake.AmendmentReject || decided.Note != "Keep the acknowledged-chunk contract." || decided.Packet < 1 {
		t.Fatalf("the recorded amendment decision %+v", decided)
	}

	f.AwaitMerged(t, stream, "resume")
	p.OpenActivity()
	p.AwaitText(ws+`[data-kind=transition][data-unit=resume][data-to=merged]`, "Unit resume: approved → merged")
	p.OpenConversation()

	f.AwaitUnit(t, stream, "dedupe", intake.UnitContested)
	journey.RaiseFixtureContest(t, f.Repository(), stream, "dedupe")
	card = intakefixture.DecisionCard(entry(api.InboxContested))
	p.AwaitText(card+"[data-field=options]", "Revise the unit")
	p.TypeInto(card+"input[name=note]", "Skip chunks the store acknowledged.")
	p.Click(card + `[data-decision-choice="revise"]`)
	p.AwaitOutcome("Ruled revise on unit dedupe.")
	p.AwaitGone("the contested unit", card)
	transitions := slices.DeleteFunc(f.Transitions(t, stream), func(tr trace.Transition) bool {
		return tr.Subject != trace.UnitSubject("dedupe") || tr.From != intake.UnitContested
	})
	if len(transitions) != 1 || transitions[0].Actor != intake.OwnerActor || transitions[0].To != intake.UnitImplementing || !strings.HasSuffix(transitions[0].Reason, "Skip chunks the store acknowledged.") {
		t.Fatalf("the contested ruling %+v", transitions)
	}
	f.AwaitMerged(t, stream, "dedupe")

	f.AwaitFeature(t, stream, intake.AssembledState)
	card = intakefixture.DecisionCard(entry(api.InboxDelivery))
	presented, err := f.C.Delivery(ctx, stream)
	fixturekit.Must(t, err)
	target := "workstream:" + string(stream)
	shownPause := `[data-pause="` + target + `"] `
	p.Await("the workstream as a pause target", `[...document.querySelectorAll('#pause-form select[name=target] option')].some((o) => o.value === `+browsertest.Quote(target)+`)`)

	p.Click("#workstream-menu-button")
	p.Click(`[data-workstream-action="pause"]`)
	p.Await("the workstream chosen to pause", `document.getElementById('pause-popover').matches(':popover-open') && document.querySelector('#pause-form select[name=target]').value === `+browsertest.Quote(target))
	p.Choose("#pause-form select[name=mode]", "soft")
	p.TypeInto("#pause-form input[name=reason]", "Hold the delivery until I have read it.")
	p.Click("#pause-form button[type=submit]")
	p.AwaitText("#pause-result", "paused (soft)")
	p.AwaitText(shownPause+"[data-field=source]", "the owner")
	rt, err := f.C.Runtime(ctx)
	fixturekit.Must(t, err)
	paused := runtime.Target{Scope: "workstream", Project: f.Project, Workstream: stream}
	if pause, ok := fixturekit.PauseOf(rt, paused); !ok || pause.Source != runtime.PauseOwner || pause.Mode != "soft" || pause.Reason != "Hold the delivery until I have read it." {
		t.Fatalf("the owner's pause %+v %v", pause, ok)
	}

	p.ClosePopovers()
	p.AwaitText(card+"[data-field=question]", "Mark ready for review: Resumable uploads?")
	p.Await("the drafted description", `document.querySelector(`+browsertest.Quote(card+"[data-field=preview] [data-field=description]")+`)?.textContent === `+browsertest.Quote(presented.Draft))
	p.Await("the approval enabled", `!document.querySelector(`+browsertest.Quote(card+"button[type=submit]")+`).disabled`)
	p.Click(card + "button[type=submit]")
	p.AwaitOutcome("Approved marking final review 1")
	p.AwaitGone("the approved delivery", card)
	var approval api.DeliveryApproval
	fixturekit.Must(t, json.Unmarshal([]byte(ownerDocument(delivery.DeliveryDocument).Content), &approval))
	if approval.Description != presented.Draft || approval.DraftHash != presented.DraftHash || approval.Commit != presented.Report.Commit || approval.ReviewRevision != presented.ReviewRevision {
		t.Fatalf("the recorded delivery approval %+v, presented %+v", approval, presented)
	}
	base := passed.Load()
	intakefixture.Eventually(t, "reconciliation never got two passes to try publishing while the workstream was paused", func() bool {
		return passed.Load()-base >= 2
	})
	if feature, err := f.Repository().Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != intake.AssembledState {
		t.Fatalf("the paused workstream went on to %+v %v", feature, err)
	}

	prs.Mu.Lock()
	opened, drafts, readied := len(prs.Prs), 0, prs.Readies
	for _, pr := range prs.Prs {
		if pr.Draft {
			drafts++
		}
	}
	prs.Mu.Unlock()
	if opened != 1 || drafts != 1 || readied != 0 {
		t.Fatalf("pull requests while the workstream was paused: %d, %d of them drafts, %d marked ready; want the one draft", opened, drafts, readied)
	}
	p.OpenPauses()
	p.Click(shownPause + "[data-field=resume]")
	p.AwaitText("#pause-result", "resumed")
	p.AwaitGone("the resumed pause", shownPause)
	rt, err = f.C.Runtime(ctx)
	fixturekit.Must(t, err)
	if _, ok := fixturekit.PauseOf(rt, paused); ok {
		t.Fatal("the workstream is still paused after the owner resumed it")
	}
	f.AwaitFeature(t, stream, intake.DeliveredState)
	p.AwaitText(ws+"[data-field=state]", intake.DeliveredState)

	problems.check(t)
	faults.Check(t)
	masons.Check(t)
	prs.Mu.Lock()
	if len(prs.Prs) != 1 || prs.Creates != 1 || prs.Prs[0].Body != presented.Draft {
		t.Fatalf("pull requests %+v", prs.Prs)
	}
	prs.Mu.Unlock()
	inbox, err := f.C.Inbox(ctx)
	fixturekit.Must(t, err)
	if len(inbox.Entries) != 0 {
		t.Fatalf("inbox after delivery: %+v", inbox.Entries)
	}
	p.Await("an empty inbox", `document.querySelector('[data-decision]') === null`)

	p.Await("the same document", `window.notReloaded === true`)
}
