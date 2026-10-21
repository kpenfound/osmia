package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const affectedReposTool = "deliver_affected_repos"
const designStartSubject = "design-production-start"
const designSelectionDocument = "design-selection"
const designSelectionPath = "tools/design-selection.json"

type designMember struct {
	Repo       config.ProjectID    `json:"repo"`
	Workstream config.WorkstreamID `json:"workstream"`
}
type designSelection struct {
	Group   config.WorkgroupID `json:"group"`
	Members []designMember     `json:"members"`
}

func (s *Service) designRepos(repo config.ProjectID) []config.Project {
	cfg := s.current()
	var group config.ProjectID
	for _, p := range cfg.Projects {
		if p.ID == repo {
			group = p.Group()
		}
	}
	var out []config.Project
	for _, p := range cfg.Projects {
		if p.Group() == group && cfg.Active(p.ID) {
			out = append(out, p)
		}
	}
	return out
}

func designSelectionOf(r *trace.Repository, stream config.WorkstreamID) (designSelection, bool, error) {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return designSelection{}, false, err
	}
	for _, d := range docs {
		if d.ID == designSelectionDocument {
			var selected designSelection
			err := json.Unmarshal([]byte(d.Content), &selected)
			return selected, true, err
		}
	}
	return designSelection{}, false, nil
}

// designStartBlocksDraft keeps the source stream out of the ordinary drafter
// until repo selection has chosen the single-repo path. Allocated members use
// the ordinary drafter after the durable join is complete.
func (s *Service) designStartBlocksDraft(r *trace.Repository, stream config.WorkstreamID) (bool, error) {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return false, err
	}
	// Joined members carry copies of the handed input, but only its original
	// stream owns selection. The durable selection distinguishes that source
	// from its members even after conversation adoption.
	id, err := r.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return false, err
	}
	if id != "" {
		if _, source, err := designSelectionOf(r, stream); err != nil || !source {
			return false, err
		}
	}
	source := slices.ContainsFunc(docs, func(d trace.Document) bool {
		if d.ID != handedDocument {
			return false
		}
		_, issueErr := issues.Parse(d.Source)
		return d.Source == "design:"+string(stream) || d.Source == "stdin" || strings.HasPrefix(d.Source, "file:") || issueErr == nil
	})
	if !source || len(s.designRepos(r.Project())) < 2 {
		return false, nil
	}
	state, err := r.Workflow(stream, designStartSubject)
	return state.Value != "single", err
}

// designStartPass reconciles the owner's handed design before any member can
// draft or ask. All identities are recorded before allocating the first member.
func (s *Service) designStartPass(ctx context.Context, r *trace.Repository) error {
	streams, err := r.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		if err := ctx.Err(); err != nil {
			return err
		}
		feature, err := r.Workflow(stream, trace.FeatureSubject)
		if err != nil {
			return err
		}
		if feature.Value != HandedState {
			continue
		}
		blocked, err := s.designStartBlocksDraft(r, stream)
		if err != nil {
			return err
		}
		if !blocked {
			continue
		}
		state, err := r.Workflow(stream, designStartSubject)
		if err != nil {
			return err
		}
		if state.Value == "complete" || state.Value == "invalid" {
			continue
		}
		mu := designLock(r, stream)
		if !mu.TryLock() {
			continue
		}
		err = s.reconcileDesignStart(ctx, r, stream)
		mu.Unlock()
		if err != nil {
			return fmt.Errorf("design start %s: %w", stream, err)
		}
	}
	return nil
}

func (s *Service) designStartState(ctx context.Context, r *trace.Repository, stream config.WorkstreamID, to string) error {
	state, err := r.Workflow(stream, designStartSubject)
	if err != nil || state.Value == to {
		return err
	}
	h := ownerHeader("design-production-"+to, r.Project(), stream, designStartTransition, s.now())
	_, err = r.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{
		Header: h, Subject: designStartSubject, From: state.Value, To: to, Reason: "reconcile the owner-started design: " + to}})
	return err
}

func (s *Service) reconcileDesignStart(ctx context.Context, r *trace.Repository, stream config.WorkstreamID) error {
	selected, found, err := designSelectionOf(r, stream)
	if err != nil {
		return err
	}
	if !found {
		if s.options.Architect == nil {
			return nil
		}
		if err := s.selectDesignRepos(ctx, r, stream); err != nil {
			return err
		}
		selected, found, err = designSelectionOf(r, stream)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
	}
	if len(selected.Members) == 1 && selected.Members[0].Repo == r.Project() {
		return s.designStartState(ctx, r, stream, "single")
	}
	d := &drafter{s: s, repository: r}
	handed, err := d.handed(stream)
	if err != nil {
		return err
	}
	skipped, err := skippedDebate(r, stream)
	if err != nil {
		return err
	}
	base, err := r.WorkstreamBase(stream)
	if err != nil {
		return err
	}
	members := make([]trace.CrossRepoMember, 0, len(selected.Members))
	for _, member := range selected.Members {
		repo, err := s.repository(member.Repo)
		if err != nil {
			return err
		}
		streams, err := repo.Workstreams()
		if err != nil {
			return err
		}
		if !slices.Contains(streams, member.Workstream) {
			backend, err := s.newWorkspaces(ctx, s.about(repo))
			if err != nil {
				return err
			}
			if err := repo.CreateWorkstreamOn(ctx, member.Workstream, backend.Backend, handed.At, ownerActor); err != nil {
				return err
			}
		}
		if err := s.step("design-member-allocated"); err != nil {
			return err
		}
		docs, err := trace.Read[trace.Document](repo, member.Workstream)
		if err != nil {
			return err
		}
		copied := false
		for _, doc := range docs {
			if doc.ID == handedDocument {
				if doc.Content != handed.Content || doc.Source != handed.Source || doc.Path != handed.Path {
					return trace.ErrConflict
				}
				copied = true
			}
		}
		if !copied {
			doc := handed
			doc.Project, doc.Workstream = member.Repo, member.Workstream
			if err := repo.Append(ctx, doc); err != nil {
				return err
			}
		}
		if skipped {
			tx := trace.Transaction{Transition: trace.Transition{Header: ownerHeader(skipTransition, member.Repo, member.Workstream, handInTransition, handed.At), Subject: ownerSubject, To: skippedValue, Reason: handInSkipReason}}
			if _, err := repo.Transact(ctx, tx); err != nil {
				return err
			}
		}
		if base.Base != "" && member.Repo == r.Project() {
			recorded, err := repo.WorkstreamBase(member.Workstream)
			if err != nil {
				return err
			}
			if _, err := repo.SetWorkstreamBase(ctx, member.Workstream, base.Base, recorded.Revision, handed.At); err != nil {
				return err
			}
		}
		members = append(members, trace.CrossRepoMember{Workstream: member.Workstream, Project: repo})
	}
	if err := s.refreshRuntime(); err != nil {
		return err
	}
	wt, g, err := trace.PrepareDesignWorkgroup(ctx, s.current().Root, selected.Group, trace.ConversationOrigin{Project: r.Project(), Workstream: stream}, members, handed.At, ownerActor)
	if wt != nil {
		defer wt.Close()
	}
	if err != nil {
		return err
	}
	if err := s.step("design-workgroup-prepared"); err != nil {
		return err
	}
	// Locators are written before removing local chiefs. Startup therefore
	// cannot replace an adopted chief or turn an interrupted join into singles.
	for _, m := range members {
		if err := m.Project.RecordCrossRepoWorkgroup(ctx, m.Workstream, g.ID, handed.At, ownerActor, designStartTransition); err != nil {
			return err
		}
		if err := m.Project.RetractChiefOfStaff(ctx, m.Workstream); err != nil {
			return err
		}
		if err := m.Project.RetractWorkgroup(ctx, m.Workstream, handed.At, ownerActor, designStartTransition); err != nil {
			return err
		}
		if err := s.step("design-member-joined"); err != nil {
			return err
		}
	}
	if err := wt.AdoptDesignConversation(ctx, r, stream); err != nil {
		return err
	}
	if err := s.step("design-conversation-imported"); err != nil {
		return err
	}
	if err := r.RecordCrossRepoWorkgroup(ctx, stream, g.ID, handed.At, ownerActor, designStartTransition); err != nil {
		return err
	}
	if err := r.RetireDesignConversation(ctx, stream); err != nil {
		return err
	}
	if err := r.RetractWorkgroup(ctx, stream, handed.At, ownerActor, designStartTransition); err != nil {
		return err
	}
	if err := s.step("design-conversation-adopted"); err != nil {
		return err
	}
	if err := s.designStartState(ctx, r, stream, "active"); err != nil {
		return err
	}
	for _, m := range members {
		feature, err := m.Project.Workflow(m.Workstream, trace.FeatureSubject)
		if err != nil {
			return err
		}
		if feature.Value != "" {
			continue
		}
		if err := recordFactoryPolicy(ctx, m.Project, m.Workstream, handed.At, false); err != nil {
			return err
		}
		h := ownerHeader(designStartTransition, m.Project.Project(), m.Workstream, designStartTransition, handed.At)
		if _, err := m.Project.SetFeatureState(ctx, h, HandedState, "the owner started work from the immutable design brief"); err != nil {
			return err
		}
	}
	if err := s.designStartState(ctx, r, stream, "complete"); err != nil {
		return err
	}
	return s.refreshRuntime()
}

func (s *Service) selectDesignRepos(ctx context.Context, r *trace.Repository, stream config.WorkstreamID) error {
	const turn = "select-repos-1"
	d := &drafter{s: s, repository: r}
	if err := d.ensureThread(ctx, stream); err != nil {
		return err
	}
	t, err := r.Thread(stream, architectAgent)
	if err != nil {
		return err
	}
	directory := filepath.Join(s.current().Root.String(), "architect", string(stream), turn)
	if !queuedTurn(t, turn) {
		handed, err := d.handed(stream)
		if err != nil {
			return err
		}
		prompt, err := s.designSystemPrompt(ctx, s.about(r), r, s.now())
		if err != nil {
			return err
		}
		profile, _, err := s.roleExecution(s.current(), architectRole)
		if err != nil {
			return err
		}
		h := ownerHeader("request_"+turn, r.Project(), stream, designStartTransition, s.now())
		h.Schema = "osmia.trace.turn-request"
		_, err = r.EnqueueTurn(ctx, trace.TurnRequest{Header: h,
			AgentID: architectAgent, ThreadID: architectThread, TurnID: turn, Profile: profile,
			SystemPrompt: "You are the architect. Select every repo affected by the handed brief using deliver_affected_repos. Use only repo IDs from the project context. Go allocates members and retains the owner's gates.\n" + prompt, Prompt: handed.Content})
		if err != nil {
			return err
		}
	}
	_, settings, err := s.roleExecution(s.current(), architectRole)
	if err != nil {
		return err
	}
	turns := &isolation.Turns{Workspaces: stagedWorkspaces{}, Views: isolation.Views{Directory: filepath.Join(s.current().Root.String(), "views")},
		Select: func(_ context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
			paths, err := stageDesignWorkspace(directory)
			return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: directory, Directory: directory}, Paths: paths, Execution: settings}, err
		}, Grants: map[string]coreadapter.Capabilities{architectRole: {Tools: []string{affectedReposTool, designReadRepoToolName}}},
		Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			return []coreadapter.Tool{s.affectedReposDelivery(r, stream, scope), designReadRepoTool(s.current(), r, s.options.controls, scope)}, nil
		},
		Engine: s.options.Architect.Engine, Hosts: s.options.Architect.Hosts}
	dispatcher := thread.Dispatcher{Runner: s.threadRunner(s.current(), r, turns, s.now), Prepare: func(_ context.Context, _ thread.TurnInput) (coreadapter.PreparedTurn, error) {
		session := filepath.Join(directory, "session")
		return coreadapter.PreparedTurn{SessionDirectory: session}, os.MkdirAll(session, 0700)
	}}
	op, err := thread.TurnOperation(r.Project(), turn, thread.TurnInput{Workstream: stream, Agent: architectAgent, Turn: turn})
	if err != nil {
		return err
	}
	result, err := dispatcher.Apply(ctx, op)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if result.Outcome == "idle" || result.Outcome == "failed" {
		_, found, err := designSelectionOf(r, stream)
		if err != nil {
			return err
		}
		if !found {
			return s.designStartState(ctx, r, stream, "invalid")
		}
	}
	return nil
}

func (s *Service) affectedReposDelivery(r *trace.Repository, stream config.WorkstreamID, scope coreadapter.Scope) coreadapter.Tool {
	return coreadapter.Tool{Name: affectedReposTool, Effect: coreadapter.ToolMemory, Description: "Deliver the affected repo IDs. The service validates scope, records stable member identities and returns the recorded members.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"repos":{"type":"array","items":{"type":"string"},"minItems":1,"uniqueItems":true}},"required":["repos"],"additionalProperties":false}`),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			if scope.Role != architectRole || scope.Project != string(r.Project()) || scope.Workstream != string(stream) {
				return nil, errors.New("turn scope denied")
			}
			if err := r.ActiveTurn(scope); err != nil {
				return nil, err
			}
			var in struct {
				Repos []config.ProjectID `json:"repos"`
			}
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&in); err != nil {
				return nil, err
			}
			allowed := s.designRepos(r.Project())
			seen := map[config.ProjectID]bool{}
			if len(in.Repos) == 0 {
				return nil, errors.New("select at least one project repo")
			}
			selected := designSelection{Group: config.WorkgroupID("g_" + strings.TrimPrefix(string(stream), "w_"))}
			for _, repo := range in.Repos {
				if seen[repo] || !slices.ContainsFunc(allowed, func(p config.Project) bool { return p.ID == repo }) {
					return nil, fmt.Errorf("invalid, duplicate or outside-project repo %s", repo)
				}
				seen[repo] = true
				member := HandInWorkstream(repo, "design-"+string(stream))
				if len(in.Repos) == 1 && repo == r.Project() {
					member = stream
				}
				selected.Members = append(selected.Members, designMember{Repo: repo, Workstream: member})
			}
			base, err := r.WorkstreamBase(stream)
			if err != nil {
				return nil, err
			}
			if base.Base != "" && !seen[r.Project()] {
				return nil, errors.New("affected repos must include the repo of the handed workstream's base")
			}
			old, found, err := designSelectionOf(r, stream)
			if err != nil {
				return nil, err
			}
			data, err := json.Marshal(selected)
			if err != nil {
				return nil, err
			}
			recorded := func() (json.RawMessage, error) {
				return json.Marshal(struct {
					Recorded bool           `json:"recorded"`
					Members  []designMember `json:"members"`
				}{true, selected.Members})
			}
			if found {
				previous, _ := json.Marshal(old)
				if string(previous) != string(data) {
					return nil, trace.ErrConflict
				}
				return recorded()
			}
			h := ownerHeader(designSelectionDocument, r.Project(), stream, designStartTransition, s.now())
			h.Schema = "osmia.trace.document"
			if err := r.RecordDocuments(ctx, []trace.Document{{Header: h, Path: designSelectionPath, Content: string(data)}}); err != nil {
				return nil, err
			}
			if err := s.step("design-selection-recorded"); err != nil {
				return nil, err
			}
			return recorded()
		}}
}

// designMemberTrace resolves the continued design address to a real member
// for member-owned drafting and approval documents. Conversation routing keeps
// the original address and authoritative chief thread.
func (s *Service) designMemberTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	project, stream, repo, api := s.conversationTrace(raw)
	if api != nil {
		return project, stream, repo, api
	}
	id, err := repo.CrossRepoWorkgroupOf(stream)
	if err != nil {
		return "", "", nil, &APIError{Code: Internal, Message: err.Error()}
	}
	if id == "" {
		return project, stream, repo, nil
	}
	_, g, _, err := repo.CrossRepoProjectSpec(id)
	if err != nil {
		return "", "", nil, &APIError{Code: Internal, Message: err.Error()}
	}
	if g.Conversation != nil && g.Conversation.Workstream == stream && len(g.Members) > 1 {
		if _, err := s.crossRepoMemberRepos(context.Background(), g); err != nil {
			return "", "", nil, &APIError{Code: Conflict, Message: "the workgroup is still joining its members"}
		}
		leader, err := s.firstLiveWorkgroupMember(g)
		if err != nil {
			return "", "", nil, &APIError{Code: Conflict, Message: err.Error()}
		}
		if leader == "" {
			return "", "", nil, &APIError{Code: Conflict, Message: "the workgroup has no live member"}
		}
		return s.conversationTrace(string(leader))
	}
	return project, stream, repo, nil
}
