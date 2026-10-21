package executionjourney

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const BuildAction = service.BuildAction

type ChangeDecision = service.ChangeDecision
type CheckSlotRun = service.CheckSlotRun
type CheckSlotStatus = service.CheckSlotStatus

const DraftAction = service.DraftAction
const Prefix = service.Prefix
const RebaseAction = service.RebaseAction
const RedraftAction = service.RedraftAction
const SealAction = service.SealAction

var archiveResolved = lifecycle.ArchiveResolved
var await = intakefixture.Await

const baseWaitSubject = lifecycle.BaseWaitSubject

var changeHeld = lifecycle.ChangeHeld

const checksRole = execution.ChecksRole

var concurrentOperation = service.TestConcurrentOperation
var conversationOperation = service.TestConversationOperation

func driftOperations(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []trace.OperationRecord {
	return intakefixture.DriftOperations(t, repo, stream, DriftAction)
}

var ensureSessionThread = service.TestEnsureSessionThread
var incompleteFinalChecks = delivery.IncompleteFinalChecks
var jump = fixturekit.Jump
var operationClass = service.TestOperationClass

const otherStream = fixturekit.OtherStream

var passingChecks = fixturekit.PassingChecks

type replier = intake.Replier

var roleCapacity = fixturekit.RoleCapacityEntry

var roundTurnID = intake.RoundTurnID
var sealingActor = intake.SealingActor

var operationRecord = fixturekit.OperationRecord

type turnsFunc = fixturekit.TurnsFunc
type completedEffect = fixturekit.CompletedEffect

const sibling config.WorkstreamID = "w_0123456789abcdef0123456789abcdee"
const ReplyAction = intake.ReplyAction
const AmendmentDraftAction = intake.AmendmentDraftAction
const AmendmentReplyAction = intake.AmendmentReplyAction
const ExtractAction = service.ExtractAction

var stagePriority = service.TestStagePriority

func (s *Service) setReviewChecks(checks ReviewChecks) {
	s.options.reviewChecks = checks
	InspectForTest(s).SetReviewChecks(checks)
}
func (s *Service) decideChange(ctx context.Context, raw, id string, d ChangeDecision) (WorkgroupChange, *APIError) {
	return s.lifecycle().DecideChange(ctx, raw, id, d)
}
func recordUpstreamOwnPullRequest(t *testing.T, s *Service, repo *trace.Repository, stream config.WorkstreamID, p DeliveryPublication) {
	t.Helper()
	fixturekit.UpstreamOwnPullRequest(t, repo, stream, p, s.now(), foremanActor, publicationDocument, publicationPath)
}
func (c *checkers) incompleteSinceEntry(stream config.WorkstreamID, unit string, latest UnitCheckRun, runs []UnitCheckRun) (int, error) {
	return c.driver().IncompleteSinceEntry(stream, unit, latest, runs)
}
func (d *finalReviewer) rebase(stream config.WorkstreamID, k int) (FinalRebase, bool, error) {
	return d.driver().Rebase(stream, k)
}
func (f *shedFixture) awaitShed(t *testing.T, stream config.WorkstreamID, want string, never ...string) {
	t.Helper()
	f.scenario().AwaitShed(t, stream, want, never...)
}
