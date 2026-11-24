package lifecycle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/readview"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestArchivedWorkstreamKeepsLiveChangesAndStatusAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := journey.NewTwoProjectFixture(t)
	opts, cfg := f.Opts, f.Cfg
	now := f.Clock.Now()
	opts.Reconciliation.Now = func() time.Time { return now }
	journey.RunChief(&opts, cfg, &fixturekit.ChiefTurns{})
	cleanup := &journey.LifecycleCleanupGate{Reached: make(chan struct{})}
	t.Cleanup(cleanup.Release)
	previous := opts.Boundary
	opts.Boundary = func(ctx context.Context, step string) error {
		if previous != nil {
			if err := previous(ctx, step); err != nil {
				return err
			}
		}
		return cleanup.Checkpoint(ctx, step)
	}
	s, err := journey.Start(ctx, opts)
	fixturekit.Must(t, err)
	t.Cleanup(func() { fixturekit.Must(t, s.Close()) })
	c := journey.NewClient(s.Socket())
	t.Cleanup(func() { c.Close() })

	_, err = c.Abandon(ctx, fixturekit.Stream, "Superseded.")
	fixturekit.Must(t, err)
	_, err = c.Archive(ctx, fixturekit.Stream)
	fixturekit.Must(t, err)

	assertLive := func(deleted bool) {
		t.Helper()
		sent, err := c.Send(ctx, fixturekit.OtherStream, "Continue the unrelated work.")
		fixturekit.Must(t, err)
		wait, closeWait := journey.ServiceChanges(t, s)
		defer closeWait()
		deadline := time.Now().Add(fixturekit.DemoTimeout)
		for {
			conversation, err := c.Conversation(ctx, fixturekit.OtherStream)
			fixturekit.Must(t, err)
			if slices.ContainsFunc(conversation.Entries, func(e api.ConversationEntry) bool {
				return e.Turn == sent.Turn && e.Kind == "response" && e.State == api.TurnDone && e.Text == "Answer to Continue the unrelated work."
			}) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("unrelated work stopped after archive: %+v", conversation)
			}
			wait()
		}
		health, err := c.Health(ctx)
		fixturekit.Must(t, err)
		if !health.Ready || !health.API || health.Backend != api.BackendReady {
			t.Fatalf("archive stopped the live controller: %+v", health)
		}
		archived, err := c.Status(ctx, fixturekit.Stream)
		fixturekit.Must(t, err)
		if !archived.Archived || archived.TraceDeleted != deleted || archived.State == nil || *archived.State != intake.AbandonedState {
			t.Fatalf("archived status: %+v", archived)
		}
		list, err := c.Statuses(ctx)
		fixturekit.Must(t, err)
		if len(list.Diagnostics) != 0 || !slices.ContainsFunc(list.Workstreams, func(w api.WorkstreamStatus) bool { return w.Workstream == fixturekit.OtherStream && !w.Archived }) ||
			!slices.ContainsFunc(list.Workstreams, func(w api.WorkstreamStatus) bool { return w.Workstream == fixturekit.Stream && w.Archived }) {
			t.Fatalf("global status after archive: %+v", list)
		}
	}
	assertLive(false)
	c.Close()
	fixturekit.Must(t, s.Close())
	s, err = journey.Start(ctx, opts)
	fixturekit.Must(t, err)
	c = journey.NewClient(s.Socket())
	assertLive(false)

	cleanup.Await(t)
	cleanup.Release()
	wait, closeWait := journey.ServiceChanges(t, s)
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		a, ok := intakefixture.ArchiveRecord(t, s.Store, fixturekit.Stream)
		if ok && !a.CleanedAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owner-authorized archive cleanup did not finish: %+v", a)
		}
		wait()
	}
	closeWait()
	archivedRepo, err := s.Repository(fixturekit.Project)
	fixturekit.Must(t, err)
	remaining, err := archivedRepo.Workstreams()
	fixturekit.Must(t, err)
	if slices.Contains(remaining, fixturekit.Stream) {
		t.Fatalf("cleanup changed unrelated work: %v", remaining)
	}
	unrelatedRepo, err := s.Repository(fixturekit.OtherProject)
	fixturekit.Must(t, err)
	unrelated, err := unrelatedRepo.Workstreams()
	fixturekit.Must(t, err)
	if !slices.Contains(unrelated, fixturekit.OtherStream) {
		t.Fatal("cleanup removed unrelated work")
	}
	assertLive(true)
	c.Close()
	fixturekit.Must(t, s.Close())
	s, err = journey.Start(ctx, opts)
	fixturekit.Must(t, err)
	c = journey.NewClient(s.Socket())
	assertLive(true)
}
func TestChangePassReportsLiveTraceErrorsAndCancellation(t *testing.T) {
	t.Parallel()
	opts, cfg := journey.ConversationFixture(t, "change-errors-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { fixturekit.Must(t, repo.Close()) })
	s := &journey.Service{Cfg: cfg, Options: opts}
	s.SetSole(journey.RuntimeFor(repo))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Lifecycle().ChangePass(ctx, repo); err != context.Canceled {
		t.Fatalf("pass lost cancellation: %v", err)
	}
	fixturekit.Must(t, repo.Close())
	if err := s.Lifecycle().ChangePass(context.Background(), repo); err == nil {
		t.Fatal("pass suppressed a live trace error without an archive record")
	}
}

func TestChangePassAppliesPendingChangeAfterArchivedCrossRepoLeader(t *testing.T) {
	t.Parallel()
	type scenario struct {
		kind                           string
		affectsRetired, resumeRetained bool
		order                          string
	}
	var scenarios []scenario
	for _, kind := range []string{"cancel", "amend"} {
		for _, affected := range []bool{false, true} {
			for _, retained := range []bool{false, true} {
				scenarios = append(scenarios, scenario{kind, affected, retained, "ordinary"})
			}
		}
	}
	scenarios = append(scenarios, scenario{"cancel", true, true, "archive-first"}, scenario{"cancel", true, true, "change-first"}, scenario{"amend", false, true, "merged-followup"}, scenario{"amend", false, true, "merged-followup-allocation"}, scenario{"amend", false, true, "merged-followup-join"})
	for _, tc := range scenarios {
		kind, affectsRetired, resumeRetained := tc.kind, tc.affectsRetired, tc.resumeRetained
		retiresAffected := affectsRetired && tc.order != "change-first"
		t.Run(fmt.Sprintf("%s/retired-affected=%t/resume-retained=%t/%s", kind, affectsRetired, resumeRetained, tc.order), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cleanup := &journey.LifecycleCleanupGate{Reached: make(chan struct{})}
			t.Cleanup(cleanup.Release)
			race := &intakefixture.ChangeRaceGate{Step: "member-retirement-snapshot", Reached: make(chan struct{})}
			if tc.order == "change-first" {
				race.Step = "change-before-member-effect"
			}
			if tc.order == "merged-followup-allocation" {
				race.Step = "change-followup-allocated"
			}
			if tc.order == "merged-followup-join" {
				race.Step = "change-followup-joined"
			}
			t.Cleanup(race.Release)
			f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait, Checkpoint: func(ctx context.Context, step string) error {
				if err := race.Checkpoint(ctx, step); err != nil {
					return err
				}
				return cleanup.Checkpoint(ctx, step)
			}})
			defer func() {
				race.Release()
				cleanup.Release()
				f.Stop(t)
			}()
			retired, survivor := f.Stream1, f.Stream2
			retiredProject, survivorProject := f.Project, f.Project2
			if retired > survivor {
				retired, survivor = survivor, retired
				retiredProject, survivorProject = survivorProject, retiredProject
			}
			f.Engine.Mu.Lock()
			f.Engine.Turns["project-spec-draft-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				_, err := fixturekit.CallTool(ctx, tools, lifecycle.ProjectSpecDraftTool, map[string]any{
					"behavior":  "Chunks the server accepts are acknowledged with their offset; the client resumes uploads from the last acknowledged offset.",
					"contracts": []map[string]any{{"name": "acknowledgment", "text": "The server's acknowledgment of a stored chunk names its offset; the client reads that offset to resume."}},
					"graph":     []map[string]any{{"from": string(retired), "to": string(survivor), "type": "contract"}},
					"slices":    []map[string]any{{"workstream": string(f.Stream1), "contract_criteria": []int{2}}, {"workstream": string(f.Stream2), "contract_criteria": []int{1}}},
				})
				return intakefixture.QuestionResult(req, "session-project-spec", "The least member supplies the shared contract."), err
			}
			f.Engine.Mu.Unlock()
			f.StartOwner(t)
			packet := f.AwaitPacket(t)
			_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
			fixturekit.Must(t, err)
			f.AwaitApproved(t)
			repo, err := f.S.Repository(survivorProject)
			fixturekit.Must(t, err)
			wt, host, _, closeAuthority, err := f.S.Lifecycle().ChiefRepository(ctx, repo, survivor)
			fixturekit.Must(t, err)
			approval, approvalDoc, found, err := trace.ReadGroupApproval(wt, host)
			fixturekit.Must(t, err)
			if !found || !approval.Complete {
				t.Fatal("production effective seals are incomplete")
			}
			group := approval.Workgroup
			closeAuthority()

			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var approvals atomic.Int32
			var revision atomic.Int64
			const key = "retired-driver-change"
			f.Engine.Mu.Lock()
			f.Engine.Turns["change-unit-boundary"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
				select {
				case <-release:
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
						if sl.Workstream == retired && !affectsRetired {
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
					approvals.Add(1)
					_, err = fixturekit.CallTool(ctx, tools, lifecycle.DecideChangeTool, map[string]any{"id": key, "revision": int(revision.Load()), "decision": "approve", "pull_requests": "draft"})
					return intakefixture.QuestionResult(req, "session-approval", "Approved this impact and draft disposition."), err
				}
				if strings.Contains(req.Prompt, "Request the driver change") {
					scope := "workstream"
					if affectsRetired {
						scope = "workgroup"
					}
					_, err = fixturekit.CallTool(ctx, tools, lifecycle.RequestChangeTool, toolMap(lifecycle.ChangeRequest{Key: key, Kind: kind, Scope: scope, Workstream: survivor, Change: "Cancel the selected work or add diagnostic refusal checks to its selected slices."}))
					return intakefixture.QuestionResult(req, "session-request", "The impact will be presented here."), err
				}
				return intakefixture.QuestionResult(req, "session-chief", "Following the original shared conversation."), nil
			}
			f.Engine.Mu.Unlock()
			f.CheckContinuation(t)
			lifecycleWorker(t, f, retiredProject, retired, "worker_first", "change-unit-boundary", "Finish this whole unit before changing intent.")
			lifecycleWorker(t, f, survivorProject, survivor, "worker_second", "change-unit-boundary", "Finish this whole unit before changing intent.")
			t.Log("waiting: both member turns running")
			journey.LifecycleAwait(t, f, "both member turns running", func() bool {
				for _, m := range []struct {
					project config.ProjectID
					stream  config.WorkstreamID
					worker  string
				}{{retiredProject, retired, "worker_first"}, {survivorProject, survivor, "worker_second"}} {
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
			if strings.HasPrefix(tc.order, "merged-followup") {
				fixturekit.Must(t, delivery.RecordOwnPullRequestOutcome(ctx, repo, f.S.Now(), survivor, "owner", 7, delivery.OwnPullRequestOutcomeMerged))
			}
			_, err = f.C.Send(ctx, f.Origin, "Request the driver change.")
			fixturekit.Must(t, err)
			t.Log("waiting: single impact presented")
			journey.LifecycleAwait(t, f, "single impact presented", func() bool {
				c := journey.CurrentChange(t, f.S, f.Origin, key)
				if c.State == "failed" {
					t.Fatalf("delta failed before presentation: %s", c.Failure)
				}
				return c.State == "presented"
			})
			presented := journey.CurrentChange(t, f.S, f.Origin, key)
			revision.Store(int64(presented.Revision))
			approve := func() lifecycle.WorkgroupChange {
				t.Helper()
				_, err = f.C.Send(ctx, f.Origin, "Approve the driver impact; leave open PRs as drafts.")
				fixturekit.Must(t, err)
				t.Log("waiting: approved boundary stop")
				lastRevision := -1
				journey.LifecycleAwait(t, f, "approved boundary stop", func() bool {
					c := journey.CurrentChange(t, f.S, f.Origin, key)
					if c.Revision != lastRevision {
						t.Logf("approval boundary: state=%s revision=%d decision=%+v attempts=%d failure=%s", c.State, c.Revision, c.Decision, approvals.Load(), c.Failure)
						lastRevision = c.Revision
					}
					return c.Decision != nil && c.State == "applying"
				})
				pinned := journey.CurrentChange(t, f.S, f.Origin, key)
				if len(pinned.Applied) != 0 {
					t.Fatalf("change crossed a running unit: %+v", pinned)
				}
				if !intakefixture.EqualJSONTest(pinned.Before, presented.Before) || !intakefixture.EqualJSONTest(pinned.After, presented.After) || !intakefixture.EqualJSONTest(pinned.Members, presented.Members) || !intakefixture.EqualJSONTest(pinned.Impact, presented.Impact) || pinned.Decision.Revision != presented.Revision {
					t.Fatal("approval replaced the presented pins")
				}
				return pinned
			}
			approveAfterArchive := affectsRetired && resumeRetained && tc.order == "ordinary"
			var pinned lifecycle.WorkgroupChange
			if !approveAfterArchive {
				pinned = approve()
			}
			if strings.HasPrefix(tc.order, "merged-followup") {
				interrupted := tc.order != "merged-followup"
				if interrupted {
					race.Armed.Store(true)
				}
				unblock()
				if interrupted {
					race.Await(t)
					authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, group)
					fixturekit.Must(t, err)
					changes, err := lifecycle.ReadChanges(authority, host)
					fixturekit.Must(t, err)
					fixturekit.Must(t, authority.Close())
					if len(changes) != 1 || len(changes[0].Followups) != 1 || len(changes[0].Applied) != 0 || !intakefixture.EqualJSONTest(changes[0].Decision, pinned.Decision) {
						t.Fatal("interrupted follow-up lost pins or falsely completed")
					}
					f.Stop(t)
					race.Armed.Store(false)
					race.Release()
					f.Start(t)
				}
				journey.LifecycleAwait(t, f, "approved shared follow-up applies", func() bool {
					c := journey.CurrentChange(t, f.S, f.Origin, key)
					return c.State == "applied" && c.Followups[survivor] != ""
				})
				c := journey.CurrentChange(t, f.S, f.Origin, key)
				if len(c.Followups) != 1 || len(c.Applied) != 1 || !intakefixture.EqualJSONTest(c.Decision, pinned.Decision) {
					t.Fatalf("follow-up duplicated effects or changed approval: %+v", c)
				}
				checkFollowupContinuation(t, f, survivorProject, c.Followups[survivor], "Owner-approved follow-up after merged work: "+c.Request.Change+"\n\n"+func() string { sl, _ := trace.SliceFor(c.After, survivor); return sl.Spec }())
				f.Stop(t)
				f.Start(t)
				c = journey.CurrentChange(t, f.S, survivor, key)
				if c.State != "applied" || len(c.Followups) != 1 || len(c.Applied) != 1 || !intakefixture.EqualJSONTest(c.Decision, pinned.Decision) {
					t.Fatal("restart changed the completed follow-up")
				}
				authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, group)
				fixturekit.Must(t, err)
				gotApproval, doc, found, err := trace.ReadGroupApproval(authority, host)
				fixturekit.Must(t, err)
				if !found || !intakefixture.EqualJSONTest(approval, gotApproval) || !intakefixture.EqualJSONTest(approvalDoc, doc) {
					t.Fatal("follow-up rewrote original approval pins")
				}
				groups, _, err := trace.Workgroups(authority)
				fixturekit.Must(t, err)
				actual, ok := trace.WorkgroupOf(groups, survivor)
				if !ok {
					t.Fatal("follow-up lost original membership")
				}
				actual.Members = append(slices.Clone(actual.Members), config.WorkstreamID("w_ffffffffffffffffffffffffffffffff"))
				if _, err := trace.ApprovedWorkgroupMembers(authority, actual, approval); err == nil {
					t.Fatal("unapproved extra member was admitted")
				}
				fixturekit.Must(t, authority.Close())
				return
			}
			_, err = f.C.Abandon(ctx, retired, "The owner cancelled the driver.")
			fixturekit.Must(t, err)
			if tc.order == "change-first" {
				race.Armed.Store(true)
				unblock()
				race.Await(t)
				_, err = f.C.Archive(ctx, retired)
				if err == nil || !strings.Contains(err.Error(), "an approved change is being applied to this workgroup; retry") {
					t.Fatalf("archive must refuse an in-flight member effect promptly: %v", err)
				}
				if _, archived := intakefixture.ArchiveRecord(t, f.S.Store, retired); archived {
					t.Fatal("refused archive published retirement")
				}
				race.Release()
				journey.LifecycleAwait(t, f, "application wins retirement race", func() bool {
					c := journey.CurrentChange(t, f.S, f.Origin, key)
					return c.State == "applied" && len(c.Applied) == 2
				})
			}
			archive := func() error {
				archiveCtx, cancel := context.WithTimeout(ctx, fixturekit.DemoTimeout)
				defer cancel()
				retry := time.NewTicker(10 * time.Millisecond)
				defer retry.Stop()
				for {
					_, err := f.C.Archive(archiveCtx, retired)
					if err == nil || !strings.Contains(err.Error(), "an approved change is being applied to this workgroup; retry") {
						return err
					}
					select {
					case <-archiveCtx.Done():
						return archiveCtx.Err()
					case <-retry.C:
					}
				}
			}
			if tc.order == "archive-first" {
				race.Armed.Store(true)
				archived := make(chan error, 1)
				go func() { archived <- archive() }()
				select {
				case <-race.Reached:
				case err := <-archived:
					t.Fatalf("archive returned before checkpoint: %v", err)
				case <-time.After(fixturekit.DemoTimeout):
					t.Fatal("archive checkpoint not reached")
				}
				unblock()
				lock := f.S.Lifecycle().ChangeApplicationLock(group)
				if lock.TryLock() {
					lock.Unlock()
					t.Fatal("archive snapshot did not exclude change effects")
				}
				authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, group)
				fixturekit.Must(t, err)
				changes, err := lifecycle.ReadChanges(authority, host)
				fixturekit.Must(t, err)
				fixturekit.Must(t, authority.Close())
				if len(changes) != 1 || !intakefixture.EqualJSONTest(changes[0], pinned) {
					t.Fatal("application crossed the held retirement snapshot")
				}
				race.Release()
				select {
				case err := <-archived:
					fixturekit.Must(t, err)
				case <-time.After(fixturekit.DemoTimeout):
					t.Fatal("archive did not finish after snapshot release")
				}
			} else {
				journey.LifecycleAwait(t, f, "owner archive admitted", func() bool {
					err := archive()
					if err != nil && strings.Contains(err.Error(), "an approved change is being applied to this workgroup; retry") {
						return false
					}
					fixturekit.Must(t, err)
					return true
				})
			}
			journey.LifecycleAwait(t, f, "eligible production cleanup", func() bool {
				select {
				case <-cleanup.Reached:
					return true
				default:
					return false
				}
			})
			if approveAfterArchive {
				pinned = approve()
			}
			check := func(boundary string) lifecycle.WorkgroupChange {
				t.Helper()
				authority, err := trace.OpenWorkgroupAuthority(f.S.Current().Root, group)
				fixturekit.Must(t, err)
				defer authority.Close()
				gotApproval, gotDoc, found, err := trace.ReadGroupApproval(authority, host)
				fixturekit.Must(t, err)
				if !found || !intakefixture.EqualJSONTest(gotApproval, approval) || !intakefixture.EqualJSONTest(gotDoc, approvalDoc) {
					t.Fatalf("%s changed original effective approval pins", boundary)
				}
				changes, err := lifecycle.ReadChanges(authority, host)
				fixturekit.Must(t, err)
				if len(changes) != 1 {
					t.Fatalf("%s lost or duplicated approved journal: %+v", boundary, changes)
				}
				c := changes[0]
				if !intakefixture.EqualJSONTest(c.Decision, pinned.Decision) || !intakefixture.EqualJSONTest(c.Before, pinned.Before) || !intakefixture.EqualJSONTest(c.After, pinned.After) || !intakefixture.EqualJSONTest(c.Members, pinned.Members) || !intakefixture.EqualJSONTest(c.Impact, pinned.Impact) {
					t.Fatalf("%s reselected approved intent or member pins", boundary)
				}
				if retiresAffected && slices.Contains(c.Applied, retired) {
					t.Fatalf("%s falsely applied retired effect", boundary)
				}
				return c
			}
			consume := func() {
				t.Helper()
				unblock()
				journey.LifecycleAwait(t, f, "survivor consumes original approval", func() bool {
					c := check("reconciling")
					return slices.Contains(c.Applied, survivor) && c.State == "applied" && slices.Contains(c.Retired, retired) == retiresAffected && c.Failure == ""
				})
			}
			check("retained")
			if resumeRetained {
				retiredRepo, err := f.S.Repository(retiredProject)
				fixturekit.Must(t, err)
				history, err := trace.Read[trace.Transition](retiredRepo, retired)
				fixturekit.Must(t, err)
				consume()
				after, err := trace.Read[trace.Transition](retiredRepo, retired)
				fixturekit.Must(t, err)
				if !intakefixture.EqualJSONTest(history, after) {
					t.Fatal("survivor effect mutated retained retired history")
				}
			}
			cleanup.Release()
			t.Log("waiting: legitimate cleanup")
			journey.LifecycleAwait(t, f, "legitimate cleanup", func() bool {
				a, ok := intakefixture.ArchiveRecord(t, f.S.Store, retired)
				return ok && !a.CleanedAt.IsZero()
			})
			check("deleted")
			f.Stop(t)
			f.Start(t)
			check("restarted pending")
			if !resumeRetained {
				consume()
			}
			finished := check("survivor effect")
			expectedApplied := 1
			if tc.order == "change-first" {
				expectedApplied = 2
			}
			if len(finished.Applied) != expectedApplied {
				t.Fatalf("duplicate effect or owner decision: %+v approvals=%d", finished, approvals.Load())
			}
			repo, err = f.S.Repository(survivorProject)
			fixturekit.Must(t, err)
			assertEffect := func() {
				t.Helper()
				if kind == "cancel" {
					transitions, err := trace.Read[trace.Transition](repo, survivor)
					fixturekit.Must(t, err)
					count := 0
					for _, tr := range transitions {
						if tr.ID == "cancel-"+key {
							count++
						}
					}
					if count != 1 {
						t.Fatalf("survivor cancellation reconciled %d times", count)
					}
					return
				}
				docs, err := trace.Read[trace.Document](repo, survivor)
				fixturekit.Must(t, err)
				count := 0
				for _, doc := range docs {
					if doc.ID != amendment.DocumentID("group-"+key) {
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
			assertEffect()
			f.Stop(t)
			f.Start(t)
			sent, err := f.C.Send(ctx, survivor, "Confirm survivor journal status after restart.")
			fixturekit.Must(t, err)
			journey.LifecycleAwait(t, f, "survivor owner turn after restart", func() bool {
				conversation, err := f.C.Conversation(ctx, survivor)
				fixturekit.Must(t, err)
				return slices.ContainsFunc(conversation.Entries, func(e api.ConversationEntry) bool {
					return e.Turn == sent.Turn && e.Kind == "response" && e.State == api.TurnDone
				})
			})
			if c := check("second real restart"); c.Revision != finished.Revision {
				t.Fatalf("restart rewrote completed effects or hold: before=%+v after=%+v", finished, c)
			}
			repo, err = f.S.Repository(survivorProject)
			fixturekit.Must(t, err)
			assertEffect()
			gone, err := trace.WorkstreamTraceGone(f.S.Current().Root, retiredProject, retired)
			fixturekit.Must(t, err)
			if !gone {
				t.Fatal("change reconciliation recreated retired history")
			}
			conversation, err := f.C.Conversation(ctx, survivor)
			fixturekit.Must(t, err)
			prompts, ownerDecisions := 0, 0
			for _, entry := range conversation.Entries {
				if entry.Kind == "message" && strings.Contains(entry.Text, "Approve the driver impact") {
					ownerDecisions++
				}
				if entry.Turn == "change-"+key+"-impact" {
					prompts++
				}
			}
			if prompts != 1 || ownerDecisions != 1 {
				t.Fatalf("continued conversation received %d impact prompts and %d owner decisions", prompts, ownerDecisions)
			}
			if retiresAffected && !strings.Contains(finished.Summary(), "Archived before this change reached it: "+string(retired)) {
				t.Fatal("impact hides the archived member that received no effect")
			}
			repo, err = f.S.Repository(survivorProject)
			fixturekit.Must(t, err)
			authority, _, _, closeAuthority, err := f.S.Lifecycle().ChiefRepository(ctx, repo, survivor)
			fixturekit.Must(t, err)
			chief, err := authority.Thread(host, trace.ChiefOfStaff)
			closeAuthority()
			fixturekit.Must(t, err)
			if !intakefixture.EqualJSONTest(f.Before.Identity, chief.Identity) || len(chief.Turns) < len(f.Before.Turns) || !intakefixture.EqualJSONTest(f.Before.Turns, chief.Turns[:len(f.Before.Turns)]) {
				t.Fatal("retirement changed original conversation or design turns")
			}
		})
	}
}
func TestArchiveSucceedsDuringRoutineGroupApprovalRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gate := &intakefixture.ChangeRaceGate{Step: "group-approval-retry-locked", Reached: make(chan struct{})}
	t.Cleanup(gate.Release)
	cleanup := &journey.LifecycleCleanupGate{Reached: make(chan struct{})}
	t.Cleanup(cleanup.Release)
	f := journey.NewConfiguredProductionDesignFixture(t, journey.ProductionDesignConfiguration{Wait: journey.LifecycleAwait, Checkpoint: func(ctx context.Context, step string) error {
		if err := gate.Checkpoint(ctx, step); err != nil {
			return err
		}
		return cleanup.Checkpoint(ctx, step)
	}})
	defer func() { f.Stop(t) }()
	f.StartOwner(t)
	packet := f.AwaitPacket(t)
	_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	fixturekit.Must(t, err)
	f.AwaitApproved(t)
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	group, err := repo.CrossRepoWorkgroupOf(f.Stream1)
	fixturekit.Must(t, err)
	_, err = f.C.Abandon(ctx, f.Stream1, "Owner retires this completed member.")
	fixturekit.Must(t, err)
	gate.Armed.Store(true)
	retry := make(chan error, 1)
	go func() {
		_, err := f.C.Ratify(ctx, f.Origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
		retry <- err
	}()
	select {
	case <-gate.Reached:
	case err := <-retry:
		t.Fatalf("approval retry returned before checkpoint: %v", err)
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("approval retry checkpoint not reached")
	}
	archived := make(chan error, 1)
	go func() { _, err := f.C.Archive(ctx, f.Stream1); archived <- err }()
	lock := journey.InspectForTest(f.S).GroupApprovalLock(group)
	if lock.TryLock() {
		lock.Unlock()
		t.Fatal("approval checkpoint did not retain group coordination")
	}
	select {
	case err := <-archived:
		fixturekit.Must(t, err)
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("archive waited for routine group coordination")
	}
	gate.Release()
	select {
	case err := <-retry:
		fixturekit.Must(t, err)
	case <-time.After(fixturekit.DemoTimeout):
		t.Fatal("approval retry did not finish")
	}
	cleanup.Await(t)
}
func checkFollowupContinuation(t *testing.T, f *journey.ProductionDesignFixture, project config.ProjectID, addition config.WorkstreamID, brief string) {
	t.Helper()
	ctx := context.Background()
	for i, m := range []config.WorkstreamID{f.Stream1, f.Stream2, addition} {
		p := f.Project
		if i == 1 {
			p = f.Project2
		}
		if i == 2 {
			p = project
		}
		repo, err := f.S.Repository(p)
		fixturekit.Must(t, err)
		streams, err := repo.Workstreams()
		fixturekit.Must(t, err)
		features := slices.DeleteFunc(slices.Clone(streams), func(stream config.WorkstreamID) bool {
			return stream == f.Origin
		})
		expected := []config.WorkstreamID{f.Stream1}
		if p == f.Project2 {
			expected = []config.WorkstreamID{f.Stream2}
		}
		if p == project {
			expected = append(expected, addition)
		}
		slices.Sort(features)
		slices.Sort(expected)
		if !slices.Equal(features, expected) {
			t.Fatalf("repo has lost or duplicated its allocated member: %v", features)
		}
		if f.S.Context().Mode(p) != bundle.ModeFile {
			t.Fatal("production path requires Hearsay")
		}
		handed, err := trace.Get[trace.Document](repo, m, intake.HandedDocument, 1)
		fixturekit.Must(t, err)
		expectedBrief, source := f.Handed.Content, f.Handed.Source
		if m == addition {
			expectedBrief, source = brief, "stdin"
		}
		if handed.Content != expectedBrief || handed.Source != source {
			t.Fatal("member input differs from the immutable brief")
		}
		cfg := f.S.Current().For(p)
		for _, name := range []string{"spec.md", "plan.json", "handed", "tools/design-brief.json", "workgroups", ".osmia"} {
			if _, err := os.Stat(filepath.Join(cfg.Project.Clone, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("factory state leaked into target repo at %s: %v", name, err)
			}
		}
		if _, err := repo.Thread(m, trace.ChiefOfStaff); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("member retains an independent chief: %v", err)
		}
		groups, _, err := trace.Workgroups(repo)
		fixturekit.Must(t, err)
		if _, found := trace.WorkgroupOf(groups, m); found {
			t.Fatal("member retains a one-workstream group")
		}
		target, host, _, close, err := f.S.Lifecycle().ChiefRepository(ctx, repo, m)
		fixturekit.Must(t, err)
		chief, err := target.Thread(host, trace.ChiefOfStaff)
		close()
		fixturekit.Must(t, err)
		if len(chief.Turns) < len(f.Before.Turns) {
			t.Fatal("design turns were lost")
		}
		if !intakefixture.EqualJSONTest(f.Before.Identity, chief.Identity) || !intakefixture.EqualJSONTest(f.Before.Turns, chief.Turns[:len(f.Before.Turns)]) {
			t.Fatal("design identity, requests, responses or recovery state changed at adoption")
		}
	}
	repo, err := f.S.Repository(f.Project)
	fixturekit.Must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(f.Origin)
	fixturekit.Must(t, err)
	_, group, _, err := repo.CrossRepoProjectSpec(id)
	fixturekit.Must(t, err)
	if group.Conversation == nil || group.Conversation.Workstream != f.Origin || group.Conversation.Project != f.Project ||
		len(group.Members) != 4 || group.Members[0] != f.Origin ||
		!slices.Contains(group.Members[1:], f.Stream1) || !slices.Contains(group.Members[1:], f.Stream2) || !slices.Contains(group.Members[1:], addition) {
		t.Fatalf("continued conversation or exact member coverage changed: %+v", group)
	}
	groups, err := os.ReadDir(filepath.Join(f.S.Current().Root.String(), "workgroups"))
	fixturekit.Must(t, err)
	if len(groups) != 1 || groups[0].Name() != string(id) {
		t.Fatalf("start or recovery duplicated workgroup authority: %v", groups)
	}
	handed, err := trace.Get[trace.Document](repo, f.Origin, intake.HandedDocument, 1)
	fixturekit.Must(t, err)
	if !intakefixture.EqualJSONTest(handed, f.Handed) {
		t.Fatal("immutable handed brief changed")
	}
	alias, err := f.C.Status(ctx, f.Origin)
	fixturekit.Must(t, err)
	if alias.Workstream != f.Stream1 || alias.Project != f.Project {
		t.Fatalf("design status did not resolve to its member: %+v", alias)
	}
	listed, err := f.C.Statuses(ctx)
	fixturekit.Must(t, err)
	if slices.ContainsFunc(listed.Workstreams, func(w api.WorkstreamStatus) bool { return w.Workstream == f.Origin }) {
		t.Fatal("status list exposes the adopted design host as independent work")
	}
	if _, err := repo.Thread(f.Origin, trace.ChiefOfStaff); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source retains an authoritative queue")
	}
	historical, err := trace.Read[trace.TurnRequest](repo, f.Origin)
	fixturekit.Must(t, err)
	if !slices.ContainsFunc(historical, func(req trace.TurnRequest) bool { return req.ID == f.Before.Turns[0].Request.ID }) {
		t.Fatal("historical design request was lost")
	}
	chief, _, _, closeChief, err := f.S.Lifecycle().ChiefRepository(ctx, repo, f.Stream1)
	fixturekit.Must(t, err)
	defer closeChief()
	matching, err := readview.Consistent(func() (bool, error) {
		var entries []api.ConversationEntry
		var feed []api.FeedEntry
		for i, member := range []config.WorkstreamID{f.Origin, f.Stream1, f.Stream2, addition} {
			conversation, err := f.C.Conversation(ctx, member)
			if err != nil {
				return false, err
			}
			got, apiFault := f.S.FeedList(string(member))
			if apiFault != nil {
				return false, intake.ApiErrAsError{Api: apiFault}
			}
			shared := slices.DeleteFunc(slices.Clone(got.Entries), func(e api.FeedEntry) bool {
				return e.Kind != "message" && e.Kind != "response" && e.Kind != "action" && e.Kind != "status"
			})
			if i == 0 {
				entries, feed = conversation.Entries, shared
			} else if !intakefixture.EqualJSONTest(entries, conversation.Entries) || !intakefixture.EqualJSONTest(feed, shared) {
				return false, nil
			}
		}
		return true, nil
	}, chief.Generation)
	fixturekit.Must(t, err)
	if !matching {
		t.Fatal("continued address and member endpoints resolve to different conversations or feeds")
	}
}
