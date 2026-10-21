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
)

// Each kind of inbox entry that opens while the webhook is set is posted
// once as plain text naming the project, workstream, kind, question and
// recommendation, with a link to the page on the tailnet. An entry open
// before the webhook was ever set is not back-filled, and a restart posts
// nothing again.
func TestNotifyPostsEveryNewInboxEntryOnce(t *testing.T) {
	t.Parallel()
	hook := newFakeWebhook(t)
	opts, inbox := notifyFixture(t, "")
	hook.records(opts)
	withTailnet(t, &opts, "osmia.example.ts.net", "osmia")
	withListen(t, opts, "tailnet = \"osmia\"\n")
	setWebhook(t, opts, hook.hook())
	old := entryOf(InboxEscalation, 1)
	inbox.set(old)
	s, _ := startNotifying(t, opts, inbox)
	soon(t, "the open entry recorded as skipped", func() bool {
		l := readLedger(t, opts)
		return l.Enabled && l.Notifications[notificationKey(project, old)] != nil
	})
	if got := readLedger(t, opts).Notifications[notificationKey(project, old)].State; got != notificationSkipped {
		t.Fatalf("entry open before the webhook: %s", got)
	}
	kinds := []string{InboxEscalation, InboxRatification, InboxContested, InboxAmendment, InboxDelivery}
	entries := []InboxEntry{old}
	for i, kind := range kinds {
		entries = append(entries, entryOf(kind, i+2))
	}
	inbox.set(entries...)
	bodies := hook.await(t, len(kinds))
	for i, kind := range kinds {
		want := fmt.Sprintf("Osmia needs your decision.\nProject: %s\nWorkstream: %s\nKind: %s\nQuestion: Decide %s %d? It matters.\n", project, stream, kind, kind, i+2)
		if kind != InboxContested {
			want += fmt.Sprintf("Recommendation: Recommend %d.\n", i+2)
		}
		want += "Open: http://osmia.example.ts.net/\n"
		if !slices.Contains(bodies, want) {
			t.Fatalf("no post\n%s\nin %q", want, bodies)
		}
	}
	// The last post is recorded as sent after its response arrives.
	soon(t, "every post recorded as sent", func() bool {
		for key, rec := range readLedger(t, opts).Notifications {
			if key != notificationKey(project, old) && rec.State != notificationSent {
				return false
			}
		}
		return true
	})
	must(t, s.Close())

	// After a restart only the entry that opens since is posted.
	inbox.Observe(nil)
	startNotifying(t, opts, inbox)
	next := entryOf(InboxEscalation, 9)
	inbox.set(append(entries, next)...)
	hook.await(t, len(kinds)+1)
	settled(t, func() int { return len(hook.received()) })
	if bodies := hook.received(); len(bodies) != len(kinds)+1 || !strings.Contains(bodies[len(kinds)], "Decide escalation 9?") {
		t.Fatalf("posts after restart: %q", bodies[len(kinds):])
	}
}

// A notification recorded as pending before a crash is posted by the next
// start; one recorded as sent is never posted again.
func TestNotifyRestartSendsPendingAndNeverResendsSent(t *testing.T) {
	t.Parallel()
	hook := newFakeWebhook(t)
	opts, inbox := notifyFixture(t, hook.hook())
	hook.records(opts)
	pending, sent := entryOf(InboxRatification, 1), entryOf(InboxDelivery, 2)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	ledger := notifyLedger{Enabled: true, Notifications: map[string]*notification{
		notificationKey(project, pending): {State: notificationPending, Kind: pending.Kind, Workstream: string(stream), Body: "the pending body\n", Recorded: at},
		notificationKey(project, sent):    {State: notificationSent, Kind: sent.Kind, Workstream: string(stream), Recorded: at, Settled: at},
	}}
	data, err := json.Marshal(ledger)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(opts.Config.Root, "notifications.json"), data, 0600))
	inbox.set(pending, sent)
	startNotifying(t, opts, inbox)
	if got := hook.await(t, 1); got[0] != "the pending body\n" {
		t.Fatalf("first post %q", got[0])
	}
	marker := entryOf(InboxEscalation, 3)
	inbox.set(pending, sent, marker)
	hook.await(t, 2)
	settled(t, func() int { return len(hook.received()) })
	if got := hook.received(); len(got) != 2 || !strings.Contains(got[1], "Decide escalation 3?") {
		t.Fatalf("posts %q", got)
	}
	if l := readLedger(t, opts); l.Notifications[notificationKey(project, pending)].State != notificationSent || l.Notifications[notificationKey(project, sent)].State != notificationSent {
		t.Fatalf("ledger %+v", l.Notifications)
	}
}

// Setting, changing and removing notify.webhook apply on reload. Entries
// open when the webhook is set are not back-filled, and nothing is posted
// while it is unset.
func TestNotifyReloadTurnsNotificationsOnAndOff(t *testing.T) {
	t.Parallel()
	first, second := newFakeWebhook(t), newFakeWebhook(t)
	opts, inbox := notifyFixture(t, "")
	first.records(opts)
	second.records(opts)
	opts.NotifyRetry = time.Minute
	e1 := entryOf(InboxEscalation, 1)
	inbox.set(e1)
	_, c := startNotifying(t, opts, inbox)
	ctx := context.Background()
	if _, err := os.Stat(filepath.Join(opts.Config.Root, "notifications.json")); !os.IsNotExist(err) {
		t.Fatalf("ledger without a webhook: %v", err)
	}

	setWebhook(t, opts, first.hook())
	_, err := c.Reload(ctx)
	must(t, err)
	cfg, err := c.Configuration(ctx)
	must(t, err)
	if cfg.Effective.Notify.Webhook != first.hook() {
		t.Fatalf("effective notify %+v", cfg.Effective.Notify)
	}
	// The pass the reload triggers records what is open as skipped; an entry
	// that opens before that pass reads the inbox would be skipped too.
	soon(t, "the open entry recorded as skipped", func() bool {
		l := readLedger(t, opts)
		return l.Enabled && l.Notifications[notificationKey(project, e1)] != nil
	})
	e2 := entryOf(InboxAmendment, 2)
	inbox.set(e1, e2)
	if got := first.await(t, 1); len(got) != 1 || !strings.Contains(got[0], "Kind: amendment") {
		t.Fatalf("posts after setting the webhook: %q", got)
	}
	if got := readLedger(t, opts).Notifications[notificationKey(project, e1)]; got == nil || got.State != notificationSkipped {
		t.Fatalf("entry open before the webhook was set: %+v", got)
	}

	setWebhook(t, opts, second.hook())
	_, err = c.Reload(ctx)
	must(t, err)
	e3 := entryOf(InboxContested, 3)
	inbox.set(e1, e2, e3)
	if got := second.await(t, 1); !strings.Contains(got[0], "Kind: contested") {
		t.Fatalf("posts to the changed webhook: %q", got)
	}

	// A post still pending when the webhook is removed is dropped.
	second.respond(http.StatusBadGateway)
	e3b := entryOf(InboxEscalation, 6)
	inbox.set(e1, e2, e3, e3b)
	second.await(t, 2)
	setWebhook(t, opts, "")
	_, err = c.Reload(ctx)
	must(t, err)
	soon(t, "notifications turned off", func() bool { return !readLedger(t, opts).Enabled })
	if rec := readLedger(t, opts).Notifications[notificationKey(project, e3b)]; rec.State != notificationDropped || rec.Attempts != 1 {
		t.Fatalf("pending when the webhook was removed: %+v", rec)
	}
	second.respond(http.StatusOK)
	e4 := entryOf(InboxDelivery, 4)
	inbox.set(e1, e2, e3, e3b, e4)

	setWebhook(t, opts, second.hook())
	_, err = c.Reload(ctx)
	must(t, err)
	e5 := entryOf(InboxRatification, 5)
	soon(t, "the entry opened while unset recorded", func() bool {
		return readLedger(t, opts).Notifications[notificationKey(project, e4)] != nil
	})
	inbox.set(e1, e2, e3, e3b, e4, e5)
	second.await(t, 3)
	settled(t, func() int { return len(second.received()) })
	if got := second.received(); len(got) != 3 || !strings.Contains(got[2], "Kind: ratification") || len(first.received()) != 1 {
		t.Fatalf("posts after turning notifications back on: %q and %q", first.received(), got)
	}
	if rec := readLedger(t, opts).Notifications[notificationKey(project, e4)]; rec.State != notificationSkipped {
		t.Fatalf("entry opened while unset: %+v", rec)
	}
}

// A failing webhook is retried with backoff and then given up on, which
// /v1/status and /v1/config report. An entry decided before its post
// succeeds is dropped, and a later successful post clears the diagnostic.
func TestNotifyFailingWebhookRetriesThenGivesUp(t *testing.T) {
	t.Parallel()
	hook := newFakeWebhook(t)
	hook.respond(http.StatusServiceUnavailable)
	opts, inbox := notifyFixture(t, hook.hook())
	hook.records(opts)
	opts.NotifyRetry = 100 * time.Millisecond
	_, c := startNotifying(t, opts, inbox)
	soon(t, "notifications on", func() bool { return readLedger(t, opts).Enabled })
	failing := entryOf(InboxEscalation, 1)
	inbox.set(failing)
	hook.await(t, 1)
	soon(t, "a retrying diagnostic", func() bool {
		st, cf := notifyDiagnostics(t, c)
		return len(st) == 1 && len(cf) == 1 && strings.Contains(st[0].Message, "; retrying")
	})
	start := time.Now()
	hook.await(t, notifyAttempts)
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Fatalf("retries without backoff: %d attempts in %s", notifyAttempts, elapsed)
	}
	want := Diagnostic{Field: "notify", Code: Unavailable, Message: ""}
	soon(t, "a gave-up diagnostic", func() bool {
		st, cf := notifyDiagnostics(t, c)
		return len(st) == 1 && len(cf) == 1 && st[0] == cf[0] && strings.HasPrefix(st[0].Message, "gave up notifying the owner of the escalation decision in workstream "+string(stream))
	})
	st, _ := notifyDiagnostics(t, c)
	if st[0].Code != want.Code || !strings.Contains(st[0].Message, "after 5 attempts: the webhook responded 503 Service Unavailable") || strings.Contains(st[0].Message, "secret") {
		t.Fatalf("diagnostic %+v", st[0])
	}
	settled(t, func() int { return len(hook.received()) })
	if n := len(hook.received()); n != notifyAttempts {
		t.Fatalf("%d posts after giving up", n)
	}
	if rec := readLedger(t, opts).Notifications[notificationKey(project, failing)]; rec.State != notificationGaveUp || rec.Attempts != notifyAttempts {
		t.Fatalf("gave up: %+v", rec)
	}

	decided := entryOf(InboxDelivery, 2)
	inbox.set(failing, decided)
	hook.await(t, notifyAttempts+1)
	inbox.set(failing)
	soon(t, "the decided entry dropped", func() bool {
		rec := readLedger(t, opts).Notifications[notificationKey(project, decided)]
		return rec != nil && rec.State == notificationDropped
	})
	if rec := readLedger(t, opts).Notifications[notificationKey(project, decided)]; rec.Attempts >= notifyAttempts || rec.Body != "" {
		t.Fatalf("dropped: %+v", rec)
	}

	hook.respond(http.StatusNoContent)
	inbox.set(failing, entryOf(InboxContested, 3))
	soon(t, "the diagnostic cleared", func() bool {
		st, cf := notifyDiagnostics(t, c)
		return len(st) == 0 && len(cf) == 0
	})
	if got := hook.received(); !strings.Contains(got[len(got)-1], "Kind: contested") {
		t.Fatalf("last post %q", got[len(got)-1])
	}
}

// Without a tailnet DNS name the Open line names the configured
// listen.tailnet hostname.
func TestNotifyLinksTheConfiguredTailnetHostname(t *testing.T) {
	t.Parallel()
	hook := newFakeWebhook(t)
	opts, inbox := notifyFixture(t, "")
	hook.records(opts)
	withTailnet(t, &opts)
	withListen(t, opts, "tailnet = \"osmia\"\n")
	setWebhook(t, opts, hook.hook())
	startNotifying(t, opts, inbox)
	soon(t, "notifications on", func() bool { return readLedger(t, opts).Enabled })
	inbox.set(entryOf(InboxDelivery, 1))
	if got := hook.await(t, 1); !strings.HasSuffix(got[0], "\nOpen: http://osmia/\n") {
		t.Fatalf("post %q", got[0])
	}
}

// A ledger that cannot be read or written, or an inbox that cannot be read,
// sends nothing and shows an internal notify diagnostic in /v1/status and
// /v1/config. Once the fault is gone the entry is posted and the diagnostic
// clears.
func TestNotifyFaultsSendNothingUntilCleared(t *testing.T) {
	t.Parallel()
	hook := newFakeWebhook(t)
	opts, inbox := notifyFixture(t, hook.hook())
	hook.records(opts)
	ledger := filepath.Join(opts.Config.Root, "notifications.json")
	// An unreadable ledger at start.
	must(t, os.WriteFile(ledger, []byte("{not json"), 0600))
	_, c := startNotifying(t, opts, inbox)
	fault := func(message string) {
		t.Helper()
		soon(t, message, func() bool {
			st, cf := notifyDiagnostics(t, c)
			want := Diagnostic{Field: "notify", Code: Internal, Message: message}
			return len(st) == 1 && len(cf) == 1 && st[0] == want && cf[0] == want
		})
	}
	cleared := func(step string) {
		t.Helper()
		soon(t, step, func() bool {
			st, cf := notifyDiagnostics(t, c)
			return len(st) == 0 && len(cf) == 0
		})
	}
	fault("cannot read " + ledger + "; nothing is sent until it can be read")
	e1 := entryOf(InboxEscalation, 1)
	inbox.set(e1)
	must(t, os.Remove(ledger))
	inbox.set(e1)
	cleared("the ledger readable again")
	if rec := readLedger(t, opts).Notifications[notificationKey(project, e1)]; rec == nil || rec.State != notificationSkipped {
		t.Fatalf("entry open while the ledger was unreadable: %+v", rec)
	}

	// A ledger that cannot be written: a directory takes its place.
	must(t, os.Remove(ledger))
	must(t, os.MkdirAll(filepath.Join(ledger, "blocked"), 0700))
	e2 := entryOf(InboxAmendment, 2)
	inbox.set(e1, e2)
	fault("cannot record notifications in " + ledger + "; nothing is sent until it can be written")
	settled(t, func() int { return len(hook.received()) })
	if got := hook.received(); len(got) != 0 {
		t.Fatalf("posted without a record: %q", got)
	}
	must(t, os.RemoveAll(ledger))
	inbox.set(e1, e2)
	if got := hook.await(t, 1); !strings.Contains(got[0], "Kind: amendment") {
		t.Fatalf("post after the ledger was writable again: %q", got)
	}
	cleared("the ledger writable again")

	// An unreadable inbox.
	inbox.breakInbox(true)
	fault("cannot read the inbox; notifications wait until it can be read")
	e3 := entryOf(InboxContested, 3)
	inbox.set(e1, e2, e3)
	settled(t, func() int { return len(hook.received()) })
	if got := hook.received(); len(got) != 1 {
		t.Fatalf("posted while the inbox was unreadable: %q", got)
	}
	inbox.breakInbox(false)
	if got := hook.await(t, 2); !strings.Contains(got[1], "Kind: contested") {
		t.Fatalf("post after the inbox was readable again: %q", got)
	}
	cleared("the inbox readable again")
}
