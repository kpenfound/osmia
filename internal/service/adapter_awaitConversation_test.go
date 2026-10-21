package service

import (
	context "context"
	intakefixture "github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	testing "testing"
)

// awaitConversation polls the listing until done accepts it.
func awaitConversation(t *testing.T, c *Client, done func(ConversationResponse) bool) ConversationResponse {
	t.Helper()
	return intakefixture.AwaitView(t, func() (ConversationResponse, error) {
		return c.Conversation(context.Background(), stream)
	}, done, "conversation did not settle: %+v")
}
