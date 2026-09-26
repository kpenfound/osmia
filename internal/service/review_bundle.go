package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/followup"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

// UnitReviewIdentity is the durable identity of a prepared review. A returned
// review must match Candidate and DiffSHA256 before its verdict can be used.
type UnitReviewIdentity struct {
	Subject    string                `json:"subject"`
	Candidate  coreadapter.Candidate `json:"candidate"`
	DiffSHA256 string                `json:"diff_sha256"`
	Report     string                `json:"report"`
	Seal       int                   `json:"seal"`
}

func (i UnitReviewIdentity) Matches(result coreadapter.ReviewResult) bool {
	return result.Subject == i.Subject && result.Candidate == i.Candidate && result.DiffSHA256 == i.DiffSHA256
}

func reviewDocument(unit string) string { return trace.UnitSubject(unit) + "-review" }

// prepareUnitReview reads only recorded evidence. It records the identity
// before a reviewer can be dispatched; failed preparation records a block.
func (m *masons) prepareUnitReview(ctx context.Context, stream config.WorkstreamID, unit string) (coreadapter.ReviewRequest, UnitReviewIdentity, error) {
	req, identity, err := m.unitReviewEvidence(ctx, stream, unit)
	if err != nil {
		if ctx.Err() == nil {
			cause := reviewingTransitionID(unit, 1)
			blockErr := m.recordReviewPreparationError(ctx, stream, unit, cause, fmt.Sprintf("unit %s stays reviewing: its reviewer bundle cannot be assembled: %v", unit, err))
			if blockErr != nil {
				return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, fmt.Errorf("%w; record review error: %v", err, blockErr)
			}
		}
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, err
	}
	docs, err := trace.Read[trace.Document](m.repository, stream)
	if err != nil {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, err
	}
	var last trace.Document
	for _, d := range docs {
		if d.ID == reviewDocument(unit) {
			last = d
		}
	}
	encoded, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, err
	}
	content := string(encoded) + "\n"
	if last.Content == content {
		return req, identity, nil
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: reviewDocument(unit), Revision: last.Revision + 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.now(), Actor: trace.Actor{Kind: "service", ID: "reviewer"}, Cause: reportDocument(unit)}
	doc := trace.Document{Header: h, Path: fmt.Sprintf("units/%s/review.json", unit), Content: content}
	if err := m.repository.RecordDocuments(ctx, []trace.Document{doc}); err != nil {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, fmt.Errorf("record review identity: %w", err)
	}
	return req, identity, nil
}

func reviewBlockedSubject(unit string) string {
	return "blocked-reviewer" + strings.TrimPrefix(trace.UnitSubject(unit), "unit")
}

func (m *masons) recordReviewPreparationError(ctx context.Context, stream config.WorkstreamID, unit, cause, reason string) error {
	subject := reviewBlockedSubject(unit)
	state, err := m.repository.Workflow(stream, subject)
	if err != nil {
		return err
	}
	if state.Value != "" {
		transitions, err := trace.Read[trace.Transition](m.repository, stream)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(transitions, func(t trace.Transition) bool {
			return t.Subject == subject && t.To == state.Value && t.Reason == reason
		}) {
			return nil
		}
	}
	k := state.Version + 1
	id := fmt.Sprintf("%s-%d", subject, k)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.now(), Actor: trace.Actor{Kind: "service", ID: "reviewer"}, Cause: cause}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: fmt.Sprintf("blocked-%d", k), Reason: reason}, Events: []trace.Event{trace.Notice(id, "chief", "The reviewer bundle is blocked: "+reason+". It can be prepared again after the evidence is fixed.")}}
	_, err = m.repository.Transact(ctx, tx)
	if errors.Is(err, trace.ErrConflict) {
		return nil
	}
	return err
}

func (m *masons) unitReviewEvidence(ctx context.Context, stream config.WorkstreamID, unit string) (coreadapter.ReviewRequest, UnitReviewIdentity, error) {
	state, err := m.repository.Workflow(stream, trace.UnitSubject(unit))
	if err != nil {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, err
	}
	if state.Value != UnitReviewing {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, fmt.Errorf("unit %s is %s, not reviewing", unit, state.Value)
	}
	return m.candidateEvidence(ctx, stream, unit)
}

// candidateEvidence assembles the review request and identity of the unit's
// recorded report, whatever the unit's state. A candidate that holds a
// stored conflict has none.
func (m *masons) candidateEvidence(ctx context.Context, stream config.WorkstreamID, unit string) (coreadapter.ReviewRequest, UnitReviewIdentity, error) {
	docs, err := trace.Read[trace.Document](m.repository, stream)
	if err != nil {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, err
	}
	var reportDoc trace.Document
	for _, d := range docs {
		if d.ID == reportDocument(unit) {
			reportDoc = d
		}
	}
	if reportDoc.Revision == 0 {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, fmt.Errorf("unit %s has no recorded mason report", unit)
	}
	var report UnitReport
	if err := json.Unmarshal([]byte(reportDoc.Content), &report); err != nil {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, fmt.Errorf("%s: %w", reportDoc.Path, err)
	}
	if report.Unit != unit || report.Base == "" || report.Candidate == "" || report.Seal < 1 || report.Branch == "" || report.Turn == "" || report.Outcome == "" {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, fmt.Errorf("%s lacks the unit, candidate, base, seal, branch or mason evidence", reportDoc.Path)
	}
	s, _, found, err := seal.Latest(m.repository, stream)
	if err != nil {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, err
	}
	if !found {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, fmt.Errorf("workstream %s has no seal", stream)
	}
	applications, err := amendment.FromDocuments(docs)
	if err != nil {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, err
	}
	if s.Seal != report.Seal && !amendment.CarriesReport(applications, unit, report.Seal, s.Seal) {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, fmt.Errorf("unit %s report seal %d is not the current seal", unit, report.Seal)
	}
	footprint, err := reviewFootprint(m.repository, stream, s, unit)
	if err != nil {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, err
	}
	if len(footprint.Entities) == 0 || len(footprint.Paths) == 0 {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, fmt.Errorf("seal %d has no resolved footprint for unit %s", s.Seal, unit)
	}
	files := bundle.Files{Repository: func(config.ProjectID) (*trace.Repository, error) { return m.repository, nil }, Now: m.s.now}
	mason, err := files.Mason(ctx, m.repository.Project(), stream, unit)
	if err != nil {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, err
	}
	if mason.Spec.Revision != s.Revision.Spec || mason.Plan.Revision != s.Revision.Plan || mason.Context.Charter.Revision == 0 || len(mason.Context.Entities.Unresolved) != 0 {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, fmt.Errorf("unit %s lacks sealed revisions, charter or resolved local context", unit)
	}
	graph, err := plan.Parse([]byte(mason.Plan.Content))
	if err != nil {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, err
	}
	planned, ok := graph.Unit(unit)
	if !ok {
		planned, err = sealedUnit(m.repository, coreadapter.Scope{Workstream: string(stream), Unit: unit})
		if err != nil {
			return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, err
		}
	}
	if reason := checkReport(planned, MasonReport{Outcome: report.Outcome, Criteria: report.Criteria}); reason != "" {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, fmt.Errorf("%s: %s", reportDoc.Path, reason)
	}
	g, err := newUnitWorkspaces(m.cfg, m.repository).of(stream)
	if err != nil {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, err
	}
	if stored, err := g.StoredConflicts(ctx, report.Candidate); err != nil {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, err
	} else if len(stored) != 0 {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, fmt.Errorf("candidate %s holds unresolved conflicts in %s", report.Candidate, strings.Join(stored, ", "))
	}
	diff, err := g.Diff(ctx, report.Base, report.Candidate)
	if err != nil {
		return coreadapter.ReviewRequest{}, UnitReviewIdentity{}, fmt.Errorf("candidate diff: %w", err)
	}
	sum := sha256.Sum256([]byte(diff))
	identity := UnitReviewIdentity{Subject: string(stream) + "/" + unit, Candidate: coreadapter.Candidate{Revision: report.Candidate, BaseRevision: report.Base, SpecRevision: fmt.Sprint(s.Revision.Spec), PlanRevision: fmt.Sprint(s.Revision.Plan)}, DiffSHA256: hex.EncodeToString(sum[:]), Report: fmt.Sprintf("%s revision %d", reportDoc.Path, reportDoc.Revision), Seal: s.Seal}
	encodedFootprint, _ := json.Marshal(footprint)
	items := []coreadapter.ContextItem{
		{Source: mason.Spec.Source, Content: mason.Spec.Content},
		{Source: mason.Plan.Source, Content: mason.Plan.Content},
		{Source: reportDoc.Path, Content: reportDoc.Content},
		{Source: "seal.json footprint", Content: string(encodedFootprint)},
	}
	if notices := mason.RenderAmendments(); notices != "" {
		items = append(items, coreadapter.ContextItem{Source: "amendment notices", Content: notices})
	}
	items = append(items, coreadapter.ContextItem{Source: "local context", Content: mason.Context.Render()})
	return coreadapter.ReviewRequest{Subject: identity.Subject, Candidate: identity.Candidate, Diff: diff, Context: items}, identity, nil
}

// carried returns the reviewed identity with the current identity's seal,
// spec and plan revisions when the amendments applied since the review leave
// the review of the unit in force, and the reviewed identity unchanged
// otherwise.
func (m *masons) carried(stream config.WorkstreamID, unit string, reviewed, current UnitReviewIdentity) (UnitReviewIdentity, error) {
	pin := func(i UnitReviewIdentity) (amendment.Pin, bool) {
		spec, specErr := strconv.Atoi(i.Candidate.SpecRevision)
		graph, planErr := strconv.Atoi(i.Candidate.PlanRevision)
		return amendment.Pin{Seal: i.Seal, Spec: spec, Plan: graph}, specErr == nil && planErr == nil
	}
	from, ok := pin(reviewed)
	to, now := pin(current)
	if !ok || !now || from == to {
		return reviewed, nil
	}
	applications, err := amendment.Read(m.repository, stream)
	if err != nil {
		return reviewed, err
	}
	if amendment.CarriesReview(applications, unit, from, to) {
		reviewed.Seal, reviewed.Candidate.SpecRevision, reviewed.Candidate.PlanRevision = current.Seal, current.Candidate.SpecRevision, current.Candidate.PlanRevision
	}
	return reviewed, nil
}

func reviewFootprint(repo *trace.Repository, stream config.WorkstreamID, s seal.Seal, unit string) (seal.Footprint, error) {
	i := slices.IndexFunc(s.Footprints, func(f seal.Footprint) bool { return f.Unit == unit })
	if i >= 0 {
		return s.Footprints[i], nil
	}
	added, found, err := followup.Find(repo, stream, unit)
	if err != nil {
		return seal.Footprint{}, err
	}
	if !found {
		return seal.Footprint{}, fmt.Errorf("follow-up %s is not recorded", unit)
	}
	graph, err := sealedPlan(repo, stream, s.Revision.Plan)
	if err != nil {
		return seal.Footprint{}, err
	}
	p, err := plan.Parse([]byte(graph.Content))
	if err != nil {
		return seal.Footprint{}, err
	}
	resolved := seal.Footprint{Unit: unit}
	for _, source := range p.Units {
		if !slices.ContainsFunc(source.Addresses, func(a plan.Address) bool { return a.Criterion == added.Criterion }) {
			continue
		}
		i := slices.IndexFunc(s.Footprints, func(f seal.Footprint) bool { return f.Unit == source.ID })
		if i < 0 {
			return seal.Footprint{}, fmt.Errorf("sealed plan unit %s has no footprint", source.ID)
		}
		for _, name := range s.Footprints[i].Entities {
			if !slices.Contains(resolved.Entities, name) {
				resolved.Entities = append(resolved.Entities, name)
			}
		}
		for _, path := range s.Footprints[i].Paths {
			if !slices.Contains(resolved.Paths, path) {
				resolved.Paths = append(resolved.Paths, path)
			}
		}
	}
	if len(resolved.Entities) == 0 || len(resolved.Paths) == 0 {
		return seal.Footprint{}, fmt.Errorf("follow-up %s has no footprint for %s in seal %d; an owner-approved amendment is required", unit, added.Criterion, s.Seal)
	}
	return resolved, nil
}
