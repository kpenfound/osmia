package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func workgroupFeedFixture(t *testing.T, cross bool, count int, pending ...bool) (*Service, *trace.Repository, trace.Workgroup) {
	t.Helper()
	opts, cfg := conversationFixture(t, "workgroup-feed-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	must(t, err)
	t.Cleanup(func() { repo.Close() })
	members := []trace.CrossRepoMember{{Workstream: stream, Project: repo}}
	if count > 1 {
		members = append(members, trace.CrossRepoMember{Workstream: fixturekit.Quiet, Project: repo})
	}
	if cross {
		_, err := repo.MarkWorkstreamTraceForDeletion(fixturekit.Quiet)
		must(t, err)
		must(t, trace.FinishWorkstreamTraceDeletion(cfg.Root, repo.Project(), fixturekit.Quiet))
		p := cfg.Project
		p.ID = "p_00000000000000000000000000000022"
		p.Clone = filepath.Join(t.TempDir(), "clone")
		demoGit(t, filepath.Dir(p.Clone), "init", "-q", p.Clone)
		r, err := trace.Create(context.Background(), cfg.Root, p, demoStart, ownerActor)
		must(t, err)
		t.Cleanup(func() { r.Close() })
		must(t, r.CreateWorkstream(context.Background(), fixturekit.Quiet, demoStart, ownerActor))
		cfg.Projects = append(cfg.Projects, p)
		cfg.ActiveProjects = append(cfg.ActiveProjects, string(p.ID))
		members[1].Project = r
	}
	var group trace.Workgroup
	chief := repo
	if cross {
		chief, group, err = trace.CreateWorkgroupTrace(context.Background(), cfg.Root, members, demoStart, ownerActor, "test")
		must(t, err)
		t.Cleanup(func() { chief.Close() })
		for _, m := range members {
			if len(pending) > 0 && pending[0] && m.Workstream == fixturekit.Quiet {
				continue
			}
			must(t, m.Project.JoinCrossRepoWorkgroup(context.Background(), m.Workstream, group.ID, demoStart, ownerActor, "test"))
		}
	} else {
		ids := []config.WorkstreamID{}
		for _, m := range members {
			ids = append(ids, m.Workstream)
		}
		group, err = trace.CreateWorkgroup(context.Background(), repo, ids, demoStart, ownerActor, "test")
		must(t, err)
	}
	streams := map[config.ProjectID][]config.WorkstreamID{}
	for _, m := range members {
		streams[m.Project.Project()] = append(streams[m.Project.Project()], m.Workstream)
	}
	store, _, err := runtime.Open(runtime.Inputs{Config: cfg, Workstreams: streams})
	must(t, err)
	t.Cleanup(func() { store.Close() })
	s := &Service{cfg: cfg, store: store, options: opts, projects: []*activeProject{{id: repo.Project(), repository: repo}}}
	if cross {
		s.projects = append(s.projects, &activeProject{id: members[1].Project.Project(), repository: members[1].Project})
	}
	for _, m := range members {
		s.installMemberLifecycle(m.Project)
	}
	_, err = chief.EnsureChiefOfStaff(context.Background(), group.Members[0], demoStart, serviceActor)
	must(t, err)
	return s, chief, group
}

func finishFeedChief(t *testing.T, chief *trace.Repository, host config.WorkstreamID) trace.QueuedTurn {
	t.Helper()
	ctx := context.Background()
	q, err := chief.ClaimTurn(ctx, host, trace.ChiefOfStaff, "chief-token", filepath.Join(t.TempDir(), "chief"), demoStart.Add(time.Minute))
	must(t, err)
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), demoStart.Add(2*time.Minute)
	must(t, chief.CaptureTurn(ctx, q.Claim.Token, trace.TurnResponse{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: "chief-session"}, StartedAt: q.Claim.At, SessionDirectory: q.Claim.SessionDirectory, FinalResponse: "Shared reply"}}))
	must(t, chief.CompleteTurn(ctx, host, trace.ChiefOfStaff, q.Request.TurnID, q.Claim.Token, h.At))
	return q
}

func feedHTTP(t *testing.T, s *Service, method, path, text string, output any) int {
	t.Helper()
	var body strings.Reader
	if text != "" {
		data, _ := json.Marshal(SendRequest{Text: text})
		body = *strings.NewReader(string(data))
	}
	req := httptest.NewRequest(method, path, &body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handle(w, req)
	if w.Code != 200 {
		t.Logf("HTTP %s %s: %s", method, path, w.Body.String())
	}
	if output != nil {
		must(t, json.Unmarshal(w.Body.Bytes(), output))
	}
	return w.Code
}

func TestWorkgroupFeedConversationAndMemberActivity(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(fmt.Sprint("cross=", cross), func(t *testing.T) {
			s, chief, g := workgroupFeedFixture(t, cross, 2)
			ctx := context.Background()
			var accepted ConversationEntry
			if code := feedHTTP(t, s, http.MethodPost, Prefix+"/workgroups/"+string(g.ID)+"/conversation", "Whole group", &accepted); code != 200 {
				t.Fatalf("send status %d: %+v", code, accepted)
			}
			th, err := chief.ChiefOfStaffThread(g.Members[0])
			must(t, err)
			if len(th.Turns) != 1 || strings.Contains(th.Turns[0].Request.SystemPrompt, "The owner addressed workstream") {
				t.Fatalf("durable group turn: %+v", th)
			}
			q := finishFeedChief(t, chief, g.Members[0])
			scope := coreadapter.Scope{Project: string(chief.Project()), Workstream: string(g.Members[0]), Thread: trace.ChiefOfStaff, Turn: q.Request.TurnID, Role: trace.ChiefOfStaff}
			// A status can be written only by an active turn.
			scope.Turn = "status-turn"
			_, err = chief.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request-status", Project: chief.Project(), Workstream: g.Members[0], At: demoStart.Add(3 * time.Minute), Actor: serviceActor, Cause: "test"}, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: scope.Turn, Profile: q.Request.Profile, Prompt: "status"})
			must(t, err)
			_, err = chief.ClaimTurn(ctx, g.Members[0], trace.ChiefOfStaff, "status-token", filepath.Join(t.TempDir(), "status"), demoStart.Add(3*time.Minute))
			must(t, err)
			_, err = chief.SetStatus(ctx, trace.ChiefOfStaff, scope, trace.StatusContent{Goal: "Shared goal", Note: "Working", Agents: []string{}}, demoStart.Add(4*time.Minute), func(trace.StatusContent, []string, []trace.OwnerGate) error { return nil })
			must(t, err)
			member := fixturekit.Quiet
			second, api := s.send(ctx, string(member), SendRequest{Text: "About this member"})
			if api != nil {
				t.Fatal(api)
			}
			th, err = chief.ChiefOfStaffThread(member)
			must(t, err)
			if !strings.Contains(th.Turns[len(th.Turns)-1].Request.SystemPrompt, "The owner addressed workstream "+string(member)) {
				t.Fatal("member note missing")
			}
			_, _, repo, api := s.conversationTrace(string(member))
			if api != nil {
				t.Fatal(api)
			}
			if repo.Project() == project {
				queueRoleTurn(t, repo, member, "worker", masonRole, "upload", demoStart)
			} else {
				must(t, repo.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: 1, Revision: 1, ID: "worker", Project: repo.Project(), Workstream: member, At: demoStart, Actor: ownerActor, Cause: "test"}, Role: masonRole, ThreadID: "worker"}))
				_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: 1, Revision: 1, ID: "request-worker", Project: repo.Project(), Workstream: member, Unit: "upload", At: demoStart, Actor: ownerActor, Cause: "test"}, AgentID: "worker", ThreadID: "worker", TurnID: "worker-turn", Profile: q.Request.Profile, Prompt: "work"})
				must(t, err)
			}
			_, err = repo.ClaimTurn(ctx, member, "worker", "worker-token", filepath.Join(t.TempDir(), "worker"), demoStart)
			must(t, err)
			question, err := repo.Ask(ctx, "worker", coreadapter.Scope{Project: string(repo.Project()), Workstream: string(member), Thread: "worker", Turn: "worker-turn", Unit: "upload", Role: masonRole}, "Which retry budget?", demoStart.Add(5*time.Minute))
			must(t, err)
			batch, err := chief.EscalateQuestions(ctx, trace.ChiefOfStaff, scope, trace.EscalationRequest{Questions: []string{string(member) + ":" + question.ID}, Rephrasing: "Which retry budget?", Blocked: "Upload retry", Recommendation: "Use three retries"}, demoStart.Add(6*time.Minute))
			must(t, err)
			fixturekit.FeedDocument(t, repo, member, checkDocumentID("upload", 1), "units/upload/checks-1.json", 1, demoStart, UnitCheckRun{Unit: "upload", Run: 1, Status: ChecksPassed, StartedAt: demoStart, FinishedAt: demoStart})
			fixturekit.FeedDocument(t, repo, member, "final-checks-1", "final/checks-1.json", 1, demoStart, FinalCheckRun{Review: 1, Attempt: 1, Status: ChecksPassed, StartedAt: demoStart, FinishedAt: demoStart})
			fixturekit.FeedDocument(t, repo, member, finalReportDocument, finalReportPath, 1, demoStart, FinalReport{Review: 1, Outcome: finalReviewed, Reader: "reader", Turn: "review"})
			data, _ := json.Marshal(ChiefContestDecision{Decision: "revise", Note: "Retry the check", Turn: "contest-turn"})
			must(t, repo.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: 1, Revision: 1, ID: "contest", Project: repo.Project(), Workstream: member, Unit: "upload", At: demoStart, Actor: chiefActor, Cause: "test"}, Path: "units/unit-upload/chief-decision.json", Content: string(data)}}))
			var feed WorkgroupFeedResponse
			if code := feedHTTP(t, s, http.MethodGet, Prefix+"/workgroups/"+string(g.ID)+"/feed", "", &feed); code != 200 {
				t.Fatalf("feed status %d: %+v", code, feed)
			}
			if len(feed.Members) != 2 {
				t.Fatalf("summaries: %+v", feed)
			}
			kinds := map[string]int{}
			for _, e := range feed.Entries {
				kinds[e.Kind]++
				if e.Turn == second.Turn || e.Turn == "contest-turn" || e.Turn == batch {
					if e.Workstream != member || e.Repo != repo.Project() {
						t.Fatalf("member identity: %+v", e)
					}
				}
				if e.Kind == "session" || e.Kind == "transition" || e.Kind == "unit_checks" {
					t.Fatalf("member detail in workgroup: %+v", e)
				}
			}
			if kinds["message"] != 2 || kinds["response"] != 1 || kinds["status"] != 1 || kinds["action"] != 2 {
				t.Fatalf("shared entries: %+v", feed.Entries)
			}
			for _, m := range feed.Members {
				if m.Workstream == member && !m.Running {
					t.Fatal("worker running indicator missing")
				}
			}
			for _, member := range []config.WorkstreamID{stream, fixturekit.Quiet} {
				activity, api := s.feedList(string(member))
				if api != nil {
					t.Fatal(api)
				}
				for _, e := range activity.Entries {
					if e.Kind == "message" || e.Kind == "response" || e.Kind == "status" || e.Kind == "action" || e.Session != nil && e.Session.Role == trace.ChiefOfStaff {
						t.Fatalf("shared entry in member feed: %+v", e)
					}
				}
				if member == fixturekit.Quiet {
					for _, kind := range []string{"transition", "unit_checks", "final_checks", "final_review"} {
						if !slices.ContainsFunc(activity.Entries, func(e FeedEntry) bool { return e.Kind == kind }) {
							t.Fatalf("member %s missing", kind)
						}
					}
				}
				if member == fixturekit.Quiet && !slices.ContainsFunc(activity.Entries, func(e FeedEntry) bool { return e.Session != nil && e.Session.Agent == "worker" }) {
					t.Fatal("worker session missing")
				}
			}
			before, api := s.conversationList(string(member))
			if api != nil {
				t.Fatal(api)
			}
			expected, err := conversationEntries(chief, member)
			must(t, err)
			if !reflect.DeepEqual(before.Entries, expected) {
				t.Fatal("member conversation changed")
			}
		})
	}
}

func TestMemberFeedChiefSessionsFollowMembership(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprint("completed=", completed), func(t *testing.T) {
			s, chief, g := workgroupFeedFixture(t, false, 1)
			_, api := s.workgroupSend(context.Background(), string(g.ID), SendRequest{Text: "Before joining"})
			if api != nil {
				t.Fatal(api)
			}
			if completed {
				finishFeedChief(t, chief, stream)
			} else {
				_, err := chief.ClaimTurn(context.Background(), stream, trace.ChiefOfStaff, "chief-token", filepath.Join(t.TempDir(), "chief"), demoStart.Add(time.Minute))
				must(t, err)
			}
			queueRoleTurn(t, chief, stream, "worker", masonRole, "upload", demoStart)
			_, err := chief.ClaimTurn(context.Background(), stream, "worker", "worker-token", filepath.Join(t.TempDir(), "worker"), demoStart)
			must(t, err)
			single, api := s.feedList(string(stream))
			if api != nil {
				t.Fatal(api)
			}
			for _, kind := range []string{"message", "session"} {
				if !slices.ContainsFunc(single.Entries, func(e FeedEntry) bool {
					return e.Kind == kind && (e.Session == nil || e.Session.Role == trace.ChiefOfStaff)
				}) {
					t.Fatalf("single-member %s missing", kind)
				}
			}
			must(t, chief.RetractWorkgroup(context.Background(), stream, demoStart, ownerActor, "draft-workgroup"))
			g, err = trace.CreateWorkgroup(context.Background(), chief, []config.WorkstreamID{stream, fixturekit.Quiet}, demoStart, ownerActor, "test")
			must(t, err)
			multi, api := s.feedList(string(stream))
			if api != nil {
				t.Fatal(api)
			}
			sessions := 0
			for _, e := range multi.Entries {
				if e.Kind == "message" || e.Session != nil && e.Session.Role == trace.ChiefOfStaff {
					t.Fatalf("chief leak: %+v", e)
				}
				if e.Session != nil {
					sessions++
				}
			}
			if sessions != 1 {
				t.Fatalf("worker sessions: %d", sessions)
			}
			shared, api := s.workgroupFeed(context.Background(), string(g.ID))
			if api != nil {
				t.Fatal(api)
			}
			if !slices.ContainsFunc(shared.Entries, func(e FeedEntry) bool { return e.Text == "Before joining" }) {
				t.Fatal("earlier conversation missing")
			}
		})
	}
}

func TestWorkgroupMemberArchiveSummaryUsesPinnedIdentity(t *testing.T) {
	s, chief, g := workgroupFeedFixture(t, false, 2)
	state := AbandonedState
	a := runtime.Archive{Workgroup: g.ID, Project: chief.Project(), Workstream: fixturekit.Quiet, State: state, Title: "Archived member", DeliveryLink: "https://example.invalid/pull/4", Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: demoStart, CleanupStartedAt: demoStart, CleanedAt: demoStart}
	must(t, s.store.SetArchived(a))
	dir, err := s.cfg.Root.WorkstreamTrace(chief.Project(), a.Workstream)
	must(t, err)
	must(t, os.RemoveAll(dir))
	summary, err := s.workgroupMemberSummary(trace.CrossRepoMember{Workstream: a.Workstream, Project: chief})
	must(t, err)
	if !summary.Archived || summary.PRLink != a.DeliveryLink || summary.DisplayState != "cancelled" || *summary.State != state || len(summary.Units) != 0 || summary.Running {
		t.Fatalf("archive summary: %+v", summary)
	}
}

func assertWorkgroupHandleReleased(t *testing.T, s *Service, g trace.Workgroup, chief *trace.Repository) {
	t.Helper()
	must(t, chief.Close())
	dir, err := s.cfg.Root.WorkgroupTrace(g.ID)
	must(t, err)
	lock, err := os.OpenFile(filepath.Join(dir, "osmia.lock"), os.O_RDWR, 0600)
	must(t, err)
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("workgroup handle leaked: %v", err)
	}
	must(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_UN))
}

func TestWorkgroupEndpointsHoldAuthorityAndCloseHandles(t *testing.T) {
	for _, scenario := range []string{"success", "read-error", "joining", "half-joined", "unpublished-membership", "retired", "locked"} {
		t.Run(scenario, func(t *testing.T) {
			s, chief, g := workgroupFeedFixture(t, true, 2, scenario == "joining")
			ctx := context.Background()
			if scenario == "unpublished-membership" {
				must(t, chief.RecordDocuments(ctx, []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: 1, Revision: 2, ID: trace.WorkgroupsDocument, Project: chief.Project(), At: demoStart, Actor: ownerActor, Cause: "test"}, Path: trace.WorkgroupsPath, Content: `{"version":1,"workgroups":[]}`}}))
			}
			if scenario == "half-joined" {
				_, err := trace.CreateWorkgroup(ctx, s.projects[1].repository, []config.WorkstreamID{fixturekit.Quiet}, demoStart, ownerActor, "test")
				must(t, err)
			}
			if scenario == "retired" {
				pins := []trace.GroupMemberApproval{}
				for i, member := range []config.WorkstreamID{stream, fixturekit.Quiet} {
					pins = append(pins, trace.GroupMemberApproval{Project: s.projects[i].id, Workstream: member})
				}
				must(t, trace.RecordGroupApproval(ctx, chief, g.Members[0], trace.GroupApproval{Workgroup: g.ID, Members: pins, Complete: true}, demoStart, ownerActor))
				for _, pin := range pins {
					must(t, s.store.SetArchived(runtime.Archive{Project: pin.Project, Workstream: pin.Workstream, Workgroup: g.ID, State: AbandonedState, Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: demoStart}))
				}
			}
			if scenario == "locked" {
				must(t, chief.Close())
				dir, err := s.cfg.Root.WorkgroupTrace(g.ID)
				must(t, err)
				lock, err := os.OpenFile(filepath.Join(dir, "osmia.lock"), os.O_RDWR, 0600)
				must(t, err)
				defer lock.Close()
				must(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
				defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			}
			if scenario == "read-error" {
				s.setBoundary(func(step string) error {
					if step == "status-units" {
						return errors.New("cannot read units")
					}
					return nil
				})
			}
			var feed WorkgroupFeedResponse
			code := feedHTTP(t, s, http.MethodGet, Prefix+"/workgroups/"+string(g.ID)+"/feed", "", &feed)
			var entry ConversationEntry
			send := feedHTTP(t, s, http.MethodPost, Prefix+"/workgroups/"+string(g.ID)+"/conversation", "Whole group", &entry)
			switch scenario {
			case "success":
				if code != 200 || send != 200 {
					t.Fatalf("success: feed=%d send=%d", code, send)
				}
			case "read-error":
				if code != 500 || send != 200 {
					t.Fatalf("read failure: feed=%d send=%d", code, send)
				}
			default:
				if code != 409 || send != 409 {
					t.Fatalf("authority hold: feed=%d send=%d", code, send)
				}
			}
			if scenario != "locked" {
				assertWorkgroupHandleReleased(t, s, g, chief)
			}
		})
	}
}

func TestWorkgroupFeedKeepsCleanedMemberCard(t *testing.T) {
	s, chief, g := workgroupFeedFixture(t, true, 2)
	ctx := context.Background()
	pins := []trace.GroupMemberApproval{{Project: s.projects[0].id, Workstream: stream}, {Project: s.projects[1].id, Workstream: fixturekit.Quiet}}
	must(t, trace.RecordGroupApproval(ctx, chief, g.Members[0], trace.GroupApproval{Workgroup: g.ID, Members: pins, Complete: true}, demoStart, ownerActor))
	repo := s.projects[1].repository
	must(t, chief.PreserveMemberQuestions(ctx, g, trace.CrossRepoMember{Workstream: fixturekit.Quiet, Project: repo}, demoStart, serviceActor))
	a := runtime.Archive{Workgroup: g.ID, Project: repo.Project(), Workstream: fixturekit.Quiet, State: AbandonedState, Title: "Archived member", DeliveryLink: "https://example.invalid/pull/4", Resolved: true, ResolutionEvidence: "cancelled", ArchivedAt: demoStart, CleanupStartedAt: demoStart, CleanedAt: demoStart}
	must(t, s.store.SetArchived(a))
	_, err := repo.MarkWorkstreamTraceForDeletion(a.Workstream)
	must(t, err)
	must(t, trace.FinishWorkstreamTraceDeletion(s.cfg.Root, a.Project, a.Workstream))
	feed, api := s.workgroupFeed(ctx, string(g.ID))
	if api != nil {
		t.Fatal(api)
	}
	if len(feed.Members) != 2 {
		t.Fatalf("cards: %+v", feed)
	}
	for _, m := range feed.Members {
		if m.Workstream == a.Workstream && (!m.Archived || m.PRLink != a.DeliveryLink || m.DisplayState != "cancelled") {
			t.Fatalf("pinned card: %+v", m)
		}
	}
	dir, err := s.cfg.Root.WorkstreamTrace(a.Project, a.Workstream)
	must(t, err)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("deleted history was recreated: %v", err)
	}
	assertWorkgroupHandleReleased(t, s, g, chief)
}

func TestWorkgroupFeedSummaryProjectsMemberFacts(t *testing.T) {
	s, repo, g := workgroupFeedFixture(t, false, 2)
	p := overlapPlanted{t: t, clock: &fixedClock{At: demoStart}}
	p.plant(repo, stream, 1)
	p.feature(repo, stream, BuildingState)
	ctx := context.Background()
	for _, transition := range []struct{ subject, to, reason string }{{trace.UnitSubject("work"), UnitContested, "Review disagreed"}, {"base-wait", "waiting", "waiting on owner: dependency merge"}} {
		_, err := repo.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: 1, Revision: 1, ID: transition.subject + "-state", Project: repo.Project(), Workstream: stream, At: demoStart, Actor: serviceActor, Cause: "test"}, Subject: transition.subject, To: transition.to, Reason: transition.reason}})
		must(t, err)
	}
	fixturekit.FeedDocument(t, repo, stream, publicationDocument, "final/publication.json", 1, demoStart, DeliveryPublication{Status: publicationOpened, PullRequest: 42, URL: "https://example.invalid/pull/42"})
	feed, api := s.workgroupFeed(ctx, string(g.ID))
	if api != nil {
		t.Fatal(api)
	}
	m := feed.Members[0]
	if m.Workstream != stream || m.Repo != repo.Project() || m.State == nil || *m.State != BuildingState || m.Units[UnitContested] != 1 || m.Wait != "waiting on owner: dependency merge" || m.PRLink != "https://example.invalid/pull/42" || m.Archived || m.Running {
		t.Fatalf("member facts: %+v", m)
	}
}
