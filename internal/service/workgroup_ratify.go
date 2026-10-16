package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// workgroup's host workstream in its own trace, that tracks the one
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
		return "", &APIError{Validation, "a cross-repo workgroup needs at least two member workstreams"}
	}
	resolved := make([]trace.CrossRepoMember, 0, len(members))
	seen := map[config.WorkstreamID]bool{}
	for _, m := range members {
		if seen[m] {
			return "", &APIError{Validation, fmt.Sprintf("workstream %s is named twice", m)}
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
			return "", &APIError{Internal, fmt.Sprintf("cannot read the cross-repo workgroup locator of workstream %s: %v", m.Workstream, err)}
		}
		if id != "" {
			if resuming != "" && resuming != id {
				return "", &APIError{Conflict, fmt.Sprintf("workstream %s already belongs to a different cross-repo workgroup than the rest of this join", m.Workstream)}
			}
			resuming = id
		}
		groups, _, err := trace.Workgroups(m.Project)
		if err != nil {
			return "", &APIError{Internal, err.Error()}
		}
		if group, ok := trace.WorkgroupOf(groups, m.Workstream); ok && len(group.Members) != 1 {
			return "", &APIError{Conflict, fmt.Sprintf("workstream %s already belongs to a multi-member local workgroup", m.Workstream)}
		}
		th, err := m.Project.ChiefOfStaffThread(m.Workstream)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", &APIError{Internal, fmt.Sprintf("cannot read the chief-of-staff thread of workstream %s: %v", m.Workstream, err)}
		}
		if err == nil && len(th.Turns) > 0 {
			return "", &APIError{Conflict, fmt.Sprintf("workstream %s already has chief-of-staff turns of its own; it cannot join a cross-repo workgroup", m.Workstream)}
		}
	}
	var wt *trace.Repository
	var g trace.Workgroup
	if resuming == "" {
		entries, err := os.ReadDir(filepath.Join(s.current().Root.String(), "workgroups"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", &APIError{Internal, fmt.Sprintf("cannot inspect pending workgroup joins: %v", err)}
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
				return "", &APIError{Internal, fmt.Sprintf("cannot inspect workgroup %s: %v", id, err)}
			}
			groups, _, err := trace.Workgroups(candidate)
			candidate.Close()
			if err != nil {
				return "", &APIError{Internal, fmt.Sprintf("cannot read workgroup %s: %v", id, err)}
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
			return "", &APIError{Internal, fmt.Sprintf("cannot create the workgroup: %v", err)}
		}
		wt, g = created, createdGroup
	} else {
		opened, err := trace.OpenWorkgroupTrace(s.current().Root, resuming, resolved)
		if opened != nil && err != nil {
			opened.Close()
		}
		if err != nil {
			return "", &APIError{Internal, fmt.Sprintf("cannot reopen the half-joined cross-repo workgroup %s: %v", resuming, err)}
		}
		groups, _, err := trace.Workgroups(opened)
		if err != nil {
			opened.Close()
			return "", &APIError{Internal, fmt.Sprintf("cannot read the record of cross-repo workgroup %s: %v", resuming, err)}
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
			return "", &APIError{Internal, fmt.Sprintf("cross-repo workgroup %s does not match the requested members", resuming)}
		}
		wt = opened
	}
	defer wt.Close()
	if err := s.step("workgroup-created"); err != nil {
		return "", &APIError{Internal, err.Error()}
	}
	for _, m := range resolved {
		if err := s.step("workgroup-member-joining"); err != nil {
			return "", &APIError{Internal, err.Error()}
		}
		// The locator and chief share a member trace; the solitary group is
		// in the separate project trace. Publish the route before removing
		// that group, so interruption leaves an authoritative shared route.
		if err := m.Project.JoinCrossRepoWorkgroup(ctx, m.Workstream, g.ID, s.now(), ownerActor, "draft-workgroup"); err != nil {
			return "", &APIError{Internal, fmt.Sprintf("cannot join workstream %s to the shared chief: %v", m.Workstream, err)}
		}
		if err := s.step("workgroup-member-routed"); err != nil {
			return "", &APIError{Internal, err.Error()}
		}
		if err := m.Project.RetractWorkgroup(ctx, m.Workstream, s.now(), ownerActor, "draft-workgroup"); err != nil {
			return "", &APIError{Internal, fmt.Sprintf("cannot retract workstream %s's own one-workstream workgroup: %v", m.Workstream, err)}
		}
		if err := s.step("workgroup-member-transferred"); err != nil {
			return "", &APIError{Internal, err.Error()}
		}
		if err := s.step("workgroup-member-joined"); err != nil {
			return "", &APIError{Internal, err.Error()}
		}
	}
	return g.ID, nil
}

// draftProjectSpecIfReady drafts and records the project spec of the
// cross-repo workgroup id, through one architect turn, once every real
// member has reached sketched; it does nothing, without error, when the
// workgroup is not yet ready, already has a recorded project spec, or its
// draft already failed. known is any already-open repository, used only to
// resolve where the service root is: the read and write both touch only
// id's own trace and, for sketched content, each real member's own trace,
// never known's.
func (s *Service) draftProjectSpecIfReady(ctx context.Context, known *trace.Repository, id config.WorkgroupID) error {
	_, g, found, err := known.CrossRepoProjectSpec(id)
	if err != nil {
		return err
	}
	if found || len(g.Members) < 2 || g.Conversation != nil && len(g.Members) == 2 {
		return nil
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
	// Re-check under the workgroup trace's own lock: another member's pass
	// may have drafted and recorded it between the read above and this open.
	if _, found, err := trace.ProjectSpecFor(wt, id); err != nil || found {
		return err
	}
	state, err := wt.Workflow(host, projectSpecDraftSubject)
	if err != nil {
		return err
	}
	switch state.Value {
	case "":
		if err := s.requestProjectSpecDraft(ctx, wt, host, state); err != nil {
			return err
		}
	case "drafting":
		// Fall through to dispatch below.
	default:
		// "drafted" is unreachable here (the re-check above would have found
		// it), and "failed" is terminal: the architect could not deliver a
		// valid draft, and this unit asks for no further automatic retry.
		return nil
	}
	return s.dispatchProjectSpecDraft(ctx, wt, g, host, members)
}

// requestProjectSpecDraft creates the workgroup's shared chief-of-staff
// thread's architect counterpart if missing, and enqueues the one draft
// turn.
func (s *Service) requestProjectSpecDraft(ctx context.Context, wt *trace.Repository, host config.WorkstreamID, state trace.WorkflowState) error {
	if _, err := wt.Thread(host, architectAgent); errors.Is(err, os.ErrNotExist) {
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: architectAgent, Revision: 1, Project: wt.Project(), Workstream: host, At: s.now(), Actor: projectSpecDraftActor, Cause: "project-spec-draft"}, Role: architectRole, ThreadID: architectThread}
		if err := wt.CreateThread(ctx, identity); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "project-spec-draft-1", Revision: 1, Project: wt.Project(), Workstream: host, At: s.now(), Actor: projectSpecDraftActor, Cause: "project-spec-draft"},
			Subject: projectSpecDraftSubject, From: state.Value, To: "drafting", Reason: "every member is sketched; the architect is asked for the project spec"}}
	_, err := wt.Transact(ctx, tx)
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
func (s *Service) dispatchProjectSpecDraft(ctx context.Context, wt *trace.Repository, g trace.Workgroup, host config.WorkstreamID, members []trace.CrossRepoMember) error {
	turn := "project-spec-draft-1"
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
	dispatcher := thread.Dispatcher{Runner: s.threadRunner(s.current(), wt, s.projectSpecDraftTurns(wt, host, members), s.now), Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
		directory := filepath.Join(s.projectSpecTurnDirectoryFor(in.Workstream), "session")
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

// queuedTurn reports whether the thread already has a queued turn with the
// given ID.
func queuedTurn(t trace.Thread, turn string) bool {
	for _, q := range t.Turns {
		if q.Request.TurnID == turn {
			return true
		}
	}
	return false
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
func (s *Service) projectSpecDraftTurns(wt *trace.Repository, host config.WorkstreamID, members []trace.CrossRepoMember) *isolation.Turns {
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if a := s.options.Architect; a != nil {
		engine, hosts = a.Engine, a.Hosts
	}
	workspace := filepath.Join(s.projectSpecTurnDirectoryFor(host), "workspace")
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
			if err := stageProjectSpecView(members, workspace); err != nil {
				return isolation.Selection{}, err
			}
			return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: []string{"members"}, Execution: settings}, nil
		},
		Grants: map[string]coreadapter.Capabilities{architectRole: {Tools: []string{ProjectSpecDraftTool}}},
		Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			if scope.Workstream != string(host) {
				return nil, errors.New("turn scope denied")
			}
			return []coreadapter.Tool{s.projectSpecDraftTool(host)}, nil
		},
		Hosts:  hosts,
		Engine: engine,
	}
	return memoryTurns(turns, s.about(wt), wt)
}

// stageProjectSpecView writes, under workspace/members/<id>/, each
// member's own handed input and its latest recorded spec.md and plan.json,
// read directly from that member's own already-open repository: the
// architect's view of a cross-repo workgroup's members, assembled without
// opening any backend workspace or cloning any target repository.
func stageProjectSpecView(members []trace.CrossRepoMember, workspace string) error {
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
		if spec, ok := latest[plan.SpecDocument]; ok {
			files[plan.SpecPath] = spec.Content
		}
		if p, ok := latest[plan.PlanDocument]; ok {
			files[plan.PlanPath] = p.Content
		}
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

func (s *Service) projectSpecDraftTool(host config.WorkstreamID) coreadapter.Tool {
	return coreadapter.Tool{Name: ProjectSpecDraftTool, Description: "Deliver the project spec: behavior, contracts, graph and each member's own contract-criteria numbers.", Effect: coreadapter.ToolMemory,
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
			if err := os.MkdirAll(filepath.Join(s.projectSpecTurnDirectoryFor(host), "output"), 0700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(s.projectSpecTurnDirectoryFor(host), "output", "project-spec.json"), data, 0600); err != nil {
				return nil, err
			}
			return json.RawMessage(`{}`), nil
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
// full project spec by filling in each member's own current spec.md and
// plan.json content as its slice, validates and records it, or records why
// the draft failed.
func (s *Service) recordProjectSpecDraft(ctx context.Context, wt *trace.Repository, g trace.Workgroup, host config.WorkstreamID, turn string, result coreadapter.OperationResult) error {
	if result.Outcome != "idle" {
		return s.failProjectSpecDraft(ctx, wt, host, "the architect's turn did not deliver a project spec: "+result.Evidence)
	}
	data, err := os.ReadFile(filepath.Join(s.projectSpecTurnDirectoryFor(host), "output", "project-spec.json"))
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
	slices := make([]trace.Slice, 0, len(g.Members)-1)
	criteria := map[config.WorkstreamID][]int{}
	for _, sl := range draft.Slices {
		criteria[sl.Workstream] = sl.ContractCriteria
	}
	for _, m := range g.Members[1:] {
		_, _, repo, api := s.conversationTrace(string(m))
		if api != nil {
			return fmt.Errorf("workstream %s: %s", m, api.Message)
		}
		specContent, planContent, err := memberSpecAndPlan(repo, m)
		if err != nil {
			return err
		}
		slices = append(slices, trace.Slice{Workstream: m, Spec: specContent, Plan: planContent, ContractCriteria: criteria[m]})
	}
	full := trace.ProjectSpec{Behavior: draft.Behavior, Contracts: draft.Contracts, Graph: draft.Graph, Slices: slices}
	if err := trace.RecordProjectSpec(ctx, wt, g, full, s.now(), projectSpecDraftActor, turn); err != nil {
		return s.failProjectSpecDraft(ctx, wt, host, "the delivered project spec is invalid: "+err.Error())
	}
	state, err := wt.Workflow(host, projectSpecDraftSubject)
	if err != nil {
		return err
	}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "project-spec-drafted", Revision: 1, Project: wt.Project(), Workstream: host, At: s.now(), Actor: projectSpecDraftActor, Cause: turn},
			Subject: projectSpecDraftSubject, From: state.Value, To: "drafted", Reason: "the project spec is recorded, covering every member"}}
	_, err = wt.Transact(ctx, tx)
	return err
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
		Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "project-spec-draft-failed", Revision: 1, Project: wt.Project(), Workstream: host, At: s.now(), Actor: projectSpecDraftActor, Cause: "project-spec-draft"},
			Subject: projectSpecDraftSubject, From: state.Value, To: "failed", Reason: reason},
		Events: []trace.Event{trace.Notice("project-spec-draft-failed", "chief", "The architect could not draft the workgroup's project spec: "+reason+". Raise an amendment to retry.")}}
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
		_, g, found, err := repository.CrossRepoProjectSpec(id)
		if errors.Is(err, trace.ErrLocked) {
			// Another member's own pass, or this workgroup's leader, holds
			// the workgroup trace open right now (for example, while
			// dispatching the architect's draft turn); try again next pass.
			continue
		}
		if err != nil || found || len(g.Members) < 2 {
			if err != nil {
				return fmt.Errorf("workgroup %s: %w", id, err)
			}
			continue
		}
		if leader := leastWorkstream(g.Members[1:]); leader != stream {
			continue
		}
		err = s.draftProjectSpecIfReady(ctx, repository, id)
		if errors.Is(err, trace.ErrLocked) {
			// The workgroup trace is busy; try again next pass.
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
