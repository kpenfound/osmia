package intakejourney

import (
	"context"
	"fmt"
	agent "github.com/kpenfound/agentcore/agent"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/shed"
	"github.com/kpenfound/osmia/internal/trace"
)

func awaitNumberedAmendment(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, id, want string) {
	t.Helper()
	deadline := time.Now().Add(intakefixture.DemoTimeout)
	for {
		state, err := repo.Workflow(stream, intake.AmendmentSubject(id))
		must(t, err)
		if state.Value == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("amendment %s is %s, want %s", id, state.Value, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func checkRetirement(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, id, requester, unit string, sealRevision int) {
	t.Helper()
	decisions, err := intake.AmendmentDecisions(repo, stream, id)
	must(t, err)
	if len(decisions) != 1 || decisions[0].Actor != intake.AmendmentActor.ID || decisions[0].Decision != intake.AmendmentReject || decisions[0].SealRevision != sealRevision {
		t.Fatalf("decisions %+v", decisions)
	}
	d := decisions[0]
	for _, part := range []string{"the service retired amendment " + id, "sealed spec or plan changed", fmt.Sprintf("seal.json revision %d", sealRevision), "File a new request against the current seal if the change is still needed"} {
		if !strings.Contains(d.Note, part) {
			t.Errorf("retirement misses %q: %s", part, d.Note)
		}
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	n := 0
	for _, doc := range docs {
		if doc.Path == intake.AmendmentDecisionPath(id) {
			n++
			if doc.Actor != intake.AmendmentActor {
				t.Errorf("decision actor %+v", doc.Actor)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d decision revisions", n)
	}
	transitions, err := trace.Read[trace.Transition](repo, stream)
	must(t, err)
	for _, tr := range transitions {
		if tr.ID == intake.AmendmentDecided(id, 1) || tr.ID == trace.UnitSubject(unit)+"_resumed_amendment_"+id {
			if tr.Actor != intake.AmendmentActor || strings.Contains(tr.Reason, "the owner") || strings.Contains(tr.Reason, "the chief of staff") {
				t.Errorf("retirement attributed incorrectly: %+v", tr)
			}
		}
	}
	th, err := repo.Thread(stream, requester)
	must(t, err)
	rulings := 0
	for _, q := range th.Turns {
		if q.Request.TurnID != intake.AmendmentRulingTurn(id) {
			continue
		}
		rulings++
		for _, part := range []string{"Decision: reject", "Actor: amendments", "File a new request", "osmia:service_response | service"} {
			if !strings.Contains(q.Request.Prompt, part) {
				t.Errorf("ruling misses %q: %s", part, q.Request.Prompt)
			}
		}
		for _, part := range []string{"owner_response", "the owner rejected", "the chief of staff rejected"} {
			if strings.Contains(q.Request.Prompt, part) {
				t.Errorf("ruling misattributes retirement: %s", q.Request.Prompt)
			}
		}
	}
	if rulings != 1 {
		t.Errorf("%d ruling turns", rulings)
	}
	stage := intake.UnitImplementing
	if th.Identity.Role == intake.ReviewerRole {
		stage = intake.UnitReviewing
	}
	if st, err := repo.Workflow(stream, trace.UnitSubject(unit)); err != nil || st.Value != stage {
		t.Errorf("unit did not resume: %+v %v", st, err)
	}
	outbox, err := repo.Outbox(stream)
	must(t, err)
	notices := 0
	for _, e := range outbox {
		if e.TransitionID == intake.AmendmentDecided(id, 1) {
			notices++
			if !strings.Contains(e.Event.Body, d.Note) {
				t.Errorf("notice lost reason: %+v", e)
			}
		}
	}
	if notices != 1 {
		t.Errorf("%d retirement notices", notices)
	}
	feed, api := f.s.feedList(string(stream))
	if api != nil {
		t.Fatalf("feed: %+v", api)
	}
	feedDecisions := 0
	for _, entry := range feed.Entries {
		if tr := entry.Transition; tr != nil && tr.Subject == intake.AmendmentSubject(id) && tr.To == intake.AmendmentRejected {
			feedDecisions++
			if tr.Actor != "service:amendments" || tr.Reason != d.Note {
				t.Errorf("feed attribution %+v", tr)
			}
		}
	}
	if feedDecisions != 1 {
		t.Errorf("%d feed decisions", feedDecisions)
	}
}

func TestAmendmentApprovalRetiresOtherPresentedAmendments(t *testing.T) {
	t.Parallel()
	for _, approved := range []string{"spec", "plan"} {
		for _, retired := range []string{"spec", "plan"} {
			t.Run(approved+" retires "+retired, func(t *testing.T) {
				f, stream := builtForAmendment(t, 2)
				defer f.stop(t)
				spec, graph := amendedSpec, validPlan
				if approved == "plan" {
					spec, graph = validSpec, amendedPlan
				}
				presentAmendment(t, f, stream, masonRole, spec, graph)
				spec, graph = amendedSpec, validPlan
				if retired == "plan" {
					spec, graph = validSpec, amendedPlan
				}
				requester := intakefixture.PresentAmendmentForUnit(t, f.repository(), f.project, f.clock.Now(), stream, "2", "dedupe", masonRole, spec, graph)
				_, err := f.c.DecideAmendment(context.Background(), stream, "1", AmendmentDecisionRequest{Decision: AmendmentApprove, Packet: 1})
				must(t, err)
				awaitNumberedAmendment(t, f.repository(), stream, "2", intake.AmendmentRuled)
				checkRetirement(t, f, f.repository(), stream, "2", requester, "dedupe", 2)
				if entries := f.inboxEntries(t, stream, InboxAmendment); len(entries) != 0 {
					t.Fatalf("stale cards %+v", entries)
				}
				for id, want := range map[string]int{plan.SpecDocument: 1, plan.PlanDocument: 1, seal.DocumentID: 2} {
					if approved == "spec" && id == plan.SpecDocument || approved == "plan" && id == plan.PlanDocument {
						want = 2
					}
					if got := f.revisions(t, stream, id); len(got) != want {
						t.Errorf("%s revisions %v, want %d", id, got, want)
					}
				}
				f.stop(t)
				f.start(t)
				checkRetirement(t, f, f.repository(), stream, "2", requester, "dedupe", 2)
			})
		}
	}
}

func TestStaleAmendmentHiddenBeforeRetirementAndRetiredOnStartup(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"spec", "plan"} {
		t.Run(change, func(t *testing.T) {
			f, stream := builtForAmendment(t, 2)
			f.stop(t)
			repo, err := trace.Open(f.s.current().Root, f.s.current().Project)
			must(t, err)
			f.s.setSole(&activeProject{repository: repo})
			requester := intakefixture.PresentAmendment(t, repo, f.project, f.clock.Now(), stream, masonRole, amendedSpec, validPlan)
			recordAmendmentSealRevision(t, f, repo, stream, func(s *seal.Seal) {
				if change == "spec" {
					s.Revision.Spec++
					s.SpecHash = seal.SpecHash(amendedSpec)
				} else {
					s.Revision.Plan++
				}
			})
			c := f.s.intake()
			if _, open, err := c.AmendmentEntry(repo, stream, "1"); err != nil || open {
				t.Fatalf("stale card: %t %v", open, err)
			}
			for _, decision := range []string{intake.AmendmentApprove, intake.AmendmentOverrule} {
				_, api := c.RecordAmendmentDecision(context.Background(), f.project, stream, repo, "1", AmendmentDecisionRequest{Decision: decision, Packet: 1}, intake.OwnerActor, "owner-amendment")
				if api == nil || !strings.Contains(api.Message, "sealed spec or plan changed") || strings.Contains(api.Message, "reject it") {
					t.Fatalf("stale %s refusal: %+v", decision, api)
				}
			}
			must(t, repo.Close())
			f.start(t)
			defer f.stop(t)
			awaitNumberedAmendment(t, f.repository(), stream, "1", intake.AmendmentRuled)
			checkRetirement(t, f, f.repository(), stream, "1", requester, "resume", 2)
			for id, want := range map[string]int{plan.SpecDocument: 1, plan.PlanDocument: 1, seal.DocumentID: 2} {
				if change == "spec" && id == plan.SpecDocument || change == "plan" && id == plan.PlanDocument {
					want = 2
				}
				if got := f.revisions(t, stream, id); len(got) != want {
					t.Errorf("%s revisions %v want %d", id, got, want)
				}
			}

			for _, decision := range []string{intake.AmendmentApprove, intake.AmendmentOverrule} {
				if _, err := f.c.DecideAmendment(context.Background(), stream, "1", AmendmentDecisionRequest{Decision: decision, Packet: 1}); !failed(err, Conflict) {
					t.Fatalf("retired %s: %v", decision, err)
				}
			}
		})
	}
}

func TestAmendmentRetirementRulingRecoversAtEveryBoundary(t *testing.T) {
	t.Parallel()
	for _, boundary := range []string{"decision", "ruling queued", "unit resumed"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			f, stream := builtForAmendment(t, 1)
			f.stop(t)
			var repo *trace.Repository
			reopen := func() {
				if repo != nil {
					must(t, repo.Close())
				}
				var err error
				repo, err = trace.Open(f.s.current().Root, f.s.current().Project)
				must(t, err)
				f.s.setSole(&activeProject{repository: repo})
			}
			reopen()
			defer func() { repo.Close() }()
			requester := intakefixture.PresentAmendment(t, repo, f.project, f.clock.Now(), stream, masonRole, amendedSpec, validPlan)
			recordAmendmentSealRevision(t, f, repo, stream, func(s *seal.Seal) { s.Revision.Plan++ })
			c := f.s.intake()
			handled, err := c.RetireStaleAmendment(ctx, repo, stream, "1")
			must(t, err)
			if !handled {
				t.Fatal("not retired")
			}
			a := intake.AmendmentDebate{Debate: &intake.Debate{S: c.DebateDependencies(), Repository: repo}}
			ac, _, err := intake.ReadAmendment(repo, stream, "1")
			must(t, err)
			if boundary != "decision" {
				must(t, a.DeliverRuling(ctx, stream, ac.Request, ac.Decisions[0], intake.AmendmentDecided("1", 1), ac.Decisions[0].Note))
			}
			if boundary == "unit resumed" {
				trs, err := trace.Read[trace.Transition](repo, stream)
				must(t, err)
				must(t, a.ResumeUnit(ctx, stream, ac.Request, intake.AmendmentDecided("1", 1), "rejected", intake.AmendmentActor.ID, trs))
			}
			for range 2 {
				reopen()
				a = intake.AmendmentDebate{Debate: &intake.Debate{S: c.DebateDependencies(), Repository: repo}}
				must(t, a.Pass(ctx))
				must(t, a.Pass(ctx))
				checkRetirement(t, f, repo, stream, "1", requester, "resume", 2)
			}
		})
	}
}

func TestOwnerDecisionRacesAmendmentRetirement(t *testing.T) {
	t.Parallel()
	for attempt := range 8 {
		t.Run(fmt.Sprint(attempt), func(t *testing.T) {
			f, stream := builtForAmendment(t, 1)
			f.stop(t)
			repo, err := trace.Open(f.s.current().Root, f.s.current().Project)
			must(t, err)
			defer repo.Close()
			f.s.setSole(&activeProject{repository: repo})
			intakefixture.PresentAmendment(t, repo, f.project, f.clock.Now(), stream, masonRole, amendedSpec, validPlan)
			recordAmendmentSealRevision(t, f, repo, stream, func(s *seal.Seal) { s.Revision.Plan++ })
			c := f.s.intake()
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			var api *intake.APIError
			var ownerResponse intake.AmendmentResponse
			var retireErr error
			go func() {
				defer wg.Done()
				<-start
				ownerResponse, api = c.RecordAmendmentDecision(context.Background(), f.project, stream, repo, "1", AmendmentDecisionRequest{Decision: intake.AmendmentReject, Packet: 1, Note: "Keep it"}, intake.OwnerActor, "owner-amendment")
			}()
			go func() {
				defer wg.Done()
				<-start
				_, retireErr = c.RetireStaleAmendment(context.Background(), repo, stream, "1")
			}()
			close(start)
			wg.Wait()
			must(t, retireErr)
			decisions, err := intake.AmendmentDecisions(repo, stream, "1")
			must(t, err)
			if len(decisions) != 1 {
				t.Fatalf("race recorded %d decisions", len(decisions))
			}
			if decisions[0].Actor == intake.AmendmentActor.ID && api == nil {
				// A retry of the same rejection can return the winning service record.
				if ownerResponse.Decision == nil || ownerResponse.Decision.Actor != intake.AmendmentActor.ID || !strings.Contains(ownerResponse.Detail, "already recorded") {
					t.Fatalf("owner claimed the service's decision: %+v", ownerResponse)
				}
			} else if decisions[0].Actor == intake.OwnerActor.ID && api != nil {
				t.Fatalf("owner won but returned %+v", api)
			}
			outbox, err := repo.Outbox(stream)
			must(t, err)
			n := 0
			for _, e := range outbox {
				if e.TransitionID == intake.AmendmentDecided("1", 1) {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("race recorded %d notices", n)
			}
			a := intake.AmendmentDebate{Debate: &intake.Debate{S: c.DebateDependencies(), Repository: repo}}
			must(t, a.Pass(context.Background()))
			must(t, a.Pass(context.Background()))
			if got := f.revisions(t, stream, intake.AmendmentDecisionID("1")); !slices.Equal(got, []int{1}) {
				t.Fatalf("race revisions %v", got)
			}
		})
	}
}

func TestAmendmentCardsRecommendTheirOwnDecisions(t *testing.T) {
	t.Parallel()
	for _, kind := range []shed.Kind{"", shed.Fit, shed.Charter} {
		t.Run(string(kind), func(t *testing.T) {
			f, stream := builtForAmendment(t, 2)
			f.stop(t)
			repo, err := trace.Open(f.s.current().Root, f.s.current().Project)
			must(t, err)
			defer repo.Close()
			var records []shed.Record
			if kind != "" {
				records = append(records, amendmentObjection(kind))
			}
			intakefixture.PresentAmendment(t, repo, f.project, f.clock.Now(), stream, masonRole, amendedSpec, validPlan, records...)
			docs, err := trace.Read[trace.Document](repo, stream)
			must(t, err)
			for _, doc := range docs {
				if doc.Path == intake.AmendmentPacketPath("1") {
					doc.Revision++
					doc.Content = `{"recommendation":"ratify: no objection stands"}`
					must(t, repo.RecordDocuments(context.Background(), []trace.Document{doc}))
				}
			}
			card, open, err := f.s.intake().AmendmentEntry(repo, stream, "1")
			must(t, err)
			if !open || strings.Contains(card.Recommendation, "ratify") {
				t.Fatalf("card %+v open %t", card, open)
			}
			want := "approve:"
			if kind == shed.Charter {
				want = "do not approve yet:"
				for _, step := range []string{"overrule", "reject", "another debate round"} {
					if !strings.Contains(card.Recommendation, step) {
						t.Errorf("missing step %s: %s", step, card.Recommendation)
					}
				}
			}
			if !strings.HasPrefix(card.Recommendation, want) {
				t.Fatalf("recommendation %q", card.Recommendation)
			}
			if got := shed.Recommend(nil); got != "ratify: no objection stands" {
				t.Fatalf("ratification recommendation %q", got)
			}
		})
	}
}

func fileCurrentAmendment(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, id string, filingRevision int) {
	t.Helper()
	current, doc, found, err := seal.Latest(repo, stream)
	must(t, err)
	if !found {
		t.Fatal("missing seal")
	}
	if filingRevision != 0 {
		doc.Revision = filingRevision
	}
	h := trace.Header{Schema: "osmia.trace.amendment", Version: trace.Version, ID: id, Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: trace.Actor{Kind: "agent", ID: "requester"}, Cause: "fixture"}
	request := trace.Amendment{Header: h, Requester: h.Actor, Role: masonRole, Thread: "requester", Turn: "filing_" + id, Citations: []string{"spec#1"}, Change: "Revise checkpoint semantics", Reason: "Durable storage", Seal: current.Seal, SealRevision: doc.Revision, SpecHash: current.SpecHash}
	must(t, repo.Append(context.Background(), request))
	h.Schema, h.ID = "osmia.trace.transition", "amendment_"+id+"_filed"
	_, err = repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: intake.AmendmentSubject(id), To: "filed", Reason: "filed"}})
	must(t, err)
}

func TestRetiredAmendmentOperationsSettleAndLaterAmendmentProgresses(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"filed", "draft queued", "draft running", "draft interrupted", "draft captured", "draft enqueue admission", "draft claim admission", "debate queued", "debate running", "reply queued", "reply running"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			ask := newAmendmentAskFixture(t, func(*shedFixture) {})
			f, stream, repo := ask.shedFixture, ask.stream, ask.repo
			f.s.setSole(&activeProject{repository: repo})
			recordDraftFixtureSeal(t, f, repo, stream)
			var err error
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			hold := func(ctx context.Context, _ agent.Request, _ *agent.Turn, _ *mcp.ClientSession) error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			draftCheck := (func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error)(nil)
			if stage == "draft running" {
				draftCheck = hold
			}
			f.script("amend-1-1", map[string]string{plan.SpecPath: amendedSpec}, draftCheck)
			f.script("amend-1-2", map[string]string{plan.SpecPath: amendedSpec}, nil)
			member := committeeAgent(1)
			debateCheck := (func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error)(nil)
			if stage == "debate running" {
				debateCheck = hold
			}
			f.script("amend-1-round-1-"+member+"-1", nil, debateCheck)
			replyCheck := (func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error)(nil)
			if stage == "reply running" {
				replyCheck = hold
			}
			f.script("amend-1-reply-1", nil, replyCheck)
			f.script("amend-2-1", map[string]string{plan.SpecPath: amendedSpec}, nil)
			f.script("amend-2-round-1-"+member+"-1", nil, nil)
			f.script("amend-2-reply-1", nil, nil)
			c := f.s.intake()
			draft := &intake.AmendmentDrafter{Drafter: &intake.Drafter{S: c.DrafterDependencies(), Repository: repo}}
			debate := intake.AmendmentDebate{Debate: &intake.Debate{S: c.DebateDependencies(), Repository: repo}}
			admission := strings.HasSuffix(stage, "admission")
			if admission {
				// Held runs after Apply's outcome check and its queue snapshot,
				// immediately before admitting an enqueue or dispatching a turn.
				held := draft.S.Held
				draft.S.Held = func(repository *trace.Repository, stream config.WorkstreamID) error {
					close(entered)
					<-release
					return held(repository, stream)
				}
			}
			var op coreadapter.Operation
			var driver interface {
				Apply(context.Context, coreadapter.Operation) (coreadapter.OperationResult, error)
				Inspect(context.Context, coreadapter.Operation) (coreadapter.Observation, error)
			}
			if stage != "filed" {
				must(t, draft.Pass(ctx))
				op = intakefixture.AmendmentOp(t, repo, stream, intake.AmendmentDraftAction)
				driver = draft
			}
			if strings.HasPrefix(stage, "debate") || strings.HasPrefix(stage, "reply") {
				_, err = draft.Apply(ctx, op)
				must(t, err)
				must(t, debate.Pass(ctx))
				op = intakefixture.AmendmentOp(t, repo, stream, intake.AmendmentRoundAction)
				driver = debate
			}
			if strings.HasPrefix(stage, "reply") {
				_, err = debate.Apply(ctx, op)
				must(t, err)
				must(t, debate.Pass(ctx))
				op = intakefixture.AmendmentOp(t, repo, stream, intake.AmendmentReplyAction)
			}
			if stage == "draft queued" || stage == "draft interrupted" || stage == "draft captured" || admission {
				must(t, draft.Enqueue(ctx, f.s.current(), stream, "1", 1, op.ID, nil))
				if stage == "draft interrupted" || stage == "draft captured" || admission {
					q, err := repo.ClaimTurn(ctx, stream, intake.ArchitectAgent, "old-claim", filepath.Join(t.TempDir(), "session"), f.clock.Now())
					must(t, err)
					if stage == "draft captured" || admission {
						h := q.Request.Header
						h.Schema, h.ID, h.Actor, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), intake.ArchitectActor, f.clock.Now()
						response := trace.TurnResponse{Header: h, AgentID: q.Request.AgentID, ThreadID: q.Request.ThreadID, TurnID: q.Request.TurnID, RequestID: q.Request.ID, RequestRevision: q.Request.Revision, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: q.Request.Profile.Backend, ID: "captured"}, SessionDirectory: q.Claim.SessionDirectory, StartedAt: q.Claim.At, FinalResponse: "Draft captured"}}
						// Admission cases resume after an interrupted first attempt,
						// so the paused operation would start attempt two.
						response.Result.Cancelled = admission
						must(t, repo.CaptureTurn(ctx, q.Claim.Token, response))
						if admission {
							must(t, repo.CompleteTurn(ctx, stream, intake.ArchitectAgent, q.Request.TurnID, q.Claim.Token, f.clock.Now()))
						}
					}
				}
			}
			if stage == "draft claim admission" {
				must(t, draft.Enqueue(ctx, f.s.current(), stream, "1", 2, op.ID, nil))
			}
			beforeAdmission, err := repo.Thread(stream, intake.ArchitectAgent)
			must(t, err)
			beforeRuns := slices.Clone(f.runs())
			running := strings.HasSuffix(stage, "running") || admission
			results := make(chan error, 1)
			if running {
				go func() {
					result, err := driver.Apply(ctx, op)
					if err == nil && result.Outcome != "succeeded" {
						err = fmt.Errorf("operation result %+v", result)
					}
					results <- err
				}()
				select {
				case <-entered:
				case err := <-results:
					t.Fatalf("operation ended before the turn ran: %v", err)
				case <-time.After(intakefixture.DemoTimeout):
					t.Fatal("turn did not enter")
				}
			}
			recordAmendmentSealRevision(t, f, repo, stream, func(s *seal.Seal) { s.Revision.Plan++ })
			// Either controller retires before starting the next attempt or round.
			if stage == "filed" {
				must(t, draft.Pass(ctx))
			} else {
				must(t, debate.Pass(ctx))
			}
			if st, err := repo.Workflow(stream, intake.AmendmentSubject("1")); err != nil || st.Value != intake.AmendmentRejected {
				t.Fatalf("not retired: %+v %v", st, err)
			}
			if running {
				releaseOnce.Do(func() { close(release) })
				select {
				case err := <-results:
					must(t, err)
				case <-time.After(intakefixture.DemoTimeout):
					t.Fatal("retired operation did not settle")
				}
			}
			if admission {
				after, err := repo.Thread(stream, intake.ArchitectAgent)
				must(t, err)
				if !reflect.DeepEqual(beforeAdmission, after) || !slices.Equal(beforeRuns, f.runs()) {
					t.Fatalf("retired draft admitted more work: before %+v after %+v; runs %v -> %v", beforeAdmission, after, beforeRuns, f.runs())
				}
				observed, err := driver.Inspect(ctx, op)
				must(t, err)
				if observed.State != coreadapter.EffectCompleted {
					t.Fatalf("retired operation did not settle: %+v", observed)
				}
			}
			retired, _, err := intake.ReadAmendment(repo, stream, "1")
			must(t, err)
			if len(retired.Decisions) != 1 {
				t.Fatalf("retired decisions %+v", retired.Decisions)
			}
			if stage == "filed" || strings.HasPrefix(stage, "draft") {
				if retired.Decisions[0].Packet != 0 {
					t.Fatalf("unpresented retirement names packet %d", retired.Decisions[0].Packet)
				}
				for _, decision := range []string{intake.AmendmentApprove, intake.AmendmentOverrule} {
					if _, api := c.RecordAmendmentDecision(ctx, f.project, stream, repo, "1", AmendmentDecisionRequest{Decision: decision, Packet: 1}, intake.OwnerActor, "owner-amendment"); api == nil {
						t.Fatalf("retired draft accepted %s", decision)
					}
				}
			}
			before := slices.Clone(f.runs())
			must(t, repo.Close())
			repo, err = trace.Open(f.s.current().Root, f.s.current().Project)
			must(t, err)
			ask.repo = repo
			f.s.setSole(&activeProject{repository: repo})
			draft = &intake.AmendmentDrafter{Drafter: &intake.Drafter{S: c.DrafterDependencies(), Repository: repo}}
			debate = intake.AmendmentDebate{Debate: &intake.Debate{S: c.DebateDependencies(), Repository: repo}}
			if stage != "filed" {
				driver = draft
				if op.Action != intake.AmendmentDraftAction {
					driver = debate
				}
				observed, err := driver.Inspect(ctx, op)
				must(t, err)
				if observed.State != coreadapter.EffectCompleted {
					t.Fatalf("inspection %+v", observed)
				}
				_, err = driver.Apply(ctx, op)
				must(t, err)
			}
			must(t, debate.Pass(ctx))
			must(t, draft.Pass(ctx))
			must(t, debate.Pass(ctx))
			if !slices.Equal(before, f.runs()) {
				t.Fatalf("retired operation ran again: before %v after %v", before, f.runs())
			}
			// Reuse the same architect and committee threads on the new seal.
			fileCurrentAmendment(t, f, repo, stream, "2", 0)
			must(t, draft.Pass(ctx))
			ops, err := repo.Operations(stream)
			must(t, err)
			for _, record := range ops {
				if record.Operation.Action == intake.AmendmentDraftAction && strings.Contains(string(record.Operation.Input), `"2"`) {
					_, err = draft.Apply(ctx, record.Operation)
					must(t, err)
				}
			}
			must(t, debate.Pass(ctx))
			ops, err = repo.Operations(stream)
			must(t, err)
			for _, record := range ops {
				if record.Operation.Action == intake.AmendmentRoundAction && strings.Contains(string(record.Operation.Input), `"2"`) {
					_, err = debate.Apply(ctx, record.Operation)
					must(t, err)
				}
			}
			must(t, debate.Pass(ctx))
			ops, err = repo.Operations(stream)
			must(t, err)
			for _, record := range ops {
				if record.Operation.Action == intake.AmendmentReplyAction && strings.Contains(string(record.Operation.Input), `"2"`) {
					_, err = debate.Apply(ctx, record.Operation)
					must(t, err)
				}
			}
			must(t, debate.Pass(ctx))
			decision, api := c.RecordAmendmentDecision(ctx, f.project, stream, repo, "2", AmendmentDecisionRequest{Decision: intake.AmendmentReject, Packet: 1}, intake.OwnerActor, "owner-amendment")
			if api != nil || decision.State != intake.AmendmentRejected {
				t.Fatalf("later decision %+v %+v; runs %v", decision, api, f.runs())
			}
			for _, run := range f.runs() {
				if strings.HasPrefix(run, "amend-1-") && !slices.Contains(before, run) {
					t.Errorf("extra retired turn %s", run)
				}
			}
		})
	}
}

func TestRetirementLeavesDraftQuestionsOnTheirNormalPath(t *testing.T) {
	t.Parallel()
	for _, answerTime := range []string{"before retirement", "after retirement"} {
		t.Run(answerTime, func(t *testing.T) {
			ctx := context.Background()
			ask := newAmendmentAskFixture(t, func(*shedFixture) {})
			f, stream, repo := ask.shedFixture, ask.stream, ask.repo
			f.s.setSole(&activeProject{repository: repo})
			recordDraftFixtureSeal(t, f, repo, stream)
			p := &faults{}
			f.script("amend-1-1", nil, parks(p))
			answer := f.answer("1", func(context.Context, agent.Request, *agent.Turn, *mcp.ClientSession) error {
				p.Report("retired draft's answer ran")
				return nil
			})
			f.script("amend-2-1", map[string]string{plan.SpecPath: amendedSpec}, nil)
			c := f.s.intake()
			draft := &intake.AmendmentDrafter{Drafter: &intake.Drafter{S: c.DrafterDependencies(), Repository: repo}}
			debate := intake.AmendmentDebate{Debate: &intake.Debate{S: c.DebateDependencies(), Repository: repo}}
			must(t, draft.Pass(ctx))
			op := intakefixture.AmendmentOp(t, repo, stream, intake.AmendmentDraftAction)
			result, err := draft.Apply(ctx, op)
			must(t, err)
			if result.Outcome != questions.Waiting {
				t.Fatalf("draft %+v", result)
			}
			if answerTime == "before retirement" {
				chiefAnswers(t, f, repo, stream, "1", askedAnswer, "charter#1")
			}
			before, err := repo.Questions(stream)
			must(t, err)
			recordAmendmentSealRevision(t, f, repo, stream, func(s *seal.Seal) { s.Revision.Plan++ })
			must(t, debate.Pass(ctx))
			must(t, debate.Pass(ctx))
			after, err := repo.Questions(stream)
			must(t, err)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("retirement changed the question: before %+v after %+v", before, after)
			}
			if answerTime == "after retirement" {
				chiefAnswers(t, f, repo, stream, "1", askedAnswer, "charter#1")
			}
			must(t, debate.Pass(ctx))
			th, err := repo.Thread(stream, intake.ArchitectAgent)
			must(t, err)
			found := false
			for _, q := range th.Turns {
				if q.Request.TurnID == answer {
					found = true
					if q.CompletedAt.IsZero() || q.Response == nil || !q.Response.Result.Cancelled {
						t.Errorf("answer left blocking the thread: %+v", q)
					}
				}
			}
			if !found {
				t.Fatal("normal question path did not queue its answer")
			}
			fileCurrentAmendment(t, f, repo, stream, "2", 0)
			must(t, draft.Pass(ctx))
			ops, err := repo.Operations(stream)
			must(t, err)
			for _, o := range ops {
				if o.Operation.Action == intake.AmendmentDraftAction && strings.Contains(string(o.Operation.Input), `"2"`) {
					_, err = draft.Apply(ctx, o.Operation)
					must(t, err)
				}
			}
			st, err := repo.Workflow(stream, intake.AmendmentSubject("2"))
			must(t, err)
			if st.Value != "proposed" {
				t.Fatalf("later amendment %+v", st)
			}
			p.Check(t)
		})
	}
}

func TestAmendmentRetirementSkipsUnreadableAndTerminalRequests(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := builtForAmendment(t, 1)
	f.stop(t)
	repo, err := trace.Open(f.s.current().Root, f.s.current().Project)
	must(t, err)
	defer repo.Close()
	f.s.setSole(&activeProject{repository: repo})
	fileCurrentAmendment(t, f, repo, stream, "1", 999)
	fileCurrentAmendment(t, f, repo, stream, "2", 0)
	for i, state := range []string{"declined", "invalid"} {
		id := fmt.Sprint(i + 3)
		fileCurrentAmendment(t, f, repo, stream, id, 0)
		h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "terminal-" + id, Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: intake.ArchitectActor, Cause: "fixture"}
		_, err := repo.Transact(ctx, trace.Transaction{ExpectedVersion: 1, Transition: trace.Transition{Header: h, Subject: intake.AmendmentSubject(id), From: "filed", To: state, Reason: state}})
		must(t, err)
	}
	recordAmendmentSealRevision(t, f, repo, stream, func(s *seal.Seal) { s.Revision.Plan++ })
	c := f.s.intake()
	draft := &intake.AmendmentDrafter{Drafter: &intake.Drafter{S: c.DrafterDependencies(), Repository: repo}}
	debate := intake.AmendmentDebate{Debate: &intake.Debate{S: c.DebateDependencies(), Repository: repo}}
	must(t, draft.Pass(ctx))
	must(t, debate.Pass(ctx))
	for id, want := range map[string]string{"1": "filed", "2": intake.AmendmentRuled, "3": "declined", "4": "invalid"} {
		st, err := repo.Workflow(stream, intake.AmendmentSubject(id))
		must(t, err)
		if st.Value != want {
			t.Errorf("amendment %s: %+v want %s", id, st, want)
		}
	}
}

// recordDraftFixtureSeal seals the documents of the stopped intake fixture.
func recordDraftFixtureSeal(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID) {
	t.Helper()
	content, err := seal.Encode(seal.Seal{Version: seal.Version, Seal: 1, Round: 1, Revision: shed.Pin{Spec: 1, Plan: 1}, SpecHash: seal.SpecHash(validSpec), Base: seal.Base{Remote: "origin", Branch: "main", Commit: "fixture-base"}, Branch: "fixture-branch", Workspace: t.TempDir()})
	must(t, err)
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: seal.DocumentID, Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: intake.OwnerActor, Cause: "fixture"}, Path: seal.Path, Content: string(content)}}))
}

func TestAmendmentRetirementOnlyReconcilesActiveBuilds(t *testing.T) {
	t.Parallel()
	for _, feature := range []string{intake.BuildingState, intake.AssembledState, intake.DeliveredState, intake.AbandonedState} {
		t.Run(feature, func(t *testing.T) {
			ctx := context.Background()
			f, stream := builtForAmendment(t, 1)
			f.stop(t)
			repo, err := trace.Open(f.s.current().Root, f.s.current().Project)
			must(t, err)
			defer repo.Close()
			f.s.setSole(&activeProject{repository: repo})
			fileCurrentAmendment(t, f, repo, stream, "1", 0)
			recordAmendmentSealRevision(t, f, repo, stream, func(s *seal.Seal) { s.Revision.Plan++ })
			if feature != intake.BuildingState {
				st, err := repo.Workflow(stream, trace.FeatureSubject)
				must(t, err)
				_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: st.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "feature-" + feature, Revision: 1, Project: f.project, Workstream: stream, At: f.clock.Now(), Actor: intake.OwnerActor, Cause: "fixture"}, Subject: trace.FeatureSubject, From: st.Value, To: feature, Reason: feature}})
				must(t, err)
			}
			c := f.s.intake()
			draft := &intake.AmendmentDrafter{Drafter: &intake.Drafter{S: c.DrafterDependencies(), Repository: repo}}
			debate := intake.AmendmentDebate{Debate: &intake.Debate{S: c.DebateDependencies(), Repository: repo}}
			must(t, draft.Pass(ctx))
			must(t, debate.Pass(ctx))
			ac, _, err := intake.ReadAmendment(repo, stream, "1")
			must(t, err)
			if feature == intake.BuildingState || feature == intake.AssembledState {
				if len(ac.Decisions) != 1 || ac.Decisions[0].Actor != intake.AmendmentActor.ID {
					t.Errorf("active build not retired: %+v", ac)
				}
			} else if len(ac.Decisions) != 0 || ac.State.Value != "filed" {
				t.Errorf("terminal workstream changed: %+v", ac)
			}
		})
	}
}

func TestAmendmentRetirementResumesAReviewer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, stream := builtForAmendment(t, 1)
	f.stop(t)
	repo, err := trace.Open(f.s.current().Root, f.s.current().Project)
	must(t, err)
	defer repo.Close()
	f.s.setSole(&activeProject{repository: repo})
	requester := intakefixture.PresentAmendment(t, repo, f.project, f.clock.Now(), stream, reviewerRole, amendedSpec, validPlan)
	recordAmendmentSealRevision(t, f, repo, stream, func(s *seal.Seal) { s.Revision.Plan++ })
	debate := intake.AmendmentDebate{Debate: &intake.Debate{S: f.s.intake().DebateDependencies(), Repository: repo}}
	must(t, debate.Pass(ctx))
	must(t, debate.Pass(ctx))
	checkRetirement(t, f, repo, stream, "1", requester, "resume", 2)
}
