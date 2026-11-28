package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const DeltaDraftTool = "deliver_delta"
const DeltaReviewTool = "review_delta"
const DeltaReplyTool = "answer_delta"
const DeltaTurnAction = "workgroup-delta-turn"

type DeltaTurnInput struct {
	Address config.WorkstreamID `json:"address"`
	Change  string              `json:"change"`
	Role    string              `json:"role"`
	Agent   string              `json:"agent"`
	Tool    string              `json:"tool"`
}

// DeltaRunner resolves the authoritative group journal per operation. The
// real member's controller owns the operation; the turn lives with the group.
type DeltaRunner struct{ S *Controller }

func DecodeDeltaTurn(op coreadapter.Operation) (DeltaTurnInput, error) {
	var in DeltaTurnInput
	if op.Boundary != coreadapter.RunnerBoundary || op.Action != DeltaTurnAction {
		return in, errors.New("unsupported delta operation")
	}
	if err := DecodeToolInput(op.Input, &in); err != nil {
		return in, err
	}
	if config.CheckWorkstreamIDs(in.Address) != nil || in.Change == "" || in.Agent == "" || !(in.Role == architectRole && in.Agent == architectAgent && (in.Tool == DeltaDraftTool || in.Tool == DeltaReplyTool) || in.Role == committeeRole && in.Tool == DeltaReviewTool) {
		return in, errors.New("invalid delta turn scope")
	}
	return in, nil
}
func DeltaTurnID(c WorkgroupChange, agent, tool string) string {
	if c.Attempt > 0 {
		return ChangeIdentity(fmt.Sprintf("change-%s-retry-%d-%s-%s", c.ID, c.Attempt, tool, agent))
	}
	return ChangeIdentity("change-" + c.ID + "-" + tool + "-" + agent)
}

// Load uses only the group journal when the owning controller is retired.
// Retirement cancels that member's dispatch intent, never the shared decision.
func (d DeltaRunner) Load(ctx context.Context, in DeltaTurnInput) (*trace.Repository, WorkgroupChange, bool, func(), error) {
	noop := func() {
	}
	var wt *trace.Repository
	var g trace.Workgroup
	var close func()
	var err error
	retired := false
	if _, archived := d.S.ArchivedWorkstream(string(in.Address)); !archived {
		wt, g, _, close, err = d.S.ChangeContext(ctx, string(in.Address))
		if err != nil {
			if _, archived = d.S.ArchivedWorkstream(string(in.Address)); !archived {
				return nil, WorkgroupChange{}, false, noop, err
			}
		}
	}
	if wt == nil {
		a, archived := d.S.ArchivedWorkstream(string(in.Address))
		if !archived || a.Workgroup == "" {
			return nil, WorkgroupChange{}, false, noop, errors.New("delta controller has no shared retirement authority")
		}
		wt, err = trace.OpenWorkgroupAuthority(d.S.Current().Root, a.Workgroup)
		if err != nil {
			if wt != nil {
				wt.Close()
			}
			return nil, WorkgroupChange{}, false, noop, err
		}
		close = func() {
			wt.Close()
		}
		groups, _, readErr := trace.Workgroups(wt)
		if readErr == nil {
			for _, candidate := range groups {
				if candidate.ID == a.Workgroup {
					g = candidate
				}
			}
			if len(g.Members) < 2 || !slices.Contains(g.Members[1:], in.Address) {
				readErr = errors.New("retired delta controller is not an approved workgroup member")
			} else {
				members, approved, memberErr := d.S.WorkgroupMembers(ctx, g)
				readErr = memberErr
				if readErr == nil && (!approved || !slices.ContainsFunc(members, func(m trace.CrossRepoMember) bool {
					return m.Workstream == in.Address && m.Project.Project() == a.Project
				})) {
					readErr = errors.New("retired delta controller identity differs from approval")
				}
			}
		}
		if readErr != nil {
			close()
			return nil, WorkgroupChange{}, false, noop, readErr
		}
		retired = true
	}
	changes, err := ReadChanges(wt, g.Members[0])
	if err == nil {
		for _, c := range changes {
			if c.ID == in.Change {
				if retired && (c.Workgroup != g.ID || c.Host != g.Members[0]) {
					err = errors.New("retired delta journal identity differs from its authority")
					break
				}
				return wt, c, retired, close, nil
			}
		}
		if err == nil {
			err = errors.New("delta journal not found")
		}
	}
	close()
	return nil, WorkgroupChange{}, false, noop, err
}
func DeltaTurnState(wt *trace.Repository, c WorkgroupChange, in DeltaTurnInput) (running, captured bool, err error) {
	t, err := wt.Thread(c.Host, in.Agent)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, false, err
	}
	for _, q := range t.Turns {
		if q.Request.TurnID != DeltaTurnID(c, in.Agent, in.Tool) {
			continue
		}
		captured = q.Response != nil
		if q.Claim != nil && q.Response == nil && q.CompletedAt.IsZero() {
			current, err := wt.CurrentTurnClaim(c.Host, q.Claim)
			if err != nil || current {
				return current, captured, err
			}
		}
	}
	return false, captured, nil
}
func RetiredDeltaResult(in DeltaTurnInput) coreadapter.OperationResult {
	return coreadapter.OperationResult{Outcome: "cancelled", Evidence: fmt.Sprintf("owner archived delta controller %s; shared decision %s remains in its authoritative journal", in.Address, in.Change)}
}
func (d DeltaRunner) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := DecodeDeltaTurn(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	wt, c, retired, close, err := d.Load(ctx, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	defer close()
	running, captured, err := DeltaTurnState(wt, c, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if running {
		return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "delta turn is running"}, nil
	}
	if retired && !captured {
		result := RetiredDeltaResult(in)
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: result.Evidence, Result: &result}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "delta turn requires reconciliation"}, nil
}
func (d DeltaRunner) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := DecodeDeltaTurn(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	wt, c, retired, close, err := d.Load(ctx, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	defer close()
	if retired {
		running, captured, err := DeltaTurnState(wt, c, in)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if running {
			return coreadapter.OperationResult{}, errors.New("shared delta turn is still running; retired controller remains held")
		}
		if !captured {
			return RetiredDeltaResult(in), nil
		}
	}
	ctx, release := d.S.Stoppable(ctx, wt.Project(), c.Host)
	defer release()
	if err := d.S.RunChangeTurn(ctx, wt, c, in.Role, in.Agent, in.Tool); err != nil {
		return coreadapter.OperationResult{}, err
	}
	changes, err := ReadChanges(wt, c.Host)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	for _, latest := range changes {
		if latest.ID == c.ID && latest.State == "failed" {
			return coreadapter.OperationResult{Outcome: "failed", Evidence: latest.Failure}, nil
		}
	}
	return coreadapter.OperationResult{Outcome: "completed", Evidence: "delta turn and its contribution are recorded"}, nil
}

// QueueChangeTurn records intent only; the controller's runner executes it
// outside the schedule stage, with the ordinary shutdown/retry lifecycle.
func (s *Controller) QueueChangeTurn(ctx context.Context, wt *trace.Repository, g trace.Workgroup, c WorkgroupChange, role, agent, tool string) error {
	address := s.ChangeLeader(s.ChangeMembers(g, wt))
	if address == "" {
		return errors.New("workgroup has no live controller for its pending delta turn")
	}
	_, _, repo, api := s.ConversationTrace(string(address))
	if api != nil {
		return errors.New(api.Message)
	}
	id := DeltaTurnID(c, agent, tool)
	state, err := repo.Workflow(address, id)
	if err != nil || state.Value != "" {
		return err
	}
	data, err := json.Marshal(DeltaTurnInput{Address: address, Change: c.ID, Role: role, Agent: agent, Tool: tool})
	if err != nil {
		return err
	}
	event := trace.EventID(id, "run")
	op := coreadapter.Operation{ID: trace.OperationID(repo.Project(), address, event), Boundary: coreadapter.RunnerBoundary, Action: DeltaTurnAction, Input: data}
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: address, At: s.Now(), Actor: amendmentActor, Cause: WorkgroupChangeDocument(c.ID)}, Subject: id, To: "queued", Reason: "debate only the requested delta"}, Events: []trace.Event{{ID: event, Kind: "delta", Body: "Debate the requested amendment delta", Operation: &op}}})
	if err != nil {
		return err
	}
	return s.Step("change-delta-queued")
}
func DecodeToolInput(raw json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one object")
	}
	return nil
}

// ChangeLeader selects a controller that still has a live conversation.
// Archive records remain authoritative before and after trace cleanup.
func (s *Controller) ChangeLeader(members []config.WorkstreamID) config.WorkstreamID {
	var leader config.WorkstreamID
	for _, member := range members {
		if _, archived := s.ArchivedWorkstream(string(member)); archived {
			continue
		}
		if leader == "" || member < leader {
			leader = member
		}
	}
	return leader
}
func (s *Controller) ChangePass(ctx context.Context, repo *trace.Repository) error {
	streams, err := nonSessionWorkstreams(repo)
	if err != nil {
		return err
	}
	groups, _, err := trace.Workgroups(repo)
	if err != nil {
		return err
	}
	type crossRepoReads struct {
		authority         *trace.Repository
		group             trace.Workgroup
		retirementChecked bool
		retired           bool
		pending           bool
	}
	crossRepo := map[config.WorkgroupID]*crossRepoReads{}
	seen := map[config.WorkgroupID]bool{}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !s.Current().Active(repo.Project()) {
			return nil
		}
		if _, archived := s.ArchivedWorkstream(string(stream)); archived {
			continue
		}
		if kind, err := repo.Kind(stream); err != nil {
			return err
		} else if kind == trace.SessionKind {
			continue
		}
		group, local := trace.WorkgroupOf(groups, stream)
		cross, err := repo.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return err
		}
		if !local && cross == "" {
			continue
		}
		scanMembers := group.Members
		if cross != "" {
			cached, ok := crossRepo[cross]
			if !ok {
				g, _, err := repo.CrossRepoWorkgroup(cross)
				if err != nil {
					return err
				}
				cached = &crossRepoReads{group: g}
				crossRepo[cross] = cached
			}
			group = cached.group
			if group.ID == cross && len(group.Members) > 0 && !slices.Contains(group.Members, stream) {
				continue
			}
			if group.ID == cross && len(group.Members) > 1 {
				if !cached.retirementChecked {
					live, err := s.LiveWorkgroupMembers(group)
					if err != nil {
						return err
					}
					if len(live) == 0 {
						authority, err := trace.OpenWorkgroupAuthority(s.Current().Root, cross)
						if authority != nil {
							defer authority.Close()
						}
						if err != nil {
							return err
						}
						cached.authority = authority
						cached.retired, err = s.validateRetiredWorkgroup(ctx, cached.authority, group)
						if err != nil {
							return err
						}
					}
					if cached.retired {
						cached.pending, err = retiredChangesPending(cached.authority, group)
						if err != nil {
							return err
						}
					}
					cached.retirementChecked = true
				}
				if cached.retired {
					if cached.pending {
						s.Log().Warn("workgroup changes remain held", "workgroup", cross, "member", group.Members[1], "error", "no live member remains to reconcile pending project decisions")
					}
					continue
				}
				scanMembers = group.Members[1:]
			} else {
				scanMembers = nil
			}
		}
		if len(scanMembers) > 0 && (seen[group.ID] || s.ChangeLeader(scanMembers) != stream) {
			continue
		}
		wt, g, _, close, err := s.ChangeContext(ctx, string(stream))
		if errors.Is(err, errWorkgroupJoining) {
			continue
		}
		if err != nil {
			if canceled := ctx.Err(); canceled != nil {
				return canceled
			}
			if errors.Is(err, ErrNoLiveWorkgroupContext) {
				s.Log().Warn("workgroup changes remain held", "workgroup", cross, "error", err.Error())
				continue
			}
			if !s.Current().Active(repo.Project()) {
				return nil
			}
			if _, archived := s.ArchivedWorkstream(string(stream)); archived {
				continue
			}
			return err
		}
		members := s.ChangeMembers(g, wt)
		if seen[g.ID] || len(members) == 0 || s.ChangeLeader(members) != stream {
			close()
			continue
		}
		seen[g.ID] = true
		err = s.AdvanceChanges(ctx, wt, g)
		close()
		if err != nil {
			if canceled := ctx.Err(); canceled != nil {
				return canceled
			}
			return fmt.Errorf("workgroup %s change: %w", g.ID, err)
		}
	}
	return nil
}

func retiredChangesPending(wt *trace.Repository, g trace.Workgroup) (bool, error) {
	requests, _, err := trace.ProjectAmendments(wt)
	if err != nil {
		return false, err
	}
	changes, err := ReadChanges(wt, g.Members[0])
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(changes, func(c WorkgroupChange) bool {
		return c.State != "applied" && c.State != "rejected" && c.State != "failed"
	}) {
		return true, nil
	}
	return slices.ContainsFunc(requests, func(r trace.ProjectAmendment) bool {
		return r.Workgroup == g.ID && !slices.ContainsFunc(changes, func(c WorkgroupChange) bool {
			return c.ID == "contract-"+r.ID
		})
	}), nil
}
func (s *Controller) AdvanceChanges(ctx context.Context, wt *trace.Repository, g trace.Workgroup) error {
	requests, _, err := trace.ProjectAmendments(wt)
	if err != nil {
		return err
	}
	changes, err := ReadChanges(wt, g.Members[0])
	if err != nil {
		return err
	}
	if len(requests) == 0 && !slices.ContainsFunc(changes, func(c WorkgroupChange) bool {
		return c.State != "applied" && c.State != "rejected" && c.State != "failed" || c.State == "failed" && !c.FailureSettled
	}) {
		return nil
	}
	lock := s.GroupApprovalLock(g.ID)
	if !lock.TryLock() {
		return nil
	}
	defer lock.Unlock()
	requests, _, err = trace.ProjectAmendments(wt)
	if err != nil {
		return err
	}
	changes, err = ReadChanges(wt, g.Members[0])
	if err != nil {
		return err
	}
	for _, r := range requests {
		if r.Workgroup != g.ID {
			continue
		}
		id := "contract-" + r.ID
		if slices.ContainsFunc(changes, func(c WorkgroupChange) bool {
			return c.ID == id
		}) {
			continue
		}
		if slices.ContainsFunc(changes, func(c WorkgroupChange) bool {
			return c.State != "applied" && c.State != "rejected" && c.State != "failed"
		}) {
			break
		}
		before, members, err := s.SnapshotChange(g, wt)
		if err != nil {
			return err
		}
		c := WorkgroupChange{ID: id, Workgroup: g.ID, Host: g.Members[0], Origin: r.Origin, Request: ChangeRequest{Key: id, Kind: "amend", Scope: "workgroup", Change: r.Change + "\n" + r.Reason}, State: "drafting", Before: before, Members: members, Reviewers: s.Current().Capacity.Committee, Followups: map[config.WorkstreamID]config.WorkstreamID{}}
		c.Requester = &r
		if err := s.SaveChange(ctx, wt, &c); err != nil {
			return err
		}
		changes = append(changes, c)
	}
	for _, c := range changes {
		if c.Request.Kind == "amend" && c.Reviewers == 0 && (c.State == "drafting" || c.State == "debating" || c.State == "answering") {
			c.Reviewers = s.Current().Capacity.Committee
			if err := s.SaveChange(ctx, wt, &c); err != nil {
				return err
			}
		}
		switch c.State {
		case "drafting":
			if s.Options.Architect == nil {
				continue
			}
			if err := s.QueueChangeTurn(ctx, wt, g, c, architectRole, architectAgent, DeltaDraftTool); err != nil {
				return err
			}
		case "debating":
			if s.Options.Committee == nil {
				continue
			}
			for n := 1; n <= c.Reviewers; n++ {
				member := committeeAgent(n)
				if slices.ContainsFunc(c.Debate, func(a DeltaAssessment) bool {
					return a.Member == member
				}) {
					continue
				}
				if err := s.QueueChangeTurn(ctx, wt, g, c, committeeRole, member, DeltaReviewTool); err != nil {
					return err
				}
			}
			if len(c.Debate) == c.Reviewers {
				c.State = "answering"
				if err := s.SaveChange(ctx, wt, &c); err != nil {
					return err
				}
			}
		case "answering":
			if s.Options.Architect == nil {
				continue
			}
			if err := s.QueueChangeTurn(ctx, wt, g, c, architectRole, architectAgent, DeltaReplyTool); err != nil {
				return err
			}
		case "presenting":
			if err := s.ChangeNotice(ctx, wt, c); err != nil {
				return err
			}
			c.State = "presented"
			if err := s.SaveChange(ctx, wt, &c); err != nil {
				return err
			}
		case "approved", "applying", "releasing":
			if err := s.Step("change-before-application-lock"); err != nil {
				return err
			}
			if err := s.ApplyChange(ctx, wt, g, &c); err != nil {
				if !errors.Is(err, trace.ErrMemberRetired) {
					return err
				}
				if err := s.HoldChange(ctx, wt, &c, err.Error()); err != nil {
					return err
				}
			}
		case "failed":
			if c.FailureSettled {
				continue
			}
			if err := s.SettleChangeRequester(ctx, c); err != nil && !errors.Is(err, trace.ErrMemberRetired) {
				return err
			}
			if err := s.ChangeFailureNotice(ctx, wt, g, c); err != nil {
				return err
			}
			c.FailureSettled = true
			if err := s.SaveChange(ctx, wt, &c); err != nil {
				return err
			}
		case "rejected", "applied":
			if err := s.SettleChangeRequester(ctx, c); err != nil {
				if !errors.Is(err, trace.ErrMemberRetired) {
					return err
				}
				if err := s.HoldChange(ctx, wt, &c, err.Error()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// DeltaSystemPrompt is the role prompt of a delta debate turn. The web
// guidance a turn holding Web is told is added when the turn runs.
func DeltaSystemPrompt(role string) string {
	if role == architectRole {
		return fmt.Sprintf(`You are the architect considering a delta amendment to a ratified workgroup spec.

## Purpose and setting
The owner or a member asked to change the ratified project spec, the shared behaviour, contracts and graph that span the workgroup's projects. You draft the changed text, and later answer the committee's assessment of it. Hold approved text fixed except for the requested change, because the owner ratified it and only the requested delta is up for decision.

## Tools and gates
- %[1]s records the full proposed project spec, and %[2]s records your answer to the committee's assessment. Each records durably and approves nothing: the owner decides the delta.
- You never edit code, use version control, publish or approve. No owner gate is available in this turn.

## Your part of the state machine
A delta is drafted, assessed by the committee, answered by you and then presented to the owner. A turn handles one of those steps, and its prompt says which.

## Output format
The tool call the turn's prompt names, and nothing the service does not record.

## When you are done
Finish the whole task of the turn: call the tool the prompt names with a complete input, correct it if it is refused, and end your turn.`, DeltaDraftTool, DeltaReplyTool)
	}
	return fmt.Sprintf(`You are a member of the committee considering a delta amendment to a ratified workgroup spec.

## Purpose and setting
The owner or a member asked to change the ratified project spec, the shared behaviour, contracts and graph that span the workgroup's projects. You assess the delta only, because approved text outside it was already ratified. Your assessment goes to the architect, who answers it, and then to the owner, who decides.

## Tools and gates
- %[1]s records your objections and recommendations about the delta. It records durably and approves nothing.
- You never edit code, use version control, publish or approve. No owner gate is available in this turn.

## Your part of the state machine
A delta is drafted by the architect, assessed by each committee member, answered by the architect and presented to the owner. Your turn is the assessment.

## Output format
One %[1]s call: a list of objections, each a statement the architect can answer, and a note.

## When you are done
You are done when %[1]s has recorded your assessment, with an empty objections list when you have none. End your turn then.`, DeltaReviewTool)
}

func (s *Controller) RunChangeTurn(ctx context.Context, wt *trace.Repository, c WorkgroupChange, role, agent, tool string) error {
	turn := DeltaTurnID(c, agent, tool)
	if _, err := wt.Thread(c.Host, agent); errors.Is(err, os.ErrNotExist) {
		if err := wt.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: agent, Revision: 1, Project: wt.Project(), Workstream: c.Host, At: s.Now(), Actor: amendmentActor, Cause: WorkgroupChangeDocument(c.ID)}, Role: role, ThreadID: agent}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	t, err := wt.Thread(c.Host, agent)
	if err != nil {
		return err
	}
	if t.Identity.Role != role {
		return errors.New("delta thread has a different role")
	}
	identity := t.Identity.ThreadID
	for _, q := range t.Turns {
		if q.Request.TurnID == turn && q.Claim != nil && q.Response == nil && q.CompletedAt.IsZero() {
			current, err := wt.CurrentTurnClaim(c.Host, q.Claim)
			if err != nil {
				return err
			}
			if !current {
				if err := s.RecoverThread(ctx, s.Current(), wt, c.Host, t); err != nil {
					return err
				}
			}
		}
	}
	if !queuedTurn(t, turn) {
		profile, _, err := s.RoleExecution(s.Current(), role)
		if err != nil {
			return err
		}
		prompt := `Debate only the requested delta against before.json, the ratified project spec and slices. Text and assignments the delta does not touch stay as they are.

## Your view
- change.json: the request, the proposed text, the impact, the committee objections and the prior answers.
- before.json: the ratified project spec and slices.
- charters.md: every member's charter. Check shared behaviour and contracts against all of them, because each member charter governs its own slice and all govern the shared text.

## What to do
- Deliver a full proposed project spec with deliver_delta, keeping unchanged sections byte-for-byte. Every member needs a slice, and changed and new sections need units. A member marked retired in change.json is archived: its slice is frozen and stays exactly as approved.
- With review_delta, record objections and recommendations about the delta only.
- With answer_delta, answer the committee's delta objections.
`
		_, err = wt.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: wt.Project(), Workstream: c.Host, At: s.Now(), Actor: amendmentActor, Cause: WorkgroupChangeDocument(c.ID)}, AgentID: agent, ThreadID: identity, TurnID: turn, Profile: profile, SystemPrompt: DeltaSystemPrompt(role), Prompt: prompt + acceptanceDraftInstructions})
		if err != nil {
			return err
		}
	}
	directory := filepath.Join(s.Current().Root.String(), "workgroup-changes", string(c.Workgroup), c.ID, turn)
	turns, err := s.ChangeTurns(ctx, wt, c, role, agent, tool, identity, directory)
	if err != nil {
		return err
	}
	dispatcher := thread.Dispatcher{Runner: s.ThreadRunner(s.Current(), wt, turns, s.Now), Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
		session := filepath.Join(directory, "session")
		return coreadapter.PreparedTurn{SessionDirectory: session}, os.MkdirAll(session, 0700)
	}}
	op, err := thread.TurnOperation(wt.Project(), turn, thread.TurnInput{Workstream: c.Host, Agent: agent, Turn: turn})
	if err != nil {
		return err
	}
	_, err = dispatcher.Apply(ctx, op)
	if err != nil {
		return err
	}
	lock := s.GroupApprovalLock(c.Workgroup)
	lock.Lock()
	defer lock.Unlock()
	changes, err := ReadChanges(wt, c.Host)
	if err != nil {
		return err
	}
	for _, latest := range changes {
		if latest.ID != c.ID {
			continue
		}
		if NameRecorded(latest, agent, tool) {
			return nil
		}
		t, err := wt.Thread(c.Host, agent)
		if err != nil {
			return err
		}
		for _, q := range t.Turns {
			if q.Request.TurnID == turn && !q.CompletedAt.IsZero() {
				latest.State, latest.Failure = "failed", "The delta turn finished without recording "+tool+". Request a new change after addressing its failure."
				if q.Response != nil && q.Response.Failure != "" {
					latest.Failure += " " + q.Response.Failure
				}
				return s.SaveChange(ctx, wt, &latest)
			}
		}
	}
	return nil
}
func NameRecorded(c WorkgroupChange, agent, tool string) bool {
	switch tool {
	case DeltaDraftTool:
		return c.State != "drafting"
	case DeltaReplyTool:
		return c.State != "answering"
	case DeltaReviewTool:
		return slices.ContainsFunc(c.Debate, func(a DeltaAssessment) bool {
			return a.Member == agent
		})
	}
	return false
}
func (s *Controller) DeltaTool(wt *trace.Repository, c WorkgroupChange, agent, name string) coreadapter.Tool {
	group := c.Workgroup
	schema := `{"type":"object"}`
	switch name {
	case DeltaReviewTool:
		schema = `{"type":"object","properties":{"objections":{"type":"array","items":{"type":"string"}},"note":{"type":"string"}},"required":["objections","note"],"additionalProperties":false}`
	case DeltaReplyTool:
		schema = `{"type":"object","properties":{"reply":{"type":"string"}},"required":["reply"],"additionalProperties":false}`
	}
	return coreadapter.Tool{Name: name, Effect: coreadapter.ToolMemory, Description: DeltaToolDescription(name), InputSchema: json.RawMessage(schema), Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		lock := s.GroupApprovalLock(group)
		lock.Lock()
		defer lock.Unlock()
		changes, err := ReadChanges(wt, c.Host)
		if err != nil {
			return nil, err
		}
		for _, latest := range changes {
			if latest.ID == c.ID {
				c = latest
			}
		}
		if NameRecorded(c, agent, name) {
			return json.RawMessage(`{"recorded":true}`), nil
		}
		switch name {
		case DeltaDraftTool:
			if c.State != "drafting" {
				return json.RawMessage(`{"recorded":true}`), nil
			}
			var after trace.ProjectSpec
			if err := DecodeToolInput(raw, &after); err != nil {
				return nil, err
			}
			members := slices.Clone(c.Members)
			for n := range members {
				if members[n].Retired {
					continue
				}
				if a, retired := s.ArchivedWorkstream(string(members[n].Workstream)); retired {
					if a.Workgroup != c.Workgroup {
						return nil, errors.New("retired delta member identity differs from its workgroup")
					}
					members[n].Retired = true
				}
			}
			carried := false
			for _, m := range members {
				if _, found := trace.SliceFor(after, m.Workstream); !found && m.Retired {
					if sl, ok := trace.SliceFor(c.Before, m.Workstream); ok {
						after.Slices, carried = append(after.Slices, sl), true
					}
				}
			}
			if carried {
				order := func(sl trace.Slice) int {
					return slices.IndexFunc(c.Before.Slices, func(b trace.Slice) bool { return b.Workstream == sl.Workstream })
				}
				slices.SortStableFunc(after.Slices, func(x, y trace.Slice) int { return order(x) - order(y) })
			}
			if problems := SharedCharterProblems(ctx, wt, c.Origin, after); len(problems) > 0 {
				return priorityRefusal(strings.Join(problems, "; "))
			}
			impact, err := amendment.ClassifyImpact(c.ID, c.Before, after, members)
			if err != nil {
				return priorityRefusal(err.Error())
			}
			if SameProjectSpec(c.Before, after) {
				return priorityRefusal("the proposal changes no approved text")
			}
			for _, sl := range after.Slices {
				if slices.ContainsFunc(members, func(m amendment.Member) bool { return m.Workstream == sl.Workstream && m.Retired }) {
					continue
				}
				_, _, repo, api := s.ConversationTrace(string(sl.Workstream))
				if api != nil {
					return nil, errors.New(api.Message)
				}
				draft := &drafter{S: s.Intake().DrafterDependencies(), Repository: repo}
				problems, err := draft.Validate(trace.Document{Header: trace.Header{Workstream: sl.Workstream}, Content: sl.Spec}, trace.Document{Content: sl.Plan})
				if err != nil {
					return nil, err
				}
				before, _ := trace.SliceFor(c.Before, sl.Workstream)
				problems = append(problems, plan.CheckAcceptanceProblems(sl.Spec, sl.Plan, before.Spec, before.Plan)...)
				if len(problems) > 0 {
					return priorityRefusal(fmt.Sprint(problems))
				}
			}
			c.After, c.Impact, c.State = after, impact, "debating"
		case DeltaReviewTool:
			if c.State != "debating" {
				return priorityRefusal("the delta is not in debate")
			}
			var in DeltaAssessment
			if err := DecodeToolInput(raw, &in); err != nil {
				return nil, err
			}
			in.Member = agent
			if slices.ContainsFunc(c.Debate, func(d DeltaAssessment) bool {
				return d.Member == agent
			}) {
				return json.RawMessage(`{"recorded":true}`), nil
			}
			c.Debate = append(c.Debate, in)
		case DeltaReplyTool:
			if c.State != "answering" {
				return priorityRefusal("the delta is not awaiting an answer")
			}
			var in struct {
				Reply string `json:"reply"`
			}
			if err := DecodeToolInput(raw, &in); err != nil {
				return nil, err
			}
			if in.Reply == "" {
				return priorityRefusal("answer the committee's delta assessment")
			}
			c.Reply, c.State = in.Reply, "presenting"
		}
		if err := s.SaveChange(ctx, wt, &c); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"recorded":true}`), nil
	}}
}
func SameProjectSpec(a, b trace.ProjectSpec) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// RevisionSlice keeps the approved proposal separate from the mechanical
// revision identities needed to preserve historical completed unit evidence.
func RevisionSlice(c WorkgroupChange, row amendment.StreamImpact) (trace.Slice, error) {
	old, _ := trace.SliceFor(c.Before, row.Workstream)
	now, _ := trace.SliceFor(c.After, row.Workstream)
	a, err := plan.Parse([]byte(old.Plan))
	if err != nil {
		return now, err
	}
	b, err := plan.Parse([]byte(now.Plan))
	if err != nil {
		return now, err
	}
	p := amendment.RevisionPlan(c.ID, a, b, row)
	data, err := plan.Encode(p)
	if err == nil {
		seen := map[string]bool{}
		for _, u := range p.Units {
			if seen[u.ID] {
				return now, fmt.Errorf("revision identity collides with unit %s", u.ID)
			}
			seen[u.ID] = true
		}
	}
	now.Plan = string(data)
	return now, err
}

// ChangeTurns is the isolated turn path of one delta debate turn: the
// architect's deliver_delta and answer_delta, or the committee's review_delta.
// Its view is read-only staged files, its service tools are file_read and the
// turn's own tool, and it holds the backend's web tools through the same
// grants as the architect's and the committee's other planning turns. It
// holds no write, execute, Network or VCS capability.
func (s *Controller) ChangeTurns(ctx context.Context, wt *trace.Repository, c WorkgroupChange, role, agent, tool, identity, directory string) (*isolation.Turns, error) {
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if role == committeeRole {
		if s.Options.Committee == nil {
			return nil, errors.New("delta committee runner unavailable")
		}
		engine, hosts = s.Options.Committee.Engine, s.Options.Committee.Hosts
	} else {
		if s.Options.Architect == nil {
			return nil, errors.New("delta architect runner unavailable")
		}
		engine, hosts = s.Options.Architect.Engine, s.Options.Architect.Hosts
	}
	views := filepath.Join(directory, "views")
	if err := os.MkdirAll(views, 0700); err != nil {
		return nil, err
	}
	grant := intake.ArchitectGrant("file_read", tool)
	if role == committeeRole {
		grant = intake.CommitteeDebateGrant("file_read", tool)
	}
	turns := &isolation.Turns{Workspaces: stagedWorkspaces{}, Views: isolation.Views{Directory: views}, Engine: engine, Hosts: hosts, Grants: map[string]coreadapter.Capabilities{role: grant}, Select: func(_ context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
		if scope.Workstream != string(c.Host) || scope.Role != role {
			return isolation.Selection{}, errors.New("delta scope denied")
		}
		workspace := filepath.Join(directory, "workspace")
		if err := os.MkdirAll(workspace, 0700); err != nil {
			return isolation.Selection{}, err
		}
		charters, err := WorkgroupCharterText(ctx, wt, c.Host)
		if err != nil {
			return isolation.Selection{}, err
		}
		before, _ := json.MarshalIndent(c.Before, "", "  ")
		change, _ := json.MarshalIndent(c.shown(), "", "  ")
		for name, data := range map[string][]byte{"before.json": before, "change.json": change, "charters.md": []byte(charters)} {
			if err := os.WriteFile(filepath.Join(workspace, name), data, 0600); err != nil {
				return isolation.Selection{}, err
			}
		}
		_, settings, err := s.RoleExecution(s.Current(), role)
		return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: []string{"before.json", "change.json", "charters.md"}, Execution: settings}, err
	}, Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
		if scope.Workstream != string(c.Host) || scope.Thread != identity {
			return nil, errors.New("delta scope denied")
		}
		return []coreadapter.Tool{s.DeltaTool(wt, c, agent, tool)}, nil
	}}
	return turns, nil
}

// DeltaToolDescription describes the delta tool of the given name to the
// agent that calls it.
func DeltaToolDescription(name string) string {
	switch name {
	case DeltaDraftTool:
		return "Record the architect's full proposed project spec for the delta: behavior, contracts, graph and a slice for every member. Keep sections the delta does not change byte-for-byte. The result is recorded true, or a refusal that names the problem with the proposal (an unresolvable charter citation, a spec or plan that fails validation, or no change to approved text); correct it and call again. This records the proposal and approves nothing: the committee assesses it and the owner decides."
	case DeltaReviewTool:
		return "Record your assessment of the delta: objections, a list of statements about the delta that the architect can answer, empty when you have none, and note, your recommendation for the owner. Call it once, after you have read the proposal against the ratified spec and every member charter. The result is recorded true, or a refusal that says why; correct it and call again. This records your assessment and approves nothing."
	case DeltaReplyTool:
		return "Record the architect's answer to the committee's assessment of the delta. reply is your answer to each objection, naming what you change or why you do not. The result is recorded true, or a refusal that says why; correct it and call again. This records the answer and approves nothing: the owner decides the delta."
	}
	return "Record a delta turn's contribution durably. This approves nothing."
}
