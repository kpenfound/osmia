package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

var groupApprovalLocks sync.Map

func (s *Service) groupApprovalLock(id config.WorkgroupID) *sync.Mutex {
	key := s.current().Root.String() + ":" + string(id)
	value, _ := groupApprovalLocks.LoadOrStore(key, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (s *Service) approvalTrace(ctx context.Context, repo *trace.Repository, id config.WorkgroupID) (*trace.Repository, trace.Workgroup, error) {
	_, g, _, err := repo.CrossRepoProjectSpec(id)
	if err != nil {
		return nil, g, err
	}
	members, err := s.crossRepoMemberRepos(ctx, g)
	if err != nil {
		return nil, g, err
	}
	wt, err := trace.OpenWorkgroupTrace(s.current().Root, id, members)
	if err != nil && wt != nil {
		wt.Close()
		wt = nil
	}
	return wt, g, err
}

// prepareGroupApproval replays only the decision's exact pinned records. Each
// member's durable record asks the existing sealer to reconcile its VCS effects.
func (s *Service) prepareGroupApproval(ctx context.Context, decision trace.GroupApproval) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	state, _ := s.store.Snapshot()
	for _, member := range decision.Members {
		for _, a := range state.Archived {
			if a.Workstream != member.Workstream {
				continue
			}
			if a.Project != member.Project || a.Workgroup != decision.Workgroup {
				return trace.ErrConflict
			}
			return &trace.RetiredMemberError{Project: member.Project, Workstream: member.Workstream}
		}
	}
	for _, member := range decision.Members {
		repo, err := s.approvalMemberRepository(ctx, member.Project)
		if err != nil {
			return err
		}
		latest, found, err := latestRatification(repo, member.Workstream)
		if err != nil {
			return err
		}
		var record shed.Ratification
		if err := json.Unmarshal(member.Record, &record); err != nil {
			return err
		}
		if record.Round != member.Round || record.Revision != (shed.Pin{Spec: member.Spec, Plan: member.Plan}) {
			return trace.ErrConflict
		}
		pinned, err := shed.EncodeRatification(record)
		if err != nil {
			return err
		}

		if found {
			encoded, err := shed.EncodeRatification(latest.Ratification)
			if err != nil {
				return err
			}
			if string(encoded) != string(pinned) {
				return fmt.Errorf("workstream %s ratification differs from the pinned group decision", member.Workstream)
			}
		} else {
			o, api := s.shedAction(string(member.Workstream), InShedState, SketchedState)
			if api != nil {
				if err := ctx.Err(); err != nil {
					return err
				}
				return fmt.Errorf("prepare %s: %s", member.Workstream, api.Message)
			}
			// Recovery uses the decision's round and encoded dispositions, never the
			// current draft or committee record.
			if api := o.record(ctx, shed.RatificationDocumentID(member.Round), shed.RatificationPath(member.Round), string(pinned), fmt.Sprintf("ratified-%d", member.Round), "prepare the pinned owner workgroup approval"); api != nil {
				return fmt.Errorf("prepare %s: %s", member.Workstream, api.Message)
			}
		}
		if err := s.step("group-member-decision-prepared"); err != nil {
			return err
		}
	}
	return nil
}

// approvalMemberRepository resolves a live member while the controller is
// running. Shutdown cancels its context before clearing active projects.
func (s *Service) approvalMemberRepository(ctx context.Context, project config.ProjectID) (*trace.Repository, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	repo, err := s.repository(project)
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return repo, err
}

func groupApprovalResponse(project config.ProjectID, stream config.WorkstreamID, decision trace.GroupApproval) RatifyResponse {
	out := RatifyResponse{Project: project, Workstream: stream, Sealing: "pending", Detail: "the owner approved the workgroup; member seal preparations are pending"}
	for _, member := range decision.Members {
		out.Members = append(out.Members, member.Workstream)
		if member.Workstream == stream {
			out.Round, out.Spec, out.Plan = member.Round, member.Spec, member.Plan
		}
	}
	if decision.Complete {
		out.Sealing = "succeeded"
		out.Detail = "every pinned member seal is complete; the workgroup is ratified"
	}
	return out
}

// resumeGroupApproval also serves retries after the members have left the shed.
func (s *Service) resumeGroupApproval(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, id config.WorkgroupID, asked shed.Pin) (RatifyResponse, bool, *APIError) {
	lock := s.groupApprovalLock(id)
	lock.Lock()
	defer lock.Unlock()
	if err := s.step("group-approval-retry-locked"); err != nil {
		return RatifyResponse{}, true, &APIError{Internal, err.Error()}
	}

	wt, g, err := s.approvalTrace(ctx, repo, id)
	if err != nil {
		return RatifyResponse{}, true, &APIError{Internal, err.Error()}
	}
	defer wt.Close()
	decision, _, found, err := trace.ReadGroupApproval(wt, g.Members[0])
	if err != nil {
		return RatifyResponse{}, true, &APIError{Internal, err.Error()}
	}
	if !found {
		return RatifyResponse{}, false, nil
	}
	matched := false
	for _, member := range decision.Members {
		if member.Workstream == stream && member.Spec == asked.Spec && member.Plan == asked.Plan {
			matched = true
		}
	}
	if !matched {
		return RatifyResponse{}, true, &APIError{Conflict, "the request differs from the immutable workgroup approval"}
	}
	if !decision.Complete {
		if err := s.prepareGroupApproval(ctx, decision); err != nil {
			return RatifyResponse{}, true, &APIError{Internal, err.Error()}
		}
	}
	out := groupApprovalResponse(repo.Project(), stream, decision)
	out.Sealing, out.Detail, err = s.groupApprovalDetail(decision)
	if err != nil {
		return RatifyResponse{}, true, &APIError{Internal, err.Error()}
	}
	return out, true, nil
}

// groupApprovalPass publishes completion only after every exact pinned sealing
// has a successful durable outcome. A failed preparation remains pending with
// the existing sealing diagnostics and prevents all group admission.
func (s *Service) groupApprovalPass(ctx context.Context, repo *trace.Repository) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	streams, err := repo.Workstreams()
	if err != nil {
		return err
	}
	for _, stream := range streams {
		id, err := repo.CrossRepoWorkgroupOf(stream)
		if err != nil {
			return err
		}
		if id == "" {
			continue
		}
		_, g, _, err := repo.CrossRepoProjectSpec(id)
		if err != nil {
			return err
		}
		if len(g.Members) < 2 {
			continue
		}
		// Completion is authoritative even when every member has retired and
		// only the adopted conversation's source locator remains.
		authority, err := trace.OpenWorkgroupAuthority(s.current().Root, id)
		if err != nil {
			if authority != nil {
				authority.Close()
			}
			return err
		}
		decision, _, found, readErr := trace.ReadGroupApproval(authority, g.Members[0])
		closeErr := authority.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if found && decision.Complete {
			continue
		}
		leader, err := s.liveWorkgroupLeader(g)
		if err != nil {
			return err
		}
		if leader == "" {
			s.log().Warn("workgroup approval remains held", "workgroup", id, "error", "no live member remains to reconcile incomplete approval")
			continue
		}
		if leader != stream {
			continue
		}
		lock := s.groupApprovalLock(id)
		if !lock.TryLock() {
			continue
		}
		err = s.reconcileGroupApproval(ctx, repo, id)
		lock.Unlock()
		if errors.Is(err, trace.ErrMemberRetired) {
			s.log().Warn("workgroup approval remains held", "workgroup", id, "error", err.Error())
			continue
		}
		if err != nil {
			if canceled := ctx.Err(); canceled != nil {
				return canceled
			}
			return err
		}
	}
	return nil
}

func (s *Service) reconcileGroupApproval(ctx context.Context, repo *trace.Repository, id config.WorkgroupID) error {
	wt, g, err := s.approvalTrace(ctx, repo, id)
	if errors.Is(err, errWorkgroupJoining) || errors.Is(err, trace.ErrLocked) || errors.Is(err, trace.ErrMembershipChanged) {
		return nil
	}
	if err != nil {
		return err
	}
	defer wt.Close()
	decision, _, found, err := trace.ReadGroupApproval(wt, g.Members[0])
	if err != nil || decision.Complete {
		return err
	}
	if !found {
		decision, found, err = s.recoverMemberApprovals(ctx, wt, g)
		if err != nil || !found {
			return err
		}
	}
	if err := s.prepareGroupApproval(ctx, decision); err != nil {
		return err
	}
	for _, member := range decision.Members {
		r, err := s.approvalMemberRepository(ctx, member.Project)
		if err != nil {
			return err
		}
		latest, found, err := latestRatification(r, member.Workstream)
		if err != nil || !found {
			return err
		}
		z := &sealer{s: s, repository: r}
		op, requested, err := z.sealing(member.Workstream, latest)
		if err != nil || !requested {
			return err
		}
		input, err := decodeSeal(op.Operation)
		if err != nil {
			return err
		}
		outcome, err := z.outcome(member.Workstream, input.Seal, op.Operation.ID)
		if err != nil {
			return err
		}
		if outcome == nil || outcome.Outcome != "succeeded" {
			return nil
		}
		prepared, _, found, err := seal.PreparedLatest(r, member.Workstream)
		if err != nil {
			return err
		}
		if !found || prepared.Round != member.Round || prepared.Revision != (shed.Pin{Spec: member.Spec, Plan: member.Plan}) || prepared.Seal != input.Seal {
			return trace.ErrConflict
		}
		if err := s.step("group-member-seal-prepared"); err != nil {
			return err
		}
	}
	if err := s.step("group-before-completion"); err != nil {
		return err
	}
	decision.Complete = true
	if err := trace.RecordGroupApproval(ctx, wt, g.Members[0], decision, s.now(), sealingActor); err != nil {
		return err
	}
	return s.step("group-completed")
}

// recoverMemberApprovals preserves existing owner decisions recorded before
// a group completion receipt exists. Every member must have its own decision,
// and the recorded project slices must match those exact revisions. Missing
// coverage grants nothing; the existing owner ratification remains available.
func (s *Service) recoverMemberApprovals(ctx context.Context, wt *trace.Repository, g trace.Workgroup) (trace.GroupApproval, bool, error) {
	spec, found, err := trace.ProjectSpecFor(wt, g.ID)
	if err != nil || !found {
		return trace.GroupApproval{}, false, err
	}
	if err := trace.ValidateGraph(g.Members[1:], spec.Graph); err != nil {
		return trace.GroupApproval{}, false, nil
	}
	decision := trace.GroupApproval{Workgroup: g.ID, ProjectSpec: spec}
	for _, stream := range g.Members[1:] {
		project, _, repo, api := s.conversationTrace(string(stream))
		if api != nil {
			return trace.GroupApproval{}, false, fmt.Errorf("recover member approval: %s", api.Message)
		}
		r, found, err := latestRatification(repo, stream)
		if err != nil || !found {
			return trace.GroupApproval{}, false, err
		}
		slice, covered := trace.SliceFor(spec, stream)
		if !covered {
			return trace.GroupApproval{}, false, nil
		}
		specDoc, err := trace.Get[trace.Document](repo, stream, plan.SpecDocument, r.Revision.Spec)
		if err != nil {
			return trace.GroupApproval{}, false, err
		}
		planDoc, err := trace.Get[trace.Document](repo, stream, plan.PlanDocument, r.Revision.Plan)
		if err != nil {
			return trace.GroupApproval{}, false, err
		}
		if slice.Spec != specDoc.Content || slice.Plan != planDoc.Content {
			return trace.GroupApproval{}, false, nil
		}
		content, err := shed.EncodeRatification(r.Ratification)
		if err != nil {
			return trace.GroupApproval{}, false, err
		}
		decision.Members = append(decision.Members, trace.GroupMemberApproval{Project: project, Workstream: stream, Round: r.Round, Spec: r.Revision.Spec, Plan: r.Revision.Plan, Record: content})
	}
	if err := trace.RecordGroupApproval(ctx, wt, g.Members[0], decision, s.now(), sealingActor); err != nil {
		return trace.GroupApproval{}, false, err
	}
	return decision, true, nil
}

func (s *Service) groupApprovalDetail(decision trace.GroupApproval) (string, string, error) {
	if decision.Complete {
		return "succeeded", "every pinned member seal is complete; the workgroup is ratified", nil
	}
	for _, member := range decision.Members {
		repo, err := s.repository(member.Project)
		if err != nil {
			return "", "", err
		}
		latest, found, err := latestRatification(repo, member.Workstream)
		if err != nil {
			return "", "", err
		}
		if !found {
			continue
		}
		z := &sealer{s: s, repository: repo}
		op, requested, err := z.sealing(member.Workstream, latest)
		if err != nil {
			return "", "", err
		}
		if !requested {
			continue
		}
		input, err := decodeSeal(op.Operation)
		if err != nil {
			return "", "", err
		}
		outcome, err := z.outcome(member.Workstream, input.Seal, op.Operation.ID)
		if err != nil {
			return "", "", err
		}
		if outcome != nil && outcome.Outcome == "failed" {
			return "failed", outcome.Evidence + "; the entire workgroup remains unratified", nil
		}
		_, reason, _ := progress(op)
		if reason != "" {
			return "pending", fmt.Sprintf("member %s sealing is pending after a failed attempt: %s; the entire workgroup remains unratified", member.Workstream, reason), nil
		}
	}
	return "pending", "the owner approved the workgroup; member seal preparations are pending", nil
}
