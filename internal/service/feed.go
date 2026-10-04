package service

import (
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// feedList reads a workstream's feed: its conversation with the chief of
// staff, every status the chief of staff wrote, every agent turn that started
// and every change of the workstream's and its units' states, oldest first.
func (s *Service) feedList(raw string) (FeedResponse, *APIError) {
	_, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return FeedResponse{}, api
	}
	entries, err := consistentRead(repository, func() ([]FeedEntry, error) { return feedEntries(repository, stream) })
	if err != nil {
		return FeedResponse{}, &APIError{Internal, fmt.Sprintf("cannot read the feed of workstream %s; check the trace repository", stream)}
	}
	return FeedResponse{Workstream: stream, Entries: entries}, nil
}

// feedEntries merges the workstream's records into one feed ordered by time.
// Entries recorded at the same time keep the conversation first, then
// statuses, transitions and sessions.
func feedEntries(repository *trace.Repository, stream config.WorkstreamID) ([]FeedEntry, error) {
	conversation, err := conversationEntries(repository, stream)
	if err != nil {
		return nil, err
	}
	out := []FeedEntry{}
	for _, e := range conversation {
		out = append(out, FeedEntry{Kind: e.Kind, At: e.At, Turn: e.Turn, Text: e.Text, State: e.State})
	}
	statuses, err := trace.Read[trace.Status](repository, stream)
	if err != nil {
		return nil, err
	}
	for _, st := range statuses {
		out = append(out, FeedEntry{Kind: "status", At: st.At, Status: &StatusView{Goal: st.Goal, Attention: st.Attention, Note: st.Note, Agents: st.Agents, Revision: st.Revision, UpdatedAt: st.At}})
	}
	transitions, err := trace.Read[trace.Transition](repository, stream)
	if err != nil {
		return nil, err
	}
	for _, t := range transitions {
		var unit string
		switch {
		case t.Subject == trace.FeatureSubject:
		case t.Unit != "" && t.Subject == trace.UnitSubject(t.Unit):
			unit = t.Unit
		default:
			continue
		}
		out = append(out, FeedEntry{Kind: "transition", At: t.At, Transition: &FeedTransition{Unit: unit, From: t.From, To: t.To, Actor: t.Actor.Kind + ":" + t.Actor.ID, Reason: t.Reason}})
	}
	threads, err := repository.Threads(stream)
	if err != nil {
		return nil, err
	}
	for _, th := range threads {
		for _, q := range th.Turns {
			// A chief-of-staff turn answering an owner message is its
			// message and response.
			if q.Claim == nil || th.Identity.ID == trace.ChiefOfStaff && q.Request.Actor == ownerActor {
				continue
			}
			out = append(out, FeedEntry{Kind: "session", At: q.Claim.At, Session: feedSession(th, q)})
		}
	}
	slices.SortStableFunc(out, func(a, b FeedEntry) int { return a.At.Compare(b.At) })
	return out, nil
}

// feedSession describes one claimed turn of a thread.
func feedSession(th trace.Thread, q trace.QueuedTurn) *FeedSession {
	out := &FeedSession{Agent: th.Identity.ID, Role: th.Identity.Role, Unit: string(q.Request.Unit), Turn: q.Request.TurnID, Profile: q.Request.Profile.Name, StartedAt: q.Claim.At}
	if n := len(q.Attempts); n > 0 {
		out.Profile = q.Attempts[n-1].Profile.Name
	}
	switch q.Status() {
	case "running", "captured":
		out.State = "running"
		// A claim an earlier service session left without a result never
		// completes.
		if th.Status == "interrupted" && th.Active == q.Request.TurnID {
			out.State = "interrupted"
		}
	case "idle":
		out.State = "done"
	default:
		out.State = q.Status()
	}
	if r := q.Response; r != nil {
		ended := r.At
		out.EndedAt = &ended
		out.Failure = r.Failure
		out.Summary = clip(r.Result.FinalResponse)
		if o := r.Result.Outcome; o != nil && o.Report != "" {
			out.Summary = clip(o.Report)
		}
	}
	return out
}
