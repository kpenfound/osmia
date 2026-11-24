package delivery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

// The machine, triggers and targets of a final review's exits, as the intake
// package defines them for every exit of a workstream.
const (
	FinalMachine         = intake.FinalMachine
	TriggerFinalClean    = intake.TriggerFinalClean
	TriggerFinalBlocking = intake.TriggerFinalBlocking
	TriggerFinalFailed   = intake.TriggerFinalFailed
	TargetReviewed       = intake.TargetReviewed
	TargetRequested      = intake.TargetRequested
	TargetFailed         = intake.TargetFailed
	TargetFreshReview    = intake.TargetFreshReview
	TargetReopenBuilding = intake.TargetReopenBuilding
)

var (
	ExitTarget    = intake.ExitTarget
	ExitQuestions = intake.ExitQuestions
)

type finalExitInput struct {
	Stream    config.WorkstreamID
	State     trace.WorkflowState
	Report    FinalReport
	ID        string
	Reason    string
	Body      string
	Blocking  []string
	Rounds    int
	Documents []trace.Document
	Followups []trace.Transaction
	Plain     trace.Transaction
	At        time.Time
}

func finalTrigger(r FinalReport, blocking int) string {
	switch {
	case r.Outcome != FinalReviewed:
		return TriggerFinalFailed
	case blocking == 0:
		return TriggerFinalClean
	}
	return TriggerFinalBlocking
}

func (a *FinalReviewer) finalExit(in finalExitInput) router.Exit {
	trigger := finalTrigger(in.Report, len(in.Blocking))
	k := in.Report.Review
	def := TargetReviewed
	if in.Report.Outcome != FinalReviewed {
		def = TargetFailed
	}
	targets := []router.Target{ExitTarget(trigger, router.Target{Name: def, State: fmt.Sprintf("%s-%d", in.Report.Outcome, k), Description: "Record the final review."})}
	if trigger != TriggerFinalClean && !in.Report.Maintenance {
		targets = append(targets,
			ExitTarget(trigger, router.Target{Name: TargetFreshReview, State: fmt.Sprintf("rerequested-%d", k), Description: "Ask for a fresh final review.", Bar: router.BarModest, Works: true}),
			ExitTarget(trigger, router.Target{Name: TargetReopenBuilding, State: fmt.Sprintf("reopened-%d", k), Description: "Reopen the workstream for repair units.", Bar: router.BarStrict, Works: true}))
	}
	repo := a.Repository
	stream := in.Stream
	x := router.Exit{
		Repo: repo, Project: repo.Project(), Workstream: stream, Machine: FinalMachine,
		Subject: FinalReviewSubject, From: in.State.Value, Version: in.State.Version,
		Trigger: trigger, Cause: fmt.Sprintf("%s-%d", FinalReviewSubject, k), HeaderCause: in.Report.Operation, Actor: FinalReviewActor, At: in.At,
		Default: def, Targets: targets, Questions: ExitQuestions(trigger),
		TransitionID: func(router.Target) string { return in.ID },
		Enter: func(context.Context, router.Plan) (router.Effect, error) {
			return router.Effect{Reason: in.Reason}, nil
		},
		Records: func(context.Context, router.Plan) (router.Effect, error) {
			return router.Effect{Documents: in.Documents, Events: []trace.Event{trace.Notice(in.ID, "chief", in.Body)}, With: in.Followups}, nil
		},
		Note: finalMoveNote(in.Report),
		Evidence: router.Evidence{
			Intent: "The assembled branch is read against the sealed spec and plan before the owner decides delivery.",
			Facts: map[string]any{
				"review": k, "blocking": len(in.Blocking), "advisory": len(in.Report.Findings) - CountSeverity(in.Report.Findings, FindingBlocking),
				"rounds_with_blocking": in.Rounds, "max_final_rounds": maxFinalRounds, "maintenance": in.Report.Maintenance,
			},
		},
		Prereq: router.Prerequisites{
			Other: func(ctx context.Context, t router.Target) (string, error) {
				if t.Name != TargetReopenBuilding && t.Name != TargetFreshReview {
					return "", nil
				}
				return a.workstreamMovable(stream)
			},
		},
	}
	for i, item := range in.Blocking {
		x.Evidence.Records = append(x.Evidence.Records, router.Entry{Kind: "blocking finding", ID: fmt.Sprintf("%d:%d", k, i+1), Text: item})
	}
	if in.Report.Failure != "" {
		x.Evidence.Records = append(x.Evidence.Records, router.Entry{Kind: "failure", ID: in.ID, Text: in.Report.Failure})
	}
	return x
}

func finalMoveNote(r FinalReport) string {
	if r.Outcome != FinalReviewed {
		return fmt.Sprintf("final review %d failed", r.Review)
	}
	return fmt.Sprintf("final review %d found blocking problems", r.Review)
}

func (a *FinalReviewer) workstreamMovable(stream config.WorkstreamID) (string, error) {
	feature, err := a.Repository.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return "", err
	}
	if feature.Value != AssembledState {
		return fmt.Sprintf("the workstream is %s; a final review moves it while it is assembled", featureState(feature.Value)), nil
	}
	if held, err := a.S.ChangeHeld(a.Repository, stream); err != nil || held {
		return "a workgroup change holds the workstream", err
	}
	records, err := a.Repository.Operations(stream)
	if err != nil {
		return "", err
	}
	for _, r := range records {
		if r.Result == nil && (r.Operation.Action == PublishAction || r.Operation.Action == publishUpstreamAction) {
			return "the workstream's publication is in flight", nil
		}
		if r.Result == nil && r.Operation.Action == LandDeliveryAction {
			return "the workstream's landing is in flight", nil
		}
	}
	return "", nil
}

func (a *FinalReviewer) routeFinal(ctx context.Context, in finalExitInput, outcome coreadapter.OperationResult) (coreadapter.OperationResult, error) {
	if a.S.Router == nil {
		return coreadapter.OperationResult{}, errors.New("routing the rounds of the final review is unavailable")
	}
	res, err := a.S.Router.Route(ctx, a.finalExit(in))
	switch {
	case err != nil:
		return coreadapter.OperationResult{}, err
	case res.Committed:
		return outcome, nil
	}
	if res.Hold != router.HoldStale {
		txs := append([]trace.Transaction{in.Plain}, in.Followups...)
		if _, err := a.Repository.RecordDocumentsWith(ctx, in.Documents, txs...); err == nil {
			return outcome, nil
		} else if !errors.Is(err, trace.ErrConflict) {
			return coreadapter.OperationResult{}, err
		}
	}
	if recorded, outcomeErr := a.Outcome(in.Stream, in.Report.Review); outcomeErr == nil && recorded != nil {
		return *recorded, nil
	}
	return coreadapter.OperationResult{}, fmt.Errorf("final review %d could not be recorded: %s", in.Report.Review, res.Reason)
}

// The trigger and target of a workstream's delivery.
const (
	TriggerDeliveryApproved = intake.TriggerDeliveryApproved
	TriggerReadyOnHost      = intake.TriggerReadyOnHost
	TriggerMergeRecorded    = intake.TriggerMergeRecorded
	TriggerLanded           = intake.TriggerLanded
	TargetDelivered         = intake.TargetDelivered
)

func routeDelivered(ctx context.Context, rt router.Port, repo *trace.Repository, stream config.WorkstreamID, trigger, judgment string, docs []trace.Document, feature trace.Transaction, with ...trace.Transaction) (router.Result, error) {
	by := LandDecider{}
	if judgment != "" {
		by = LandDecider{By: trace.DecidedByJev, Judgment: judgment}
	}
	return routeDeliveredBy(ctx, rt, repo, stream, trigger, by, docs, feature, with...)
}

func routeDeliveredBy(ctx context.Context, rt router.Port, repo *trace.Repository, stream config.WorkstreamID, trigger string, by LandDecider, docs []trace.Document, feature trace.Transaction, with ...trace.Transaction) (router.Result, error) {
	if rt == nil {
		return router.Result{}, errors.New("routing the delivery of a workstream is unavailable")
	}
	return rt.Route(ctx, deliveredMove(repo, stream, trigger, by, docs, feature, with...).Exit())
}

func deliveredMove(repo *trace.Repository, stream config.WorkstreamID, trigger string, by LandDecider, docs []trace.Document, feature trace.Transaction, with ...trace.Transaction) intake.PinnedMove {
	t := feature.Transition
	m := intake.PinnedMove{
		Repo: repo, Stream: stream, Subject: trace.FeatureSubject, State: trace.WorkflowState{Version: feature.ExpectedVersion, Value: t.From},
		Trigger: trigger, Cause: t.ID, Target: TargetDelivered, Description: "Record the workstream delivered.", Gate: true,
		Ref: t.Cause, ID: t.ID, HeaderCause: t.Cause, Reason: t.Reason, Actor: t.Actor, At: t.At,
		Events: feature.Events, Documents: docs, With: with,
	}
	if by.By == trace.DecidedByJev {
		m.Jev = by.Judgment
	}
	return m
}
