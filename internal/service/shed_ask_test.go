package service

import (
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"

	config "github.com/kpenfound/osmia/internal/config"
	trace "github.com/kpenfound/osmia/internal/trace"

	testing "testing"
)

const ownerRuling = intakefixture.OwnerRuling
const relayedRuling = intakefixture.RelayedRuling
const chiefRole = intakefixture.ChiefRole

type chief = intakefixture.Chief

// answer installs the fake behaviour of the turn that delivers the answer to
// question id to its asker.
func (f *shedFixture) answer(id string, run fakeTurn) string {
	return intakefixture.Answer(f.engine, id, run)
}

var asks = intakefixture.Asks

// rule has the owner rule on the inbox entry that escalated question id, once
// it is there.
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

// awaitQuestion waits until question id is in the given state.

// awaitTurn waits until the member's thread holds the given turn.
