package intakefixture

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// AwaitView waits until the read response satisfies done.
func AwaitView[T any](t *testing.T, read func() (T, error), done func(T) bool, failure string) T {
	t.Helper()
	var view T
	wake, release := fixturekit.Ticks(20 * time.Millisecond)
	defer release()
	fixturekit.Until(t, "the view to settle", wake, func() bool {
		var err error
		view, err = read()
		fixturekit.Must(t, err)
		return done(view)
	})
	return view
}

// ConversationReader reads a workstream's conversation.
type ConversationReader interface {
	Conversation(context.Context, config.WorkstreamID) (api.ConversationResponse, error)
}

// AwaitConversation waits until the fixture workstream's conversation satisfies done.
func AwaitConversation(t *testing.T, c ConversationReader, done func(api.ConversationResponse) bool) api.ConversationResponse {
	t.Helper()
	return AwaitView(t, func() (api.ConversationResponse, error) {
		return c.Conversation(context.Background(), fixturekit.Stream)
	}, done, "conversation did not settle: %+v")
}
