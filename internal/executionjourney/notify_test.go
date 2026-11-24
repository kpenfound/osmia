package executionjourney

import (
	context "context"
	json "encoding/json"
	fmt "fmt"
	http "net/http"
	os "os"
	filepath "path/filepath"
	slices "slices"
	strings "strings"
	testing "testing"
	time "time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
)

func TestNotifyPostsEveryNewInboxEntryOnce(t *testing.T) {
	t.Parallel()
	hook := fixturekit.NewWebhook(t)
	opts, inbox := journey.NotifyFixture(t, "")
	hook.Records(opts.Config.Root)
	journey.WithTailnet(t, &opts, "osmia.example.ts.net", "osmia")
	fixturekit.WithListen(t, opts.Config.Root, "tailnet = \"osmia\"\ntailnet_https = false\n")
	fixturekit.SetWebhook(t, opts.Config.Root, hook.Hook())
	old := fixturekit.NotificationEntry(api.InboxEscalation, 1)
	inbox.Set(old)
	s, _ := journey.StartNotifying(t, opts, inbox)
	intakefixture.Soon(t, "the open entry recorded as skipped", func() bool {
		l := fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root)
		return l.Enabled && l.Notifications[service.NotificationKey(fixturekit.Project, old)] != nil
	})
	if got := fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root).Notifications[service.NotificationKey(fixturekit.Project, old)].State; got != service.NotificationSkipped {
		t.Fatalf("entry open before the webhook: %s", got)
	}
	kinds := []string{api.InboxEscalation, api.InboxRatification, api.InboxContested, api.InboxAmendment, api.InboxDelivery}
	entries := []api.InboxEntry{old}
	for i, kind := range kinds {
		entries = append(entries, fixturekit.NotificationEntry(kind, i+2))
	}
	inbox.Set(entries...)
	bodies := intakefixture.AwaitWebhook(t, hook, len(kinds))
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
		for key, rec := range fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root).Notifications {
			if key != service.NotificationKey(fixturekit.Project, old) && rec.State != service.NotificationSent {
				return false
			}
		}
		return true
	})
	fixturekit.Must(t, s.Close())

	inbox.Observe(nil)
	journey.StartNotifying(t, opts, inbox)
	next := fixturekit.NotificationEntry(api.InboxEscalation, 9)
	inbox.Set(append(entries, next)...)
	intakefixture.AwaitWebhook(t, hook, len(kinds)+1)
	intakefixture.Settled(t, func() int { return len(hook.Received()) })
	if bodies := hook.Received(); len(bodies) != len(kinds)+1 || !strings.Contains(bodies[len(kinds)], "Decide escalation 9?") {
		t.Fatalf("posts after restart: %q", bodies[len(kinds):])
	}
}

func TestNotifyRestartSendsPendingAndNeverResendsSent(t *testing.T) {
	t.Parallel()
	hook := fixturekit.NewWebhook(t)
	opts, inbox := journey.NotifyFixture(t, hook.Hook())
	hook.Records(opts.Config.Root)
	pending, sent := fixturekit.NotificationEntry(api.InboxRatification, 1), fixturekit.NotificationEntry(api.InboxDelivery, 2)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	ledger := service.TestNotifyLedger{Enabled: true, Notifications: map[string]*service.Notification{
		service.NotificationKey(fixturekit.Project, pending): {State: service.TestNotificationPending, Kind: pending.Kind, Workstream: string(fixturekit.Stream), Body: "the pending body\n", Recorded: at},
		service.NotificationKey(fixturekit.Project, sent):    {State: service.NotificationSent, Kind: sent.Kind, Workstream: string(fixturekit.Stream), Recorded: at, Settled: at},
	}}
	data, err := json.Marshal(ledger)
	fixturekit.Must(t, err)
	fixturekit.Must(t, os.WriteFile(filepath.Join(opts.Config.Root, "notifications.json"), data, 0600))
	inbox.Set(pending, sent)
	journey.StartNotifying(t, opts, inbox)
	if got := intakefixture.AwaitWebhook(t, hook, 1); got[0] != "the pending body\n" {
		t.Fatalf("first post %q", got[0])
	}
	marker := fixturekit.NotificationEntry(api.InboxEscalation, 3)
	inbox.Set(pending, sent, marker)
	intakefixture.AwaitWebhook(t, hook, 2)
	intakefixture.Settled(t, func() int { return len(hook.Received()) })
	if got := hook.Received(); len(got) != 2 || !strings.Contains(got[1], "Decide escalation 3?") {
		t.Fatalf("posts %q", got)
	}
	if l := fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root); l.Notifications[service.NotificationKey(fixturekit.Project, pending)].State != service.NotificationSent || l.Notifications[service.NotificationKey(fixturekit.Project, sent)].State != service.NotificationSent {
		t.Fatalf("ledger %+v", l.Notifications)
	}
}

func TestNotifyReloadTurnsNotificationsOnAndOff(t *testing.T) {
	t.Parallel()
	first, second := fixturekit.NewWebhook(t), fixturekit.NewWebhook(t)
	opts, inbox := journey.NotifyFixture(t, "")
	first.Records(opts.Config.Root)
	second.Records(opts.Config.Root)
	opts.NotifyRetry = time.Minute
	e1 := fixturekit.NotificationEntry(api.InboxEscalation, 1)
	inbox.Set(e1)
	_, c := journey.StartNotifying(t, opts, inbox)
	ctx := context.Background()
	if _, err := os.Stat(filepath.Join(opts.Config.Root, "notifications.json")); !os.IsNotExist(err) {
		t.Fatalf("ledger without a webhook: %v", err)
	}

	fixturekit.SetWebhook(t, opts.Config.Root, first.Hook())
	_, err := c.Reload(ctx)
	fixturekit.Must(t, err)
	cfg, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	if cfg.Effective.Notify.Webhook != first.Hook() {
		t.Fatalf("effective notify %+v", cfg.Effective.Notify)
	}

	intakefixture.Soon(t, "the open entry recorded as skipped", func() bool {
		l := fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root)
		return l.Enabled && l.Notifications[service.NotificationKey(fixturekit.Project, e1)] != nil
	})
	e2 := fixturekit.NotificationEntry(api.InboxAmendment, 2)
	inbox.Set(e1, e2)
	if got := intakefixture.AwaitWebhook(t, first, 1); len(got) != 1 || !strings.Contains(got[0], "Kind: amendment") {
		t.Fatalf("posts after setting the webhook: %q", got)
	}
	if got := fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root).Notifications[service.NotificationKey(fixturekit.Project, e1)]; got == nil || got.State != service.NotificationSkipped {
		t.Fatalf("entry open before the webhook was set: %+v", got)
	}

	fixturekit.SetWebhook(t, opts.Config.Root, second.Hook())
	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	e3 := fixturekit.NotificationEntry(api.InboxContested, 3)
	inbox.Set(e1, e2, e3)
	if got := intakefixture.AwaitWebhook(t, second, 1); !strings.Contains(got[0], "Kind: contested") {
		t.Fatalf("posts to the changed webhook: %q", got)
	}

	second.Respond(http.StatusBadGateway)
	e3b := fixturekit.NotificationEntry(api.InboxEscalation, 6)
	inbox.Set(e1, e2, e3, e3b)
	intakefixture.AwaitWebhook(t, second, 2)
	fixturekit.SetWebhook(t, opts.Config.Root, "")
	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	intakefixture.Soon(t, "notifications turned off", func() bool {
		return !fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root).Enabled
	})
	if rec := fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root).Notifications[service.NotificationKey(fixturekit.Project, e3b)]; rec.State != service.NotificationDropped || rec.Attempts != 1 {
		t.Fatalf("pending when the webhook was removed: %+v", rec)
	}
	second.Respond(http.StatusOK)
	e4 := fixturekit.NotificationEntry(api.InboxDelivery, 4)
	inbox.Set(e1, e2, e3, e3b, e4)

	fixturekit.SetWebhook(t, opts.Config.Root, second.Hook())
	_, err = c.Reload(ctx)
	fixturekit.Must(t, err)
	e5 := fixturekit.NotificationEntry(api.InboxRatification, 5)
	intakefixture.Soon(t, "the entry opened while unset recorded", func() bool {
		return fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root).Notifications[service.NotificationKey(fixturekit.Project, e4)] != nil
	})
	inbox.Set(e1, e2, e3, e3b, e4, e5)
	intakefixture.AwaitWebhook(t, second, 3)
	intakefixture.Settled(t, func() int { return len(second.Received()) })
	if got := second.Received(); len(got) != 3 || !strings.Contains(got[2], "Kind: ratification") || len(first.Received()) != 1 {
		t.Fatalf("posts after turning notifications back on: %q and %q", first.Received(), got)
	}
	if rec := fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root).Notifications[service.NotificationKey(fixturekit.Project, e4)]; rec.State != service.NotificationSkipped {
		t.Fatalf("entry opened while unset: %+v", rec)
	}
}

func TestNotifyFailingWebhookRetriesThenGivesUp(t *testing.T) {
	t.Parallel()
	hook := fixturekit.NewWebhook(t)
	hook.Respond(http.StatusServiceUnavailable)
	opts, inbox := journey.NotifyFixture(t, hook.Hook())
	hook.Records(opts.Config.Root)
	opts.NotifyRetry = 100 * time.Millisecond
	_, c := journey.StartNotifying(t, opts, inbox)
	intakefixture.Soon(t, "notifications on", func() bool {
		return fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root).Enabled
	})
	failing := fixturekit.NotificationEntry(api.InboxEscalation, 1)
	inbox.Set(failing)
	intakefixture.AwaitWebhook(t, hook, 1)
	intakefixture.Soon(t, "a retrying diagnostic", func() bool {
		st, cf := notifyDiagnostics(t, c)
		return len(st) == 1 && len(cf) == 1 && strings.Contains(st[0].Message, "; retrying")
	})
	start := time.Now()
	intakefixture.AwaitWebhook(t, hook, service.NotifyAttempts)
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
	intakefixture.Settled(t, func() int { return len(hook.Received()) })
	if n := len(hook.Received()); n != service.NotifyAttempts {
		t.Fatalf("%d posts after giving up", n)
	}
	if rec := fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root).Notifications[service.NotificationKey(fixturekit.Project, failing)]; rec.State != service.NotificationGaveUp || rec.Attempts != service.NotifyAttempts {
		t.Fatalf("gave up: %+v", rec)
	}

	decided := fixturekit.NotificationEntry(api.InboxDelivery, 2)
	inbox.Set(failing, decided)
	intakefixture.AwaitWebhook(t, hook, service.NotifyAttempts+1)
	inbox.Set(failing)
	intakefixture.Soon(t, "the decided entry dropped", func() bool {
		rec := fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root).Notifications[service.NotificationKey(fixturekit.Project, decided)]
		return rec != nil && rec.State == service.NotificationDropped
	})
	if rec := fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root).Notifications[service.NotificationKey(fixturekit.Project, decided)]; rec.Attempts >= service.NotifyAttempts || rec.Body != "" {
		t.Fatalf("dropped: %+v", rec)
	}

	hook.Respond(http.StatusNoContent)
	inbox.Set(failing, fixturekit.NotificationEntry(api.InboxContested, 3))
	intakefixture.Soon(t, "the diagnostic cleared", func() bool {
		st, cf := notifyDiagnostics(t, c)
		return len(st) == 0 && len(cf) == 0
	})
	if got := hook.Received(); !strings.Contains(got[len(got)-1], "Kind: contested") {
		t.Fatalf("last post %q", got[len(got)-1])
	}
}

func TestNotifyLinksTheConfiguredTailnetHostname(t *testing.T) {
	t.Parallel()
	hook := fixturekit.NewWebhook(t)
	opts, inbox := journey.NotifyFixture(t, "")
	hook.Records(opts.Config.Root)
	journey.WithTailnet(t, &opts)
	fixturekit.WithListen(t, opts.Config.Root, "tailnet = \"osmia\"\n")
	fixturekit.SetWebhook(t, opts.Config.Root, hook.Hook())
	journey.StartNotifying(t, opts, inbox)
	intakefixture.Soon(t, "notifications on", func() bool {
		return fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root).Enabled
	})
	inbox.Set(fixturekit.NotificationEntry(api.InboxDelivery, 1))
	if got := intakefixture.AwaitWebhook(t, hook, 1); !strings.HasSuffix(got[0], "\nOpen: http://osmia/\n") {
		t.Fatalf("post %q", got[0])
	}
}

func TestNotifyFaultsSendNothingUntilCleared(t *testing.T) {
	t.Parallel()
	hook := fixturekit.NewWebhook(t)
	opts, inbox := journey.NotifyFixture(t, hook.Hook())
	hook.Records(opts.Config.Root)
	ledger := filepath.Join(opts.Config.Root, "notifications.json")

	fixturekit.Must(t, os.WriteFile(ledger, []byte("{not json"), 0600))
	_, c := journey.StartNotifying(t, opts, inbox)
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
	inbox.Set(e1)
	fixturekit.Must(t, os.Remove(ledger))
	inbox.Set(e1)
	cleared("the ledger readable again")
	if rec := fixturekit.ReadNotificationLedger[service.TestNotifyLedger](t, opts.Config.Root).Notifications[service.NotificationKey(fixturekit.Project, e1)]; rec == nil || rec.State != service.NotificationSkipped {
		t.Fatalf("entry open while the ledger was unreadable: %+v", rec)
	}

	fixturekit.Must(t, os.Remove(ledger))
	fixturekit.Must(t, os.MkdirAll(filepath.Join(ledger, "blocked"), 0700))
	e2 := fixturekit.NotificationEntry(api.InboxAmendment, 2)
	inbox.Set(e1, e2)
	fault("cannot record notifications in " + ledger + "; nothing is sent until it can be written")
	intakefixture.Settled(t, func() int { return len(hook.Received()) })
	if got := hook.Received(); len(got) != 0 {
		t.Fatalf("posted without a record: %q", got)
	}
	fixturekit.Must(t, os.RemoveAll(ledger))
	inbox.Set(e1, e2)
	if got := intakefixture.AwaitWebhook(t, hook, 1); !strings.Contains(got[0], "Kind: amendment") {
		t.Fatalf("post after the ledger was writable again: %q", got)
	}
	cleared("the ledger writable again")

	inbox.Break(true)
	fault("cannot read the inbox; notifications wait until it can be read")
	e3 := fixturekit.NotificationEntry(api.InboxContested, 3)
	inbox.Set(e1, e2, e3)
	intakefixture.Settled(t, func() int { return len(hook.Received()) })
	if got := hook.Received(); len(got) != 1 {
		t.Fatalf("posted while the inbox was unreadable: %q", got)
	}
	inbox.Break(false)
	if got := intakefixture.AwaitWebhook(t, hook, 2); !strings.Contains(got[1], "Kind: contested") {
		t.Fatalf("post after the inbox was readable again: %q", got)
	}
	cleared("the inbox readable again")
}
