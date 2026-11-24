package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/runtime"
)

const NotifyAttempts = 5

const notifyPoll = time.Minute

const (
	notificationPending = "pending"
	NotificationSent    = "sent"
	NotificationSkipped = "skipped"
	NotificationDropped = "dropped"
	NotificationGaveUp  = "gave_up"
)

type Notification struct {
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

type notifyFailure struct {
	Kind       string    `json:"kind"`
	Workstream string    `json:"workstream"`
	Reason     string    `json:"reason"`
	Attempts   int       `json:"attempts"`
	GaveUp     bool      `json:"gave_up"`
	At         time.Time `json:"at"`
}

type notifyLedger struct {
	Enabled       bool                     `json:"enabled"`
	Notifications map[string]*Notification `json:"notifications"`
	Failure       *notifyFailure           `json:"failure,omitempty"`
}

type notifier struct {
	s      *Service
	inbox  func(context.Context) (InboxResponse, *APIError)
	client *http.Client
	retry  time.Duration
	done   chan struct{}

	mu     sync.Mutex
	ledger *notifyLedger
	fault  string
}

func newNotifier(s *Service, retry time.Duration, inbox func(context.Context) (InboxResponse, *APIError)) *notifier {
	if inbox == nil {
		inbox = s.inbox
	}
	return &notifier{s: s, inbox: inbox, client: &http.Client{Timeout: 30 * time.Second}, retry: retry, done: make(chan struct{})}
}

func (n *notifier) run(ctx context.Context) {
	defer close(n.done)
	var wake <-chan struct{}
	sub, ok := n.s.hub.Subscribe()
	if ok {
		defer n.s.hub.Unsubscribe(sub)
		wake = sub.Wake()
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
				waiting = !slices.ContainsFunc(sub.Take(), func(e Event) bool {
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

func (n *notifier) pass(ctx context.Context) time.Time {
	cfg := n.s.current()
	path, err := cfg.Root.Notifications()
	if err != nil {
		n.fail("cannot resolve the Notification ledger under the root; nothing is sent")
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
				rec.settle(NotificationDropped, n.s.now())
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
		occurrences = append(occurrences, occurrence{key: NotificationKey(cfg.Project.ID, e), kind: e.Kind, workstream: string(e.Workstream), body: notificationBody(cfg.Project.ID, e, link)})
	}
	pause, fault := n.budgetPause(cfg, link)
	if pause != nil {
		occurrences = append(occurrences, *pause)
	}
	landed, landedFault := n.landings(link)
	occurrences = append(occurrences, landed...)
	if fault == "" {
		fault = landedFault
	}
	now := n.s.now()
	open := map[string]bool{}
	n.mu.Lock()
	for _, o := range occurrences {
		open[o.key] = true
		if _, seen := ledger.Notifications[o.key]; seen {
			continue
		}
		rec := &Notification{State: notificationPending, Kind: o.kind, Workstream: o.workstream, Recorded: now}
		if !ledger.Enabled {
			rec.settle(NotificationSkipped, now)
		} else {
			rec.Body = o.body
		}
		ledger.Notifications[o.key] = rec
	}
	for key, rec := range ledger.Notifications {
		if rec.State == notificationPending && !open[key] && fault == "" {
			rec.settle(NotificationDropped, now)
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

type occurrence struct {
	key, kind, workstream, body string
}

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
			return nil, ""
		}
		status := n.s.dailyBudgetStatus()
		if status == nil {
			return nil, "no daily budget is configured; the budget pause Notification waits until one is"
		}
		key := strings.Join([]string{string(cfg.Project.ID), NotifyBudgetPause, p.SetAt.UTC().Format(time.RFC3339Nano)}, "/")
		return &occurrence{key: key, kind: NotifyBudgetPause, body: budgetPauseBody(cfg.Project.ID, status, day.end, link)}, ""
	}
	return nil, ""
}

// NotifyBudgetPause is the kind of the Notification for a daily budget pause.
const NotifyBudgetPause = "budget_pause"

// NotifyLanded is the kind of the Notification for a workstream the service
// landed on its base branch.
const NotifyLanded = "landed"

func (n *notifier) landings(link string) (landed []occurrence, fault string) {
	cfg, projects := n.s.runtimes()
	for _, p := range projects {
		if p.repository == nil || !cfg.Active(p.id) {
			continue
		}
		streams, err := p.repository.Workstreams()
		if err != nil {
			return landed, "cannot read the landings of project " + string(p.id) + "; landing notifications wait until they can be read"
		}
		for _, stream := range streams {
			landing, found, err := delivery.ReadDeliveryLanding(p.repository, stream)
			if err != nil {
				return landed, "cannot read the landing of workstream " + string(stream) + "; landing notifications wait until it can be read"
			}
			if !found {
				continue
			}
			key := strings.Join([]string{string(p.id), NotifyLanded, string(stream), landing.Commit, landing.At.UTC().Format(time.RFC3339Nano)}, "/")
			landed = append(landed, occurrence{key: key, kind: NotifyLanded, workstream: string(stream), body: landedBody(p.id, stream, landing, link)})
		}
	}
	return landed, ""
}

func landedBody(project config.ProjectID, stream config.WorkstreamID, landing delivery.DeliveryLanding, link string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Osmia landed a workstream on its base branch.\nProject: %s\nWorkstream: %s\nLanded commit: %s\nBase branch: %s of %s\nDecided by: %s\n", project, stream, landing.Commit, landing.BaseBranch, landing.Upstream, landing.DecidedBy)
	if link != "" {
		fmt.Fprintf(&b, "Open: %s\n", link)
	}
	return b.String()
}

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
		attemptStart := time.Now()
		err := n.post(ctx, webhook, body)
		if ctx.Err() != nil {
			return time.Time{}
		}
		now := n.s.now()
		n.mu.Lock()
		kind, workstream := rec.Kind, rec.Workstream
		var attempts int
		var gaveUp bool
		if err == nil {
			rec.settle(NotificationSent, now)
			n.ledger.Failure = nil
		} else {
			rec.Attempts++
			rec.LastError = err.Error()
			attempts = rec.Attempts
			failure := &notifyFailure{Kind: rec.Kind, Workstream: rec.Workstream, Reason: rec.LastError, Attempts: rec.Attempts, At: now}
			if rec.Attempts >= NotifyAttempts {
				rec.settle(NotificationGaveUp, now)
				failure.GaveUp, gaveUp = true, true
			} else {
				rec.Next = time.Now().Add(n.retry << (rec.Attempts - 1))
				if due.IsZero() || rec.Next.Before(due) {
					due = rec.Next
				}
			}
			n.ledger.Failure = failure
		}
		n.mu.Unlock()
		if err != nil {
			level := slog.LevelWarn
			if gaveUp {
				level = slog.LevelError
			}
			n.s.log().Log(ctx, level, "Notification post attempt failed", "kind", kind, "workstream", workstream, "attempts", attempts, "gave_up", gaveUp, "duration", time.Since(attemptStart).String(), "error", err.Error())
		} else {
			n.s.log().Info("Notification post attempt succeeded", "kind", kind, "workstream", workstream, "duration", time.Since(attemptStart).String())
		}
		if !n.save(path) {
			return time.Time{}
		}
	}
	return due
}

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

func (rec *Notification) settle(state string, at time.Time) {
	rec.State, rec.Body, rec.Next, rec.Settled = state, "", time.Time{}, at
}

func NotificationKey(project config.ProjectID, e InboxEntry) string {
	return strings.Join([]string{string(project), e.Kind, string(e.Workstream), fmt.Sprint(e.Number), e.Unit, e.Amendment, fmt.Sprint(e.Revision), e.OpenedAt.UTC().Format(time.RFC3339Nano)}, "/")
}

func notificationBody(project config.ProjectID, e InboxEntry, link string) string {
	var b strings.Builder
	opening := "Osmia needs your decision."
	if e.Kind == api.InboxPublication {
		opening = "Osmia cannot publish a pull request."
	}
	if e.Kind == InboxDrift {
		opening = "Osmia holds a drift rebase for you."
	}
	if e.Kind == InboxNotices {
		opening = "Osmia cannot reach the chief of staff."
	}
	if e.Kind == InboxLoop {
		opening = "Osmia paused a workstream that is not making progress."
	}
	fmt.Fprintf(&b, "%s\nProject: %s\nWorkstream: %s\nKind: %s\nQuestion: %s\n", opening, project, e.Workstream, e.Kind, oneLine(e.Question))
	if r := oneLine(e.Recommendation); r != "" {
		fmt.Fprintf(&b, "Recommendation: %s\n", r)
	}
	if link != "" {
		fmt.Fprintf(&b, "Open: %s\n", link)
	}
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func (n *notifier) link(ctx context.Context, cfg *config.Config) string {
	if cfg.Listen.Tailnet == "" || n.s.tailnet == nil {
		return ""
	}
	if status := n.s.tailnetStatus(ctx); status.HTTPSState == api.TailnetHTTPSUp {
		return status.HTTPSAddress
	}
	host := cfg.Listen.Tailnet
	names, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if list := n.s.tailnetNames(names); len(list) > 0 {
		host = list[0]
	}
	return "http://" + host + "/"
}

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
		ledger.Notifications = map[string]*Notification{}
	}
	n.ledger = ledger
	return ledger, nil
}

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

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

func (n *notifier) fail(reason string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.fault = reason
}

func (n *notifier) diagnostics() []Diagnostic {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []Diagnostic
	if n.fault != "" {
		out = append(out, Diagnostic{Field: "notify", Code: Internal, Message: n.fault})
	}
	if n.ledger == nil || n.ledger.Failure == nil {
		return out
	}
	f := n.ledger.Failure
	what := fmt.Sprintf("the %s decision in workstream %s", f.Kind, f.Workstream)
	switch f.Kind {
	case NotifyBudgetPause:
		what = "the daily budget pause"
	case NotifyLanded:
		what = "the landing of workstream " + f.Workstream
	}
	message := fmt.Sprintf("notifying the owner of %s failed at %s (attempt %d of %d): %s; retrying", what, f.At.Format(time.RFC3339), f.Attempts, NotifyAttempts, f.Reason)
	if f.GaveUp {
		message = fmt.Sprintf("gave up notifying the owner of %s at %s after %d attempts: %s", what, f.At.Format(time.RFC3339), f.Attempts, f.Reason)
	}
	return append(out, Diagnostic{Field: "notify", Code: Unavailable, Message: message})
}
