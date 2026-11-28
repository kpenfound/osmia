package service

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/eventstream"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

func notifyFixture(t *testing.T, webhook string) (Options, *fixturekit.NotificationInbox) {
	t.Helper()
	opts := fixture(t)
	inbox := fixturekit.NewNotifyInbox(t, opts.Config.Root, webhook)
	opts.notifyInbox = inbox.Read
	opts.NotifyRetry = fixturekit.NotifyRetry
	return opts, inbox
}

func startNotifying(t *testing.T, opts Options, inbox *fixturekit.NotificationInbox) (*Service, *Client) {
	t.Helper()
	s, c := start(t, opts)
	inbox.Observe(func() { s.hub.Publish(Event{Kind: EventInbox}) })
	return s, c
}

// The notifier reads the service's own inbox: an escalation that opens after
// the webhook is set is posted with the chief of staff's rephrasing and
// recommendation, and the delivery already open is not.
func TestNotifyReadsTheServiceInbox(t *testing.T) {
	t.Parallel()
	f, _, repository, _ := deliveryFixture(t)
	ctx := context.Background()
	hook := fixturekit.NewWebhook(t)
	hook.Records(f.s.cfg.Root.String())
	f.s.mu.Lock()
	cfg := *f.s.cfg
	cfg.Notify.Webhook = hook.Hook()
	f.s.cfg = &cfg
	f.s.mu.Unlock()
	n := f.s.notifier
	n.inbox = f.s.inbox
	if due := n.pass(ctx); !due.IsZero() || len(hook.Received()) != 0 {
		t.Fatalf("delivery open before the webhook: %v %q", due, hook.Received())
	}

	p := repository.Project()
	escalating := config.WorkstreamID("w_" + strings.Repeat("1", 32))
	at := time.Now().UTC()
	fixturekit.Must(t, repository.CreateWorkstream(ctx, escalating, at, ownerActor))
	header := trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: fixturekit.DemoAgent, Revision: 1, Project: p, Workstream: escalating, At: at, Actor: ownerActor, Cause: "test"}
	fixturekit.Must(t, repository.CreateThread(ctx, trace.Agent{Header: header, Role: fixturekit.DemoRole, ThreadID: fixturekit.DemoThread}))
	claim := func(agent, thread, turn string) coreadapter.Scope {
		t.Helper()
		h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: p, Workstream: escalating, At: at, Actor: ownerActor, Cause: "test"}
		_, err := repository.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: agent, ThreadID: thread, TurnID: turn, Profile: coreadapter.Profile{Name: "other", Backend: "codex", Model: "other"}, Prompt: "Work"})
		fixturekit.Must(t, err)
		_, err = repository.ClaimTurn(ctx, escalating, agent, "token_"+turn, filepath.Join(t.TempDir(), turn), at)
		fixturekit.Must(t, err)
		th, err := repository.Thread(escalating, agent)
		fixturekit.Must(t, err)
		return coreadapter.Scope{Project: string(p), Workstream: string(escalating), Thread: thread, Turn: turn, Role: th.Identity.Role}
	}
	_, err := repository.Ask(ctx, fixturekit.DemoAgent, claim(fixturekit.DemoAgent, fixturekit.DemoThread, "build"), "Where does state live?", at)
	fixturekit.Must(t, err)
	_, err = repository.EscalateQuestions(ctx, trace.Manager, claim(trace.Manager, trace.Manager, "events"),
		trace.EscalationRequest{Questions: []string{"1"}, Rephrasing: "Where should state live?", Blocked: "The unit.", Options: []string{"Files"}, Recommendation: "In files."}, at)
	fixturekit.Must(t, err)
	n.pass(ctx)
	want := fmt.Sprintf("Osmia needs your decision.\nProject: %s\nWorkstream: %s\nKind: escalation\nQuestion: Where should state live?\nRecommendation: In files.\n", p, escalating)
	if got := hook.Received(); len(got) != 1 || got[0] != want {
		t.Fatalf("posts %q, want %q", got, want)
	}
	n.pass(ctx)
	if got := hook.Received(); len(got) != 1 {
		t.Fatalf("posted again: %q", got)
	}
}

func budgetNotifier(t *testing.T, s *Service, hook string) (*notifier, *fixturekit.NotificationInbox) {
	t.Helper()
	cfg := *s.cfg
	cfg.Notify.Webhook = hook
	s.cfg = &cfg
	inbox := &fixturekit.NotificationInbox{}
	return newNotifier(s, 10*time.Millisecond, inbox.Read), inbox
}

// A daily budget pause is posted once with the spend, the limit and when it
// clears; further passes and a restart post nothing for it, a pause cleared
// before its post is dropped, and a pause on a later day is a new occurrence.
func TestNotifyDailyBudgetPauseOncePerPause(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hook := fixturekit.NewWebhook(t)
	s, _, repo, clock := dailyBudgetService(t, 0, nil)
	daily := dailyBudget{s: s, repository: repo}
	n, _ := budgetNotifier(t, s, hook.Hook())
	n.pass(ctx)
	if got := hook.Received(); len(got) != 0 {
		t.Fatalf("posts without a pause: %q", got)
	}

	appendDayCost(t, repo, fixturekit.Stream, "spent", clock.Now(), 1.25, true)
	fixturekit.Must(t, daily.Pass(ctx))
	n.pass(ctx)
	n.pass(ctx)
	want := fmt.Sprintf("Osmia paused dispatch: the daily budget is reached.\nProject: %s\nSpend: USD 1.25\nLimit: USD 1.00\nClears: 2026-09-17T00:00:00-07:00\n", fixturekit.Project)
	if got := hook.Received(); len(got) != 1 || got[0] != want {
		t.Fatalf("posts for the pause\n%q\nwant\n%q", got, want)
	}

	restarted, _ := budgetNotifier(t, s, hook.Hook())
	restarted.pass(ctx)
	if got := hook.Received(); len(got) != 1 {
		t.Fatalf("posts after a restart: %q", got)
	}

	fixturekit.Jump(clock, time.Date(2026, 9, 17, 7, 0, 0, 0, time.UTC))
	fixturekit.Must(t, daily.Pass(ctx))
	n.pass(ctx)
	appendDayCost(t, repo, fixturekit.Sibling, "next-day", clock.Now(), 1, true)
	fixturekit.Must(t, daily.Pass(ctx))
	hook.Respond(http.StatusBadGateway)
	n.pass(ctx)
	if got := hook.Received(); len(got) != 2 || !strings.Contains(got[1], "Spend: USD 1\n") || !strings.Contains(got[1], "Clears: 2026-09-18T00:00:00-07:00") {
		t.Fatalf("posts for the next day's pause: %q", got)
	}

	fixturekit.Must(t, s.store.ClearPause(factoryTarget, runtime.PauseDailyBudget))
	hook.Respond(http.StatusOK)
	time.Sleep(20 * time.Millisecond)
	n.pass(ctx)
	if got := hook.Received(); len(got) != 2 {
		t.Fatalf("posts for a cleared pause: %q", got)
	}
	states := map[string]int{}
	for _, rec := range n.ledger.Notifications {
		states[rec.State]++
	}
	if states[NotificationSent] != 1 || states[NotificationDropped] != 1 || len(n.ledger.Notifications) != 2 {
		t.Fatalf("ledger %+v", n.ledger.Notifications)
	}
}

// A pause already in force when notifications turn on is not posted, and one
// that cannot be described, with no daily budget configured, waits and is
// reported.
func TestNotifyDailyBudgetPauseBeforeTheWebhookAndUnreadableSpend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hook := fixturekit.NewWebhook(t)
	s, _, repo, clock := dailyBudgetService(t, 0, nil)
	appendDayCost(t, repo, fixturekit.Stream, "spent", clock.Now(), 1.25, true)
	fixturekit.Must(t, dailyBudget{s: s, repository: repo}.Pass(ctx))
	n, inbox := budgetNotifier(t, s, hook.Hook())
	n.pass(ctx)
	if got := hook.Received(); len(got) != 0 {
		t.Fatalf("posts for a pause open before the webhook: %q", got)
	}
	for _, rec := range n.ledger.Notifications {
		if rec.State != NotificationSkipped {
			t.Fatalf("pause open before the webhook: %+v", rec)
		}
	}

	e := fixturekit.NotificationEntry(InboxEscalation, 1)
	inbox.Set(e)
	hook.Respond(http.StatusBadGateway)
	n.pass(ctx)
	if got := hook.Received(); len(got) != 1 {
		t.Fatalf("posts for the entry: %q", got)
	}
	unbudgeted := *s.cfg
	unbudgeted.Budget.PerDay = ""
	s.mu.Lock()
	s.cfg = &unbudgeted
	s.mu.Unlock()
	inbox.Set()
	hook.Respond(http.StatusOK)
	time.Sleep(20 * time.Millisecond)
	n.pass(ctx)
	if got := hook.Received(); len(got) != 1 {
		t.Fatalf("posts with no daily budget configured: %q", got)
	}
	if rec := n.ledger.Notifications[NotificationKey(fixturekit.Project, e)]; rec == nil || rec.State != notificationPending {
		t.Fatalf("the decided entry with no daily budget configured: %+v", rec)
	}
	if d := n.diagnostics(); !slices.ContainsFunc(d, func(d Diagnostic) bool {
		return d.Message == "no daily budget is configured; the budget pause Notification waits until one is"
	}) {
		t.Fatalf("diagnostics %+v", d)
	}

	fixturekit.Must(t, s.store.ClearPause(factoryTarget, runtime.PauseDailyBudget))
	n.pass(ctx)
	if rec := n.ledger.Notifications[NotificationKey(fixturekit.Project, e)]; rec.State != NotificationDropped {
		t.Fatalf("the decided entry after the pause cleared: %+v", rec)
	}
}

// A runtime change wakes the notifier, so a budget pause is posted without
// waiting for the periodic pass.
func TestNotifyWakesOnRuntimeChanges(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	hook := fixturekit.NewWebhook(t)
	s, _, repo, clock := dailyBudgetService(t, 0, nil)
	s.hub = eventstream.New()
	n, _ := budgetNotifier(t, s, hook.Hook())
	s.notifier = n
	s.store.Observe(func() { s.hub.Publish(Event{Kind: EventRuntime}) })
	go n.run(ctx)
	t.Cleanup(func() { cancel(); <-n.done })
	intakefixture.Soon(t, "the first pass", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.ledger != nil && n.ledger.Enabled
	})
	s.hub.Publish(Event{Kind: EventSpend})
	intakefixture.Soon(t, "the pending resync consumed", s.hub.Idle)
	appendDayCost(t, repo, fixturekit.Stream, "spent", clock.Now(), 1.25, true)
	fixturekit.Must(t, dailyBudget{s: s, repository: repo}.Pass(ctx))
	if got := intakefixture.AwaitWebhook(t, hook, 1); !strings.Contains(got[0], "Spend: USD 1.25") {
		t.Fatalf("post %q", got)
	}
}
