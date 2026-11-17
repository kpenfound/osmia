package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/trace"
)

// workstreamMoveRecords defines, once, what moving a workstream to a feature
// state records, for every way the move is made: the owner's command, the
// chief of staff's tool and the transition router. It checks the move can be
// made from the state it reads and returns the move, its document, its
// transaction on the move subject and the reason; the feature's own
// transition is the router's, or the exit's, to make. A move to building
// reopens an assembled workstream, or a delivered one whose pull request is
// open, on its existing feature branch for repair units; to assembled asks
// for a fresh final review of the branch as it is; and to delivered returns a
// reopened workstream whose branch is still the one its open pull request
// carries.
func (s *Controller) workstreamMoveRecords(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, feature trace.WorkflowState, to, note string, actor trace.Actor, turn string, at time.Time) (workstreamMoveRecord, *APIError) {
	if action, err := PublicationInFlight(repo, stream); err != nil {
		return workstreamMoveRecord{}, &APIError{Code: Internal, Message: "cannot read the workstream's operations"}
	} else if action != "" {
		return workstreamMoveRecord{}, &APIError{Code: Conflict, Message: "the workstream's publication is in flight; it moves once the publication has an outcome"}
	}
	by := "the " + rulerName(actor)
	var reason string
	switch to {
	case BuildingState:
		switch feature.Value {
		case AssembledState:
			reason = fmt.Sprintf("%s moved the workstream from assembled to building on %s", by, featureBranch(stream))
		case DeliveredState:
			pr, api := OpenPullRequest(repo, stream)
			if api != nil {
				return workstreamMoveRecord{}, api
			}
			reason = fmt.Sprintf("%s reopened the delivered workstream on %s, where pull request #%d stays open; it is updated only after a fresh final review and the owner's delivery approval", by, featureBranch(stream), pr.PullRequest)
		case BuildingState:
			return workstreamMoveRecord{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is already building", stream)}
		default:
			return workstreamMoveRecord{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the workstream is %s; it moves to building once it is assembled or delivered with an open pull request", featureState(feature.Value))}
		}
		reason += "; it stays building until a revision filed after this move is decided and its units land, or until it is moved to assembled, and its final review then runs again"
	case AssembledState:
		switch feature.Value {
		case AssembledState:
			reason = fmt.Sprintf("%s asked for a fresh final review of the assembled branch", by)
		case DeliveredState:
			pr, api := OpenPullRequest(repo, stream)
			if api != nil {
				return workstreamMoveRecord{}, api
			}
			reason = fmt.Sprintf("%s returned the delivered workstream to assembled for a fresh final review; pull request #%d stays open and is updated only after the owner's delivery approval", by, pr.PullRequest)
		case BuildingState:
			if api := s.Assemblable(repo, stream); api != nil {
				return workstreamMoveRecord{}, api
			}
			reason = fmt.Sprintf("%s moved the workstream from building to assembled for a fresh final review", by)
		default:
			return workstreamMoveRecord{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the workstream is %s; it moves to assembled once it is building, assembled or delivered with an open pull request", featureState(feature.Value))}
		}
	case DeliveredState:
		if feature.Value != BuildingState && feature.Value != AssembledState {
			return workstreamMoveRecord{}, &APIError{Code: Conflict, Message: fmt.Sprintf("the workstream is %s; only a reopened workstream moves back to delivered", featureState(feature.Value))}
		}
		pr, api := OpenPullRequest(repo, stream)
		if api != nil {
			return workstreamMoveRecord{}, api
		}
		if api := s.Assemblable(repo, stream); api != nil {
			return workstreamMoveRecord{}, api
		}
		tip, api := s.FeatureTip(ctx, repo, stream)
		if api != nil {
			return workstreamMoveRecord{}, api
		}
		if tip != pr.Reviewed && tip != pr.Commit {
			return workstreamMoveRecord{}, &APIError{Code: Conflict, Message: fmt.Sprintf("feature branch %s moved to %s since pull request #%d was published; it is delivered through a fresh final review and the owner's delivery approval", featureBranch(stream), tip, pr.PullRequest)}
		}
		reason = fmt.Sprintf("%s returned the reopened workstream to delivered; feature branch %s is still what pull request #%d carries", by, featureBranch(stream), pr.PullRequest)
	default:
		return workstreamMoveRecord{}, &APIError{Code: Validation, Message: fmt.Sprintf("a workstream moves to building, assembled or delivered through its records; %s is another action", to)}
	}
	reason += ": " + note
	moves, err := WorkstreamMoves(repo, stream)
	if err != nil {
		return workstreamMoveRecord{}, &APIError{Code: Internal, Message: "cannot read the workstream's moves"}
	}
	subject, err := repo.Workflow(stream, WorkstreamMoveSubject)
	if err != nil {
		return workstreamMoveRecord{}, &APIError{Code: Internal, Message: "cannot read the workstream's moves"}
	}
	n := len(moves) + 1
	move := WorkstreamMove{Move: n, From: feature.Value, To: to, Note: note, By: rulerName(actor), Turn: turn}
	data, err := json.MarshalIndent(move, "", "  ")
	if err != nil {
		return workstreamMoveRecord{}, &APIError{Code: Internal, Message: err.Error()}
	}
	cause := turn
	if cause == "" {
		cause = trace.FeatureSubject + "-" + feature.Value
	}
	id := WorkstreamMoveDocument(n)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: stream, At: at, Actor: actor, Cause: cause}
	doc := trace.Document{Header: h, Path: WorkstreamMovePath(n), Content: string(data) + "\n"}
	h.Schema, h.ID, h.Cause = "osmia.trace.transition", id+"-recorded", id
	tx := trace.Transaction{ExpectedVersion: subject.Version, Transition: trace.Transition{Header: h, Subject: WorkstreamMoveSubject, From: subject.Value, To: fmt.Sprintf("moved-%d", n), Reason: reason},
		Events: []trace.Event{trace.Notice(h.ID, "workstream", fmt.Sprintf("Workstream %s: %s.", stream, strings.TrimSuffix(reason, ".")))}}
	return workstreamMoveRecord{move: move, doc: doc, tx: tx, reason: reason, id: id}, nil
}

// workstreamMoveRecord is what entering a feature state by a move records.
type workstreamMoveRecord struct {
	move   WorkstreamMove
	doc    trace.Document
	tx     trace.Transaction
	reason string
	// id is the move's document ID, which the feature transition's ID derives
	// from.
	id string
}

// latestWorkstreamMove returns the workstream's latest recorded move.
func latestWorkstreamMove(repo *trace.Repository, stream config.WorkstreamID) (WorkstreamMove, bool, error) {
	moves, err := WorkstreamMoves(repo, stream)
	if err != nil || len(moves) == 0 {
		return WorkstreamMove{}, false, err
	}
	return moves[len(moves)-1], true, nil
}

// The targets of a workstream move, as Jev reads them.
var workstreamMoveTargets = map[string]router.Target{
	BuildingState:  {Name: BuildingState, Description: "Reopen the workstream on its feature branch for repair units; its final review runs again afterwards.", Bar: router.BarStrict},
	AssembledState: {Name: AssembledState, Description: "Run a fresh final review of the branch as it is.", Bar: router.BarModest},
	DeliveredState: {Name: DeliveredState, Description: "Return the reopened workstream to delivered: its branch is still what its open pull request carries.", Bar: router.BarStrict},
}

// routeWorkstreamMove routes a move to building, assembled or delivered. The
// owner's move is a pinned move: the router enters exactly the target the owner
// named, and asks Jev nothing. A move of the chief of staff's own is a
// proposal: its target is the default, and the router judges it with the other
// legal targets.
func (s *Controller) routeWorkstreamMove(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, feature trace.WorkflowState, to, note string, actor trace.Actor, turn string) (router.Result, error) {
	if s.Router == nil {
		return router.Result{}, errors.New("routing a workstream's moves is unavailable")
	}
	now := s.Now()
	trigger := intake.TriggerMoveProposal
	var authority *router.Authority
	names := []string{AssembledState, BuildingState, DeliveredState}
	if actor.Kind == ownerActor.Kind {
		trigger, authority = intake.TriggerMoveOwner, &router.Authority{Kind: router.AuthorityOwner, Target: to}
		names = []string{to}
	}
	targets := make([]router.Target, 0, len(names))
	for _, name := range names {
		t := workstreamMoveTargets[name]
		// A move to the state the workstream is in asks for a fresh review or
		// restarts the stage: it records the move and changes no state.
		t.NoMove = name == feature.Value
		targets = append(targets, t)
	}
	x := router.Exit{
		Repo: repo, Project: repo.Project(), Workstream: stream, Machine: intake.WorkstreamMachine,
		Subject: trace.FeatureSubject, From: feature.Value, Version: feature.Version,
		Trigger: trigger, Cause: fmt.Sprintf("%s-%d-%d", WorkstreamMovePrefix, feature.Version, now.UnixNano()), Actor: actor, Note: note, Turn: turn, At: now,
		Default: to, Targets: targets, Authority: authority,
		Questions: []router.Question{{Name: "proposal_fits", Instructions: "The proposed move fits the evidence."}, {Name: "evidence_supports", Instructions: "The history and the findings of the final reviews support the move."}},
		TransitionID: func(t router.Target) string {
			return WorkstreamMoveDocument(s.nextMove(repo, stream)) + "-" + t.Name
		},
		Evidence: router.Evidence{Intent: "The workstream's sealed spec and plan are what the owner approved.", Facts: map[string]any{"state": feature.Value, "target": to}},
		Prereq: router.Prerequisites{
			Other: func(ctx context.Context, t router.Target) (string, error) {
				_, api := s.workstreamMoveRecords(ctx, repo, stream, feature, t.Name, note, actor, turn, now)
				switch {
				case api == nil:
					return "", nil
				case api.Code == Internal:
					return "", errors.New(api.Message)
				}
				return api.Message, nil
			},
		},
	}
	moves, err := WorkstreamMoves(repo, stream)
	if err != nil {
		return router.Result{}, err
	}
	for _, m := range moves {
		x.Evidence.Records = append(x.Evidence.Records, router.Entry{Kind: "earlier move", ID: WorkstreamMoveDocument(m.Move), Text: fmt.Sprintf("%s moved the workstream from %s to %s: %s", m.By, m.From, m.To, m.Note)})
	}
	return s.Router.Route(ctx, x)
}

// StalledInput is what the stall watchdog knows of a stalled workstream.
type StalledInput struct {
	// Cause identifies the stall, so that a later stall of the same state is
	// another occurrence.
	Cause string
	Actor trace.Actor
	// Notice is the notice to the chief of staff the default commits.
	Notice string
	// Reason is the transition's reason when the exit is followed.
	Reason   string
	Evidence router.Evidence
	At       time.Time
}

// StalledExit returns the exit of a workstream that has not progressed for the
// stall window with nothing to explain it. The default leaves the workstream
// where it is and tells the chief of staff; an assembled workstream can instead
// be sent to a fresh final review of its branch at the modest bar.
func (s *Controller) StalledExit(repo *trace.Repository, stream config.WorkstreamID, feature trace.WorkflowState, in StalledInput) router.Exit {
	targets := []router.Target{intake.ExitTarget(intake.TriggerStalled, router.Target{Name: intake.TargetWait, Stay: true, NoMove: true})}
	if feature.Value == AssembledState {
		t := workstreamMoveTargets[AssembledState]
		t.NoMove = true
		targets = append(targets, intake.ExitTarget(intake.TriggerStalled, t))
	}
	id := fmt.Sprintf("ws-stalled-%d", feature.Version)
	return router.Exit{
		Repo: repo, Project: repo.Project(), Workstream: stream, Machine: intake.WorkstreamMachine,
		Subject: trace.FeatureSubject, From: feature.Value, Version: feature.Version,
		Trigger: intake.TriggerStalled, Cause: in.Cause, HeaderCause: in.Cause, Actor: in.Actor, At: in.At,
		Default: intake.TargetWait, Targets: targets, Questions: intake.ExitQuestions(intake.TriggerStalled), Evidence: in.Evidence,
		TransitionID: func(t router.Target) string {
			if t.Name == intake.TargetWait {
				return id
			}
			return WorkstreamMoveDocument(s.nextMove(repo, stream)) + "-" + t.Name
		},
		Enter: func(context.Context, router.Plan) (router.Effect, error) {
			return router.Effect{Events: []trace.Event{trace.Notice(id+"-"+in.Cause, "chief", in.Notice)}, Reason: in.Reason}, nil
		},
		Prereq: router.Prerequisites{
			Other: func(ctx context.Context, t router.Target) (string, error) {
				if t.Name == intake.TargetWait {
					return "", nil
				}
				_, api := s.workstreamMoveRecords(ctx, repo, stream, feature, t.Name, in.Reason, in.Actor, "", s.Now())
				switch {
				case api == nil:
					return "", nil
				case api.Code == Internal:
					return "", errors.New(api.Message)
				}
				return api.Message, nil
			},
		},
	}
}

// nextMove returns the number the workstream's next move will take.
func (s *Controller) nextMove(repo *trace.Repository, stream config.WorkstreamID) int {
	moves, err := WorkstreamMoves(repo, stream)
	if err != nil {
		return 1
	}
	return len(moves) + 1
}

// proposeToOwner routes the chief of staff's proposal of a move that is an owner
// gate, ratification or abandonment. A proposal carries no authority for a
// gate, so the router commits nothing and leaves the move as a pending owner
// decision, with a notice that names where the owner decides it.
func (s *Controller) proposeToOwner(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, to, note string, actor trace.Actor, turn string) (router.Result, error) {
	if s.Router == nil {
		return router.Result{}, errors.New("routing a workstream's moves is unavailable")
	}
	feature, err := repo.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return router.Result{}, err
	}
	card := "the recovery card"
	desc := "Abandon the workstream."
	if to == RatifiedState {
		card, desc = "the ratification card", "Ratify the latest packet."
	}
	now := s.Now()
	return s.Router.Route(ctx, router.Exit{
		Repo: repo, Project: repo.Project(), Workstream: stream, Machine: intake.WorkstreamMachine,
		Subject: trace.FeatureSubject, From: feature.Value, Version: feature.Version,
		Trigger: intake.TriggerMoveProposal, Cause: fmt.Sprintf("%s-%s-%d-%d", WorkstreamMovePrefix, to, feature.Version, now.UnixNano()), Actor: actor, Note: note, Turn: turn, At: now,
		Default: to, Targets: []router.Target{{Name: to, Description: desc, Gate: true}}, OwnerCard: card,
	})
}

// RegisterWorkstreamEffects defines, in the router's registry, the effect of
// entering each feature state a move reaches, and of the departures of a final
// review's round: the move with its note, its document and its notice. They are
// what the owner's command and the chief of staff's tool record, and every
// other way a workstream is moved to a state its trigger does not enter as a
// default. load returns the lifecycle controller.
func RegisterWorkstreamEffects(reg *router.Effects, load func() *Controller) {
	effect := func(to string) router.EffectFunc {
		return func(ctx context.Context, p router.Plan) (router.Effect, error) {
			s := load()
			if s == nil {
				return router.Effect{}, errors.New("workstream moves are unavailable")
			}
			x := p.Exit
			feature, err := x.Repo.Workflow(x.Workstream, trace.FeatureSubject)
			if err != nil {
				return router.Effect{}, err
			}
			actor := x.Actor
			if p.Routing.DecidedBy == trace.DecidedByJev {
				actor = trace.JevActor
			}
			note := x.Note
			if note == "" {
				note = p.Reason
			}
			records, api := s.workstreamMoveRecords(ctx, x.Repo, x.Workstream, feature, to, note, actor, x.Turn, p.At)
			switch {
			case api == nil:
			case api.Code == Conflict || api.Code == Validation:
				return router.Effect{}, router.Refusal(api.Message)
			default:
				return router.Effect{}, errors.New(api.Message)
			}
			out := router.Effect{Documents: []trace.Document{records.doc}, With: []trace.Transaction{records.tx}, Reason: records.reason}
			if x.Subject != trace.FeatureSubject && feature.Value != to {
				// The exit belongs to another subject, as a final review's round
				// does: the workstream's own transition is recorded beside it.
				h := records.tx.Transition.Header
				h.ID = records.id + "-" + to
				out.With = append(out.With, trace.Transaction{ExpectedVersion: feature.Version, Transition: trace.Transition{Header: h, Subject: trace.FeatureSubject, From: feature.Value, To: to, Reason: records.reason}})
			}
			return out, nil
		}
	}
	for _, to := range []string{BuildingState, AssembledState, DeliveredState} {
		reg.Register(intake.WorkstreamMachine, to, effect(to))
	}
	reg.Register(intake.FinalMachine, intake.TargetReopenBuilding, effect(BuildingState))
	reg.Register(intake.FinalMachine, intake.TargetFreshReview, effect(AssembledState))
}
