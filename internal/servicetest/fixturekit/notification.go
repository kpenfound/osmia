package fixturekit

import (
	"context"
	"github.com/kpenfound/osmia/internal/api"
	"slices"
	"sync"
)

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
