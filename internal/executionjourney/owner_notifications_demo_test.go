package executionjourney

import (
	context "context"
	errors "errors"
	fmt "fmt"
	maps "maps"
	http "net/http"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	agent "github.com/kpenfound/agentcore/agent"
	config "github.com/kpenfound/osmia/internal/config"
	plan "github.com/kpenfound/osmia/internal/plan"
	questions "github.com/kpenfound/osmia/internal/questions"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	trace "github.com/kpenfound/osmia/internal/trace"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func ledgerModTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	fixturekit.Must(t, err)
	return info.ModTime()
}

func notifiedKind(body string) string {
	if strings.HasPrefix(body, "Osmia paused dispatch: the daily budget is reached.\n") {
		return service.NotifyBudgetPause
	}
	for line := range strings.SplitSeq(body, "\n") {
		if kind, ok := strings.CutPrefix(line, "Kind: "); ok {
			return kind
		}
	}
	return ""
}

func TestOwnerNotificationsDemonstration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, masons := journey.NewMasonFixture(t, 1, fixturekit.ValidPlan)
	defer func() { f.Stop(t) }()

	f.Stop(t)
	hook := fixturekit.NewWebhook(t).Records(f.Opts.Config.Root)
	configPath := filepath.Join(f.Opts.Config.Root, "config.toml")
	configFile, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0)
	fixturekit.Must(t, err)
	_, err = configFile.WriteString("[budget]\nper_day = \"1.00\"\n")
	fixturekit.Must(t, errors.Join(err, configFile.Close()))
	fixturekit.SetWebhook(t, f.Opts.Config.Root, hook.Hook())
	f.Opts.Location = time.UTC
	f.Opts.NotifyRetry = 2 * time.Second

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

	f.Script("amend-1-1", map[string]string{plan.SpecPath: intakefixture.AmendedSpec}, nil)
	f.Script("amend-1-round-1-"+intake.CommitteeAgent(1)+"-1", nil, nil)
	f.Script("amend-1-reply-1", nil, nil)

	problems := &demoProblems{}
	p := &intakefixture.Faults{}
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

	masons.Manager.Route("2", []string{"spec#1"}, "Resume from a durable checkpoint", "Acknowledgements are not durable")
	f.Answer("1", func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) error {
		_, err := fixturekit.CallTool(ctx, tools, questions.AskTool, map[string]any{"question": "Acknowledgements are not durable, so resuming from the last one loses chunks. Should the spec say a durable checkpoint?"})
		return err
	})

	f.Engine.Mu.Lock()

	f.Engine.Turns[execution.MasonTurnID("resume")] = masons.Asking(p, "", "1")
	f.Engine.Turns[rulingTurn] = func(ctx context.Context, req agent.Request, verified *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		result, err := masons.Turn(ctx, req, verified, tools)
		if result != nil {
			result.CostUSD, result.CostKnown = 1.25, true
		}
		return result, err
	}
	for i := 1; i <= 3; i++ {
		f.Engine.Turns[fmt.Sprintf("%s-owner-revise-%d", intake.MasonAgent("dedupe"), i)] = masons.Turn
	}
	manager := f.Engine.Turns["*"]
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
			return manager(ctx, req, turn, tools)
		}
		return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Reviewed", SessionDir: req.SessionDir, NumTurns: 1}, nil
	}
	f.Engine.Mu.Unlock()
	f.Start(t)

	notified := func(kind string) string {
		t.Helper()
		var body string
		intakefixture.Eventually(t, "the webhook never accepted a "+kind+" notification", func() bool {
			for _, b := range hook.Accepted() {
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
		if !strings.Contains(body, "\nProject: "+string(f.Project)+"\n") || !strings.Contains(body, "\nWorkstream: "+string(stream)+"\n") || !strings.Contains(body, "\nKind: "+kind+"\n") {
			t.Fatalf("the %s notification does not name its project, workstream and kind:\n%s", kind, body)
		}
	}

	stream = f.ShedAs(t, "first-release")
	names(notified(api.InboxRatification), api.InboxRatification)
	f.RatifiedBuild(t, stream)

	intakefixture.Eventually(t, "question 1 was never escalated", func() bool {
		asked, err := f.Repository().Questions(stream)
		fixturekit.Must(t, err)
		return slices.ContainsFunc(asked, func(q trace.QuestionState) bool { return q.Asked.ID == "1" && q.State == trace.QuestionEscalated })
	})
	names(notified(api.InboxEscalation), api.InboxEscalation)

	intakefixture.Eventually(t, "the escalation notification was never recorded as sent", func() bool {
		return slices.ContainsFunc(slices.Collect(maps.Values(fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, f.Opts.Config.Root).Notifications)), func(rec *service.Notification) bool {
			return rec.Kind == api.InboxEscalation && rec.State == service.NotificationSent
		})
	})
	f.Stop(t)
	sent := len(hook.Received())
	ledgerPath := filepath.Join(f.Opts.Config.Root, "notifications.json")
	beforeRestart := ledgerModTime(t, ledgerPath)
	f.Start(t)

	intakefixture.Soon(t, "the notifier to pass after the restart", func() bool {
		return ledgerModTime(t, ledgerPath).After(beforeRestart)
	})
	intakefixture.Settled(t, func() int { return len(hook.Received()) })
	if since := hook.Received()[sent:]; len(since) != 0 {
		t.Fatalf("posts after the restart with the escalation open: %q", since)
	}
	f.Rule(t, "1")

	f.AwaitAmendment(t, stream, intake.AmendmentPresented)
	names(notified(api.InboxAmendment), api.InboxAmendment)
	view, err := f.C.Amendment(ctx, stream, "1")
	fixturekit.Must(t, err)
	_, err = f.C.DecideAmendment(ctx, stream, "1", api.AmendmentDecisionRequest{Decision: intake.AmendmentReject, Packet: view.Revision, Note: "Keep the acknowledged-chunk contract."})
	fixturekit.Must(t, err)
	f.AwaitAmendment(t, stream, intake.AmendmentRuled)

	intakefixture.Eventually(t, "the daily budget never paused the factory", func() bool {
		pause, ok := factoryPauseOf(t, f.C)
		return ok && pause.Source == runtime.PauseDailyBudget
	})
	if body := notified(service.NotifyBudgetPause); !strings.Contains(body, "\nProject: "+string(f.Project)+"\n") || !strings.Contains(body, "\nSpend: USD 1.25 or more\nLimit: USD 1.00\n") {
		t.Fatalf("budget pause notification:\n%s", body)
	}
	intakefixture.Mutation(t, f.C, "DELETE", "pause", api.ClearPauseRequest(factoryTarget))
	f.AwaitMerged(t, stream, "resume")

	hook.Respond(http.StatusServiceUnavailable)
	f.AwaitUnit(t, stream, "dedupe", intake.UnitContested)
	journey.RaiseFixtureContest(t, f.Repository(), stream, "dedupe")
	intakefixture.Eventually(t, "the contested notification was never attempted", func() bool {
		return slices.ContainsFunc(hook.Received(), func(b string) bool { return notifiedKind(b) == api.InboxContested })
	})
	f.Stop(t)
	before := len(hook.Received())
	var pending []*service.Notification
	for _, rec := range fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, f.Opts.Config.Root).Notifications {
		if rec.State == service.TestNotificationPending {
			pending = append(pending, rec)
		}
	}
	if len(pending) != 1 || pending[0].Kind != api.InboxContested || pending[0].Workstream != string(stream) {
		t.Fatalf("pending across the restart: %+v", pending)
	}
	hook.Respond(http.StatusOK)
	f.Start(t)
	names(notified(api.InboxContested), api.InboxContested)
	if since := hook.Received()[before:]; len(since) != 1 || notifiedKind(since[0]) != api.InboxContested {
		t.Fatalf("posts after the restart: %q", since)
	}
	if _, err := f.C.RuleContested(ctx, stream, "dedupe", "revise", "Skip chunks the store acknowledged."); err != nil {
		t.Fatal(err)
	}
	f.AwaitMerged(t, stream, "dedupe")

	f.AwaitFeature(t, stream, intake.AssembledState)
	names(notified(api.InboxDelivery), api.InboxDelivery)
	presented, err := f.C.Delivery(ctx, stream)
	fixturekit.Must(t, err)
	_, err = f.C.ApproveDelivery(ctx, stream, api.DeliveryDecision{Review: presented.Report.Review, ReviewRevision: presented.ReviewRevision, Commit: presented.Report.Commit, DraftHash: presented.DraftHash})
	fixturekit.Must(t, err)
	f.AwaitFeature(t, stream, intake.DeliveredState)
	problems.check(t)
	p.Check(t)
	masons.Check(t)
	prs.Mu.Lock()
	if len(prs.Prs) != 1 || prs.Creates != 1 || prs.Prs[0].Body != presented.Draft {
		t.Fatalf("pull requests %+v", prs.Prs)
	}
	prs.Mu.Unlock()

	kinds := []string{api.InboxRatification, api.InboxEscalation, api.InboxAmendment, service.NotifyBudgetPause, api.InboxContested, api.InboxDelivery}
	var got []string
	for _, b := range hook.Accepted() {
		got = append(got, notifiedKind(b))
	}
	if !slices.Equal(got, kinds) {
		t.Fatalf("accepted notifications %v, want %v", got, kinds)
	}
	for _, b := range hook.Received() {
		if k := notifiedKind(b); !slices.Contains(kinds, k) {
			t.Fatalf("unexpected post:\n%s", b)
		}
	}
	for key, rec := range fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, f.Opts.Config.Root).Notifications {
		if rec.State != service.NotificationSent {
			t.Fatalf("notification %s is %s", key, rec.State)
		}
	}
	inbox, err := f.C.Inbox(ctx)
	fixturekit.Must(t, err)
	if len(inbox.Entries) != 0 {
		t.Fatalf("inbox after delivery: %+v", inbox.Entries)
	}
}
