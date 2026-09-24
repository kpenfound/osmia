package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// OverlapKind is the kind of the outbox event that warns a workstream's chief
// of staff that another workstream of the project builds in the same code.
const OverlapKind = "overlap-advisory"

// overlapPrefix opens the workflow subject overlap-<other-workstream> that
// records, in each workstream of a pair, the seals of the latest warning about
// the other: seals-<this seal>-<other seal>.
const overlapPrefix = "overlap-"

func overlapSubject(other config.WorkstreamID) string { return overlapPrefix + string(other) }

func overlapValue(mine, theirs int) string { return fmt.Sprintf("seals-%d-%d", mine, theirs) }

// OverlapAdvisory is a warning that another workstream of the project builds
// in the same code: Workstream is that workstream, Seal and OtherSeal the
// seals of this workstream and of it that were compared, Subsystems the
// top-level entities the shared code belongs to, Entities the most specific
// entities both sealed footprints cover and Paths the sealed path patterns of
// either that overlap the other's. Message says the same in plain language.
type OverlapAdvisory struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Seal       int                 `json:"seal"`
	OtherSeal  int                 `json:"other_seal"`
	Subsystems []string            `json:"subsystems"`
	Entities   []string            `json:"entities"`
	Paths      []string            `json:"paths"`
	Message    string              `json:"message"`
}

// footprintOverlap is what two sealed footprints share.
type footprintOverlap struct {
	Subsystems, Entities, Paths []string
}

func (o footprintOverlap) empty() bool { return len(o.Entities) == 0 && len(o.Paths) == 0 }

// sealedFootprint returns the entities and path patterns of every unit of the
// seal.
func sealedFootprint(s seal.Seal) (entities, paths map[string]bool) {
	entities, paths = map[string]bool{}, map[string]bool{}
	for _, f := range s.Footprints {
		for _, e := range f.Entities {
			entities[e] = true
		}
		for _, p := range f.Paths {
			paths[p] = true
		}
	}
	return entities, paths
}

// overlapOf compares the footprints of two seals. An entity is shared when
// both footprints name it or when its path patterns in the entity map overlap
// the other footprint's sealed patterns; an entity that another shared entity
// is part of is left out for the more specific one. The subsystems are the
// shared entities' top-level part_of ancestors in the map, or the entities
// themselves when the map has no parent for them.
func overlapOf(a, b seal.Seal, m kb.Map) footprintOverlap {
	ea, pa := sealedFootprint(a)
	eb, pb := sealedFootprint(b)
	byID := map[string]kb.Entity{}
	for _, e := range m.Entities {
		if _, ok := byID[e.ID]; !ok {
			byID[e.ID] = e
		}
	}
	covers := func(patterns []string, other map[string]bool) bool {
		for _, x := range patterns {
			for y := range other {
				if kb.PatternsOverlap(x, y) {
					return true
				}
			}
		}
		return false
	}
	paths := map[string]bool{}
	for x := range pa {
		for y := range pb {
			if kb.PatternsOverlap(x, y) {
				paths[x], paths[y] = true, true
			}
		}
	}
	shared := map[string]bool{}
	for id := range ea {
		if eb[id] || covers(byID[id].Paths, pb) {
			shared[id] = true
		}
	}
	for id := range eb {
		if ea[id] || covers(byID[id].Paths, pa) {
			shared[id] = true
		}
	}
	ancestors := func(id string) map[string]bool {
		seen := map[string]bool{}
		var up func(string)
		up = func(id string) {
			for _, parent := range byID[id].PartOf {
				if !seen[parent] {
					seen[parent] = true
					up(parent)
				}
			}
		}
		up(id)
		return seen
	}
	general := map[string]bool{}
	for id := range shared {
		for parent := range ancestors(id) {
			general[parent] = true
		}
	}
	subsystems := map[string]bool{}
	var entities []string
	for id := range shared {
		if general[id] {
			continue
		}
		entities = append(entities, id)
		top := false
		for parent := range ancestors(id) {
			if len(byID[parent].PartOf) == 0 {
				subsystems[parent], top = true, true
			}
		}
		if !top {
			subsystems[id] = true
		}
	}
	slices.Sort(entities)
	return footprintOverlap{Subsystems: sortedKeys(subsystems), Entities: entities, Paths: sortedKeys(paths)}
}

func sortedKeys(set map[string]bool) []string {
	out := []string{}
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// overlapMessage is the advisory for workstream mine about the other, in
// plain language.
func overlapMessage(other config.WorkstreamID, mine, theirs int, o footprintOverlap) string {
	where := "the same code: both sealed footprints cover the paths " + strings.Join(o.Paths, ", ")
	if len(o.Entities) > 0 {
		where = fmt.Sprintf("subsystem %s: both sealed footprints cover %s", strings.Join(o.Subsystems, ", "), strings.Join(o.Entities, ", "))
	}
	return fmt.Sprintf("Workstream %s of this project builds in %s (seal %d of this workstream, seal %d of that one). Its changes may conflict with this workstream's before both pull requests are open. This is an advisory and blocks neither workstream; the owner can pause or reprioritise either.", other, where, mine, theirs)
}

// sealedStream is a building or assembled workstream and its latest seal.
type sealedStream struct {
	stream   config.WorkstreamID
	seal     seal.Seal
	document trace.Document
}

// sealedBuild returns the workstream's latest seal while it is building or
// assembled.
func sealedBuild(repository *trace.Repository, stream config.WorkstreamID) (sealedStream, bool, error) {
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil || (feature.Value != BuildingState && feature.Value != AssembledState) {
		return sealedStream{}, false, err
	}
	latest, doc, found, err := seal.Latest(repository, stream)
	if err != nil || !found {
		return sealedStream{}, false, err
	}
	return sealedStream{stream: stream, seal: latest, document: doc}, true, nil
}

// overlaps is the overlap controller. Its pass compares the sealed
// footprints of every pair of the project's building or assembled
// workstreams that no runtime pause covers, and warns the chief of staff of
// each workstream of a pair that overlaps.
type overlaps struct {
	s          *Service
	repository *trace.Repository
}

// Pass warns about each overlapping pair once for each pair of seals: in each
// workstream, the transition overlap-<other>-<seal>-<other seal> moves the
// subject overlap-<other> to seals-<seal>-<other seal> with an OverlapKind
// event for its chief of staff. A pair whose subject already records its
// seals is not warned again.
func (o *overlaps) Pass(ctx context.Context) error {
	streams, err := o.repository.Workstreams()
	if err != nil {
		return err
	}
	state, _ := o.s.store.Effective()
	librarian := librarianWorkstream(o.repository.Project())
	var active []sealedStream
	for _, stream := range streams {
		if stream == librarian || scheduler.Paused(state.Pauses, o.repository.Project(), stream) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		b, found, err := sealedBuild(o.repository, stream)
		if err != nil {
			return fmt.Errorf("workstream %s overlap: %w", stream, err)
		}
		if found {
			active = append(active, b)
		}
	}
	if len(active) < 2 {
		return nil
	}
	m, err := kb.Load(o.repository)
	if err != nil {
		return err
	}
	for i, a := range active {
		for _, b := range active[i+1:] {
			overlap := overlapOf(a.seal, b.seal, m)
			if overlap.empty() {
				continue
			}
			if err := o.warn(ctx, a, b, overlap); err != nil {
				return fmt.Errorf("workstream %s overlap: %w", a.stream, err)
			}
			if err := o.warn(ctx, b, a, overlap); err != nil {
				return fmt.Errorf("workstream %s overlap: %w", b.stream, err)
			}
		}
	}
	return nil
}

// warn records the advisory about other in mine unless its subject already
// records these seals. A write another writer got to first is left to the
// next pass.
func (o *overlaps) warn(ctx context.Context, mine, other sealedStream, overlap footprintOverlap) error {
	subject := overlapSubject(other.stream)
	value := overlapValue(mine.seal.Seal, other.seal.Seal)
	state, err := o.repository.Workflow(mine.stream, subject)
	if err != nil || state.Value == value {
		return err
	}
	id := fmt.Sprintf("%s-%d-%d", subject, mine.seal.Seal, other.seal.Seal)
	message := overlapMessage(other.stream, mine.seal.Seal, other.seal.Seal, overlap)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: o.repository.Project(), Workstream: mine.stream, At: o.s.now(), Actor: foremanActor, Cause: fmt.Sprintf("%s-%d", seal.DocumentID, mine.document.Revision)}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: value, Reason: message},
		Events: []trace.Event{{ID: trace.EventID(id, "advisory"), Kind: OverlapKind, Body: message}}}
	if _, err := o.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// overlapAdvisories returns the advisories of the workstream that are still
// active: the workstream and the other are building or assembled on the seals
// the latest warning compared. Subjects are the workstream's workflow states.
func overlapAdvisories(repository *trace.Repository, stream config.WorkstreamID, subjects map[string]trace.WorkflowState) ([]OverlapAdvisory, error) {
	out := []OverlapAdvisory{}
	var mine sealedStream
	var m *kb.Map
	var names []string
	for s := range subjects {
		if strings.HasPrefix(s, overlapPrefix) {
			names = append(names, s)
		}
	}
	slices.Sort(names)
	for _, subject := range names {
		other, err := config.ParseWorkstreamID(strings.TrimPrefix(subject, overlapPrefix))
		if err != nil {
			continue
		}
		var seals [2]int
		if _, err := fmt.Sscanf(subjects[subject].Value, "seals-%d-%d", &seals[0], &seals[1]); err != nil {
			return nil, fmt.Errorf("subject %s is %q", subject, subjects[subject].Value)
		}
		if m == nil {
			var found bool
			if mine, found, err = sealedBuild(repository, stream); err != nil || !found {
				return out, err
			}
			loaded, err := kb.Load(repository)
			if err != nil {
				return nil, err
			}
			m = &loaded
		}
		theirs, found, err := sealedBuild(repository, other)
		if err != nil {
			return nil, err
		}
		if !found || mine.seal.Seal != seals[0] || theirs.seal.Seal != seals[1] {
			continue
		}
		overlap := overlapOf(mine.seal, theirs.seal, *m)
		if overlap.empty() {
			continue
		}
		out = append(out, OverlapAdvisory{Workstream: other, Seal: seals[0], OtherSeal: seals[1], Subsystems: overlap.Subsystems, Entities: overlap.Entities, Paths: overlap.Paths,
			Message: overlapMessage(other, seals[0], seals[1], overlap)})
	}
	return out, nil
}
