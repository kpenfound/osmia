package service

import (
	"context"
	"net/http"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/eventstream"
	"github.com/kpenfound/osmia/internal/readview"
)

func (a TestAccess) WritePendingSession(p PendingSession) error   { return a.s.writePendingSession(p) }
func (a TestAccess) ReadPendingSession() (*PendingSession, error) { return a.s.readPendingSession() }
func (a TestAccess) CompleteSessionOpen(ctx context.Context, p PendingSession) (SessionView, error) {
	return a.s.completeSessionOpen(ctx, p)
}
func (a TestAccess) StatusCache() *readview.Cache[StatusResponse] { return &a.s.statusViews }
func (a TestAccess) SetConfig(cfg *config.Config) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.s.cfg = cfg
}

func (c *Client) SetSessionCheckTimeoutForTest(d time.Duration) { c.sessionCheckTimeout = d }
func (a TestAccess) SetReviewChecks(checks ReviewChecks)        { a.s.options.reviewChecks = checks }
func (a TestAccess) SetCloudChecks(checks ReviewChecks)         { a.s.options.cloudChecks = checks }

func (a TestAccess) DailyBudget() *DailyBudgetStatus { return a.s.dailyBudgetStatus() }

func ClientForTest(h *http.Client) *Client {
	return &Client{http: h, defaultTimeout: defaultClientTimeout}
}

func (a TestAccess) Publish(e Event) { a.s.hub.Publish(e) }

func (a TestAccess) Hub() *eventstream.Hub { return a.s.hub }

type TestNotifier struct{ n *notifier }

func NotifierForTest(s *Service, retry time.Duration) *TestNotifier {
	return &TestNotifier{newNotifier(s, retry, nil)}
}
func (n *TestNotifier) Pass(ctx context.Context) time.Time { return n.n.pass(ctx) }
func (n *TestNotifier) Diagnostics() []Diagnostic          { return n.n.diagnostics() }
