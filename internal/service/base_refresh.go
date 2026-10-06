package service

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

const baseRefreshAction = "base-refresh"
const deliveryReviewAction = "delivery-review"
const publishUpstreamAction = "publish-upstream"
const baseObservationDocument = "base-observation"

// baseObservation pins an integration check without changing the declared
// dependency. Unavailable observations never authorize switching bases.
type baseObservation struct {
	Base        seal.Base `json:"base"`
	Unavailable bool      `json:"unavailable,omitempty"`
}

type baseRefreshInput struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Check      int                 `json:"check"`
}

type baseRefresher struct {
	s          *Service
	repository *trace.Repository
}

func baseObservationAt(repo *trace.Repository, stream config.WorkstreamID) (baseObservation, int, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return baseObservation{}, 0, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID == baseObservationDocument {
			var b baseObservation
			err := json.Unmarshal([]byte(d.Content), &b)
			return b, d.Revision, err
		}
	}
	return baseObservation{}, 0, nil
}

// integrationRecorded reports whether observed, read at revision, durably
// records that the workstream's dependency has integrated into upstream.
func integrationRecorded(observed baseObservation, revision int) bool {
	return revision > 0 && observed.Base.Commit != "" && observed.Base.Workstream == ""
}

func (b *baseRefresher) Pass(ctx context.Context) error {
	streams, err := b.repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		dependency, err := b.repository.WorkstreamBase(stream)
		if err != nil {
			return err
		}
		if dependency.Base == "" {
			continue
		}
		state, err := b.repository.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return err
		}
		if state.Value == AbandonedState {
			continue
		}
		_, _, sealed, err := seal.Latest(b.repository, stream)
		if err != nil {
			return err
		}
		if !sealed {
			// Ratification freezes the dependency even before sealing can run.
			// Observe integration here so a removed, merged parent branch does
			// not leave sealing parked behind the base-availability gate.
			_, ratified, err := latestRatification(b.repository, stream)
			if err != nil {
				return err
			}
			if !ratified {
				continue
			}
		}
		transitions, err := trace.Read[trace.Transition](b.repository, stream)
		if err != nil {
			return err
		}
		var latest trace.Transition
		for _, t := range transitions {
			if t.Subject == baseRefreshAction {
				latest = t
			}
		}
		if latest.To == "checking" || !latest.At.IsZero() && b.s.now().Sub(latest.At) < time.Minute {
			continue
		}
		subject, err := b.repository.Workflow(stream, baseRefreshAction)
		if err != nil {
			return err
		}
		in := baseRefreshInput{stream, int(subject.Version + 1)}
		input, err := json.Marshal(in)
		if err != nil {
			return err
		}
		id := fmt.Sprintf("base-refresh-%d", in.Check)
		event := trace.EventID(id, "run")
		op := coreadapter.Operation{ID: trace.OperationID(b.repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: baseRefreshAction, Input: input}
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: b.repository.Project(), Workstream: stream, At: b.s.now(), Actor: foremanActor, Cause: "base-integration"}
		_, err = b.repository.Transact(ctx, trace.Transaction{ExpectedVersion: subject.Version, Transition: trace.Transition{Header: h, Subject: baseRefreshAction, From: subject.Value, To: "checking", Reason: "Check the dependency's branch and upstream integration"}, Events: []trace.Event{{ID: event, Kind: baseRefreshAction, Body: "Check dependency integration", Operation: &op}}})
		if err != nil {
			return err
		}
	}
	return nil
}

func (b *baseRefresher) input(op coreadapter.Operation) (baseRefreshInput, error) {
	var in baseRefreshInput
	if err := json.Unmarshal(op.Input, &in); err != nil {
		return in, err
	}
	event := trace.EventID(fmt.Sprintf("base-refresh-%d", in.Check), "run")
	if op.Action != baseRefreshAction || op.Boundary != coreadapter.RepositoryBoundary || in.Check < 1 || config.CheckWorkstreamIDs(in.Workstream) != nil || op.ID != trace.OperationID(b.repository.Project(), in.Workstream, event) {
		return in, fmt.Errorf("invalid base refresh operation")
	}
	return in, nil
}

func (b *baseRefresher) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := b.input(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	transitions, err := trace.Read[trace.Transition](b.repository, in.Workstream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	for _, t := range transitions {
		if t.Cause == op.ID && t.Subject == baseRefreshAction && t.To == "checked" {
			return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: fmt.Sprintf("integration check %d is recorded as %s", in.Check, t.ID), Result: &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}}, nil
		}
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("integration check %d has not recorded its result", in.Check)}, nil
}

func (b *baseRefresher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	observed, err := b.Inspect(ctx, op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if observed.Result != nil {
		return *observed.Result, nil
	}
	in, err := b.input(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, done, err := b.checkOwnPullRequest(ctx, in, op); err != nil {
		return coreadapter.OperationResult{}, err
	} else if done {
		return result, nil
	}
	cfg := b.s.about(b.repository)
	g, err := featureWorkspaces(cfg, b.repository).of(in.Workstream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	selected, resolveErr := b.s.resolveBase(ctx, cfg, b.repository, in.Workstream, g)
	if err := ctx.Err(); err != nil {
		return coreadapter.OperationResult{}, err
	}
	next := baseObservation{Base: selected, Unavailable: resolveErr != nil}
	prior, revision, err := baseObservationAt(b.repository, in.Workstream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if resolveErr != nil {
		next.Base = prior.Base
	}
	var docs []trace.Document
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: baseObservationDocument, Revision: revision + 1, Project: b.repository.Project(), Workstream: in.Workstream, At: b.s.now(), Actor: foremanActor, Cause: op.ID}
	if prior != next || revision == 0 {
		data, err := json.Marshal(next)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		docs = []trace.Document{{Header: h, Path: "base-observation.json", Content: string(data)}}
	}
	state, err := b.repository.Workflow(in.Workstream, baseRefreshAction)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	h.Schema, h.ID, h.Revision = "osmia.trace.transition", fmt.Sprintf("base-refresh-%d-checked", in.Check), 1
	reason := "Dependency integration checked"
	if next.Unavailable {
		reason = "Dependency integration could not be checked; retain the recorded base and retry"
	}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: baseRefreshAction, From: state.Value, To: "checked", Reason: reason}}
	if len(docs) != 0 {
		tx.Events = []trace.Event{trace.Notice(h.ID, "base", reason)}
	}
	if len(docs) == 0 {
		_, err = b.repository.Transact(ctx, tx)
	} else {
		_, err = b.repository.RecordDocumentsWith(ctx, docs, tx)
	}
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}

// checkOwnPullRequest looks up a delivered workstream's own pull request
// once, before resolveBase's upstream fetch and the parent's PR lookup, and
// durably records a merged or closed outcome (spec#5). done is true only
// when it recorded the outcome; the caller then ends the run without
// calling resolveBase. An open PR, an outcome already recorded for the
// current PR, a recorded integration, a workstream with no PR of its own,
// or no PullRequests client all skip the lookup, so the run continues as it
// did before this check existed.
func (b *baseRefresher) checkOwnPullRequest(ctx context.Context, in baseRefreshInput, op coreadapter.Operation) (coreadapter.OperationResult, bool, error) {
	feature, err := b.repository.Workflow(in.Workstream, trace.FeatureSubject)
	if err != nil || feature.Value != DeliveredState {
		return coreadapter.OperationResult{}, false, err
	}
	publication, hasPR, err := currentOwnPublication(b.repository, in.Workstream)
	if err != nil || !hasPR {
		return coreadapter.OperationResult{}, false, err
	}
	if finished, err := ownPullRequestFinished(b.repository, in.Workstream); err != nil || finished {
		return coreadapter.OperationResult{}, false, err
	}
	observed, revision, err := baseObservationAt(b.repository, in.Workstream)
	if err != nil {
		return coreadapter.OperationResult{}, false, err
	}
	if integrationRecorded(observed, revision) {
		return coreadapter.OperationResult{}, false, nil
	}
	client := b.s.options.PullRequests
	if client == nil {
		return coreadapter.OperationResult{}, false, nil
	}
	found, err := client.Find(ctx, publication.Upstream, publication.Fork, publication.Branch)
	if err != nil {
		return coreadapter.OperationResult{}, false, err
	}
	for _, pr := range found {
		if pr.Number != publication.PullRequest {
			continue
		}
		outcome, finished := pullRequestOutcomeOf(pr)
		if !finished {
			return coreadapter.OperationResult{}, false, nil
		}
		// A restart here, before the outcome is durably recorded, leaves
		// nothing recorded; the next attempt looks the pull request up again.
		if err := b.s.step("own-pr-outcome-found"); err != nil {
			return coreadapter.OperationResult{}, false, err
		}
		result, err := b.completeOwnPullRequestOutcome(ctx, in, op, publication.PullRequest, outcome)
		return result, true, err
	}
	return coreadapter.OperationResult{}, false, nil
}

// completeOwnPullRequestOutcome records, in one commit, the own-PR outcome
// document and the base-refresh's completing "checked" transition, so a
// restart between the lookup and this commit re-looks-up the pull request
// and still yields one recorded outcome (charter#2).
func (b *baseRefresher) completeOwnPullRequestOutcome(ctx context.Context, in baseRefreshInput, op coreadapter.Operation, pr int, outcome string) (coreadapter.OperationResult, error) {
	at := b.s.now()
	doc, ok, err := ownPullRequestOutcomeDocumentFor(b.repository, in.Workstream, at, op.ID, pr, outcome)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	state, err := b.repository.Workflow(in.Workstream, baseRefreshAction)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	reason := fmt.Sprintf("pull request #%d is %s; the workstream's own pull request outcome is recorded and this check makes no further remote call", pr, outcome)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: fmt.Sprintf("base-refresh-%d-checked", in.Check), Revision: 1, Project: b.repository.Project(), Workstream: in.Workstream, At: at, Actor: foremanActor, Cause: op.ID}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: baseRefreshAction, From: state.Value, To: "checked", Reason: reason}}
	if ok {
		tx.Events = []trace.Event{trace.Notice(h.ID, "base", reason)}
		_, err = b.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx)
	} else {
		_, err = b.repository.Transact(ctx, tx)
	}
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}

// upstreamDeliveryPending allows only delivery maintenance of a terminal
// feature with a recorded dependent request, after its dependency integrated.
func upstreamDeliveryPending(repo *trace.Repository, cfg *config.Config, stream config.WorkstreamID) (bool, error) {
	observed, revision, err := baseObservationAt(repo, stream)
	if err != nil || revision == 0 || observed.Unavailable || observed.Base.Workstream != "" {
		return false, err
	}
	publications, err := publications(repo, stream)
	if err != nil {
		return false, err
	}
	dependent := false
	for _, p := range publications {
		if p.Status != publicationOpened {
			continue
		}
		if canonicalPublication(p, cfg) {
			return false, nil
		}
		if p.Upstream == cfg.Project.PushRepository() {
			dependent = true
		}
	}
	return dependent, nil
}

// canonicalPublication identifies delivery against the project's base branch.
func canonicalPublication(p DeliveryPublication, cfg *config.Config) bool {
	return p.BaseWorkstream == "" && p.Upstream == cfg.Project.Upstream && p.Base == cfg.Project.BaseBranch
}
