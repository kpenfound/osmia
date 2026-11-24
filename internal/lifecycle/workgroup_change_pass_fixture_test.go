package lifecycle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	changeKey           = "retired-driver-change"
	changeBeingApplied  = "an approved change is being applied to this workgroup; retry"
	retirementSnapshot  = "member-retirement-snapshot"
	changeBeforeMember  = "change-before-member-effect"
	changeFollowupAlloc = "change-followup-allocated"
	changeFollowupJoin  = "change-followup-joined"
)

type changePass struct {
	f               *journey.ProductionDesignFixture
	cleanup         *journey.LifecycleCleanupGate
	race            *intakefixture.ChangeRaceGate
	kind            string
	affectsRetired  bool
	retiresAffected bool
	retired         config.WorkstreamID
	survivor        config.WorkstreamID
	retiredProject  config.ProjectID
	survivorProject config.ProjectID
	host            config.WorkstreamID
	group           config.WorkgroupID
	approval        trace.GroupApproval
	approvalDoc     trace.Document
	once            sync.Once
	release         chan struct{}
	approvals       atomic.Int32
	revision        atomic.Int64
	presented       lifecycle.WorkgroupChange
	pinned          lifecycle.WorkgroupChange
}

func newChangePass(t *testing.T, kind string, affectsRetired bool, raceStep string) *changePass {
	t.Helper()
	ctx := context.Background()
	c := &changePass{
		kind:            kind,
		affectsRetired:  affectsRetired,
		retiresAffected: affectsRetired && raceStep != changeBeforeMember,
		cleanup:         &journey.LifecycleCleanupGate{Reached: make(chan struct{})},
		race:            &intakefixture.ChangeRaceGate{Step: raceStep, Reached: make(chan struct{})},
		release:         make(chan struct{}),
	}
	t.Cleanup(c.cleanup.Release)
	t.Cleanup(c.race.Release)
	t.Cleanup(c.unblock)
	c.f = journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait, Checkpoint: func(ctx context.Context, step string) error {
		if err := c.race.Checkpoint(ctx, step); err != nil {
			return err
		}
		return c.cleanup.Checkpoint(ctx, step)
	}})
	f := c.f
	t.Cleanup(func() {
		c.race.Release()
		c.cleanup.Release()
		f.Stop(t)
	})
	c.retired, c.survivor = f.Stream1, f.Stream2
	c.retiredProject, c.survivorProject = f.Project, f.Project2
	if c.retired > c.survivor {
		c.retired, c.survivor = c.survivor, c.retired
		c.retiredProject, c.survivorProject = c.survivorProject, c.retiredProject
	}
	c.scriptProjectSpecDraft()
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	repo := c.survivorRepo(t)
	wt, host, _, closeAuthority, err := f.S.Lifecycle().ChiefRepository(ctx, repo, c.survivor)
	fixturekit.Must(t, err)
	approval, approvalDoc, found, err := trace.ReadGroupApproval(wt, host)
	fixturekit.Must(t, err)
	if !found || !approval.Complete {
		t.Fatal("production effective seals are incomplete")
	}
	c.host, c.group, c.approval, c.approvalDoc = host, approval.Workgroup, approval, approvalDoc
	closeAuthority()

	c.scriptChangeTurns()
	f.CheckContinuation(t)
	lifecycleWorker(t, f, c.retiredProject, c.retired, "worker_first", "change-unit-boundary", "Finish this whole unit before changing intent.")
	lifecycleWorker(t, f, c.survivorProject, c.survivor, "worker_second", "change-unit-boundary", "Finish this whole unit before changing intent.")
	journey.LifecycleAwait(t, f, "both member turns running", func() bool {
		for _, m := range []struct {
			project config.ProjectID
			stream  config.WorkstreamID
			worker  string
		}{{c.retiredProject, c.retired, "worker_first"}, {c.survivorProject, c.survivor, "worker_second"}} {
			r, err := f.S.Repository(m.project)
			fixturekit.Must(t, err)
			th, err := r.Thread(m.stream, m.worker)
			fixturekit.Must(t, err)
			if th.Active == "" {
				return false
			}
		}
		return true
	})
	return c
}

func (c *changePass) unblock() { c.once.Do(func() { close(c.release) }) }

func (c *changePass) survivorRepo(t *testing.T) *trace.Repository {
	t.Helper()
	repo, err := c.f.S.Repository(c.survivorProject)
	fixturekit.Must(t, err)
	return repo
}

func (c *changePass) scriptProjectSpecDraft() {
	f := c.f
	f.Engine.Mu.Lock()
	defer f.Engine.Mu.Unlock()
	f.Engine.Turns["project-spec-draft-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		_, err := fixturekit.CallTool(ctx, tools, lifecycle.ProjectSpecDraftTool, map[string]any{
			"behavior":  "Chunks the server accepts are acknowledged with their offset; the client resumes uploads from the last acknowledged offset.",
			"contracts": []map[string]any{{"name": "acknowledgment", "text": "The server's acknowledgment of a stored chunk names its offset; the client reads that offset to resume."}},
			"graph":     []map[string]any{{"from": string(c.retired), "to": string(c.survivor), "type": "contract"}},
			"slices":    []map[string]any{{"workstream": string(f.Stream1), "contract_criteria": []int{2}}, {"workstream": string(f.Stream2), "contract_criteria": []int{1}}},
		})
		return intakefixture.QuestionResult(req, "session-project-spec", "The least member supplies the shared contract."), err
	}
}

func (c *changePass) scriptChangeTurns() {
	f := c.f
	f.Engine.Mu.Lock()
	defer f.Engine.Mu.Unlock()
	f.Engine.Turns["change-unit-boundary"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
		select {
		case <-c.release:
			return intakefixture.QuestionResult(req, "session-boundary", "The whole unit boundary is safe."), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		names, err := intakefixture.ToolNames(ctx, tools)
		if err != nil {
			return nil, err
		}
		if slices.Contains(names, lifecycle.DeltaDraftTool) {
			text, err := intakefixture.ReadTool(ctx, tools, "before.json")
			if err != nil {
				return nil, err
			}
			var proposed trace.ProjectSpec
			if err := json.Unmarshal([]byte(text), &proposed); err != nil {
				return nil, err
			}
			for i := range proposed.Slices {
				sl := &proposed.Slices[i]
				if sl.Workstream == c.retired && !c.affectsRetired {
					continue
				}
				p, err := plan.Parse([]byte(sl.Plan))
				if err != nil {
					return nil, err
				}
				number := 2
				if sl.Workstream == f.Stream1 {
					number = 3
				}
				sl.Spec += fmt.Sprintf("%d. Invalid diagnostic requests are refused.\n", number)
				p.Units = append(p.Units, plan.Unit{ID: "diagnostics", Task: "Refuse invalid diagnostic requests", Acceptance: []string{"Invalid diagnostic requests are refused"}, Criteria: []string{plan.Cite(number)}, DependsOn: []string{}, Footprint: []string{"internal.trace"}})
				data, err := plan.Encode(p)
				if err != nil {
					return nil, err
				}
				sl.Plan = string(data)
			}
			_, err = fixturekit.CallTool(ctx, tools, lifecycle.DeltaDraftTool, toolMap(proposed))
			return intakefixture.QuestionResult(req, "session-delta", "Added only the requested diagnostics."), err
		}
		if slices.Contains(names, lifecycle.DeltaReviewTool) {
			_, err = fixturekit.CallTool(ctx, tools, lifecycle.DeltaReviewTool, map[string]any{"objections": []string{}, "note": "Only requested diagnostics change."})
			return intakefixture.QuestionResult(req, "session-review", "Reviewed the delta."), err
		}
		if slices.Contains(names, lifecycle.DeltaReplyTool) {
			_, err = fixturekit.CallTool(ctx, tools, lifecycle.DeltaReplyTool, map[string]any{"reply": "Existing contract and unit assignments remain fixed."})
			return intakefixture.QuestionResult(req, "session-answer", "Answered the committee."), err
		}
		if strings.Contains(req.Prompt, "Confirm survivor journal status") {
			return intakefixture.QuestionResult(req, "session-owner-status", "The original journal and its prerequisites are preserved."), nil
		}
		if strings.Contains(req.Prompt, "Approve the driver impact") {
			c.approvals.Add(1)
			_, err = fixturekit.CallTool(ctx, tools, lifecycle.DecideChangeTool, map[string]any{"id": changeKey, "revision": int(c.revision.Load()), "decision": "approve", "pull_requests": "draft"})
			return intakefixture.QuestionResult(req, "session-approval", "Approved this impact and draft disposition."), err
		}
		if strings.Contains(req.Prompt, "Request the driver change") {
			scope := "workstream"
			if c.affectsRetired {
				scope = "workgroup"
			}
			_, err = fixturekit.CallTool(ctx, tools, lifecycle.RequestChangeTool, toolMap(lifecycle.ChangeRequest{Key: changeKey, Kind: c.kind, Scope: scope, Workstream: c.survivor, Change: "Cancel the selected work or add diagnostic refusal checks to its selected slices."}))
			return intakefixture.QuestionResult(req, "session-request", "The impact will be presented here."), err
		}
		return intakefixture.QuestionResult(req, "session-chief", "Following the original shared conversation."), nil
	}
}

func (c *changePass) recordSurvivorPullRequestMerged(t *testing.T) {
	t.Helper()
	fixturekit.Must(t, delivery.RecordOwnPullRequestOutcome(context.Background(), c.survivorRepo(t), c.f.S.Now(), c.survivor, "owner", 7, delivery.OwnPullRequestOutcomeMerged))
}

func (c *changePass) requestAndPresent(t *testing.T) {
	t.Helper()
	_, err := c.f.C.Send(context.Background(), c.f.Origin, "Request the driver change.")
	fixturekit.Must(t, err)
	journey.LifecycleAwait(t, c.f, "single impact presented", func() bool {
		current := journey.CurrentChange(t, c.f.S, c.f.Origin, changeKey)
		if current.State == "failed" {
			t.Fatalf("delta failed before presentation: %s", current.Failure)
		}
		return current.State == "presented"
	})
	c.presented = journey.CurrentChange(t, c.f.S, c.f.Origin, changeKey)
	c.revision.Store(int64(c.presented.Revision))
}

func (c *changePass) approve(t *testing.T) lifecycle.WorkgroupChange {
	t.Helper()
	_, err := c.f.C.Send(context.Background(), c.f.Origin, "Approve the driver impact; leave open PRs as drafts.")
	fixturekit.Must(t, err)
	lastRevision := -1
	journey.LifecycleAwait(t, c.f, "approved boundary stop", func() bool {
		current := journey.CurrentChange(t, c.f.S, c.f.Origin, changeKey)
		if current.Revision != lastRevision {
			t.Logf("approval boundary: state=%s revision=%d decision=%+v attempts=%d failure=%s", current.State, current.Revision, current.Decision, c.approvals.Load(), current.Failure)
			lastRevision = current.Revision
		}
		return current.Decision != nil && current.State == "applying"
	})
	pinned := journey.CurrentChange(t, c.f.S, c.f.Origin, changeKey)
	if len(pinned.Applied) != 0 {
		t.Fatalf("change crossed a running unit: %+v", pinned)
	}
	if !intakefixture.EqualJSONTest(pinned.Before, c.presented.Before) || !intakefixture.EqualJSONTest(pinned.After, c.presented.After) || !intakefixture.EqualJSONTest(pinned.Members, c.presented.Members) || !intakefixture.EqualJSONTest(pinned.Impact, c.presented.Impact) || pinned.Decision.Revision != c.presented.Revision {
		t.Fatal("approval replaced the presented pins")
	}
	c.pinned = pinned
	return pinned
}

func (c *changePass) abandonLeader(t *testing.T) {
	t.Helper()
	_, err := c.f.C.Abandon(context.Background(), c.retired, "The owner cancelled the driver.")
	fixturekit.Must(t, err)
}

func (c *changePass) archiveLeader() error {
	ctx, cancel := context.WithTimeout(context.Background(), fixturekit.DemoTimeout)
	defer cancel()
	retry := time.NewTicker(10 * time.Millisecond)
	defer retry.Stop()
	for {
		_, err := c.f.C.Archive(ctx, c.retired)
		if err == nil || !strings.Contains(err.Error(), changeBeingApplied) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-retry.C:
		}
	}
}

func (c *changePass) awaitLeaderArchived(t *testing.T) {
	t.Helper()
	journey.LifecycleAwait(t, c.f, "owner archive admitted", func() bool {
		err := c.archiveLeader()
		if err != nil && strings.Contains(err.Error(), changeBeingApplied) {
			return false
		}
		fixturekit.Must(t, err)
		return true
	})
}

func (c *changePass) awaitCleanupEligible(t *testing.T) {
	t.Helper()
	journey.LifecycleAwait(t, c.f, "eligible production cleanup", func() bool {
		select {
		case <-c.cleanup.Reached:
			return true
		default:
			return false
		}
	})
}

func (c *changePass) readChanges(t *testing.T) []lifecycle.WorkgroupChange {
	t.Helper()
	authority, err := trace.OpenWorkgroupAuthority(c.f.S.Current().Root, c.group)
	fixturekit.Must(t, err)
	changes, err := lifecycle.ReadChanges(authority, c.host)
	fixturekit.Must(t, err)
	fixturekit.Must(t, authority.Close())
	return changes
}

func (c *changePass) check(t *testing.T, boundary string) lifecycle.WorkgroupChange {
	t.Helper()
	authority, err := trace.OpenWorkgroupAuthority(c.f.S.Current().Root, c.group)
	fixturekit.Must(t, err)
	defer authority.Close()
	gotApproval, gotDoc, found, err := trace.ReadGroupApproval(authority, c.host)
	fixturekit.Must(t, err)
	if !found || !intakefixture.EqualJSONTest(gotApproval, c.approval) || !intakefixture.EqualJSONTest(gotDoc, c.approvalDoc) {
		t.Fatalf("%s changed original effective approval pins", boundary)
	}
	changes, err := lifecycle.ReadChanges(authority, c.host)
	fixturekit.Must(t, err)
	if len(changes) != 1 {
		t.Fatalf("%s lost or duplicated approved journal: %+v", boundary, changes)
	}
	got := changes[0]
	if !intakefixture.EqualJSONTest(got.Decision, c.pinned.Decision) || !intakefixture.EqualJSONTest(got.Before, c.pinned.Before) || !intakefixture.EqualJSONTest(got.After, c.pinned.After) || !intakefixture.EqualJSONTest(got.Members, c.pinned.Members) || !intakefixture.EqualJSONTest(got.Impact, c.pinned.Impact) {
		t.Fatalf("%s reselected approved intent or member pins", boundary)
	}
	if c.retiresAffected && slices.Contains(got.Applied, c.retired) {
		t.Fatalf("%s falsely applied retired effect", boundary)
	}
	return got
}

func (c *changePass) consume(t *testing.T) {
	t.Helper()
	c.unblock()
	journey.LifecycleAwait(t, c.f, "survivor consumes original approval", func() bool {
		got := c.check(t, "reconciling")
		return slices.Contains(got.Applied, c.survivor) && got.State == "applied" && slices.Contains(got.Retired, c.retired) == c.retiresAffected && got.Failure == ""
	})
}

func (c *changePass) completeTheChange(t *testing.T, resumeRetained bool, expectedApplied int) lifecycle.WorkgroupChange {
	t.Helper()
	f := c.f
	c.check(t, "retained")
	if resumeRetained {
		retiredRepo, err := f.S.Repository(c.retiredProject)
		fixturekit.Must(t, err)
		history, err := trace.Read[trace.Transition](retiredRepo, c.retired)
		fixturekit.Must(t, err)
		c.consume(t)
		after, err := trace.Read[trace.Transition](retiredRepo, c.retired)
		fixturekit.Must(t, err)
		if !intakefixture.EqualJSONTest(history, after) {
			t.Fatal("survivor effect mutated retained retired history")
		}
	}
	c.cleanup.Release()
	journey.LifecycleAwait(t, f, "legitimate cleanup", func() bool {
		a, ok := intakefixture.ArchiveRecord(t, f.S.Store, c.retired)
		return ok && !a.CleanedAt.IsZero()
	})
	c.check(t, "deleted")
	f.Stop(t)
	f.Start(t)
	c.check(t, "restarted pending")
	if !resumeRetained {
		c.consume(t)
	}
	finished := c.check(t, "survivor effect")
	if len(finished.Applied) != expectedApplied {
		t.Fatalf("duplicate effect or owner decision: %+v approvals=%d", finished, c.approvals.Load())
	}
	c.requireEffectOnce(t)
	return finished
}

func (c *changePass) confirmCompletedAcrossRestart(t *testing.T, finished lifecycle.WorkgroupChange) {
	t.Helper()
	f := c.f
	f.Stop(t)
	f.Start(t)
	sent, err := f.C.Send(context.Background(), c.survivor, "Confirm survivor journal status after restart.")
	fixturekit.Must(t, err)
	journey.LifecycleAwait(t, f, "survivor owner turn after restart", func() bool {
		conversation, err := f.C.Conversation(context.Background(), c.survivor)
		fixturekit.Must(t, err)
		return slices.ContainsFunc(conversation.Entries, func(e api.ConversationEntry) bool {
			return e.Turn == sent.Turn && e.Kind == "response" && e.State == api.TurnDone
		})
	})
	if got := c.check(t, "second real restart"); got.Revision != finished.Revision {
		t.Fatalf("restart rewrote completed effects or hold: before=%+v after=%+v", finished, got)
	}
	c.requireEffectOnce(t)
	gone, err := trace.WorkstreamTraceGone(f.S.Current().Root, c.retiredProject, c.retired)
	fixturekit.Must(t, err)
	if !gone {
		t.Fatal("change reconciliation recreated retired history")
	}
}

func (c *changePass) requireEffectOnce(t *testing.T) {
	t.Helper()
	repo := c.survivorRepo(t)
	if c.kind == "cancel" {
		transitions, err := trace.Read[trace.Transition](repo, c.survivor)
		fixturekit.Must(t, err)
		count := 0
		for _, tr := range transitions {
			if tr.ID == "cancel-"+changeKey {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("survivor cancellation reconciled %d times", count)
		}
		return
	}
	docs, err := trace.Read[trace.Document](repo, c.survivor)
	fixturekit.Must(t, err)
	count := 0
	for _, doc := range docs {
		if doc.ID != amendment.DocumentID("group-"+changeKey) {
			continue
		}
		count++
		var app amendment.Application
		fixturekit.Must(t, json.Unmarshal([]byte(doc.Content), &app))
		if doc.Revision != 1 || app.From.Seal != 1 || app.To.Seal != 2 || app.To.SealRevision != app.From.SealRevision+1 || app.To.Spec != app.From.Spec+1 || app.To.Plan != app.From.Plan+1 || !slices.Equal(app.Added, []string{"diagnostics"}) || len(app.Rework) != 0 || len(app.Removed) != 0 {
			t.Fatalf("survivor amendment changed unapproved sections or seals: %+v", app)
		}
	}
	if count != 1 {
		t.Fatalf("survivor amendment reconciled %d times", count)
	}
}

func (c *changePass) runAfterTheLeaderIsArchived(t *testing.T, resumeRetained bool) lifecycle.WorkgroupChange {
	t.Helper()
	c.requestAndPresent(t)
	approveAfterArchive := c.affectsRetired && resumeRetained
	if !approveAfterArchive {
		c.approve(t)
	}
	c.abandonLeader(t)
	c.awaitLeaderArchived(t)
	c.awaitCleanupEligible(t)
	if approveAfterArchive {
		c.approve(t)
	}
	finished := c.completeTheChange(t, resumeRetained, 1)
	c.confirmCompletedAcrossRestart(t, finished)
	return finished
}

func (c *changePass) requireOneImpactPromptAndOwnerDecision(t *testing.T) {
	t.Helper()
	conversation, err := c.f.C.Conversation(context.Background(), c.survivor)
	fixturekit.Must(t, err)
	prompts, ownerDecisions := 0, 0
	for _, entry := range conversation.Entries {
		if entry.Kind == "message" && strings.Contains(entry.Text, "Approve the driver impact") {
			ownerDecisions++
		}
		if entry.Turn == "change-"+changeKey+"-impact" {
			prompts++
		}
	}
	if prompts != 1 || ownerDecisions != 1 {
		t.Fatalf("continued conversation received %d impact prompts and %d owner decisions", prompts, ownerDecisions)
	}
}

func (c *changePass) requireDesignConversationUnchanged(t *testing.T) {
	t.Helper()
	repo := c.survivorRepo(t)
	authority, _, _, closeAuthority, err := c.f.S.Lifecycle().ChiefRepository(context.Background(), repo, c.survivor)
	fixturekit.Must(t, err)
	chief, err := authority.Thread(c.host, trace.ChiefOfStaff)
	closeAuthority()
	fixturekit.Must(t, err)
	before := c.f.Before
	if !intakefixture.EqualJSONTest(before.Identity, chief.Identity) || len(chief.Turns) < len(before.Turns) || !intakefixture.EqualJSONTest(before.Turns, chief.Turns[:len(before.Turns)]) {
		t.Fatal("retirement changed original conversation or design turns")
	}
}

func (c *changePass) requireArchivedMemberNamed(t *testing.T, finished lifecycle.WorkgroupChange) {
	t.Helper()
	if !strings.Contains(finished.Summary(), "Archived before this change reached it: "+string(c.retired)) {
		t.Fatal("impact hides the archived member that received no effect")
	}
}

func (c *changePass) holdArchiveAtTheRetirementSnapshot(t *testing.T) {
	t.Helper()
	c.race.Armed.Store(true)
	archived := make(chan error, 1)
	go func() { archived <- c.archiveLeader() }()
	select {
	case <-c.race.Reached:
	case err := <-archived:
		t.Fatalf("archive returned before checkpoint: %v", err)
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("archive checkpoint not reached")
	}
	c.unblock()
	lock := c.f.S.Lifecycle().ChangeApplicationLock(c.group)
	if lock.TryLock() {
		lock.Unlock()
		t.Fatal("archive snapshot did not exclude change effects")
	}
	changes := c.readChanges(t)
	if len(changes) != 1 || !intakefixture.EqualJSONTest(changes[0], c.pinned) {
		t.Fatal("application crossed the held retirement snapshot")
	}
	c.race.Release()
	select {
	case err := <-archived:
		fixturekit.Must(t, err)
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("archive did not finish after snapshot release")
	}
}

func (c *changePass) refuseArchiveWhileTheMemberEffectIsInFlight(t *testing.T) {
	t.Helper()
	c.race.Armed.Store(true)
	c.unblock()
	c.race.Await(t)
	_, err := c.f.C.Archive(context.Background(), c.retired)
	if err == nil || !strings.Contains(err.Error(), changeBeingApplied) {
		t.Fatalf("archive must refuse an in-flight member effect promptly: %v", err)
	}
	if _, archived := intakefixture.ArchiveRecord(t, c.f.S.Store, c.retired); archived {
		t.Fatal("refused archive published retirement")
	}
	c.race.Release()
	journey.LifecycleAwait(t, c.f, "application wins retirement race", func() bool {
		current := journey.CurrentChange(t, c.f.S, c.f.Origin, changeKey)
		return current.State == "applied" && len(current.Applied) == 2
	})
}

func (c *changePass) applySharedFollowup(t *testing.T, interrupted bool) {
	t.Helper()
	f := c.f
	if interrupted {
		c.race.Armed.Store(true)
	}
	c.unblock()
	if interrupted {
		c.race.Await(t)
		changes := c.readChanges(t)
		if len(changes) != 1 || len(changes[0].Followups) != 1 || len(changes[0].Applied) != 0 || !intakefixture.EqualJSONTest(changes[0].Decision, c.pinned.Decision) {
			t.Fatal("interrupted follow-up lost pins or falsely completed")
		}
		f.Stop(t)
		c.race.Armed.Store(false)
		c.race.Release()
		f.Start(t)
	}
	journey.LifecycleAwait(t, f, "approved shared follow-up applies", func() bool {
		current := journey.CurrentChange(t, f.S, f.Origin, changeKey)
		return current.State == "applied" && current.Followups[c.survivor] != ""
	})
	current := journey.CurrentChange(t, f.S, f.Origin, changeKey)
	if len(current.Followups) != 1 || len(current.Applied) != 1 || !intakefixture.EqualJSONTest(current.Decision, c.pinned.Decision) {
		t.Fatalf("follow-up duplicated effects or changed approval: %+v", current)
	}
	slice, _ := trace.SliceFor(current.After, c.survivor)
	checkFollowupContinuation(t, f, c.survivorProject, current.Followups[c.survivor], "Owner-approved follow-up after merged work: "+current.Request.Change+"\n\n"+slice.Spec)
	f.Stop(t)
	f.Start(t)
	current = journey.CurrentChange(t, f.S, c.survivor, changeKey)
	if current.State != "applied" || len(current.Followups) != 1 || len(current.Applied) != 1 || !intakefixture.EqualJSONTest(current.Decision, c.pinned.Decision) {
		t.Fatal("restart changed the completed follow-up")
	}
	authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, c.group)
	fixturekit.Must(t, err)
	defer authority.Close()
	gotApproval, doc, found, err := trace.ReadGroupApproval(authority, c.host)
	fixturekit.Must(t, err)
	if !found || !intakefixture.EqualJSONTest(c.approval, gotApproval) || !intakefixture.EqualJSONTest(c.approvalDoc, doc) {
		t.Fatal("follow-up rewrote original approval pins")
	}
	groups, _, err := trace.Workgroups(authority)
	fixturekit.Must(t, err)
	actual, ok := trace.WorkgroupOf(groups, c.survivor)
	if !ok {
		t.Fatal("follow-up lost original membership")
	}
	actual.Members = append(slices.Clone(actual.Members), config.WorkstreamID("w_ffffffffffffffffffffffffffffffff"))
	if _, err := trace.ApprovedWorkgroupMembers(authority, actual, c.approval); err == nil {
		t.Fatal("unapproved extra member was admitted")
	}
}
