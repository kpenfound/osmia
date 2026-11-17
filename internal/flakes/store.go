package flakes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// Store keeps a project's flake record in the project's trace: one revisioned
// document per flake group, holding its occurrences, their decisions, its
// generation and its open workstream. Every change is one new revision of one
// group's document, so it is either recorded whole or not at all, and a change
// that races another writer is retried against the revision it lost to.
//
// A Store never writes inside a workstream's transaction. Its methods return
// the record as committed, so a caller that is interrupted repeats the call
// and finds the earlier outcome.
type Store struct {
	// Repository is the project's trace.
	Repository *trace.Repository
	// Actor attributes the revisions; the zero value is the flake recorder.
	Actor trace.Actor
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu sync.Mutex
}

// maxAttempts bounds how often a change retries after losing a revision race.
const maxAttempts = 8

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) actor() trace.Actor {
	if s.Actor == (trace.Actor{}) {
		return trace.Actor{Kind: "service", ID: "flakes"}
	}
	return s.Actor
}

// Group returns the group's record, and whether one is recorded.
func (s *Store) Group(group string) (Group, bool, error) {
	d, found, err := s.Repository.LatestProjectDocument(trace.FlakeDocumentID(GroupID(group)), trace.FlakePath(GroupID(group)))
	if err != nil || !found {
		return Group{}, false, err
	}
	g, err := s.decode(d)
	return g, err == nil, err
}

func (s *Store) decode(d trace.Document) (Group, error) {
	var g Group
	if err := json.Unmarshal([]byte(d.Content), &g); err != nil {
		return Group{}, fmt.Errorf("%s: %w", d.Path, err)
	}
	if g.Project != s.Repository.Project() || d.Path != trace.FlakePath(GroupID(g.Group)) {
		return Group{}, fmt.Errorf("%s: the flake record does not belong to its path", d.Path)
	}
	return g, nil
}

// Groups returns every group's record, ordered by group. It reads the whole
// project document history, so it serves recovery and lookups by workstream,
// not each recording.
func (s *Store) Groups() ([]Group, error) {
	docs, err := s.Repository.ProjectDocuments()
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	for _, d := range docs {
		if strings.HasPrefix(d.Path, "flakes/") && d.Revision > latest[d.Path].Revision {
			latest[d.Path] = d
		}
	}
	var out []Group
	for _, d := range latest {
		g, err := s.decode(d)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	slices.SortFunc(out, func(a, b Group) int { return strings.Compare(a.Group, b.Group) })
	return out, nil
}

// Occurrences returns every occurrence of every group, in group order and then
// the order each group recorded them. A recorder resumes from it after a
// restart: the occurrences whose notice, surfacing or decision is unfinished
// are the work left.
func (s *Store) Occurrences() ([]Occurrence, error) {
	groups, err := s.Groups()
	if err != nil {
		return nil, err
	}
	var out []Occurrence
	for _, g := range groups {
		out = append(out, g.Occurrences...)
	}
	return out, nil
}

// update applies change to the group's record, creating the record for seed
// when the group has none, and commits the result as the next revision. A
// change reporting no change commits nothing.
func (s *Store) update(ctx context.Context, group, cause string, seed *Group, change func(*Group) (bool, error)) (Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := GroupID(group)
	var lastErr error
	for range maxAttempts {
		d, found, err := s.Repository.LatestProjectDocument(trace.FlakeDocumentID(id), trace.FlakePath(id))
		if err != nil {
			return Group{}, err
		}
		var g Group
		revision := 1
		switch {
		case found:
			if g, err = s.decode(d); err != nil {
				return Group{}, err
			}
			revision = d.Revision + 1
		case seed != nil:
			g = *seed
		default:
			return Group{}, fmt.Errorf("%w: no record for flake group %q", ErrUnknownOccurrence, group)
		}
		changed, err := change(&g)
		if err != nil {
			return Group{}, err
		}
		if !changed && found {
			return g, nil
		}
		data, err := json.MarshalIndent(g, "", "  ")
		if err != nil {
			return Group{}, err
		}
		err = s.Repository.Append(ctx, trace.Document{
			Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: trace.FlakeDocumentID(id), Revision: revision,
				Project: s.Repository.Project(), At: s.now(), Actor: s.actor(), Cause: cause},
			Path: trace.FlakePath(id), Content: string(data) + "\n",
		})
		if err == nil {
			return g, nil
		}
		if !errors.Is(err, trace.ErrConflict) {
			return Group{}, err
		}
		lastErr = err
	}
	return Group{}, fmt.Errorf("flake record of %q changed concurrently: %w", group, lastErr)
}

func (s *Store) occurrenceOf(g Group, run string) (Occurrence, error) {
	o, ok := g.Find(run)
	if !ok {
		return Occurrence{}, ErrUnknownOccurrence
	}
	return o, nil
}

// Record records the occurrence of in.Run in in.Group. An occurrence is keyed
// by its run record and its group, so recording it again changes nothing and
// returns the occurrence as first recorded; added reports whether this call
// recorded it. The occurrence joins the group's history after those recorded
// before it.
func (s *Store) Record(ctx context.Context, in OccurrenceInput) (o Occurrence, added bool, err error) {
	if strings.TrimSpace(in.Group) == "" {
		return Occurrence{}, false, fmt.Errorf("a flake occurrence needs its group")
	}
	seed := &Group{Project: s.Repository.Project(), Group: in.Group, Unresolved: in.Unresolved}
	g, err := s.update(ctx, in.Group, "flake-occurrence", seed, func(g *Group) (bool, error) {
		var err error
		o, added, err = g.record(in, s.now())
		return added, err
	})
	if err != nil {
		return Occurrence{}, false, err
	}
	if !added {
		o, err = s.occurrenceOf(g, in.Run)
	}
	return o, added, err
}

// Reserve decides the occurrence of run in group to open the group's
// workstream. A pending occurrence is decided Joined when the group has an
// open workstream, and otherwise Opening, with the group's current generation,
// the hand-in key for it and the workstream that key creates, all recorded
// before the hand-in is made. An occurrence that is already decided keeps its
// decision. The returned decision is the occurrence's decision after the call.
func (s *Store) Reserve(ctx context.Context, group, run string) (Decision, error) {
	return s.decide(ctx, group, run, "flake-reserve", func(g *Group) (Decision, bool, error) { return g.reserve(run, s.now()) })
}

// Opened records that the hand-in for run's reserved opening created
// workstream (empty means the one its key names). It decides the occurrence
// Opened, or Joined when another occurrence of the same reservation opened the
// workstream first, and makes the workstream the group's open one when the
// reservation is still the group's current generation.
func (s *Store) Opened(ctx context.Context, group, run string, workstream config.WorkstreamID) (Decision, error) {
	return s.decide(ctx, group, run, "flake-opened", func(g *Group) (Decision, bool, error) { return g.opened(run, workstream, s.now()) })
}

// Join decides the pending occurrence of run to have joined workstream.
func (s *Store) Join(ctx context.Context, group, run string, workstream config.WorkstreamID) (Decision, error) {
	return s.decide(ctx, group, run, "flake-joined", func(g *Group) (Decision, bool, error) { return g.join(run, workstream, s.now()) })
}

// Decline decides the pending occurrence of run to be declined for reason.
func (s *Store) Decline(ctx context.Context, group, run, reason string) (Decision, error) {
	return s.decide(ctx, group, run, "flake-declined", func(g *Group) (Decision, bool, error) { return g.decline(run, reason, s.now()) })
}

func (s *Store) decide(ctx context.Context, group, run, cause string, change func(*Group) (Decision, bool, error)) (Decision, error) {
	var d Decision
	_, err := s.update(ctx, group, cause, nil, func(g *Group) (bool, error) {
		var changed bool
		var err error
		d, changed, err = change(g)
		return changed, err
	})
	return d, err
}

// CloseWorkstream records that workstream was delivered or abandoned: every
// group it was the open workstream of, or had a key reserved for, ends the
// association and advances to its next generation. Occurrences keep their own
// reservations and decisions. It returns the groups it closed, and does
// nothing for a workstream that no group holds, however often it is repeated.
func (s *Store) CloseWorkstream(ctx context.Context, workstream config.WorkstreamID) ([]string, error) {
	groups, err := s.Groups()
	if err != nil {
		return nil, err
	}
	var closed []string
	for _, g := range groups {
		if !g.holds(workstream) {
			continue
		}
		var changed bool
		if _, err := s.update(ctx, g.Group, "flake-closed", nil, func(g *Group) (bool, error) {
			changed = g.close(workstream, s.now())
			return changed, nil
		}); err != nil {
			return closed, err
		}
		if changed {
			closed = append(closed, g.Group)
		}
	}
	return closed, nil
}

// holds reports whether workstream is the group's open workstream or the one
// of an opening reserved in its current generation.
func (g Group) holds(workstream config.WorkstreamID) bool {
	if g.Open != nil {
		return g.Open.Workstream == workstream
	}
	return slices.ContainsFunc(g.Occurrences, func(o Occurrence) bool {
		return o.Decision.State == Opening && o.Decision.Generation == g.Generation && o.Decision.Workstream == workstream
	})
}

// OpenWorkstream returns the group's open flake-fix workstream.
func (s *Store) OpenWorkstream(group string) (config.WorkstreamID, bool, error) {
	g, found, err := s.Group(group)
	if err != nil || !found || g.Open == nil {
		return "", false, err
	}
	return g.Open.Workstream, true, nil
}

// MarkNoticed marks the notice of run's occurrence in group as sent. It is
// idempotent. A run's notice covers all its occurrences: mark each of them.
func (s *Store) MarkNoticed(ctx context.Context, group, run string) error {
	_, err := s.update(ctx, group, "flake-noticed", nil, func(g *Group) (bool, error) { return g.markNoticed(run, s.now()) })
	return err
}

// MarkSurfaced marks run's occurrence in group as surfaced to the open
// workstream. It is idempotent per workstream.
func (s *Store) MarkSurfaced(ctx context.Context, group, run string, workstream config.WorkstreamID) error {
	_, err := s.update(ctx, group, "flake-surfaced", nil, func(g *Group) (bool, error) { return g.markSurfaced(run, workstream, s.now()) })
	return err
}
