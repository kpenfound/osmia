package delivery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/reconcile"
)

// The words of a publication refusal that was not about the approval: an
// operation that gave up after repeated failures, and one whose outcome could
// not be read.
const (
	RefusedAfterRetries   = "gave up after repeated failures"
	RefusedUnknownOutcome = "its outcome could not be determined"
)

var _ reconcile.Bounded = (*Publisher)(nil)

// Bound reports whether the operation is a publication: each of its attempts
// runs under the attempt deadline and ends in a result after repeated failure.
func (p *Publisher) Bound(op coreadapter.Operation) bool { return PublicationAction(op) }

// Failing has nothing to tell the chief of staff: a publication that keeps
// failing opens its inbox entry (see PublicationEntry).
func (p *Publisher) Failing(context.Context, coreadapter.Operation, int, string) error { return nil }

// verdict is what the read-only reconcile of a publication found.
type verdict int

const (
	// verdictAbsent means the remote shows the effect is not there.
	verdictAbsent verdict = iota
	// verdictReady means the pull request is ready for review as approved.
	verdictReady
	// verdictDraft means the approval is applied by recording the pull request
	// as a draft, which changes nothing on it.
	verdictDraft
	// verdictOpened means the pushed branch and its pull request are as approved.
	verdictOpened
)

type reading struct {
	verdict   verdict
	evidence  string
	push      Push
	reference string
	record    DeliveryPublication
}

// GiveUp ends a publication that has failed past the limit and has no recorded
// outcome. It reads the pull request of the recorded push, its draft state and
// its head, without pushing, updating or marking anything ready, and records
// the publication through its own success path (DeliverReady, DeliverDraft or
// Deliver) when the approved effect is there, refuses it when the effect is
// confirmed absent, and refuses it saying its outcome could not be determined
// when the remote cannot be read.
func (p *Publisher) GiveUp(ctx context.Context, op coreadapter.Operation, why reconcile.GiveUpReason) (coreadapter.OperationResult, error) {
	in, err := DecodePublish(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := p.Stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	ctx = ghlog.WithScope(ctx, ghlog.Scope{Project: string(p.Repository.Project()), Workstream: string(stream)})
	// An outcome the publication recorded stands: a restart between its record
	// and the controller's result keeps it.
	if result, err := p.Outcome(stream, in.Approval); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	var found reading
	if in.Maintenance {
		found, err = p.readMaintenance(ctx, stream, op, in)
	} else {
		found, err = p.readReady(ctx, stream, op, in)
	}
	note := fmt.Sprintf("The publication of owner approval %d failed repeatedly (%s) but its effect is already on the pull request, so it is recorded as done and not repeated.", in.Approval, why.Summary())
	if err == nil && found.verdict != verdictAbsent {
		switch found.verdict {
		case verdictReady:
			return p.deliverReady(ctx, stream, op, in, found.push, note)
		case verdictDraft:
			return p.deliverDraft(ctx, stream, op, in, found.push, found.reference, note)
		default:
			return p.deliver(ctx, stream, in, found.record, note)
		}
	}
	write := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		write, cancel = context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
	}
	if err == nil {
		reason := fmt.Sprintf("%s (%s); the effect is confirmed absent: %s", RefusedAfterRetries, why.Summary(), found.evidence)
		recorded := fmt.Sprintf("owner approval %d was not applied: %s", in.Approval, reason)
		return p.refuse(write, stream, in, recorded, fmt.Sprintf("The pull request was not marked ready for review: %s. The workstream stays assembled until the owner marks it ready again.", reason))
	}
	reason := fmt.Sprintf("%s, and %s: %v (%s); no further push, ready or update call was made", RefusedAfterRetries, RefusedUnknownOutcome, err, why.Summary())
	recorded := fmt.Sprintf("owner approval %d: %s", in.Approval, reason)
	return p.refuse(write, stream, in, recorded, fmt.Sprintf("Whether the pull request was marked ready for review could not be determined: %s. The workstream stays assembled; check the pull request, and the owner can mark it ready again.", reason))
}

// readReady reads the draft pull request of the push that signed the reviewed
// commit, as MarkReady does before it acts. An error means the remote or the
// recorded evidence cannot be read.
func (p *Publisher) readReady(ctx context.Context, stream config.WorkstreamID, op coreadapter.Operation, in PublishInput) (reading, error) {
	approval, err := ApprovalAt(p.Repository, stream, in.Approval)
	if err != nil {
		return reading{}, err
	}
	report, _, err := LatestFinalReport(p.Repository, stream)
	if err != nil {
		return reading{}, err
	}
	if report.Upstream == nil {
		return reading{evidence: "final review records no delivery base"}, nil
	}
	if approval.Style != in.Style || len(approval.Messages) == 0 {
		return reading{evidence: "delivery messages and style require current owner approval"}, nil
	}
	push, found, err := PushOfCommit(p.Repository, stream, report.Commit)
	if err != nil {
		return reading{}, err
	}
	if !found {
		return reading{evidence: fmt.Sprintf("final review %d records no push of the reviewed commit %s to a pull request", report.Review, report.Commit)}, nil
	}
	client := p.S.PullRequests
	if !pulls.Credentialed(client) {
		return reading{}, ErrNoPullRequestCredential
	}
	prs, err := client.Find(ctx, push.Upstream, push.Fork, push.Branch)
	if err != nil {
		return reading{}, err
	}
	var pr pulls.PullRequest
	found = false
	for _, candidate := range prs {
		if candidate.Number == push.PullRequest {
			pr, found = candidate, true
		}
	}
	if !found {
		return reading{evidence: fmt.Sprintf("pull request #%d of %s is missing", push.PullRequest, push.Upstream)}, nil
	}
	if err := RecordOwnPullRequestOutcomeFromFind(ctx, p.Repository, p.S.Now(), stream, op.ID, pr.Number, prs); err != nil {
		return reading{}, err
	}
	if outcome, finished := PullRequestOutcomeOf(pr); finished {
		return reading{evidence: fmt.Sprintf("pull request #%d of %s is %s", pr.Number, push.Upstream, outcome)}, nil
	}
	if pr.HeadCommit != push.Commit {
		return reading{evidence: fmt.Sprintf("pull request #%d of %s is at %s, not the pushed commit %s", pr.Number, push.Upstream, pr.HeadCommit, push.Commit)}, nil
	}
	if pr.Base != push.Base {
		return reading{evidence: fmt.Sprintf("pull request #%d of %s is against %s, not the reviewed base %s", pr.Number, push.Upstream, pr.Base, push.Base)}, nil
	}
	reference, unresolved, err := p.S.UnresolvedMergeOrderEdge(p.Repository, stream)
	if err != nil {
		return reading{}, err
	}
	if unresolved || in.Draft {
		return reading{verdict: verdictDraft, push: push, reference: reference}, nil
	}
	title, description := PublishedTitle(approval, stream), approval.Description
	described := pr.Title == title && pr.Body == description
	switch {
	case !pr.Draft && described:
		return reading{verdict: verdictReady, push: push}, nil
	case !pr.Draft:
		return reading{evidence: fmt.Sprintf("pull request #%d of %s is ready for review, but its title and description are not the approved ones", pr.Number, push.Upstream)}, nil
	case described:
		return reading{evidence: fmt.Sprintf("pull request #%d of %s has the approved description but is still a draft", pr.Number, push.Upstream)}, nil
	}
	return reading{evidence: fmt.Sprintf("pull request #%d of %s is still a draft and does not have the approved description", pr.Number, push.Upstream)}, nil
}

// readMaintenance reads the branch and pull request a delivery maintenance
// publication pushes and opens, as Apply does before it acts. An error means
// the remote or the recorded evidence cannot be read.
func (p *Publisher) readMaintenance(ctx context.Context, stream config.WorkstreamID, op coreadapter.Operation, in PublishInput) (reading, error) {
	cfg := p.S.About(p.Repository)
	if !cfg.HasProject() || cfg.Project.ID != p.Repository.Project() {
		return reading{}, errors.New("the project is not active")
	}
	approval, err := ApprovalAt(p.Repository, stream, in.Approval)
	if err != nil {
		return reading{}, err
	}
	prior, err := Publications(p.Repository, stream)
	if err != nil {
		return reading{}, err
	}
	var record *DeliveryPublication
	for i, d := range prior {
		if d.Operation == op.ID {
			record = &prior[i]
		}
	}
	// The push is recorded before it is made: without a record nothing was pushed.
	if record == nil {
		return reading{evidence: "no push of this publication is recorded, so nothing was pushed or opened"}, nil
	}
	client := p.S.PullRequests
	if !pulls.Credentialed(client) {
		return reading{}, ErrNoPullRequestCredential
	}
	g, err := featureWorkspaces(cfg, p.Repository).Of(stream)
	if err != nil {
		return reading{}, err
	}
	tip, exists, err := g.RemoteBranch(ctx, record.Remote, record.Branch)
	if err != nil {
		return reading{}, err
	}
	if !exists || tip != record.Commit {
		return reading{evidence: fmt.Sprintf("%s of %s is at %q, not the commit %s the publication pushes", record.Branch, record.Fork, tip, record.Commit)}, nil
	}
	prs, err := client.Find(ctx, in.Upstream, in.Fork, record.Branch)
	if err != nil {
		return reading{}, err
	}
	title := PublishedTitle(approval, stream)
	if len(prs) != 1 || !MatchingPublicationPR(prs[0], in, record.Branch, title, approval.Description, record.Commit) {
		return reading{evidence: fmt.Sprintf("%s of %s holds the pushed commit %s, but %d pull requests of %s are open against %s with the approved description at it", record.Branch, record.Fork, record.Commit, len(prs), in.Upstream, in.Base)}, nil
	}
	opened := *record
	opened.Status, opened.PullRequest, opened.URL, opened.Draft = PublicationOpened, prs[0].Number, prs[0].URL, prs[0].Draft
	return reading{verdict: verdictOpened, record: opened}, nil
}
