package service

import (
	context "context"
	fmt "fmt"
	config "github.com/kpenfound/osmia/internal/config"
	coreadapter "github.com/kpenfound/osmia/internal/coreadapter"
	eventstream "github.com/kpenfound/osmia/internal/eventstream"
	runtime "github.com/kpenfound/osmia/internal/runtime"
	fixturekit "github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	trace "github.com/kpenfound/osmia/internal/trace"
	http "net/http"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"
)

// fakeWebhook records the bodies posted to it and answers with status. It
// stands in for the owner's real webhook endpoint, leaving that endpoint's
// actual network behaviour and availability unverified.
type fakeWebhook struct{ *fixturekit.Webhook }

func newFakeWebhook(t *testing.T) *fakeWebhook { return &fakeWebhook{fixturekit.NewWebhook(t)} }

func (w *fakeWebhook) records(opts Options) *fakeWebhook {
	w.Webhook.Records(opts.Config.Root)
	return w
}

func (w *fakeWebhook) hook() string { return w.Hook() }

func (w *fakeWebhook) respond(status int) { w.Respond(status) }

func (w *fakeWebhook) received() []string { return w.Received() }

func (w *fakeWebhook) await(t *testing.T, n int) []string {
	t.Helper()
	return intakefixture.AwaitWebhook(t, w.Webhook, n)
}

type fakeInbox struct{ fixturekit.NotificationInbox }

func (f *fakeInbox) read(ctx context.Context) (InboxResponse, *APIError) { return f.Read(ctx) }

func (f *fakeInbox) set(entries ...InboxEntry) { f.Set(entries...) }

var soon = intakefixture.Soon

// settled waits until count stops changing for a 50ms window, so a check
// right after it returns sees nothing still in flight (such as a webhook
// post a background pass has not yet made), bounded overall by soon's
// 20-second deadline.
func settled(t *testing.T, count func() int) {
	t.Helper()
	intakefixture.Settled(t, count)
}

func notifyFixture(t *testing.T, webhook string) (Options, *fakeInbox) {
	t.Helper()
	opts := fixture(t)
	inbox := &fakeInbox{}
	opts.notifyInbox = inbox.read
	opts.NotifyRetry = 10 * time.Millisecond
	if webhook != "" {
		setWebhook(t, opts, webhook)
	}
	return opts, inbox
}

func setWebhook(t *testing.T, opts Options, webhook string) {
	t.Helper()
	fixturekit.SetWebhook(t, opts.Config.Root, webhook)
}

func startNotifying(t *testing.T, opts Options, inbox *fakeInbox) (*Service, *Client) {
	t.Helper()
	s, c := start(t, opts)
	inbox.Observe(func() { s.hub.Publish(Event{Kind: EventInbox}) })
	return s, c
}

func readLedger(t *testing.T, opts Options) notifyLedger {
	t.Helper()
	return fixturekit.ReadNotificationLedger[notifyLedger](t, opts.Config.Root)
}

var entryOf = fixturekit.NotificationEntry

// The notifier reads the service's own inbox: an escalation that opens after
// the webhook is set is posted with the chief of staff's rephrasing and
// recommendation, and the delivery already open is not.
func TestNotifyReadsTheServiceInbox(t *testing.T) {
	t.Parallel()
	f, _, repository, _ := deliveryFixture(t)
	ctx := context.Background()
	hook := newFakeWebhook(t)
	hook.records(Options{Config: config.Options{Root: f.s.cfg.Root.String()}})
	f.s.mu.Lock()
	cfg := *f.s.cfg
	cfg.Notify.Webhook = hook.hook()
	f.s.cfg = &cfg
	f.s.mu.Unlock()
	n := f.s.notifier
	n.inbox = f.s.inbox
	if due := n.pass(ctx); !due.IsZero() || len(hook.received()) != 0 {
		t.Fatalf("delivery open before the webhook: %v %q", due, hook.received())
	}

	p := repository.Project()
	escalating := config.WorkstreamID("w_" + strings.Repeat("1", 32))
	at := time.Now().UTC()
	must(t, repository.CreateWorkstream(ctx, escalating, at, ownerActor))
	header := trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: demoAgent, Revision: 1, Project: p, Workstream: escalating, At: at, Actor: ownerActor, Cause: "test"}
	must(t, repository.CreateThread(ctx, trace.Agent{Header: header, Role: demoRole, ThreadID: demoThread}))
	claim := func(agent, thread, turn string) coreadapter.Scope {
		t.Helper()
		h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: p, Workstream: escalating, At: at, Actor: ownerActor, Cause: "test"}
		_, err := repository.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: agent, ThreadID: thread, TurnID: turn, Profile: coreadapter.Profile{Name: "other", Backend: "codex", Model: "other"}, Prompt: "Work"})
		must(t, err)
		_, err = repository.ClaimTurn(ctx, escalating, agent, "token_"+turn, filepath.Join(t.TempDir(), turn), at)
		must(t, err)
		th, err := repository.Thread(escalating, agent)
		must(t, err)
		return coreadapter.Scope{Project: string(p), Workstream: string(escalating), Thread: thread, Turn: turn, Role: th.Identity.Role}
	}
	_, err := repository.Ask(ctx, demoAgent, claim(demoAgent, demoThread, "build"), "Where does state live?", at)
	must(t, err)
	_, err = repository.EscalateQuestions(ctx, trace.ChiefOfStaff, claim(trace.ChiefOfStaff, trace.ChiefOfStaff, "events"),
		trace.EscalationRequest{Questions: []string{"1"}, Rephrasing: "Where should state live?", Blocked: "The unit.", Options: []string{"Files"}, Recommendation: "In files."}, at)
	must(t, err)
	n.pass(ctx)
	want := fmt.Sprintf("Osmia needs your decision.\nProject: %s\nWorkstream: %s\nKind: escalation\nQuestion: Where should state live?\nRecommendation: In files.\n", p, escalating)
	if got := hook.received(); len(got) != 1 || got[0] != want {
		t.Fatalf("posts %q, want %q", got, want)
	}
	n.pass(ctx)
	if got := hook.received(); len(got) != 1 {
		t.Fatalf("posted again: %q", got)
	}
}

// budgetNotifier is a notifier over a daily budget service whose webhook is
// hook, and the fake inbox it reads.
func budgetNotifier(t *testing.T, s *Service, hook string) (*notifier, *fakeInbox) {
	t.Helper()
	cfg := *s.cfg
	cfg.Notify.Webhook = hook
	s.cfg = &cfg
	inbox := &fakeInbox{}
	return newNotifier(s, 10*time.Millisecond, inbox.read), inbox
}

// A daily budget pause is posted once with the spend, the limit and when it
// clears; further passes and a restart post nothing for it, a pause cleared
// before its post is dropped, and a pause on a later day is a new occurrence.
func TestNotifyDailyBudgetPauseOncePerPause(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hook := newFakeWebhook(t)
	s, _, repo, clock := dailyBudgetService(t, 0, nil)
	daily := dailyBudget{s: s, repository: repo}
	n, _ := budgetNotifier(t, s, hook.hook())
	n.pass(ctx)
	if got := hook.received(); len(got) != 0 {
		t.Fatalf("posts without a pause: %q", got)
	}

	appendDayCost(t, repo, stream, "spent", clock.Now(), 1.25, true)
	must(t, daily.Pass(ctx))
	n.pass(ctx)
	n.pass(ctx)
	want := fmt.Sprintf("Osmia paused dispatch: the daily budget is reached.\nProject: %s\nSpend: USD 1.25\nLimit: USD 1.00\nClears: 2026-09-17T00:00:00-07:00\n", project)
	if got := hook.received(); len(got) != 1 || got[0] != want {
		t.Fatalf("posts for the pause\n%q\nwant\n%q", got, want)
	}

	// A restart reads the ledger again and posts nothing.
	restarted, _ := budgetNotifier(t, s, hook.hook())
	restarted.pass(ctx)
	if got := hook.received(); len(got) != 1 {
		t.Fatalf("posts after a restart: %q", got)
	}

	// The next day's pause is a new occurrence.
	jump(clock, time.Date(2026, 9, 17, 7, 0, 0, 0, time.UTC))
	must(t, daily.Pass(ctx))
	n.pass(ctx)
	appendDayCost(t, repo, sibling, "next-day", clock.Now(), 1, true)
	must(t, daily.Pass(ctx))
	hook.respond(http.StatusBadGateway)
	n.pass(ctx)
	if got := hook.received(); len(got) != 2 || !strings.Contains(got[1], "Spend: USD 1\n") || !strings.Contains(got[1], "Clears: 2026-09-18T00:00:00-07:00") {
		t.Fatalf("posts for the next day's pause: %q", got)
	}

	// The owner clears the pause before the failed post is retried.
	must(t, s.store.ClearPause(factoryTarget, runtime.PauseDailyBudget))
	hook.respond(http.StatusOK)
	time.Sleep(20 * time.Millisecond)
	n.pass(ctx)
	if got := hook.received(); len(got) != 2 {
		t.Fatalf("posts for a cleared pause: %q", got)
	}
	states := map[string]int{}
	for _, rec := range n.ledger.Notifications {
		states[rec.State]++
	}
	if states[notificationSent] != 1 || states[notificationDropped] != 1 || len(n.ledger.Notifications) != 2 {
		t.Fatalf("ledger %+v", n.ledger.Notifications)
	}
}

// A pause already in force when notifications turn on is not posted, and one
// that cannot be described, with no daily budget configured, waits and is
// reported.
func TestNotifyDailyBudgetPauseBeforeTheWebhookAndUnreadableSpend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hook := newFakeWebhook(t)
	s, _, repo, clock := dailyBudgetService(t, 0, nil)
	appendDayCost(t, repo, stream, "spent", clock.Now(), 1.25, true)
	must(t, dailyBudget{s: s, repository: repo}.Pass(ctx))
	n, inbox := budgetNotifier(t, s, hook.hook())
	n.pass(ctx)
	if got := hook.received(); len(got) != 0 {
		t.Fatalf("posts for a pause open before the webhook: %q", got)
	}
	for _, rec := range n.ledger.Notifications {
		if rec.State != notificationSkipped {
			t.Fatalf("pause open before the webhook: %+v", rec)
		}
	}

	// An entry whose post failed stays pending while the pause in force
	// cannot be described, and is not posted once it is decided.
	e := entryOf(InboxEscalation, 1)
	inbox.set(e)
	hook.respond(http.StatusBadGateway)
	n.pass(ctx)
	if got := hook.received(); len(got) != 1 {
		t.Fatalf("posts for the entry: %q", got)
	}
	unbudgeted := *s.cfg
	unbudgeted.Budget.PerDay = ""
	s.mu.Lock()
	s.cfg = &unbudgeted
	s.mu.Unlock()
	inbox.set()
	hook.respond(http.StatusOK)
	time.Sleep(20 * time.Millisecond)
	n.pass(ctx)
	if got := hook.received(); len(got) != 1 {
		t.Fatalf("posts with no daily budget configured: %q", got)
	}
	if rec := n.ledger.Notifications[notificationKey(project, e)]; rec == nil || rec.State != notificationPending {
		t.Fatalf("the decided entry with no daily budget configured: %+v", rec)
	}
	if d := n.diagnostics(); !slices.ContainsFunc(d, func(d Diagnostic) bool {
		return d.Message == "no daily budget is configured; the budget pause notification waits until one is"
	}) {
		t.Fatalf("diagnostics %+v", d)
	}

	// Once the pause is cleared the decided entry is dropped.
	must(t, s.store.ClearPause(factoryTarget, runtime.PauseDailyBudget))
	n.pass(ctx)
	if rec := n.ledger.Notifications[notificationKey(project, e)]; rec.State != notificationDropped {
		t.Fatalf("the decided entry after the pause cleared: %+v", rec)
	}
}

// A runtime change wakes the notifier, so a budget pause is posted without
// waiting for the periodic pass.
func TestNotifyWakesOnRuntimeChanges(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	hook := newFakeWebhook(t)
	s, _, repo, clock := dailyBudgetService(t, 0, nil)
	s.hub = eventstream.New()
	n, _ := budgetNotifier(t, s, hook.hook())
	s.notifier = n
	s.store.Observe(func() { s.hub.Publish(Event{Kind: EventRuntime}) })
	go n.run(ctx)
	t.Cleanup(func() { cancel(); <-n.done })
	soon(t, "the first pass", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.ledger != nil && n.ledger.Enabled
	})
	// A new subscriber starts with a resync pending, which wakes the
	// notifier whatever the event; let an unrelated event use it up.
	s.hub.Publish(Event{Kind: EventSpend})
	soon(t, "the pending resync consumed", s.hub.Idle)
	appendDayCost(t, repo, stream, "spent", clock.Now(), 1.25, true)
	must(t, dailyBudget{s: s, repository: repo}.Pass(ctx))
	if got := hook.await(t, 1); !strings.Contains(got[0], "Spend: USD 1.25") {
		t.Fatalf("post %q", got)
	}
}
