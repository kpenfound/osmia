package lifecycle_test

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
)

var baseEntry = service.TestBaseEntry
var reapSandboxes = service.TestReapSandboxes
var masonGrant = service.TestMasonGrant
var reviewerGrant = service.TestReviewerGrant
var driftMasonTools = service.TestDriftMasonTools
var chiefContext = service.TestChiefContext
var concurrentOperation = service.TestConcurrentOperation

const discoveryTool = "record_discovery"
const coreReadBound = time.Second

type notifyLedger = service.TestNotifyLedger

type testNotifier struct {
	s     *Service
	retry time.Duration
}

func newNotifier(s *Service, retry time.Duration, _ any) *testNotifier {
	return &testNotifier{s, retry}
}
func (n *testNotifier) pass(ctx context.Context) {
	service.InspectForTest(n.s.core()).NotifyPass(ctx, n.retry)
}

type threadWorkspaces struct {
	units  unitWorkspaces
	drifts resolutions
}

func (w threadWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	return service.ThreadWorkspaceAcquireForTest(ctx, w.units.streamWorkspaces.Cfg, w.units.streamWorkspaces.Repository, req)
}
