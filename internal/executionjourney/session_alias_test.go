package executionjourney

import "github.com/kpenfound/osmia/internal/service"

const NotifyBudgetPause = service.TestNotifyBudgetPause

type notification = service.TestNotification

const notificationPending = service.TestNotificationPending
const notificationSent = service.TestNotificationSent

type pendingSession = service.TestPendingSession

var sessionActor = service.TestSessionActor

const sessionAgentID = service.TestSessionAgentID

var sessionBranch = service.TestSessionBranch
var sessionCheckRuns = service.TestSessionCheckRuns
var sessionCommitMessage = service.TestSessionCommitMessage
var sessionEntriesSince = service.TestSessionEntriesSince
var sessionGit = service.TestSessionGit
var sessionLock = service.TestSessionLock
var sessionRelay = service.TestSessionRelay

const sessionRole = service.TestSessionAgentID
const sessionRolePrompt = service.TestSessionRolePrompt
const sessionsDirectory = service.TestSessionsDirectory

var setSessionState = service.TestSetSessionState
var turnDirectories = service.TestTurnDirectories
var recordSessionEntry = service.TestRecordSessionEntry
var errSessionDiscarded = service.TestErrSessionDiscarded
var ensureChiefsOfStaff = service.TestEnsureChiefsOfStaff
