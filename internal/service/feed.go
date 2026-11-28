package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/readview"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Service) feedList(raw string) (FeedResponse, *APIError) {
	_, stream, repository, api := s.conversationTrace(raw)
	if api != nil {
		return FeedResponse{}, api
	}
	chief, _, _, closeChief, err := s.chiefRepositoryForRead(context.Background(), repository, stream)
	if err != nil {
		return FeedResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot resolve the workgroup of workstream %s; check the trace repository", stream)}
	}
	defer closeChief()
	entries, err := readview.Consistent(func() ([]FeedEntry, error) {
		out, err := FeedEntries(repository, chief, stream)
		if err != nil {
			return nil, err
		}
		recoveries, err := s.feedRecoveries(repository, stream)
		if err != nil {
			return nil, err
		}
		out = append(out, recoveries...)
		slices.SortStableFunc(out, func(a, b FeedEntry) int { return a.At.Compare(b.At) })
		return out, nil
	}, repository.Generation, chief.Generation)
	if err != nil {
		return FeedResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the feed of workstream %s; check the trace repository", stream)}
	}
	return FeedResponse{Workstream: stream, Entries: entries}, nil
}

func FeedEntries(repository, chief *trace.Repository, stream config.WorkstreamID) ([]FeedEntry, error) {
	group, err := paneWorkgroup(repository, stream)
	if err != nil {
		return nil, err
	}
	shared := group != nil && len(group.Members) > 1
	out := []FeedEntry{}
	if !shared {
		out, err = chiefFeedEntries(chief, stream)
		if err != nil {
			return nil, err
		}
	}
	transitions, err := trace.Read[trace.Transition](repository, stream)
	if err != nil {
		return nil, err
	}
	for _, t := range transitions {
		if router.IsJournal(t.Subject) {
			continue
		}
		out = append(out, FeedEntry{Kind: "transition", At: t.At, Transition: &api.FeedTransition{Subject: t.Subject, Unit: t.Unit, From: t.From, To: t.To, Actor: t.Actor.Kind + ":" + t.Actor.ID, Reason: t.Reason, DecidedBy: t.DecidedBy, Judgment: t.Judgment, DefaultTo: t.DefaultTo, Departed: t.Departed()}})
	}
	threads, err := repository.Threads(stream)
	if err != nil {
		return nil, err
	}
	for _, th := range threads {
		if shared && (trace.ManagerLike(th.Identity.ID) || trace.ManagerLike(th.Identity.Role)) {
			continue
		}
		for _, q := range th.Turns {
			if q.Claim == nil {
				continue
			}
			out = append(out, FeedEntry{Kind: "session", At: q.Claim.At, Session: feedSession(th, q)})
		}
	}
	checks, err := feedChecks(repository, stream)
	if err != nil {
		return nil, err
	}
	out = append(out, checks...)
	images, err := memberImageEntries(repository, stream)
	if err != nil {
		return nil, err
	}
	out = append(out, images...)
	briefs, err := briefEntries(repository, stream)
	if err != nil {
		return nil, err
	}
	out = append(out, briefs...)
	slices.SortStableFunc(out, func(a, b FeedEntry) int { return a.At.Compare(b.At) })
	return out, nil
}

func briefEntries(repository *trace.Repository, stream config.WorkstreamID) ([]FeedEntry, error) {
	briefs, err := delivery.Briefs(repository, stream)
	if err != nil {
		return nil, err
	}
	out := make([]FeedEntry, 0, len(briefs))
	for _, b := range briefs {
		brief := &api.FeedBrief{Review: b.Review, ReviewRevision: b.ReviewRevision, Commit: b.Commit, Text: b.Text, BeforeYouShip: b.BeforeYouShip}
		for _, id := range b.Screenshots {
			image := api.FeedImage{ID: id}
			v, err := repository.StoredImage(stream, id)
			switch {
			case errors.Is(err, os.ErrNotExist):
				image.Placeholder = "image " + id + " is no longer stored"
			case err != nil:
				return nil, err
			default:
				image.URL, image.ContentType = imageURL(stream, id), v.ContentType
			}
			brief.Screenshots = append(brief.Screenshots, image)
		}
		out = append(out, FeedEntry{Kind: "brief", At: b.At, Brief: brief})
	}
	return out, nil
}

func chiefFeedEntries(chief *trace.Repository, stream config.WorkstreamID, earlier ...[][]ConversationEntry) ([]FeedEntry, error) {
	conversation, err := ConversationEntries(chief, stream, earlier...)
	if err != nil {
		return nil, err
	}
	out := []FeedEntry{}
	for _, e := range conversation {
		out = append(out, FeedEntry{Kind: e.Kind, At: e.At, Turn: e.Turn, Text: e.Text, State: e.State, QueuedReason: e.QueuedReason})
	}
	host, err := chief.ManagerHost(stream)
	if err != nil {
		return nil, err
	}
	statuses, err := trace.Read[trace.Status](chief, host)
	if err != nil {
		return nil, err
	}
	for _, st := range statuses {
		out = append(out, FeedEntry{Kind: "status", At: st.At, Status: &StatusView{Goal: st.Goal, Attention: st.Attention, Note: st.Note, Agents: st.Agents, Revision: st.Revision, UpdatedAt: st.At}})
	}
	return out, nil
}

func feedSession(th trace.Thread, q trace.QueuedTurn) *FeedSession {
	out := &FeedSession{Agent: th.Identity.ID, Role: th.Identity.Role, Unit: string(q.Request.Unit), Turn: q.Request.TurnID, Profile: q.Request.Profile.Name, StartedAt: q.Claim.At}
	if n := len(q.Attempts); n > 0 {
		out.Profile = q.Attempts[n-1].Profile.Name
	}
	switch q.Status() {
	case "running", "captured":
		out.State = "running"
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
