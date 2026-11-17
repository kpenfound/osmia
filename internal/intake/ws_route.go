package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

// The machines of a workstream's exits in the router's effects: the
// workstream's own states and the records of its shed, draft and seal, and the
// rounds of its final review.
const (
	WorkstreamMachine = "ws"
	FinalMachine      = "final"
)

// The triggers of a workstream's exits, as the route span lists them.
const (
	TriggerDraftExhausted    = "ws.draft_exhausted"
	TriggerShedConcluded     = "ws.shed_concluded"
	TriggerSealFailed        = "ws.seal_failed"
	TriggerRatified          = "ws.ratified"
	TriggerAbandon           = "ws.abandon"
	TriggerDeliveryApproved  = "ws.delivery_approved"
	TriggerReadyOnHost       = "ws.ready_on_host"
	TriggerMergeRecorded     = "ws.merge_recorded"
	TriggerMoveOwner         = "ws.move_owner"
	TriggerMoveProposal      = "ws.move_proposal"
	TriggerUnitsMerged       = "ws.units_merged"
	TriggerFinalRequested    = "final.requested"
	TargetRequested          = "requested"
	TriggerFinalClean        = "final.reviewed_clean"
	TriggerFinalBlocking     = "final.reviewed_blocking"
	TriggerFinalFailed       = "final.failed"
	TargetHold               = "hold"
	TargetWait               = "wait"
	TargetDebateRound        = "debate_round"
	TargetRedraft            = "redraft"
	TargetNewDraft           = "new_draft"
	TargetFailed             = "failed"
	TargetExhausted          = "exhausted"
	TargetConcluded          = "concluded"
	TargetRatified           = "ratified"
	TargetAbandoned          = "abandoned"
	TargetDelivered          = "delivered"
	TargetReopenBuilding     = "reopen_building"
	TargetFreshReview        = "fresh_review"
	TargetReviewed           = "reviewed"
	TargetBuilding           = "building"
	TargetAssembled          = "assembled"
	ownerCardRatification    = "the ratification card"
	ownerCardRecovery        = "the recovery card"
	routeRatifiedDescription = "Record the owner's ratification of the spec and the plan so the sealing is asked for."
)

// PinnedMove is an authorised owner or gate decision that the router applies:
// the exact target it names, with the records the decision has always written.
// The router checks the hard prerequisites once more, commits the target with
// its records, and records the owner or Jev's gate decision as the authority; it
// asks Jev nothing.
type PinnedMove struct {
	Repo   *trace.Repository
	Stream config.WorkstreamID
	// Subject is the workflow subject the decision moves and State the value
	// and version the caller read it at.
	Subject string
	State   trace.WorkflowState
	Trigger string
	// Cause identifies the decision, so that two decisions that leave the state
	// unchanged are different occurrences.
	Cause string
	// Target is the fixed word of the target and Enters the state of the subject
	// it enters when that is another value.
	Target, Enters string
	Description    string
	// Gate says the target is an owner gate: it opens only on this authority.
	Gate bool
	// Jev names the judgment of one of Jev's gate decisions; empty is the owner's
	// decision.
	Jev string
	// Ref names the record the decision rests on.
	Ref string
	// ID is the transition's ID, HeaderCause the record it cites and Reason its
	// reason; Actor attributes it.
	ID, HeaderCause, Reason string
	Actor                   trace.Actor
	Machine                 string
	Events                  []trace.Event
	Documents               []trace.Document
	With                    []trace.Transaction
	// Other holds the machine's own hard prerequisites for the target.
	Other func(ctx context.Context) (string, error)
	At    time.Time
}

// Exit returns the pinned move as an exit of the router.
func (m PinnedMove) Exit() router.Exit {
	machine := m.Machine
	if machine == "" {
		machine = WorkstreamMachine
	}
	authority := &router.Authority{Kind: router.AuthorityOwner, Target: m.Target, Ref: m.Ref}
	if m.Jev != "" {
		authority = &router.Authority{Kind: router.AuthorityJevGate, Target: m.Target, Judgment: m.Jev, Ref: m.Ref}
	}
	x := router.Exit{
		Repo: m.Repo, Project: m.Repo.Project(), Workstream: m.Stream, Machine: machine,
		Subject: m.Subject, From: m.State.Value, Version: m.State.Version,
		Trigger: m.Trigger, Cause: m.Cause, HeaderCause: m.HeaderCause, Actor: m.Actor, At: m.At,
		Default: m.Target, Authority: authority,
		Targets: []router.Target{{Name: m.Target, State: m.Enters, Description: m.Description, Gate: m.Gate, Reason: m.Reason}},
		TransitionID: func(router.Target) string {
			return m.ID
		},
		Enter: func(context.Context, router.Plan) (router.Effect, error) {
			return router.Effect{Events: m.Events, Documents: m.Documents, With: m.With, Reason: m.Reason, Cause: m.HeaderCause}, nil
		},
	}
	if m.Other != nil {
		other := m.Other
		x.Prereq.Other = func(ctx context.Context, _ router.Target) (string, error) { return other(ctx) }
	}
	return x
}

// RouteError turns the result of routing a decision that was to be applied as
// asked into the error its caller answers with: nil when the target was
// committed. A subject that moved since the decision was read is a conflict,
// like a transition that conflicts; a target the router held or refused is a
// conflict too, with the reason.
func RouteError(res router.Result, err error, what string) *APIError {
	switch {
	case err != nil:
		return &APIError{Code: Internal, Message: fmt.Sprintf("cannot record %s: %v", what, err)}
	case res.Committed:
		return nil
	case res.Hold == router.HoldStale:
		return &APIError{Code: Conflict, Message: fmt.Sprintf("%s changed while it was recorded; check osmia status and retry", what)}
	}
	return &APIError{Code: Conflict, Message: fmt.Sprintf("%s was not recorded: the router held it (%s). %s", what, res.Hold, res.Reason)}
}

// raise routes an exit a pass raises: on the scheduling path it is handed to the
// router's worker, which asks Jev beside the pass, and anywhere else it is
// routed in place. The pass that raised an exit raises it again while the
// subject is still where the exit left it, and the router finishes whatever an
// earlier run recorded.
func raise(ctx context.Context, rt router.Port, x router.Exit) error {
	if rt == nil {
		return errors.New("routing a workstream's exits is unavailable")
	}
	if router.IsSchedulingPath(ctx) {
		rt.Submit(x, nil)
		return nil
	}
	_, err := rt.Route(ctx, x)
	return err
}

// Raise routes an exit of a pass of another controller of the workstream's
// machines, as raise does.
func Raise(ctx context.Context, rt router.Port, x router.Exit) error { return raise(ctx, rt, x) }

// route applies a pinned move through the controller's router.
func (s *Controller) route(ctx context.Context, m PinnedMove) (router.Result, error) {
	if s.Router == nil {
		return router.Result{}, errors.New("routing a workstream's gate decisions is unavailable")
	}
	return s.Router.Route(ctx, m.Exit())
}

// WorkstreamTargets returns the exit's targets in the order given: a target
// with a bar departs from the default only at it.
type wsTrigger struct {
	questions   []router.Question
	corroborate map[string][]router.Condition
	describe    map[string]string
}

func ask(name, instructions string) router.Question {
	return router.Question{Name: name, Instructions: instructions}
}

// triggerSpecs defines, per trigger of a workstream's exits that Jev judges,
// what the router asks Jev beyond the questions every exit has. Each question
// is answered as a probability of a proposition about the records in the
// summary.
var triggerSpecs = map[string]wsTrigger{
	TriggerDraftExhausted: {
		describe: map[string]string{
			TargetExhausted: "Stop drafting: none of the architect's drafts was accepted, so the workstream stays handed in and the chief of staff is told.",
			TargetNewDraft:  "Ask the architect for one more draft of the spec and plan.",
		},
		questions:   []router.Question{ask("draft_fixable", "The failures of the last drafts point to a gap one more draft can close."), ask("failures_repeat", "The same failure repeats across the drafts shown.")},
		corroborate: map[string][]router.Condition{TargetNewDraft: {{Question: "draft_fixable", AtLeast: router.LevelStrict}, {Question: "failures_repeat", AtMost: router.LevelHalf}}},
	},
	TriggerShedConcluded: {
		describe: map[string]string{
			TargetWait:        "Leave the decision to the owner: the debate concluded and the owner ratifies, asks for more rounds or asks for a redraft.",
			TargetDebateRound: "Run one more round of debate on the standing dissent.",
			TargetRedraft:     "Ask the architect for a redraft that answers the standing dissent.",
		},
		questions: []router.Question{ask("dissent_substantive", "Open dissent on the record is unresolved and substantive."), ask("converging", "Successive rounds are converging on the same objections being answered.")},
		corroborate: map[string][]router.Condition{
			TargetDebateRound: {{Question: "dissent_substantive", AtLeast: router.LevelStrict}, {Question: "converging", AtLeast: router.LevelHalf}},
			TargetRedraft:     {{Question: "dissent_substantive", AtLeast: router.LevelStrict}, {Question: "converging", AtMost: router.LevelHalf}},
		},
	},
	TriggerSealFailed: {
		describe:  map[string]string{TargetFailed: "Record the failed sealing and tell the chief of staff; the owner ratifies again once it is put right."},
		questions: []router.Question{ask("transient", "The sealing failure looks like a one-off of infrastructure.")},
	},
	TriggerFinalClean: {
		describe: map[string]string{TargetReviewed: "Record the final review with nothing blocking so the owner decides delivery."},
	},
	TriggerFinalBlocking: {
		describe: map[string]string{
			TargetReviewed:       "Record the final review and leave the blocking findings to be triaged.",
			TargetReopenBuilding: "Reopen the workstream for repair units: the blocking findings need code.",
			TargetFreshReview:    "Ask for a fresh final review of the branch: the findings look mistaken or out of date.",
		},
		questions: []router.Question{ask("need_code", "The blocking findings need code to resolve."), ask("mistaken", "The blocking findings contradict the sealed spec or are out of scope."), ask("converging", "Successive final reviews found fewer or new problems and not the same ones again.")},
		corroborate: map[string][]router.Condition{
			TargetReopenBuilding: {{Question: "need_code", AtLeast: router.LevelStrict}},
			TargetFreshReview:    {{Question: "mistaken", AtLeast: router.LevelModest}},
		},
	},
	TriggerFinalFailed: {
		describe: map[string]string{
			TargetFailed:         "Record the failed final review and tell the chief of staff; a new review runs once the branch or its governing documents change.",
			TargetFreshReview:    "Ask for a fresh final review of the branch: the failure looks transient.",
			TargetReopenBuilding: "Reopen the workstream for repair units: the failure comes from the branch.",
		},
		questions: []router.Question{ask("transient", "The failure looks like a one-off of infrastructure."), ask("branch_caused", "The failure comes from the branch and not from the review.")},
		corroborate: map[string][]router.Condition{
			TargetFreshReview:    {{Question: "transient", AtLeast: router.LevelModest}},
			TargetReopenBuilding: {{Question: "branch_caused", AtLeast: router.LevelStrict}},
		},
	},
}

// ExitTarget returns the target name with its description, bar and
// corroboration as the trigger offers it.
func ExitTarget(trigger string, t router.Target) router.Target {
	spec := triggerSpecs[trigger]
	if d, ok := spec.describe[t.Name]; ok {
		t.Description = d
	}
	t.Corroborate = spec.corroborate[t.Name]
	return t
}

// ExitQuestions returns the questions of the trigger.
func ExitQuestions(trigger string) []router.Question { return triggerSpecs[trigger].questions }

// ---------------------------------------------------------------------------
// Draft exhausted

// draftExhaustedExit is the exit of a handed-in workstream none of whose drafts
// was accepted. Today's default stops drafting with a notice to the chief of
// staff; one more draft is a departure at the strict bar.
func (d *Drafter) exhaustedExit(stream config.WorkstreamID, state trace.WorkflowState, n int, last trace.Transition, reason, body string) router.Exit {
	targets := []router.Target{
		ExitTarget(TriggerDraftExhausted, router.Target{Name: TargetExhausted, State: "exhausted", Description: "Stop drafting."}),
		ExitTarget(TriggerDraftExhausted, router.Target{Name: TargetNewDraft, State: fmt.Sprintf("drafting-%d", n+1), Description: "Ask for one more draft.", Bar: router.BarStrict}),
	}
	return router.Exit{
		Repo: d.Repository, Project: d.Repository.Project(), Workstream: stream, Machine: WorkstreamMachine,
		Subject: DraftSubject, From: state.Value, Version: state.Version,
		Trigger: TriggerDraftExhausted, Cause: last.ID, HeaderCause: last.ID, Actor: DraftingActor,
		Default: TargetExhausted, Targets: targets, Questions: ExitQuestions(TriggerDraftExhausted),
		TransitionID: func(t router.Target) string {
			if t.Name == TargetExhausted {
				return "draft-exhausted"
			}
			return fmt.Sprintf("draft-%d-%s", n, TargetNewDraft)
		},
		Enter: func(_ context.Context, p router.Plan) (router.Effect, error) {
			return router.Effect{Events: []trace.Event{trace.Notice("draft-exhausted", "chief", body)}, Reason: reason}, nil
		},
		Evidence: router.Evidence{
			Intent:  "The workstream was handed in and the architect drafts its spec and plan.",
			Facts:   map[string]any{"drafts": n, "max_drafts": MaxDrafts},
			Records: []router.Entry{{Kind: "last draft", ID: last.ID, Text: last.Reason}},
		},
	}
}

// ---------------------------------------------------------------------------
// Shed concluded

// concludedExit is the exit of a workstream whose debate concluded after round
// n, raised on the owner subject the owner's next action moves. Today's default
// leaves the decision to the owner; another round or a redraft is a departure
// at the strict bar.
func (d *Debate) concludedExit(stream config.WorkstreamID, owner trace.WorkflowState, n int, open []shed.Entry, canRound, canRedraft bool) router.Exit {
	targets := []router.Target{ExitTarget(TriggerShedConcluded, router.Target{Name: TargetWait, Description: "Wait for the owner.", Stay: true, NoMove: true})}
	if canRound {
		targets = append(targets, ExitTarget(TriggerShedConcluded, router.Target{Name: TargetDebateRound, State: fmt.Sprintf("more-%d", n), Description: "Run another round.", Bar: router.BarStrict}))
	}
	if canRedraft {
		targets = append(targets, ExitTarget(TriggerShedConcluded, router.Target{Name: TargetRedraft, State: fmt.Sprintf("redraft-%d", n), Description: "Ask for a redraft.", Bar: router.BarStrict}))
	}
	x := router.Exit{
		Repo: d.Repository, Project: d.Repository.Project(), Workstream: stream, Machine: WorkstreamMachine,
		Subject: OwnerSubject, From: owner.Value, Version: owner.Version,
		Trigger: TriggerShedConcluded, Cause: fmt.Sprintf("shed-concluded-%d", n), HeaderCause: fmt.Sprintf("shed-concluded-%d", n), Actor: ShedActor,
		Default: TargetWait, Targets: targets, Questions: ExitQuestions(TriggerShedConcluded),
		TransitionID: func(t router.Target) string { return fmt.Sprintf("%s-%d-jev", t.Name, n) },
		Evidence: router.Evidence{
			Intent: "The committee debated the workstream's spec and plan and the debate concluded; the owner ratifies, asks for more rounds or asks for a redraft.",
			Facts:  map[string]any{"round": n, "standing_objections": len(shed.Standing(open)), "blocking_objections": len(shed.Blocked(open))},
		},
	}
	for _, e := range open {
		x.Evidence.Records = append(x.Evidence.Records, router.Entry{Kind: "objection", ID: e.ID, Text: e.Argument})
	}
	return x
}

// shedRoundOf returns the round a concluded exit was raised for.
func shedRoundOf(x router.Exit) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(x.Cause, "shed-concluded-"))
	if err != nil {
		return 0, fmt.Errorf("the exit %q does not name a round", x.Cause)
	}
	return n, nil
}

// recordsOfOwnerAction returns what an owner action of the shed records for a
// target: the file the debate reads and the notice naming it. It is the one
// definition of the records, whoever acts: the owner's command and the router.
func ownerShedDocument(repo *trace.Repository, project config.ProjectID, stream config.WorkstreamID, id, path, content string, at time.Time, actor trace.Actor) (trace.Document, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return trace.Document{}, err
	}
	revision := 1
	for _, d := range docs {
		if d.Path == path && d.Revision >= revision {
			revision = d.Revision + 1
		}
	}
	return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: project, Workstream: stream, At: at, Actor: actor, Cause: "owner-shed"}, Path: path, Content: content}, nil
}

// RegisterWorkstreamEffects defines, in the router's registry, the effect of
// entering each target of the exits intake raises: another debate round or a
// redraft, which record what the owner's command records, and another draft,
// which queues the architect. load returns the intake controller.
func RegisterWorkstreamEffects(reg *router.Effects, load func() *Controller) {
	reg.Register(WorkstreamMachine, TargetDebateRound, func(ctx context.Context, p router.Plan) (router.Effect, error) {
		x := p.Exit
		n, err := shedRoundOf(x)
		if err != nil {
			return router.Effect{}, err
		}
		content, err := shed.EncodeMore(shed.More{Version: shed.Version, Round: n, Rounds: 1})
		if err != nil {
			return router.Effect{}, router.Refusal(err.Error())
		}
		reason := fmt.Sprintf("Jev asked for %s of debate after round %d: the standing dissent is unresolved", Rounds(1), n)
		doc, err := ownerShedDocument(x.Repo, x.Project, x.Workstream, shed.MoreDocumentID(n), shed.MorePath(n), string(content), p.At, trace.JevActor)
		if err != nil {
			return router.Effect{}, err
		}
		return router.Effect{Documents: []trace.Document{doc}, Events: []trace.Event{trace.Notice(p.TransitionID, "owner", reason)}, Reason: reason}, nil
	})
	reg.Register(WorkstreamMachine, TargetRedraft, func(ctx context.Context, p router.Plan) (router.Effect, error) {
		x := p.Exit
		n, err := shedRoundOf(x)
		if err != nil {
			return router.Effect{}, err
		}
		pin, err := LatestPin(x.Repo, x.Workstream)
		if err != nil {
			return router.Effect{}, err
		}
		note := fmt.Sprintf("Answer the objections that stand after round %d.", n)
		content, err := shed.EncodeRedraft(shed.Redraft{Version: shed.Version, Round: n, Revision: pin, Note: note})
		if err != nil {
			return router.Effect{}, router.Refusal(err.Error())
		}
		reason := fmt.Sprintf("Jev asked the architect for a redraft of %s after round %d: %s", pin, n, note)
		doc, err := ownerShedDocument(x.Repo, x.Project, x.Workstream, shed.RedraftDocumentID(n), shed.RedraftPath(n), string(content), p.At, trace.JevActor)
		if err != nil {
			return router.Effect{}, err
		}
		return router.Effect{Documents: []trace.Document{doc}, Events: []trace.Event{trace.Notice(p.TransitionID, "owner", reason)}, Reason: reason}, nil
	})
	reg.Register(WorkstreamMachine, TargetNewDraft, func(ctx context.Context, p router.Plan) (router.Effect, error) {
		x := p.Exit
		kind, n, ok := DraftState(x.From)
		if !ok {
			return router.Effect{}, router.Refusal(fmt.Sprintf("the draft state %q has no draft number", x.From))
		}
		in := DraftInput{Draft: n + 1}
		_, event := in.RunIDs()
		input, err := marshalDraft(in)
		if err != nil {
			return router.Effect{}, err
		}
		op := draftOperation(x.Repo.Project(), x.Workstream, event, input)
		reason := fmt.Sprintf("draft %d was %s and Jev asked the architect for draft %d", n, kind, n+1)
		return router.Effect{Events: []trace.Event{{ID: event, Kind: "draft", Body: fmt.Sprintf("Draft %d of the spec and plan", in.Draft), Operation: &op}}, Reason: reason}, nil
	})
}

// failedExit is the exit of a sealing that failed: its subject moves to
// failed-<k> while it still reads sealing-<k>, and the chief of staff is told.
// It has one legal target, so the router keeps it without asking Jev.
func (z *Sealer) failedExit(stream config.WorkstreamID, state trace.WorkflowState, to, id, cause, reason, body string) router.Exit {
	return router.Exit{
		Repo: z.Repository, Project: z.Repository.Project(), Workstream: stream, Machine: WorkstreamMachine,
		Subject: SealSubject, From: state.Value, Version: state.Version,
		Trigger: TriggerSealFailed, Cause: id, HeaderCause: cause, Actor: SealingActor,
		Default: TargetFailed, Targets: []router.Target{ExitTarget(TriggerSealFailed, router.Target{Name: TargetFailed, State: to, Description: "Record the failed sealing."})},
		Questions: ExitQuestions(TriggerSealFailed), TransitionID: func(router.Target) string { return id },
		Enter: func(context.Context, router.Plan) (router.Effect, error) {
			return router.Effect{Events: []trace.Event{trace.Notice(id, "failed", body)}, Reason: reason}, nil
		},
		Evidence: router.Evidence{Intent: "The owner ratified the spec and the plan and the sealing was asked for.", Records: []router.Entry{{Kind: "sealing failure", ID: id, Text: reason}}},
	}
}

// marshalDraft encodes the input of a draft operation.
func marshalDraft(in DraftInput) ([]byte, error) { return json.Marshal(in) }

// draftOperation is the runner operation that runs the draft the input numbers.
func draftOperation(project config.ProjectID, stream config.WorkstreamID, event string, input []byte) coreadapter.Operation {
	return coreadapter.Operation{ID: trace.OperationID(project, stream, event), Boundary: coreadapter.RunnerBoundary, Action: DraftAction, Input: input}
}

// DefaultMove is a move of a workstream's subject that its trigger always enters:
// the one target of the exit, which the router keeps without asking Jev, with
// the records and notices the move has always had. It is how the machine's
// mechanical exits, a building workstream assembling or a final review being
// requested, are routed, so that their transitions record the default they took.
type DefaultMove struct {
	Repo    *trace.Repository
	Stream  config.WorkstreamID
	Subject string
	State   trace.WorkflowState
	// Machine is the exit's machine; empty is the workstream's.
	Machine string
	Trigger string
	// Cause identifies the exit; ID is the transition's ID, HeaderCause the
	// record it cites and Reason its reason.
	Cause, ID, HeaderCause, Reason string
	// Target is the fixed word of the target and Enters the state it enters.
	Target, Enters, Description string
	Actor                       trace.Actor
	Events                      []trace.Event
	Documents                   []trace.Document
}

// Exit returns the move as an exit of the router.
func (m DefaultMove) Exit() router.Exit {
	machine := m.Machine
	if machine == "" {
		machine = WorkstreamMachine
	}
	return router.Exit{
		Repo: m.Repo, Project: m.Repo.Project(), Workstream: m.Stream, Machine: machine,
		Subject: m.Subject, From: m.State.Value, Version: m.State.Version,
		Trigger: m.Trigger, Cause: m.Cause, HeaderCause: m.HeaderCause, Actor: m.Actor,
		Default: m.Target, Targets: []router.Target{{Name: m.Target, State: m.Enters, Description: m.Description}},
		TransitionID: func(router.Target) string { return m.ID },
		Enter: func(context.Context, router.Plan) (router.Effect, error) {
			return router.Effect{Events: m.Events, Documents: m.Documents, Reason: m.Reason}, nil
		},
	}
}

// FeatureProgress is the notice that tells the chief of staff of a move of the
// workstream's feature state: the one every move of it records.
func FeatureProgress(id, from, to, reason string) trace.Event {
	body := fmt.Sprintf("Workstream state changed to %s: %s", to, reason)
	if from != "" {
		body = fmt.Sprintf("Workstream state changed from %s to %s: %s", from, to, reason)
	}
	return trace.Progress(id, "state", body)
}
