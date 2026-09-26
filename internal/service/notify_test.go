package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// fakeWebhook records the bodies posted to it and answers with status.
type fakeWebhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
	status int
	// ledger, when set, is the notification ledger that must hold each
	// posted body as pending before the post arrives.
	ledger string
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	t.Helper()
	w := &fakeWebhook{status: http.StatusOK}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		defer w.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/hook/secret" || r.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("request %s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		if w.ledger != "" && !pendingIn(w.ledger, string(body)) {
			t.Errorf("posted before the ledger recorded it as pending: %q", body)
		}
		w.bodies = append(w.bodies, string(body))
		rw.WriteHeader(w.status)
	}))
	t.Cleanup(w.Close)
	return w
}

// pendingIn reports whether the ledger at path holds body as a pending
// notification.
func pendingIn(path, body string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var l notifyLedger
	if json.Unmarshal(data, &l) != nil {
		return false
	}
	for _, rec := range l.Notifications {
		if rec.State == notificationPending && rec.Body == body {
			return true
		}
	}
	return false
}

// records makes the webhook check that each post is recorded as pending in
// the ledger of opts' root first.
func (w *fakeWebhook) records(opts Options) *fakeWebhook {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ledger = filepath.Join(opts.Config.Root, "notifications.json")
	return w
}

func (w *fakeWebhook) hook() string { return w.URL + "/hook/secret" }

func (w *fakeWebhook) respond(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = status
}

func (w *fakeWebhook) received() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.bodies)
}

// await waits until the webhook has received n posts and returns them.
func (w *fakeWebhook) await(t *testing.T, n int) []string {
	t.Helper()
	soon(t, fmt.Sprintf("%d posts", n), func() bool { return len(w.received()) >= n })
	return w.received()
}

// fakeInbox is the inbox the notifier reads in place of the trace's.
type fakeInbox struct {
	mu      sync.Mutex
	entries []InboxEntry
	s       *Service
	broken  bool
}

func (f *fakeInbox) read(context.Context) (InboxResponse, *APIError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return InboxResponse{}, &APIError{Internal, "cannot read the inbox"}
	}
	return InboxResponse{Entries: slices.Clone(f.entries)}, nil
}

// set replaces the open entries and announces the change.
func (f *fakeInbox) set(entries ...InboxEntry) {
	f.mu.Lock()
	f.entries = entries
	s := f.s
	f.mu.Unlock()
	if s != nil {
		s.hub.publish(Event{Kind: EventInbox})
	}
}

// soon polls ok until it holds, failing with what after 20 seconds.
func soon(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// breakInbox makes the inbox unreadable, or readable again, and announces it.
func (f *fakeInbox) breakInbox(broken bool) {
	f.mu.Lock()
	f.broken = broken
	f.mu.Unlock()
	f.set(f.current()...)
}

func (f *fakeInbox) current() []InboxEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.entries)
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

// setWebhook rewrites the fixture's top-level file with notify.webhook set,
// or without a [notify] table when webhook is empty.
func setWebhook(t *testing.T, opts Options, webhook string) {
	t.Helper()
	path := filepath.Join(opts.Config.Root, "config.toml")
	top, err := os.ReadFile(path)
	must(t, err)
	text, _, _ := strings.Cut(string(top), "[notify]\n")
	if webhook != "" {
		text += "[notify]\nwebhook = " + fmt.Sprintf("%q", webhook) + "\n"
	}
	must(t, os.WriteFile(path, []byte(text), 0600))
}

func startNotifying(t *testing.T, opts Options, inbox *fakeInbox) (*Service, *Client) {
	t.Helper()
	s, c := start(t, opts)
	inbox.mu.Lock()
	inbox.s = s
	inbox.mu.Unlock()
	return s, c
}

func readLedger(t *testing.T, opts Options) notifyLedger {
	t.Helper()
	var l notifyLedger
	data, err := os.ReadFile(filepath.Join(opts.Config.Root, "notifications.json"))
	if os.IsNotExist(err) {
		return l
	}
	must(t, err)
	must(t, json.Unmarshal(data, &l))
	return l
}

// entryOf is an open inbox entry of kind, told apart by n.
func entryOf(kind string, n int) InboxEntry {
	e := InboxEntry{Kind: kind, Workstream: stream, OpenedAt: time.Date(2026, 9, 1, 12, 0, n, 0, time.UTC), Question: fmt.Sprintf("Decide %s %d?\nIt matters.", kind, n), Recommendation: fmt.Sprintf("Recommend %d.", n)}
	switch kind {
	case InboxEscalation:
		e.Number = n
	case InboxContested:
		e.Unit, e.Recommendation = fmt.Sprintf("unit-%d", n), ""
	case InboxAmendment:
		e.Amendment, e.Revision = fmt.Sprint(n), 1
	default:
		e.Revision = n
	}
	return e
}

func notifyDiagnostics(t *testing.T, c *Client) (status, cfg []Diagnostic) {
	t.Helper()
	st, err := c.Statuses(context.Background())
	must(t, err)
	cr, err := c.Configuration(context.Background())
	must(t, err)
	pick := func(ds []Diagnostic) []Diagnostic {
		var out []Diagnostic
		for _, d := range ds {
			if d.Field == "notify" {
				out = append(out, d)
			}
		}
		return out
	}
	return pick(st.Diagnostics), pick(cr.Diagnostics)
}

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
	for key, rec := range readLedger(t, opts).Notifications {
		if want := notificationSent; key != notificationKey(project, old) && rec.State != want {
			t.Fatalf("%s: %+v", key, rec)
		}
	}
	must(t, s.Close())

	// After a restart only the entry that opens since is posted.
	inbox.mu.Lock()
	inbox.s = nil
	inbox.mu.Unlock()
	startNotifying(t, opts, inbox)
	next := entryOf(InboxEscalation, 9)
	inbox.set(append(entries, next)...)
	bodies = hook.await(t, len(kinds)+1)
	time.Sleep(50 * time.Millisecond)
	if bodies = hook.received(); len(bodies) != len(kinds)+1 || !strings.Contains(bodies[len(kinds)], "Decide escalation 9?") {
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
	time.Sleep(50 * time.Millisecond)
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
	got := second.await(t, 3)
	time.Sleep(50 * time.Millisecond)
	if got = second.received(); len(got) != 3 || !strings.Contains(got[2], "Kind: ratification") || len(first.received()) != 1 {
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
	want := Diagnostic{"notify", Unavailable, ""}
	soon(t, "a gave-up diagnostic", func() bool {
		st, cf := notifyDiagnostics(t, c)
		return len(st) == 1 && len(cf) == 1 && st[0] == cf[0] && strings.HasPrefix(st[0].Message, "gave up notifying the owner of the escalation decision in workstream "+string(stream))
	})
	st, _ := notifyDiagnostics(t, c)
	if st[0].Code != want.Code || !strings.Contains(st[0].Message, "after 5 attempts: the webhook responded 503 Service Unavailable") || strings.Contains(st[0].Message, "secret") {
		t.Fatalf("diagnostic %+v", st[0])
	}
	time.Sleep(time.Second)
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
			want := Diagnostic{"notify", Internal, message}
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
	time.Sleep(50 * time.Millisecond)
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
	time.Sleep(50 * time.Millisecond)
	if got := hook.received(); len(got) != 1 {
		t.Fatalf("posted while the inbox was unreadable: %q", got)
	}
	inbox.breakInbox(false)
	if got := hook.await(t, 2); !strings.Contains(got[1], "Kind: contested") {
		t.Fatalf("post after the inbox was readable again: %q", got)
	}
	cleared("the inbox readable again")
}

// budgetNotifier is a notifier over a daily budget service whose webhook is
// hook, and the ledger path it writes.
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
// whose spend cannot be read waits and is reported.
func TestNotifyDailyBudgetPauseBeforeTheWebhookAndUnreadableSpend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hook := newFakeWebhook(t)
	s, _, repo, clock := dailyBudgetService(t, 0, nil)
	appendDayCost(t, repo, stream, "spent", clock.Now(), 1.25, true)
	must(t, dailyBudget{s: s, repository: repo}.Pass(ctx))
	n, _ := budgetNotifier(t, s, hook.hook())
	n.pass(ctx)
	if got := hook.received(); len(got) != 0 {
		t.Fatalf("posts for a pause open before the webhook: %q", got)
	}
	for _, rec := range n.ledger.Notifications {
		if rec.State != notificationSkipped {
			t.Fatalf("pause open before the webhook: %+v", rec)
		}
	}

	// A later pause cannot be described while today's spend is unreadable.
	jump(clock, time.Date(2026, 9, 17, 7, 0, 0, 0, time.UTC))
	must(t, dailyBudget{s: s, repository: repo}.Pass(ctx))
	appendDayCost(t, repo, sibling, "next-day", clock.Now(), 1, true)
	must(t, dailyBudget{s: s, repository: repo}.Pass(ctx))
	must(t, repo.Close())
	n.pass(ctx)
	if got := hook.received(); len(got) != 0 {
		t.Fatalf("posts with unreadable spend: %q", got)
	}
	if d := n.diagnostics(); len(d) != 1 || d[0].Message != "cannot read today's spend; the budget pause notification waits until it can be read" {
		t.Fatalf("diagnostics %+v", d)
	}
}
