package executionjourney

import (
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	config "github.com/kpenfound/osmia/internal/config"

	trace "github.com/kpenfound/osmia/internal/trace"

	testing "testing"
)

const askedQuestion = intakefixture.AskedQuestion

var asks = intakefixture.Asks

type chief = intakefixture.Chief

const chiefRole = intakefixture.ChiefRole

const relayedRuling = intakefixture.RelayedRuling

func (f *shedFixture) answer(id string, run fakeTurn) string {
	return intakefixture.Answer(f.engine, id, run)
}

func (f *shedFixture) rule(t *testing.T, id string) {
	t.Helper()
	intakefixture.Rule(t, f.c, id)
}

func (f *shedFixture) thread(t *testing.T, stream config.WorkstreamID, member string) trace.Thread {
	t.Helper()
	return intakefixture.Thread(t, f.repository(), stream, member)
}

func (f *shedFixture) question(t *testing.T, stream config.WorkstreamID, id string) trace.QuestionState {
	t.Helper()
	return intakefixture.Question(t, f.repository(), stream, id)
}

func (f *shedFixture) awaitQuestion(t *testing.T, stream config.WorkstreamID, id, state string) {
	t.Helper()
	intakefixture.AwaitQuestion(t, f.repository(), stream, id, state)
}

func (f *shedFixture) awaitTurn(t *testing.T, stream config.WorkstreamID, member, turn string) trace.QueuedTurn {
	t.Helper()
	return intakefixture.AwaitNamedTurn(t, f.repository(), stream, member, turn)
}
