package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

// PublishAction is the repository-boundary operation action that publishes
// an assembled workstream's owner-approved branch and description: it pushes
// the delivery commit to the push repository, opens one pull request against
// the upstream base branch and records the workstream delivered.
const PublishAction = "publish"

const (
	// publicationSubject is the workflow subject that tracks the
	// publications of a workstream: requested-<k> once publishing the owner
	// approval recorded as revision k of final/delivery.json is asked for,
	// then published-<k> or refused-<k>.
	publicationSubject = "publication"
	// publicationPath is the workstream document of the publication, with
	// its trace record ID.
	publicationPath     = "final/publication.json"
	publicationDocument = "final-publication"
	// publicationPushing and publicationOpened are the statuses of a
	// publication record: the feature branch is being moved to the delivery
	// commit, then the pull request is open and the workstream delivered.
	publicationPushing = "pushing"
	publicationOpened  = "opened"
	// squashStyle delivers the reviewed branch as one owner-signed commit.
	squashStyle = "squash"
)

type retargetInput struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	Base        string `json:"base"`
	Description string `json:"description"`
}

// publishInput names one publication: the approval revision it publishes,
// the reviewed commit and description hash that approval records, and the
// delivery style, resolved push repository, upstream and base branch when it was
// asked for, so a retry publishes the same way.
type publishInput struct {
	BaseWorkstream  config.WorkstreamID `json:"base_workstream,omitempty"`
	Retarget        *retargetInput      `json:"retarget,omitempty"`
	Maintenance     bool                `json:"maintenance,omitempty"`
	Approval        int                 `json:"approval"`
	Commit          string              `json:"commit"`
	DescriptionHash string              `json:"description_hash"`
	Style           string              `json:"style"`
	Fork            string              `json:"fork"`
	Upstream        string              `json:"upstream"`
	Base            string              `json:"base"`
}

// DeliveryPublication is the document final/publication.json: the delivery
// of one owner approval. Reviewed is the reviewed branch commit and Commit
// the commit pushed to Branch of Fork through the clone's Remote; with the
// squash style it is one commit holding Reviewed's tree. PullRequest and URL
// name the pull request opened against Base of Upstream with Title and the
// approved Description once Status is opened. Fork is the resolved push
// repository, including upstream when the project has no fork. BaseWorkstream
// identifies a dependent request; it is empty for the canonical base branch.
type DeliveryPublication struct {
	BaseWorkstream  config.WorkstreamID `json:"base_workstream,omitempty"`
	PriorURL        string              `json:"prior_url,omitempty"`
	Approval        int                 `json:"approval"`
	Operation       string              `json:"operation"`
	Status          string              `json:"status"`
	Style           string              `json:"style"`
	Fork            string              `json:"fork"`
	Remote          string              `json:"remote"`
	Branch          string              `json:"branch"`
	Reviewed        string              `json:"reviewed"`
	Commit          string              `json:"commit"`
	Upstream        string              `json:"upstream"`
	Base            string              `json:"base"`
	PullRequest     int                 `json:"pull_request,omitempty"`
	URL             string              `json:"url,omitempty"`
	Title           string              `json:"title"`
	Description     string              `json:"description"`
	DescriptionHash string              `json:"description_hash"`
}

func publishIDs(approval int) (transition, event string) {
	transition = fmt.Sprintf("%s-%d", publicationSubject, approval)
	return transition, trace.EventID(transition, "run")
}

// publisher is the publication controller. Its pass asks to publish the
// current owner approval of every assembled workstream that is not paused;
// its reconciler runs each publication operation.
type publisher struct {
	s          *Service
	repository *trace.Repository
}

var _ coreadapter.Reconciler = (*publisher)(nil)

// Pass asks for the publication of an assembled workstream's latest owner
// approval once it passes the delivery gate, unless a publication of the
// workstream has neither a result nor a recorded outcome yet, or one of that
// approval was already asked for.
func (p *publisher) Pass(ctx context.Context) error {
	streams, err := p.repository.Workstreams()
	if err != nil {
		return err
	}
	state, _ := p.s.effective()
	librarian := librarianWorkstream(p.repository.Project())
	for _, stream := range streams {
		if stream == librarian || scheduler.Paused(state.Pauses, p.repository.Project(), stream) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.request(ctx, stream); err != nil {
			return fmt.Errorf("workstream %s publication: %w", stream, err)
		}
	}
	return nil
}

func (p *publisher) request(ctx context.Context, stream config.WorkstreamID) error {
	feature, err := p.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	if feature.Value != AssembledState {
		if feature.Value != DeliveredState {
			return nil
		}
		pending, err := upstreamDeliveryPending(p.repository, p.s.about(p.repository), stream)
		if err != nil || !pending {
			return err
		}
	}
	_, decision, approval, err := deliveryDocuments(p.repository, stream)
	if err != nil || approval == nil {
		return err
	}
	ops, err := p.repository.Operations(stream)
	if err != nil {
		return err
	}
	for _, o := range ops {
		if o.Operation.Action != PublishAction && o.Operation.Action != publishUpstreamAction {
			continue
		}
		in, err := decodePublish(o.Operation)
		if err != nil {
			return err
		}
		if in.Approval == decision.Revision {
			return nil
		}
		if o.Result == nil {
			if result, err := p.outcome(stream, in.Approval); err != nil || result == nil {
				return err
			}
		}
	}
	if _, reason, err := p.s.deliveryGate(ctx, p.repository, stream, approval.Description); err != nil || reason != "" {
		return err
	}
	cfg := p.s.about(p.repository)
	if !cfg.HasProject() || cfg.Project.ID != p.repository.Project() {
		return nil
	}
	in := publishInput{Maintenance: feature.Value == DeliveredState, Approval: decision.Revision, Commit: approval.Commit, DescriptionHash: approval.DescriptionHash, Style: cfg.Project.Landing, Fork: cfg.Project.PushRepository(), Upstream: cfg.Project.Upstream, Base: cfg.Project.BaseBranch}
	report, _, err := latestFinalReport(p.repository, stream)
	if err != nil {
		return err
	}
	if report.Upstream != nil && report.Upstream.Workstream != "" {
		in.Upstream = cfg.Project.PushRepository()
		in.Base = report.Upstream.Branch
		in.BaseWorkstream = report.Upstream.Workstream
	}
	if in.Maintenance && in.Fork == in.Upstream {
		records, err := publications(p.repository, stream)
		if err != nil {
			return err
		}
		for _, d := range records {
			if d.Status == publicationOpened && d.Upstream == in.Upstream && !canonicalPublication(d, cfg) {
				in.Retarget = &retargetInput{Number: d.PullRequest, URL: d.URL, Base: d.Base, Description: d.Description}
			}
		}
		if in.Retarget == nil {
			return fmt.Errorf("delivery maintenance has no dependent pull request to retarget")
		}
	}
	input, err := json.Marshal(in)
	if err != nil {
		return err
	}
	subject, err := p.repository.Workflow(stream, publicationSubject)
	if err != nil {
		return err
	}
	transition, event := publishIDs(in.Approval)
	action := PublishAction
	if in.Maintenance {
		action = publishUpstreamAction
	}
	op := coreadapter.Operation{ID: trace.OperationID(p.repository.Project(), stream, event), Boundary: coreadapter.RepositoryBoundary, Action: action, Input: input}
	requestAction := "opens a pull request"
	if in.Retarget != nil {
		requestAction = fmt.Sprintf("retargets pull request #%d", in.Retarget.Number)
	}
	reason := fmt.Sprintf("the owner approved description %s for %s at %s; publication pushes it (%s) to %s of %s and %s against %s of %s", in.DescriptionHash, featureBranch(stream), in.Commit, in.Style, featureBranch(stream), in.Fork, requestAction, in.Base, in.Upstream)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: transition, Revision: 1, Project: p.repository.Project(), Workstream: stream, At: p.s.now(), Actor: foremanActor, Cause: decision.Cause}
	tx := trace.Transaction{ExpectedVersion: subject.Version,
		Transition: trace.Transition{Header: h, Subject: publicationSubject, From: subject.Value, To: fmt.Sprintf("requested-%d", in.Approval), Reason: reason},
		Events:     []trace.Event{{ID: event, Kind: action, Body: fmt.Sprintf("Publish owner approval %d", in.Approval), Operation: &op}}}
	if _, err := p.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

func decodePublish(op coreadapter.Operation) (publishInput, error) {
	var in publishInput
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
	if in.Retarget != nil && (!in.Maintenance || in.Fork != in.Upstream || in.BaseWorkstream != "" || in.Retarget.Number < 1 || in.Retarget.URL == "" || in.Retarget.Base == "" || in.Retarget.Base == in.Base) {
		return in, errors.New("invalid dependent pull request retarget operation")
	}
	return in, nil
}

// stream returns the workstream a publication operation belongs to: the one
// whose run event derives the operation ID.
func (p *publisher) stream(op coreadapter.Operation, in publishInput) (config.WorkstreamID, error) {
	streams, err := p.repository.Workstreams()
	if err != nil {
		return "", err
	}
	_, event := publishIDs(in.Approval)
	for _, stream := range streams {
		if trace.OperationID(p.repository.Project(), stream, event) == op.ID {
			return stream, nil
		}
	}
	return "", fmt.Errorf("publish operation %s belongs to no workstream", op.ID)
}

// outcome returns the recorded result of the publication of approval k:
// succeeded once it recorded the workstream delivered, failed once its
// refusal is recorded, nil before either.
func (p *publisher) outcome(stream config.WorkstreamID, k int) (*coreadapter.OperationResult, error) {
	transitions, err := trace.Read[trace.Transition](p.repository, stream)
	if err != nil {
		return nil, err
	}
	transition, _ := publishIDs(k)
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

// publicationRefused reports whether the publication of approval k was
// refused.
func publicationRefused(repository *trace.Repository, stream config.WorkstreamID, k int) (bool, error) {
	result, err := (&publisher{repository: repository}).outcome(stream, k)
	return result != nil && result.Outcome == "failed", err
}

// Inspect reads the recorded transitions: a recorded outcome completes the
// operation; otherwise it is absent, and Apply inspects the push repository and its pull
// requests before acting.
func (p *publisher) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := decodePublish(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	stream, err := p.stream(op, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	result, err := p.outcome(stream, in.Approval)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if result != nil {
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "publication " + result.Outcome, Result: result}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: fmt.Sprintf("publication of owner approval %d is not recorded; it inspects the push repository and its pull requests before acting", in.Approval)}, nil
}

// Apply publishes owner approval k. The workstream must be assembled and the
// approval must still pass the delivery gate for the description it records:
// the feature branch, final review and governing documents unchanged since.
// Delivery amends the approved messages and identity and signs the outgoing
// commits, preserving the reviewed tree and base; squash produces one commit. The service then reads the feature branch: at the delivery commit it is
// already pushed; absent, or at a commit an earlier publication of the
// workstream recorded, it is pushed with that commit as the expected one;
// at any other commit the publication is refused. An existing pull request
// must match the approved base, description and current branch tip before a
// push. final/publication.json records the delivery commit before the push.
// After the push the pull request head is checked at the delivery commit; if
// none exists, one is opened and checked. A maintenance retarget instead
// reconciles the recorded request and updates its base and approved text. One commit then records the opened
// publication, the workstream's move to delivered with a notice, and the
// publication's outcome. A refusal records the reason and tells the chief of
// staff. Git, GitHub and storage errors leave the operation pending for
// another attempt, which inspects the push repository and pull requests again.
func (p *publisher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := decodePublish(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := p.stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if result, err := p.outcome(stream, in.Approval); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	cfg := p.s.about(p.repository)
	if !cfg.HasProject() || cfg.Project.ID != p.repository.Project() {
		return coreadapter.OperationResult{}, errors.New("the project is not active")
	}
	client := p.s.options.PullRequests
	if client == nil {
		return coreadapter.OperationResult{}, errors.New("the service has no pull request client")
	}
	approval, err := approvalAt(p.repository, stream, in.Approval)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if approval.Commit != in.Commit || approval.DescriptionHash != in.DescriptionHash {
		return coreadapter.OperationResult{}, fmt.Errorf("%s revision %d does not record the approval of %s with description %s", deliveryPath, in.Approval, in.Commit, in.DescriptionHash)
	}
	feature, err := p.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if feature.Value != AssembledState && !(feature.Value == DeliveredState && in.Maintenance) {
		return p.refuse(ctx, stream, in, fmt.Sprintf("the workstream is %s, not assembled", featureState(feature.Value)))
	}
	latest, reason, err := p.s.deliveryGate(ctx, p.repository, stream, approval.Description)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if reason == "" && (latest.Commit != in.Commit || latest.DescriptionHash != in.DescriptionHash || latest.Style != approval.Style || !slices.Equal(latest.Messages, approval.Messages)) {
		reason = "a later owner approval replaced it"
	}
	if reason != "" {
		return p.refuse(ctx, stream, in, reason)
	}
	report, _, err := latestFinalReport(p.repository, stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if in.Maintenance && (!report.Maintenance || in.Upstream != cfg.Project.Upstream || in.Base != cfg.Project.BaseBranch || report.Upstream == nil || report.Upstream.Workstream != "") {
		return p.refuse(ctx, stream, in, "upstream publication requires a fresh delivery maintenance review of the integrated base")
	}
	g, err := featureWorkspaces(cfg, p.repository).of(stream)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	branch := featureBranch(stream)
	if branch == cfg.Project.BaseBranch {
		return p.refuse(ctx, stream, in, "delivery cannot push to the project base branch")
	}
	title := deliveryTitle(approval.Description, stream)
	if approval.Style != in.Style || len(approval.Messages) == 0 {
		return p.refuse(ctx, stream, in, "delivery messages and style require current owner approval")
	}
	if report.Upstream == nil {
		return p.refuse(ctx, stream, in, "final review records no delivery base")
	}
	prior, err := publications(p.repository, stream)
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
		requested, err := (&foreman{masons: &masons{s: p.s, cfg: cfg, repository: p.repository}}).requestedAt(stream, op.ID)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		published, err = (&workspace.Git{Clone: cfg.Project.Clone}).SignDelivery(ctx, op.ID, report.Upstream.Commit, in.Commit, approval.Messages, in.Style == squashStyle, requested)
		if err != nil {
			return p.refuse(ctx, stream, in, "pre-publication amendment failed: "+err.Error())
		}
	}
	remote, err := g.Remote(ctx, in.Fork)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if report.Upstream != nil && report.Upstream.Workstream != "" {
		if in.Upstream != cfg.Project.PushRepository() || in.Base != report.Upstream.Branch {
			return p.refuse(ctx, stream, in, "the dependent pull request target changed; refresh owner approval")
		}
		baseTip, exists, err := g.RemoteBranch(ctx, remote, in.Base)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if !exists || baseTip != report.Upstream.Commit {
			return p.refuse(ctx, stream, in, "the published base branch differs from the reviewed base; refresh the base and review")
		}
	}
	record := DeliveryPublication{BaseWorkstream: in.BaseWorkstream, Approval: in.Approval, Operation: op.ID, Status: publicationPushing, Style: in.Style, Fork: in.Fork, Remote: remote, Branch: branch, Reviewed: in.Commit, Commit: published,
		Upstream: in.Upstream, Base: in.Base, Title: title, Description: approval.Description, DescriptionHash: approval.DescriptionHash}
	for _, d := range prior {
		if in.Maintenance && d.Status == publicationOpened && d.Upstream == cfg.Project.PushRepository() && !canonicalPublication(d, cfg) {
			record.PriorURL = d.URL
		}
		if d.Operation == op.ID && (d.Approval != in.Approval || d.Reviewed != in.Commit || d.Commit != published || d.DescriptionHash != in.DescriptionHash || d.Description != approval.Description || d.Style != in.Style || d.Fork != in.Fork || d.Remote != remote || d.Branch != branch || d.Upstream != in.Upstream || d.Base != in.Base || d.BaseWorkstream != in.BaseWorkstream) {
			return p.refuse(ctx, stream, in, "the recorded publication does not match this owner approval and delivery")
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
		return p.refuse(ctx, stream, in, fmt.Sprintf("feature branch %s of %s is at %s, which no publication of this workstream pushed", branch, in.Fork, tip))
	}
	// An open PR at the old owned tip advances when the feature branch is pushed.
	// Read it before the push so a closed or edited PR cannot be hidden by that advance.
	before, err := client.Find(ctx, in.Upstream, in.Fork, branch)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if in.Retarget != nil && len(before) == 0 {
		return p.refuse(ctx, stream, in, "the dependent pull request is missing")
	}
	if len(before) != 0 && (len(before) != 1 || !matchingPublicationPR(before[0], in, branch, approval.Description, tip) && !matchingRetargetPR(before[0], in, branch, tip)) {
		return p.refusePR(ctx, stream, in, branch, published, before)
	}
	if !exists || tip != published {
		if !slices.ContainsFunc(prior, func(d DeliveryPublication) bool { return d.Operation == op.ID && d.Commit == published }) {
			if err := p.recordPublication(ctx, stream, record); err != nil {
				return coreadapter.OperationResult{}, err
			}
		}
		if err := p.s.step("publish-recorded"); err != nil {
			return coreadapter.OperationResult{}, err
		}
		if err := g.Push(ctx, remote, published, branch, tip); err != nil {
			return coreadapter.OperationResult{}, fmt.Errorf("push %s to %s of %s: %w", published, branch, in.Fork, err)
		}
	}
	if err := p.s.step("publish-pushed"); err != nil {
		return coreadapter.OperationResult{}, err
	}
	found, err := client.Find(ctx, in.Upstream, in.Fork, branch)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if in.Retarget != nil {
		if len(found) != 1 || found[0].Number != in.Retarget.Number || found[0].URL != in.Retarget.URL {
			return p.refusePR(ctx, stream, in, branch, published, found)
		}
		if !matchingPublicationPR(found[0], in, branch, approval.Description, published) {
			if !matchingRetargetPR(found[0], in, branch, published) {
				return p.refusePR(ctx, stream, in, branch, published, found)
			}
			if err := p.s.step("publish-retargeting"); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if _, err := client.Update(ctx, in.Upstream, in.Retarget.Number, pulls.Update{Base: in.Base, Title: title, Body: approval.Description}); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if err := p.s.step("publish-retargeted"); err != nil {
				return coreadapter.OperationResult{}, err
			}
			found, err = client.Find(ctx, in.Upstream, in.Fork, branch)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
		}
	}
	if len(found) == 0 {
		created, createErr := client.Create(ctx, in.Upstream, pulls.New{HeadRepository: in.Fork, Head: branch, Base: in.Base, Title: title, Body: approval.Description})
		if createErr != nil {
			return coreadapter.OperationResult{}, createErr
		}
		if err := p.s.step("publish-opened"); err != nil {
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
	if len(found) != 1 || !matchingPublicationPR(found[0], in, branch, approval.Description, published) {
		return p.refusePR(ctx, stream, in, branch, published, found)
	}
	pr := found[0]
	record.Status, record.PullRequest, record.URL = publicationOpened, pr.Number, pr.URL
	return p.deliver(ctx, stream, in, record)
}

func matchingPublicationPR(pr pulls.PullRequest, in publishInput, branch, description, commit string) bool {
	return (in.Retarget == nil || pr.Number == in.Retarget.Number && pr.URL == in.Retarget.URL) && pr.Number > 0 && pr.URL != "" && pr.State == "open" && pr.HeadRepository == in.Fork && pr.Head == branch && pr.Base == in.Base && pr.HeadCommit == commit && pr.Body == description
}

func (p *publisher) refusePR(ctx context.Context, stream config.WorkstreamID, in publishInput, branch, commit string, found []pulls.PullRequest) (coreadapter.OperationResult, error) {
	var seen []string
	for _, f := range found {
		seen = append(seen, fmt.Sprintf("#%d (%s, against %s at %s)", f.Number, f.State, f.Base, f.HeadCommit))
	}
	return p.refuse(ctx, stream, in, fmt.Sprintf("%s of %s already has pull requests %s, and none is the one open against %s at %s with the approved description", branch, in.Fork, strings.Join(seen, ", "), in.Base, commit))
}

// deliveryTitle is the pull request title and delivery commit subject: the
// description's first non-empty line without its heading marks.
func deliveryTitle(description string, stream config.WorkstreamID) string {
	for _, line := range strings.Split(description, "\n") {
		if line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#")); line != "" {
			if runes := []rune(line); len(runes) > subjectLength {
				line = string(append(runes[:subjectLength-1], '…'))
			}
			return line
		}
	}
	return "Workstream " + string(stream)
}

// approvalAt returns revision k of the workstream's final/delivery.json.
func approvalAt(repository *trace.Repository, stream config.WorkstreamID, k int) (DeliveryApproval, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return DeliveryApproval{}, err
	}
	for _, d := range docs {
		if d.ID == deliveryDocument && d.Revision == k {
			var approval DeliveryApproval
			err := json.Unmarshal([]byte(d.Content), &approval)
			return approval, err
		}
	}
	return DeliveryApproval{}, fmt.Errorf("workstream %s has no %s revision %d", stream, deliveryPath, k)
}

// publications returns every recorded revision of the workstream's
// final/publication.json, oldest first.
func publications(repository *trace.Repository, stream config.WorkstreamID) ([]DeliveryPublication, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	var out []DeliveryPublication
	for _, d := range docs {
		if d.ID != publicationDocument {
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

func (p *publisher) publicationDocument(stream config.WorkstreamID, record DeliveryPublication) (trace.Document, error) {
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return trace.Document{}, err
	}
	revision, err := nextRevision(p.repository, stream, publicationDocument)
	if err != nil {
		return trace.Document{}, err
	}
	return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: publicationDocument, Revision: revision, Project: p.repository.Project(), Workstream: stream, At: p.s.now(), Actor: foremanActor, Cause: record.Operation},
		Path: publicationPath, Content: string(content) + "\n"}, nil
}

// recordPublication records the next revision of final/publication.json.
func (p *publisher) recordPublication(ctx context.Context, stream config.WorkstreamID, record DeliveryPublication) error {
	doc, err := p.publicationDocument(stream, record)
	if err != nil {
		return err
	}
	return p.repository.RecordDocuments(ctx, []trace.Document{doc})
}

// deliver records, in one commit, the opened publication, the workstream's
// move from assembled to delivered with a notice for the chief of staff, and
// the publication's outcome.
func (p *publisher) deliver(ctx context.Context, stream config.WorkstreamID, in publishInput, record DeliveryPublication) (coreadapter.OperationResult, error) {
	doc, err := p.publicationDocument(stream, record)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	feature, err := p.repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	subject, err := p.repository.Workflow(stream, publicationSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	at := p.s.now()
	header := func(id string) trace.Header {
		return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: p.repository.Project(), Workstream: stream, At: at, Actor: foremanActor, Cause: record.Operation}
	}
	reason := fmt.Sprintf("pull request #%d (%s) is open against %s of %s from %s of %s at %s (%s, reviewed %s) with the description the owner approved as %s revision %d",
		record.PullRequest, record.URL, record.Base, record.Upstream, record.Branch, record.Fork, record.Commit, record.Style, record.Reviewed, deliveryPath, record.Approval)
	transition, _ := publishIDs(in.Approval)
	txs := []trace.Transaction{
		{ExpectedVersion: feature.Version,
			Transition: trace.Transition{Header: header(DeliveredState), Subject: trace.FeatureSubject, From: feature.Value, To: DeliveredState, Reason: reason},
			Events:     []trace.Event{trace.Notice(DeliveredState, "state", fmt.Sprintf("Workstream delivered: pull request #%d is open at %s.", record.PullRequest, record.URL))}},
		{ExpectedVersion: subject.Version,
			Transition: trace.Transition{Header: header(transition + "-published"), Subject: publicationSubject, From: subject.Value, To: fmt.Sprintf("published-%d", in.Approval), Reason: reason}},
	}
	if in.Maintenance {
		if feature.Value != DeliveredState {
			return coreadapter.OperationResult{}, errors.New("delivery maintenance requires a delivered feature")
		}
		txs = txs[1:]
		txs[0].Events = []trace.Event{trace.Notice(transition, "delivery", fmt.Sprintf("Upstream pull request opened at %s; dependent request retained at %s. The feature remains delivered.", record.URL, record.PriorURL))}
	}
	if _, err := p.repository.RecordDocumentsWith(ctx, []trace.Document{doc}, txs...); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if recorded, outcomeErr := p.outcome(stream, in.Approval); outcomeErr == nil && recorded != nil {
				return *recorded, nil
			}
		}
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: reason}, nil
}

// refuse records why the publication was refused, tells the chief of staff,
// and returns the refusal as the operation's result. The workstream stays
// assembled; approving delivery again asks for another publication.
func (p *publisher) refuse(ctx context.Context, stream config.WorkstreamID, in publishInput, reason string) (coreadapter.OperationResult, error) {
	transition, _ := publishIDs(in.Approval)
	id := transition + "-refused"
	state, err := p.repository.Workflow(stream, publicationSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	recorded := fmt.Sprintf("owner approval %d was not published: %s", in.Approval, reason)
	body := fmt.Sprintf("Delivery was not published: %s. No pull request was opened; the workstream stays assembled until the owner approves delivery again.", reason)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: p.repository.Project(), Workstream: stream, At: p.s.now(), Actor: foremanActor, Cause: transition}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: publicationSubject, From: state.Value, To: fmt.Sprintf("refused-%d", in.Approval), Reason: recorded},
		Events:     []trace.Event{trace.Notice(id, "chief", body)}}
	if _, err := p.repository.Transact(ctx, tx); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if result, outcomeErr := p.outcome(stream, in.Approval); outcomeErr == nil && result != nil {
				return *result, nil
			}
		}
		return coreadapter.OperationResult{}, err
	}
	return coreadapter.OperationResult{Outcome: "failed", Evidence: recorded}, nil
}

func matchingRetargetPR(pr pulls.PullRequest, in publishInput, branch, commit string) bool {
	if in.Retarget == nil {
		return false
	}
	return pr.Number == in.Retarget.Number && pr.URL == in.Retarget.URL && pr.State == "open" && !pr.Merged && pr.HeadRepository == in.Fork && pr.Head == branch && pr.HeadCommit == commit && pr.Base == in.Retarget.Base && pr.Body == in.Retarget.Description
}
