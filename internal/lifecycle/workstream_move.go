package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/router"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

const MoveWorkstreamTool = "move_workstream"

// WorkstreamMoveSubject is the workflow subject that orders a workstream's
// moves: moved-<n> once move n is recorded.
const WorkstreamMoveSubject = "workstream-move"

// WorkstreamMovePrefix begins the trace record ID of every workstream move.
const WorkstreamMovePrefix = "workstream-move-"

// WorkstreamMoveTargets are the feature states a workstream moves to.
var WorkstreamMoveTargets = []string{

	SketchedState, InShedState, RatifiedState, BuildingState, AssembledState, DeliveredState, AbandonedState}

func WorkstreamMovePath(n int) string {
	return fmt.Sprintf("moves/move-%d.json", n)
}
func WorkstreamMoveDocument(n int) string {
	return fmt.Sprintf("%s%d", WorkstreamMovePrefix, n)
}

// WorkstreamMoves returns the workstream's recorded moves in the order they
// were recorded.
func WorkstreamMoves(repo *trace.Repository, stream config.WorkstreamID) ([]WorkstreamMove, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return nil, err
	}
	var moves []WorkstreamMove
	for _, d := range docs {
		if !strings.HasPrefix(d.ID, WorkstreamMovePrefix) {
			continue
		}
		var m WorkstreamMove
		if err := json.Unmarshal([]byte(d.Content), &m); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Path, err)
		}
		moves = append(moves, m)
	}
	return moves, nil
}

// ReviewMoves returns the workstream's moves to building or assembled, each
// of which asks for a fresh final review once every unit has merged.
func ReviewMoves(repo *trace.Repository, stream config.WorkstreamID) ([]WorkstreamMove, error) {
	moves, err := WorkstreamMoves(repo, stream)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(moves, func(m WorkstreamMove) bool {
		return m.To != BuildingState && m.To != AssembledState
	}), nil
}

// ReopenHeld reports whether a move to building holds the workstream there:
// its latest move is to building and no amendment has been filed since. The
// Foreman reassembles it once a revision filed after the move is decided and
// its units land, or once it is moved to assembled.
func ReopenHeld(repo *trace.Repository, stream config.WorkstreamID) (bool, error) {
	records, err := trace.Read[trace.Record](repo, stream)
	if err != nil {
		return false, err
	}
	held := false
	for _, record := range records {
		switch r := record.(type) {
		case trace.Document:
			if !strings.HasPrefix(r.ID, WorkstreamMovePrefix) {
				continue
			}
			var m WorkstreamMove
			if err := json.Unmarshal([]byte(r.Content), &m); err != nil {
				return false, fmt.Errorf("%s: %w", r.Path, err)
			}
			held = m.To == BuildingState
		case trace.Amendment:
			held = false
		}
	}
	return held, nil
}

// MoveWorkstream moves a workstream to another feature state, as actor: the
// owner, or the chief of staff in turn. The owner's move is a pinned move. The
// chief of staff's is a proposal the router decides: it commits the move, leaves
// the workstream where it is, holds it, or, for ratification and abandonment,
// which the proposal cannot carry the authority for, leaves a pending owner
// decision.
//
// A move to sketched asks the architect for a redraft with the note, to
// in-shed for one more round of debate, to ratified ratifies the latest
// ratification packet, and to abandoned abandons the workstream; each is the
// owner's own shed, ratification or abandonment action and records itself.
// A move to building reopens an assembled workstream, or a delivered one
// whose pull request is open, on its existing feature branch for repair
// units; to assembled asks for a fresh final review of the branch as it is;
// and to delivered returns a reopened workstream whose branch is still the
// one its open pull request carries to delivered. Those three record the
// Move and its note together with the move of the feature state.
func (s *Controller) MoveWorkstream(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, req WorkstreamMoveRequest, actor trace.Actor, turn string) (WorkstreamMoveResponse, *APIError) {
	note := strings.TrimSpace(req.Note)
	if !slices.Contains(WorkstreamMoveTargets, req.To) || note == "" {
		return WorkstreamMoveResponse{}, &APIError{Code: Validation, Message: "a workstream move requires a note and a state: " + strings.Join(WorkstreamMoveTargets, ", ")}
	}
	if session, err := isSession(repo, stream); err != nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream kind"}
	} else if session {
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: "a session has no feature states to move"}
	}
	if held, err := ChangeHeld(repo, stream); err != nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workgroup change"}
	} else if held {
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: "a workgroup change holds the workstream; it moves once the change is applied"}
	}
	feature, err := repo.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream state"}
	}
	if feature.Value == intake.HandedState && (req.To == SketchedState || req.To == InShedState) {
		return s.moveHanded(ctx, repo, stream, req.To, note, actor)
	}
	if feature.Value == SketchedState && (req.To == SketchedState || req.To == InShedState) {
		if out, waiting, api := s.moveWaitingMember(ctx, repo, stream, req.To, note, actor); api != nil || waiting {
			return out, api
		}
	}
	if actor.Kind != ownerActor.Kind {
		switch req.To {
		case RatifiedState, AbandonedState:
			return WorkstreamMoveResponse{}, s.proposeGate(ctx, repo, stream, req.To, note, actor, turn)
		case SketchedState, InShedState:
			return s.proposeShed(ctx, repo, stream, req.To, note, actor, turn)
		}
	}
	respond := func(detail string) (WorkstreamMoveResponse, *APIError) {
		after, err := repo.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream state"}
		}
		return WorkstreamMoveResponse{Workstream: stream, State: after.Value, Detail: detail}, nil
	}
	raw := string(stream)
	switch req.To {
	case SketchedState:
		out, api := s.ShedRedraft(ctx, raw, ShedRedraftRequest{Note: note})
		if api != nil {
			return WorkstreamMoveResponse{}, api
		}
		return respond(out.Detail)
	case InShedState:
		out, api := s.ShedMore(ctx, raw, ShedMoreRequest{Rounds: 1})
		if api != nil {
			return WorkstreamMoveResponse{}, api
		}
		return respond(out.Detail)
	case RatifiedState:
		packet, _, found, err := shed.LatestPacket(repo, stream)
		if err != nil {
			return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the ratification packet"}
		}
		if !found {
			return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s has no ratification packet to ratify", stream)}
		}
		out, api := s.Ratify(ctx, raw, RatifyRequest{Spec: packet.Revision.Spec, Plan: packet.Revision.Plan})
		if api != nil {
			return WorkstreamMoveResponse{}, api
		}
		return respond(out.Detail)
	case AbandonedState:
		if _, api := s.Abandon(ctx, raw, AbandonRequest{Reason: note}); api != nil {
			return WorkstreamMoveResponse{}, api
		}
		return respond("the owner abandoned the workstream: " + note)
	}
	records, api := s.workstreamMoveRecords(ctx, repo, stream, feature, req.To, note, actor, turn, s.Now())
	if api != nil {
		return WorkstreamMoveResponse{}, api
	}
	policy, err := s.workstreamMovePolicy(repo, stream, feature, req.To, actor)
	if err != nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream's final reviews"}
	}
	res, err := s.routeWorkstreamMove(ctx, repo, stream, feature, req.To, note, actor, turn, policy)
	switch {
	case err != nil:
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot record the move"}
	case res.Committed && res.Target == NoMoveTarget:
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s stays %s and was not moved to %s: %s, and Jev judged that the evidence does not support the move. The proposal, the earlier moves and Jev's judgment are in the trace", stream, feature.Value, req.To, policy.Why)}
	case res.Committed:
		move := records.move
		if res.Target != req.To {
			// Jev departed from the chief of staff's proposal: the move
			// recorded is the one it entered.
			if entered, ok, err := latestWorkstreamMove(repo, stream); err == nil && ok {
				move = entered
			}
		}
		return WorkstreamMoveResponse{Workstream: stream, State: res.State, Detail: records.reason, Move: &move}, nil
	case res.Outcome == router.OutcomeHeld && res.Hold == router.HoldStale:
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s changed since it was read; read it again", stream)}
	case res.Pending():
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: pendingOwnerMove(stream, req.To, res, recoveryCard)}
	}
	return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s was not moved to %s: the move is held (%s). %s Nothing moved, and the workstream stays %s", stream, req.To, res.Hold, res.Reason, feature.Value)}
}

// recoveryCard names where the owner decides a move that belongs to the owner
// and has no card of its own.
const recoveryCard = router.RecoveryCard

// pendingOwnerMove says what became of a proposal that belongs to the owner: it
// is a pending owner decision, and where the owner decides it.
func pendingOwnerMove(stream config.WorkstreamID, to string, res router.Result, card string) string {
	why := "the owner decides it"
	switch {
	case res.Hold == router.HoldIntent:
		why = "Jev judged that it would change what the owner approved"
	case to == RatifiedState:
		why = "ratification is the owner's decision"
	case to == AbandonedState:
		why = "abandonment is the owner's decision"
	}
	return fmt.Sprintf("workstream %s was not moved to %s: %s. The move is pending as an owner decision on %s; nothing moved", stream, to, why, card)
}

// proposeGate routes the chief of staff's proposal to ratify or abandon the
// workstream. A proposal carries no authority for an owner gate, so the router
// commits nothing and leaves the move as a pending owner decision, on the
// ratification card for a ratification and the recovery card otherwise. The
// answer is the account of that, for the chief of staff to pass on.
func (s *Controller) proposeGate(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, to, note string, actor trace.Actor, turn string) *APIError {
	res, err := s.proposeToOwner(ctx, repo, stream, to, note, actor, turn)
	switch {
	case err != nil:
		return &APIError{Code: Internal, Message: "cannot record the proposal"}
	case res.Hold == router.HoldStale:
		return &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s changed since it was read; read it again", stream)}
	}
	card := recoveryCard
	if to == RatifiedState {
		card = "the ratification card"
	}
	return &APIError{Code: Conflict, Message: pendingOwnerMove(stream, to, res, card)}
}

// moveWaitingMember handles a move of a sketched member of a cross-repo
// workgroup whose committee review waits for the workgroup spec. A move to
// sketched asks the architect for another draft of a workgroup spec whose
// drafting failed; a move to in-shed is refused with what the member waits
// for. waiting is false for any other workstream, which moves as usual.
func (s *Controller) moveWaitingMember(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, to, note string, actor trace.Actor) (WorkstreamMoveResponse, bool, *APIError) {
	unreadable := &APIError{Code: Internal, Message: "cannot read the workstream's workgroup"}
	required, err := repo.GroupApprovalRequired(stream)
	if err != nil {
		return WorkstreamMoveResponse{}, true, unreadable
	}
	if !required {
		return WorkstreamMoveResponse{}, false, nil
	}
	id, err := repo.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return WorkstreamMoveResponse{}, true, unreadable
	}
	_, drafted, err := repo.CrossRepoWorkgroup(id)
	if err != nil {
		return WorkstreamMoveResponse{}, true, unreadable
	}
	if drafted {
		return WorkstreamMoveResponse{}, false, nil
	}
	if to == InShedState {
		wait, err := s.ProjectSpecDraftWait(ctx, id)
		if errors.Is(err, trace.ErrLocked) {
			return WorkstreamMoveResponse{}, true, &APIError{Code: Conflict, Message: fmt.Sprintf("workgroup %s is busy; ask again in a moment", id)}
		}
		if err != nil {
			return WorkstreamMoveResponse{}, true, unreadable
		}
		return WorkstreamMoveResponse{}, true, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s stays sketched: its committee review starts by itself once workgroup %s has a workgroup spec, and %s", stream, id, wait)}
	}
	detail, api := s.RequestProjectSpecDraftAgain(ctx, id, note, actor)
	if api != nil {
		return WorkstreamMoveResponse{}, true, api
	}
	return WorkstreamMoveResponse{Workstream: stream, State: SketchedState, Detail: detail}, true, nil
}

// ProjectSpecDraftWait describes where drafting workgroup id's first
// workgroup spec stands, for a member that waits for it. Before the first
// draft, it names each member that is not sketched yet and what it waits for.
func (s *Controller) ProjectSpecDraftWait(ctx context.Context, id config.WorkgroupID) (string, error) {
	wt, err := trace.OpenWorkgroupAuthority(s.Current().Root, id)
	if err != nil {
		if wt != nil {
			wt.Close()
		}
		return "", err
	}
	defer wt.Close()
	groups, _, err := trace.Workgroups(wt)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(groups, func(g trace.Workgroup) bool { return g.ID == id })
	if i < 0 || len(groups[i].Members) == 0 {
		return "", fmt.Errorf("workgroup %s is not recorded", id)
	}
	host := groups[i].Members[0]
	state, err := wt.Workflow(host, ProjectSpecDraftSubject)
	if err != nil {
		return "", err
	}
	switch state.Value {
	case "":
		members, err := s.CrossRepoMemberRepos(ctx, groups[i])
		if err != nil {
			return "", err
		}
		waiting, err := s.UnsketchedMembers(members)
		if err != nil || waiting == "" {
			return "the architect drafts it once every member is sketched", err
		}
		return "the architect drafts it once every member is sketched; " + waiting, nil
	case "drafting":
		return "the architect is drafting it now", nil
	case "failed":
		failures, last, err := ProjectSpecDraftFailures(wt, host)
		if err != nil {
			return "", err
		}
		if failures >= ProjectSpecDraftAttempts {
			return fmt.Sprintf("its last %d drafts were refused, most recently because %s; move a member to sketched with a note to ask the architect for another draft", failures, last.Reason), nil
		}
		return fmt.Sprintf("its last draft was refused because %s; the service asks the architect again on its next pass", last.Reason), nil
	}
	return "its draft is " + state.Value, nil
}

// UnsketchedMembers names each workgroup member that is not sketched yet, its
// state and, for a handed member, where drafting its spec and plan stands, or
// "" when every member is sketched.
func (s *Controller) UnsketchedMembers(members []trace.CrossRepoMember) (string, error) {
	var parts []string
	for _, m := range members {
		feature, err := m.Project.Workflow(m.Workstream, trace.FeatureSubject)
		if err != nil {
			return "", err
		}
		if feature.Value == SketchedState {
			continue
		}
		part := fmt.Sprintf("member %s is %s", m.Workstream, intake.FeatureState(feature.Value))
		if feature.Value == intake.HandedState {
			d := &intake.Drafter{S: s.Intake().DrafterDependencies(), Repository: m.Project}
			wait, _, err := d.DraftWait(m.Workstream)
			if err != nil {
				return "", err
			}
			part += ": " + wait
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; "), nil
}

// moveHanded handles a move to sketched or in-shed of a handed workstream,
// whose spec and plan are not accepted yet. A move to sketched after drafting
// stopped asks the architect for a new run of drafts with the note; any other
// such move is refused with where drafting stands.
func (s *Controller) moveHanded(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, to, note string, actor trace.Actor) (WorkstreamMoveResponse, *APIError) {
	d := &intake.Drafter{S: s.Intake().DrafterDependencies(), Repository: repo}
	if to == InShedState {
		wait, stopped, err := d.DraftWait(stream)
		if err != nil {
			return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream's drafts"}
		}
		next := "its committee review starts by itself once a draft is accepted and it is sketched"
		if !stopped {
			next = "it moves to sketched by itself once a draft is accepted, and its committee review starts then"
		}
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s is %s and has no accepted spec and plan to debate: %s; %s", stream, intake.HandedState, wait, next)}
	}
	detail, refused, err := d.RequestAgain(ctx, stream, note, actor)
	switch {
	case err != nil:
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot ask the architect for another run of drafts"}
	case refused != "":
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: refused}
	}
	return WorkstreamMoveResponse{Workstream: stream, State: intake.HandedState, Detail: detail}, nil
}

// proposeShed routes the chief of staff's proposal to ask the architect for a
// redraft (to sketched) or to run another round of debate (to in-shed) after
// debate concluded. Leaving the decision to the owner is the default, and Jev
// may depart from it at the strict bar.
func (s *Controller) proposeShed(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, to, note string, actor trace.Actor, turn string) (WorkstreamMoveResponse, *APIError) {
	if s.Router == nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "routing a workstream's moves is unavailable"}
	}
	x, api := s.Intake().ShedProposalExit(string(stream), intake.ShedProposal{Redraft: to == SketchedState, Note: note, Turn: turn, Actor: actor, At: s.Now()})
	if api != nil {
		return WorkstreamMoveResponse{}, api
	}
	moves, err := WorkstreamMoves(repo, stream)
	if err != nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream's moves"}
	}
	for _, m := range moves {
		x.Evidence.Records = append(x.Evidence.Records, router.Entry{Kind: "earlier move", ID: WorkstreamMoveDocument(m.Move), Text: fmt.Sprintf("%s moved the workstream from %s to %s: %s", m.By, m.From, m.To, m.Note)})
	}
	res, err := s.Router.Route(ctx, x)
	switch {
	case err != nil:
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot record the move"}
	case res.Committed && res.Target == intake.TargetWait:
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s stays where it is and was not moved to %s: the chief of staff does not ask for a redraft or another round of debate by default, and Jev judged that the evidence does not support it. The proposal and Jev's judgment are in the trace", stream, to)}
	case res.Committed:
		feature, err := repo.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream state"}
		}
		detail := "the architect redrafts the spec and the plan with your note"
		if to == InShedState {
			detail = "the committee runs one more round of debate"
		}
		return WorkstreamMoveResponse{Workstream: stream, State: feature.Value, Detail: detail}, nil
	case res.Outcome == router.OutcomeHeld && res.Hold == router.HoldStale:
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s changed since it was read; read it again", stream)}
	case res.Pending():
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: pendingOwnerMove(stream, to, res, recoveryCard)}
	}
	return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s was not moved to %s: the move is held (%s). %s Nothing moved", stream, to, res.Hold, res.Reason)}
}

// PublicationInFlight returns the action of a publication of the workstream
// that has no result yet, or "" when none is in flight.
func PublicationInFlight(repo *trace.Repository, stream config.WorkstreamID) (string, error) {
	records, err := repo.Operations(stream)
	if err != nil {
		return "", err
	}
	for _, r := range records {
		if r.Result == nil && (r.Operation.Action == PublishAction || r.Operation.Action == PublishUpstreamAction) {
			return r.Operation.Action, nil
		}
	}
	return "", nil
}

// OpenPullRequest returns the workstream's current publication when its pull
// Request is open: recorded, neither merged nor closed, and the workstream
// not delivered by an owner-recorded merge.
func OpenPullRequest(repo *trace.Repository, stream config.WorkstreamID) (DeliveryPublication, *APIError) {
	if _, merged, err := deliveryMerge(repo, stream); err != nil {
		return DeliveryPublication{}, &APIError{Code: Internal, Message: "cannot read the workstream's delivery"}
	} else if merged {
		return DeliveryPublication{}, &APIError{Code: Conflict, Message: "the owner recorded the workstream's branch merged upstream; it does not reopen"}
	}
	pr, found, err := currentOwnPublication(repo, stream)
	if err != nil {
		return DeliveryPublication{}, &APIError{Code: Internal, Message: "cannot read the workstream's publications"}
	}
	if !found {
		return DeliveryPublication{}, &APIError{Code: Conflict, Message: "the workstream has no pull request of its own; it is delivered through the owner's delivery approval"}
	}
	if finished, err := ownPullRequestFinished(repo, stream); err != nil {
		return DeliveryPublication{}, &APIError{Code: Internal, Message: "cannot read the pull request outcome"}
	} else if finished {
		return DeliveryPublication{}, &APIError{Code: Conflict, Message: fmt.Sprintf("pull request #%d is merged or closed; the workstream does not reopen", pr.PullRequest)}
	}
	return pr, nil
}

// Assemblable refuses a workstream with an unmerged unit of its sealed plan,
// an open discovery or a pending revision.
func (s *Controller) Assemblable(repo *trace.Repository, stream config.WorkstreamID) *APIError {
	b, found, err := (&masons{s: s, cfg: s.About(repo), repository: repo}).readSealed(stream)
	if err != nil {
		return &APIError{Code: Internal, Message: "cannot read the sealed plan"}
	}
	if !found || !allMerged(b) {
		var open []string
		if found {
			for _, u := range b.Plan().Units {
				if b.States()[trace.UnitSubject(u.ID)].Value != UnitMerged {
					open = append(open, u.ID)
				}
			}
		}
		return &APIError{Code: Conflict, Message: fmt.Sprintf("units %s have not merged; move them, or wait for them to land", strings.Join(open, ", "))}
	}
	if pending, err := s.UnresolvedWork(repo, stream); err != nil {
		return &APIError{Code: Internal, Message: "cannot read the workstream's discoveries and revisions"}
	} else if pending {
		return &APIError{Code: Conflict, Message: "an open discovery or a pending revision prevents assembly"}
	}
	return nil
}

// FeatureTip returns the commit the workstream's feature branch is at.
func (s *Controller) FeatureTip(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID) (string, *APIError) {
	g, err := featureWorkspaces(s.About(repo), repo).Of(stream)
	if err != nil {
		return "", &APIError{Code: Internal, Message: "cannot open the workstream's feature branch"}
	}
	tip, exists, err := g.Branch(ctx, featureBranch(stream))
	if err != nil {
		return "", &APIError{Code: Internal, Message: "cannot read the workstream's feature branch"}
	}
	if !exists {
		return "", &APIError{Code: Conflict, Message: fmt.Sprintf("the clone has no feature branch %s", featureBranch(stream))}
	}
	return tip, nil
}

// OwnerMoveWorkstream moves a workstream as the owner.
func (s *Controller) OwnerMoveWorkstream(ctx context.Context, raw string, req WorkstreamMoveRequest) (WorkstreamMoveResponse, *APIError) {
	_, stream, repo, api := s.ConversationTrace(raw)
	if api != nil {
		return WorkstreamMoveResponse{}, api
	}
	if gone, err := abandoned(repo, stream); err != nil {
		return WorkstreamMoveResponse{}, &APIError{Code: Internal, Message: "cannot read the workstream state"}
	} else if gone {
		return WorkstreamMoveResponse{}, &APIError{Code: Conflict, Message: "an abandoned workstream does not move"}
	}
	return s.MoveWorkstream(ctx, repo, stream, req, ownerActor, "")
}

// MoveWorkstream returns the move_workstream tool of one claimed
// chief-of-staff turn. The chief of staff's move is its own decision, which the
// router enters after the hard prerequisites; ratification and abandonment are
// owner gates that wait on a card. The result says whether the workstream moved
// or what holds it.
// In a turn answering the owner, it records the owner's own move. A move that is
// not made is an ordinary result, {"recorded":false,"reason":...}, which says
// what became of it. A shared chief of staff names the member that moves.
func (c *Controls) MoveWorkstream(repository *trace.Repository, scope coreadapter.Scope) coreadapter.Tool {
	tool := coreadapter.Tool{Name: MoveWorkstreamTool, Effect: coreadapter.ToolMemory, Description: "Move this workstream to another feature state when the state machine leaves it stuck or wrong. The move is your decision: the service makes it unless a hard prerequisite refuses it (a landing, rebase or publication in flight, a pause or the budget), and the result says what happened. " +
		"to: building (reopen an assembled workstream, or a delivered one whose pull request is open, on its existing feature branch; it stays building until a revision you file afterwards with route_amendment is decided and its units land; route_amendment reopens the workstream itself, so you rarely need this move), " +
		"assembled (run a fresh final review of the branch as it is: after a failed or stuck final review, after a drift rebase, or to return a delivered workstream with an open pull request to final review), " +
		"sketched (the architect redrafts the spec and the plan with the note, once debate concluded; for a sketched member of a cross-repo workgroup that has no workgroup spec yet, the architect drafts the workgroup spec again with the note after its drafting failed; for a handed workstream whose drafting stopped after its drafts were refused, the architect starts a new run of drafts with the note), in-shed (one more round of debate, once debate concluded; a sketched member of a cross-repo workgroup enters review by itself once its workgroup spec is recorded, and the refusal says where that stands), delivered (return a reopened workstream whose branch is unchanged since its pull request was published), " +
		"or ratified and abandoned, which are the owner's decisions: the proposal waits as a pending owner decision on a card. " +

		"A delivered workstream's open pull request is updated only after a fresh final review and the owner's delivery approval. note: what is stuck or wrong and what should happen next; the final reviewer receives it. Report to the owner what the result says happened. owner_decided: true only when the owner asked for the move in the message this turn answers, with the owner's words as the note. workstream: a member workstream ID of your workgroup to act on; omit for this conversation's own workstream.", InputSchema: json.RawMessage(`{"type":"object","properties":{"to":{"type":"string","enum":["sketched","in-shed","ratified","building","assembled","delivered","abandoned"]},"note":{"type":"string"},"owner_decided":{"type":"boolean"},"workstream":{"type":"string"}},"required":["to","note"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			To           string              `json:"to"`
			Note         string              `json:"note"`
			OwnerDecided bool                `json:"owner_decided"`
			Workstream   config.WorkstreamID `json:"workstream"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return nil, fmt.Errorf("tool input: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("tool input must be one object")
		}
		s := c.Load()
		if s == nil {
			return nil, errors.New("workstream moves are unavailable")
		}
		if err := repository.ActiveTurn(scope); err != nil {
			return nil, err
		}
		target, stream, refusal, err := repository.ChiefMember(config.WorkstreamID(scope.Workstream), input.Workstream)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return priorityRefusal(refusal)
		}
		request := WorkstreamMoveRequest{To: input.To, Note: input.Note}
		if gone, err := abandoned(target, stream); err != nil {
			return nil, err
		} else if gone {
			return priorityRefusal("an abandoned workstream does not move")
		}
		if input.OwnerDecided {
			turn, owner, err := repository.OwnerTurn(trace.ChiefOfStaff, scope)
			if err != nil {
				return nil, err
			}
			if !owner {
				return priorityRefusal("owner_decided is only for a move the owner asked for in the message this turn answers")
			}
			return WorkstreamMoveResult(s.MoveWorkstream(ctx, target, stream, request, turn.Actor, scope.Turn))
		}
		return WorkstreamMoveResult(s.MoveWorkstream(ctx, target, stream, request, chiefActor, scope.Turn))
	}
	return tool
}
func WorkstreamMoveResult(out WorkstreamMoveResponse, api *APIError) (json.RawMessage, error) {
	if api != nil {
		if api.Code == Internal {
			return nil, errors.New(api.Message)
		}
		return priorityRefusal(api.Message)
	}
	return json.Marshal(struct {
		Recorded bool   `json:"recorded"`
		State    string `json:"state"`
		Detail   string `json:"detail"`
	}{true, out.State, out.Detail})
}

// ReopenForRevision wraps the chief of staff's route_amendment tool: a revision
// filed without a question for an assembled workstream, or a delivered one
// whose pull request is open, first reopens that workstream to building on its
// existing feature branch as the chief of staff's own move, so the revision's
// units can be built. The result names the reopening.
func (c *Controls) ReopenForRevision(repository *trace.Repository, scope coreadapter.Scope, route coreadapter.Tool) coreadapter.Tool {
	inner := route.Handle
	route.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var input struct {
			Question   string              `json:"question"`
			Workstream config.WorkstreamID `json:"workstream"`
			Reason     string              `json:"reason"`
		}
		if err := json.Unmarshal(raw, &input); err != nil || input.Question != "" {
			return inner(ctx, raw)
		}
		s := c.Load()
		if s == nil {
			return inner(ctx, raw)
		}
		target, stream, refusal, err := repository.ChiefMember(config.WorkstreamID(scope.Workstream), input.Workstream)
		if err != nil || refusal != "" {
			return inner(ctx, raw)
		}
		feature, err := target.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return nil, err
		}
		reopened := ""
		if feature.Value == AssembledState || feature.Value == DeliveredState {
			note := "Reopened so the revision the chief of staff commissioned can be built"
			if r := strings.TrimSpace(input.Reason); r != "" {
				note += ": " + r
			}
			out, api := s.MoveWorkstream(ctx, target, stream, WorkstreamMoveRequest{To: BuildingState, Note: note}, chiefActor, scope.Turn)
			if api != nil {
				if api.Code == Internal {
					return nil, errors.New(api.Message)
				}
				return priorityRefusal("the workstream could not be reopened for the revision: " + api.Message)
			}
			reopened = out.Detail
		}
		result, err := inner(ctx, raw)
		if err != nil || reopened == "" {
			return result, err
		}
		var fields map[string]any
		if json.Unmarshal(result, &fields) != nil {
			return result, nil
		}
		fields["reopened"] = reopened
		return json.Marshal(fields)
	}
	return route
}
