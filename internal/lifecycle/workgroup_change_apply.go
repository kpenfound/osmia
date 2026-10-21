package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/pulls"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func (s *Controller) ApplyChange(ctx context.Context, wt *trace.Repository, g trace.Workgroup, c *WorkgroupChange) error {
	// Production holds group coordination before this guard, then takes
	// member locks. An owner archive holds only the shared guard. If archive
	// owns this boundary, leave the original journal for the next live pass.
	lock := s.ChangeApplicationLock(g.ID)
	if !lock.TryLock() {
		return nil
	}
	defer lock.Unlock()
	// Archive may finish after the observer's read but before this lock.
	// Consume the current decision and retirement proof under the same guard
	// that excludes archive from every journal write and member effect.
	changes, err := ReadChanges(wt, c.Host)
	if err != nil {
		return err
	}
	index := slices.IndexFunc(changes, func(latest WorkgroupChange) bool { return latest.ID == c.ID })
	if index < 0 {
		return nil
	}
	*c = changes[index]
	if c.State != "approved" && c.State != "applying" && c.State != "releasing" {
		return nil
	}
	if ChangeSharedRepository(g, wt) {
		retired, err := s.WorkgroupFullyRetired(ctx, g)
		if err != nil {
			return err
		}
		if retired {
			s.Log().Warn("workgroup changes remain held", "workgroup", g.ID, "error", "no live member remains to reconcile pending project decisions")
			return nil
		}
	}
	if c.Request.Kind == "amend" && c.Result == nil {
		data, _ := json.Marshal(c.After)
		var result trace.ProjectSpec
		_ = json.Unmarshal(data, &result)
		c.Result = &result
	}
	var holds []string

	// Install every barrier before any member receives changed documents. A
	// crash during this loop resumes it; approval has already pinned the delta.
	for _, row := range c.Impact.Streams {
		if c.State == "releasing" || row.Action == amendment.Leave || row.Action == amendment.FollowUp {
			continue
		}
		repo, err := s.ChangeMemberRepository(wt, row.Workstream)
		if errors.Is(err, trace.ErrMemberRetired) {
			holds = append(holds, err.Error())
			continue
		}
		if err != nil {
			return err
		}
		if err := s.ChangeBoundary(ctx, repo, row.Workstream, *c, false); err != nil {
			return err
		}
	}
	if c.State == "approved" {
		c.State = "applying"
		if err := s.SaveChange(ctx, wt, c); err != nil {
			return err
		}
	}
	if len(holds) > 0 {
		if err := s.HoldChange(ctx, wt, c, strings.Join(holds, "; ")); err != nil {
			return err
		}
	}
	// Every available affected member reaches a safe boundary before the
	// first effect. A retired prerequisite remains held independently.
	for _, row := range c.Impact.Streams {
		if row.Action == amendment.Leave || slices.Contains(c.Applied, row.Workstream) {
			continue
		}
		repo, err := s.ChangeMemberRepository(wt, row.Workstream)
		if errors.Is(err, trace.ErrMemberRetired) {
			if !slices.Contains(holds, err.Error()) {
				holds = append(holds, err.Error())
			}
			continue
		}
		if err != nil {
			return err
		}
		threads, err := repo.Threads(row.Workstream)
		if err != nil {
			return err
		}
		for _, t := range threads {
			if t.Identity.Role == trace.ChiefOfStaff {
				continue
			}
			for _, q := range t.Turns {
				if q.Response != nil && q.CompletedAt.IsZero() && q.Claim != nil {
					if err := repo.CompleteTurn(ctx, row.Workstream, t.Identity.ID, q.Request.TurnID, q.Claim.Token, s.Now()); err != nil && !errors.Is(err, trace.ErrClaim) {
						return err
					}
				}
			}
		}
		idle, err := ChangeIdle(repo, row.Workstream)
		if err != nil || !idle {
			return err
		}
	}
	for n, row := range c.Impact.Streams {
		if row.Action == amendment.Leave || slices.Contains(c.Applied, row.Workstream) {
			continue
		}
		repo, err := s.ChangeMemberRepository(wt, row.Workstream)
		if errors.Is(err, trace.ErrMemberRetired) {
			if !slices.Contains(holds, err.Error()) {
				holds = append(holds, err.Error())
			}
			continue
		}
		if err != nil {
			return err
		}
		if c.Request.Kind == "amend" && row.Action != amendment.FollowUp {
			merged, err := s.ChangeMemberMerged(ctx, repo, row.Workstream)
			if err != nil {
				return err
			}
			if merged {
				row.Action = amendment.FollowUp
				c.Impact.Streams[n] = row
				if err := s.SaveChange(ctx, wt, c); err != nil {
					return err
				}
			}
		}
		if err := s.Step("change-before-member-effect"); err != nil {
			return err
		}
		repo, err = s.ChangeMemberRepository(wt, row.Workstream)
		if errors.Is(err, trace.ErrMemberRetired) {
			if !slices.Contains(holds, err.Error()) {
				holds = append(holds, err.Error())
			}
			continue
		}
		if err != nil {
			return err
		}
		if c.Request.Kind == "cancel" {
			if err := s.CancelChangeMember(ctx, repo, row.Workstream, *c); err != nil {
				return err
			}
		} else {
			if row.Action == amendment.FollowUp {
				ready, err := s.CreateChangeFollowup(ctx, wt, &g, repo, row, c)
				if err != nil || !ready {
					return err
				}
			} else {
				if err := s.ReworkChangeMember(ctx, repo, row, c); err != nil {
					return err
				}
				ready, err := s.ChangeSealReady(ctx, repo, row.Workstream, c.ID)
				if err != nil || !ready {
					return err
				}
			}
		}
		c.Applied = append(c.Applied, row.Workstream)
		if err := s.SaveChange(ctx, wt, c); err != nil {
			return err
		}
		if err := s.Step("change-member-applied"); err != nil {
			return err
		}
	}
	if len(holds) > 0 {
		return s.HoldChange(ctx, wt, c, strings.Join(holds, "; "))
	}
	if err := s.HoldChange(ctx, wt, c, ""); err != nil {
		return err
	}
	if c.Request.Kind == "amend" {
		if err := trace.RecordWorkgroupRevision(ctx, wt, g, c.ID, *c.Result, s.Now(), ownerActor); err != nil {
			return err
		}
		if err := trace.ReopenWorkgroup(ctx, wt, g.ID, s.Now(), amendmentActor, WorkgroupChangeDocument(c.ID)); err != nil {
			return err
		}
	}
	if c.State != "releasing" {
		c.State = "releasing"
		if err := s.SaveChange(ctx, wt, c); err != nil {
			return err
		}
	}
	// Release only after every member and the shared spec are reconciled.
	for _, row := range c.Impact.Streams {
		if row.Action == amendment.Leave {
			continue
		}
		repo, err := s.ChangeMemberRepository(wt, row.Workstream)
		if err != nil {
			return err
		}
		if err := s.ChangeBoundary(ctx, repo, row.Workstream, *c, true); err != nil {
			return err
		}
	}
	for _, id := range c.Followups {
		_, _, repo, api := s.ConversationTrace(string(id))
		if api != nil {
			return errors.New(api.Message)
		}
		if err := s.ChangeBoundary(ctx, repo, id, *c, true); err != nil {
			return err
		}
	}
	if err := s.SettleChangeRequester(ctx, *c); err != nil {
		return err
	}
	c.State = "applied"
	return s.SaveChange(ctx, wt, c)
}
func (s *Controller) ChangeMemberMerged(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID) (bool, error) {
	if _, merged, err := deliveryMerge(repo, stream); err != nil || merged {
		return merged, err
	}
	if o, rev, err := ownPullRequestOutcomeAt(repo, stream); err != nil {
		return false, err
	} else if rev > 0 && o.Outcome == ownPullRequestOutcomeMerged {
		return true, nil
	}
	p, found, err := currentOwnPublication(repo, stream)
	if err != nil || !found || p.PullRequest == 0 || s.Options.PullRequests == nil {
		return false, err
	}
	prs, err := s.Options.PullRequests.Find(ctx, p.Upstream, p.Fork, p.Branch)
	if err != nil {
		return false, err
	}
	if err := recordOwnPullRequestOutcomeFromFind(ctx, repo, s.Now(), stream, "change-boundary", p.PullRequest, prs); err != nil {
		return false, err
	}
	return slices.ContainsFunc(prs, func(pr pulls.PullRequest) bool {
		return pr.Number == p.PullRequest && pr.Merged
	}), nil
}
func (s *Controller) ChangeSealReady(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, id string) (bool, error) {
	if err := (&sealer{S: s.Intake().SealerDependencies(), Repository: repo}).Reconcile(ctx, stream); err != nil {
		return false, err
	}
	_, _, found, err := seal.PreparedLatest(repo, stream)
	if err != nil || !found {
		return false, err
	}
	st, err := repo.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return false, err
	}
	if st.Value == trace.SealPreparedState {
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "change-" + id + "-ratified", Revision: 1, Project: repo.Project(), Workstream: stream, At: s.Now(), Actor: amendmentActor, Cause: WorkgroupChangeDocument(id)}
		_, err = repo.MoveFeatureState(ctx, h, trace.SealPreparedState, RatifiedState, "the owner-approved delta seal is complete")
	}
	return st.Value == trace.SealPreparedState || st.Value == RatifiedState || st.Value == BuildingState || st.Value == AssembledState || st.Value == DeliveredState, err
}
func (s *Controller) CancelChangeMember(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, c WorkgroupChange) error {
	// Inspect the actual current PR before disposition. A merge occurring after
	// the impact was approved is retained rather than closed, drafted or undone.
	if p, found, err := currentOwnPublication(repo, stream); err != nil {
		return err
	} else if found && p.PullRequest > 0 {
		client := s.Options.PullRequests
		if client == nil {
			return errNoPullRequestCredential
		}
		prs, err := client.Find(ctx, p.Upstream, p.Fork, p.Branch)
		if err != nil {
			return err
		}
		for _, pr := range prs {
			if pr.Number == p.PullRequest {
				if pr.Merged {
					return nil
				}
				if pr.State == "open" {
					disposition, ok := client.(pulls.CancellationClient)
					if !ok {
						return errors.New("pull request client does not support cancellation disposition")
					}
					var disposed pulls.PullRequest
					if c.Decision.PullRequests == "close" {
						disposed, err = disposition.Close(ctx, p.Upstream, pr.Number)
					} else {
						disposed, err = disposition.Draft(ctx, p.Upstream, pr.Number)
					}
					if err != nil {
						return err
					}
					if disposed.Merged {
						return nil
					}
					if err := s.Step("cancel-pr-disposition"); err != nil {
						return err
					}
				}
			}
		}
	}
	if _, merged, err := deliveryMerge(repo, stream); err != nil || merged {
		return err
	}
	if o, rev, err := ownPullRequestOutcomeAt(repo, stream); err != nil {
		return err
	} else if rev > 0 && o.Outcome == ownPullRequestOutcomeMerged {
		return nil
	}
	st, err := repo.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	if st.Value != AbandonedState {
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "cancel-" + c.ID, Revision: 1, Project: repo.Project(), Workstream: stream, At: s.Now(), Actor: ownerActor, Cause: WorkgroupChangeDocument(c.ID)}
		_, err = repo.SetFeatureStateUnless(ctx, h, AbandonedState, c.Request.Change, AbandonedState)
		if err != nil {
			return err
		}
	}
	// Existing abandonment cleanup snapshots unlanded files onto unit branches.
	_, err = repo.CancelTurns(ctx, stream, s.Now(), abandonActor, "the owner cancelled the workstream")
	return err
}

// A protected contract request parks its origin just like a local amendment.
// An applied delta already queues changed workers; a rejected delta or an
// unchanged origin instead resumes its preserved stage with the decision.
func (s *Controller) SettleChangeRequester(ctx context.Context, c WorkgroupChange) error {
	if c.Requester == nil || c.Decision == nil {
		return nil
	}
	pa := *c.Requester
	if a, archived := s.ArchivedWorkstream(string(pa.Origin)); archived {
		retired, err := s.MemberLifecycle(a.Project)(c.Workgroup, pa.Origin)
		if err != nil {
			return err
		}
		if retired {
			return fmt.Errorf("%w: requester %s", trace.ErrMemberRetired, pa.Origin)
		}
	}
	_, _, repo, api := s.ConversationTrace(string(pa.Origin))
	if api != nil {
		return errors.New(api.Message)
	}
	if gone, err := abandoned(repo, pa.Origin); err != nil || gone {
		return err
	}
	th, err := repo.Thread(pa.Origin, pa.Requester.ID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	req := trace.Amendment{Header: trace.Header{ID: "group-" + c.ID, Workstream: pa.Origin, Unit: th.Identity.Unit}, Requester: pa.Requester, Role: th.Identity.Role, Thread: th.Identity.ThreadID, QuestionID: pa.QuestionID, Citations: pa.Citations, Change: pa.Change, Reason: pa.Reason}
	if pa.QuestionID != "" {
		questions, err := repo.Questions(pa.Origin)
		if err != nil {
			return err
		}
		for _, question := range questions {
			if question.Asked.ID == pa.QuestionID {
				req.Unit, req.Turn = question.Asked.Unit, question.Asked.Turn
			}
		}
	}
	if req.Unit != "" {
		state, err := repo.Workflow(pa.Origin, trace.UnitSubject(req.Unit))
		if err != nil || state.Value != UnitWaiting {
			return err
		}
	}
	transitions, err := trace.Read[trace.Transition](repo, pa.Origin)
	if err != nil {
		return err
	}
	// Reuse the local amendment continuation machinery on the equivalent
	// recorded project-amendment wait; the durable records remain unchanged.
	for n := range transitions {
		if transitions[n].ID == trace.UnitSubject(req.Unit)+"_waiting_project_amendment_"+string(pa.Workgroup)+"_"+pa.ID {
			transitions[n].ID = trace.UnitSubject(req.Unit) + "_waiting_amendment_" + req.ID
		}
	}
	a := amendmentDebate{Debate: &debate{S: s.Intake().DebateDependencies(), Repository: repo}}
	cause := WorkgroupChangeDocument(c.ID)
	verb := map[string]string{"approve": "approved", "reject": "rejected"}[c.Decision.Decision]
	decision := AmendmentDecision{Decision: c.Decision.Decision, Actor: "owner", Note: "The owner " + verb + " the workgroup impact summary. " + c.Reply}
	if err := a.DeliverRuling(ctx, pa.Origin, req, decision, cause, "The impact summary decision preserves the recorded project spec until an approved delta is applied."); err != nil {
		return err
	}
	return a.ResumeUnit(ctx, pa.Origin, req, cause, verb, "owner", transitions)
}
func (s *Controller) ReworkChangeMember(ctx context.Context, repo *trace.Repository, row amendment.StreamImpact, c *WorkgroupChange) error {
	stream := row.Workstream
	cause := WorkgroupChangeDocument(c.ID)
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return err
	}
	for _, d := range docs {
		if d.ID == "change-applied-"+c.ID {
			var sl trace.Slice
			if err := json.Unmarshal([]byte(d.Content), &sl); err != nil {
				return err
			}
			for n := range c.Result.Slices {
				if c.Result.Slices[n].Workstream == stream {
					c.Result.Slices[n] = sl
				}
			}
			return s.ResumeChangeUnits(ctx, repo, stream, c.ID)
		}
	}
	current, sealDoc, found, err := seal.Latest(repo, stream)
	if err != nil {
		return err
	}
	proposed, _ := trace.SliceFor(c.After, stream)
	if !found {
		return s.PlanChangeMember(ctx, repo, stream, proposed, c.ID)
	}
	changed := []string{}
	for _, u := range row.Units {
		if u.Action != amendment.Leave {
			changed = append(changed, u.Unit)
		}
	}
	if _, err := repo.CancelUnitTurns(ctx, stream, changed, s.Now(), amendmentActor, "superseded by the owner-approved delta"); err != nil {
		return err
	}
	// A whole unit that landed while the impact waited retains that commit and
	// receives a revision. An approved but unlanded candidate stays on its unit
	// branch, and the mason resumes there under the new intent.
	states, err := repo.WorkflowStates(stream)
	if err != nil {
		return err
	}
	row.Units = slices.Clone(row.Units)
	for n := range row.Units {
		u := &row.Units[n]
		if u.Action == amendment.Rework && states[trace.UnitSubject(u.Unit)].Value == UnitMerged {
			u.Action = amendment.Revise
			u.Revision = amendment.RevisionID(c.ID, u.Unit)
		}
	}
	sl, err := RevisionSlice(*c, row)
	if err != nil {
		return err
	}
	p, err := plan.Parse([]byte(sl.Plan))
	if err != nil {
		return err
	}
	entities, err := kb.Load(repo)
	if err != nil {
		return err
	}
	footprints, unresolved := seal.Take(p, entities)
	if len(unresolved) > 0 {
		return fmt.Errorf("amended footprints do not resolve: %v", unresolved)
	}
	specRev, err := nextRevision(repo, stream, plan.SpecDocument)
	if err != nil {
		return err
	}
	planRev, err := nextRevision(repo, stream, plan.PlanDocument)
	if err != nil {
		return err
	}
	next := current
	next.Seal++
	next.Revision = shed.Pin{Spec: specRev, Plan: planRev}
	next.SpecHash = seal.SpecHash(sl.Spec)
	next.Footprints = footprints
	encoded, err := seal.Encode(next)
	if err != nil {
		return err
	}
	doc := func(id, path, content string, revision int) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: revision, Project: repo.Project(), Workstream: stream, At: s.Now(), Actor: amendmentActor, Cause: cause}, Path: path, Content: content}
	}
	app := amendment.Application{Amendment: "group-" + c.ID, Actor: "owner", From: amendment.Pin{Seal: current.Seal, SealRevision: sealDoc.Revision, Spec: current.Revision.Spec, Plan: current.Revision.Plan}, To: amendment.Pin{Seal: next.Seal, SealRevision: sealDoc.Revision + 1, Spec: specRev, Plan: planRev}, Change: c.Request.Change, Criteria: amendment.ChangedCriteria(TraceSpecAt(docs, current.Revision.Spec), sl.Spec), Rework: []string{}, Notify: []string{}, Added: []string{}, Removed: []string{}, Applied: &amendment.Applied{}}
	txs := []trace.Transaction{}
	h := func(id, unit string) trace.Header {
		return trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: ChangeIdentity(id), Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: s.Now(), Actor: amendmentActor, Cause: cause}
	}
	for _, u := range row.Units {
		if states[trace.FeatureSubject].Value == RatifiedState || states[trace.FeatureSubject].Value == trace.SealPreparedState {
			// The builder initializes the entire replacement plan after release.
			// Pre-creating changed units here would race that initialization.
			break
		}
		switch u.Action {
		case amendment.Leave:
			continue
		case amendment.Rework:
			app.Rework = append(app.Rework, u.Unit)
			st := states[trace.UnitSubject(u.Unit)]
			to := UnitImplementing
			if st.Value == UnitReady || st.Value == UnitPlanned || st.Value == "" {
				to = UnitPlanned
			}
			txs = append(txs, trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{Header: h("change-"+c.ID+"-rework-"+u.Unit, u.Unit), Subject: trace.UnitSubject(u.Unit), From: st.Value, To: to, Reason: "owner-approved delta reworks only this unit's changed sections"}})
		case amendment.Remove:
			app.Removed = append(app.Removed, u.Unit)
			st := states[trace.UnitSubject(u.Unit)]
			txs = append(txs, trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{Header: h("change-"+c.ID+"-obsolete-"+u.Unit, u.Unit), Subject: trace.UnitSubject(u.Unit), From: st.Value, To: "obsolete", Reason: "the approved delta removed this unit's section; its removal revision keeps landed work traceable"}})
			fallthrough
		case amendment.Revise, amendment.Add:
			id := u.Unit
			if u.Revision != "" {
				id = u.Revision
			}
			app.Added = append(app.Added, id)
			txs = append(txs, trace.Transaction{Transition: trace.Transition{Header: h("change-"+c.ID+"-planned-"+id, id), Subject: trace.UnitSubject(id), To: UnitPlanned, Reason: "owner-approved delta adds this unit behind its dependencies"}})
		}
	}
	// Ready units are computed against the amended dependency graph. In
	// particular a reopened dependent waits for the upstream revision.
	plannedStates := map[string]string{}
	for id, st := range states {
		plannedStates[id] = st.Value
	}
	for _, tx := range txs {
		plannedStates[tx.Transition.Subject] = tx.Transition.To
	}
	for _, u := range p.Units {
		subject := trace.UnitSubject(u.ID)
		if plannedStates[subject] != UnitPlanned {
			continue
		}
		if slices.ContainsFunc(u.DependsOn, func(d string) bool {
			return plannedStates[trace.UnitSubject(d)] != UnitMerged
		}) {
			continue
		}
		version := states[subject].Version
		for _, tx := range txs {
			if tx.Transition.Subject == subject {
				version++
			}
		}
		txs = append(txs, trace.Transaction{ExpectedVersion: version, Transition: trace.Transition{Header: h("change-"+c.ID+"-ready-"+u.ID, u.ID), Subject: subject, From: UnitPlanned, To: UnitReady, Reason: "every amended dependency has landed"}})
	}
	data, err := json.Marshal(app)
	if err != nil {
		return err
	}
	sliceData, _ := json.Marshal(sl)
	records := []trace.Document{doc(plan.SpecDocument, plan.SpecPath, sl.Spec, specRev), doc(plan.PlanDocument, plan.PlanPath, sl.Plan, planRev), doc(seal.DocumentID, seal.Path, string(encoded), sealDoc.Revision+1), doc(amendment.DocumentID(app.Amendment), amendment.Path(app.Amendment), string(data), 1), doc("change-applied-"+c.ID, "tools/change-applied-"+c.ID+".json", string(sliceData), 1)}
	// PR-open work keeps its terminal publication record. Its repair units run
	// through the same build loop, then ordinary review and delivery approval.
	feature := states[trace.FeatureSubject]
	if feature.Value == DeliveredState || feature.Value == AssembledState {
		txs = append(txs, trace.Transaction{ExpectedVersion: feature.Version, Transition: trace.Transition{Header: h("change-"+c.ID+"-building", ""), Subject: trace.FeatureSubject, From: feature.Value, To: BuildingState, Reason: "rework the same feature branch; the existing pull request requires fresh delivery approval"}})
	}
	if _, err := repo.RecordDocumentsWith(ctx, records, txs...); err != nil {
		return err
	}
	for n := range c.Result.Slices {
		if c.Result.Slices[n].Workstream == stream {
			c.Result.Slices[n] = sl
		}
	}
	return s.ResumeChangeUnits(ctx, repo, stream, c.ID)
}
func (s *Controller) ResumeChangeUnits(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, id string) error {
	apps, err := amendment.Read(repo, stream)
	if err != nil {
		return err
	}
	for _, app := range apps {
		if app.Amendment != "group-"+id {
			continue
		}
		a := amendmentDebate{Debate: &debate{S: s.Intake().DebateDependencies(), Repository: repo}}
		for _, unit := range app.Rework {
			st, err := repo.Workflow(stream, trace.UnitSubject(unit))
			if err != nil {
				return err
			}
			if st.Value == UnitImplementing {
				moved := trace.Transition{Header: trace.Header{ID: ChangeIdentity("change-" + id + "-rework-" + unit), At: s.Now()}, Subject: trace.UnitSubject(unit), To: UnitImplementing}
				if err := a.EnqueueAmended(ctx, stream, app, unit, moved); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func TraceSpecAt(docs []trace.Document, revision int) string {
	for _, d := range docs {
		if d.ID == plan.SpecDocument && d.Revision == revision {
			return d.Content
		}
	}
	return ""
}

// PlanChangeMember records the already owner-approved slice and asks the
// existing sealer to prepare its branch. It adds no ratification prompt.
func (s *Controller) PlanChangeMember(ctx context.Context, repo *trace.Repository, stream config.WorkstreamID, sl trace.Slice, id string) error {
	if _, _, found, err := seal.Latest(repo, stream); err != nil || found {
		return err
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return err
	}
	for _, d := range docs {
		if d.ID == "change-approved-"+id {
			return nil
		}
	}
	specRev, err := nextRevision(repo, stream, plan.SpecDocument)
	if err != nil {
		return err
	}
	planRev, err := nextRevision(repo, stream, plan.PlanDocument)
	if err != nil {
		return err
	}
	rat := shed.Ratify(1, shed.Pin{Spec: specRev, Plan: planRev}, nil)
	data, err := shed.EncodeRatification(rat)
	if err != nil {
		return err
	}
	cause := WorkgroupChangeDocument(id)
	doc := func(name, path, content string, rev int) trace.Document {
		return trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: name, Revision: rev, Project: repo.Project(), Workstream: stream, At: s.Now(), Actor: ownerActor, Cause: cause}, Path: path, Content: content}
	}
	rev, err := nextRevision(repo, stream, shed.RatificationDocumentID(1))
	if err != nil {
		return err
	}
	records := []trace.Document{doc(plan.SpecDocument, plan.SpecPath, sl.Spec, specRev), doc(plan.PlanDocument, plan.PlanPath, sl.Plan, planRev), doc(shed.RatificationDocumentID(1), shed.RatificationPath(1), string(data), rev), doc("change-approved-"+id, "tools/change-approved-"+id+".json", `{"approved":true}`, 1)}
	st, err := repo.Workflow(stream, trace.FeatureSubject)
	if err != nil {
		return err
	}
	tx := trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "change-" + id + "-sketched", Revision: 1, Project: repo.Project(), Workstream: stream, At: s.Now(), Actor: amendmentActor, Cause: cause}, Subject: trace.FeatureSubject, From: st.Value, To: SketchedState, Reason: "the owner's impact approval covers this new slice and replan"}}
	_, err = repo.RecordDocumentsWith(ctx, records, tx)
	return err
}
func (s *Controller) CreateChangeFollowup(ctx context.Context, wt *trace.Repository, g *trace.Workgroup, repo *trace.Repository, row amendment.StreamImpact, c *WorkgroupChange) (bool, error) {
	key := "followup-" + c.ID + "-" + string(row.Workstream)
	id := HandInWorkstream(repo.Project(), key)
	if c.Followups[row.Workstream] == "" {
		if c.Followups == nil {
			c.Followups = map[config.WorkstreamID]config.WorkstreamID{}
		}
		c.Followups[row.Workstream] = id
		if err := s.SaveChange(ctx, wt, c); err != nil {
			return false, err
		}
	}
	sl, _ := trace.SliceFor(c.After, row.Workstream)
	brief := "Owner-approved follow-up after merged work: " + c.Request.Change + "\n\n" + sl.Spec
	streams, err := repo.Workstreams()
	if err != nil {
		return false, err
	}
	if !slices.Contains(streams, id) {
		backend, err := s.NewWorkspaces(ctx, s.About(repo))
		if err != nil {
			return false, err
		}
		if err := repo.CreateWorkstreamOn(ctx, id, backend.Backend, s.Now(), ownerActor); err != nil {
			return false, err
		}
	}
	docs, err := trace.Read[trace.Document](repo, id)
	if err != nil {
		return false, err
	}
	copied := false
	for _, d := range docs {
		if d.ID == handedDocument {
			if d.Content != brief {
				return false, trace.ErrConflict
			}
			copied = true
		}
	}
	if !copied {
		if err := repo.Append(ctx, trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: handedDocument, Revision: 1, Project: repo.Project(), Workstream: id, At: s.Now(), Actor: ownerActor, Cause: WorkgroupChangeDocument(c.ID)}, Path: "handed/stdin", Source: "stdin", Content: brief}); err != nil {
			return false, err
		}
	}
	if err := s.RefreshRuntime(); err != nil {
		return false, err
	}
	if err := s.ChangeBoundary(ctx, repo, id, *c, false); err != nil {
		return false, err
	}
	if err := s.Step("change-followup-allocated"); err != nil {
		return false, err
	}
	if ChangeSharedRepository(*g, wt) {
		if err := repo.RetractChiefOfStaff(ctx, id); err != nil {
			return false, err
		}
		if err := repo.RetractWorkgroup(ctx, id, s.Now(), amendmentActor, WorkgroupChangeDocument(c.ID)); err != nil {
			return false, err
		}
		if err := repo.RecordCrossRepoWorkgroup(ctx, id, g.ID, s.Now(), amendmentActor, WorkgroupChangeDocument(c.ID)); err != nil {
			return false, err
		}
	}
	if err := trace.AddWorkgroupMember(ctx, wt, g.ID, id, s.Now(), amendmentActor, WorkgroupChangeDocument(c.ID)); err != nil {
		return false, err
	}
	if !slices.Contains(g.Members, id) {
		g.Members = append(g.Members, id)
	}
	if err := s.Step("change-followup-joined"); err != nil {
		return false, err
	}
	sl.Workstream = id
	if err := s.PlanChangeMember(ctx, repo, id, sl, c.ID); err != nil {
		return false, err
	}
	// Sealing is an existing reconciled repository operation. Group completion
	// prepares ordinary members; a delta approval prepares its new follow-up.
	st, err := repo.Workflow(id, trace.FeatureSubject)
	if err != nil {
		return false, err
	}
	if st.Value == trace.SealPreparedState {
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "change-" + c.ID + "-ratified", Revision: 1, Project: repo.Project(), Workstream: id, At: s.Now(), Actor: amendmentActor, Cause: WorkgroupChangeDocument(c.ID)}
		_, err = repo.MoveFeatureState(ctx, h, trace.SealPreparedState, RatifiedState, "the owner-approved follow-up seal is complete")
		if err != nil {
			return false, err
		}
	}
	_, _, found, err := seal.Latest(repo, id)
	if err != nil || !found {
		return false, err
	}
	if !slices.ContainsFunc(c.Result.Slices, func(s trace.Slice) bool {
		return s.Workstream == id
	}) {
		c.Result.Slices = append(c.Result.Slices, sl)
		old, _ := trace.SliceFor(c.Before, row.Workstream)
		for n := range c.Result.Slices {
			if c.Result.Slices[n].Workstream == row.Workstream {
				c.Result.Slices[n] = old
			}
		}
	}
	// The merged member's own slice stays historical; outgoing dependencies
	// now wait on its new follow-up's concrete merge when their edge needs it.
	for n := range c.Result.Graph {
		if c.Result.Graph[n].From == row.Workstream {
			c.Result.Graph[n].From = id
		}
		if c.Result.Graph[n].To == row.Workstream {
			c.Result.Graph[n].To = id
		}
	}
	return true, nil
}
