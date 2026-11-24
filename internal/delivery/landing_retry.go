package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/scheduler"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	// LandingRoutePath and LandingRouteDocument are the workstream document that
	// records the route a base-moved landing keeps, and its fate.
	LandingRoutePath     = "final/landing-route.json"
	LandingRouteDocument = "final-landing-route"
	// LandingRoutePending and LandingRouteDropped are the statuses of the route:
	// the landing is retried on the next push that is assembled, checked and
	// reviewed, or the decision was dropped and the delivery is the owner's.
	LandingRoutePending = "pending"
	LandingRouteDropped = "dropped"
)

// LandingRoute is the decision to land that a landing whose base moved keeps.
// Landing is the attempt that ended base-moved, Push and Commit the push and
// the signed commit that did not land, and Decider who decided to land.
type LandingRoute struct {
	Landing int         `json:"landing"`
	Push    int         `json:"push"`
	Review  int         `json:"review"`
	Commit  string      `json:"commit"`
	Decider LandDecider `json:"decider"`
	Status  string      `json:"status"`
	Reason  string      `json:"reason,omitempty"`
	At      time.Time   `json:"at"`
}

// ReadLandingRoute returns the latest record of the workstream's landing route,
// and whether one is recorded.
func ReadLandingRoute(repository *trace.Repository, stream config.WorkstreamID) (LandingRoute, bool, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return LandingRoute{}, false, err
	}
	for _, d := range slices.Backward(docs) {
		if d.ID != LandingRouteDocument {
			continue
		}
		var route LandingRoute
		if err := json.Unmarshal([]byte(d.Content), &route); err != nil {
			return LandingRoute{}, false, fmt.Errorf("%s revision %d: %w", LandingRoutePath, d.Revision, err)
		}
		return route, true, nil
	}
	return LandingRoute{}, false, nil
}

// PendingLandingRoute returns the decision to land that the workstream's latest
// landing attempt kept when the base branch moved, and whether one stands: the
// route is pending and no later attempt has been asked for.
func PendingLandingRoute(repository *trace.Repository, stream config.WorkstreamID) (LandingRoute, bool, error) {
	if subject, err := repository.Workflow(stream, LandingSubject); err != nil || !strings.HasPrefix(subject.Value, LandOutcomeBaseMoved+"-") {
		return LandingRoute{}, false, err
	}
	route, found, err := ReadLandingRoute(repository, stream)
	if err != nil || !found || route.Status != LandingRoutePending {
		return LandingRoute{}, false, err
	}
	attempt, _, outcome, landing, err := (&Lander{Repository: repository}).latest(stream)
	if err != nil {
		return LandingRoute{}, false, err
	}
	if !landing || attempt != route.Landing || outcome == nil || outcome.Outcome != LandOutcomeBaseMoved {
		return LandingRoute{}, false, nil
	}
	return route, true, nil
}

func (l *Lander) routeDocument(stream config.WorkstreamID, route LandingRoute, cause string) (trace.Document, error) {
	content, err := jsonMarshal(route)
	if err != nil {
		return trace.Document{}, err
	}
	revision, err := nextRevision(l.Repository, stream, LandingRouteDocument)
	if err != nil {
		return trace.Document{}, err
	}
	return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: LandingRouteDocument, Revision: revision, Project: l.Repository.Project(), Workstream: stream, At: route.At, Actor: foremanActor, Cause: cause},
		Path: LandingRoutePath, Content: content}, nil
}

func (l *Lander) dropRoute(ctx context.Context, stream config.WorkstreamID, route LandingRoute, reason string) error {
	route.Status, route.Reason, route.At = LandingRouteDropped, reason, l.S.Now()
	doc, err := l.routeDocument(stream, route, fmt.Sprintf("%s-route-dropped", mustTransition(route.Landing)))
	if err != nil {
		return err
	}
	if err := l.Repository.RecordDocuments(ctx, []trace.Document{doc}); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

type retryVerdict int

const (
	retryWait retryVerdict = iota
	retryLand
	retryDrop
)

func (s *Controller) retryOf(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, route LandingRoute) (retryVerdict, string, error) {
	cfg := s.About(repository)
	if !cfg.HasProject() || cfg.Project.ID != repository.Project() {
		return retryWait, "", nil
	}
	if !cfg.Project.Lands() {
		return retryDrop, notLandingReason(cfg), nil
	}
	feature, err := repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return retryWait, "", err
	}
	switch feature.Value {
	case AssembledState:
	case DeliveredState, intake.AbandonedState:
		return retryDrop, fmt.Sprintf("the workstream is %s", featureState(feature.Value)), nil
	default:
		return retryWait, "", nil
	}
	push, found, err := CurrentPush(repository, stream)
	if err != nil || !found || push.Number <= route.Push {
		return retryWait, "", err
	}
	report, found, err := LatestFinalReport(repository, stream)
	if err != nil || !found || report.Review != push.Review || report.Commit != push.Reviewed {
		return retryWait, "", err
	}
	if blocking := ReportBlocking(report); len(blocking) > 0 {
		return retryDrop, fmt.Sprintf("final review %d of the rebased workstream has blocking findings left to the owner: %v", report.Review, blocking), nil
	}
	_, latest, err := IncompleteFinalChecks(repository, stream, push.Review, push.Reviewed)
	if err != nil {
		return retryWait, "", err
	}
	switch {
	case latest.Review == 0, latest.Commit != push.Reviewed, latest.Status == ChecksIncomplete:
		return retryWait, "", nil
	case latest.Status != checkrun.ChecksPassed:
		return retryDrop, fmt.Sprintf("checks did not pass on the signed commit %s (the recorded run is %s)", push.Commit, latest.Status), nil
	}
	if pending, err := s.UnresolvedWork(repository, stream); err != nil || pending {
		return retryWait, "", err
	}
	if _, reason, err := s.finalGate(ctx, repository, stream); err != nil || reason != "" {
		return retryWait, "", err
	}
	if _, reason, err := s.landingTarget(ctx, repository, cfg, stream, false, true); err != nil {
		return retryWait, "", err
	} else if reason != "" {
		return retryDrop, reason, nil
	}
	return retryLand, "", nil
}

// RetryPass lands again the workstreams whose landing ended because the base
// branch moved, once each is assembled again on a new push with a current final
// review and passing checks. A workstream whose landing gates no longer hold
// loses its route. The landing is LandWorkstream's, under the decider that chose
// the route, so the router and the landing's own preconditions apply as they do
// to any landing.
func (l *Lander) RetryPass(ctx context.Context) error {
	streams, err := nonSessionWorkstreams(l.Repository)
	if err != nil {
		return err
	}
	state, _ := l.S.Effective()
	for _, stream := range streams {
		if scheduler.Paused(state.Pauses, l.Repository.Project(), stream) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := l.retry(ctx, stream); err != nil {
			return fmt.Errorf("workstream %s landing retry: %w", stream, err)
		}
	}
	return nil
}

func (l *Lander) retry(ctx context.Context, stream config.WorkstreamID) error {
	if held, err := l.S.ChangeHeld(l.Repository, stream); err != nil || held {
		return err
	}
	route, pending, err := PendingLandingRoute(l.Repository, stream)
	if err != nil || !pending {
		return err
	}
	verdict, reason, err := l.S.retryOf(ctx, l.Repository, stream, route)
	if err != nil {
		return err
	}
	switch verdict {
	case retryDrop:
		return l.dropRoute(ctx, stream, route, reason)
	case retryLand:
		result, err := l.S.LandWorkstream(ctx, l.Repository, stream, LandRequest{Decider: route.Decider})
		if err != nil {
			return err
		}
		if result.Outcome == LandingRefusedBeforePush {
			return l.dropRoute(ctx, stream, route, result.Reason)
		}
	}
	return nil
}
