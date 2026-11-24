package delivery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/gitdiff"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/jev"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

// The options of Jev's delivery-route decision. They are the routes a delivery
// approval takes.
const (
	RouteLand        = DeliveryRouteLand
	RoutePullRequest = "pull-request"
)

const (
	maxRouteSummary = 2 << 10
	maxRouteReport  = 12 << 10
	maxRouteAreas   = 40
)

func deliveryRouteKey(stream config.WorkstreamID, push Push) string {
	return jev.DeliveryRouteKey(stream, push.Review, push.Commit)
}

func routeLabel(route string) string {
	if route == RouteLand {
		return "landing it on the base branch"
	}
	return "marking the pull request ready for review"
}

func (s *Controller) deliveryRouteReason(ctx context.Context, repository *trace.Repository, cfg *config.Config, stream config.WorkstreamID) (landingPush, string, error) {
	if cfg.HasProject() && cfg.Project.Lands() && !strings.EqualFold(cfg.Project.PushRepository(), cfg.Project.Upstream) {
		return landingPush{}, fmt.Sprintf("the project delivers through %s, not its upstream %s", cfg.Project.PushRepository(), cfg.Project.Upstream), nil
	}
	return s.landingTarget(ctx, repository, cfg, stream, false, true)
}

// DeliveryRouteGate returns the gate of the workstream's delivery, whose
// decision is how it is delivered: landed on the base branch, or as its draft
// pull request. It is nil unless the workstream is assembled with a current
// push, and, while Jev has not answered, unless the delivery is clean, so that
// Jev is asked only about a delivery nothing holds back. A result recorded for
// the occurrence stays a gate even when the delivery stopped being clean: a
// binding pick is applied, or refused by the service and handed to the owner,
// and the card shows Jev's result.
func (s *Controller) DeliveryRouteGate(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (*intake.Gate, error) {
	cfg := s.About(repository)
	if !cfg.HasProject() || cfg.Project.ID != repository.Project() || !cfg.Project.Lands() {
		return nil, nil
	}
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return nil, err
	}
	if feature.Value != AssembledState {
		return nil, nil
	}
	push, found, err := CurrentPush(repository, stream)
	if err != nil || !found {
		return nil, err
	}
	if _, retrying, err := PendingLandingRoute(repository, stream); err != nil || retrying {
		return nil, err
	}
	key := deliveryRouteKey(stream, push)
	recorded, err := jev.Recorded(repository, stream, jev.KindDeliveryRoute, key)
	if err != nil {
		return nil, err
	}
	if recorded.Status == jev.None {
		if _, reason, err := s.deliveryRouteReason(ctx, repository, cfg, stream); err != nil || reason != "" {
			return nil, err
		}
	}
	review, decision, approval, err := DeliveryDocuments(repository, stream)
	if err != nil {
		return nil, err
	}
	if approval != nil && approval.DecidedBy == trace.DecidedByJev {
		if refused, err := PublicationRefused(repository, stream, decision.Revision); err != nil {
			return nil, err
		} else if refused {
			return nil, nil
		}
	}
	reached := push.At
	if review.At.After(reached) {
		reached = review.At
	}
	project := repository.Project()
	g := &intake.Gate{Kind: jev.KindDeliveryRoute, Project: project, Stream: stream, Key: key,
		Label: fmt.Sprintf("the delivery of final review %d at %s of workstream %s", push.Review, push.Commit, stream), Cause: FinalReportDocument, Reached: reached,
		Options: []jev.DecisionOption{
			{Name: RouteLand, Description: "Land the workstream: the service fast-forwards the project's base branch to the signed commit checks passed on. No pull request is merged."},
			{Name: RoutePullRequest, Description: "Deliver the workstream as its draft pull request: the service marks it ready for review and the owner or the project merges it."}}}
	g.State = func(ctx context.Context) (string, []jev.Source, error) {
		target, reason, err := s.deliveryRouteReason(ctx, repository, cfg, stream)
		if err != nil {
			return "", nil, err
		}
		if reason != "" {
			return "", nil, errors.New("the delivery is no longer clean: " + reason)
		}
		return s.deliveryRouteState(ctx, repository, cfg, stream, push, target.report, review)
	}
	g.Apply = func(ctx context.Context, res jev.Result) *APIError {
		return s.applyDeliveryRoute(ctx, repository, stream, key, res)
	}
	g.Decision = func() (intake.GateDecision, error) {
		return s.deliveryRouteDecision(repository, stream, push)
	}
	return g, nil
}

func (s *Controller) deliveryRouteDecision(repository *trace.Repository, stream config.WorkstreamID, push Push) (intake.GateDecision, error) {
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return intake.GateDecision{}, err
	}
	if route, retrying, err := PendingLandingRoute(repository, stream); err != nil {
		return intake.GateDecision{}, err
	} else if retrying {
		return intake.GateDecision{Recorded: true, ByJev: route.Decider.By == trace.DecidedByJev}, nil
	}
	l := &Lander{Repository: repository}
	_, input, outcome, landing, err := l.latest(stream)
	if err != nil {
		return intake.GateDecision{}, err
	}
	landedPush := landing && input.Push == push.Number
	if landedPush && input.Decider.By == trace.DecidedByJev && outcome != nil && outcome.Outcome == LandOutcomeFailed && feature.Value == AssembledState {
		return intake.GateDecision{}, nil
	}
	if landedPush && (outcome == nil || outcome.Outcome != LandOutcomeFailed) {
		return intake.GateDecision{Recorded: true, ByJev: input.Decider.By == trace.DecidedByJev}, nil
	}
	_, decision, approval, err := DeliveryDocuments(repository, stream)
	if err != nil {
		return intake.GateDecision{}, err
	}
	if approval != nil {
		refused, err := PublicationRefused(repository, stream, decision.Revision)
		if err != nil {
			return intake.GateDecision{}, err
		}
		if !refused || approval.DecidedBy == trace.DecidedByJev {
			return intake.GateDecision{Recorded: true, ByJev: approval.DecidedBy == trace.DecidedByJev}, nil
		}
	}
	if feature.Value != AssembledState {
		return intake.GateDecision{Recorded: true}, nil
	}
	return intake.GateDecision{}, nil
}

func (s *Controller) deliveryRouteState(ctx context.Context, repository *trace.Repository, cfg *config.Config, stream config.WorkstreamID, push Push, report FinalReport, review trace.Document) (string, []jev.Source, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return "", nil, err
	}
	var spec trace.Document
	for _, d := range docs {
		if d.ID == plan.SpecDocument && d.Revision == report.Spec {
			spec = d
		}
	}
	title, err := WorkstreamTitle(repository, stream, spec, trace.Document{})
	if err != nil {
		return "", nil, err
	}
	checks, err := PresentChecks(repository, stream, push.Review, push.Reviewed)
	if err != nil {
		return "", nil, err
	}
	size, err := s.changeSize(ctx, repository, cfg, stream, push, report)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	intake.GateSection(&b, "Decision", fmt.Sprintf("Workstream %s of the project %s is finished and its delivery is clean: the final review left nothing blocking, checks passed on the pushed commit and nothing waits to merge before it. Choose how it is delivered. Landing moves the base branch %s of %s to the checked commit by a fast-forward; the pull request route marks the existing draft pull request ready for review. No code is shown here.", stream, repository.Project(), cfg.Project.BaseBranch, cfg.Project.Upstream), 0)
	intake.GateSection(&b, "Spec summary", title+"\n"+SpecSummary(spec.Content), maxRouteSummary)
	intake.GateSection(&b, "Final review report", routeReport(report), maxRouteReport)
	intake.GateSection(&b, "Check results", routeChecks(checks), 0)
	intake.GateSection(&b, "Size of the change and the areas it touches", size, 0)
	return b.String(), []jev.Source{{Kind: "final-review", ID: FinalReportDocument, Revision: review.Revision}, {Kind: "spec", ID: plan.SpecDocument, Revision: report.Spec}}, nil
}

func routeReport(r FinalReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Final review %d of commit %s.\n", r.Review, r.Commit)
	if text := strings.TrimSpace(r.Summary); text != "" {
		b.WriteString(text + "\n")
	}
	for _, c := range r.Criteria {
		switch {
		case c.Gap != "":
			fmt.Fprintf(&b, "- Criterion %s is not served: %s\n", c.Criterion, c.Gap)
		case c.Evidence != "":
			fmt.Fprintf(&b, "- Criterion %s is served: %s\n", c.Criterion, c.Evidence)
		default:
			fmt.Fprintf(&b, "- Criterion %s\n", c.Criterion)
		}
	}
	if len(r.Findings) == 0 {
		b.WriteString("No findings.\n")
	}
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "- Finding (%s): %s\n", f.Severity, f.Describe())
	}
	return b.String()
}

func routeChecks(c *DeliveryChecks) string {
	if c == nil {
		return "No check result is recorded."
	}
	out := c.Summary + "."
	if c.Commit != "" {
		out += " Commit " + c.Commit + "."
	}
	return out
}

func (s *Controller) changeSize(ctx context.Context, repository *trace.Repository, cfg *config.Config, stream config.WorkstreamID, push Push, report FinalReport) (string, error) {
	if report.Upstream == nil || report.Upstream.Commit == "" {
		return "", errors.New("final review records no base the change is measured from")
	}
	g, err := featureWorkspaces(cfg, repository).Of(stream)
	if err != nil {
		return "", err
	}
	diff, err := g.Diff(ctx, report.Upstream.Commit, push.Reviewed)
	if err != nil {
		return "", fmt.Errorf("cannot measure the change: %w", err)
	}
	files := gitdiff.Parse(diff)
	added, removed := 0, 0
	paths := make([]string, 0, len(files))
	for _, f := range files {
		added += f.Added
		removed += f.Removed
		paths = append(paths, f.Path)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d files changed, %d lines added and %d removed.\n", len(files), added, removed)
	entities, err := kb.Load(repository)
	if err != nil || len(entities.Entities) == 0 {
		b.WriteString("The project's entity map names no areas for the change.\n")
		return b.String(), nil
	}
	located := entities.ResolvePaths(paths)
	counts := map[string]int{}
	for _, m := range located.Matches {
		for _, id := range m.Entities {
			counts[id]++
		}
	}
	names := make([]string, 0, len(counts))
	for id := range counts {
		names = append(names, id)
	}
	slices.Sort(names)
	if len(names) > maxRouteAreas {
		names = names[:maxRouteAreas]
	}
	for _, id := range names {
		fmt.Fprintf(&b, "- %s: %d files\n", id, counts[id])
	}
	if n := len(located.Unresolved); n > 0 {
		fmt.Fprintf(&b, "%d files belong to no entity.\n", n)
	}
	return b.String(), nil
}

func (s *Controller) applyDeliveryRoute(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, key string, res jev.Result) *APIError {
	decider := LandDecider{By: trace.DecidedByJev, Judgment: res.Judgment}
	cfg := s.About(repository)
	switch res.Pick {
	case RouteLand, RoutePullRequest:
	default:
		return &APIError{Code: Conflict, Message: fmt.Sprintf("%q is not a delivery route", res.Pick)}
	}
	if !cfg.HasProject() || cfg.Project.ID != repository.Project() || !cfg.Project.Lands() {
		return &APIError{Code: Conflict, Message: "the project does not land, so Jev has no delivery route to choose"}
	}
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return &APIError{Code: Internal, Message: "cannot read feature state"}
	}
	if feature.Value != AssembledState {
		return &APIError{Code: Conflict, Message: fmt.Sprintf("the workstream is %s, not assembled", featureState(feature.Value))}
	}
	push, found, err := CurrentPush(repository, stream)
	if err != nil {
		return &APIError{Code: Internal, Message: "cannot read pushes"}
	}
	if !found || deliveryRouteKey(stream, push) != key {
		if started, err := landingDecidedBy(repository, stream, decider); err != nil {
			return &APIError{Code: Internal, Message: "cannot read landing records"}
		} else if started {
			return nil
		}
		return &APIError{Code: Conflict, Message: "the delivery this decision was made for is no longer the workstream's current delivery"}
	}
	l := &Lander{Repository: repository}
	if _, input, outcome, landing, err := l.latest(stream); err != nil {
		return &APIError{Code: Internal, Message: "cannot read landing records"}
	} else if landing && input.Push == push.Number && input.Decider == decider {
		if outcome != nil && outcome.Outcome == LandOutcomeFailed {
			return &APIError{Code: Conflict, Message: outcome.Evidence}
		}
		if res.Pick == RouteLand {
			return s.applyLand(ctx, repository, stream, decider)
		}
		return nil
	}
	if res.Pick == RouteLand {
		return s.applyLand(ctx, repository, stream, decider)
	}
	return s.applyPullRequest(ctx, repository, cfg, stream, decider)
}

func landingDecidedBy(repository *trace.Repository, stream config.WorkstreamID, decider LandDecider) (bool, error) {
	records, err := repository.Operations(stream)
	if err != nil {
		return false, err
	}
	for _, r := range records {
		if in, err := DecodeLand(r.Operation); err == nil && in.Decider == decider {
			return true, nil
		}
	}
	return false, nil
}

func (s *Controller) applyLand(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, decider LandDecider) *APIError {
	result, err := s.LandWorkstream(ctx, repository, stream, LandRequest{Decider: decider})
	if err != nil {
		return &APIError{Code: Internal, Message: "cannot land the workstream: " + err.Error()}
	}
	switch result.Outcome {
	case LandingRefusedBeforePush:
		return &APIError{Code: Conflict, Message: result.Reason}
	}
	return nil
}

func (s *Controller) applyPullRequest(ctx context.Context, repository *trace.Repository, cfg *config.Config, stream config.WorkstreamID, decider LandDecider) *APIError {
	target, reason, err := s.deliveryRouteReason(ctx, repository, cfg, stream)
	if err != nil {
		return &APIError{Code: Internal, Message: "cannot validate the delivery"}
	}
	if reason != "" {
		return &APIError{Code: Conflict, Message: reason}
	}
	if inFlight, err := LandingInFlight(repository, stream); err != nil {
		return &APIError{Code: Internal, Message: "cannot read landing records"}
	} else if inFlight {
		return &APIError{Code: Conflict, Message: "a landing of the workstream is under way; it delivers the workstream, or says why it did not, before the pull request is marked ready"}
	}
	if _, _, err := s.recordDecidedApproval(ctx, repository, cfg, stream, target.report, decider, "Jev chose to deliver the workstream as its pull request"); err != nil {
		if errors.Is(err, trace.ErrConflict) {
			return &APIError{Code: Conflict, Message: "delivery decision changed; read the current presentation"}
		}
		return &APIError{Code: Internal, Message: "cannot record the approval to mark the pull request ready"}
	}
	return nil
}

func recommendedRoute(repository *trace.Repository, stream config.WorkstreamID, push Push) (string, error) {
	res, err := jev.Recorded(repository, stream, jev.KindDeliveryRoute, deliveryRouteKey(stream, push))
	if err != nil {
		return "", err
	}
	switch {
	case res.Status == jev.Nonbinding && res.Pick != "", res.Status == jev.Binding && res.Refused != "":
		return res.Pick, nil
	}
	return "", nil
}
