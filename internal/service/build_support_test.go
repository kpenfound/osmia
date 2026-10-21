package service

import (
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

var conversationAwait = fixturekit.ConversationAwait
var streamDocuments = intakefixture.StreamDocuments

func approvedReview(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, unit string) (trace.Document, UnitReviewResult) {
	t.Helper()
	f := &foreman{masons: &masons{repository: repository}}
	return intakefixture.ApprovedReview(t, stream, unit, f.approval)
}

func awaitCheck[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	return intakefixture.AwaitCheck(t, ch)
}

var awaitPollSignal = fixturekit.AwaitPollSignal
