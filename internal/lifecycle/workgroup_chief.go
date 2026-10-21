package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

var ErrNoLiveWorkgroupContext = fmt.Errorf("%w: no live member remains to supply context", trace.ErrMemberRetired)

// ChiefRepository returns the original shared authority, its chief host and
// the live member supplying context. Ordinary workstreams keep their local
// Repository and host. Each successful shared open owns the returned close;
// closing it never closes a member's repository. Lifecycle bindings validate
// retired identities without opening their history.
func (s *Controller) ChiefRepository(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (target *trace.Repository, host, bundleStream config.WorkstreamID, close func(), err error) {
	noop := func() {
	}
	id, err := repository.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return nil, "", "", noop, err
	}
	if id == "" {
		pending, err := repository.PendingCrossRepoWorkgroup(stream)
		if err != nil {
			return nil, "", "", noop, err
		}
		if pending != "" {
			return nil, "", "", noop, fmt.Errorf("%w: member %s transfer to %s", errWorkgroupJoining, stream, pending)
		}
		host, err = repository.ChiefOfStaffHost(stream)
		return repository, host, host, noop, err
	}
	var g trace.Workgroup
	var wt *trace.Repository
	// A member can join between reading the membership and binding it;
	// the membership is then read again.
	for attempt := 0; ; attempt++ {
		_, g, _, err = repository.CrossRepoProjectSpec(id)
		if err != nil {
			return nil, "", "", noop, err
		}
		if len(g.Members) == 0 {
			return nil, "", "", noop, fmt.Errorf("workstream %s points to cross-repo workgroup %s, which has no recorded trace", stream, id)
		}
		members, err := s.CrossRepoMemberRepos(ctx, g)
		if err != nil {
			return nil, "", "", noop, err
		}
		wt, err = trace.OpenWorkgroupTrace(s.Current().Root, id, members)
		if wt != nil && err != nil {
			wt.Close()
		}
		if errors.Is(err, trace.ErrMembershipChanged) {
			if attempt < 2 {
				continue
			}
			return nil, "", "", noop, fmt.Errorf("%w: %w", errWorkgroupJoining, err)
		}
		if err != nil {
			return nil, "", "", noop, err
		}
		break
	}
	wt.Observe(s.ObserveSharedConversation)
	host, err = wt.ChiefOfStaffHost(stream)
	if err != nil {
		wt.Close()
		return nil, "", "", noop, err
	}
	if g.Conversation != nil && stream == g.Conversation.Workstream {
		stream, err = s.FirstLiveWorkgroupMember(g)
		if err != nil {
			wt.Close()
			return nil, "", "", noop, err
		}
		if stream == "" {
			wt.Close()
			return nil, "", "", noop, fmt.Errorf("%w: workgroup %s member %s", ErrNoLiveWorkgroupContext, id, g.Members[1])
		}
	}
	return wt, host, stream, func() {
		wt.Close()
	}, nil
}
