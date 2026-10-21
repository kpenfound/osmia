package executionjourney

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func reviewTurn(t *testing.T, f *shedFixture, repository *trace.Repository, stream config.WorkstreamID) trace.TurnRequest {
	t.Helper()
	r := &reviewers{masons: newMasonController(f.s, repository)}
	must(t, r.Pass(context.Background()))
	th, err := repository.Thread(stream, reviewerAgent("resume"))
	must(t, err)
	if len(th.Turns) == 0 {
		t.Fatal("no review turn was queued")
	}
	return th.Turns[len(th.Turns)-1].Request
}
