package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
)

// notifyAttempts is how many times one notification is posted before the
// notifier gives up on it.
const notifyAttempts = 5

// notifyPoll is how long the notifier waits for a change before it reads the
// inbox again anyway.
const notifyPoll = time.Minute

// Notification states in the ledger.
const (
	notificationPending = "pending"
	notificationSent    = "sent"
	notificationSkipped = "skipped"
	notificationDropped = "dropped"
	notificationGaveUp  = "gave_up"
)

// notification is one inbox entry's occurrence in the ledger. Body is kept
// until the notification settles.
type notification struct {
	State      string    `json:"state"`
	Kind       string    `json:"kind"`
	Workstream string    `json:"workstream"`
	Body       string    `json:"body,omitempty"`
	Attempts   int       `json:"attempts,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	Next       time.Time `json:"next,omitzero"`
	Recorded   time.Time `json:"recorded"`
	Settled    time.Time `json:"settled,omitzero"`
}

// notifyFailure is the latest failed post that no successful post has
// followed.
type notifyFailure struct {
	Kind       string    `json:"kind"`
	Workstream string    `json:"workstream"`
	Reason     string    `json:"reason"`
	Attempts   int       `json:"attempts"`
	GaveUp     bool      `json:"gave_up"`
	At         time.Time `json:"at"`
}

// notifyLedger is the durable record of notifications, keyed by the inbox
// entry's identity and revision. Enabled says whether the last pass ran with
// a webhook, so a pass that finds it newly set records the entries already
// open as skipped.
type notifyLedger struct {
	Enabled       bool                     `json:"enabled"`
	Notifications map[string]*notification `json:"notifications"`
	Failure       *notifyFailure           `json:"failure,omitempty"`
}

// notifier posts every inbox entry that opens while notify.webhook is set to
// the webhook, once, off the scheduling path. Only its run loop changes the
// ledger; mu lets the status and configuration views read it meanwhile.
type notifier struct {
	s      *Service
	inbox  func(context.Context) (InboxResponse, *APIError)
	client *http.Client
	retry  time.Duration
	done   chan struct{}

	mu     sync.Mutex
	ledger *notifyLedger // nil until the ledger file is read
	fault  string        // why the last pass could not finish, or empty
}

func newNotifier(s *Service, retry time.Duration, inbox func(context.Context) (InboxResponse, *APIError)) *notifier {
	if inbox == nil {
		inbox = s.inbox
	}
	return &notifier{s: s, inbox: inbox, client: &http.Client{Timeout: 30 * time.Second}, retry: retry, done: make(chan struct{})}
}

// run passes over the inbox at start, whenever the inbox or the configuration
// changes, when a retry is due, and every notifyPoll, until ctx ends.
func (n *notifier) run(ctx context.Context) {
	defer close(n.done)
	var wake <-chan struct{}
	sub, ok := n.s.hub.subscribe()
	if ok {
		defer n.s.hub.unsubscribe(sub)
		wake = sub.wake
	}
	timer := time.NewTimer(notifyPoll)
	defer timer.Stop()
	for {
		due := n.pass(ctx)
		wait := notifyPoll
		if !due.IsZero() {
			wait = min(wait, max(time.Until(due), 0))
		}
		timer.Reset(wait)
		for waiting := true; waiting; {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				waiting = false
			case <-wake:
				waiting = !slices.ContainsFunc(sub.take(), func(e Event) bool {
					return e.Kind == EventInbox || e.Kind == EventRuntime || e.Kind == EventConfig || e.Kind == EventResync
				})
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

// pass records the inbox's new entries and drops pending ones that closed,
// then posts the pending notifications that are due. It returns when the next
// retry is due, or zero when none is.
func (n *notifier) pass(ctx context.Context) time.Time {
	cfg := n.s.current()
	path, err := cfg.Root.Notifications()
	if err != nil {
		n.fail("cannot resolve the notification ledger under the root; nothing is sent")
		return time.Time{}
	}
	ledger, err := n.load(path)
	if err != nil {
		n.fail("cannot read " + path + "; nothing is sent until it can be read")
		return time.Time{}
	}
	webhook := cfg.Notify.Webhook
	if webhook == "" {
		if !ledger.Enabled {
			n.fail("")
			return time.Time{}
		}
		n.mu.Lock()
		ledger.Enabled, ledger.Failure = false, nil
		for _, rec := range ledger.Notifications {
			if rec.State == notificationPending {
				rec.settle(notificationDropped, n.s.now())
			}
		}
		n.mu.Unlock()
		if !n.save(path) {
			return time.Time{}
		}
		n.fail("")
		return time.Time{}
	}
	inbox, api := n.inbox(ctx)
	if api != nil {
		n.fail("cannot read the inbox; notifications wait until it can be read")
		return time.Time{}
	}
	link := n.link(ctx, cfg)
	occurrences := make([]occurrence, 0, len(inbox.Entries)+1)
	for _, e := range inbox.Entries {
		occurrences = append(occurrences, occurrence{key: notificationKey(cfg.Project.ID, e), kind: e.Kind, workstream: string(e.Workstream), body: notificationBody(cfg.Project.ID, e, link)})
	}
	pause, fault := n.budgetPause(cfg, link)
	if pause != nil {
		occurrences = append(occurrences, *pause)
	}
	now := n.s.now()
	open := map[string]bool{}
	n.mu.Lock()
	for _, o := range occurrences {
		open[o.key] = true
		if _, seen := ledger.Notifications[o.key]; seen {
			continue
		}
		rec := &notification{State: notificationPending, Kind: o.kind, Workstream: o.workstream, Recorded: now}
		if !ledger.Enabled {
			rec.settle(notificationSkipped, now)
		} else {
			rec.Body = o.body
		}
		ledger.Notifications[o.key] = rec
	}
	for key, rec := range ledger.Notifications {
		if rec.State == notificationPending && !open[key] && fault == "" {
			rec.settle(notificationDropped, now)
		}
	}
	ledger.Enabled = true
	n.mu.Unlock()
	if !n.save(path) {
		return time.Time{}
	}
	n.fail(fault)
	return n.send(ctx, path, webhook, open)
}

// occurrence is one thing to notify the owner of: an open inbox entry or the
// daily budget pause in force.
type occurrence struct {
	key, kind, workstream, body string
}

// budgetPause is the occurrence for the daily budget pause in force, or nil
// when there is none. fault is set when the pause cannot be described yet, in
// which case pending notifications are kept rather than dropped.
func (n *notifier) budgetPause(cfg *config.Config, link string) (pause *occurrence, fault string) {
	if n.s.store == nil {
		return nil, ""
	}
	state, _ := n.s.store.Snapshot()
	for _, p := range state.Pauses {
		if p.Target != factoryTarget || p.Source != runtime.PauseDailyBudget {
			continue
		}
		day := n.s.localDay()
		if day.of(p.SetAt) != day.date {
			// The pause expired at midnight and awaits its clearing.
			return nil, ""
		}
		status, diagnostic := n.s.dailyBudgetStatus()
		if diagnostic != nil || status == nil {
			return nil, "cannot read today's spend; the budget pause notification waits until it can be read"
		}
		key := strings.Join([]string{string(cfg.Project.ID), NotifyBudgetPause, p.SetAt.UTC().Format(time.RFC3339Nano)}, "/")
		return &occurrence{key: key, kind: NotifyBudgetPause, body: budgetPauseBody(cfg.Project.ID, status, day.end, link)}, ""
	}
	return nil, ""
}

// NotifyBudgetPause is the kind of the notification for a daily budget pause.
const NotifyBudgetPause = "budget_pause"

// budgetPauseBody is the plain-text post for a daily budget pause.
func budgetPauseBody(project config.ProjectID, status *DailyBudgetStatus, clears time.Time, link string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Osmia paused dispatch: the daily budget is reached.\nProject: %s\nSpend: USD %s", project, status.SpendUSD)
	if status.LowerBound {
		b.WriteString(" or more")
	}
	fmt.Fprintf(&b, "\nLimit: USD %s\nClears: %s\n", status.LimitUSD, clears.Format(time.RFC3339))
	if link != "" {
		fmt.Fprintf(&b, "Open: %s\n", link)
	}
	return b.String()
}

// send posts every due pending notification that is still open, oldest first, recording each
// result before the next post. A post interrupted by the service stopping is
// not recorded, so a notification the webhook already accepted stays pending
// and is posted again after a restart: delivery is at least once.
func (n *notifier) send(ctx context.Context, path, webhook string, open map[string]bool) time.Time {
	n.mu.Lock()
	var keys []string
	for key, rec := range n.ledger.Notifications {
		if rec.State == notificationPending && open[key] {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b string) int {
		if c := n.ledger.Notifications[a].Recorded.Compare(n.ledger.Notifications[b].Recorded); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	n.mu.Unlock()
	var due time.Time
	for _, key := range keys {
		n.mu.Lock()
		rec := n.ledger.Notifications[key]
		next, body := rec.Next, rec.Body
		n.mu.Unlock()
		if time.Now().Before(next) {
			if due.IsZero() || next.Before(due) {
				due = next
			}
			continue
		}
		err := n.post(ctx, webhook, body)
		if ctx.Err() != nil {
			return time.Time{}
		}
		now := n.s.now()
		n.mu.Lock()
		if err == nil {
			rec.settle(notificationSent, now)
			n.ledger.Failure = nil
		} else {
			rec.Attempts++
			rec.LastError = err.Error()
			failure := &notifyFailure{Kind: rec.Kind, Workstream: rec.Workstream, Reason: rec.LastError, Attempts: rec.Attempts, At: now}
			if rec.Attempts >= notifyAttempts {
				rec.settle(notificationGaveUp, now)
				failure.GaveUp = true
			} else {
				rec.Next = time.Now().Add(n.retry << (rec.Attempts - 1))
				if due.IsZero() || rec.Next.Before(due) {
					due = rec.Next
				}
			}
			n.ledger.Failure = failure
		}
		n.mu.Unlock()
		if !n.save(path) {
			return time.Time{}
		}
	}
	return due
}

// post sends body to the webhook as plain text. Its error never carries the
// webhook's URL, which may hold a secret.
func (n *notifier) post(ctx context.Context, webhook, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, strings.NewReader(body))
	if err != nil {
		return errors.New("cannot build the request")
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := n.client.Do(req)
	if err != nil {
		var failed *url.Error
		if errors.As(err, &failed) {
			err = failed.Err
		}
		return fmt.Errorf("the request failed: %v", err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the webhook responded %s", resp.Status)
	}
	return nil
}

func (rec *notification) settle(state string, at time.Time) {
	rec.State, rec.Body, rec.Next, rec.Settled = state, "", time.Time{}, at
}

// notificationKey identifies one occurrence of an inbox entry: its project,
// kind, workstream, number, unit, amendment, revision and opening time.
func notificationKey(project config.ProjectID, e InboxEntry) string {
	return strings.Join([]string{string(project), e.Kind, string(e.Workstream), fmt.Sprint(e.Number), e.Unit, e.Amendment, fmt.Sprint(e.Revision), e.OpenedAt.UTC().Format(time.RFC3339Nano)}, "/")
}

// notificationBody is the plain-text post for an inbox entry.
func notificationBody(project config.ProjectID, e InboxEntry, link string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Osmia needs your decision.\nProject: %s\nWorkstream: %s\nKind: %s\nQuestion: %s\n", project, e.Workstream, e.Kind, oneLine(e.Question))
	if r := oneLine(e.Recommendation); r != "" {
		fmt.Fprintf(&b, "Recommendation: %s\n", r)
	}
	if link != "" {
		fmt.Fprintf(&b, "Open: %s\n", link)
	}
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// link is the page's address on the tailnet: the node's first tailnet name,
// or its configured hostname while it has none. It is empty without
// listen.tailnet.
func (n *notifier) link(ctx context.Context, cfg *config.Config) string {
	if cfg.Listen.Tailnet == "" || n.s.tailnet == nil {
		return ""
	}
	host := cfg.Listen.Tailnet
	names, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if list := n.s.tailnetNames(names); len(list) > 0 {
		host = list[0]
	}
	return "http://" + host + "/"
}

// load returns the ledger, reading it on first use; a missing file is an
// empty ledger.
func (n *notifier) load(path string) (*notifyLedger, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ledger != nil {
		return n.ledger, nil
	}
	ledger := &notifyLedger{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, ledger); err != nil {
			return nil, err
		}
	}
	if ledger.Notifications == nil {
		ledger.Notifications = map[string]*notification{}
	}
	n.ledger = ledger
	return ledger, nil
}

// save writes the ledger atomically. A failure is reported as the fault and
// stops the pass, so nothing is posted that the ledger does not hold.
func (n *notifier) save(path string) bool {
	n.mu.Lock()
	data, err := json.MarshalIndent(n.ledger, "", "  ")
	n.mu.Unlock()
	if err == nil {
		err = writeFileSynced(path, append(data, '\n'))
	}
	if err != nil {
		n.fail("cannot record notifications in " + path + "; nothing is sent until it can be written")
		return false
	}
	return true
}

// writeFileSynced replaces path with data through a synced temporary file
// beside it, so a reader sees the old contents or the new ones.
func writeFileSynced(path string, data []byte) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"-"+rand.Text()+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (n *notifier) fail(reason string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.fault = reason
}

// diagnostics reports why notifications cannot be sent, and the latest failed
// post that no successful post has followed.
func (n *notifier) diagnostics() []Diagnostic {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []Diagnostic
	if n.fault != "" {
		out = append(out, Diagnostic{"notify", Internal, n.fault})
	}
	if n.ledger == nil || n.ledger.Failure == nil {
		return out
	}
	f := n.ledger.Failure
	what := fmt.Sprintf("the %s decision in workstream %s", f.Kind, f.Workstream)
	if f.Kind == NotifyBudgetPause {
		what = "the daily budget pause"
	}
	message := fmt.Sprintf("notifying the owner of %s failed at %s (attempt %d of %d): %s; retrying", what, f.At.Format(time.RFC3339), f.Attempts, notifyAttempts, f.Reason)
	if f.GaveUp {
		message = fmt.Sprintf("gave up notifying the owner of %s at %s after %d attempts: %s", what, f.At.Format(time.RFC3339), f.Attempts, f.Reason)
	}
	return append(out, Diagnostic{"notify", Unavailable, message})
}
