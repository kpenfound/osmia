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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/osmia/internal/amendment"
	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestArchivedWorkstreamKeepsLiveChangesAndStatusAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newTwoProjectFixture(t)
	opts, cfg := f.opts, f.cfg
	// Reads do not advance service time while cleanup is deliberately held.
	// Scheduled deadlines are unnecessary for these immediate owner turns.
	now := f.clock.Now()
	opts.Reconciliation.Now = func() time.Time { return now }
	runChief(&opts, cfg, &chiefTurns{})
	cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(cleanup.release)
	previous := opts.boundary
	opts.boundary = func(ctx context.Context, step string) error {
		if previous != nil {
			if err := previous(ctx, step); err != nil {
				return err
			}
		}
		return cleanup.checkpoint(ctx, step)
	}
	s, err := Start(ctx, opts)
	must(t, err)
	t.Cleanup(func() { must(t, s.Close()) })
	c := NewClient(s.Socket())
	t.Cleanup(func() { c.Close() })

	// Keep the archived trace discoverable until a live controller pass has
	// dispatched an unrelated owner's turn through every scheduling hook.
	_, err = c.Abandon(ctx, stream, "Superseded.")
	must(t, err)
	_, err = c.Archive(ctx, stream)
	must(t, err)

	assertLive := func(deleted bool) {
		t.Helper()
		sent, err := c.Send(ctx, otherStream, "Continue the unrelated work.")
		must(t, err)
		wait, closeWait := serviceChanges(t, s)
		defer closeWait()
		deadline := time.Now().Add(demoTimeout)
		for {
			conversation, err := c.Conversation(ctx, otherStream)
			must(t, err)
			if slices.ContainsFunc(conversation.Entries, func(e ConversationEntry) bool {
				return e.Turn == sent.Turn && e.Kind == "response" && e.State == TurnDone && e.Text == "Answer to Continue the unrelated work."
			}) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("unrelated work stopped after archive: %+v", conversation)
			}
			wait()
		}
		health, err := c.Health(ctx)
		must(t, err)
		if !health.Ready || !health.API || health.Backend != BackendReady {
			t.Fatalf("archive stopped the live controller: %+v", health)
		}
		archived, err := c.Status(ctx, stream)
		must(t, err)
		if !archived.Archived || archived.TraceDeleted != deleted || archived.State == nil || *archived.State != AbandonedState {
			t.Fatalf("archived status: %+v", archived)
		}
		list, err := c.Statuses(ctx)
		must(t, err)
		if len(list.Diagnostics) != 0 || !slices.ContainsFunc(list.Workstreams, func(w WorkstreamStatus) bool { return w.Workstream == otherStream && !w.Archived }) ||
			!slices.ContainsFunc(list.Workstreams, func(w WorkstreamStatus) bool { return w.Workstream == stream && w.Archived }) {
			t.Fatalf("global status after archive: %+v", list)
		}
	}
	assertLive(false)
	// A restart must honor the durable archive even before cleanup can run.
	c.Close()
	must(t, s.Close())
	s, err = Start(ctx, opts)
	must(t, err)
	c = NewClient(s.Socket())
	assertLive(false)

	cleanup.await(t)
	cleanup.release()
	wait, closeWait := serviceChanges(t, s)
	deadline := time.Now().Add(demoTimeout)
	for {
		a, ok := archiveRecord(t, s.store, stream)
		if ok && !a.CleanedAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owner-authorized archive cleanup did not finish: %+v", a)
		}
		wait()
	}
	closeWait()
	archivedRepo, err := s.repository(project)
	must(t, err)
	remaining, err := archivedRepo.Workstreams()
	must(t, err)
	if slices.Contains(remaining, stream) {
		t.Fatalf("cleanup changed unrelated work: %v", remaining)
	}
	unrelatedRepo, err := s.repository(otherProject)
	must(t, err)
	unrelated, err := unrelatedRepo.Workstreams()
	must(t, err)
	if !slices.Contains(unrelated, otherStream) {
		t.Fatal("cleanup removed unrelated work")
	}
	assertLive(true)
	c.Close()
	must(t, s.Close())
	s, err = Start(ctx, opts)
	must(t, err)
	c = NewClient(s.Socket())
	assertLive(true)
}

func TestChangePassReportsLiveTraceErrorsAndCancellation(t *testing.T) {
	t.Parallel()
	opts, cfg := conversationFixture(t, "change-errors-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { must(t, repo.Close()) })
	s := &Service{cfg: cfg, options: opts}
	s.setSole(runtimeFor(repo))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.changePass(ctx, repo); err != context.Canceled {
		t.Fatalf("pass lost cancellation: %v", err)
	}
	// A closed live repository is an operational error, never an archive.
	must(t, repo.Close())
	if err := s.changePass(context.Background(), repo); err == nil {
		t.Fatal("pass suppressed a live trace error without an archive record")
	}
}

// changeRaceGate stops only the selected production boundary after approval.
type changeRaceGate struct {
	step        string
	armed       atomic.Bool
	reached     chan struct{}
	released    chan struct{}
	once        sync.Once
	releaseOnce sync.Once
}

func (g *changeRaceGate) checkpoint(ctx context.Context, step string) error {
	if !g.armed.Load() || step != g.step {
		return nil
	}
	g.once.Do(func() { close(g.reached) })
	select {
	case <-g.released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (g *changeRaceGate) release() { g.releaseOnce.Do(func() { close(g.released) }) }
func (g *changeRaceGate) await(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(demoTimeout):
		t.Fatalf("production checkpoint %s not reached", g.step)
	}
}

func TestChangePassAppliesPendingChangeAfterArchivedCrossRepoLeader(t *testing.T) {
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
		requiresHold := affectsRetired && tc.order != "change-first"
		t.Run(fmt.Sprintf("%s/retired-affected=%t/resume-retained=%t/%s", kind, affectsRetired, resumeRetained, tc.order), func(t *testing.T) {
			ctx := context.Background()
			cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
			t.Cleanup(cleanup.release)
			race := &changeRaceGate{step: "member-retirement-snapshot", reached: make(chan struct{}), released: make(chan struct{})}
			if tc.order == "change-first" {
				race.step = "change-before-member-effect"
			}
			if tc.order == "merged-followup-allocation" {
				race.step = "change-followup-allocated"
			}
			if tc.order == "merged-followup-join" {
				race.step = "change-followup-joined"
			}
			t.Cleanup(race.release)
			f := newConfiguredProductionDesignFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: func(ctx context.Context, step string) error {
				if err := race.checkpoint(ctx, step); err != nil {
					return err
				}
				return cleanup.checkpoint(ctx, step)
			}})
			defer func() { f.stop(t) }()
			retired, survivor := f.stream1, f.stream2
			retiredProject, survivorProject := f.project, f.project2
			if retired > survivor {
				retired, survivor = survivor, retired
				retiredProject, survivorProject = survivorProject, retiredProject
			}
			f.engine.mu.Lock()
			f.engine.turns["project-spec-draft-1"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				_, err := callTool(ctx, tools, ProjectSpecDraftTool, map[string]any{
					"behavior":  "Chunks the server accepts are acknowledged with their offset; the client resumes uploads from the last acknowledged offset.",
					"contracts": []map[string]any{{"name": "acknowledgment", "text": "The server's acknowledgment of a stored chunk names its offset; the client reads that offset to resume."}},
					"graph":     []map[string]any{{"from": string(retired), "to": string(survivor), "type": "contract"}},
					"slices":    []map[string]any{{"workstream": string(f.stream1), "contract_criteria": []int{2}}, {"workstream": string(f.stream2), "contract_criteria": []int{1}}},
				})
				return questionResult(req, "session-project-spec", "The least member supplies the shared contract."), err
			}
			f.engine.mu.Unlock()
			f.startOwner(t)
			packet := f.awaitPacket(t)
			_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan)
			must(t, err)
			f.awaitApproved(t)
			repo, err := f.s.repository(survivorProject)
			must(t, err)
			wt, host, _, closeAuthority, err := f.s.chiefRepository(ctx, repo, survivor)
			must(t, err)
			approval, approvalDoc, found, err := trace.ReadGroupApproval(wt, host)
			must(t, err)
			if !found || !approval.Complete {
				t.Fatal("production effective seals are incomplete")
			}
			group := approval.Workgroup
			closeAuthority()

			// Real running member turns hold the approved operation at its
			// unit boundary while the owner retires the driver. Shutdown
			// cancels these turns; replay keeps the same bounded gate.
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var approvals atomic.Int32
			var revision atomic.Int64
			const key = "retired-driver-change"
			f.engine.mu.Lock()
			f.engine.turns["change-unit-boundary"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, _ *mcp.ClientSession) (*agent.Result, error) {
				select {
				case <-release:
					return questionResult(req, "session-boundary", "The whole unit boundary is safe."), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			f.engine.turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
				names, err := toolNames(ctx, tools)
				if err != nil {
					return nil, err
				}
				if slices.Contains(names, deltaDraftTool) {
					text, err := readTool(ctx, tools, "before.json")
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
						if sl.Workstream == f.stream1 {
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
					_, err = callTool(ctx, tools, deltaDraftTool, toolMap(proposed))
					return questionResult(req, "session-delta", "Added only the requested diagnostics."), err
				}
				if slices.Contains(names, deltaReviewTool) {
					_, err = callTool(ctx, tools, deltaReviewTool, map[string]any{"objections": []string{}, "note": "Only requested diagnostics change."})
					return questionResult(req, "session-review", "Reviewed the delta."), err
				}
				if slices.Contains(names, deltaReplyTool) {
					_, err = callTool(ctx, tools, deltaReplyTool, map[string]any{"reply": "Existing contract and unit assignments remain fixed."})
					return questionResult(req, "session-answer", "Answered the committee."), err
				}
				if strings.Contains(req.Prompt, "Confirm survivor journal status") {
					return questionResult(req, "session-owner-status", "The original journal and its prerequisites are preserved."), nil
				}
				if strings.Contains(req.Prompt, "Approve the driver impact") {
					approvals.Add(1)
					_, err = callTool(ctx, tools, decideChangeTool, map[string]any{"id": key, "revision": int(revision.Load()), "decision": "approve", "pull_requests": "draft"})
					return questionResult(req, "session-approval", "Approved this impact and draft disposition."), err
				}
				if strings.Contains(req.Prompt, "Request the driver change") {
					scope := "workstream"
					if affectsRetired {
						scope = "workgroup"
					}
					_, err = callTool(ctx, tools, requestChangeTool, toolMap(ChangeRequest{Key: key, Kind: kind, Scope: scope, Workstream: survivor, Change: "Cancel the selected work or add diagnostic refusal checks to its selected slices."}))
					return questionResult(req, "session-request", "The impact will be presented here."), err
				}
				return questionResult(req, "session-chief", "Following the original shared conversation."), nil
			}
			f.engine.mu.Unlock()
			f.checkContinuation(t)
			lifecycleWorker(t, f, retiredProject, retired, "worker_first", "change-unit-boundary", "Finish this whole unit before changing intent.")
			lifecycleWorker(t, f, survivorProject, survivor, "worker_second", "change-unit-boundary", "Finish this whole unit before changing intent.")
			t.Log("waiting: both member turns running")
			lifecycleAwait(t, f, "both member turns running", func() bool {
				for _, m := range []struct {
					project config.ProjectID
					stream  config.WorkstreamID
					worker  string
				}{{retiredProject, retired, "worker_first"}, {survivorProject, survivor, "worker_second"}} {
					r, err := f.s.repository(m.project)
					must(t, err)
					th, err := r.Thread(m.stream, m.worker)
					must(t, err)
					if th.Active == "" {
						return false
					}
				}
				return true
			})
			if strings.HasPrefix(tc.order, "merged-followup") {
				must(t, recordOwnPullRequestOutcome(ctx, repo, f.s.now(), survivor, "owner", 7, ownPullRequestOutcomeMerged))
			}
			_, err = f.c.Send(ctx, f.origin, "Request the driver change.")
			must(t, err)
			t.Log("waiting: single impact presented")
			lifecycleAwait(t, f, "single impact presented", func() bool {
				c := currentChange(t, f.s, f.origin, key)
				if c.State == "failed" {
					t.Fatalf("delta failed before presentation: %s", c.Failure)
				}
				return c.State == "presented"
			})
			presented := currentChange(t, f.s, f.origin, key)
			revision.Store(int64(presented.Revision))
			approve := func() WorkgroupChange {
				t.Helper()
				_, err = f.c.Send(ctx, f.origin, "Approve the driver impact; leave open PRs as drafts.")
				must(t, err)
				t.Log("waiting: approved boundary stop")
				lastRevision := -1
				lifecycleAwait(t, f, "approved boundary stop", func() bool {
					c := currentChange(t, f.s, f.origin, key)
					if c.Revision != lastRevision {
						t.Logf("approval boundary: state=%s revision=%d decision=%+v attempts=%d failure=%s", c.State, c.Revision, c.Decision, approvals.Load(), c.Failure)
						lastRevision = c.Revision
					}
					return c.Decision != nil && c.State == "applying"
				})
				pinned := currentChange(t, f.s, f.origin, key)
				if len(pinned.Applied) != 0 {
					t.Fatalf("change crossed a running unit: %+v", pinned)
				}
				if !equalJSONTest(pinned.Before, presented.Before) || !equalJSONTest(pinned.After, presented.After) || !equalJSONTest(pinned.Members, presented.Members) || !equalJSONTest(pinned.Impact, presented.Impact) || pinned.Decision.Revision != presented.Revision {
					t.Fatal("approval replaced the presented pins")
				}
				return pinned
			}
			approveAfterArchive := affectsRetired && resumeRetained && tc.order == "ordinary"
			var pinned WorkgroupChange
			if !approveAfterArchive {
				pinned = approve()
			}
			if strings.HasPrefix(tc.order, "merged-followup") {
				interrupted := tc.order != "merged-followup"
				if interrupted {
					race.armed.Store(true)
				}
				unblock()
				if interrupted {
					race.await(t)
					authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group)
					must(t, err)
					changes, err := readChanges(authority, host)
					must(t, err)
					must(t, authority.Close())
					if len(changes) != 1 || len(changes[0].Followups) != 1 || len(changes[0].Applied) != 0 || !equalJSONTest(changes[0].Decision, pinned.Decision) {
						t.Fatal("interrupted follow-up lost pins or falsely completed")
					}
					f.stop(t)
					race.armed.Store(false)
					race.release()
					f.start(t)
				}
				lifecycleAwait(t, f, "approved shared follow-up applies", func() bool {
					c := currentChange(t, f.s, f.origin, key)
					return c.State == "applied" && c.Followups[survivor] != ""
				})
				c := currentChange(t, f.s, f.origin, key)
				if len(c.Followups) != 1 || len(c.Applied) != 1 || !equalJSONTest(c.Decision, pinned.Decision) {
					t.Fatalf("follow-up duplicated effects or changed approval: %+v", c)
				}
				checkFollowupContinuation(t, f, survivorProject, c.Followups[survivor], "Owner-approved follow-up after merged work: "+c.Request.Change+"\n\n"+func() string { sl, _ := trace.SliceFor(c.After, survivor); return sl.Spec }())
				f.stop(t)
				f.start(t)
				c = currentChange(t, f.s, survivor, key)
				if c.State != "applied" || len(c.Followups) != 1 || len(c.Applied) != 1 || !equalJSONTest(c.Decision, pinned.Decision) {
					t.Fatal("restart changed the completed follow-up")
				}
				authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group)
				must(t, err)
				gotApproval, doc, found, err := trace.ReadGroupApproval(authority, host)
				must(t, err)
				if !found || !equalJSONTest(approval, gotApproval) || !equalJSONTest(approvalDoc, doc) {
					t.Fatal("follow-up rewrote original approval pins")
				}
				// An unapproved extra remains a conflict even with a legitimate addition.
				groups, _, err := trace.Workgroups(authority)
				must(t, err)
				actual, ok := trace.WorkgroupOf(groups, survivor)
				if !ok {
					t.Fatal("follow-up lost original membership")
				}
				actual.Members = append(slices.Clone(actual.Members), config.WorkstreamID("w_ffffffffffffffffffffffffffffffff"))
				if _, err := trace.ApprovedWorkgroupMembers(authority, actual, approval); err == nil {
					t.Fatal("unapproved extra member was admitted")
				}
				must(t, authority.Close())
				return
			}
			_, err = f.c.Abandon(ctx, retired, "The owner cancelled the driver.")
			must(t, err)
			if tc.order == "change-first" {
				race.armed.Store(true)
				unblock()
				race.await(t)
				_, err = f.c.Archive(ctx, retired)
				if err == nil || !strings.Contains(err.Error(), "an approved change is being applied to this workgroup; retry") {
					t.Fatalf("archive must refuse an in-flight member effect promptly: %v", err)
				}
				if _, archived := archiveRecord(t, f.s.store, retired); archived {
					t.Fatal("refused archive published retirement")
				}
				race.release()
				lifecycleAwait(t, f, "application wins retirement race", func() bool {
					c := currentChange(t, f.s, f.origin, key)
					return c.State == "applied" && len(c.Applied) == 2
				})
			}
			archive := func() error {
				_, err := f.c.Archive(ctx, retired)
				return err
			}
			if tc.order == "archive-first" {
				race.armed.Store(true)
				archived := make(chan error, 1)
				go func() { archived <- archive() }()
				select {
				case <-race.reached:
				case err := <-archived:
					t.Fatalf("archive returned before checkpoint: %v", err)
				case <-time.After(demoTimeout):
					t.Fatal("archive checkpoint not reached")
				}
				unblock()
				lock := f.s.changeApplicationLock(group)
				if lock.TryLock() {
					lock.Unlock()
					t.Fatal("archive snapshot did not exclude change effects")
				}
				authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group)
				must(t, err)
				changes, err := readChanges(authority, host)
				must(t, err)
				must(t, authority.Close())
				if len(changes) != 1 || !equalJSONTest(changes[0], pinned) {
					t.Fatal("application crossed the held retirement snapshot")
				}
				race.release()
				select {
				case err := <-archived:
					must(t, err)
				case <-time.After(demoTimeout):
					t.Fatal("archive did not finish after snapshot release")
				}
			} else {
				// Retry only a real application; routine holders never refuse
				// an archive and reserve its next boundary.
				lifecycleAwait(t, f, "owner archive admitted", func() bool {
					err := archive()
					if err != nil && strings.Contains(err.Error(), "an approved change is being applied to this workgroup; retry") {
						return false
					}
					must(t, err)
					return true
				})
			}
			lifecycleAwait(t, f, "eligible production cleanup", func() bool {
				select {
				case <-cleanup.reached:
					return true
				default:
					return false
				}
			})
			if approveAfterArchive {
				pinned = approve()
			}
			check := func(boundary string) WorkgroupChange {
				t.Helper()
				authority, err := trace.OpenWorkgroupAuthority(f.s.current().Root, group)
				must(t, err)
				defer authority.Close()
				gotApproval, gotDoc, found, err := trace.ReadGroupApproval(authority, host)
				must(t, err)
				if !found || !equalJSONTest(gotApproval, approval) || !equalJSONTest(gotDoc, approvalDoc) {
					t.Fatalf("%s changed original effective approval pins", boundary)
				}
				changes, err := readChanges(authority, host)
				must(t, err)
				if len(changes) != 1 {
					t.Fatalf("%s lost or duplicated approved journal: %+v", boundary, changes)
				}
				c := changes[0]
				if !equalJSONTest(c.Decision, pinned.Decision) || !equalJSONTest(c.Before, pinned.Before) || !equalJSONTest(c.After, pinned.After) || !equalJSONTest(c.Members, pinned.Members) || !equalJSONTest(c.Impact, pinned.Impact) {
					t.Fatalf("%s reselected approved intent or member pins", boundary)
				}
				if requiresHold && slices.Contains(c.Applied, retired) {
					t.Fatalf("%s falsely applied retired effect", boundary)
				}
				return c
			}
			consume := func() {
				t.Helper()
				unblock()
				lifecycleAwait(t, f, "survivor consumes original approval", func() bool {
					c := check("reconciling")
					return slices.Contains(c.Applied, survivor) && ((!requiresHold && c.State == "applied") || (requiresHold && c.State == "applying" && strings.Contains(c.Failure, trace.ErrMemberRetired.Error())))
				})
			}
			check("retained")
			if resumeRetained {
				retiredRepo, err := f.s.repository(retiredProject)
				must(t, err)
				history, err := trace.Read[trace.Transition](retiredRepo, retired)
				must(t, err)
				consume()
				after, err := trace.Read[trace.Transition](retiredRepo, retired)
				must(t, err)
				if !equalJSONTest(history, after) {
					t.Fatal("survivor effect mutated retained retired history")
				}
			}
			cleanup.release()
			t.Log("waiting: legitimate cleanup")
			lifecycleAwait(t, f, "legitimate cleanup", func() bool { a, ok := archiveRecord(t, f.s.store, retired); return ok && !a.CleanedAt.IsZero() })
			check("deleted")
			f.stop(t)
			f.start(t)
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
			repo, err = f.s.repository(survivorProject)
			must(t, err)
			assertEffect := func() {
				t.Helper()
				if kind == "cancel" {
					transitions, err := trace.Read[trace.Transition](repo, survivor)
					must(t, err)
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
				must(t, err)
				count := 0
				for _, doc := range docs {
					if doc.ID != amendment.DocumentID("group-"+key) {
						continue
					}
					count++
					var app amendment.Application
					must(t, json.Unmarshal([]byte(doc.Content), &app))
					if doc.Revision != 1 || app.From.Seal != 1 || app.To.Seal != 2 || app.To.SealRevision != app.From.SealRevision+1 || app.To.Spec != app.From.Spec+1 || app.To.Plan != app.From.Plan+1 || !slices.Equal(app.Added, []string{"diagnostics"}) || len(app.Rework) != 0 || len(app.Removed) != 0 {
						t.Fatalf("survivor amendment changed unapproved sections or seals: %+v", app)
					}
				}
				if count != 1 {
					t.Fatalf("survivor amendment reconciled %d times", count)
				}
			}
			assertEffect()
			f.stop(t)
			f.start(t)
			sent, err := f.c.Send(ctx, survivor, "Confirm survivor journal status after restart.")
			must(t, err)
			lifecycleAwait(t, f, "survivor owner turn after restart", func() bool {
				conversation, err := f.c.Conversation(ctx, survivor)
				must(t, err)
				return slices.ContainsFunc(conversation.Entries, func(e ConversationEntry) bool {
					return e.Turn == sent.Turn && e.Kind == "response" && e.State == TurnDone
				})
			})
			if c := check("second real restart"); c.Revision != finished.Revision {
				t.Fatalf("restart rewrote completed effects or hold: before=%+v after=%+v", finished, c)
			}
			repo, err = f.s.repository(survivorProject)
			must(t, err)
			assertEffect()
			gone, err := trace.WorkstreamTraceGone(f.s.current().Root, retiredProject, retired)
			must(t, err)
			if !gone {
				t.Fatal("change reconciliation recreated retired history")
			}
			conversation, err := f.c.Conversation(ctx, survivor)
			must(t, err)
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
			if requiresHold && !strings.Contains(finished.Summary(), trace.ErrMemberRetired.Error()) {
				t.Fatal("impact hides its recoverable hold")
			}
			repo, err = f.s.repository(survivorProject)
			must(t, err)
			authority, _, _, closeAuthority, err := f.s.chiefRepository(ctx, repo, survivor)
			must(t, err)
			chief, err := authority.Thread(host, trace.ChiefOfStaff)
			closeAuthority()
			must(t, err)
			if !equalJSONTest(f.before.Identity, chief.Identity) || len(chief.Turns) < len(f.before.Turns) || !equalJSONTest(f.before.Turns, chief.Turns[:len(f.before.Turns)]) {
				t.Fatal("retirement changed original conversation or design turns")
			}
		})
	}
}

func TestArchiveSucceedsDuringRoutineGroupApprovalRetry(t *testing.T) {
	ctx := context.Background()
	gate := &changeRaceGate{step: "group-approval-retry-locked", reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(gate.release)
	cleanup := &lifecycleCleanupGate{reached: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(cleanup.release)
	f := newConfiguredProductionDesignFixture(t, productionDesignConfiguration{wait: lifecycleAwait, checkpoint: func(ctx context.Context, step string) error {
		if err := gate.checkpoint(ctx, step); err != nil {
			return err
		}
		return cleanup.checkpoint(ctx, step)
	}})
	defer func() { f.stop(t) }()
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan)
	must(t, err)
	f.awaitApproved(t)
	repo, err := f.s.repository(f.project)
	must(t, err)
	group, err := repo.CrossRepoWorkgroupOf(f.stream1)
	must(t, err)
	_, err = f.c.Abandon(ctx, f.stream1, "Owner retires this completed member.")
	must(t, err)
	gate.armed.Store(true)
	retry := make(chan error, 1)
	go func() {
		_, err := f.c.Ratify(ctx, f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan)
		retry <- err
	}()
	select {
	case <-gate.reached:
	case err := <-retry:
		t.Fatalf("approval retry returned before checkpoint: %v", err)
	case <-time.After(demoTimeout):
		t.Fatal("approval retry checkpoint not reached")
	}
	archived := make(chan error, 1)
	go func() { _, err := f.c.Archive(ctx, f.stream1); archived <- err }()
	lock := f.s.groupApprovalLock(group)
	if lock.TryLock() {
		lock.Unlock()
		t.Fatal("approval checkpoint did not retain group coordination")
	}
	select {
	case err := <-archived:
		must(t, err)
	case <-time.After(demoTimeout):
		t.Fatal("archive waited for routine group coordination")
	}
	gate.release()
	select {
	case err := <-retry:
		must(t, err)
	case <-time.After(demoTimeout):
		t.Fatal("approval retry did not finish")
	}
	cleanup.await(t)
}

func checkFollowupContinuation(t *testing.T, f *productionDesignFixture, project config.ProjectID, addition config.WorkstreamID, brief string) {
	t.Helper()
	ctx := context.Background()
	for i, m := range []config.WorkstreamID{f.stream1, f.stream2, addition} {
		p := f.project
		if i == 1 {
			p = f.project2
		}
		if i == 2 {
			p = project
		}
		repo, err := f.s.repository(p)
		must(t, err)
		streams, err := repo.Workstreams()
		must(t, err)
		features := slices.DeleteFunc(slices.Clone(streams), func(stream config.WorkstreamID) bool {
			return stream == librarianWorkstream(p) || stream == f.origin
		})
		expected := []config.WorkstreamID{f.stream1}
		if p == f.project2 {
			expected = []config.WorkstreamID{f.stream2}
		}
		if p == project {
			expected = append(expected, addition)
		}
		slices.Sort(features)
		slices.Sort(expected)
		if !slices.Equal(features, expected) {
			t.Fatalf("repo has lost or duplicated its allocated member: %v", features)
		}
		if f.s.Context().Mode(p) != bundle.ModeFile {
			t.Fatal("production path requires Hearsay")
		}
		handed, err := trace.Get[trace.Document](repo, m, handedDocument, 1)
		must(t, err)
		expectedBrief, source := f.handed.Content, f.handed.Source
		if m == addition {
			expectedBrief, source = brief, "stdin"
		}
		if handed.Content != expectedBrief || handed.Source != source {
			t.Fatal("member input differs from the immutable brief")
		}
		cfg := f.s.current().For(p)
		for _, name := range []string{"spec.md", "plan.json", "handed", "tools/design-brief.json", "workgroups", ".osmia"} {
			if _, err := os.Stat(filepath.Join(cfg.Project.Clone, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("factory state leaked into target repo at %s: %v", name, err)
			}
		}
		if _, err := repo.Thread(m, trace.ChiefOfStaff); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("member retains an independent chief: %v", err)
		}
		groups, _, err := trace.Workgroups(repo)
		must(t, err)
		if _, found := trace.WorkgroupOf(groups, m); found {
			t.Fatal("member retains a one-workstream group")
		}
		target, host, _, close, err := f.s.chiefRepository(ctx, repo, m)
		must(t, err)
		chief, err := target.Thread(host, trace.ChiefOfStaff)
		close()
		must(t, err)
		if len(chief.Turns) < len(f.before.Turns) {
			t.Fatal("design turns were lost")
		}
		if !equalJSONTest(f.before.Identity, chief.Identity) || !equalJSONTest(f.before.Turns, chief.Turns[:len(f.before.Turns)]) {
			t.Fatal("design identity, requests, responses or recovery state changed at adoption")
		}
	}
	repo, err := f.s.repository(f.project)
	must(t, err)
	id, err := repo.CrossRepoWorkgroupOf(f.origin)
	must(t, err)
	_, group, _, err := repo.CrossRepoProjectSpec(id)
	must(t, err)
	if group.Conversation == nil || group.Conversation.Workstream != f.origin || group.Conversation.Project != f.project ||
		len(group.Members) != 4 || group.Members[0] != f.origin ||
		!slices.Contains(group.Members[1:], f.stream1) || !slices.Contains(group.Members[1:], f.stream2) || !slices.Contains(group.Members[1:], addition) {
		t.Fatalf("continued conversation or exact member coverage changed: %+v", group)
	}
	groups, err := os.ReadDir(filepath.Join(f.s.current().Root.String(), "workgroups"))
	must(t, err)
	if len(groups) != 1 || groups[0].Name() != string(id) {
		t.Fatalf("start or recovery duplicated workgroup authority: %v", groups)
	}
	handed, err := trace.Get[trace.Document](repo, f.origin, handedDocument, 1)
	must(t, err)
	if !equalJSONTest(handed, f.handed) {
		t.Fatal("immutable handed brief changed")
	}
	alias, err := f.c.Status(ctx, f.origin)
	must(t, err)
	if alias.Workstream != f.stream1 || alias.Project != f.project {
		t.Fatalf("design status did not resolve to its member: %+v", alias)
	}
	listed, err := f.c.Statuses(ctx)
	must(t, err)
	if slices.ContainsFunc(listed.Workstreams, func(w WorkstreamStatus) bool { return w.Workstream == f.origin }) {
		t.Fatal("status list exposes the adopted design host as independent work")
	}
	if _, err := repo.Thread(f.origin, trace.ChiefOfStaff); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source retains an authoritative queue")
	}
	historical, err := trace.Read[trace.TurnRequest](repo, f.origin)
	must(t, err)
	if !slices.ContainsFunc(historical, func(req trace.TurnRequest) bool { return req.ID == f.before.Turns[0].Request.ID }) {
		t.Fatal("historical design request was lost")
	}
	chief, _, _, closeChief, err := f.s.chiefRepository(ctx, repo, f.stream1)
	must(t, err)
	defer closeChief()
	matching, err := consistentRead(func() (bool, error) {
		var entries []ConversationEntry
		var feed []FeedEntry
		for i, member := range []config.WorkstreamID{f.origin, f.stream1, f.stream2, addition} {
			conversation, err := f.c.Conversation(ctx, member)
			if err != nil {
				return false, err
			}
			got, api := f.s.feedList(string(member))
			if api != nil {
				return false, apiErrAsError{api}
			}
			shared := slices.DeleteFunc(slices.Clone(got.Entries), func(e FeedEntry) bool {
				return e.Kind != "message" && e.Kind != "response" && e.Kind != "action" && e.Kind != "status"
			})
			if i == 0 {
				entries, feed = conversation.Entries, shared
			} else if !equalJSONTest(entries, conversation.Entries) || !equalJSONTest(feed, shared) {
				return false, nil
			}
		}
		return true, nil
	}, chief.Generation)
	must(t, err)
	if !matching {
		t.Fatal("continued address and member endpoints resolve to different conversations or feeds")
	}
}
