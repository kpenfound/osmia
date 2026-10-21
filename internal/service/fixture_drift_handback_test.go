package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

// chiefTurn claims a new chief-of-staff turn of the workstream and returns
// its scope.
func chiefTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID, id string) coreadapter.Scope {
	t.Helper()
	return intakefixture.ClaimChiefTurn(t, repository, f.project, stream, f.clock.Now, intakefixture.ChiefTurnInput{
		Turn: id, Prompt: "The drift rebase is held.", Actor: serviceActor, Cause: "fixture", Token: "token-" + id, Directory: id,
	})
}
