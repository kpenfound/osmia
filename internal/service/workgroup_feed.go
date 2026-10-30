package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/readview"
	"github.com/kpenfound/osmia/internal/trace"
)

type workgroupFeedAuthority struct {
	chief        *trace.Repository
	host, bundle config.WorkstreamID
	members      []trace.CrossRepoMember
	close        func()
}

func workgroupAuthorityError(err error) *APIError {
	var wrapped apiErrAsError
	if errors.As(err, &wrapped) {
		return wrapped.Api
	}
	code := Internal
	if errors.Is(err, errWorkgroupJoining) || errors.Is(err, trace.ErrMembershipChanged) || errors.Is(err, trace.ErrLocked) || errors.Is(err, trace.ErrMemberRetired) {
		code = Conflict
	}
	return &APIError{Code: code, Message: err.Error()}
}

// openWorkgroupFeed resolves local groups through their chief and shared
// groups through the existing lifecycle binding. The latter validates joins
// and retired identities before granting access to the original authority.
func (s *Service) openWorkgroupFeed(ctx context.Context, raw string) (workgroupFeedAuthority, *APIError) {
	id, err := config.ParseWorkgroupID(raw)
	if err != nil {
		return workgroupFeedAuthority{}, &APIError{Code: Validation, Message: "workgroup must be a workgroup ID: g_ followed by 32 lowercase hexadecimal digits"}
	}
	cfg, projects := s.runtimes()
	for _, active := range projects {
		if !cfg.Active(active.id) {
			continue
		}
		groups, _, err := trace.Workgroups(active.repository)
		if err != nil {
			return workgroupFeedAuthority{}, workgroupAuthorityError(err)
		}
		for _, group := range groups {
			if group.ID != id {
				continue
			}
			members := make([]trace.CrossRepoMember, 0, len(group.Members))
			for _, member := range group.Members {
				members = append(members, trace.CrossRepoMember{Workstream: member, Project: active.repository})
			}
			live := false
			for _, member := range members {
				if _, archived := s.archivedWorkstream(string(member.Workstream)); !archived {
					live = true
					break
				}
			}
			if !live {
				return workgroupFeedAuthority{}, workgroupAuthorityError(lifecycle.ErrNoLiveWorkgroupContext)
			}
			chief, host, bundle, close, err := s.chiefRepository(ctx, active.repository, group.Members[0])
			if err != nil {
				return workgroupFeedAuthority{}, workgroupAuthorityError(err)
			}
			return workgroupFeedAuthority{chief, host, bundle, members, close}, nil
		}
	}
	// CrossRepoProjectSpec reads the original workgroup journal even when no
	// member has published its locator yet. Joining must remain a hold.
	for _, active := range projects {
		if !cfg.Active(active.id) {
			continue
		}
		_, group, _, err := active.repository.CrossRepoProjectSpec(id)
		if err != nil {
			return workgroupFeedAuthority{}, workgroupAuthorityError(err)
		}
		if group.ID == "" {
			directory, err := cfg.Root.WorkgroupTrace(id)
			if err != nil {
				return workgroupFeedAuthority{}, workgroupAuthorityError(err)
			}
			if _, err := os.Stat(directory); err == nil {
				return workgroupFeedAuthority{}, workgroupAuthorityError(errWorkgroupJoining)
			} else if !errors.Is(err, os.ErrNotExist) {
				return workgroupFeedAuthority{}, workgroupAuthorityError(err)
			}
			break
		}
		members, err := s.crossRepoMemberRepos(ctx, group)
		if err != nil {
			return workgroupFeedAuthority{}, workgroupAuthorityError(err)
		}
		slices.SortFunc(members, func(a, b trace.CrossRepoMember) int {
			return slices.Index(group.Members, a.Workstream) - slices.Index(group.Members, b.Workstream)
		})
		member, err := s.firstLiveWorkgroupMember(group)
		if err != nil {
			return workgroupFeedAuthority{}, workgroupAuthorityError(err)
		}
		if member == "" {
			return workgroupFeedAuthority{}, workgroupAuthorityError(lifecycle.ErrNoLiveWorkgroupContext)
		}
		for _, m := range members {
			if m.Workstream != member {
				continue
			}
			chief, host, bundle, close, err := s.chiefRepository(ctx, m.Project, member)
			if err != nil {
				return workgroupFeedAuthority{}, workgroupAuthorityError(err)
			}
			return workgroupFeedAuthority{chief, host, bundle, members, close}, nil
		}
		return workgroupFeedAuthority{}, workgroupAuthorityError(errWorkgroupJoining)
	}
	return workgroupFeedAuthority{}, &APIError{Code: Validation, Message: fmt.Sprintf("workgroup %s is not in an active project", id)}
}

func (s *Service) workgroupSend(ctx context.Context, raw string, req SendRequest) (ConversationEntry, *APIError) {
	return s.sendChief(ctx, "", req, func() (*trace.Repository, config.WorkstreamID, config.WorkstreamID, func(), error) {
		a, api := s.openWorkgroupFeed(ctx, raw)
		if api != nil {
			return nil, "", "", nil, apiErrAsError{Api: api}
		}
		return a.chief, a.host, a.bundle, a.close, nil
	})
}

func (s *Service) workgroupFeed(ctx context.Context, raw string) (WorkgroupFeedResponse, *APIError) {
	a, api := s.openWorkgroupFeed(ctx, raw)
	if api != nil {
		return WorkgroupFeedResponse{}, api
	}
	defer a.close()
	generations := []func() uint64{a.chief.Generation, s.storeGeneration}
	for _, m := range a.members {
		generations = append(generations, m.Project.Generation)
	}
	out, err := readview.Consistent(func() (WorkgroupFeedResponse, error) {
		entries, err := chiefFeedEntries(a.chief, a.host)
		if err != nil {
			return WorkgroupFeedResponse{}, err
		}
		repos := map[config.WorkstreamID]config.ProjectID{}
		for _, m := range a.members {
			repos[m.Workstream] = m.Project.Project()
		}
		// The shared thread keeps both whole-group and member-addressed turns.
		thread, err := a.chief.ChiefOfStaffThread(a.host)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return WorkgroupFeedResponse{}, err
		}
		for _, q := range thread.Turns {
			member := q.Request.Workstream
			// EnqueueTurn canonicalizes Workstream to the host. The leading owner
			// address note preserves the member without changing the trace format.
			line, _, _ := strings.Cut(q.Request.SystemPrompt, "\n")
			if _, tail, found := strings.Cut(line, " The owner addressed workstream "); found {
				address, _, _ := strings.Cut(tail, ",")
				if id, err := config.ParseWorkstreamID(address); err == nil {
					member = id
				}
			} else if member == a.host {
				continue
			}
			if _, ok := repos[member]; !ok {
				continue
			}
			for i := range entries {
				if entries[i].Turn == q.Request.TurnID && entries[i].Kind != "action" {
					entries[i].Workstream, entries[i].Repo = member, repos[member]
				}
			}
		}
		// Read member actions with their source identity, and deduplicate shared
		// escalations and changes that are visible through several member addresses.
		entries = slices.DeleteFunc(entries, func(e FeedEntry) bool { return e.Kind == "action" })
		type actionIdentity struct {
			Entry  ConversationEntry
			Member config.WorkstreamID
		}
		seen := map[actionIdentity]bool{}
		emit := func(e ConversationEntry, member config.WorkstreamID) {
			key := actionIdentity{e, member}
			if seen[key] {
				return
			}
			seen[key] = true
			if _, ok := repos[member]; !ok {
				member = ""
			}
			entries = append(entries, FeedEntry{Kind: e.Kind, At: e.At, Turn: e.Turn, Text: e.Text, State: e.State, Workstream: member, Repo: repos[member]})
		}
		if _, err := chiefActionsWithIdentity(a.chief, a.host, emit); err != nil {
			return WorkgroupFeedResponse{}, err
		}
		summaries := []WorkgroupMemberSummary{}
		for _, m := range a.members {
			summary, err := s.workgroupMemberSummary(m)
			if err != nil {
				return WorkgroupFeedResponse{}, err
			}
			summaries = append(summaries, summary)
			if summary.Archived {
				continue
			}
			if _, err := chiefActionsWithIdentity(a.chief, m.Workstream, emit); err != nil {
				return WorkgroupFeedResponse{}, err
			}
		}
		refs, err := trace.Read[trace.ImageReference](a.chief, a.host)
		if err != nil {
			return WorkgroupFeedResponse{}, err
		}
		for _, ref := range refs {
			var source *trace.Repository
			for _, m := range a.members {
				if m.Workstream == ref.Member {
					source = m.Project
					break
				}
			}
			if source == nil {
				return WorkgroupFeedResponse{}, fmt.Errorf("image reference names an unknown member")
			}
			entry := FeedEntry{Kind: "image", At: ref.At, Turn: ref.Turn, Workstream: ref.Member, Repo: source.Project(), Image: &wire.FeedImage{ID: ref.ImageID}}
			if _, archived := s.archivedWorkstream(string(ref.Member)); archived {
				entry.Image.Placeholder = fmt.Sprintf("image removed when %s was archived", ref.Member)
			} else {
				v, err := source.StoredImage(ref.Member, ref.ImageID)
				if err != nil {
					return WorkgroupFeedResponse{}, err
				}
				entry.Image.URL, entry.Image.ContentType = imageURL(ref.Member, ref.ImageID), v.ContentType
			}
			entries = append(entries, entry)
		}
		slices.SortStableFunc(entries, func(a, b FeedEntry) int { return a.At.Compare(b.At) })
		return WorkgroupFeedResponse{Workgroup: config.WorkgroupID(raw), Entries: entries, Members: summaries}, nil
	}, generations...)
	if err != nil {
		return WorkgroupFeedResponse{}, workgroupAuthorityError(err)
	}
	return out, nil
}

func (s *Service) workgroupMemberSummary(m trace.CrossRepoMember) (WorkgroupMemberSummary, error) {
	summary := WorkgroupMemberSummary{Workstream: m.Workstream, Repo: m.Project.Project(), Units: map[string]int{}}
	if archive, ok := s.archivedWorkstream(string(m.Workstream)); ok {
		if archive.Project != summary.Repo {
			return summary, trace.ErrConflict
		}
		w := WorkstreamStatus{Workstream: m.Workstream, Project: archive.Project, State: &archive.State}
		if err := presentArchivedWorkstream(m.Project, &w, archive); err != nil {
			return summary, err
		}
		summary.State, summary.DisplayState, summary.Wait, summary.PRLink, summary.Archived = w.State, w.DisplayState, w.Wait, archive.DeliveryLink, true
		return summary, nil
	}
	// Use the same projection as the status endpoint, including edge waits,
	// owner gates, unit states and the recorded publication evidence.
	rows, unreadable, api := s.projectStatuses(&activeProject{id: m.Project.Project(), repository: m.Project})
	if api != nil {
		return summary, apiErrAsError{Api: api}
	}
	if d, ok := unreadable[m.Workstream]; ok {
		return summary, fmt.Errorf("%s", d.Message)
	}
	for _, row := range rows {
		w := row.status
		if w.Workstream != m.Workstream {
			continue
		}
		summary.State, summary.DisplayState, summary.Wait = w.State, w.DisplayState, w.Wait
		for _, u := range w.Units {
			summary.Units[u.State]++
		}
		for _, a := range w.Agents {
			if a.Role != trace.ChiefOfStaff && (a.State == "running" || a.State == "captured") {
				summary.Running = true
			}
		}
		if summary.Wait == "" {
			for _, gate := range w.Gates {
				if gate.Reason != "" {
					summary.Wait = gate.Reason
					break
				}
			}
		}
		evidence, err := readMemberEvidence(m.Project, m.Workstream)
		if err != nil {
			return summary, err
		}
		if evidence.Published {
			summary.PRLink = evidence.Publication.URL
		}
		return summary, nil
	}
	return summary, fmt.Errorf("%w: member %s has no live status", errWorkgroupJoining, m.Workstream)
}
