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
	s      *Service
	retry  time.Duration
	driver *service.TestNotifier
}

func newNotifier(s *Service, retry time.Duration, _ any) *testNotifier {
	return &testNotifier{s: s, retry: retry, driver: service.NotifierForTest(s.core(), retry)}
}
func (n *testNotifier) pass(ctx context.Context) {
	n.driver.Pass(ctx)
}

type threadWorkspaces struct {
	units  unitWorkspaces
	drifts resolutions
}

func (w threadWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	return service.ThreadWorkspaceAcquireForTest(ctx, w.units.streamWorkspaces.Cfg, w.units.streamWorkspaces.Repository, req)
}

func (n *testNotifier) diagnostics() []service.Diagnostic { return n.driver.Diagnostics() }
