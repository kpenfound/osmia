package service

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

// TestBrowserOwnerWorkflowDemonstration takes one workstream from a hand-in
// through the local API to a delivered pull request, with every owner
// decision on the way made on the embedded page over listen.web: the
// ratification, an escalated question, an amendment, a contested unit, a
// pause and its resume, and the delivery approval. The page is opened once
// and follows the workstream through its event stream. See
// docs/web-demonstration.md.
func TestBrowserOwnerWorkflowDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := newMasonFixture(t, 1, validPlan)
	t.Cleanup(func() { f.stop(t) })

	// The owner serves the page on a loopback web listener.
	f.stop(t)
	configFile, err := os.OpenFile(filepath.Join(f.opts.Config.Root, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString(listenWeb)
	must(t, err)
	must(t, configFile.Close())

	// Delivery goes to a local bare fork and a fake pull request host.
	var stream config.WorkstreamID
	home := filepath.Dir(f.clone)
	fork := filepath.Join(home, "remotes", "owner", "dagger.git")
	must(t, os.MkdirAll(filepath.Dir(fork), 0700))
	demoGit(t, home, "init", "--quiet", "--bare", fork)
	demoGit(t, home, "-C", f.clone, "remote", "add", "origin", fork)
	forkBranch := func() string {
		return strings.TrimSpace(demoGit(t, home, "-C", fork, "for-each-ref", "--format=%(objectname)", "refs/heads/"+featureBranch(stream)))
	}
	prs := &fakePulls{Fork: forkBranch}
	f.opts.PullRequests = prs
	// passed counts completed reconciliation passes, so the pause-while-
	// delivering check below waits for the scheduler to have demonstrably
	// had chances to publish, rather than sleeping a fixed duration.
	var passed atomic.Int64
	f.opts.schedulePassed = func() { passed.Add(1) }

	// The fake architect and committee draft and debate the amendment.
	f.script("amend-1-1", map[string]string{plan.SpecPath: amendedSpec}, nil)
	f.script("amend-1-round-1-"+committeeAgent(1)+"-1", nil, nil)
	f.script("amend-1-reply-1", nil, nil)

	problems := &demoProblems{}
	faults := &faults{}
	rulingTurn := amendmentRulingTurn("1")
	masons.Mu.Lock()
	masons.Response = map[string]string{masonTurnID("dedupe"): "I cannot complete this work."}
	masons.Play[rulingTurn] = reportDone("Uploads resume")
	for i := 1; i <= 3; i++ {
		masons.Play[fmt.Sprintf("%s-owner-revise-%d", masonAgent("dedupe"), i)] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
			if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/dedupe.go"), []byte("package trace\n// Skip acknowledged chunks.\n"), 0600); err != nil {
				return err
			}
			if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Acknowledged chunks are skipped"}); err != nil || !recorded {
				return fmt.Errorf("done refused: %q %v", reason, err)
			}
			return nil
		}
	}
	masons.Mu.Unlock()
	// The answer shows the spec must change: the mason asks again, and the
	// chief of staff routes that question to an amendment.
	masons.Chief.Route("2", []string{"spec#1"}, "Resume from a durable checkpoint", "Acknowledgements are not durable")
	f.answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, questions.AskTool, map[string]any{"question": "Acknowledgements are not durable, so resuming from the last one loses chunks. Should the spec say a durable checkpoint?"})
		return err
	})

	f.engine.Mu.Lock()
	// resume's mason asks question 1, and its answer turn asks question 2,
	// which the chief of staff routes to an amendment.
	f.engine.Turns[masonTurnID("resume")] = masons.Asking(faults, "", "1")
	f.engine.Turns[rulingTurn] = masons.Turn
	for i := 1; i <= 3; i++ {
		f.engine.Turns[fmt.Sprintf("%s-owner-revise-%d", masonAgent("dedupe"), i)] = masons.Turn
	}
	chief := f.engine.Turns["*"]
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		switch {
		case strings.HasPrefix(req.Name, "reviewer-") || strings.HasPrefix(req.Name, "reviewer_"):
			body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "summary": reviewSummary, "findings": []ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				problems.report("review %s: %s: %v", req.Name, body, err)
			}
		case strings.HasPrefix(req.Name, "final-"):
			body, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Resumable uploads", "findings": []any{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				problems.report("final report %s: %s: %v", req.Name, body, err)
			}
		default:
			return chief(ctx, req, turn, tools)
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.Mu.Unlock()
	f.start(t)

	// entry waits until the inbox lists one entry of kind for the workstream
	// and returns it.
	entry := func(kind string) InboxEntry {
		t.Helper()
		var out InboxEntry
		eventually(t, "the inbox never listed a "+kind+" entry", func() bool {
			entries := f.inboxEntries(t, stream, kind)
			if len(entries) == 1 {
				out = entries[0]
			}
			return len(entries) == 1
		})
		return out
	}
	// ownerDocument returns the latest revision of the document id, which the
	// owner recorded.
	ownerDocument := func(id string) trace.Document {
		t.Helper()
		docs := f.documents(t, stream, id)
		if len(docs) == 0 {
			t.Fatalf("no document %s", id)
		}
		doc := docs[len(docs)-1]
		if doc.Actor != ownerActor {
			t.Fatalf("document %s was recorded by %+v", id, doc.Actor)
		}
		return doc
	}

	// The owner opens the page at phone width before anything is handed in,
	// and keeps it open to the end.
	p := openBrowser(t)
	p.Run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.Await("the live connection", `document.body.dataset.connection === 'live'`)
	p.Eval(`window.notReloaded = true`, nil)

	// 1. The design is handed in through the API, the way osmia handin
	// sends it; the workstream and its packet appear on the page, and the
	// owner ratifies it there.
	stream = f.shedAs(t, "web-release")
	ws := `[data-workstream="` + string(stream) + `"] `
	p.SelectWorkstream(stream)
	p.AwaitText(ws+"[data-field=state]", InShedState)
	card := decisionCard(entry(InboxRatification))
	p.AwaitText(card+"[data-field=recommendation]", "ratify: no objection stands")
	p.Choose(card+"select", "ratify")
	p.Click(card + "button[type=submit]")
	p.AwaitText("#inbox-result", "the owner ratified spec.md revision 1 and plan.json revision 1")
	p.AwaitGone("the ratified packet", card)
	if record := f.ratification(t, stream, 1); record.Revision != (shed.Pin{Spec: 1, Plan: 1}) || record.Round != 1 {
		t.Fatalf("the recorded ratification %+v", record)
	}
	if moves := f.ownerMoves(t, stream); !slices.Equal(moves, []string{"skipped", "ratified-1"}) {
		t.Fatalf("owner subject went %v", moves)
	}
	f.awaitFeature(t, stream, BuildingState)
	p.AwaitText(ws+"[data-field=state]", BuildingState)

	// 2. resume's mason asks a question, the chief of staff escalates it,
	// and the owner writes the answer on the page.
	card = decisionCard(entry(InboxEscalation))
	p.AwaitText(card+"[data-field=question]", "Is the store durable across restarts?")
	p.TypeInto(card+"textarea", ownerRuling)
	p.Click(card + "button[type=submit]")
	p.AwaitText("#inbox-result", "Answered inbox entry")
	p.AwaitGone("the answered question", card)
	// f.question's Ruling is the newest revision: by the time the page has
	// cleared the card, the fake chief of staff's relay may already have
	// recorded revision 2 over it. Find the owner's own revision (1) rather
	// than whichever is latest.
	rulings, err := trace.Read[trace.Ruling](f.repository(), stream)
	must(t, err)
	var r *trace.Ruling
	for i, ruling := range rulings {
		if ruling.QuestionID == "1" && ruling.Revision == 1 {
			r = &rulings[i]
		}
	}
	if r == nil || r.OwnerResponse != ownerRuling || r.Actor != ownerActor {
		t.Fatalf("the owner's ruling on question 1: %+v", r)
	}

	// 3. The answer turn asks to amend spec#1. The architect drafts, the
	// committee debates one round, and the owner rejects the packet on the
	// page.
	f.awaitAmendment(t, stream, amendmentPresented)
	card = decisionCard(entry(InboxAmendment))
	p.AwaitText(card+"[data-field=question]", "Change: Resume from a durable checkpoint")
	p.Choose(card+"select", AmendmentReject)
	p.TypeInto(card+"input[name=note]", "Keep the acknowledged-chunk contract.")
	p.Click(card + "button[type=submit]")
	p.AwaitText("#inbox-result", "Decided reject on amendment 1.")
	p.AwaitGone("the decided amendment", card)
	f.awaitAmendment(t, stream, amendmentRuled)
	var decided AmendmentDecision
	must(t, json.Unmarshal([]byte(ownerDocument(amendmentDecisionID("1")).Content), &decided))
	if decided.Decision != AmendmentReject || decided.Note != "Keep the acknowledged-chunk contract." || decided.Packet < 1 {
		t.Fatalf("the recorded amendment decision %+v", decided)
	}

	// The ruling reaches resume's mason, which reports; resume is reviewed
	// and lands, and the page shows it merged.
	f.awaitMerged(t, stream, "resume")
	p.AwaitText(ws+`[data-kind=transition][data-unit=resume][data-to=merged]`, "Unit resume: approved → merged")

	// 4. dedupe's mason gives up and the unit is contested; the owner rules
	// revise with a note on the page.
	f.awaitUnit(t, stream, "dedupe", UnitContested)
	raiseFixtureContest(t, f.repository(), stream, "dedupe")
	card = decisionCard(entry(InboxContested))
	p.AwaitText(card+"[data-field=options]", "revise")
	p.Choose(card+"select", "revise")
	p.TypeInto(card+"input[name=note]", "Skip chunks the store acknowledged.")
	p.Click(card + "button[type=submit]")
	p.AwaitText("#inbox-result", "Ruled revise on unit dedupe.")
	p.AwaitGone("the contested unit", card)
	transitions := slices.DeleteFunc(f.transitions(t, stream), func(tr trace.Transition) bool {
		return tr.Subject != trace.UnitSubject("dedupe") || tr.From != UnitContested
	})
	if len(transitions) != 1 || transitions[0].Actor != ownerActor || transitions[0].To != UnitImplementing || !strings.HasSuffix(transitions[0].Reason, "Skip chunks the store acknowledged.") {
		t.Fatalf("the contested ruling %+v", transitions)
	}
	f.awaitMerged(t, stream, "dedupe")

	// 5. Final review presents the delivery. The owner pauses the workstream
	// on the page before approving it, so the approval waits.
	f.awaitFeature(t, stream, AssembledState)
	card = decisionCard(entry(InboxDelivery))
	presented, err := f.c.Delivery(ctx, stream)
	must(t, err)
	target := "workstream:" + string(stream)
	shownPause := `[data-pause="` + target + `"] `
	p.Await("the workstream as a pause target", `[...document.querySelectorAll('#pause-form select[name=target] option')].some((o) => o.value === `+quote(target)+`)`)
	// The workstream's menu opens the pauses with the workstream chosen.
	p.Click("#workstream-menu-button")
	p.Click(`[data-workstream-action="pause"]`)
	p.Await("the workstream chosen to pause", `document.getElementById('pause-popover').matches(':popover-open') && document.querySelector('#pause-form select[name=target]').value === `+quote(target))
	p.Choose("#pause-form select[name=mode]", "soft")
	p.TypeInto("#pause-form input[name=reason]", "Hold the delivery until I have read it.")
	p.Click("#pause-form button[type=submit]")
	p.AwaitText("#pause-result", "paused (soft)")
	p.AwaitText(shownPause+"[data-field=source]", "the owner")
	rt, err := f.c.Runtime(ctx)
	must(t, err)
	paused := runtime.Target{Scope: "workstream", Project: f.project, Workstream: stream}
	if pause, ok := pauseOf(rt, paused); !ok || pause.Source != runtime.PauseOwner || pause.Mode != "soft" || pause.Reason != "Hold the delivery until I have read it." {
		t.Fatalf("the owner's pause %+v %v", pause, ok)
	}

	// 6. The owner approves the drafted description on the page. Publication
	// waits for the pause, and runs once the owner resumes the workstream.
	p.ClosePopovers()
	p.AwaitText(card+"[data-field=question]", "Deliver Resumable uploads?")
	p.Await("the drafted description", `document.querySelector(`+quote(card+`textarea[name="description"]`)+`).value === `+quote(presented.Draft))
	p.Await("Approve enabled", `!document.querySelector(`+quote(card+"button[type=submit]")+`).disabled`)
	p.Click(card + "button[type=submit]")
	p.AwaitText("#inbox-result", "Approved the delivery of final review 1")
	p.AwaitGone("the approved delivery", card)
	var approval DeliveryApproval
	must(t, json.Unmarshal([]byte(ownerDocument(deliveryDocument).Content), &approval))
	if approval.Description != presented.Draft || approval.DraftHash != presented.DraftHash || approval.Commit != presented.Report.Commit || approval.ReviewRevision != presented.ReviewRevision {
		t.Fatalf("the recorded delivery approval %+v, presented %+v", approval, presented)
	}
	base := passed.Load()
	eventually(t, "reconciliation never got two passes to try publishing while the workstream was paused", func() bool {
		return passed.Load()-base >= 2
	})
	if feature, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != AssembledState {
		t.Fatalf("the paused workstream went on to %+v %v", feature, err)
	}
	prs.Mu.Lock()
	opened := len(prs.Prs)
	prs.Mu.Unlock()
	if opened != 0 {
		t.Fatalf("pull requests opened while the workstream was paused: %d, want 0", opened)
	}
	p.OpenPauses()
	p.Click(shownPause + "[data-field=resume]")
	p.AwaitText("#pause-result", "resumed")
	p.AwaitGone("the resumed pause", shownPause)
	rt, err = f.c.Runtime(ctx)
	must(t, err)
	if _, ok := pauseOf(rt, paused); ok {
		t.Fatal("the workstream is still paused after the owner resumed it")
	}
	f.awaitFeature(t, stream, DeliveredState)
	p.AwaitText(ws+"[data-field=state]", DeliveredState)

	problems.check(t)
	faults.Check(t)
	masons.Check(t)
	prs.Mu.Lock()
	if len(prs.Prs) != 1 || prs.Creates != 1 || prs.Prs[0].Body != presented.Draft {
		t.Fatalf("pull requests %+v", prs.Prs)
	}
	prs.Mu.Unlock()
	inbox, err := f.c.Inbox(ctx)
	must(t, err)
	if len(inbox.Entries) != 0 {
		t.Fatalf("inbox after delivery: %+v", inbox.Entries)
	}
	p.Await("an empty inbox", `document.querySelector('[data-decision]') === null`)
	// Every change reached the page through its event stream: it was never
	// reloaded.
	p.Await("the same document", `window.notReloaded === true`)
}
