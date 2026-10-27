package lifecycle_test

import (
	lifecycle "github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
)

var recoveryActor = service.TestRecoveryActor
var pendingMergeOrderReferences = lifecycle.PendingMergeOrderReferences
