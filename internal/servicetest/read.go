package servicetest

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// AwaitDesignConversation subscribes before reading and awaits the durable reply.
func AwaitDesignConversation(t *testing.T, s *service.Service, c *service.Client, id config.WorkstreamID, done func(api.ConversationResponse) bool) api.ConversationResponse {
	t.Helper()
	wait, release := ServiceChanges(t, s)
	defer release()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		list, err := c.DesignConversation(context.Background(), id)
		fixturekit.Must(t, err)
		if done(list) {
			return list
		}
		if time.Now().After(deadline) {
			t.Fatalf("design conversation did not settle: %+v", list)
		}
		wait()
	}
}

// ReadFeed reads the real feed endpoint.
func ReadFeed(t *testing.T, c *service.Client, id config.WorkstreamID) api.FeedResponse {
	t.Helper()
	var out api.FeedResponse
	err := c.Do(context.Background(), "GET", api.Prefix+"/feed/"+string(id), nil, &out)
	fixturekit.Must(t, err)
	return out
}
