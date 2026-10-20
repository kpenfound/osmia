package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const deltaDraftTool = "deliver_delta"
const deltaReviewTool = "review_delta"
const deltaReplyTool = "answer_delta"
const deltaTurnAction = "workgroup-delta-turn"

type deltaTurnInput struct {
	Address config.WorkstreamID `json:"address"`
	Change  string              `json:"change"`
	Role    string              `json:"role"`
	Agent   string              `json:"agent"`
	Tool    string              `json:"tool"`
}

// deltaRunner resolves the authoritative group journal per operation. The
// real member's controller owns the operation; the turn lives with the group.
type deltaRunner struct{ s *Service }

func decodeDeltaTurn(op coreadapter.Operation) (deltaTurnInput, error) {
	var in deltaTurnInput
	if op.Boundary != coreadapter.RunnerBoundary || op.Action != deltaTurnAction {
		return in, errors.New("unsupported delta operation")
	}
	if err := decodeToolInput(op.Input, &in); err != nil {
		return in, err
	}
	if config.CheckWorkstreamIDs(in.Address) != nil || in.Change == "" || in.Agent == "" ||
		!(in.Role == architectRole && in.Agent == architectAgent && (in.Tool == deltaDraftTool || in.Tool == deltaReplyTool) || in.Role == committeeRole && in.Tool == deltaReviewTool) {
		return in, errors.New("invalid delta turn scope")
	}
	return in, nil
}

func deltaTurnID(c WorkgroupChange, agent, tool string) string {
	return changeIdentity("change-" + c.ID + "-" + tool + "-" + agent)
}

// load uses only the group journal when the owning controller is retired.
// Retirement cancels that member's dispatch intent, never the shared decision.
func (d deltaRunner) load(ctx context.Context, in deltaTurnInput) (*trace.Repository, WorkgroupChange, bool, func(), error) {
	noop := func() {}
	var wt *trace.Repository
	var g trace.Workgroup
	var close func()
	var err error
	retired := false
	if _, archived := d.s.archivedWorkstream(string(in.Address)); !archived {
		wt, g, _, close, err = d.s.changeContext(ctx, string(in.Address))
		if err != nil {
			if _, archived = d.s.archivedWorkstream(string(in.Address)); !archived {
				return nil, WorkgroupChange{}, false, noop, err
			}
		}
	}
	if wt == nil {
		a, archived := d.s.archivedWorkstream(string(in.Address))
		if !archived || a.Workgroup == "" {
			return nil, WorkgroupChange{}, false, noop, errors.New("delta controller has no shared retirement authority")
		}
		wt, err = trace.OpenWorkgroupAuthority(d.s.current().Root, a.Workgroup)
		if err != nil {
			if wt != nil {
				wt.Close()
			}
			return nil, WorkgroupChange{}, false, noop, err
		}
		close = func() { wt.Close() }
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
				members, approved, memberErr := d.s.workgroupMembers(ctx, g)
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
	changes, err := readChanges(wt, g.Members[0])
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

func deltaTurnState(wt *trace.Repository, c WorkgroupChange, in deltaTurnInput) (running, captured bool, err error) {
	t, err := wt.Thread(c.Host, in.Agent)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, false, err
	}
	for _, q := range t.Turns {
		if q.Request.TurnID != deltaTurnID(c, in.Agent, in.Tool) {
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

func retiredDeltaResult(in deltaTurnInput) coreadapter.OperationResult {
	return coreadapter.OperationResult{Outcome: "cancelled", Evidence: fmt.Sprintf("owner archived delta controller %s; shared decision %s remains in its authoritative journal", in.Address, in.Change)}
}

func (d deltaRunner) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := decodeDeltaTurn(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	wt, c, retired, close, err := d.load(ctx, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	defer close()
	running, captured, err := deltaTurnState(wt, c, in)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if running {
		return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "delta turn is running"}, nil
	}
	if retired && !captured {
		result := retiredDeltaResult(in)
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: result.Evidence, Result: &result}, nil
	}
	// Apply also reconciles completion and missing-tool failures. A recorded
	// tool contribution alone does not replace the owned turn response.
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "delta turn requires reconciliation"}, nil
}

func (d deltaRunner) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := decodeDeltaTurn(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	wt, c, retired, close, err := d.load(ctx, in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	defer close()
	if retired {
		running, captured, err := deltaTurnState(wt, c, in)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		if running {
			return coreadapter.OperationResult{}, errors.New("shared delta turn is still running; retired controller remains held")
		}
		if !captured {
			return retiredDeltaResult(in), nil
		}
		// Captured shared responses reconcile through the ordinary turn path.
		// Archiving a controller cannot replace their recorded outcome.
	}
	ctx, release := d.s.stoppable(ctx, wt.Project(), c.Host)
	defer release()
	if err := d.s.runChangeTurn(ctx, wt, c, in.Role, in.Agent, in.Tool); err != nil {
		return coreadapter.OperationResult{}, err
	}
	changes, err := readChanges(wt, c.Host)
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

// queueChangeTurn records intent only; the controller's runner executes it
// outside the schedule stage, with the ordinary shutdown/retry lifecycle.
func (s *Service) queueChangeTurn(ctx context.Context, wt *trace.Repository, g trace.Workgroup, c WorkgroupChange, role, agent, tool string) error {
	address := s.changeLeader(s.changeMembers(g, wt))
	if address == "" {
		return errors.New("workgroup has no live controller for its pending delta turn")
	}
	_, _, repo, api := s.conversationTrace(string(address))
	if api != nil {
		return errors.New(api.Message)
	}
	id := deltaTurnID(c, agent, tool)
	state, err := repo.Workflow(address, id)
	if err != nil || state.Value != "" {
		return err
	}
	data, err := json.Marshal(deltaTurnInput{Address: address, Change: c.ID, Role: role, Agent: agent, Tool: tool})
	if err != nil {
		return err
	}
	event := trace.EventID(id, "run")
	op := coreadapter.Operation{ID: trace.OperationID(repo.Project(), address, event), Boundary: coreadapter.RunnerBoundary, Action: deltaTurnAction, Input: data}
	_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: address, At: s.now(), Actor: amendmentActor, Cause: workgroupChangeDocument(c.ID)}, Subject: id, To: "queued", Reason: "debate only the requested delta"},
		Events:     []trace.Event{{ID: event, Kind: "delta", Body: "Debate the requested amendment delta", Operation: &op}},
	})
	if err != nil {
		return err
	}
	return s.step("change-delta-queued")
}

func decodeToolInput(raw json.RawMessage, v any) error {
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

// changeLeader selects a controller that still has a live conversation.
// Archive records remain authoritative before and after trace cleanup.
func (s *Service) changeLeader(members []config.WorkstreamID) config.WorkstreamID {
	var leader config.WorkstreamID
	for _, member := range members {
		if _, archived := s.archivedWorkstream(string(member)); archived {
			continue
		}
		if leader == "" || member < leader {
			leader = member
		}
	}
	return leader
}

func (s *Service) changePass(ctx context.Context, repo *trace.Repository) error {
	streams, err := repo.Workstreams()
	if err != nil {
		return err
	}
	seen := map[config.WorkgroupID]bool{}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Removal stops new group work while the controller drains its
		// admitted turns. The retained trace is still owned by that runtime.
		if !s.current().Active(repo.Project()) {
			return nil
		}
		// Archive records are authoritative even while cleanup still retains
		// the terminal member's trace. Its conversation is no longer live.
		if _, archived := s.archivedWorkstream(string(stream)); archived {
			continue
		}
		if kind, err := repo.Kind(stream); err != nil {
			return err
		} else if kind == trace.SessionKind || stream == librarianWorkstream(repo.Project()) {
			continue
		}
		groups, _, err := trace.Workgroups(repo)
		if err != nil {
			return err
		}
		_, local := trace.WorkgroupOf(groups, stream)
		cross, err := repo.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return err
		}
		if !local && cross == "" {
			continue
		}
		if cross != "" {
			_, group, _, err := repo.CrossRepoProjectSpec(cross)
			if err != nil {
				return err
			}
			// A retained conversation source locator can point at the group
			// without being a member eligible to drive its change journal.
			if group.ID == cross && len(group.Members) > 0 && !slices.Contains(group.Members, stream) {
				continue
			}
			if group.ID == cross && len(group.Members) > 1 {
				retired, err := s.workgroupFullyRetired(ctx, group)
				if err != nil {
					return err
				}
				if retired {
					// The adopted source remains a locator after all real members
					// retire. Authority is retained, but grants no member effects.
					s.log().Warn("workgroup changes remain held", "workgroup", cross, "member", group.Members[1], "error", "no live member remains to reconcile pending project decisions")
					continue
				}
			}
		}
		wt, g, _, close, err := s.changeContext(ctx, string(stream))
		if errors.Is(err, errWorkgroupJoining) {
			// Production allocation records member links before adoption and
			// the fixed shared bindings finish. Admission stays fenced.
			continue
		}
		if err != nil {
			if canceled := ctx.Err(); canceled != nil {
				return canceled
			}
			if errors.Is(err, errNoLiveWorkgroupContext) {
				// The final live member can archive after the initial read.
				s.log().Warn("workgroup changes remain held", "workgroup", cross, "error", err.Error())
				continue
			}
			if !s.current().Active(repo.Project()) {
				return nil
			}
			// Retirement can race the initial archive read and routing.
			if _, archived := s.archivedWorkstream(string(stream)); archived {
				continue
			}
			return err
		}
		members := s.changeMembers(g, wt)
		if seen[g.ID] || len(members) == 0 || s.changeLeader(members) != stream {
			close()
			continue
		}
		seen[g.ID] = true
		err = s.advanceChanges(ctx, wt, g)
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

func (s *Service) advanceChanges(ctx context.Context, wt *trace.Repository, g trace.Workgroup) error {
	// An idle pass takes no group lock. Re-read pending work under the lock
	// so owner decisions and application consume the same durable revision.
	requests, _, err := trace.ProjectAmendments(wt)
	if err != nil {
		return err
	}
	changes, err := readChanges(wt, g.Members[0])
	if err != nil {
		return err
	}
	if len(requests) == 0 && !slices.ContainsFunc(changes, func(c WorkgroupChange) bool {
		return c.State != "applied" && c.State != "rejected" && c.State != "failed"
	}) {
		return nil
	}
	lock := s.groupApprovalLock(g.ID)
	// Schedule hooks already hold the operation lock; never wait for an
	// owner action while acquiring group coordination.
	if !lock.TryLock() {
		return nil
	}
	defer lock.Unlock()
	requests, _, err = trace.ProjectAmendments(wt)
	if err != nil {
		return err
	}
	changes, err = readChanges(wt, g.Members[0])
	if err != nil {
		return err
	}
	for _, r := range requests {
		if r.Workgroup != g.ID {
			continue
		}
		id := "contract-" + r.ID
		if slices.ContainsFunc(changes, func(c WorkgroupChange) bool { return c.ID == id }) {
			continue
		}
		if slices.ContainsFunc(changes, func(c WorkgroupChange) bool { return c.State != "applied" && c.State != "rejected" }) {
			break
		}
		before, members, err := s.snapshotChange(g, wt)
		if errors.Is(err, trace.ErrMemberRetired) {
			s.log().Warn("project amendment remains held", "workgroup", g.ID, "request", r.ID, "error", err.Error())
			break
		}
		if err != nil {
			return err
		}
		c := WorkgroupChange{ID: id, Workgroup: g.ID, Host: g.Members[0], Origin: r.Origin, Request: ChangeRequest{Key: id, Kind: "amend", Scope: "workgroup", Change: r.Change + "\n" + r.Reason}, State: "drafting", Before: before, Members: members, Reviewers: s.current().Capacity.Committee, Followups: map[config.WorkstreamID]config.WorkstreamID{}}
		c.Requester = &r
		if err := s.saveChange(ctx, wt, &c); err != nil {
			return err
		}
		changes = append(changes, c)
	}
	for _, c := range changes {
		if c.Request.Kind == "amend" && c.Reviewers == 0 && (c.State == "drafting" || c.State == "debating" || c.State == "answering") {
			c.Reviewers = s.current().Capacity.Committee
			if err := s.saveChange(ctx, wt, &c); err != nil {
				return err
			}
		}
		switch c.State {
		case "drafting":
			if s.options.Architect == nil {
				continue
			}
			if err := s.queueChangeTurn(ctx, wt, g, c, architectRole, architectAgent, deltaDraftTool); err != nil {
				return err
			}
		case "debating":
			if s.options.Committee == nil {
				continue
			}
			for n := 1; n <= c.Reviewers; n++ {
				member := committeeAgent(n)
				if slices.ContainsFunc(c.Debate, func(a deltaAssessment) bool { return a.Member == member }) {
					continue
				}
				if err := s.queueChangeTurn(ctx, wt, g, c, committeeRole, member, deltaReviewTool); err != nil {
					return err
				}
			}
			if len(c.Debate) == c.Reviewers {
				c.State = "answering"
				if err := s.saveChange(ctx, wt, &c); err != nil {
					return err
				}
			}
		case "answering":
			if s.options.Architect == nil {
				continue
			}
			if err := s.queueChangeTurn(ctx, wt, g, c, architectRole, architectAgent, deltaReplyTool); err != nil {
				return err
			}
		case "presenting":
			if err := s.changeNotice(ctx, wt, c); err != nil {
				return err
			}
			c.State = "presented"
			if err := s.saveChange(ctx, wt, &c); err != nil {
				return err
			}
		case "approved", "applying", "releasing":
			if err := s.applyChange(ctx, wt, g, &c); err != nil {
				if !errors.Is(err, trace.ErrMemberRetired) {
					return err
				}
				if err := s.holdChange(ctx, wt, &c, err.Error()); err != nil {
					return err
				}
			}
		case "rejected", "applied":
			if err := s.settleChangeRequester(ctx, c); err != nil {
				if !errors.Is(err, trace.ErrMemberRetired) {
					return err
				}
				if err := s.holdChange(ctx, wt, &c, err.Error()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Service) runChangeTurn(ctx context.Context, wt *trace.Repository, c WorkgroupChange, role, agent, tool string) error {
	turn := deltaTurnID(c, agent, tool)
	if _, err := wt.Thread(c.Host, agent); errors.Is(err, os.ErrNotExist) {
		if err := wt.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: agent, Revision: 1, Project: wt.Project(), Workstream: c.Host, At: s.now(), Actor: amendmentActor, Cause: workgroupChangeDocument(c.ID)}, Role: role, ThreadID: agent}); err != nil {
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
				if err := s.recoverThread(ctx, s.current(), wt, c.Host, t); err != nil {
					return err
				}
			}
		}
	}
	if !queuedTurn(t, turn) {
		profile, _, err := s.roleExecution(s.current(), role)
		if err != nil {
			return err
		}
		prompt := "Debate only the requested delta against before.json, the ratified project spec and slices. Unchanged assignments and intent are left alone. Read change.json for the request, proposed text, impact, committee objections and prior answers. Deliver a full proposed project spec with deliver_delta, preserving unchanged sections byte-for-byte. Every member needs a slice; changed and new sections need units. With review_delta, record objections and recommendations about the delta only. With answer_delta, answer the committee's delta objections. No owner gate is available in this turn."
		_, err = wt.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: wt.Project(), Workstream: c.Host, At: s.now(), Actor: amendmentActor, Cause: workgroupChangeDocument(c.ID)}, AgentID: agent, ThreadID: identity, TurnID: turn, Profile: profile, SystemPrompt: "You are the " + role + " considering a delta amendment. Hold approved text fixed except for the requested change; never edit code, use version control, publish or approve.", Prompt: prompt + acceptanceDraftInstructions})
		if err != nil {
			return err
		}
	}
	directory := filepath.Join(s.current().Root.String(), "workgroup-changes", string(c.Workgroup), c.ID, turn)
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if role == committeeRole {
		if s.options.Committee == nil {
			return errors.New("delta committee runner unavailable")
		}
		engine, hosts = s.options.Committee.Engine, s.options.Committee.Hosts
	} else {
		if s.options.Architect == nil {
			return errors.New("delta architect runner unavailable")
		}
		engine, hosts = s.options.Architect.Engine, s.options.Architect.Hosts
	}
	views := filepath.Join(directory, "views")
	if err := os.MkdirAll(views, 0700); err != nil {
		return err
	}
	turns := &isolation.Turns{Workspaces: stagedWorkspaces{}, Views: isolation.Views{Directory: views}, Engine: engine, Hosts: hosts,
		Grants: map[string]coreadapter.Capabilities{role: {Tools: []string{"file_read", tool}}},
		Select: func(_ context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			if scope.Workstream != string(c.Host) || scope.Role != role {
				return isolation.Selection{}, errors.New("delta scope denied")
			}
			workspace := filepath.Join(directory, "workspace")
			if err := os.MkdirAll(workspace, 0700); err != nil {
				return isolation.Selection{}, err
			}
			before, _ := json.MarshalIndent(c.Before, "", "  ")
			change, _ := json.MarshalIndent(c, "", "  ")
			for name, data := range map[string][]byte{"before.json": before, "change.json": change} {
				if err := os.WriteFile(filepath.Join(workspace, name), data, 0600); err != nil {
					return isolation.Selection{}, err
				}
			}
			_, settings, err := s.roleExecution(s.current(), role)
			return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: []string{"before.json", "change.json"}, Execution: settings}, err
		},
		Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Workstream != string(c.Host) || scope.Thread != identity {
				return nil, errors.New("delta scope denied")
			}
			return []coreadapter.Tool{s.deltaTool(wt, c, agent, tool)}, nil
		},
	}
	dispatcher := thread.Dispatcher{Runner: s.threadRunner(s.current(), wt, turns, s.now), Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
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
	lock := s.groupApprovalLock(c.Workgroup)
	lock.Lock()
	defer lock.Unlock()
	changes, err := readChanges(wt, c.Host)
	if err != nil {
		return err
	}
	for _, latest := range changes {
		if latest.ID != c.ID {
			continue
		}
		if nameRecorded(latest, agent, tool) {
			return nil
		}
		if tool == deltaDraftTool {
			for _, member := range latest.Members {
				if a, retired := s.archivedWorkstream(string(member.Workstream)); retired {
					if a.Workgroup != c.Workgroup {
						return errors.New("retired delta member identity differs from its workgroup")
					}
					// A captured turn without a draft cannot validate unavailable
					// member history. Keep its response and shared decision pending.
					return fmt.Errorf("%w: workgroup %s delta draft remains pending; member %s has unavailable validation history", trace.ErrMemberRetired, c.Workgroup, member.Workstream)
				}
			}
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
				return s.saveChange(ctx, wt, &latest)
			}
		}
	}
	return nil
}

func nameRecorded(c WorkgroupChange, agent, tool string) bool {
	switch tool {
	case deltaDraftTool:
		return c.State != "drafting"
	case deltaReplyTool:
		return c.State != "answering"
	case deltaReviewTool:
		return slices.ContainsFunc(c.Debate, func(a deltaAssessment) bool { return a.Member == agent })
	}
	return false
}

func (s *Service) deltaTool(wt *trace.Repository, c WorkgroupChange, agent, name string) coreadapter.Tool {
	group := c.Workgroup
	schema := `{"type":"object"}`
	switch name {
	case deltaReviewTool:
		schema = `{"type":"object","properties":{"objections":{"type":"array","items":{"type":"string"}},"note":{"type":"string"}},"required":["objections","note"],"additionalProperties":false}`
	case deltaReplyTool:
		schema = `{"type":"object","properties":{"reply":{"type":"string"}},"required":["reply"],"additionalProperties":false}`
	}
	return coreadapter.Tool{Name: name, Effect: coreadapter.ToolMemory, Description: "Record the delta proposal, committee assessment or architect answer durably. This approves nothing.", InputSchema: json.RawMessage(schema), Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		lock := s.groupApprovalLock(group)
		lock.Lock()
		defer lock.Unlock()
		changes, err := readChanges(wt, c.Host)
		if err != nil {
			return nil, err
		}
		for _, latest := range changes {
			if latest.ID == c.ID {
				c = latest
			}
		}
		// A turn may recover after its contribution advanced the journal.
		// Repeating that contribution must succeed even in the next stage.
		if nameRecorded(c, agent, name) {
			return json.RawMessage(`{"recorded":true}`), nil
		}
		switch name {
		case deltaDraftTool:
			if c.State != "drafting" {
				return json.RawMessage(`{"recorded":true}`), nil
			}
			var after trace.ProjectSpec
			if err := decodeToolInput(raw, &after); err != nil {
				return nil, err
			}
			impact, err := amendment.ClassifyImpact(c.ID, c.Before, after, c.Members)
			if err != nil {
				return priorityRefusal(err.Error())
			}
			if sameProjectSpec(c.Before, after) {
				return priorityRefusal("the proposal changes no approved text")
			}
			for _, sl := range after.Slices {
				if a, retired := s.archivedWorkstream(string(sl.Workstream)); retired {
					if a.Workgroup != c.Workgroup {
						return nil, errors.New("retired delta slice identity differs from its workgroup")
					}
					// The original ratified slice already passed validation. Its
					// exact pinned text grants no access or effects on retired history.
					before, found := trace.SliceFor(c.Before, sl.Workstream)
					if found && reflect.DeepEqual(before, sl) {
						continue
					}
					return nil, fmt.Errorf("%w: workgroup %s delta requires unavailable validation history for member %s", trace.ErrMemberRetired, c.Workgroup, sl.Workstream)
				}
				_, _, repo, api := s.conversationTrace(string(sl.Workstream))
				if api != nil {
					return nil, errors.New(api.Message)
				}
				draft := &drafter{s: s, repository: repo}
				problems, err := draft.validate(trace.Document{Content: sl.Spec}, trace.Document{Content: sl.Plan})
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
		case deltaReviewTool:
			if c.State != "debating" {
				return priorityRefusal("the delta is not in debate")
			}
			var in deltaAssessment
			if err := decodeToolInput(raw, &in); err != nil {
				return nil, err
			}
			in.Member = agent
			if slices.ContainsFunc(c.Debate, func(d deltaAssessment) bool { return d.Member == agent }) {
				return json.RawMessage(`{"recorded":true}`), nil
			}
			c.Debate = append(c.Debate, in)
		case deltaReplyTool:
			if c.State != "answering" {
				return priorityRefusal("the delta is not awaiting an answer")
			}
			var in struct {
				Reply string `json:"reply"`
			}
			if err := decodeToolInput(raw, &in); err != nil {
				return nil, err
			}
			if in.Reply == "" {
				return priorityRefusal("answer the committee's delta assessment")
			}
			c.Reply, c.State = in.Reply, "presenting"
		}
		if err := s.saveChange(ctx, wt, &c); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"recorded":true}`), nil
	}}
}

func sameProjectSpec(a, b trace.ProjectSpec) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// revisionSlice keeps the approved proposal separate from the mechanical
// revision identities needed to preserve historical completed unit evidence.
func revisionSlice(c WorkgroupChange, row amendment.StreamImpact) (trace.Slice, error) {
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
