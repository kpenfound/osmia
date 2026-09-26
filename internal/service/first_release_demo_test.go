package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// notifiedKind is the kind a notification body names: the Kind line of an
// owner decision, or the budget pause.
func notifiedKind(body string) string {
	if strings.HasPrefix(body, "Osmia paused dispatch: the daily budget is reached.\n") {
		return NotifyBudgetPause
	}
	for line := range strings.SplitSeq(body, "\n") {
		if kind, ok := strings.CutPrefix(line, "Kind: "); ok {
			return kind
		}
	}
	return ""
}

// TestM5FirstReleaseDemonstration takes one workstream from hand-in to a
// delivered pull request through the local API while notify.webhook is set.
// Every owner decision on the way, and the daily budget pause, reaches the
// fake webhook exactly once, across a restart while a sent notification is
// open and another while one is still pending. See docs/m5-first-release.md.
func TestM5FirstReleaseDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := newMasonFixture(t, 1, validPlan)
	defer func() { f.stop(t) }()

	// The owner allows USD 1.00 a day and sets a webhook. A failed post is
	// retried after two seconds, then four, eight and sixteen.
	f.stop(t)
	hook := newFakeWebhook(t).records(f.opts)
	configPath := filepath.Join(f.opts.Config.Root, "config.toml")
	configFile, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = configFile.WriteString("[budget]\nper_day = \"1.00\"\n")
	must(t, errors.Join(err, configFile.Close()))
	setWebhook(t, f.opts, hook.hook())
	f.opts.Location = time.UTC
	f.opts.NotifyRetry = 2 * time.Second

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
	p := &faults{}
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
	// resume's mason asks question 1; its answer turn asks for an amendment,
	// and the turn that brings the owner's ruling spends past the budget.
	f.engine.turns[masonTurnID("resume")] = masons.asking(p, "", "1")
	f.engine.turns[rulingTurn] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		result, err := masons.turn(ctx, req, verified, tools)
		if result != nil {
			result.CostUSD, result.CostKnown = 1.25, true
		}
		return result, err
	}
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

	// notified waits until the webhook has accepted the notification of kind
	// and returns it.
	notified := func(kind string) string {
		t.Helper()
		var body string
		eventually(t, "the webhook never accepted a "+kind+" notification", func() bool {
			for _, b := range hook.accepted() {
				if notifiedKind(b) == kind {
					body = b
					return true
				}
			}
			return false
		})
		return body
	}
	names := func(body, kind string) {
		t.Helper()
		if !strings.Contains(body, "\nProject: "+string(f.project)+"\n") || !strings.Contains(body, "\nWorkstream: "+string(stream)+"\n") || !strings.Contains(body, "\nKind: "+kind+"\n") {
			t.Fatalf("the %s notification does not name its project, workstream and kind:\n%s", kind, body)
		}
	}

	// 1. Hand-in and ratification.
	stream = f.shedAs(t, "first-release")
	names(notified(InboxRatification), InboxRatification)
	f.ratifiedBuild(t, stream)

	// 2. resume's question is escalated; the owner answers it.
	eventually(t, "question 1 was never escalated", func() bool {
		asked, err := f.repository().Questions(stream)
		must(t, err)
		return slices.ContainsFunc(asked, func(q trace.QuestionState) bool { return q.Asked.ID == "1" && q.State == trace.QuestionEscalated })
	})
	names(notified(InboxEscalation), InboxEscalation)
	// The service restarts while the escalation is open and already sent; it
	// is not posted again.
	f.stop(t)
	sent := len(hook.received())
	f.start(t)
	settle()
	if since := hook.received()[sent:]; len(since) != 0 {
		t.Fatalf("posts after the restart with the escalation open: %q", since)
	}
	f.rule(t, "1")

	// 3. The answer turn asks to amend spec#1; the owner rejects the packet.
	f.awaitAmendment(t, stream, amendmentPresented)
	names(notified(InboxAmendment), InboxAmendment)
	view, err := f.c.Amendment(ctx, stream, "1")
	must(t, err)
	_, err = f.c.DecideAmendment(ctx, stream, "1", AmendmentDecisionRequest{Decision: AmendmentReject, Packet: view.Revision, Note: "Keep the acknowledged-chunk contract."})
	must(t, err)
	f.awaitAmendment(t, stream, amendmentRuled)

	// 4. The ruling turn spends USD 1.25 of the USD 1.00 budget; the factory
	// pauses, and the owner resumes it once notified.
	eventually(t, "the daily budget never paused the factory", func() bool {
		pause, ok := factoryPauseOf(t, f.c)
		return ok && pause.Source == runtime.PauseDailyBudget
	})
	if body := notified(NotifyBudgetPause); !strings.Contains(body, "\nProject: "+string(f.project)+"\n") || !strings.Contains(body, "\nSpend: USD 1.25 or more\nLimit: USD 1.00\n") {
		t.Fatalf("budget pause notification:\n%s", body)
	}
	mutation(t, f.c, "DELETE", "pause", ClearPauseRequest(factoryTarget))
	f.awaitMerged(t, stream, "resume")

	// 5. dedupe's mason gives up while the webhook is failing. The service
	// stops with the contested notification pending and posts it once after
	// it starts again; nothing already sent is posted again.
	hook.respond(http.StatusServiceUnavailable)
	f.awaitUnit(t, stream, "dedupe", UnitContested)
	eventually(t, "the contested notification was never attempted", func() bool {
		return slices.ContainsFunc(hook.received(), func(b string) bool { return notifiedKind(b) == InboxContested })
	})
	f.stop(t)
	before := len(hook.received())
	var pending []*notification
	for _, rec := range readLedger(t, f.opts).Notifications {
		if rec.State == notificationPending {
			pending = append(pending, rec)
		}
	}
	if len(pending) != 1 || pending[0].Kind != InboxContested || pending[0].Workstream != string(stream) {
		t.Fatalf("pending across the restart: %+v", pending)
	}
	hook.respond(http.StatusOK)
	f.start(t)
	names(notified(InboxContested), InboxContested)
	if since := hook.received()[before:]; len(since) != 1 || notifiedKind(since[0]) != InboxContested {
		t.Fatalf("posts after the restart: %q", since)
	}
	if _, err := f.c.RuleContested(ctx, stream, "dedupe", "revise", "Skip chunks the store acknowledged."); err != nil {
		t.Fatal(err)
	}
	f.awaitMerged(t, stream, "dedupe")

	// 6. Final review presents the delivery; the owner approves it and the
	// service opens the pull request.
	f.awaitFeature(t, stream, AssembledState)
	names(notified(InboxDelivery), InboxDelivery)
	presented, err := f.c.Delivery(ctx, stream)
	must(t, err)
	_, err = f.c.ApproveDelivery(ctx, stream, DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash})
	must(t, err)
	f.awaitFeature(t, stream, DeliveredState)
	problems.check(t)
	p.check(t)
	masons.check(t)
	prs.mu.Lock()
	if len(prs.prs) != 1 || prs.creates != 1 || prs.prs[0].Body != presented.Draft {
		t.Fatalf("pull requests %+v", prs.prs)
	}
	prs.mu.Unlock()

	// Exactly one notification per owner decision and one for the pause.
	kinds := []string{InboxRatification, InboxEscalation, InboxAmendment, NotifyBudgetPause, InboxContested, InboxDelivery}
	var got []string
	for _, b := range hook.accepted() {
		got = append(got, notifiedKind(b))
	}
	if !slices.Equal(got, kinds) {
		t.Fatalf("accepted notifications %v, want %v", got, kinds)
	}
	for _, b := range hook.received() {
		if k := notifiedKind(b); !slices.Contains(kinds, k) {
			t.Fatalf("unexpected post:\n%s", b)
		}
	}
	for key, rec := range readLedger(t, f.opts).Notifications {
		if rec.State != notificationSent {
			t.Fatalf("notification %s is %s", key, rec.State)
		}
	}
	inbox, err := f.c.Inbox(ctx)
	must(t, err)
	if len(inbox.Entries) != 0 {
		t.Fatalf("inbox after delivery: %+v", inbox.Entries)
	}
}
