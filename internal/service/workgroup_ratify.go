package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// ProjectSpecDraftTool delivers the project-level behavior, the cross-repo
// contracts, the workstream graph and, per member, which of that member's
// own already-recorded acceptance criteria are contract sections. The
// service fills in each member's own slice content (its own recorded
// spec.md and plan.json) mechanically from the trace; the architect need
// not retype it.
const ProjectSpecDraftTool = "deliver_project_spec"

// projectSpecDraftSubject is the workflow subject, tracked on a cross-repo
// workgroup's host workstream in its own trace, that tracks the current
// drafting attempt of its project spec: "drafting" while the turn runs,
// "drafted" once the project spec is recorded, "failed" if the architect
// could not deliver a valid one.
const projectSpecDraftSubject = "project-spec-draft"

var projectSpecDraftActor = trace.Actor{Kind: "service", ID: "project-spec-drafting"}

var workgroupJoinLocks sync.Map

// projectSpecDraft is what the architect delivers through
// ProjectSpecDraftTool.
type projectSpecDraft struct {
	Behavior  string                  `json:"behavior"`
	Contracts []trace.Contract        `json:"contracts,omitempty"`
	Graph     []trace.Edge            `json:"graph,omitempty"`
	Slices    []projectSpecDraftSlice `json:"slices"`
}

type projectSpecDraftSlice struct {
	Workstream       config.WorkstreamID `json:"workstream"`
	ContractCriteria []int               `json:"contract_criteria,omitempty"`
}

// projectSpec builds the full project spec from the draft: one slice per
// member, holding the contract criteria the draft gives it and the spec and
// plan content returns for it.
func (d projectSpecDraft) projectSpec(members []config.WorkstreamID, content func(config.WorkstreamID) (spec, plan string, err error)) (trace.ProjectSpec, error) {
	criteria := map[config.WorkstreamID][]int{}
	for _, sl := range d.Slices {
		criteria[sl.Workstream] = sl.ContractCriteria
	}
	slices := make([]trace.Slice, 0, len(members))
	for _, m := range members {
		specContent, planContent, err := content(m)
		if err != nil {
			return trace.ProjectSpec{}, err
		}
		slices = append(slices, trace.Slice{Workstream: m, Spec: specContent, Plan: planContent, ContractCriteria: criteria[m]})
	}
	return trace.ProjectSpec{Behavior: d.Behavior, Contracts: d.Contracts, Graph: d.Graph, Slices: slices}, nil
}

// DraftWorkgroup creates a new cross-repo workgroup for the given member
// workstreams, one per repo, each already handed into its own active
// project. It records, on each member's own trace, a locator pointing to
// the new workgroup, and returns its ID. Drafting its project spec starts
// once every member is sketched: this call only creates the workgroup and
// its shared chief-of-staff thread, through unit workgroup-trace's
// workgroup-level trace. It is the production entry point a brief that
// names more than one repo is handed to; a one-repo hand-in keeps using the
// existing single-workstream path unchanged.
//
// Readiness is checked for every member before anything is written: a
// member may point at no cross-repo workgroup other than the one this very
// call is resuming (below), and it may carry no chief-of-staff turn of its
// own. JoinCrossRepoWorkgroup refuses once a thread has a real turn, and
// a refusal partway through the per-member loop would leave the members
// already processed durably joined while later ones are not. Checking
// every member first, before the workgroup trace is even created or
// reopened, keeps a refusal here leaving nothing recorded on any member.
//
// If an earlier call already created this join's workgroup trace and
// joined some of its members before being interrupted (a crash or a
// restart between members), a retry with the same members reopens that
// same trace, rather than calling trace.CreateWorkgroupTrace again and
// minting a second one, and finishes joining whichever members have not
// yet recorded their own pointer to it: JoinCrossRepoWorkgroup and
// RetractWorkgroup are documented to change
// nothing once a member has already completed them, so resuming the loop
// for every member, not only the ones still missing their pointer, is
// always safe.
func (s *Service) DraftWorkgroup(ctx context.Context, members []config.WorkstreamID) (config.WorkgroupID, *APIError) {
	lock, _ := workgroupJoinLocks.LoadOrStore(s.current().Root.String(), &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if len(members) < 2 {
		return "", &APIError{Code: Validation, Message: "a cross-repo workgroup needs at least two member workstreams"}
	}
	resolved := make([]trace.CrossRepoMember, 0, len(members))
	seen := map[config.WorkstreamID]bool{}
	for _, m := range members {
		if seen[m] {
			return "", &APIError{Code: Validation, Message: fmt.Sprintf("workstream %s is named twice", m)}
		}
		seen[m] = true
		_, _, repo, api := s.conversationTrace(string(m))
		if api != nil {
			return "", api
		}
		resolved = append(resolved, trace.CrossRepoMember{Workstream: m, Project: repo})
	}
	var resuming config.WorkgroupID
	for _, m := range resolved {
		id, err := m.Project.CrossRepoWorkgroupOf(m.Workstream)
		if err != nil {
			return "", &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the cross-repo workgroup locator of workstream %s: %v", m.Workstream, err)}
		}
		if id != "" {
			if resuming != "" && resuming != id {
				return "", &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s already belongs to a different cross-repo workgroup than the rest of this join", m.Workstream)}
			}
			resuming = id
		}
		groups, _, err := trace.Workgroups(m.Project)
		if err != nil {
			return "", &APIError{Code: Internal, Message: err.Error()}
		}
		if group, ok := trace.WorkgroupOf(groups, m.Workstream); ok && len(group.Members) != 1 {
			return "", &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s already belongs to a multi-member local workgroup", m.Workstream)}
		}
		th, err := m.Project.ChiefOfStaffThread(m.Workstream)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the chief-of-staff thread of workstream %s: %v", m.Workstream, err)}
		}
		if err == nil && len(th.Turns) > 0 {
			return "", &APIError{Code: Conflict, Message: fmt.Sprintf("workstream %s already has chief-of-staff turns of its own; it cannot join a cross-repo workgroup", m.Workstream)}
		}
	}
	var wt *trace.Repository
	var g trace.Workgroup
	if resuming == "" {
		entries, err := os.ReadDir(filepath.Join(s.current().Root.String(), "workgroups"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", &APIError{Code: Internal, Message: fmt.Sprintf("cannot inspect pending workgroup joins: %v", err)}
		}
		for _, entry := range entries {
			id, err := config.ParseWorkgroupID(entry.Name())
			if err != nil {
				continue
			}
			candidate, err := trace.OpenWorkgroupTrace(s.current().Root, id, resolved)
			if err != nil {
				if candidate != nil {
					candidate.Close()
				}
				return "", &APIError{Code: Internal, Message: fmt.Sprintf("cannot inspect workgroup %s: %v", id, err)}
			}
			groups, _, err := trace.Workgroups(candidate)
			candidate.Close()
			if err != nil {
				return "", &APIError{Code: Internal, Message: fmt.Sprintf("cannot read workgroup %s: %v", id, err)}
			}
			for _, group := range groups {
				matches := len(group.Members) == len(resolved)+1
				if !matches {
					continue
				}
				for _, member := range resolved {
					matches = matches && slices.Contains(group.Members[1:], member.Workstream)
				}
				if matches {
					resuming = group.ID
				}
			}
		}
	}
	if resuming == "" {
		created, createdGroup, err := trace.CreateWorkgroupTrace(ctx, s.current().Root, resolved, s.now(), ownerActor, "draft-workgroup")
		if err != nil {
			return "", &APIError{Code: Internal, Message: fmt.Sprintf("cannot create the workgroup: %v", err)}
		}
		wt, g = created, createdGroup
	} else {
		opened, err := trace.OpenWorkgroupTrace(s.current().Root, resuming, resolved)
		if opened != nil && err != nil {
			opened.Close()
		}
		if err != nil {
			return "", &APIError{Code: Internal, Message: fmt.Sprintf("cannot reopen the half-joined cross-repo workgroup %s: %v", resuming, err)}
		}
		groups, _, err := trace.Workgroups(opened)
		if err != nil {
			opened.Close()
			return "", &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the record of cross-repo workgroup %s: %v", resuming, err)}
		}
		found := false
		for _, candidate := range groups {
			if candidate.ID == resuming {
				g, found = candidate, true
				break
			}
		}
		matches := found && len(g.Members) == len(resolved)+1
		for _, m := range resolved {
			matches = matches && slices.Contains(g.Members[1:], m.Workstream)
		}
		if !matches {
			opened.Close()
			return "", &APIError{Code: Internal, Message: fmt.Sprintf("cross-repo workgroup %s does not match the requested members", resuming)}
		}
		wt = opened
	}
	defer wt.Close()
	if err := s.step("workgroup-created"); err != nil {
		return "", &APIError{Code: Internal, Message: err.Error()}
	}
	for _, m := range resolved {
		if err := s.step("workgroup-member-joining"); err != nil {
			return "", &APIError{Code: Internal, Message: err.Error()}
		}
		// The locator and chief share a member trace; the solitary group is
		// in the separate project trace. Publish the route before removing
		// that group, so interruption leaves an authoritative shared route.
		if err := m.Project.JoinCrossRepoWorkgroup(ctx, m.Workstream, g.ID, s.now(), ownerActor, "draft-workgroup"); err != nil {
			return "", &APIError{Code: Internal, Message: fmt.Sprintf("cannot join workstream %s to the shared chief: %v", m.Workstream, err)}
		}
		if err := s.step("workgroup-member-routed"); err != nil {
			return "", &APIError{Code: Internal, Message: err.Error()}
		}
		if err := m.Project.RetractWorkgroup(ctx, m.Workstream, s.now(), ownerActor, "draft-workgroup"); err != nil {
			return "", &APIError{Code: Internal, Message: fmt.Sprintf("cannot retract workstream %s's own one-workstream workgroup: %v", m.Workstream, err)}
		}
		if err := s.step("workgroup-member-transferred"); err != nil {
			return "", &APIError{Code: Internal, Message: err.Error()}
		}
		if err := s.step("workgroup-member-joined"); err != nil {
			return "", &APIError{Code: Internal, Message: err.Error()}
		}
	}
	return g.ID, nil
}

// draftProjectSpecIfReady starts or resumes the architect's project-spec turn
// once all members have recorded drafts. An existing unapproved spec is
// redrafted only when a member's numbered contract text has changed.
func (s *Service) draftProjectSpecIfReady(ctx context.Context, known *trace.Repository, id config.WorkgroupID) error {
	_, g, _, err := known.CrossRepoProjectSpec(id)
	if err != nil {
		return err
	}
	if len(g.Members) < 2 || g.Conversation != nil && len(g.Members) == 2 {
		return nil
	}
	// Approved groups need no live member documents, including after retirement.
	authority, err := trace.OpenWorkgroupAuthority(s.current().Root, id)
	if err != nil {
		if authority != nil {
			authority.Close()
		}
		return err
	}
	_, _, approved, readErr := trace.ReadGroupApproval(authority, g.Members[0])
	closeErr := authority.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if approved {
		return nil
	}
	live, err := s.liveWorkgroupMembers(g)
	if err != nil || len(live) != len(g.Members)-1 {
		return err
	}
	host := g.Members[0]
	real := g.Members[1:]
	if _, err := s.crossRepoMemberRepos(ctx, g); errors.Is(err, errWorkgroupJoining) {
		return nil
	} else if err != nil {
		return err
	}
	members := make([]trace.CrossRepoMember, 0, len(real))
	for _, m := range real {
		// Checked immediately before each resolution, the same way
		// crossRepoMemberRepos (workgroup_wiring.go) does: the service
		// clears its own active-project list only after cancelling the
		// context every background pass runs under, so a resolution
		// failure met with ctx already done is the service stopping, not a
		// real data problem, and is reported as ctx's own error rather
		// than a hard failure of the pass.
		if err := ctx.Err(); err != nil {
			return err
		}
		_, _, repo, api := s.conversationTrace(string(m))
		if api != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("workstream %s: %s", m, api.Message)
		}
		// Readiness is whether the member's own spec and plan are recorded,
		// not its current feature state: a member that skips debate moves
		// past SketchedState into InShedState the moment it is recorded, so
		// gating on the transient state misses it.
		if _, _, err := memberSpecAndPlan(repo, m); err != nil {
			// Not every member is sketched yet; try again once they are.
			return nil
		}
		members = append(members, trace.CrossRepoMember{Workstream: m, Project: repo})
	}
	wt, err := trace.OpenWorkgroupTrace(s.current().Root, id, members)
	if wt != nil && err != nil {
		wt.Close()
	}
	if err != nil {
		return err
	}
	defer wt.Close()
	lock := s.groupApprovalLock(id)
	lock.Lock()
	input, ready, err := s.prepareProjectSpecDraft(ctx, wt, g, members)
	lock.Unlock()
	if err != nil || !ready {
		return err
	}
	if err := s.step("project-spec-draft-requested"); err != nil {
		return err
	}
	return s.dispatchProjectSpecDraft(ctx, wt, g, host, members, input)
}

// prepareProjectSpecDraft runs under the group approval lock. Dispatch runs
// outside it, so guarded ratification can refuse promptly during the turn.
func (s *Service) prepareProjectSpecDraft(ctx context.Context, wt *trace.Repository, g trace.Workgroup, members []trace.CrossRepoMember) (projectSpecDraftInput, bool, error) {
	host, id := g.Members[0], g.ID
	if _, _, approved, err := trace.ReadGroupApproval(wt, host); err != nil || approved {
		return projectSpecDraftInput{}, false, err
	}
	spec, found, err := trace.ProjectSpecFor(wt, id)
	if err != nil {
		return projectSpecDraftInput{}, false, err
	}
	state, err := wt.Workflow(host, projectSpecDraftSubject)
	if err != nil {
		return projectSpecDraftInput{}, false, err
	}
	input, revision, err := readProjectSpecDraftInput(wt, host)
	if err != nil {
		return projectSpecDraftInput{}, false, err
	}
	if state.Value == "drafting" {
		turn := input.Turn
		if revision == 0 {
			// A drafting transition can have recorded output without input pins.
			// Reconcile by turn identity without inventing those pins.
			transitions, err := trace.Read[trace.Transition](wt, host)
			if err != nil {
				return projectSpecDraftInput{}, false, err
			}
			for i := len(transitions) - 1; i >= 0; i-- {
				if tr := transitions[i]; tr.Subject == projectSpecDraftSubject && tr.To == "drafting" {
					turn = tr.ID
					break
				}
			}
		}
		if recorded, err := projectSpecDraftRecorded(wt, turn); err != nil {
			return projectSpecDraftInput{}, false, err
		} else if recorded {
			return projectSpecDraftInput{}, false, s.finishProjectSpecDraft(ctx, wt, host, turn)
		}
		if revision == 0 {
			if err := s.supersedeUnpinnedProjectSpecDraft(ctx, wt, host, turn); err != nil {
				return projectSpecDraftInput{}, false, err
			}
			next, err := s.snapshotProjectSpecDraft(members, spec)
			if err != nil {
				return projectSpecDraftInput{}, false, err
			}
			if found && len(next.Guarded) == 0 {
				return projectSpecDraftInput{}, false, s.failProjectSpecDraft(ctx, wt, host, "the project spec draft has no recorded member revisions; its unpinned output was superseded")
			}
			next.Turn = fmt.Sprintf("project-spec-draft-%d", state.Version+1)
			input = next
			if err := s.requestProjectSpecDraft(ctx, wt, host, state, input, 1); err != nil {
				return projectSpecDraftInput{}, false, err
			}
		} else if input.Turn == "" {
			return projectSpecDraftInput{}, false, fmt.Errorf("project spec drafting has no recorded member revisions")
		}
	} else {
		next, err := s.snapshotProjectSpecDraft(members, spec)
		if err != nil {
			return projectSpecDraftInput{}, false, err
		}
		if found && len(next.Guarded) == 0 {
			return projectSpecDraftInput{}, false, nil
		}
		// Initial draft failures remain terminal. A guarded redraft can retry on
		// a later pass, retaining its request identity and member revisions.
		if !found && state.Value != "" {
			return projectSpecDraftInput{}, false, nil
		}
		if state.Value == "failed" && maps.Equal(input.Guarded, next.Guarded) {
			next = input
		}
		next.Turn = fmt.Sprintf("project-spec-draft-%d", state.Version+1)
		input = next
		if err := s.requestProjectSpecDraft(ctx, wt, host, state, input, revision+1); err != nil {
			return projectSpecDraftInput{}, false, err
		}
	}
	return input, true, nil
}

// requestProjectSpecDraft creates the workgroup's shared chief-of-staff
// thread's architect counterpart if missing, and enqueues the one draft
// turn.
func (s *Service) requestProjectSpecDraft(ctx context.Context, wt *trace.Repository, host config.WorkstreamID, state trace.WorkflowState, input projectSpecDraftInput, revision int) error {
	if _, err := wt.Thread(host, architectAgent); errors.Is(err, os.ErrNotExist) {
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: architectAgent, Revision: 1, Project: wt.Project(), Workstream: host, At: s.now(), Actor: projectSpecDraftActor, Cause: "project-spec-draft"}, Role: architectRole, ThreadID: architectThread}
		if err := wt.CreateThread(ctx, identity); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: input.Turn, Revision: 1, Project: wt.Project(), Workstream: host, At: s.now(), Actor: projectSpecDraftActor, Cause: "project-spec-draft"},
			Subject: projectSpecDraftSubject, From: state.Value, To: "drafting", Reason: "every member is sketched; the architect is asked for the project spec"}}
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: projectSpecDraftInputDocument, Revision: revision, Project: wt.Project(), Workstream: host, At: s.now(), Actor: projectSpecDraftActor, Cause: input.Turn}, Path: "tools/project-spec-draft-input.json", Content: string(data)}
	_, err = wt.RecordDocumentsWith(ctx, []trace.Document{doc}, tx)
	return err
}

// projectSpecTurnDirectoryFor is the service-owned directory of the one
// architect turn that drafts a cross-repo workgroup's project spec, keyed
// by its host workstream: the one identity every path in this flow already
// carries (the thread, the turn scope and the turn input).
func (s *Service) projectSpecTurnDirectoryFor(host config.WorkstreamID) string {
	return filepath.Join(s.current().Root.String(), "architect", "workgroups-by-host", string(host))
}

// dispatchProjectSpecDraft runs the architect's turn through the thread
// dispatcher, as drafter does for one workstream's own draft, then records
// what it delivered. If the turn asks a question, drafting is reported as
// failed: this unit's drafter gives the architect no ask tool for this step,
// since by the time every member is sketched, their own drafting already
// resolved what a worker would need to ask.
func (s *Service) dispatchProjectSpecDraft(ctx context.Context, wt *trace.Repository, g trace.Workgroup, host config.WorkstreamID, members []trace.CrossRepoMember, input projectSpecDraftInput) error {
	turn := input.Turn
	t, err := wt.Thread(host, architectAgent)
	if err != nil {
		return err
	}
	if !queuedTurn(t, turn) {
		if err := s.enqueueProjectSpecDraft(ctx, wt, g, host, members, turn); err != nil {
			return err
		}
	}
	ctx, release := s.stoppable(ctx, wt.Project(), host)
	defer release()
	dispatcher := thread.Dispatcher{Runner: s.threadRunner(s.current(), wt, s.projectSpecDraftTurns(wt, g, host, members, input), s.now), Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
		workspace := filepath.Join(s.projectSpecTurnDirectoryFor(in.Workstream), turn, "workspace")
		if err := stageProjectSpecView(members, workspace, input); err != nil {
			return coreadapter.PreparedTurn{}, err
		}
		if err := s.step("project-spec-draft-staged"); err != nil {
			return coreadapter.PreparedTurn{}, err
		}
		directory := filepath.Join(s.projectSpecTurnDirectoryFor(in.Workstream), turn, "session")
		return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
	}}
	op, err := thread.TurnOperation(wt.Project(), "project-spec-draft", thread.TurnInput{Workstream: host, Agent: architectAgent, Turn: turn})
	if err != nil {
		return err
	}
	result, err := dispatcher.Apply(ctx, op)
	if err != nil {
		return err
	}
	// A queued turn's outcome is "idle" on an ordinary completed response,
	// not "succeeded": trace.QueuedTurn.Status names it that way, and the
	// same values other factory turns check against.
	if result.Outcome != "idle" && result.Outcome != "failed" {
		// Still running, captured and not completed, or interrupted; try
		// again on a later pass.
		return nil
	}
	return s.recordProjectSpecDraft(ctx, wt, g, host, turn, result)
}

func (s *Service) enqueueProjectSpecDraft(ctx context.Context, wt *trace.Repository, g trace.Workgroup, host config.WorkstreamID, members []trace.CrossRepoMember, turn string) error {
	profile, _, err := s.roleExecution(s.current(), architectRole)
	if err != nil {
		return err
	}
	prompt, err := s.projectSpecDraftPrompt(members)
	if err != nil {
		return err
	}
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: wt.Project(), Workstream: host, At: s.now(), Actor: projectSpecDraftActor, Cause: "project-spec-draft", Depth: 1},
		AgentID: architectAgent, ThreadID: architectThread, TurnID: turn, Profile: profile, SystemPrompt: projectSpecDraftSystemPrompt(), Prompt: prompt}
	_, err = wt.EnqueueTurn(ctx, req)
	return err
}

// projectSpecDraftTurns is the architect's isolated turn path for the
// project-spec draft: a read-only staged view of every member's own already
// recorded handed input, spec.md and plan.json, the delivery tool, and no
// write, execute, network, VCS or ask capability. Unlike a per-member
// draft, this step cannot ask a question: it only combines what members'
// own drafting already resolved.
func (s *Service) projectSpecDraftTurns(wt *trace.Repository, g trace.Workgroup, host config.WorkstreamID, members []trace.CrossRepoMember, input projectSpecDraftInput) *isolation.Turns {
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if a := s.options.Architect; a != nil {
		engine, hosts = a.Engine, a.Hosts
	}
	workspace := filepath.Join(s.projectSpecTurnDirectoryFor(host), input.Turn, "workspace")
	turns := &isolation.Turns{
		Workspaces: stagedWorkspaces{},
		Views:      isolation.Views{Directory: filepath.Join(s.current().Root.String(), "views")},
		Select: func(_ context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			if scope.Role != architectRole || scope.Workstream != string(host) {
				return isolation.Selection{}, errors.New("turn scope denied")
			}
			_, settings, err := s.roleExecution(s.current(), architectRole)
			if err != nil {
				return isolation.Selection{}, err
			}
			return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: []string{"members"}, Execution: settings}, nil
		},
		Grants: map[string]coreadapter.Capabilities{architectRole: {Tools: []string{ProjectSpecDraftTool}}},
		Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Workstream != string(host) {
				return nil, errors.New("turn scope denied")
			}
			return []coreadapter.Tool{s.projectSpecDraftTool(g, host, input.Turn)}, nil
		},
		Hosts:  hosts,
		Engine: engine,
	}
	return memoryTurns(turns, s.about(wt), wt)
}

// stageProjectSpecView writes, under workspace/members/<id>/, each
// member's own handed input and its pinned spec.md and plan.json,
// read directly from that member's own already-open repository: the
// architect's view of a cross-repo workgroup's members, assembled without
// opening any backend workspace or cloning any target repository.
func stageProjectSpecView(members []trace.CrossRepoMember, workspace string, input projectSpecDraftInput) error {
	if err := os.RemoveAll(workspace); err != nil {
		return err
	}
	for _, m := range members {
		docs, err := trace.Read[trace.Document](m.Project, m.Workstream)
		if err != nil {
			return err
		}
		latest := map[string]trace.Document{}
		for _, d := range docs {
			if d.Revision >= latest[d.ID].Revision {
				latest[d.ID] = d
			}
		}
		dir := filepath.Join(workspace, "members", string(m.Workstream))
		files := map[string]string{}
		if handed, ok := latest[handedDocument]; ok {
			files[handed.Path] = handed.Content
		}
		spec, graph, err := pinnedMemberSpecAndPlan(m.Project, m.Workstream, input.Members[m.Workstream])
		if err != nil {
			return err
		}
		files[plan.SpecPath], files[plan.PlanPath] = spec, graph
		for name, content := range files {
			path := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				return err
			}
		}
	}
	return nil
}

// projectSpecDraftTool delivers the workgroup's project spec draft into the
// turn's output directory, which the service records once the turn
// completes. It returns the verdict the service would give the delivered
// draft if the turn ended now.
func (s *Service) projectSpecDraftTool(g trace.Workgroup, host config.WorkstreamID, turn string) coreadapter.Tool {
	return coreadapter.Tool{Name: ProjectSpecDraftTool, Description: "Deliver the project spec: behavior, contracts, graph and each member's own contract-criteria numbers. Returns the verdict the service would give the delivered project spec if your turn ended now: valid, or invalid with its problem.", Effect: coreadapter.ToolMemory,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"behavior":{"type":"string"},"contracts":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"text":{"type":"string"}},"required":["name","text"],"additionalProperties":false}},"graph":{"type":"array","items":{"type":"object","properties":{"from":{"type":"string"},"to":{"type":"string"},"type":{"type":"string"}},"required":["from","to","type"],"additionalProperties":false}},"slices":{"type":"array","items":{"type":"object","properties":{"workstream":{"type":"string"},"contract_criteria":{"type":"array","items":{"type":"integer"}}},"required":["workstream"],"additionalProperties":false}}},"required":["behavior","slices"],"additionalProperties":false}`),
		Handle: func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var draft projectSpecDraft
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&draft); err != nil {
				return nil, err
			}
			data, err := json.Marshal(draft)
			if err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Join(s.projectSpecTurnDirectoryFor(host), turn, "output"), 0700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(s.projectSpecTurnDirectoryFor(host), turn, "output", "project-spec.json"), data, 0600); err != nil {
				return nil, err
			}
			// Validation reads no member content, so the slices are built
			// without it.
			full, err := draft.projectSpec(g.Members[1:], func(config.WorkstreamID) (string, string, error) { return "", "", nil })
			if err != nil {
				return nil, err
			}
			status := draftStatus{Delivered: "project-spec.json", Verdict: draftValid}
			if err := trace.ValidateProjectSpec(full, g.Members); err != nil {
				status.Verdict, status.Problems = draftInvalid, []string{err.Error()}
			}
			status.Next = draftNext[status.Verdict]
			return json.Marshal(status)
		}}
}

func projectSpecDraftSystemPrompt() string {
	return fmt.Sprintf("You are the architect drafting the shared project spec of a multi-repo workgroup. Each member workstream's own spec and plan are already drafted and sketched, independently, in its own repo; your job is to add only what spans them: the intended cross-repo behaviour, the named contracts at the seams between repos, the dependency graph between members, and, for each member, which of its own already-numbered acceptance criteria are contract sections (read-only to that member's own workers). You hold no version-control tool and no way to ask a question in this step: use only what is in your view, and deliver with %s. Do not restate or retype a member's own spec or plan text; the service fills each member's own slice content in automatically from what is already recorded.", ProjectSpecDraftTool)
}

func (s *Service) projectSpecDraftPrompt(members []trace.CrossRepoMember) (string, error) {
	var b strings.Builder
	b.WriteString("Draft the project spec of this workgroup from its members' own already-sketched specs and plans.\n\nYour view holds, for each member workstream, its handed input, its own drafted spec.md and plan.json, under members/<workstream>/.\n\n")
	for _, m := range members {
		b.WriteString("Member " + string(m.Workstream) + "\n")
	}
	fmt.Fprintf(&b, `
Deliver with %s: {"behavior": "...", "contracts": [{"name": "...", "text": "..."}], "graph": [{"from": "...", "to": "...", "type": "contract|hard_block|merge_order"}], "slices": [{"workstream": "...", "contract_criteria": [1, 2]}]}.

- behavior: the intended cross-repo behaviour as prose, including what spans the members' own repos.
- contracts: the named interfaces at the seams between members' repos, each with its full text.
- graph: a dependency edge between two members is optional; every edge must name two members from the list above and one of the three types.
- slices: one entry per member listed above, naming which of that member's own spec.md acceptance criteria (by number) are contract sections, read-only to that member's own workers; an empty list is fine when none of that member's criteria is a contract section.
`, ProjectSpecDraftTool)
	return b.String(), nil
}

// recordProjectSpecDraft reads the delivered project-spec.json, builds the
// full project spec by filling in each member's own pinned spec.md and
// plan.json content as its slice, validates and records it, or records why
// the draft failed.
func (s *Service) recordProjectSpecDraft(ctx context.Context, wt *trace.Repository, g trace.Workgroup, host config.WorkstreamID, turn string, result coreadapter.OperationResult) error {
	if result.Outcome != "idle" {
		return s.failProjectSpecDraft(ctx, wt, host, "the architect's turn did not deliver a project spec: "+result.Evidence)
	}
	data, err := os.ReadFile(filepath.Join(s.projectSpecTurnDirectoryFor(host), turn, "output", "project-spec.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s.failProjectSpecDraft(ctx, wt, host, "the architect's turn ended without delivering project-spec.json")
	}
	if err != nil {
		return err
	}
	var draft projectSpecDraft
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&draft); err != nil {
		return s.failProjectSpecDraft(ctx, wt, host, "the delivered project spec does not parse: "+err.Error())
	}
	input, _, err := readProjectSpecDraftInput(wt, host)
	if err != nil {
		return err
	}
	if input.Turn != turn {
		return fmt.Errorf("project spec draft input differs from turn %s", turn)
	}
	full, err := draft.projectSpec(g.Members[1:], func(m config.WorkstreamID) (string, string, error) {
		_, _, repo, api := s.conversationTrace(string(m))
		if api != nil {
			return "", "", fmt.Errorf("workstream %s: %s", m, api.Message)
		}
		return pinnedMemberSpecAndPlan(repo, m, input.Members[m])
	})
	if err != nil {
		return err
	}
	if err := s.step("project-spec-draft-delivered"); err != nil {
		return err
	}
	lock := s.groupApprovalLock(g.ID)
	lock.Lock()
	defer lock.Unlock()
	if recorded, err := projectSpecDraftRecorded(wt, turn); err != nil {
		return err
	} else if recorded {
		return nil
	}
	if _, _, approved, err := trace.ReadGroupApproval(wt, host); err != nil || approved {
		return err
	}
	if err := trace.RecordProjectSpec(ctx, wt, g, full, s.now(), projectSpecDraftActor, turn); err != nil {
		return s.failProjectSpecDraft(ctx, wt, host, "the delivered project spec is invalid: "+err.Error())
	}
	if err := s.step("project-spec-draft-recorded"); err != nil {
		return err
	}
	return s.finishProjectSpecDraft(ctx, wt, host, turn)
}

// memberSpecAndPlan returns the member's own latest recorded spec.md and
// plan.json content.
func memberSpecAndPlan(repo *trace.Repository, stream config.WorkstreamID) (specContent, planContent string, err error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return "", "", err
	}
	var specDoc, planDoc trace.Document
	for _, d := range docs {
		switch d.ID {
		case plan.SpecDocument:
			if d.Revision >= specDoc.Revision {
				specDoc = d
			}
		case plan.PlanDocument:
			if d.Revision >= planDoc.Revision {
				planDoc = d
			}
		}
	}
	if specDoc.ID == "" || planDoc.ID == "" {
		return "", "", fmt.Errorf("workstream %s has no recorded spec and plan", stream)
	}
	return specDoc.Content, planDoc.Content, nil
}

func (s *Service) failProjectSpecDraft(ctx context.Context, wt *trace.Repository, host config.WorkstreamID, reason string) error {
	state, err := wt.Workflow(host, projectSpecDraftSubject)
	if err != nil {
		return err
	}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: fmt.Sprintf("project-spec-draft-failed-%d", state.Version), Revision: 1, Project: wt.Project(), Workstream: host, At: s.now(), Actor: projectSpecDraftActor, Cause: "project-spec-draft"},
			Subject: projectSpecDraftSubject, From: state.Value, To: "failed", Reason: reason},
		Events: []trace.Event{trace.Notice(fmt.Sprintf("project-spec-draft-failed-%d", state.Version), "chief", "The architect could not draft the workgroup's project spec: "+reason+". Guarded contract redrafts will retry on a later service pass.")}}
	_, err = wt.Transact(ctx, tx)
	return err
}

// projectSpecDraftPass checks, for every real member of repository's own
// workstreams that carries a cross-repo workgroup locator, whether that
// workgroup's project spec can now be drafted. Only the workgroup's
// alphabetically-first real member drives the draft, so that every
// member's own project pass finding the same workgroup does not race to
// draft it more than once; the others' calls are a cheap, idempotent no-op.
func (s *Service) projectSpecDraftPass(ctx context.Context, repository *trace.Repository) error {
	streams, err := repository.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, err := repository.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return fmt.Errorf("workstream %s cross-repo workgroup: %w", stream, err)
		}
		if id == "" {
			continue
		}
		_, g, _, err := repository.CrossRepoProjectSpec(id)
		if errors.Is(err, trace.ErrLocked) {
			// Another member's own pass, or this workgroup's leader, holds
			// the workgroup trace open right now (for example, while
			// dispatching the architect's draft turn); try again next pass.
			continue
		}
		if err != nil || len(g.Members) < 2 {
			if err != nil {
				return fmt.Errorf("workgroup %s: %w", id, err)
			}
			continue
		}
		if leader := leastWorkstream(g.Members[1:]); leader != stream {
			continue
		}
		err = s.draftProjectSpecIfReady(ctx, repository, id)
		if errors.Is(err, trace.ErrLocked) || errors.Is(err, errWorkgroupJoining) || errors.Is(err, trace.ErrMembershipChanged) || errors.Is(err, trace.ErrMemberRetired) {
			// The workgroup or its member bindings are held; try again next pass.
			continue
		}
		if err != nil {
			return fmt.Errorf("workgroup %s project spec: %w", id, err)
		}
	}
	return nil
}

// leastWorkstream returns the alphabetically-first of streams.
func leastWorkstream(streams []config.WorkstreamID) config.WorkstreamID {
	least := streams[0]
	for _, s := range streams[1:] {
		if s < least {
			least = s
		}
	}
	return least
}
