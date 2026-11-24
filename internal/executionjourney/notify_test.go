package executionjourney

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	http "net/http"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"
)

func TestNotifyPostsEveryNewInboxEntryOnce(t *testing.T) {
	t.Parallel()
	hook := newFakeWebhook(t)
	opts, inbox := notifyFixture(t, "")
	hook.records(opts)
	withTailnet(t, &opts, "osmia.example.ts.net", "osmia")
	withListen(t, opts, "tailnet = \"osmia\"\ntailnet_https = false\n")
	setWebhook(t, opts, hook.hook())
	old := fixturekit.NotificationEntry(api.InboxEscalation, 1)
	inbox.set(old)
	s, _ := startNotifying(t, opts, inbox)
	intakefixture.Soon(t, "the open entry recorded as skipped", func() bool {
		l := readLedger(t, opts)
		return l.Enabled && l.Notifications[service.NotificationKey(fixturekit.Project, old)] != nil
	})
	if got := readLedger(t, opts).Notifications[service.NotificationKey(fixturekit.Project, old)].State; got != service.NotificationSkipped {
		t.Fatalf("entry open before the webhook: %s", got)
	}
	kinds := []string{api.InboxEscalation, api.InboxRatification, api.InboxContested, api.InboxAmendment, api.InboxDelivery}
	entries := []api.InboxEntry{old}
	for i, kind := range kinds {
		entries = append(entries, fixturekit.NotificationEntry(kind, i+2))
	}
	inbox.set(entries...)
	bodies := hook.await(t, len(kinds))
	for i, kind := range kinds {
		want := fmt.Sprintf("Osmia needs your decision.\nProject: %s\nWorkstream: %s\nKind: %s\nQuestion: Decide %s %d? It matters.\n", fixturekit.Project, fixturekit.Stream, kind, kind, i+2)
		if kind != api.InboxContested {
			want += fmt.Sprintf("Recommendation: Recommend %d.\n", i+2)
		}
		want += "Open: http://osmia.example.ts.net/\n"
		if !slices.Contains(bodies, want) {
			t.Fatalf("no post\n%s\nin %q", want, bodies)
		}
	}

	intakefixture.Soon(t, "every post recorded as sent", func() bool {
		for key, rec := range readLedger(t, opts).Notifications {
			if key != service.NotificationKey(fixturekit.Project, old) && rec.State != service.NotificationSent {
				return false
			}
		}
		return true
	})
	fixturekit.Must(t, s.Close())

	inbox.Observe(nil)
	startNotifying(t, opts, inbox)
	next := fixturekit.NotificationEntry(api.InboxEscalation, 9)
	inbox.set(append(entries, next)...)
	hook.await(t, len(kinds)+1)
	intakefixture.Settled(t, func() int { return len(hook.received()) })
	if bodies := hook.received(); len(bodies) != len(kinds)+1 || !strings.Contains(bodies[len(kinds)], "Decide escalation 9?") {
		t.Fatalf("posts after restart: %q", bodies[len(kinds):])
	}
}

func TestNotifyRestartSendsPendingAndNeverResendsSent(t *testing.T) {
	t.Parallel()
	hook := newFakeWebhook(t)
	opts, inbox := notifyFixture(t, hook.hook())
	hook.records(opts)
	pending, sent := fixturekit.NotificationEntry(api.InboxRatification, 1), fixturekit.NotificationEntry(api.InboxDelivery, 2)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	ledger := service.TestNotifyLedger{Enabled: true, Notifications: map[string]*service.Notification{
		service.NotificationKey(fixturekit.Project, pending): {State: service.TestNotificationPending, Kind: pending.Kind, Workstream: string(fixturekit.Stream), Body: "the pending body\n", Recorded: at},
		service.NotificationKey(fixturekit.Project, sent):    {State: service.NotificationSent, Kind: sent.Kind, Workstream: string(fixturekit.Stream), Recorded: at, Settled: at},
	}}
	data, err := json.Marshal(ledger)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(filepath.Join(opts.Config.Root, "notifications.json"), data, 0600))
	inbox.set(pending, sent)
	startNotifying(t, opts, inbox)
	if got := hook.await(t, 1); got[0] != "the pending body\n" {
		t.Fatalf("first post %q", got[0])
	}
	marker := fixturekit.NotificationEntry(api.InboxEscalation, 3)
	inbox.set(pending, sent, marker)
	hook.await(t, 2)
	intakefixture.Settled(t, func() int { return len(hook.received()) })
	if got := hook.received(); len(got) != 2 || !strings.Contains(got[1], "Decide escalation 3?") {
		t.Fatalf("posts %q", got)
	}
	if l := readLedger(t, opts); l.Notifications[service.NotificationKey(fixturekit.Project, pending)].State != service.NotificationSent || l.Notifications[service.NotificationKey(fixturekit.Project, sent)].State != service.NotificationSent {
		t.Fatalf("ledger %+v", l.Notifications)
	}
}

func TestNotifyReloadTurnsNotificationsOnAndOff(t *testing.T) {
	t.Parallel()
	first, second := newFakeWebhook(t), newFakeWebhook(t)
	opts, inbox := notifyFixture(t, "")
	first.records(opts)
	second.records(opts)
	opts.NotifyRetry = time.Minute
	e1 := fixturekit.NotificationEntry(api.InboxEscalation, 1)
	inbox.set(e1)
	_, c := startNotifying(t, opts, inbox)
	ctx := context.Background()
	if _, err := os.Stat(filepath.Join(opts.Config.Root, "notifications.json")); !os.IsNotExist(err) {
		t.Fatalf("ledger without a webhook: %v", err)
	}

	setWebhook(t, opts, first.hook())
	_, err := c.Reload(ctx)
	fixturekit.Must(t, err)
	cfg, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if cfg.Effective.Notify.Webhook != first.hook() {
		t.Fatalf("effective notify %+v", cfg.Effective.Notify)
	}

	intakefixture.Soon(t, "the open entry recorded as skipped", func() bool {
		l := readLedger(t, opts)
		return l.Enabled && l.Notifications[service.NotificationKey(fixturekit.Project, e1)] != nil
	})
	e2 := fixturekit.NotificationEntry(api.InboxAmendment, 2)
	inbox.set(e1, e2)
	if got := first.await(t, 1); len(got) != 1 || !strings.Contains(got[0], "Kind: amendment") {
		t.Fatalf("posts after setting the webhook: %q", got)
	}
	if got := readLedger(t, opts).Notifications[service.NotificationKey(fixturekit.Project, e1)]; got == nil || got.State != service.NotificationSkipped {
		t.Fatalf("entry open before the webhook was set: %+v", got)
	}

	setWebhook(t, opts, second.hook())
	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	e3 := fixturekit.NotificationEntry(api.InboxContested, 3)
	inbox.set(e1, e2, e3)
	if got := second.await(t, 1); !strings.Contains(got[0], "Kind: contested") {
		t.Fatalf("posts to the changed webhook: %q", got)
	}

	second.respond(http.StatusBadGateway)
	e3b := fixturekit.NotificationEntry(api.InboxEscalation, 6)
	inbox.set(e1, e2, e3, e3b)
	second.await(t, 2)
	setWebhook(t, opts, "")
	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	intakefixture.Soon(t, "notifications turned off", func() bool { return !readLedger(t, opts).Enabled })
	if rec := readLedger(t, opts).Notifications[service.NotificationKey(fixturekit.Project, e3b)]; rec.State != service.NotificationDropped || rec.Attempts != 1 {
		t.Fatalf("pending when the webhook was removed: %+v", rec)
	}
	second.respond(http.StatusOK)
	e4 := fixturekit.NotificationEntry(api.InboxDelivery, 4)
	inbox.set(e1, e2, e3, e3b, e4)

	setWebhook(t, opts, second.hook())
	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	e5 := fixturekit.NotificationEntry(api.InboxRatification, 5)
	intakefixture.Soon(t, "the entry opened while unset recorded", func() bool {
		return readLedger(t, opts).Notifications[service.NotificationKey(fixturekit.Project, e4)] != nil
	})
	inbox.set(e1, e2, e3, e3b, e4, e5)
	second.await(t, 3)
	intakefixture.Settled(t, func() int { return len(second.received()) })
	if got := second.received(); len(got) != 3 || !strings.Contains(got[2], "Kind: ratification") || len(first.received()) != 1 {
		t.Fatalf("posts after turning notifications back on: %q and %q", first.received(), got)
	}
	if rec := readLedger(t, opts).Notifications[service.NotificationKey(fixturekit.Project, e4)]; rec.State != service.NotificationSkipped {
		t.Fatalf("entry opened while unset: %+v", rec)
	}
}

func TestNotifyFailingWebhookRetriesThenGivesUp(t *testing.T) {
	t.Parallel()
	hook := newFakeWebhook(t)
	hook.respond(http.StatusServiceUnavailable)
	opts, inbox := notifyFixture(t, hook.hook())
	hook.records(opts)
	opts.NotifyRetry = 100 * time.Millisecond
	_, c := startNotifying(t, opts, inbox)
	intakefixture.Soon(t, "notifications on", func() bool { return readLedger(t, opts).Enabled })
	failing := fixturekit.NotificationEntry(api.InboxEscalation, 1)
	inbox.set(failing)
	hook.await(t, 1)
	intakefixture.Soon(t, "a retrying diagnostic", func() bool {
		st, cf := notifyDiagnostics(t, c)
		return len(st) == 1 && len(cf) == 1 && strings.Contains(st[0].Message, "; retrying")
	})
	start := time.Now()
	hook.await(t, service.NotifyAttempts)
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Fatalf("retries without backoff: %d attempts in %s", service.NotifyAttempts, elapsed)
	}
	want := api.Diagnostic{Field: "notify", Code: api.Unavailable, Message: ""}
	intakefixture.Soon(t, "a gave-up diagnostic", func() bool {
		st, cf := notifyDiagnostics(t, c)
		return len(st) == 1 && len(cf) == 1 && st[0] == cf[0] && strings.HasPrefix(st[0].Message, "gave up notifying the owner of the escalation decision in workstream "+string(fixturekit.Stream))
	})
	st, _ := notifyDiagnostics(t, c)
	if st[0].Code != want.Code || !strings.Contains(st[0].Message, "after 5 attempts: the webhook responded 503 Service Unavailable") || strings.Contains(st[0].Message, "secret") {
		t.Fatalf("diagnostic %+v", st[0])
	}
	intakefixture.Settled(t, func() int { return len(hook.received()) })
	if n := len(hook.received()); n != service.NotifyAttempts {
		t.Fatalf("%d posts after giving up", n)
	}
	if rec := readLedger(t, opts).Notifications[service.NotificationKey(fixturekit.Project, failing)]; rec.State != service.NotificationGaveUp || rec.Attempts != service.NotifyAttempts {
		t.Fatalf("gave up: %+v", rec)
	}

	decided := fixturekit.NotificationEntry(api.InboxDelivery, 2)
	inbox.set(failing, decided)
	hook.await(t, service.NotifyAttempts+1)
	inbox.set(failing)
	intakefixture.Soon(t, "the decided entry dropped", func() bool {
		rec := readLedger(t, opts).Notifications[service.NotificationKey(fixturekit.Project, decided)]
		return rec != nil && rec.State == service.NotificationDropped
	})
	if rec := readLedger(t, opts).Notifications[service.NotificationKey(fixturekit.Project, decided)]; rec.Attempts >= service.NotifyAttempts || rec.Body != "" {
		t.Fatalf("dropped: %+v", rec)
	}

	hook.respond(http.StatusNoContent)
	inbox.set(failing, fixturekit.NotificationEntry(api.InboxContested, 3))
	intakefixture.Soon(t, "the diagnostic cleared", func() bool {
		st, cf := notifyDiagnostics(t, c)
		return len(st) == 0 && len(cf) == 0
	})
	if got := hook.received(); !strings.Contains(got[len(got)-1], "Kind: contested") {
		t.Fatalf("last post %q", got[len(got)-1])
	}
}

func TestNotifyLinksTheConfiguredTailnetHostname(t *testing.T) {
	t.Parallel()
	hook := newFakeWebhook(t)
	opts, inbox := notifyFixture(t, "")
	hook.records(opts)
	withTailnet(t, &opts)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	setWebhook(t, opts, hook.hook())
	startNotifying(t, opts, inbox)
	intakefixture.Soon(t, "notifications on", func() bool { return readLedger(t, opts).Enabled })
	inbox.set(fixturekit.NotificationEntry(api.InboxDelivery, 1))
	if got := hook.await(t, 1); !strings.HasSuffix(got[0], "\nOpen: http://osmia/\n") {
		t.Fatalf("post %q", got[0])
	}
}

func TestNotifyFaultsSendNothingUntilCleared(t *testing.T) {
	t.Parallel()
	hook := newFakeWebhook(t)
	opts, inbox := notifyFixture(t, hook.hook())
	hook.records(opts)
	ledger := filepath.Join(opts.Config.Root, "notifications.json")

	fixturekit.Must(t, os.WriteFile(ledger, []byte("{not json"), 0600))
	_, c := startNotifying(t, opts, inbox)
	fault := func(message string) {
		t.Helper()
		intakefixture.Soon(t, message, func() bool {
			st, cf := notifyDiagnostics(t, c)
			want := api.Diagnostic{Field: "notify", Code: api.Internal, Message: message}
			return len(st) == 1 && len(cf) == 1 && st[0] == want && cf[0] == want
		})
	}
	cleared := func(step string) {
		t.Helper()
		intakefixture.Soon(t, step, func() bool {
			st, cf := notifyDiagnostics(t, c)
			return len(st) == 0 && len(cf) == 0
		})
	}
	fault("cannot read " + ledger + "; nothing is sent until it can be read")
	e1 := fixturekit.NotificationEntry(api.InboxEscalation, 1)
	inbox.set(e1)
	fixturekit.Must(t, os.Remove(ledger))
	inbox.set(e1)
	cleared("the ledger readable again")
	if rec := readLedger(t, opts).Notifications[service.NotificationKey(fixturekit.Project, e1)]; rec == nil || rec.State != service.NotificationSkipped {
		t.Fatalf("entry open while the ledger was unreadable: %+v", rec)
	}

	fixturekit.Must(t, os.Remove(ledger))
	fixturekit.Must(t, os.MkdirAll(filepath.Join(ledger, "blocked"), 0700))
	e2 := fixturekit.NotificationEntry(api.InboxAmendment, 2)
	inbox.set(e1, e2)
	fault("cannot record notifications in " + ledger + "; nothing is sent until it can be written")
	intakefixture.Settled(t, func() int { return len(hook.received()) })
	if got := hook.received(); len(got) != 0 {
		t.Fatalf("posted without a record: %q", got)
	}
	fixturekit.Must(t, os.RemoveAll(ledger))
	inbox.set(e1, e2)
	if got := hook.await(t, 1); !strings.Contains(got[0], "Kind: amendment") {
		t.Fatalf("post after the ledger was writable again: %q", got)
	}
	cleared("the ledger writable again")

	inbox.breakInbox(true)
	fault("cannot read the inbox; notifications wait until it can be read")
	e3 := fixturekit.NotificationEntry(api.InboxContested, 3)
	inbox.set(e1, e2, e3)
	intakefixture.Settled(t, func() int { return len(hook.received()) })
	if got := hook.received(); len(got) != 1 {
		t.Fatalf("posted while the inbox was unreadable: %q", got)
	}
	inbox.breakInbox(false)
	if got := hook.await(t, 2); !strings.Contains(got[1], "Kind: contested") {
		t.Fatalf("post after the inbox was readable again: %q", got)
	}
	cleared("the inbox readable again")
}
