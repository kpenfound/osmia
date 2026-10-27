package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

const DesignStartSubject = "design-production-start"

const DesignSelectionDocument = "design-selection"

const DesignSelectionPath = "owner/projects.json"

const DesignStartIntentDocument = "design-start-intent"

// DesignStartIntent binds the owner-started text, revision and ordered list
// before any handed input, owner decision or member allocation is recorded.
type DesignStartIntent struct {
	BriefRevision int                `json:"brief_revision"`
	Projects      []config.ProjectID `json:"projects"`
	Handed        trace.Document     `json:"handed"`
}

func DesignStartIntentOf(r *trace.Repository, stream config.WorkstreamID) (DesignStartIntent, bool, error) {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return DesignStartIntent{}, false, err
	}
	for _, d := range docs {
		if d.ID == DesignStartIntentDocument {
			var intent DesignStartIntent
			err := json.Unmarshal([]byte(d.Content), &intent)
			return intent, true, err
		}
	}
	return DesignStartIntent{}, false, nil
}

func (s *Controller) PreparePinnedDesignStart(ctx context.Context, r *trace.Repository, stream config.WorkstreamID, intent DesignStartIntent) error {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return err
	}
	found := false
	for _, d := range docs {
		if d.ID != HandedDocument {
			continue
		}
		if d.Content != intent.Handed.Content || d.Source != intent.Handed.Source || d.Path != intent.Handed.Path {
			return trace.ErrConflict
		}
		found = true
	}
	if !found {
		if err := r.Append(ctx, intent.Handed); err != nil {
			return err
		}
		if err := s.Step("design-brief-handed"); err != nil {
			return err
		}
	}
	if api := s.RecordOwnerProjects(ctx, r, stream, intent.Projects, ""); api != nil {
		return fmt.Errorf("%s", api.Message)
	}
	return nil
}

type DesignMember struct {
	Repo       config.ProjectID    `json:"repo"`
	Workstream config.WorkstreamID `json:"workstream"`
}

type DesignSelection struct {
	Group       config.WorkgroupID  `json:"group"`
	Members     []DesignMember      `json:"members"`
	Base        config.WorkstreamID `json:"base,omitempty"`
	BaseProject config.ProjectID    `json:"base_project,omitempty"`
}

func DesignSelectionOf(r *trace.Repository, stream config.WorkstreamID) (DesignSelection, bool, error) {
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return DesignSelection{}, false, err
	}
	for _, d := range docs {
		if d.ID == DesignSelectionDocument {
			var selected DesignSelection
			err := json.Unmarshal([]byte(d.Content), &selected)
			return selected, true, err
		}
	}
	return DesignSelection{}, false, nil
}

// DesignStartPending holds an allocation until its durable join is complete.
// A handed design without a selection waits for the owner's upgrade decision.
func (s *Controller) DesignStartPending(r *trace.Repository, stream config.WorkstreamID) (bool, error) {
	selected, found, err := DesignSelectionOf(r, stream)
	if err != nil {
		return false, err
	}
	if found {
		state, err := r.Workflow(stream, DesignStartSubject)
		if err != nil {
			return false, err
		}
		if len(selected.Members) == 1 && selected.Members[0].Workstream == stream {
			return state.Value != "single", nil
		}
		return true, nil // the conversation address is not a drafting member
	}
	docs, err := trace.Read[trace.Document](r, stream)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(docs, func(d trace.Document) bool {
		return d.ID == DesignStartIntentDocument || (d.ID == HandedDocument && d.Source == "design:"+string(stream))
	}), nil
}

// designStartPass reconciles the owner's handed design before any member can
// draft or ask. All identities are recorded before allocating the first member.
func (s *Controller) DesignStartPass(ctx context.Context, r *trace.Repository) error {
	streams, err := s.designStartWorkstreams(r)
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
		if feature.Value != HandedState && feature.Value != "" {
			continue
		}
		blocked, err := s.DesignStartPending(r, stream)
		if err != nil {
			return err
		}
		if !blocked {
			continue
		}
		if feature.Value == "" {
			docs, err := trace.Read[trace.Document](r, stream)
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(docs, func(d trace.Document) bool {
				return d.ID == DesignStartIntentDocument || (d.ID == HandedDocument && d.Source == "design:"+string(stream))
			}) {
				continue
			}
		}
		state, err := r.Workflow(stream, DesignStartSubject)
		if err != nil {
			return err
		}
		if state.Value == "complete" || state.Value == "invalid" {
			continue
		}
		mu := DesignLock(r, stream)
		if !mu.TryLock() {
			continue
		}
		err = s.ReconcileDesignStart(ctx, r, stream)
		mu.Unlock()
		if err != nil {
			return fmt.Errorf("design start %s: %w", stream, err)
		}
	}
	return nil
}

func (s *Controller) DesignStartState(ctx context.Context, r *trace.Repository, stream config.WorkstreamID, to string) error {
	state, err := r.Workflow(stream, DesignStartSubject)
	if err != nil || state.Value == to {
		return err
	}
	h := OwnerHeader("design-production-"+to, r.Project(), stream, DesignStartTransition, s.Now())
	_, err = r.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{
		Header: h, Subject: DesignStartSubject, From: state.Value, To: to, Reason: "reconcile the owner-started design: " + to}})
	return err
}

func (s *Controller) ReconcileDesignStart(ctx context.Context, r *trace.Repository, stream config.WorkstreamID) error {
	if intent, found, err := DesignStartIntentOf(r, stream); err != nil {
		return err
	} else if found {
		if err := s.PreparePinnedDesignStart(ctx, r, stream, intent); err != nil {
			return err
		}
	}
	selected, found, err := DesignSelectionOf(r, stream)
	if err != nil {
		return err
	}
	if !found {
		return s.DesignStartState(ctx, r, stream, "awaiting-projects")
	}

	d := &Drafter{S: s.DrafterDependencies(), Repository: r}
	handed, err := d.Handed(stream)
	if err != nil {
		return err
	}
	if handed.Source == "design:"+string(stream) {
		if err := s.ResumeHandedDesign(ctx, r, stream, handed); err != nil {
			return err
		}
	}
	if len(selected.Members) == 1 && selected.Members[0].Workstream == stream {
		if err := s.DesignStartState(ctx, r, stream, "single"); err != nil {
			return err
		}
		return s.RefreshRuntime()
	}
	skipped, err := SkippedDebate(r, stream)
	if err != nil {
		return err
	}
	base, err := r.WorkstreamBase(stream)
	if err != nil {
		return err
	}
	members := make([]trace.CrossRepoMember, 0, len(selected.Members))
	for _, member := range selected.Members {
		repo, err := s.Repository(member.Repo)
		if err != nil {
			return err
		}
		streams, err := repo.Workstreams()
		if err != nil {
			return err
		}
		if !slices.Contains(streams, member.Workstream) {
			backend, err := s.NewWorkspaces(ctx, s.About(repo))
			if err != nil {
				return err
			}
			if err := repo.CreateWorkstreamOn(ctx, member.Workstream, backend.Backend, handed.At, OwnerActor); err != nil {
				return err
			}
		}
		if err := s.Step("design-member-allocated"); err != nil {
			return err
		}
		docs, err := trace.Read[trace.Document](repo, member.Workstream)
		if err != nil {
			return err
		}
		copied := false
		for _, doc := range docs {
			if doc.ID == HandedDocument {
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
		if err := s.Step("design-member-brief-recorded"); err != nil {
			return err
		}
		if skipped {
			tx := trace.Transaction{Transition: trace.Transition{Header: OwnerHeader(SkipTransition, member.Repo, member.Workstream, HandInTransition, handed.At), Subject: OwnerSubject, To: SkippedValue, Reason: HandInSkipReason}}
			if _, err := repo.Transact(ctx, tx); err != nil {
				return err
			}
		}
		memberBase := selected.Base
		baseProject := selected.BaseProject
		if memberBase == "" {
			memberBase, baseProject = base.Base, r.Project()
		}
		if memberBase != "" && member.Repo == baseProject {
			recorded, err := repo.WorkstreamBase(member.Workstream)
			if err != nil {
				return err
			}
			if _, err := repo.SetWorkstreamBase(ctx, member.Workstream, memberBase, recorded.Revision, handed.At); err != nil {
				return err
			}
		}
		members = append(members, trace.CrossRepoMember{Workstream: member.Workstream, Project: repo})
	}
	if err := s.RefreshRuntime(); err != nil {
		return err
	}
	wt, g, err := trace.PrepareDesignWorkgroup(ctx, s.Current().Root, selected.Group, trace.ConversationOrigin{Project: r.Project(), Workstream: stream}, members, handed.At, OwnerActor)
	if wt != nil {
		defer wt.Close()
	}
	if err != nil {
		return err
	}
	if err := s.Step("design-workgroup-prepared"); err != nil {
		return err
	}
	// Locators are written before removing local chiefs. Startup therefore
	// cannot replace an adopted chief or turn an interrupted join into singles.
	for _, m := range members {
		if err := m.Project.RecordCrossRepoWorkgroup(ctx, m.Workstream, g.ID, handed.At, OwnerActor, DesignStartTransition); err != nil {
			return err
		}
		if err := m.Project.RetractChiefOfStaff(ctx, m.Workstream); err != nil {
			return err
		}
		if err := m.Project.RetractWorkgroup(ctx, m.Workstream, handed.At, OwnerActor, DesignStartTransition); err != nil {
			return err
		}
		if err := s.Step("design-member-joined"); err != nil {
			return err
		}
	}
	if err := wt.AdoptDesignConversation(ctx, r, stream); err != nil {
		return err
	}
	if err := s.Step("design-conversation-imported"); err != nil {
		return err
	}
	if err := r.RecordCrossRepoWorkgroup(ctx, stream, g.ID, handed.At, OwnerActor, DesignStartTransition); err != nil {
		return err
	}
	if err := r.RetireDesignConversation(ctx, stream); err != nil {
		return err
	}
	if err := r.RetractWorkgroup(ctx, stream, handed.At, OwnerActor, DesignStartTransition); err != nil {
		return err
	}
	if err := s.Step("design-conversation-adopted"); err != nil {
		return err
	}
	if err := s.DesignStartState(ctx, r, stream, "active"); err != nil {
		return err
	}
	if err := s.Step("design-start-active"); err != nil {
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
		if err := RecordFactoryPolicy(ctx, m.Project, m.Workstream, handed.At, false); err != nil {
			return err
		}
		h := OwnerHeader(DesignStartTransition, m.Project.Project(), m.Workstream, DesignStartTransition, handed.At)
		if _, err := m.Project.SetFeatureState(ctx, h, HandedState, "the owner started work from the immutable design brief"); err != nil {
			return err
		}
		if err := s.Step("design-member-handed"); err != nil {
			return err
		}
	}
	if err := s.DesignStartState(ctx, r, stream, "complete"); err != nil {
		return err
	}
	if err := s.Step("design-start-complete"); err != nil {
		return err
	}
	return s.RefreshRuntime()
}

// designMemberTrace resolves the continued design address to a real member
// for member-owned drafting and approval documents. Conversation routing keeps
// the original address and authoritative chief thread.
func (s *Controller) DesignMemberTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	project, stream, repo, api := s.ConversationTrace(raw)
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
		if _, err := s.CrossRepoMemberRepos(context.Background(), g); err != nil {
			return "", "", nil, &APIError{Code: Conflict, Message: "the workgroup is still joining its members"}
		}
		leader, err := s.FirstLiveWorkgroupMember(g)
		if err != nil {
			return "", "", nil, &APIError{Code: Conflict, Message: err.Error()}
		}
		if leader == "" {
			return "", "", nil, &APIError{Code: Conflict, Message: "the workgroup has no live member"}
		}
		return s.ConversationTrace(string(leader))
	}
	return project, stream, repo, nil
}

// ResumeHandedDesign finishes an interrupted handoff using its immutable input.
func (s *Controller) ResumeHandedDesign(ctx context.Context, r *trace.Repository, stream config.WorkstreamID, handed trace.Document) error {
	feature, err := r.Workflow(stream, trace.FeatureSubject)
	if err != nil || feature.Value != "" {
		return err
	}
	if err := RecordFactoryPolicy(ctx, r, stream, handed.At, false); err != nil {
		return err
	}
	if err := s.Step("design-factory-policy-recorded"); err != nil {
		return err
	}
	design, err := r.Workflow(stream, DesignSessionSubject)
	if err != nil {
		return err
	}
	header := OwnerHeader(DesignStartTransition, r.Project(), stream, DesignStartTransition, handed.At)
	closeHeader := OwnerHeader(DesignStartTransition+"-design-session", r.Project(), stream, DesignStartTransition, handed.At)
	closeTx := trace.Transaction{ExpectedVersion: design.Version, Transition: trace.Transition{Header: closeHeader, Subject: DesignSessionSubject, From: DesignOpenValue, To: DesignClosedValue, Reason: "the owner started a workgroup from the design session's brief"}}
	_, err = r.MoveFeatureStateWith(ctx, header, "", HandedState, "the owner started a workgroup from the design session's brief", closeTx)
	if err != nil {
		return err
	}
	return s.Step("design-session-handed")
}
