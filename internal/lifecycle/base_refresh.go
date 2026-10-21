package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/trace"
)

const BaseRefreshAction = "base-refresh"
const DeliveryReviewAction = "delivery-review"
const PublishUpstreamAction = "publish-upstream"
const BaseObservationDocument = "base-observation"

// BaseObservation pins an integration check without changing the declared
// dependency. Unavailable observations never authorize switching bases.
type BaseObservation struct {
	Base        seal.Base `json:"base"`
	Unavailable bool      `json:"unavailable,omitempty"`
}
type BaseRefreshInput struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Check      int                 `json:"check"`
	OwnOnly    bool                `json:"own_only,omitempty"`
}
type BaseRefresher struct {
	S          *Controller
	Repository *trace.Repository
}

func BaseObservationAt(repo *trace.Repository, stream config.WorkstreamID) (BaseObservation, int, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return BaseObservation{}, 0, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID == BaseObservationDocument {
			var b BaseObservation
			err := json.Unmarshal([]byte(d.Content), &b)
			return b, d.Revision, err
		}
	}
	return BaseObservation{}, 0, nil
}

// IntegrationRecorded reports whether observed, read at revision, durably
// Records that the workstream's dependency has integrated into upstream.
func IntegrationRecorded(observed BaseObservation, revision int) bool {
	return revision > 0 && observed.Base.Commit != "" && observed.Base.Workstream == ""
}
func (b *BaseRefresher) Pass(ctx context.Context) error {
	if err := b.ObserveArchivedPullRequests(ctx); err != nil {
		return err
	}
	streams, err := b.Repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if _, archived := b.S.ArchivedWorkstream(string(stream)); archived {
			continue
		}
		if session, err := isSession(b.Repository, stream); err != nil {
			return err
		} else if session {
			continue
		}
		dependency, err := b.Repository.WorkstreamBase(stream)
		if err != nil {
			return err
		}
		state, err := b.Repository.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return err
		}
		if state.Value == AbandonedState {
			continue
		}
		// Finished, beyond abandoned, means delivered with the current pull
		// request durably recorded as merged or closed (spec#4). A delivered
		// workstream with no pull request of its own, or whose outcome isn't
		// recorded yet, is not finished and keeps its refreshes (spec#11).
		if state.Value == DeliveredState {
			finished, err := ownPullRequestFinished(b.Repository, stream)
			if err != nil {
				return err
			}
			if finished {
				continue
			}
		}
		observeOwn, err := b.ObserveDeliveredMember(stream, state.Value)
		if err != nil {
			return err
		}
		ownOnly := dependency.Base == ""
		observed, revision, err := BaseObservationAt(b.Repository, stream)
		if err != nil {
			return err
		}
		ownOnly = ownOnly || IntegrationRecorded(observed, revision)
		if ownOnly && !observeOwn {
			continue
		}
		if !ownOnly {
			_, _, sealed, err := seal.Latest(b.Repository, stream)
			if err != nil {
				return err
			}
			if !sealed {
				_, ratified, err := latestRatification(b.Repository, stream)
				if err != nil {
					return err
				}
				if !ratified && !observeOwn {
					continue
				}
				if !ratified {
					ownOnly = true
				}
			}
		}
		transitions, err := trace.Read[trace.Transition](b.Repository, stream)
		if err != nil {
			return err
		}
		var latest trace.Transition
		requested := false
		for _, t := range transitions {
			if t.Subject == BaseRefreshAction {
				latest, requested = t, false
			} else if t.Subject == DriftRequestSubject && t.Actor == ownerActor {
				requested = true
			}
		}
		if latest.To == "checking" || !requested && !latest.At.IsZero() && b.S.Now().Sub(latest.At) < b.S.About(b.Repository).Project.PullRequestPollInterval() {
			continue
		}
		subject, err := b.Repository.Workflow(stream, BaseRefreshAction)
		if err != nil {
			return err
		}
		in := BaseRefreshInput{Workstream: stream, Check: int(subject.Version + 1), OwnOnly: ownOnly}
		input, err := json.Marshal(in)
		if err != nil {
			return err
		}
		id := fmt.Sprintf("base-refresh-%d", in.Check)
		event := trace.EventID(id, "run")
		op := coreadapter.Operation{ID: trace.OperationID(b.Repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: BaseRefreshAction, Input: input}
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: b.Repository.Project(), Workstream: stream, At: b.S.Now(), Actor: foremanActor, Cause: "base-integration"}
		_, err = b.Repository.Transact(ctx, trace.Transaction{ExpectedVersion: subject.Version, Transition: trace.Transition{Header: h, Subject: BaseRefreshAction, From: subject.Value, To: "checking", Reason: "Check the dependency's branch and upstream integration"}, Events: []trace.Event{{ID: event, Kind: BaseRefreshAction, Body: "Check dependency integration", Operation: &op}}})
		if err != nil {
			return err
		}
	}
	return nil
}
func (b *BaseRefresher) Input(op coreadapter.Operation) (BaseRefreshInput, error) {
	var in BaseRefreshInput
	if err := json.Unmarshal(op.Input, &in); err != nil {
		return in, err
	}
	event := trace.EventID(fmt.Sprintf("base-refresh-%d", in.Check), "run")
	if op.Action != BaseRefreshAction || op.Boundary != coreadapter.RepositoryBoundary || in.Check < 1 || config.CheckWorkstreamIDs(in.Workstream) != nil || op.ID != trace.OperationID(b.Repository.Project(), in.Workstream, event) {
		return in, fmt.Errorf("invalid base refresh operation")
	}
	return in, nil
}
func (b *BaseRefresher) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := b.Input(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	transitions, err := trace.Read[trace.Transition](b.Repository, in.Workstream)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	for _, t := range transitions {
		if t.Cause == op.ID && t.Subject == BaseRefreshAction && t.To == "checked" {
			return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: fmt.Sprintf("integration check %d is recorded as %s", in.Check, t.ID), Result: &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}}, nil
		}
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("integration check %d has not recorded its result", in.Check)}, nil
}
func (b *BaseRefresher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	observed, err := b.Inspect(ctx, op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if observed.Result != nil {
		return *observed.Result, nil
	}
	in, err := b.Input(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	ctx = ghlog.WithScope(ctx, ghlog.Scope{Project: string(b.Repository.Project()), Workstream: string(in.Workstream)})
	result, done, lookupErr := b.CheckOwnPullRequest(ctx, in, op)
	if lookupErr != nil {
		return coreadapter.OperationResult{}, lookupErr
	}
	if in.OwnOnly && !done {
		reason := "Own pull request remains open or unavailable; retry on the next cadence"
		if err := ctx.Err(); err != nil {
			return coreadapter.OperationResult{}, err
		}
		return b.CompleteOwnObservation(ctx, in, op, reason)
	}
	if done {
		return result, nil
	}
	cfg := b.S.About(b.Repository)
	g, err := featureWorkspaces(cfg, b.Repository).Of(in.Workstream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	selected, resolveErr := b.S.ResolveBase(ctx, cfg, b.Repository, in.Workstream, g)
	if err := ctx.Err(); err != nil {
		return coreadapter.OperationResult{}, err
	}
	next := BaseObservation{Base: selected, Unavailable: resolveErr != nil}
	prior, revision, err := BaseObservationAt(b.Repository, in.Workstream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if resolveErr != nil {
		next.Base = prior.Base
	}
	var docs []trace.Document
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: BaseObservationDocument, Revision: revision + 1, Project: b.Repository.Project(), Workstream: in.Workstream, At: b.S.Now(), Actor: foremanActor, Cause: op.ID}
	if prior != next || revision == 0 {
		data, err := json.Marshal(next)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		docs = []trace.Document{{Header: h, Path: "base-observation.json", Content: string(data)}}
	}
	state, err := b.Repository.Workflow(in.Workstream, BaseRefreshAction)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	h.Schema, h.ID, h.Revision = "osmia.trace.transition", fmt.Sprintf("base-refresh-%d-checked", in.Check), 1
	reason := "Dependency integration checked"
	if next.Unavailable {
		reason = "Dependency integration could not be checked; retain the recorded base and retry"
	}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: BaseRefreshAction, From: state.Value, To: "checked", Reason: reason}}
	if len(docs) != 0 {
		tx.Events = []trace.Event{trace.Notice(h.ID, "base", reason)}
	}
	if len(docs) == 0 {
		_, err = b.Repository.Transact(ctx, tx)
	} else {
		_, err = b.Repository.RecordDocumentsWith(ctx, docs, tx)
	}
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}

// CheckOwnPullRequest looks up a delivered workstream's own pull request
// once, before resolveBase's upstream fetch and the parent's PR lookup, and
// durably records a merged or closed outcome (spec#5). done is true only
// when it recorded the outcome; the caller then ends the run without
// calling resolveBase. An open PR, an outcome already recorded for the
// Current PR, a recorded integration outside own-only observation,
// a workstream with no PR of its own,
// or no PullRequests client all skip the lookup, so the run continues as it
// did before this check existed.
func (b *BaseRefresher) CheckOwnPullRequest(ctx context.Context, in BaseRefreshInput, op coreadapter.Operation) (coreadapter.OperationResult, bool, error) {
	feature, err := b.Repository.Workflow(in.Workstream, trace.FeatureSubject)
	if err != nil || feature.Value != DeliveredState {
		return coreadapter.OperationResult{}, false, err
	}
	publication, hasPR, err := currentOwnPublication(b.Repository, in.Workstream)
	if err != nil || !hasPR {
		return coreadapter.OperationResult{}, false, err
	}
	if finished, err := ownPullRequestFinished(b.Repository, in.Workstream); err != nil || finished {
		return coreadapter.OperationResult{}, false, err
	}
	observed, revision, err := BaseObservationAt(b.Repository, in.Workstream)
	if err != nil {
		return coreadapter.OperationResult{}, false, err
	}
	if !in.OwnOnly && IntegrationRecorded(observed, revision) {
		return coreadapter.OperationResult{}, false, nil
	}
	client := b.S.Options.PullRequests
	if client == nil {
		return coreadapter.OperationResult{}, false, nil
	}
	found, err := client.Find(ctx, publication.Upstream, publication.Fork, publication.Branch)
	if err != nil {
		if ctx.Err() != nil {
			return coreadapter.OperationResult{}, false, ctx.Err()
		}
		observe, memberErr := b.ObserveDeliveredMember(in.Workstream, feature.Value)
		if memberErr != nil {
			return coreadapter.OperationResult{}, false, memberErr
		}
		if observe {
			result, err := b.CompleteOwnObservation(ctx, in, op, "Own pull request lookup failed; retry on the next cadence: "+err.Error())
			return result, true, err
		}
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
		if err := b.S.Step("own-pr-outcome-found"); err != nil {
			return coreadapter.OperationResult{}, false, err
		}
		result, err := b.CompleteOwnPullRequestOutcome(ctx, in, op, publication.PullRequest, outcome, pr.MergeCommit)
		return result, true, err
	}
	return coreadapter.OperationResult{}, false, nil
}

// CompleteOwnPullRequestOutcome records, in one commit, the own-PR outcome
// Document and the base-refresh's completing "checked" transition, so a
// restart between the lookup and this commit re-looks-up the pull request
// and still yields one recorded outcome (charter#2).
func (b *BaseRefresher) CompleteOwnPullRequestOutcome(ctx context.Context, in BaseRefreshInput, op coreadapter.Operation, pr int, outcome string, mergeCommit ...string) (coreadapter.OperationResult, error) {
	at := b.S.Now()
	doc, ok, err := ownPullRequestOutcomeDocumentFor(b.Repository, in.Workstream, at, op.ID, pr, outcome, mergeCommit...)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	state, err := b.Repository.Workflow(in.Workstream, BaseRefreshAction)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	reason := fmt.Sprintf("pull request #%d is %s; the workstream's own pull request outcome is recorded and this check makes no further remote call", pr, outcome)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: fmt.Sprintf("base-refresh-%d-checked", in.Check), Revision: 1, Project: b.Repository.Project(), Workstream: in.Workstream, At: at, Actor: foremanActor, Cause: op.ID}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: BaseRefreshAction, From: state.Value, To: "checked", Reason: reason}}
	if ok {
		tx.Events = []trace.Event{trace.Notice(h.ID, "base", reason)}
		_, err = b.Repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx)
	} else {
		_, err = b.Repository.Transact(ctx, tx)
	}
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}

// DependentDeliveryPending allows delivery maintenance of a terminal
// feature with a recorded dependent request, after its dependency integrated.
func DependentDeliveryPending(repo *trace.Repository, cfg *config.Config, stream config.WorkstreamID) (bool, error) {
	observed, revision, err := BaseObservationAt(repo, stream)
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
		if CanonicalPublication(p, cfg) {
			return false, nil
		}
		if p.Upstream == cfg.Project.PushRepository() {
			dependent = true
		}
	}
	return dependent, nil
}

// CanonicalPublication identifies delivery against the project's base branch.
func CanonicalPublication(p DeliveryPublication, cfg *config.Config) bool {
	return p.BaseWorkstream == "" && p.Upstream == cfg.Project.Upstream && p.Base == cfg.Project.BaseBranch
}

// UpstreamDeliveryPending reports whether a delivered branch needs reviewed,
// owner-approved publication after drift, dependency integration, or a
// workgroup merge-order edge merging since the latest final report.
func UpstreamDeliveryPending(repo *trace.Repository, cfg *config.Config, stream config.WorkstreamID) (bool, error) {
	if finished, err := ownPullRequestFinished(repo, stream); err != nil || finished {
		return false, err
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return false, err
	}
	published, pending := false, false
	for _, doc := range docs {
		switch doc.ID {
		case publicationDocument:
			var p DeliveryPublication
			if err := json.Unmarshal([]byte(doc.Content), &p); err != nil {
				return false, err
			}
			if p.Status == publicationOpened {
				published, pending = true, false
			}
		case DriftDocument:
			var d DriftRebase
			if err := json.Unmarshal([]byte(doc.Content), &d); err != nil {
				return false, err
			}
			if published && d.Outcome == DriftRebased && (d.Before != d.Commit || d.From != d.Upstream.Commit) {
				pending = true
			}
		}
	}
	if pending {
		return true, nil
	}
	if pending, err := DependentDeliveryPending(repo, cfg, stream); err != nil || pending {
		return pending, err
	}
	return WorkgroupEdgePending(repo, stream)
}

// ObserveDeliveredMember extends own-PR observation to workgroup members whose
// dependency maintenance has nothing left to observe.
func (b *BaseRefresher) ObserveDeliveredMember(stream config.WorkstreamID, state string) (bool, error) {
	if state != DeliveredState {
		return false, nil
	}
	_, found, err := currentOwnPublication(b.Repository, stream)
	if err != nil || !found {
		return false, err
	}
	id, err := b.Repository.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return false, err
	}
	if id != "" {
		return true, nil
	}
	groups, _, err := trace.Workgroups(b.Repository)
	if err != nil {
		return false, err
	}
	_, found = trace.WorkgroupOf(groups, stream)
	return found, nil
}
func (b *BaseRefresher) CompleteOwnObservation(ctx context.Context, in BaseRefreshInput, op coreadapter.Operation, reason string) (coreadapter.OperationResult, error) {
	state, err := b.Repository.Workflow(in.Workstream, BaseRefreshAction)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: fmt.Sprintf("base-refresh-%d-checked", in.Check), Revision: 1, Project: b.Repository.Project(), Workstream: in.Workstream, At: b.S.Now(), Actor: foremanActor, Cause: op.ID}
	_, err = b.Repository.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: BaseRefreshAction, From: state.Value, To: "checked", Reason: reason}})
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, err
}
