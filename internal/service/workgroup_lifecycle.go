package service

import (
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

// memberArchives reads durable retirement evidence. A service view without a
// runtime store has no evidence and cannot establish member retirement.
func (s *Service) memberArchives() []runtime.Archive {
	if s.store == nil {
		return nil
	}
	state, _ := s.store.Snapshot()
	return state.Archived
}

// archiveLifecycle reads the production resolution and locator while history
// still exists. Owner archive authorization remains independent of resolution.
func (s *Service) archiveLifecycle(repo *trace.Repository, a *runtime.Archive) error {
	feature, err := repo.Workflow(a.Workstream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	if feature.Value != DeliveredState && feature.Value != AbandonedState {
		return fmt.Errorf("archived member %s is not terminal; cleanup remains held", a.Workstream)
	}
	if a.State != "" && a.State != feature.Value {
		return fmt.Errorf("archive outcome for %s differs from its trace", a.Workstream)
	}
	a.State = feature.Value
	id, err := repo.CrossRepoWorkgroupOf(a.Workstream)
	if err != nil {
		return err
	}
	if a.Workgroup != "" && a.Workgroup != id {
		return fmt.Errorf("archive workgroup identity mismatch")
	}
	a.Workgroup = id
	if id != "" {
		authority, err := trace.OpenWorkgroupAuthority(s.current().Root, id)
		if err != nil {
			if authority != nil {
				authority.Close()
			}
			return err
		}
		defer authority.Close()
		groups, _, err := trace.Workgroups(authority)
		if err != nil {
			return err
		}
		for _, g := range groups {
			if g.ID == id && slices.Contains(g.Members[1:], a.Workstream) {
				if err := authority.PreserveMemberQuestions(context.Background(), g, trace.CrossRepoMember{Workstream: a.Workstream, Project: repo}, s.now(), serviceActor); err != nil {
					return err
				}
			}
		}
	}
	evidence, err := readMemberEvidence(repo, a.Workstream)
	if err != nil {
		return err
	}
	a.ResolutionEvidence = evidence.resolution(feature.Value)
	a.Resolved = a.ResolutionEvidence != "unresolved"
	evidence.pinArchiveMerge(a, s.about(repo).Project.Upstream)
	return nil
}

func (s *Service) installMemberLifecycle(repo *trace.Repository) {
	_ = repo.SetQuestionCommitBoundary(s.step)
	_ = repo.SetWorkgroupMemberLifecycle(s.memberLifecycle(repo.Project()))
}

func (s *Service) memberLifecycle(project config.ProjectID) trace.WorkgroupMemberLifecycle {
	return func(group config.WorkgroupID, stream config.WorkstreamID) (bool, error) {
		if !s.current().Active(project) {
			return false, fmt.Errorf("workgroup member repo %s is inactive", project)
		}
		for _, a := range s.memberArchives() {
			if a.Workstream != stream {
				continue
			}
			if a.Project != project || a.Workgroup != group {
				return false, fmt.Errorf("archived member %s identity does not match workgroup %s", stream, group)
			}
			path, err := s.current().Root.WorkstreamTrace(project, stream)
			if err != nil {
				return false, err
			}
			if _, err := os.Lstat(path); err != nil {
				if !os.IsNotExist(err) {
					return false, err
				}
				if a.CleanupStartedAt.IsZero() {
					return false, fmt.Errorf("archived member %s is missing without authorized cleanup intent", stream)
				}
			}
			return true, nil
		}
		return false, nil
	}
}

// workgroupMembers resolves exact approved identities without searching retired
// history. Before approval the ordinary join protocol still fences access.
func (s *Service) workgroupMembers(ctx context.Context, g trace.Workgroup) ([]trace.CrossRepoMember, bool, error) {
	authority, err := trace.OpenWorkgroupAuthority(s.current().Root, g.ID)
	if err != nil {
		if authority != nil {
			authority.Close()
		}
		return nil, false, err
	}
	defer authority.Close()
	approval, _, found, err := trace.ReadGroupApproval(authority, g.Members[0])
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	projects, err := trace.ApprovedWorkgroupMembers(authority, g, approval)
	if err != nil {
		return nil, false, err
	}
	members := make([]trace.CrossRepoMember, 0, len(projects))
	for stream, project := range projects {
		pin := trace.GroupMemberApproval{Workstream: stream, Project: project}
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if !slices.Contains(g.Members[1:], pin.Workstream) {
			return nil, false, fmt.Errorf("approved member %s is not in workgroup", pin.Workstream)
		}
		repo, err := s.repository(pin.Project)
		if err != nil {
			return nil, false, err
		}
		if repo.Project() != pin.Project {
			return nil, false, fmt.Errorf("approved member %s repository identity mismatch", pin.Workstream)
		}
		s.installMemberLifecycle(repo)
		resolveLifecycle := s.memberLifecycle(pin.Project)
		retired, err := resolveLifecycle(g.ID, pin.Workstream)
		if err != nil {
			return nil, false, err
		}
		if !retired {
			validateLive := func() error {
				id, err := repo.CrossRepoWorkgroupOf(pin.Workstream)
				if err != nil {
					return err
				}
				if id != g.ID {
					return errWorkgroupJoining
				}
				_, err = repo.Workflow(pin.Workstream, trace.FeatureSubject)
				return err
			}
			if liveErr := validateLive(); liveErr != nil {
				// A concurrent archive may retire the member after the first read.
				// Only matching durable evidence permits the terminal binding.
				retired, err = resolveLifecycle(g.ID, pin.Workstream)
				if err != nil {
					return nil, false, err
				}
				if !retired {
					return nil, false, liveErr
				}
			}
		}
		members = append(members, trace.CrossRepoMember{Workstream: pin.Workstream, Project: repo})
	}
	return members, true, nil
}

func (s *Service) liveWorkgroupMembers(g trace.Workgroup) ([]config.WorkstreamID, error) {
	if len(g.Members) < 2 {
		return nil, fmt.Errorf("workgroup %s has no recorded real members", g.ID)
	}
	var live []config.WorkstreamID
	for _, stream := range g.Members[1:] {
		retired := false
		for _, a := range s.memberArchives() {
			if a.Workstream != stream {
				continue
			}
			if a.Workgroup != g.ID {
				return nil, fmt.Errorf("archived member %s lacks resolution evidence", stream)
			}
			retired = true
		}
		if !retired {
			live = append(live, stream)
		}
	}
	return live, nil
}

func (s *Service) liveWorkgroupLeader(g trace.Workgroup) (config.WorkstreamID, error) {
	members, err := s.liveWorkgroupMembers(g)
	if err != nil || len(members) == 0 {
		return "", err
	}
	return leastWorkstream(members), nil
}

func (s *Service) firstLiveWorkgroupMember(g trace.Workgroup) (config.WorkstreamID, error) {
	members, err := s.liveWorkgroupMembers(g)
	if err != nil || len(members) == 0 {
		return "", err
	}
	return members[0], nil
}

// workgroupFullyRetired verifies retirement without opening member history.
// Approval pins and lifecycle bindings still validate every archived identity.
func (s *Service) workgroupFullyRetired(ctx context.Context, g trace.Workgroup) (bool, error) {
	members, err := s.liveWorkgroupMembers(g)
	if err != nil || len(members) != 0 {
		return false, err
	}
	authority, err := trace.OpenWorkgroupAuthority(s.current().Root, g.ID)
	if err != nil {
		if authority != nil {
			authority.Close()
		}
		return false, err
	}
	defer authority.Close()
	approval, _, approved, err := trace.ReadGroupApproval(authority, g.Members[0])
	if err != nil {
		return false, err
	}
	if !approved {
		return false, fmt.Errorf("workgroup %s retirement lacks approved member identities", g.ID)
	}
	projects, err := trace.ApprovedWorkgroupMembers(authority, g, approval)
	if err != nil {
		return false, err
	}
	for stream, project := range projects {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		// Fully retired groups need no member runtime, including during
		// startup before every configured repo's controller is published.
		retired, err := s.memberLifecycle(project)(g.ID, stream)
		if err != nil {
			return false, err
		}
		if !retired {
			return false, fmt.Errorf("workgroup %s member %s lacks archive evidence", g.ID, stream)
		}
	}
	return true, nil
}

// recoverArchiveLifecycle captures missing retirement evidence before shared
// session recovery or scheduling can need it. A retained trace is required;
// an older deletion without evidence remains held, never inferred terminal.
func (s *Service) recoverArchiveLifecycle(ctx context.Context) {
	for _, a := range s.memberArchives() {
		if err := ctx.Err(); err != nil {
			return
		}
		if a.ResolutionEvidence != "" || !a.CleanedAt.IsZero() {
			continue
		}
		repo, err := s.repository(a.Project)
		if err == nil {
			if a.Title == "" {
				var metadata runtime.Archive
				metadata, err = archiveTitle(repo, a.Workstream)
				if err == nil {
					a.Title, a.State = metadata.Title, metadata.State
				}
			}
			if err == nil {
				err = s.archiveLifecycle(repo, &a)
			}
			if err == nil {
				err = s.store.UpdateArchive(a)
			}
		}
		if err != nil {
			s.log().Warn("archive lifecycle recovery remains held", "project", a.Project, "workstream", a.Workstream, "error", err.Error())
		}
	}
}
