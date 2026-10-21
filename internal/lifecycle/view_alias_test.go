package lifecycle_test

import (
	"github.com/kpenfound/osmia/internal/intake"
	lifecycle "github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/service"
)

var recoveryActor = service.TestRecoveryActor
var pendingMergeOrderReferences = lifecycle.PendingMergeOrderReferences

const designStartSubject = intake.DesignStartSubject
