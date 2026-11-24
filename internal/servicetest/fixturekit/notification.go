package fixturekit

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
)

// NotifyRetry is the interval at which a fixture service retries a failed
// notification delivery.
const NotifyRetry = 10 * time.Millisecond

// NewNotifyInbox returns an empty scripted inbox and, when webhook is not
// empty, writes it into the notify settings of the configuration under root.
func NewNotifyInbox(t *testing.T, root, webhook string) *NotificationInbox {
	t.Helper()
	if webhook != "" {
		SetWebhook(t, root, webhook)
	}
	return &NotificationInbox{}
}

// NotificationInbox supplies scripted inbox reads and change notifications.
type NotificationInbox struct {
	mu      sync.Mutex
	entries []api.InboxEntry
	broken  bool
	publish func()
}

func (f *NotificationInbox) Read(context.Context) (api.InboxResponse, *api.APIError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return api.InboxResponse{}, &api.APIError{Code: api.Internal, Message: "cannot read the inbox"}
	}
	return api.InboxResponse{Entries: slices.Clone(f.entries)}, nil
}
func (f *NotificationInbox) Set(entries ...api.InboxEntry) {
	f.mu.Lock()
	f.entries = entries
	publish := f.publish
	f.mu.Unlock()
	if publish != nil {
		publish()
	}
}
func (f *NotificationInbox) Observe(publish func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publish = publish
}
func (f *NotificationInbox) Break(broken bool) {
	f.mu.Lock()
	f.broken = broken
	f.mu.Unlock()
	f.Set(f.Current()...)
}
func (f *NotificationInbox) Current() []api.InboxEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.entries)
}
