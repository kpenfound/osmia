package service

import (
	"context"
	"net/http"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/eventstream"
	"github.com/kpenfound/osmia/internal/readview"
)

const TestNotifyBudgetPause = NotifyBudgetPause

type TestNotification = notification

const TestNotificationSent = notificationSent

type TestPendingSession = pendingSession

var TestSessionActor = sessionActor

const TestSessionAgentID = sessionAgentID

var TestSessionBranch = sessionBranch
var TestSessionCheckRuns = sessionCheckRuns
var TestSessionCommitMessage = sessionCommitMessage
var TestSessionEntriesSince = sessionEntriesSince
var TestSessionGit = sessionGit
var TestSessionLock = sessionLock
var TestSessionRelay = sessionRelay

const TestSessionRolePrompt = sessionRolePrompt
const TestSessionsDirectory = sessionsDirectory

var TestSetSessionState = setSessionState
var TestTurnDirectories = turnDirectories
var TestRecordSessionEntry = recordSessionEntry
var TestErrSessionDiscarded = errSessionDiscarded
var TestEnsureChiefsOfStaff = ensureChiefsOfStaff

func (a TestAccess) WritePendingSession(p pendingSession) error   { return a.s.writePendingSession(p) }
func (a TestAccess) ReadPendingSession() (*pendingSession, error) { return a.s.readPendingSession() }
func (a TestAccess) CompleteSessionOpen(ctx context.Context, p pendingSession) (SessionView, error) {
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

type TestPendingProject = pendingProject

var TestConversationEntries = conversationEntries

func (a TestAccess) ValidateAddRepo(req RepoAddRequest) (config.Project, config.Project, *APIError) {
	return a.s.validateAddRepo(req)
}
func (a TestAccess) DailyBudget() *DailyBudgetStatus { return a.s.dailyBudgetStatus() }

var TestTraceUnit = traceUnit
var TestTraceCriterion = traceCriterion
var TestTraceCommit = traceCommit
var TestErrTraceNotFound = errTraceNotFound

const TestRulingRatification = rulingRatification
const TestUnitFromPlan = unitFromPlan
const TestCommitCandidate = commitCandidate

func ClientForTest(h *http.Client) *Client {
	return &Client{http: h, defaultTimeout: defaultClientTimeout}
}

func (a TestAccess) Publish(e Event) { a.s.hub.Publish(e) }

var TestNotificationKey = notificationKey

const TestNotificationSkipped = notificationSkipped
const TestNotificationDropped = notificationDropped
const TestNotificationGaveUp = notificationGaveUp
const TestNotifyAttempts = notifyAttempts

var TestFactoryInstructions = factoryInstructions
var TestEnsureLibrarianThread = ensureLibrarianThread
var TestFeedEntries = feedEntries
var TestFeedSelection = feedSelection
var TestStatusView = statusView
var TestRecoveryActor = recoveryActor

const TestLibrarianRole = librarianRole

func (a TestAccess) Hub() *eventstream.Hub { return a.s.hub }

type TestNotifier struct{ n *notifier }

func NotifierForTest(s *Service, retry time.Duration) *TestNotifier {
	return &TestNotifier{newNotifier(s, retry, nil)}
}
func (n *TestNotifier) Pass(ctx context.Context) time.Time { return n.n.pass(ctx) }
func (n *TestNotifier) Diagnostics() []Diagnostic          { return n.n.diagnostics() }
