package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// ErrNoPullRequestCredential refuses a publication before any side effect
// while the service has no credential to open its pull request.
var ErrNoPullRequestCredential = errors.New("the service was started without GITHUB_TOKEN, so it cannot open the pull request; restart osmia serve with GITHUB_TOKEN set")

// PublicationAction reports whether op publishes a workstream.
func PublicationAction(op coreadapter.Operation) bool {
	return op.Boundary == coreadapter.RepositoryBoundary && (op.Action == PublishAction || op.Action == publishUpstreamAction)
}

// PublishAction is the repository-boundary operation action that applies the
// owner's approval to mark an assembled workstream's draft pull request ready
// for review: it puts the approved description on the pull request, marks it
// ready and records the workstream delivered.
const PublishAction = "publish"

const (
	// PublicationSubject is the workflow subject that tracks the
	// Publications of a workstream: requested-<k> once publishing the owner
	// approval recorded as revision k of final/delivery.json is asked for,
	// then published-<k> or refused-<k>.
	PublicationSubject = "publication"
	// PublicationPath is the workstream document of the publication, with
	// its trace record ID.
	PublicationPath     = "final/publication.json"
	PublicationDocument = "final-publication"
	// PublicationPushing and PublicationOpened are the statuses of a
	// publication record: the feature branch is being moved to the delivery
	// commit, then the pull request is open and the workstream delivered.
	PublicationPushing = "pushing"
	PublicationOpened  = "opened"
	// SquashStyle delivers the reviewed branch as one owner-signed commit.
	SquashStyle = "squash"
)

type RetargetInput struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	Base        string `json:"base"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description"`
}

// PublishInput names one publication: the approval revision it publishes,
// the reviewed commit and description hash that approval records, and the
// delivery style, resolved push repository, upstream and base branch when it was
// asked for, so a retry publishes the same way.
type PublishInput struct {
	BaseWorkstream config.WorkstreamID `json:"base_workstream,omitempty"`
	Retarget       *RetargetInput      `json:"retarget,omitempty"`
	Maintenance    bool                `json:"maintenance,omitempty"`
	// Draft is true while the workstream has an incoming workgroup
	// merge_order edge whose upstream has not yet merged: the pull request
	// keeps its approved description, which links the upstream pull request,
	// and stays a draft. Once every such edge merges, a fresh owner approval
	// has Draft false and marks the pull request ready for review.
	Draft           bool   `json:"draft,omitempty"`
	Approval        int    `json:"approval"`
	Commit          string `json:"commit"`
	DescriptionHash string `json:"description_hash"`
	Style           string `json:"style"`
	Fork            string `json:"fork"`
	Upstream        string `json:"upstream"`
	Base            string `json:"base"`
}

func PublishIDs(approval int) (transition, event string) {
	transition = fmt.Sprintf("%s-%d", PublicationSubject, approval)
	return transition, trace.EventID(transition, "run")
}

// Publisher is the publication controller. Its pass asks to apply the current
// owner approval of every assembled workstream that is not paused, marking
// its draft pull request ready for review; its reconciler runs each
// operation.
type Publisher struct {
	S          *Controller
	Repository *trace.Repository
}

var _ coreadapter.Reconciler = (*Publisher)(nil)

// Pass asks for the application of an assembled workstream's latest owner
// approval once it passes the delivery gate, unless an operation of the
// workstream has neither a result nor a recorded outcome yet, or one of that
// approval was already asked for.
func (p *Publisher) Pass(ctx context.Context) error {
	streams, err := nonSessionWorkstreams(p.Repository)
	if err != nil {
		return err
	}
	state, _ := p.S.Effective()
	for _, stream := range streams {
		if scheduler.Paused(state.Pauses, p.Repository.Project(), stream) {
			continue
		}
		if session, err := isSession(p.Repository, stream); err != nil {
			return fmt.Errorf("workstream %s publication: %w", stream, err)
		} else if session {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.Request(ctx, stream); err != nil {
			return fmt.Errorf("workstream %s publication: %w", stream, err)
		}
	}
	return nil
}

func (p *Publisher) Request(ctx context.Context, stream config.WorkstreamID) error {
	if held, err := p.S.ChangeHeld(p.Repository, stream); err != nil || held {
		return err
	}
	if landing, err := LandingInFlight(p.Repository, stream); err != nil || landing {
		return err
	}
	feature, err := p.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	if feature.Value != AssembledState {
		if feature.Value != DeliveredState {
			return nil
		}
		pending, err := p.S.UpstreamDeliveryPending(p.Repository, p.S.About(p.Repository), stream)
		if err != nil || !pending {
			return err
		}
	}
	_, decision, approval, err := DeliveryDocuments(p.Repository, stream)
	if err != nil || approval == nil {
		return err
	}
	ops, err := p.Repository.Operations(stream)
	if err != nil {
		return err
	}
	for _, o := range ops {
		if o.Operation.Action != PublishAction && o.Operation.Action != publishUpstreamAction {
			continue
		}
		in, err := DecodePublish(o.Operation)
		if err != nil {
			return err
		}
		if in.Approval == decision.Revision {
			return nil
		}
		if o.Result == nil {
			if result, err := p.Outcome(stream, in.Approval); err != nil || result == nil {
				return err
			}
		}
	}
	if _, reason, err := p.S.DeliveryGate(ctx, p.Repository, stream, approval.Description); err != nil || reason != "" {
		return err
	}
	cfg := p.S.About(p.Repository)
	if !cfg.HasProject() || cfg.Project.ID != p.Repository.Project() {
		return nil
	}
	in := PublishInput{Maintenance: feature.Value == DeliveredState, Approval: decision.Revision, Commit: approval.Commit, DescriptionHash: approval.DescriptionHash, Style: cfg.Project.Landing, Fork: cfg.Project.PushRepository(), Upstream: cfg.Project.Upstream, Base: cfg.Project.BaseBranch}
	report, _, err := LatestFinalReport(p.Repository, stream)
	if err != nil {
		return err
	}
	if report.Upstream != nil && report.Upstream.Workstream != "" {
		in.Upstream = cfg.Project.PushRepository()
		in.Base = report.Upstream.Branch
		in.BaseWorkstream = report.Upstream.Workstream
	}
	if in.Maintenance {
		if previous, found, err := CurrentOwnPublication(p.Repository, stream); err != nil {
			return err
		} else if found && previous.PullRequest > 0 {
			if found && previous.Upstream == in.Upstream && (previous.Base == in.Base || in.Fork == in.Upstream && !p.S.CanonicalPublication(previous, cfg)) {
				in.Retarget = &RetargetInput{Number: previous.PullRequest, URL: previous.URL, Base: previous.Base, Title: previous.Title, Description: previous.Description}
			}
		}
	}
	if _, waiting, err := p.S.UnresolvedMergeOrderEdge(p.Repository, stream); err != nil {
		return err
	} else {
		in.Draft = waiting
	}
	input, err := json.Marshal(in)
	if err != nil {
		return err
	}
	subject, err := p.Repository.Workflow(stream, PublicationSubject)
	if err != nil {
		return err
	}
	transition, event := PublishIDs(in.Approval)
	action := PublishAction
	if in.Maintenance {
		action = publishUpstreamAction
	}
	op := coreadapter.Operation{ID: trace.OperationID(p.Repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: action, Input: input}
	var reason string
	if in.Maintenance {
		requestAction := "opens a pull request"
		if in.Retarget != nil {
			requestAction = fmt.Sprintf("updates pull request #%d", in.Retarget.Number)
		}
		reason = fmt.Sprintf("%s approved description %s for %s at %s; publication pushes it (%s) to %s of %s and %s against %s of %s", approver(*approval), in.DescriptionHash, featureBranch(stream), in.Commit, in.Style, featureBranch(stream), in.Fork, requestAction, in.Base, in.Upstream)
	} else {
		reason = fmt.Sprintf("%s approved description %s for %s at %s; the pull request against %s of %s gets it and is marked ready for review", approver(*approval), in.DescriptionHash, featureBranch(stream), in.Commit, in.Base, in.Upstream)
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: transition, Revision: 1, Project: p.Repository.Project(), Workstream: stream, At: p.S.Now(), Actor: foremanActor, Cause: decision.Cause}
	tx := trace.Transaction{ExpectedVersion: subject.Version,
		Transition: trace.Transition{Header: h, Subject: PublicationSubject, From: subject.Value, To: fmt.Sprintf("requested-%d", in.Approval), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: action, Body: fmt.Sprintf("Apply owner approval %d", in.Approval), Operation: &op}}}
	if _, err := p.Repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

func approver(a DeliveryApproval) string {
	if a.DecidedBy == trace.DecidedByJev {
		return "Jev"
	}
	return "the owner"
}

func DecodePublish(op coreadapter.Operation) (PublishInput, error) {
	var in PublishInput
	if op.Boundary != coreadapter.RepositoryBoundary || (op.Action != PublishAction && op.Action != publishUpstreamAction) {
		return in, fmt.Errorf("unsupported repository operation %q", op.Action)
	}
	dec := json.NewDecoder(bytes.NewReader(op.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid publish operation input: %w", err)
	}
	if in.Maintenance != (op.Action == publishUpstreamAction) {
		return in, errors.New("publication action differs from delivery maintenance intent")
	}
	if in.Approval < 1 || in.Commit == "" || in.DescriptionHash == "" || in.Style == "" || in.Fork == "" || in.Upstream == "" || in.Base == "" {
		return in, errors.New("publish operation requires a positive approval revision, a commit, a description hash, a style, a push repository, an upstream and a base branch")
	}
	if in.Retarget != nil && (in.Retarget.Number < 1 || in.Retarget.URL == "" || in.Retarget.Base == "") {
		return in, errors.New("invalid pull request update operation")
	}
	return in, nil
}

// Stream returns the workstream a publication operation belongs to: the one
// whose run event derives the operation ID.
func (p *Publisher) Stream(op coreadapter.Operation, in PublishInput) (config.WorkstreamID, error) {
	streams, err := p.Repository.Workstreams()
	if err != nil {
		return "", err
	}
	_, event := PublishIDs(in.Approval)
	for _, stream := range streams {
		if trace.OperationID(p.Repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("publish operation %s belongs to no workstream", op.ID)
}

// Outcome returns the recorded result of the publication of approval k:
// succeeded once it recorded the workstream delivered, failed once its
// refusal is recorded, nil before either.
func (p *Publisher) Outcome(stream config.WorkstreamID, k int) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](p.Repository, stream)
	if err != nil {
		return nil, err
	}
	transition, _ := PublishIDs(k)
	for _, t := range transitions {
		switch t.ID {
		case transition + "-published":
			return &coreadapter.OperationResult{Outcome: "succeeded", Evidence: t.Reason}, nil
		case transition + "-refused":
			return &coreadapter.OperationResult{Outcome: "failed", Evidence: t.Reason}, nil
		}
	}
	return nil, nil
}

// PublicationRefused reports whether the publication of approval k was
// refused.
func PublicationRefused(repository *trace.Repository, stream config.WorkstreamID, k int) (bool, error) {
	result, err := (&Publisher{Repository: repository}).Outcome(stream, k)
	return result != nil && result.Outcome == "failed", err
}

// Inspect reads the recorded transitions: a recorded outcome completes the
// operation; otherwise it is absent, and Apply inspects the push repository and its pull
// requests before acting.
func (p *Publisher) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := DecodePublish(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := p.Stream(op, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := p.Outcome(stream, in.Approval)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "publication " + result.Outcome, Result: result}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("publication of owner approval %d is not recorded; it inspects the push repository and its pull requests before acting", in.Approval)}, nil
}

// Apply applies owner approval k. The workstream must be assembled and the
// approval must still pass the delivery gate for the description it records:
// the feature branch, final review and governing documents unchanged since.
//
// For an assembled workstream it marks the draft pull request ready for
// review (MarkReady). Nothing is pushed and no pull request is opened: the
// draft opened with the push of the reviewed commit. A maintenance
// operation of a delivered workstream instead amends the approved messages and
// identity and signs the outgoing commits, preserving the reviewed tree and
// base; squash produces one commit. The service then reads the feature
// branch: at the delivery commit it is already pushed; absent, or at a commit
// an earlier publication of the workstream recorded, it is pushed with that
// commit as the expected one; at any other commit the publication is refused.
// An existing pull request must match the approved base, description and
// current branch tip before a push, or be otherwise matching with its head
// still at a commit an earlier publication recorded. final/publication.json
// records the delivery commit before the push. After the push the pull
// request head is checked at the delivery commit, and a head still at such a
// recorded commit leaves the operation pending; if none exists, one is opened
// and checked. A maintenance retarget instead reconciles the recorded request
// and updates its base and approved text. One commit then records the opened
// publication and the operation's outcome. A refusal records the reason and
// tells the chief of staff. Git, GitHub and storage errors leave the
// operation pending for another attempt, which inspects the push repository
// and pull requests again.
func (p *Publisher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := DecodePublish(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := p.Stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	ctx = ghlog.WithScope(ctx, ghlog.Scope{Project: string(p.Repository.Project()), Workstream: string(stream)})
	if result, err := p.Outcome(stream, in.Approval); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	if held, err := p.S.ChangeRepositoryHeld(p.Repository, stream, op); err != nil || held {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return coreadapter.OperationResult{}, errChangeBoundary
	}
	cfg := p.S.About(p.Repository)
	if !cfg.HasProject() || cfg.Project.ID != p.Repository.Project() {
		return coreadapter.OperationResult{}, errors.New("the project is not active")
	}
	client := p.S.PullRequests
	if !pulls.Credentialed(client) {
		return coreadapter.OperationResult{}, ErrNoPullRequestCredential
	}
	approval, err := ApprovalAt(p.Repository, stream, in.Approval)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if approval.Commit != in.Commit || approval.DescriptionHash != in.DescriptionHash {
		return coreadapter.OperationResult{}, fmt.Errorf("%s revision %d does not record the approval of %s with description %s", DeliveryPath, in.Approval, in.Commit, in.DescriptionHash)
	}
	feature, err := p.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if feature.Value != AssembledState && !(feature.Value == DeliveredState && in.Maintenance) {
		return p.Refuse(ctx, stream, in, fmt.Sprintf("the workstream is %s, not assembled", featureState(feature.Value)))
	}
	latest, reason, err := p.S.DeliveryGate(ctx, p.Repository, stream, approval.Description)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if reason == "" && (latest.Commit != in.Commit || latest.DescriptionHash != in.DescriptionHash || latest.Style != approval.Style || !slices.Equal(latest.Messages, approval.Messages)) {
		reason = "a later owner approval replaced it"
	}
	if reason != "" {
		return p.Refuse(ctx, stream, in, reason)
	}
	report, _, err := LatestFinalReport(p.Repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if in.Maintenance && (!report.Maintenance || report.Upstream == nil || report.Upstream.Workstream == "" && (in.Upstream != cfg.Project.Upstream || in.Base != cfg.Project.BaseBranch)) {
		return p.Refuse(ctx, stream, in, "publication requires a fresh delivery maintenance review of its current base")
	}
	if !in.Maintenance {
		return p.MarkReady(ctx, stream, op, in, approval, report, cfg)
	}
	g, err := featureWorkspaces(cfg, p.Repository).Of(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	branch := featureBranch(stream)
	if branch == cfg.Project.BaseBranch {
		return p.Refuse(ctx, stream, in, "delivery cannot push to the project base branch")
	}
	title := approval.Title
	if approval.Style != in.Style || len(approval.Messages) == 0 {
		return p.Refuse(ctx, stream, in, "delivery messages and style require current owner approval")
	}
	if report.Upstream == nil {
		return p.Refuse(ctx, stream, in, "final review records no delivery base")
	}
	prior, err := Publications(p.Repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	published := ""
	for _, d := range prior {
		if d.Operation == op.ID {
			published = d.Commit
		}
	}
	if published == "" {
		published, err = (&workspace.Git{Clone: cfg.Project.Clone}).SignDelivery(ctx, op.ID, report.Upstream.Commit, in.Commit, approval.Messages, in.Style == SquashStyle)
		if err != nil {
			return p.Refuse(ctx, stream, in, "pre-publication amendment failed: "+err.Error())
		}
	}
	remote, err := g.Remote(ctx, in.Fork)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if report.Upstream != nil && report.Upstream.Workstream != "" {
		if in.Upstream != cfg.Project.PushRepository() || in.Base != report.Upstream.Branch {
			return p.Refuse(ctx, stream, in, "the dependent pull request target changed; refresh owner approval")
		}
		baseTip, exists, err := g.RemoteBranch(ctx, remote, in.Base)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if !exists || baseTip != report.Upstream.Commit {
			return p.Refuse(ctx, stream, in, "the published base branch differs from the reviewed base; refresh the base and review")
		}
	}
	record := DeliveryPublication{BaseWorkstream: in.BaseWorkstream, Approval: in.Approval, Operation: op.ID, Status: PublicationPushing, Style: in.Style, Fork: in.Fork, Remote: remote, Branch: branch, Reviewed: in.Commit, Commit: published,
		Upstream: in.Upstream, Base: in.Base, Draft: in.Draft, Title: title, Description: approval.Description, DescriptionHash: approval.DescriptionHash}
	for _, d := range prior {
		if in.Maintenance && d.Status == PublicationOpened && d.Upstream == cfg.Project.PushRepository() && !p.S.CanonicalPublication(d, cfg) {
			record.PriorURL = d.URL
		}
		if d.Operation == op.ID && (d.Approval != in.Approval || d.Reviewed != in.Commit || d.Commit != published || d.DescriptionHash != in.DescriptionHash || d.Description != approval.Description || d.Style != in.Style || d.Fork != in.Fork || d.Remote != remote || d.Branch != branch || d.Upstream != in.Upstream || d.Base != in.Base || d.BaseWorkstream != in.BaseWorkstream || d.Draft != in.Draft) {
			return p.Refuse(ctx, stream, in, "the recorded publication does not match this owner approval and delivery")
		}
	}
	if in.Retarget != nil {
		record.PriorURL = in.Retarget.URL
	}
	tip, exists, err := g.RemoteBranch(ctx, remote, branch)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if exists && tip != published && !slices.ContainsFunc(prior, func(d DeliveryPublication) bool { return d.Commit == tip && d.Branch == branch && d.Fork == in.Fork }) {
		return p.Refuse(ctx, stream, in, fmt.Sprintf("feature branch %s of %s is at %s, which no publication of this workstream pushed", branch, in.Fork, tip))
	}
	before, err := client.Find(ctx, in.Upstream, in.Fork, branch)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if err := RecordOwnPullRequestOutcomeFromFind(ctx, p.Repository, p.S.Now(), stream, op.ID, OwnPullRequestNumber(in, prior), before); err != nil {
		return coreadapter.OperationResult{}, err
	}
	if in.Retarget != nil && len(before) == 0 {
		return p.Refuse(ctx, stream, in, "the delivered pull request is missing")
	}
	if len(before) != 0 && (len(before) != 1 || !MatchingPublicationPR(before[0], in, branch, title, approval.Description, tip) && !MatchingRetargetPR(before[0], in, branch, tip) && !pullRequestHeadBehind(before[0], in, branch, title, approval.Description, tip, prior)) {
		return p.RefusePR(ctx, stream, in, branch, published, before)
	}
	if !exists || tip != published {
		if !slices.ContainsFunc(prior, func(d DeliveryPublication) bool { return d.Operation == op.ID && d.Commit == published }) {
			if err := p.RecordPublication(ctx, stream, record); err != nil {
				return coreadapter.OperationResult{}, err
			}
		}
		if err := p.S.Step("publish-recorded"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := g.Push(ctx, remote, published, branch, tip); err != nil {
			return coreadapter.OperationResult{}, fmt.Errorf("push %s to %s of %s: %w", published, branch, in.Fork, err)
		}
	}
	if err := p.S.Step("publish-pushed"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	found, err := client.Find(ctx, in.Upstream, in.Fork, branch)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if len(found) == 1 && pullRequestHeadBehind(found[0], in, branch, title, approval.Description, published, prior) {
		return coreadapter.OperationResult{}, fmt.Errorf("pull request #%d of %s is still at %s after %s was pushed to %s", found[0].Number, in.Upstream, found[0].HeadCommit, published, branch)
	}
	if in.Retarget != nil {
		if len(found) != 1 || found[0].Number != in.Retarget.Number || found[0].URL != in.Retarget.URL {
			return p.RefusePR(ctx, stream, in, branch, published, found)
		}
		if !MatchingPublicationPR(found[0], in, branch, title, approval.Description, published) {
			if !MatchingRetargetPR(found[0], in, branch, published) {
				return p.RefusePR(ctx, stream, in, branch, published, found)
			}
			if err := p.S.Step("publish-retargeting"); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if _, err := client.Update(ctx, in.Upstream, in.Retarget.Number, pulls.Update{Base: in.Base, Title: title, Body: approval.Description}); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if err := p.S.Step("publish-retargeted"); err != nil {
				return coreadapter.OperationResult{}, err
			}
			found, err = client.Find(ctx, in.Upstream, in.Fork, branch)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
		}
	}
	if len(found) == 0 {
		created, createErr := client.Create(ctx, in.Upstream, pulls.New{HeadRepository: in.Fork, Head: branch, Base: in.Base, Title: title, Body: approval.Description, Draft: in.Draft})
		if createErr != nil {
			return coreadapter.OperationResult{}, createErr
		}
		if err := p.S.Step("publish-opened"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		found, err = client.Find(ctx, in.Upstream, in.Fork, branch)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if len(found) != 1 || found[0].Number != created.Number {
			return coreadapter.OperationResult{}, fmt.Errorf("cannot verify pull request from %s:%s after creation", in.Fork, branch)
		}
	}
	if len(found) != 1 || !MatchingPublicationPR(found[0], in, branch, title, approval.Description, published) {
		return p.RefusePR(ctx, stream, in, branch, published, found)
	}
	pr := found[0]
	if !in.Draft && pr.Draft {
		ready, err := client.Ready(ctx, in.Upstream, pr.Number)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := p.S.Step("publish-ready"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		pr = ready
	}
	record.Status, record.PullRequest, record.URL, record.Draft = PublicationOpened, pr.Number, pr.URL, pr.Draft
	return p.Deliver(ctx, stream, in, record)
}

// MarkReady puts the owner's approved title and description on the draft pull
// request of the push that signed the reviewed commit, then marks it ready for
// review and records the workstream delivered with the approval. It pushes
// nothing and opens no pull request: a reviewed commit with no recorded push is
// refused with that reason, so it only ever acts on a pushed and watched
// commit. The pull request must be open, against the pushed base and at the
// pushed commit. When a merge-order dependency has
// not merged it changes nothing on the pull request, which stays a draft, and
// records the workstream delivered with the approval applied: the merge calls
// for a dependency update and a fresh review of the delivered workstream, and
// the owner's fresh approval then marks the pull request ready. A restart
// repeats only what the pull request does not show yet, so the description is
// updated and the pull request marked ready once.
func (p *Publisher) MarkReady(ctx context.Context, stream config.WorkstreamID, op coreadapter.Operation, in PublishInput, approval DeliveryApproval, report FinalReport, cfg *config.Config) (coreadapter.OperationResult, error) {
	client := p.S.PullRequests
	if report.Upstream == nil {
		return p.Refuse(ctx, stream, in, "final review records no delivery base")
	}
	if approval.Style != in.Style || len(approval.Messages) == 0 {
		return p.Refuse(ctx, stream, in, "delivery messages and style require current owner approval")
	}
	push, found, err := PushOfCommit(p.Repository, stream, report.Commit)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !found {
		return p.Refuse(ctx, stream, in, fmt.Sprintf("final review %d records no push of the reviewed commit %s to a pull request, so there is no draft pull request whose checks were watched; nothing was pushed, opened or marked ready. The final review pushes and watches the commit, and waits for a GitHub credential when the service was started without GITHUB_TOKEN", report.Review, report.Commit))
	}
	prs, err := client.Find(ctx, push.Upstream, push.Fork, push.Branch)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	i := slices.IndexFunc(prs, func(pr pulls.PullRequest) bool { return pr.Number == push.PullRequest })
	if i < 0 {
		return p.Refuse(ctx, stream, in, fmt.Sprintf("pull request #%d of %s is missing", push.PullRequest, push.Upstream))
	}
	pr := prs[i]
	if err := RecordOwnPullRequestOutcomeFromFind(ctx, p.Repository, p.S.Now(), stream, op.ID, pr.Number, prs); err != nil {
		return coreadapter.OperationResult{}, err
	}
	if outcome, finished := PullRequestOutcomeOf(pr); finished {
		return p.Refuse(ctx, stream, in, fmt.Sprintf("pull request #%d of %s is %s, so it was not marked ready for review", pr.Number, push.Upstream, outcome))
	}
	if pr.HeadCommit != push.Commit {
		pushes, err := Pushes(p.Repository, stream)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if slices.ContainsFunc(pushes, func(earlier Push) bool { return earlier.Commit == pr.HeadCommit }) {
			return coreadapter.OperationResult{}, fmt.Errorf("pull request #%d of %s is still at %s, not at the pushed %s", pr.Number, push.Upstream, pr.HeadCommit, push.Commit)
		}
		return p.Refuse(ctx, stream, in, fmt.Sprintf("pull request #%d of %s is at %s, which no push of this workstream made; the pushed commit is %s", pr.Number, push.Upstream, pr.HeadCommit, push.Commit))
	}
	if pr.Base != push.Base {
		return p.Refuse(ctx, stream, in, fmt.Sprintf("pull request #%d of %s is against %s, not the reviewed base %s", pr.Number, push.Upstream, pr.Base, push.Base))
	}
	reference, unresolved, err := p.S.UnresolvedMergeOrderEdge(p.Repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if unresolved || in.Draft {
		return p.DeliverDraft(ctx, stream, op, in, push, reference)
	}
	title, description := approval.Title, approval.Description
	if pr.Title != title || pr.Body != description {
		if _, err := client.Update(ctx, push.Upstream, pr.Number, pulls.Update{Base: pr.Base, Title: title, Body: description}); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := p.S.Step("ready-described"); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	if pr.Draft {
		if _, err := client.Ready(ctx, push.Upstream, pr.Number); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := p.S.Step("ready-marked"); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	return p.DeliverReady(ctx, stream, op, in, push)
}

// DeliverReady records, in one commit, final/ready.json, the workstream's move
// from assembled to delivered with a notice for the chief of staff, and the
// outcome of the approval's operation.
func (p *Publisher) DeliverReady(ctx context.Context, stream config.WorkstreamID, op coreadapter.Operation, in PublishInput, push Push) (coreadapter.OperationResult, error) {
	return p.deliverReady(ctx, stream, op, in, push, "")
}

func (p *Publisher) deliverReady(ctx context.Context, stream config.WorkstreamID, op coreadapter.Operation, in PublishInput, push Push, note string) (coreadapter.OperationResult, error) {
	at := p.S.Now()
	ready := DeliveryReady{Source: api.ReadyByApproval, Approval: in.Approval, PullRequest: push.PullRequest, URL: push.URL, Upstream: push.Upstream, Commit: push.Commit, At: at}
	doc, err := ReadyDocumentFor(p.Repository, stream, ready, op.ID)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	reason := fmt.Sprintf("pull request #%d (%s) is ready for review at %s, with the description the owner approved as %s revision %d", push.PullRequest, push.URL, push.Commit, DeliveryPath, in.Approval)
	notice := fmt.Sprintf("Workstream delivered: pull request #%d is ready for review at %s.", push.PullRequest, push.URL)
	txs, err := p.deliveredTransactions(stream, op, in, at, reason, notice, note)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return p.recordApplied(ctx, stream, in, reason, []trace.Document{doc}, txs...)
}

// DeliverDraft records, in one commit, final/ready.json with Draft set, the
// workstream's move from assembled to delivered and the outcome of an approval
// whose pull request stays a draft
// because the merge-order dependency reference has not merged. Nothing is
// written to the pull request.
func (p *Publisher) DeliverDraft(ctx context.Context, stream config.WorkstreamID, op coreadapter.Operation, in PublishInput, push Push, reference string) (coreadapter.OperationResult, error) {
	return p.deliverDraft(ctx, stream, op, in, push, reference, "")
}

func (p *Publisher) deliverDraft(ctx context.Context, stream config.WorkstreamID, op coreadapter.Operation, in PublishInput, push Push, reference, note string) (coreadapter.OperationResult, error) {
	waiting := "a merge-order dependency"
	if reference != "" {
		waiting = reference
	}
	at := p.S.Now()
	ready := DeliveryReady{Source: api.ReadyByApproval, Approval: in.Approval, Draft: true, PullRequest: push.PullRequest, URL: push.URL, Upstream: push.Upstream, Commit: push.Commit, At: at}
	doc, err := ReadyDocumentFor(p.Repository, stream, ready, op.ID)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	reason := fmt.Sprintf("the owner approved marking pull request #%d (%s) ready as %s revision %d; it stays a draft until %s merges", push.PullRequest, push.URL, DeliveryPath, in.Approval, waiting)
	notice := fmt.Sprintf("Workstream delivered: pull request #%d stays a draft at %s until %s merges; once it has and the dependency update has been reviewed, the owner approves again to mark it ready.", push.PullRequest, push.URL, waiting)
	txs, err := p.deliveredTransactions(stream, op, in, at, reason, notice, note)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return p.recordApplied(ctx, stream, in, reason, []trace.Document{doc}, txs...)
}

func (p *Publisher) deliveredTransactions(stream config.WorkstreamID, op coreadapter.Operation, in PublishInput, at time.Time, reason, notice, note string) ([]trace.Transaction, error) {
	feature, err := p.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return nil, err
	}
	subject, err := p.Repository.Workflow(stream, PublicationSubject)
	if err != nil {
		return nil, err
	}
	transition, _ := PublishIDs(in.Approval)
	deliveredID, err := deliveredTransitionID(p.Repository, stream, transition+"-delivered")
	if err != nil {
		return nil, err
	}
	header := func(id string) trace.Header {
		return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: p.Repository.Project(), Workstream: stream, At: at, Actor: foremanActor, Cause: op.ID}
	}
	events := []trace.Event{trace.Notice(deliveredID, "state", notice)}
	if note != "" {
		events = append(events, trace.Notice(deliveredID, "gave-up", note))
	}
	return []trace.Transaction{
		{ExpectedVersion: feature.Version,
			Transition: trace.Transition{Header: header(deliveredID), Subject: trace.FeatureSubject, From: feature.Value, To: DeliveredState, Reason: reason},
			Events:     events},
		{ExpectedVersion: subject.Version,
			Transition: trace.Transition{Header: header(transition + "-published"), Subject: PublicationSubject, From: subject.Value, To: fmt.Sprintf("published-%d", in.Approval), Reason: reason}},
	}, nil
}

func (p *Publisher) recordApplied(ctx context.Context, stream config.WorkstreamID, in PublishInput, reason string, docs []trace.Document, txs ...trace.Transaction) (coreadapter.OperationResult, error) {
	approval, err := ApprovalAt(p.Repository, stream, in.Approval)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	res, err := routeDeliveredBy(ctx, p.S.Router, p.Repository, stream, TriggerDeliveryApproved, LandDecider{By: approval.DecidedBy, Judgment: approval.Judgment}, docs, txs[0], txs[1:]...)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if !res.Committed {
		if recorded, outcomeErr := p.Outcome(stream, in.Approval); outcomeErr == nil && recorded != nil {
			return *recorded, nil
		}
		return coreadapter.OperationResult{}, fmt.Errorf("the delivery of workstream %s could not be recorded: %s", stream, res.Reason)
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}

func MatchingPublicationPR(pr pulls.PullRequest, in PublishInput, branch, title, description, commit string) bool {
	return (!in.Draft || pr.Draft) && (in.Retarget == nil || pr.Number == in.Retarget.Number && pr.URL == in.Retarget.URL) && pr.Number > 0 && pr.URL != "" && pr.State == "open" && !pr.Merged && pr.HeadRepository == in.Fork && pr.Head == branch && pr.Base == in.Base && pr.HeadCommit == commit && pr.Title == title && pr.Body == description
}

func pullRequestHeadBehind(pr pulls.PullRequest, in PublishInput, branch, title, description, tip string, prior []DeliveryPublication) bool {
	if pr.HeadCommit == tip || !slices.ContainsFunc(prior, func(d DeliveryPublication) bool {
		return d.Commit == pr.HeadCommit && d.Branch == branch && d.Fork == in.Fork
	}) {
		return false
	}
	pr.HeadCommit = tip
	return MatchingPublicationPR(pr, in, branch, title, description, tip) || MatchingRetargetPR(pr, in, branch, tip)
}

func (p *Publisher) RefusePR(ctx context.Context, stream config.WorkstreamID, in PublishInput, branch, commit string, found []pulls.PullRequest) (coreadapter.OperationResult, error) {
	var seen []string
	for _, f := range found {
		seen = append(seen, fmt.Sprintf("#%d (%s, against %s at %s)", f.Number, f.State, f.Base, f.HeadCommit))
	}
	return p.Refuse(ctx, stream, in, fmt.Sprintf("%s of %s already has pull requests %s, and none is the one open against %s at %s with the approved description", branch, in.Fork, strings.Join(seen, ", "), in.Base, commit))
}

// ApprovalAt returns revision k of the workstream's final/delivery.json.
func ApprovalAt(repository *trace.Repository, stream config.WorkstreamID, k int) (DeliveryApproval, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return DeliveryApproval{}, err
	}
	for _, d := range docs {
		if d.ID == DeliveryDocument && d.Revision == k {
			var approval DeliveryApproval
			err := json.Unmarshal([]byte(d.Content), &approval)
			return approval, err
		}
	}
	return DeliveryApproval{}, fmt.Errorf("workstream %s has no %s revision %d", stream, DeliveryPath, k)
}

// Publications returns every recorded revision of the workstream's
// final/publication.json, oldest first.
func Publications(repository *trace.Repository, stream config.WorkstreamID) ([]DeliveryPublication, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var out []DeliveryPublication
	for _, d := range docs {
		if d.ID != PublicationDocument {
			continue
		}
		var p DeliveryPublication
		if err := json.Unmarshal([]byte(d.Content), &p); err != nil {
			return nil, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func (p *Publisher) PublicationDocument(stream config.WorkstreamID, record DeliveryPublication) (trace.Document, error) {
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return trace.Document{}, err
	}
	revision, err := nextRevision(p.Repository, stream, PublicationDocument)
	if err != nil {
		return trace.Document{}, err
	}
	return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: PublicationDocument, Revision: revision, Project: p.Repository.Project(), Workstream: stream, At: p.S.Now(), Actor: foremanActor, Cause: record.Operation},
		Path: PublicationPath, Content: string(content) + "\n"}, nil
}

// RecordPublication records the next revision of final/publication.json.
func (p *Publisher) RecordPublication(ctx context.Context, stream config.WorkstreamID, record DeliveryPublication) error {
	doc, err := p.PublicationDocument(stream, record)
	if err != nil {
		return err
	}
	return p.Repository.RecordDocuments(ctx, []trace.Document{doc})
}

// Deliver records, in one commit, the opened publication, the workstream's
// move from assembled to delivered with a notice for the chief of staff, and
// the publication's outcome.
func (p *Publisher) Deliver(ctx context.Context, stream config.WorkstreamID, in PublishInput, record DeliveryPublication) (coreadapter.OperationResult, error) {
	return p.deliver(ctx, stream, in, record, "")
}

func (p *Publisher) deliver(ctx context.Context, stream config.WorkstreamID, in PublishInput, record DeliveryPublication, note string) (coreadapter.OperationResult, error) {
	doc, err := p.PublicationDocument(stream, record)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	feature, err := p.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	subject, err := p.Repository.Workflow(stream, PublicationSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	at := p.S.Now()
	header := func(id string) trace.Header {
		return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: p.Repository.Project(), Workstream: stream, At: at, Actor: foremanActor, Cause: record.Operation}
	}
	reason := fmt.Sprintf("pull request #%d (%s) is open against %s of %s from %s of %s at %s (%s, reviewed %s) with the description the owner approved as %s revision %d",
		record.PullRequest, record.URL, record.Base, record.Upstream, record.Branch, record.Fork, record.Commit, record.Style, record.Reviewed, DeliveryPath, record.Approval)
	transition, _ := PublishIDs(in.Approval)
	deliveredID := DeliveredState
	if in.Retarget != nil {
		deliveredID = transition + "-delivered"
	}
	txs := []trace.Transaction{
		{ExpectedVersion: feature.Version,
			Transition: trace.Transition{Header: header(deliveredID), Subject: trace.FeatureSubject, From: feature.Value, To: DeliveredState, Reason: reason},
			Events:     []trace.Event{trace.Notice(deliveredID, "state", fmt.Sprintf("Workstream delivered: pull request #%d is open at %s.", record.PullRequest, record.URL))}},
		{ExpectedVersion: subject.Version,
			Transition: trace.Transition{Header: header(transition + "-published"), Subject: PublicationSubject, From: subject.Value, To: fmt.Sprintf("published-%d", in.Approval), Reason: reason}},
	}
	if in.Maintenance {
		if feature.Value != DeliveredState {
			return coreadapter.OperationResult{}, errors.New("delivery maintenance requires a delivered feature")
		}
		txs = txs[1:]
		txs[0].Events = []trace.Event{trace.Notice(transition, "delivery", fmt.Sprintf("Delivery maintenance published at %s. The feature remains delivered.", record.URL))}
	}
	if note != "" {
		txs[0].Events = append(txs[0].Events, trace.Notice(transition, "gave-up", note))
	}
	if !in.Maintenance {
		return p.recordApplied(ctx, stream, in, reason, []trace.Document{doc}, txs...)
	}
	if _, err := p.Repository.RecordDocumentsWith(ctx, []trace.Document{doc}, txs...); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if recorded, outcomeErr := p.Outcome(stream, in.Approval); outcomeErr == nil && recorded != nil {
				return *recorded, nil
			}
		}
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}

// Refuse records why the approval was refused, tells the chief of staff, and
// returns the refusal as the operation's result. The workstream stays
// assembled; approving again asks for another application.
func (p *Publisher) Refuse(ctx context.Context, stream config.WorkstreamID, in PublishInput, reason string) (coreadapter.OperationResult, error) {
	recorded := fmt.Sprintf("owner approval %d was not applied: %s", in.Approval, reason)
	body := fmt.Sprintf("The pull request was not marked ready for review: %s. The workstream stays assembled until the owner marks it ready again.", reason)
	return p.refuse(ctx, stream, in, recorded, body)
}

func (p *Publisher) refuse(ctx context.Context, stream config.WorkstreamID, in PublishInput, recorded, body string) (coreadapter.OperationResult, error) {
	transition, _ := PublishIDs(in.Approval)
	id := transition + "-refused"
	state, err := p.Repository.Workflow(stream, PublicationSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: p.Repository.Project(), Workstream: stream, At: p.S.Now(), Actor: foremanActor, Cause: transition}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: PublicationSubject, From: state.Value, To: fmt.Sprintf("refused-%d", in.Approval), Reason: recorded},
		Events:     []trace.Event{trace.Notice(id, "chief", body)}}
	if _, err := p.Repository.Transact(ctx, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if result, outcomeErr := p.Outcome(stream, in.Approval); outcomeErr == nil && result != nil {
				return *result, nil
			}
		}
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "failed", Evidence: recorded}, nil
}

func MatchingRetargetPR(pr pulls.PullRequest, in PublishInput, branch, commit string) bool {
	if in.Retarget == nil {
		return false
	}
	return (!in.Draft || pr.Draft) && pr.Number == in.Retarget.Number && pr.URL == in.Retarget.URL && pr.State == "open" && !pr.Merged && pr.HeadRepository == in.Fork && pr.Head == branch && pr.HeadCommit == commit && pr.Base == in.Retarget.Base && pr.Title == in.Retarget.Title && pr.Body == in.Retarget.Description
}
