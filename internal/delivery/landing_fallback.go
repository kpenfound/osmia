package delivery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/reconcile"
	"github.com/kpenfound/osmia/internal/trace"
)

var _ reconcile.Bounded = (*Lander)(nil)

// Bound reports whether the operation is a delivery landing: each of its
// attempts runs under the attempt deadline and ends in a result after repeated
// failure.
func (l *Lander) Bound(op coreadapter.Operation) bool { return LandingAction(op) }

// Failing has nothing to tell the chief of staff: a landing that keeps failing
// ends with a notice that says so.
func (l *Lander) Failing(context.Context, coreadapter.Operation, int, string) error { return nil }

// GiveUp ends a landing that has failed past the limit and has no recorded
// outcome. It fetches the base branch and pushes nothing: a base branch that
// holds the signed commit completes the landing through its own success path,
// a refused push whose pull request is already delivered completes the
// fallback, and anything else ends the attempt as failed, saying whether the
// absence of the landing is confirmed or the base branch could not be read. The
// workstream stays assembled, so the owner's usual delivery card applies.
func (l *Lander) GiveUp(ctx context.Context, op coreadapter.Operation, why reconcile.GiveUpReason) (coreadapter.OperationResult, error) {
	in, err := DecodeLand(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	stream, err := l.Stream(op, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	ctx = ghlog.WithScope(ctx, ghlog.Scope{Project: string(l.Repository.Project()), Workstream: string(stream)})
	defer lockLanding(op.ID)()
	if result, err := l.Outcome(stream, in.Landing); err != nil || result != nil {
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		return *result, nil
	}
	cfg := l.S.About(l.Repository)
	if !cfg.HasProject() || cfg.Project.ID != l.Repository.Project() {
		return coreadapter.OperationResult{}, errors.New("the project is not active")
	}
	run := &landingRun{Lander: l, cfg: cfg, stream: stream, op: op, in: in, branch: cfg.Project.BaseBranch}
	write := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		write, cancel = context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
	}
	intent, recorded, err := ReadLandingIntent(l.Repository, stream, in.Landing)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if recorded && intent.Status == LandingRefused {
		if feature, err := l.Repository.Workflow(stream, trace.FeatureSubject); err == nil && feature.Value != AssembledState {
			return run.completeFallback(write, intent)
		}
	}
	unreadable := func(err error) (coreadapter.OperationResult, error) {
		return run.fail(write, fmt.Sprintf("%s, and %s: %v (%s); nothing further was pushed", RefusedAfterRetries, RefusedUnknownOutcome, err, why.Summary()))
	}
	if run.g, err = featureWorkspaces(cfg, l.Repository).Of(stream); err != nil {
		return unreadable(err)
	}
	if run.remote, err = run.g.Remote(ctx, cfg.Project.Upstream); err != nil {
		return unreadable(err)
	}
	fetched, err := run.g.Fetch(ctx, run.remote, run.branch)
	if err != nil {
		return unreadable(err)
	}
	landed, err := run.holds(ctx, fetched)
	if err != nil {
		return unreadable(err)
	}
	if landed {
		return run.complete(write, intent, recorded, fetched)
	}
	return run.fail(write, fmt.Sprintf("%s (%s); the fetched %s of %s at %s does not hold %s, so the landing is confirmed absent", RefusedAfterRetries, why.Summary(), run.branch, cfg.Project.Upstream, fetched, in.Commit))
}

func (r *landingRun) fallback(ctx context.Context, intent LandingIntent) (coreadapter.OperationResult, error) {
	feature, err := r.Repository.Workflow(r.stream, trace.FeatureSubject)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if feature.Value == AssembledState {
		if result, err := r.markReady(ctx, intent); err != nil || result != nil {
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			return *result, nil
		}
	}
	return r.completeFallback(ctx, intent)
}

func (r *landingRun) completeFallback(ctx context.Context, intent LandingIntent) (coreadapter.OperationResult, error) {
	push, _, err := pushNumbered(r.Repository, r.stream, intent.Push)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	reason := fmt.Sprintf("the upstream refused the push of %s to %s of %s, so it did not land: %s. Pull request #%d was marked ready for review and delivers the workstream", r.in.Commit, intent.BaseBranch, intent.Upstream, intent.Refusal, push.PullRequest)
	notice := fmt.Sprintf("Workstream delivered without landing: the upstream refused the push of %s to %s of %s (%s). Pull request #%d (%s) was marked ready for review instead; no new pull request was opened.", r.in.Commit, intent.BaseBranch, intent.Upstream, intent.Refusal, push.PullRequest, push.URL)
	return r.finish(ctx, LandOutcomeFellBack, reason, notice)
}

func (r *landingRun) markReady(ctx context.Context, intent LandingIntent) (*coreadapter.OperationResult, error) {
	report, found, err := LatestFinalReport(r.Repository, r.stream)
	if err != nil {
		return nil, err
	}
	if !found || report.Review != intent.Review || report.Commit != intent.Reviewed {
		result, err := r.fail(ctx, fmt.Sprintf("the upstream refused the push (%s), and final review %d is no longer the latest, so its draft pull request is not marked ready", intent.Refusal, intent.Review))
		return &result, err
	}
	approval, revision, err := r.fallbackApproval(ctx, report)
	if err != nil {
		return nil, err
	}
	publisher := &Publisher{S: r.S, Repository: r.Repository}
	cfg := r.cfg
	push, found, err := pushNumbered(r.Repository, r.stream, intent.Push)
	if err != nil {
		return nil, err
	}
	if !found {
		result, err := r.fail(ctx, fmt.Sprintf("the upstream refused the push (%s), and push %d is not recorded", intent.Refusal, intent.Push))
		return &result, err
	}
	in := PublishInput{Approval: revision, Commit: approval.Commit, DescriptionHash: approval.DescriptionHash, Style: approval.Style, Fork: push.Fork, Upstream: push.Upstream, Base: push.Base}
	if err := r.S.landingStep("land-marking-ready"); err != nil {
		return nil, err
	}
	result, err := publisher.MarkReady(ctx, r.stream, coreadapter.Operation{ID: r.op.ID}, in, approval, report, cfg)
	if err != nil {
		return nil, err
	}
	if result.Outcome == "failed" {
		failed, err := r.fail(ctx, fmt.Sprintf("the upstream refused the push (%s), and the draft pull request could not be marked ready: %s", intent.Refusal, result.Evidence))
		return &failed, err
	}
	return nil, nil
}

func (r *landingRun) fallbackApproval(ctx context.Context, report FinalReport) (DeliveryApproval, int, error) {
	review, decision, approval, err := DeliveryDocuments(r.Repository, r.stream)
	if err != nil {
		return DeliveryApproval{}, 0, err
	}
	blocking := ReportBlocking(report)
	if approval != nil && approval.Review == report.Review && approval.ReviewRevision == review.Revision && approval.Commit == report.Commit && approval.Style == r.cfg.Project.Landing &&
		len(approval.Messages) > 0 && slices.Equal(approval.AcceptedBlocking, blocking) && approval.DescriptionHash == DescriptionHash(approval.Description) {
		if refused, err := PublicationRefused(r.Repository, r.stream, decision.Revision); err != nil {
			return DeliveryApproval{}, 0, err
		} else if !refused {
			return *approval, decision.Revision, nil
		}
	}
	recorded, next, err := r.S.recordDecidedApproval(ctx, r.Repository, r.cfg, r.stream, report, r.in.Decider, "the upstream refused the landing push")
	if err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return DeliveryApproval{}, 0, fmt.Errorf("the delivery approval changed while the fallback recorded its own: %w", err)
		}
		return DeliveryApproval{}, 0, err
	}
	return recorded, next, nil
}

func (s *Controller) recordDecidedApproval(ctx context.Context, repository *trace.Repository, cfg *config.Config, stream config.WorkstreamID, report FinalReport, decider LandDecider, because string) (DeliveryApproval, int, error) {
	review, decision, _, err := DeliveryDocuments(repository, stream)
	if err != nil {
		return DeliveryApproval{}, 0, err
	}
	blocking := ReportBlocking(report)
	title, body, _, err := s.draftDelivery(cfg, repository, stream, report)
	if err != nil {
		return DeliveryApproval{}, 0, err
	}
	style := cfg.Project.Landing
	messages, err := DeliveryMessages(ctx, cfg, report, style)
	if err != nil {
		return DeliveryApproval{}, 0, err
	}
	at := s.Now()
	by, judgment := decider.approval()
	next := decision.Revision + 1
	recorded := DeliveryApproval{Review: report.Review, ReviewRevision: review.Revision, Commit: report.Commit, Seal: report.Seal, SpecHash: report.SpecHash, Spec: report.Spec, Plan: report.Plan, Charter: report.Charter,
		DraftHash: draftHash(title, body, style, messages), Title: title, Description: body, DescriptionHash: DescriptionHash(body), Messages: messages, Style: style, At: at, AcceptedBlocking: blocking, DecidedBy: by, Judgment: judgment}
	content, err := jsonMarshal(recorded)
	if err != nil {
		return DeliveryApproval{}, 0, err
	}
	actor := foremanActor
	if decider.By == trace.DecidedByJev {
		actor = trace.JevActor
	}
	id := fmt.Sprintf("delivery-owner-%d", next)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: DeliveryDocument, Revision: next, Project: repository.Project(), Workstream: stream, At: at, Actor: actor, Cause: id}, Path: DeliveryPath, Content: content}
	state, err := repository.Workflow(stream, DeliverySubject)
	if err != nil {
		return DeliveryApproval{}, 0, err
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repository.Project(), Workstream: stream, At: at, Actor: actor, Cause: "delivery-approval"}
	tx := trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: DeliverySubject, From: state.Value, To: fmt.Sprintf("approved-%d", next),
		Reason: fmt.Sprintf("the service recorded the drafted description %s for final review %d at %s as %s's approval, because %s", recorded.DescriptionHash, report.Review, report.Commit, decider.name(), because)}}
	if _, err := repository.RecordDocumentsWith(ctx, []trace.Document{doc}, tx); err != nil {
		return DeliveryApproval{}, 0, err
	}
	return recorded, next, nil
}

func pushNumbered(repository *trace.Repository, stream config.WorkstreamID, n int) (Push, bool, error) {
	pushes, err := Pushes(repository, stream)
	if err != nil {
		return Push{}, false, err
	}
	for _, p := range pushes {
		if p.Number == n {
			return p, true, nil
		}
	}
	return Push{}, false, nil
}
