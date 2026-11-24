package journey

import (
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

// NotifyFixture returns fixture options whose inbox is scripted and whose
// notifier retries quickly. A non-empty webhook is written into the root's
// notify settings.
func NotifyFixture(t *testing.T, webhook string) (Options, *fixturekit.NotificationInbox) {
	t.Helper()
	opts := Fixture(t)
	inbox := fixturekit.NewNotifyInbox(t, opts.Config.Root, webhook)
	opts.NotifyInbox = inbox.Read
	opts.NotifyRetry = fixturekit.NotifyRetry
	return opts, inbox
}

// StartNotifying starts a service on opts and makes inbox changes reach it
// as inbox events.
func StartNotifying(t *testing.T, opts Options, inbox *fixturekit.NotificationInbox) (*Service, *Client) {
	t.Helper()
	s, c := StartT(t, opts)
	inbox.Observe(func() { service.InspectForTest(s.Core()).Publish(api.Event{Kind: api.EventInbox}) })
	return s, c
}

// WithTailnet joins the named tailnet nodes for opts and returns the node
// the service joined as.
func WithTailnet(t *testing.T, opts *Options, names ...string) **fixturekit.TailnetNode {
	t.Helper()
	join, joined := fixturekit.TailnetJoiner(names...)
	opts.JoinTailnet = func(hostname, dir string) (service.TailnetListener, error) { return join(hostname, dir) }
	return joined
}

// EventTraceOptions returns conversation fixture options over a root whose
// trace holds the seeded event records.
func EventTraceOptions(t *testing.T) Options {
	t.Helper()
	opts, cfg := ConversationFixture(t, "evs-")
	fixturekit.SeedEventTrace(t, cfg)
	return opts
}

// EventTraceFixture starts a service on EventTraceOptions.
func EventTraceFixture(t *testing.T) (*Service, *Client) {
	t.Helper()
	return StartT(t, EventTraceOptions(t))
}
