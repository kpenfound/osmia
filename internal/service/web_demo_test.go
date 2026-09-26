package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// TestBrowserM5WebPageDemonstration takes one workstream from a hand-in
// through the local API to a delivered pull request, with every owner
// decision on the way made on the embedded page over listen.web: the
// ratification, an escalated question, an amendment, a contested unit, a
// pause and its resume, and the delivery approval. The page is opened once
// and follows the workstream through its event stream. See
// docs/m5-web-page.md.
func TestBrowserM5WebPageDemonstration(t *testing.T) {
	p := openBrowser(t)
	ctx := context.Background()
	f, masons := newMasonFixture(t, 1, validPlan)
	defer func() { f.stop(t) }()

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
	prs := &fakePulls{fork: forkBranch}
	f.opts.PullRequests = prs

	// The fake architect and committee draft and debate the amendment.
	f.script("amend-1-1", map[string]string{plan.SpecPath: amendedSpec}, nil)
	f.script("amend-1-round-1-"+committeeAgent(1)+"-1", nil, nil)
	f.script("amend-1-reply-1", nil, nil)

	problems := &demoProblems{}
	faults := &faults{}
	rulingTurn := amendmentRulingTurn("1")
	masons.mu.Lock()
	masons.response = map[string]string{masonTurnID("dedupe"): "I cannot complete this work."}
	masons.play[rulingTurn] = reportDone("Uploads resume")
	for i := 1; i <= 3; i++ {
		masons.play[fmt.Sprintf("%s-owner-revise-%d", masonAgent("dedupe"), i)] = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
			if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/dedupe.go"), []byte("package trace\n// Skip acknowledged chunks.\n"), 0600); err != nil {
				return err
			}
			if recorded, reason, err := done(ctx, tools, map[string]any{"outcome": "Acknowledged chunks are skipped", "criteria": []any{criterionArgs(dedupeReport)}}); err != nil || !recorded {
				return fmt.Errorf("done refused: %q %v", reason, err)
			}
			return nil
		}
	}
	masons.mu.Unlock()
	f.answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		_, err := callTool(ctx, tools, questions.AmendTool, map[string]any{"citations": []string{"spec#1"}, "change": "Resume from a durable checkpoint", "reason": "Acknowledgements are not durable"})
		return err
	})

	f.engine.mu.Lock()
	// resume's mason asks question 1, and its answer turn asks for an
	// amendment.
	f.engine.turns[masonTurnID("resume")] = masons.asking(faults, "", "1")
	f.engine.turns[rulingTurn] = masons.turn
	for i := 1; i <= 3; i++ {
		f.engine.turns[fmt.Sprintf("%s-owner-revise-%d", masonAgent("dedupe"), i)] = masons.turn
	}
	chief := f.engine.turns["*"]
	f.engine.turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		switch {
		case strings.HasPrefix(req.Name, "reviewer-") || strings.HasPrefix(req.Name, "reviewer_"):
			criterion := "spec#2"
			if strings.HasPrefix(req.Name, reviewerAgent("resume")) {
				criterion = "spec#1"
			}
			body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "satisfactory", "evidence": []ReviewEvidence{{Criterion: criterion, Evidence: "The candidate contains the planned proof"}}, "findings": []ReviewFinding{}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				problems.report("review %s: %s: %v", req.Name, body, err)
			}
		case strings.HasPrefix(req.Name, "final-"):
			body, err := callTool(ctx, tools, FinalReportTool, map[string]any{"summary": "Resumable uploads", "criteria": []any{
				map[string]any{"criterion": "spec#1", "evidence": "resume landing and TestResume"},
				map[string]any{"criterion": "spec#2", "evidence": "dedupe landing skips acknowledged chunks"},
			}})
			if err != nil || !strings.Contains(body, `"recorded":true`) {
				problems.report("final report %s: %s: %v", req.Name, body, err)
			}
		default:
			return chief(ctx, req, turn, tools)
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.engine.mu.Unlock()
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
	p.run(chromedp.EmulateViewport(390, 844, chromedp.EmulateScale(3), chromedp.EmulateMobile), chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	p.await("the live connection", `document.body.dataset.connection === 'live'`)
	p.eval(`window.notReloaded = true`, nil)

	// 1. The design is handed in through the API, the way osmia handin
	// sends it; the workstream and its packet appear on the page, and the
	// owner ratifies it there.
	stream = f.shedAs(t, "web-release")
	ws := `[data-workstream="` + string(stream) + `"] `
	p.awaitText(ws+"[data-field=state]", InShedState)
	card := decisionCard(entry(InboxRatification))
	p.awaitText(card+"[data-field=recommendation]", "ratify: no objection stands")
	p.choose(card+"select", "ratify")
	p.click(card + "button[type=submit]")
	p.awaitText("#inbox-result", "the owner ratified spec.md revision 1 and plan.json revision 1")
	p.awaitGone("the ratified packet", card)
	if record := f.ratification(t, stream, 1); record.Revision != (shed.Pin{Spec: 1, Plan: 1}) || record.Round != 1 {
		t.Fatalf("the recorded ratification %+v", record)
	}
	if moves := f.ownerMoves(t, stream); !slices.Equal(moves, []string{"skipped", "ratified-1"}) {
		t.Fatalf("owner subject went %v", moves)
	}
	f.awaitFeature(t, stream, BuildingState)
	p.awaitText(ws+"[data-field=state]", BuildingState)

	// 2. resume's mason asks a question, the chief of staff escalates it,
	// and the owner writes the answer on the page.
	card = decisionCard(entry(InboxEscalation))
	p.awaitText(card+"[data-field=question]", "Is the store durable across restarts?")
	p.typeInto(card+"textarea", ownerRuling)
	p.click(card + "button[type=submit]")
	p.awaitText("#inbox-result", "Answered inbox entry")
	p.awaitGone("the answered question", card)
	if r := f.question(t, stream, "1").Ruling; r == nil || r.OwnerResponse != ownerRuling || r.Actor != ownerActor {
		t.Fatalf("the owner's ruling on question 1: %+v", r)
	}

	// 3. The answer turn asks to amend spec#1. The architect drafts, the
	// committee debates one round, and the owner rejects the packet on the
	// page.
	f.awaitAmendment(t, stream, amendmentPresented)
	card = decisionCard(entry(InboxAmendment))
	p.awaitText(card+"[data-field=question]", "Change: Resume from a durable checkpoint")
	p.choose(card+"select", AmendmentReject)
	p.typeInto(card+"input[name=note]", "Keep the acknowledged-chunk contract.")
	p.click(card + "button[type=submit]")
	p.awaitText("#inbox-result", "Decided reject on amendment 1.")
	p.awaitGone("the decided amendment", card)
	f.awaitAmendment(t, stream, amendmentRuled)
	var decided AmendmentDecision
	must(t, json.Unmarshal([]byte(ownerDocument(amendmentDecisionID("1")).Content), &decided))
	if decided.Decision != AmendmentReject || decided.Note != "Keep the acknowledged-chunk contract." || decided.Packet < 1 {
		t.Fatalf("the recorded amendment decision %+v", decided)
	}

	// The ruling reaches resume's mason, which reports; resume is reviewed
	// and lands, and the page shows it merged.
	f.awaitMerged(t, stream, "resume")
	p.awaitText(ws+"[data-state=merged]", "resume")

	// 4. dedupe's mason gives up and the unit is contested; the owner rules
	// revise with a note on the page.
	f.awaitUnit(t, stream, "dedupe", UnitContested)
	card = decisionCard(entry(InboxContested))
	p.awaitText(card+"[data-field=options]", "revise")
	p.choose(card+"select", "revise")
	p.typeInto(card+"input[name=note]", "Skip chunks the store acknowledged.")
	p.click(card + "button[type=submit]")
	p.awaitText("#inbox-result", "Ruled revise on unit dedupe.")
	p.awaitGone("the contested unit", card)
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
	p.await("the workstream as a pause target", `[...document.querySelectorAll('#pause-form select[name=target] option')].some((o) => o.value === `+quote(target)+`)`)
	p.choose("#pause-form select[name=target]", target)
	p.choose("#pause-form select[name=mode]", "soft")
	p.typeInto("#pause-form input[name=reason]", "Hold the delivery until I have read it.")
	p.click("#pause-form button[type=submit]")
	p.awaitText("#pause-result", "paused (soft)")
	p.awaitText(shownPause+"[data-field=source]", "the owner")
	rt, err := f.c.Runtime(ctx)
	must(t, err)
	paused := runtime.Target{Scope: "workstream", Project: f.project, Workstream: stream}
	if pause, ok := pauseOf(rt, paused); !ok || pause.Source != runtime.PauseOwner || pause.Mode != "soft" || pause.Reason != "Hold the delivery until I have read it." {
		t.Fatalf("the owner's pause %+v %v", pause, ok)
	}

	// 6. The owner approves the drafted description on the page. Publication
	// waits for the pause, and runs once the owner resumes the workstream.
	p.awaitText(card+"[data-field=question]", "Deliver Resumable uploads?")
	p.await("the drafted description", `document.querySelector(`+quote(card+"textarea")+`).value === `+quote(presented.Draft))
	p.await("Approve enabled", `!document.querySelector(`+quote(card+"button[type=submit]")+`).disabled`)
	p.click(card + "button[type=submit]")
	p.awaitText("#inbox-result", "Approved the delivery of final review 1")
	p.awaitGone("the approved delivery", card)
	var approval DeliveryApproval
	must(t, json.Unmarshal([]byte(ownerDocument(deliveryDocument).Content), &approval))
	if approval.Description != presented.Draft || approval.DraftHash != presented.DraftHash || approval.Commit != presented.Report.Commit || approval.ReviewRevision != presented.ReviewRevision {
		t.Fatalf("the recorded delivery approval %+v, presented %+v", approval, presented)
	}
	settle()
	if feature, err := f.repository().Workflow(stream, trace.FeatureSubject); err != nil || feature.Value != AssembledState {
		t.Fatalf("the paused workstream went on to %+v %v", feature, err)
	}
	prs.mu.Lock()
	opened := len(prs.prs)
	prs.mu.Unlock()
	if opened != 0 {
		t.Fatal("a pull request was opened while the workstream was paused")
	}
	p.click(shownPause + "[data-field=resume]")
	p.awaitText("#pause-result", "resumed")
	p.awaitGone("the resumed pause", shownPause)
	rt, err = f.c.Runtime(ctx)
	must(t, err)
	if _, ok := pauseOf(rt, paused); ok {
		t.Fatal("the workstream is still paused after the owner resumed it")
	}
	f.awaitFeature(t, stream, DeliveredState)
	p.awaitText(ws+"[data-field=state]", DeliveredState)

	problems.check(t)
	faults.check(t)
	masons.check(t)
	prs.mu.Lock()
	if len(prs.prs) != 1 || prs.creates != 1 || prs.prs[0].Body != presented.Draft {
		t.Fatalf("pull requests %+v", prs.prs)
	}
	prs.mu.Unlock()
	inbox, err := f.c.Inbox(ctx)
	must(t, err)
	if len(inbox.Entries) != 0 {
		t.Fatalf("inbox after delivery: %+v", inbox.Entries)
	}
	p.await("an empty inbox", `document.querySelector('[data-decision]') === null`)
	// Every change reached the page through its event stream: it was never
	// reloaded.
	p.await("the same document", `window.notReloaded === true`)
}
