package executionjourney

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/execution"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/questions"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/trace"
)

type questionReviewTurn func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error)

func (run questionReviewTurn) Run(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return run(ctx, p)
}

// prepareQuestionReview executes production outcome wrappers and tools with a
// fake agent, leaving capture and completion under the fixture's control.
func prepareQuestionReview(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, ask bool, verdict *UnitVerdict) func() {
	t.Helper()
	agent := reviewerAgent("resume")
	q := claimOldestTurn(t, f, repo, stream, agent)
	scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Unit: "resume", Thread: agent, Turn: q.Request.TurnID, Role: reviewerRole}
	reports := &reviewerReports{}
	fake := questionReviewTurn(func(ctx context.Context, _ coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		if ask {
			list, err := questions.Tools(repo, agent, scope, f.clock.Now)
			must(t, err)
			out, err := askTool(t, list, questions.AskTool).Handle(ctx, json.RawMessage(`{"question":"Which test evidence applies?"}`))
			must(t, err)
			if !strings.Contains(string(out), `"recorded":true`) {
				t.Fatalf("ask: %s", out)
			}
		}
		if verdict != nil {
			raw, err := json.Marshal(verdict)
			must(t, err)
			out, err := reports.Tool(scope).Handle(ctx, raw)
			must(t, err)
			if !strings.Contains(string(out), `"recorded":true`) {
				t.Fatalf("verdict: %s", out)
			}
		}
		return coreadapter.SessionResult{}, nil
	})
	wrapped := &questions.Turns{Turns: execution.NewVerdictTurns(fake, reports), Repository: repo}
	result, err := wrapped.Run(context.Background(), coreadapter.PreparedTurn{Scope: scope})
	must(t, err)
	return func() { completeClaimedTurn(t, f, repo, stream, agent, q, result.Outcome) }
}

func TestReviewerQuestionAnswerBeforeWaiting(t *testing.T) {
	t.Parallel()
	f, stream, repo := newReviewFixture(t, "answer-before-wait")
	r := &reviewers{masons: newMasonController(f.s, repo)}
	ctx := context.Background()
	must(t, r.Pass(ctx))
	complete := prepareQuestionReview(t, f, repo, stream, true, nil)
	chiefAnswers(t, f, repo, stream, "1", "Use the recorded checks.", "spec#1")
	complete()
	verdict := UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}
	prepareQuestionReview(t, f, repo, stream, false, &verdict)()
	for range 4 {
		must(t, r.Pass(ctx))
	}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	if state.Value != UnitApproved {
		t.Fatalf("answered reviewer stranded in %+v", state)
	}
}

// Every legal ordering of asking-turn completion, answer delivery, observing
// the question, and answer-turn completion is driven through ordinary passes.
func TestReviewerQuestionInterleavings(t *testing.T) {
	t.Parallel()
	var orders []string
	var visit func(string)
	visit = func(order string) {
		if len(order) == 4 {
			orders = append(orders, order)
			return
		}
		for _, event := range "CDWV" {
			if strings.ContainsRune(order, event) {
				continue
			}
			if event == 'V' && (!strings.Contains(order, "C") || !strings.Contains(order, "D")) {
				continue
			}
			visit(order + string(event))
		}
	}
	visit("")
	for _, order := range orders {
		for _, restart := range []bool{false, true} {
			name := order
			if restart {
				name += "-restart"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f, stream, repo := newReviewFixture(t, "question-order")
				defer func() { must(t, repo.Close()) }()
				r := &reviewers{masons: newMasonController(f.s, repo)}
				ctx := context.Background()
				must(t, r.Pass(ctx))
				complete := prepareQuestionReview(t, f, repo, stream, true, nil)
				verdict := UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}
				for _, event := range order {
					switch event {
					case 'C':
						complete()
					case 'D':
						chiefAnswers(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
					case 'W':
						must(t, r.Pass(ctx))
					case 'V':
						prepareQuestionReview(t, f, repo, stream, false, &verdict)()
					}
					// A restart with a live claimed turn would correctly interrupt that
					// turn. These checkpoints reopen only after the asking turn completed.
					if restart && strings.Contains(order[:strings.IndexRune(order, event)+1], "C") {
						must(t, repo.Close())
						var err error
						repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
						must(t, err)
						r = &reviewers{masons: newMasonController(f.s, repo)}
					}
				}
				for range 4 {
					must(t, r.Pass(ctx))
				}
				state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
				must(t, err)
				if state.Value != UnitApproved {
					t.Fatalf("order %s stranded reviewer in %+v", order, state)
				}
				before, err := trace.Read[trace.Transition](repo, stream)
				must(t, err)
				notices, err := repo.Outbox(stream)
				must(t, err)
				thread, err := repo.Thread(stream, reviewerAgent("resume"))
				must(t, err)
				for range 3 {
					must(t, r.Pass(ctx))
					must(t, f.s.answers(f.s.cfg, repo).Pass(ctx))
				}
				after, err := trace.Read[trace.Transition](repo, stream)
				must(t, err)
				later, err := repo.Thread(stream, reviewerAgent("resume"))
				must(t, err)
				afterNotices, err := repo.Outbox(stream)
				must(t, err)
				if len(afterNotices) != len(notices) {
					t.Fatal("repeated passes duplicated notices")
				}
				if len(after) != len(before) || len(later.Turns) != len(thread.Turns) {
					t.Fatal("repeated passes duplicated transitions or turns")
				}
			})
		}
	}
}

func TestReviewerQuestionLaterVerdictReconcilesAfterRestart(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{"satisfactory", "material_findings", "contested"} {
		t.Run(decision, func(t *testing.T) {
			f, stream, repo := newReviewFixture(t, "later-verdict")
			defer func() { must(t, repo.Close()) }()
			if decision == "contested" {
				f.s.cfg.Shed.MaxBounces = 1
			}
			r := &reviewers{masons: newMasonController(f.s, repo)}
			ctx := context.Background()
			must(t, r.Pass(ctx))
			prepareQuestionReview(t, f, repo, stream, true, nil)()
			must(t, r.Pass(ctx))
			chiefAnswers(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
			// The answer is followed by a completed review while the unit still waits.
			prepareQuestionReview(t, f, repo, stream, false, nil)()
			th, err := repo.Thread(stream, reviewerAgent("resume"))
			must(t, err)
			req := th.Turns[0].Request
			req.ID, req.TurnID, req.At = "request_review-following", "review-following", f.clock.Now()
			_, err = repo.EnqueueTurn(ctx, req)
			must(t, err)
			v := UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}
			if decision != "satisfactory" {
				v.Decision = "material_findings"
				v.Findings = []ReviewFinding{{Severity: "material", Evidence: "Retry fails", Action: "Handle retry"}}
			}
			prepareQuestionReview(t, f, repo, stream, false, &v)()
			history, err := trace.Read[trace.Transition](repo, stream)
			must(t, err)
			must(t, repo.Close())
			repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			r = &reviewers{masons: newMasonController(f.s, repo)}
			must(t, r.Pass(ctx))
			state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
			must(t, err)
			if state.Value == UnitReviewing {
				// A new review must pin the same candidate and use a new turn ID.
				th, err = repo.Thread(stream, reviewerAgent("resume"))
				must(t, err)
				last := th.Turns[len(th.Turns)-1]
				if !last.CompletedAt.IsZero() {
					t.Fatal("reviewing without a queued review")
				}
				oldID, err := reviewtools.IdentityInPrompt(req.Prompt)
				must(t, err)
				newID, err := reviewtools.IdentityInPrompt(last.Request.Prompt)
				must(t, err)
				if staleReview(oldID, newID) != "" {
					t.Fatal("candidate changed during question wait")
				}
				prepareQuestionReview(t, f, repo, stream, false, &v)()
				must(t, r.Pass(ctx))
			}
			state, err = repo.Workflow(stream, trace.UnitSubject("resume"))
			must(t, err)
			want := UnitApproved
			if decision == "material_findings" {
				want = UnitImplementing
			}
			if decision == "contested" {
				want = UnitContested
			}
			if state.Value != want {
				t.Fatalf("got %+v, want %s", state, want)
			}
			after, err := trace.Read[trace.Transition](repo, stream)
			must(t, err)
			if !reflect.DeepEqual(history, after[:len(history)]) {
				t.Fatal("recovery rewrote history")
			}
			for range 3 {
				must(t, r.Pass(ctx))
			}
			if decision == "material_findings" {
				mason, err := repo.Thread(stream, masonAgent("resume"))
				must(t, err)
				last := mason.Turns[len(mason.Turns)-1]
				if !last.CompletedAt.IsZero() || !strings.Contains(last.Request.Prompt, "Handle retry") {
					t.Fatal("material findings not queued")
				}
			}
		})
	}
}

func TestMasonQuestionDoneWhileWaiting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ late, early bool }{{}, {late: true}, {early: true}, {late: true, early: true}} {
		t.Run(fmt.Sprintf("later-turn-%v-early-answer-%v", tc.late, tc.early), func(t *testing.T) {
			f, _ := newMasonFixture(t, 1, independentPlan)
			base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
			stream, repo := seedBuild(t, f, "mason-question-done", independentPlan, config.WorkspacesGit, base)
			m := newMasonController(f.s, repo)
			ctx := context.Background()
			must(t, m.Pass(ctx))
			agent := masonAgent("resume")
			q := claimOldestTurn(t, f, repo, stream, agent)
			scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Unit: "resume", Thread: agent, Turn: q.Request.TurnID, Role: masonRole}
			list, err := questions.Tools(repo, agent, scope, f.clock.Now)
			must(t, err)
			out, err := askTool(t, list, questions.AskTool).Handle(ctx, json.RawMessage(`{"question":"Which checks apply?"}`))
			must(t, err)
			if !strings.Contains(string(out), `"recorded":true`) {
				t.Fatal(string(out))
			}
			if !tc.early {
				completeClaimedTurn(t, f, repo, stream, agent, q, &coreadapter.Outcome{Status: questions.Waiting})
				must(t, m.Pass(ctx))
				state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
				must(t, err)
				if state.Value != UnitWaiting {
					t.Fatalf("did not wait: %+v", state)
				}
			}
			reports := &masonReports{}
			if !tc.early {
				out, err = reports.Tool(repo, scope).Handle(ctx, json.RawMessage(`{"outcome":"Built and verified.","headline":"Work is complete","happened":"Built and verified the requested behavior."}`))
				must(t, err)
				if strings.Contains(string(out), `"recorded":true`) {
					t.Fatal("done accepted unanswered wait")
				}
			}
			chiefRecordsAnswer(t, f, repo, stream, "1", "Use the recorded checks.", "spec#1")
			if !tc.early {
				out, err = reports.Tool(repo, scope).Handle(ctx, json.RawMessage(`{"outcome":"Built and verified.","headline":"Work is complete","happened":"Built and verified the requested behavior."}`))
				must(t, err)
				if strings.Contains(string(out), `"recorded":true`) {
					t.Fatal("done accepted undelivered answer")
				}
			}
			must(t, f.s.answers(f.s.cfg, repo).Pass(ctx))
			if !tc.early {
				out, err = reports.Tool(repo, scope).Handle(ctx, json.RawMessage(`{"outcome":"Built and verified.","headline":"Work is complete","happened":"Built and verified the requested behavior."}`))
				must(t, err)
				if strings.Contains(string(out), `"recorded":true`) {
					t.Fatal("done accepted a turn other than the delivered answer")
				}
			}
			if tc.early {
				completeClaimedTurn(t, f, repo, stream, agent, q, &coreadapter.Outcome{Status: questions.Waiting})
			}
			if tc.late {
				captureTurn(t, f, repo, stream, agent, nil)
				queueFollowingTurn(t, f, repo, stream, agent, "resume", "mason-following")
			}
			q = claimOldestTurn(t, f, repo, stream, agent)
			scope.Turn = q.Request.TurnID
			if tc.late {
				must(t, m.Pass(ctx))
			}
			fake := questionReviewTurn(func(ctx context.Context, _ coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
				out, err := reports.Tool(repo, scope).Handle(ctx, json.RawMessage(`{"outcome":"Built and verified.","headline":"Work is complete","happened":"Built and verified the requested behavior."}`))
				must(t, err)
				if !strings.Contains(string(out), `"recorded":true`) {
					t.Fatalf("done after delivered answer: %s", out)
				}
				return coreadapter.SessionResult{}, nil
			})
			wrapper := &questions.Turns{Turns: execution.NewReportingTurns(fake, reports), Repository: repo}
			result, err := wrapper.Run(ctx, coreadapter.PreparedTurn{Scope: scope})
			must(t, err)
			completeClaimedTurn(t, f, repo, stream, agent, q, result.Outcome)
			// Reopen after done was recorded but before its controller consumes it.
			must(t, repo.Close())
			repo, err = trace.Open(f.s.cfg.Root, f.s.cfg.Project)
			must(t, err)
			defer repo.Close()
			m = newMasonController(f.s, repo)
			for range 3 {
				must(t, m.Pass(ctx))
			}
			state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
			must(t, err)
			if state.Value != UnitChecking {
				t.Fatalf("done stranded in %+v", state)
			}
			runChecks(t, f.s, repo, stream)
			state, err = repo.Workflow(stream, trace.UnitSubject("resume"))
			must(t, err)
			if state.Value != UnitReviewing {
				t.Fatalf("checks did not advance: %+v", state)
			}
		})
	}
}

func TestReviewerQuestionWaitOwnershipAndUncertainty(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ ambiguous, foreign bool }{{}, {ambiguous: true}, {ambiguous: true, foreign: true}} {
		ambiguous := tc.ambiguous
		t.Run(fmt.Sprintf("ambiguous-%v-foreign-actor-%v", ambiguous, tc.foreign), func(t *testing.T) {
			f, stream, repo := newReviewFixture(t, "wait-provenance")
			r := &reviewers{masons: newMasonController(f.s, repo)}
			ctx := context.Background()
			must(t, r.Pass(ctx))
			prepareQuestionReview(t, f, repo, stream, true, nil)()
			must(t, r.Pass(ctx))
			chiefAnswers(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
			v := UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}
			prepareQuestionReview(t, f, repo, stream, false, &v)()
			state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
			must(t, err)
			actor, cause := ownerActor, trace.QuestionSubject("1")+"_"+trace.QuestionOpen
			if ambiguous {
				actor, cause = reviewerActor, "unidentified-question"
			}
			if tc.foreign {
				actor = ownerActor
			}
			_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-wait-provenance", Revision: 1, Project: repo.Project(), Workstream: stream, Unit: "resume", At: f.clock.Now(), Actor: actor, Cause: cause}, Subject: trace.UnitSubject("resume"), From: UnitWaiting, To: UnitWaiting, Reason: "Wait on the question"}})
			must(t, err)
			for range 3 {
				must(t, r.Pass(ctx))
			}
			state, err = repo.Workflow(stream, trace.UnitSubject("resume"))
			must(t, err)
			want := UnitApproved
			if ambiguous {
				want = UnitContested
			}
			if state.Value != want {
				t.Fatalf("wait stranded in %+v, want %s", state, want)
			}
			if ambiguous {
				last, err := latestUnitTransition(repo, stream, "resume")
				must(t, err)
				if !strings.Contains(last.Reason, "does not identify") {
					t.Fatalf("contest lacks reason: %+v", last)
				}
				notices, err := repo.Outbox(stream)
				must(t, err)
				found := false
				for _, notice := range notices {
					if strings.Contains(notice.Event.Body, "cannot reconcile answered question wait") {
						found = true
					}
				}
				if !found {
					t.Fatal("contest has no notice for the chief and owner feed")
				}
				f.s.setSole(&activeProject{repository: repo})
				_, api := f.s.recordContestedRuling(ctx, repo, stream, "resume", ContestedRulingRequest{Decision: "review", Note: "Review the same candidate again."}, chiefActor)
				if api != nil {
					t.Fatalf("chief cannot resolve question contest: %+v", api)
				}
			}
		})
	}
}

func TestReviewerQuestionRequiresAnswerDelivery(t *testing.T) {
	t.Parallel()
	f, stream, repo := newReviewFixture(t, "pending-answer")
	r := &reviewers{masons: newMasonController(f.s, repo)}
	ctx := context.Background()
	must(t, r.Pass(ctx))
	// Asking and recording a verdict in the same turn must still park.
	v := UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}
	prepareQuestionReview(t, f, repo, stream, true, &v)()
	must(t, r.Pass(ctx))
	for _, answered := range []bool{false, true} {
		if answered {
			chiefRecordsAnswer(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
		}
		for range 3 {
			must(t, r.Pass(ctx))
		}
		state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
		must(t, err)
		if state.Value != UnitWaiting {
			t.Fatalf("answered=%v without delivery advanced to %+v", answered, state)
		}
	}
	must(t, f.s.answers(f.s.cfg, repo).Pass(ctx))
	prepareQuestionReview(t, f, repo, stream, false, &v)()
	for range 3 {
		must(t, r.Pass(ctx))
	}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	if state.Value != UnitApproved {
		t.Fatalf("delivered answer stranded in %+v", state)
	}
}

func TestReviewerQuestionStaleVerdictNeverApproves(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"candidate", "base", "spec", "plan"} {
		t.Run(field, func(t *testing.T) {
			f, stream, repo := newReviewFixture(t, "stale-question")
			r := &reviewers{masons: newMasonController(f.s, repo)}
			ctx := context.Background()
			must(t, r.Pass(ctx))
			prepareQuestionReview(t, f, repo, stream, true, nil)()
			must(t, r.Pass(ctx))
			chiefAnswers(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
			v := UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}
			prepareQuestionReview(t, f, repo, stream, false, &v)()
			// The original turn remains immutable; change the current candidate's
			// governing evidence before the controller reads the completed verdict.
			docs, err := trace.Read[trace.Document](repo, stream)
			must(t, err)
			id := reportDocument("resume")
			if field == "spec" {
				id = "spec"
			}
			if field == "plan" {
				id = "plan"
			}
			var latest trace.Document
			for _, d := range docs {
				if d.ID == id {
					latest = d
				}
			}
			if latest.Revision == 0 {
				t.Fatalf("missing fixture document %s", id)
			}
			if field == "candidate" || field == "base" {
				var report UnitReport
				must(t, json.Unmarshal([]byte(latest.Content), &report))
				if field == "candidate" {
					report.Candidate = report.Base
				} else {
					report.Base = report.Candidate
				}
				raw, err := json.Marshal(report)
				must(t, err)
				latest.Content = string(raw)
			}
			latest.Revision++
			latest.At = f.clock.Now()
			latest.Cause = "fixture-input-change"
			must(t, repo.RecordDocuments(ctx, []trace.Document{latest}))
			must(t, r.Pass(ctx))
			state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
			must(t, err)
			if state.Value == UnitApproved || state.Value == UnitWaiting {
				t.Fatalf("stale %s verdict produced %+v", field, state)
			}
			last, err := latestUnitTransition(repo, stream, "resume")
			must(t, err)
			if !strings.Contains(last.Reason, "stale") {
				t.Fatalf("missing refusal reason: %s", last.Reason)
			}
		})
	}
}

func TestReviewerEarlyAnswerWithoutVerdictQueuesFreshReview(t *testing.T) {
	t.Parallel()
	f, stream, repo := newReviewFixture(t, "early-answer-no-verdict")
	r := &reviewers{masons: newMasonController(f.s, repo)}
	ctx := context.Background()
	must(t, r.Pass(ctx))
	finish := prepareQuestionReview(t, f, repo, stream, true, nil)
	chiefAnswers(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
	finish()
	prepareQuestionReview(t, f, repo, stream, false, nil)()
	for range 4 {
		must(t, r.Pass(ctx))
	}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	th, err := repo.Thread(stream, reviewerAgent("resume"))
	must(t, err)
	last := th.Turns[len(th.Turns)-1]
	if state.Value != UnitReviewing || len(th.Turns) != 3 || !last.CompletedAt.IsZero() {
		t.Fatalf("no fresh review after answer: %+v, %+v", state, th)
	}
	origin, err := reviewtools.IdentityInPrompt(th.Turns[0].Request.Prompt)
	must(t, err)
	fresh, err := reviewtools.IdentityInPrompt(last.Request.Prompt)
	must(t, err)
	if staleReview(origin, fresh) != "" {
		t.Fatal("fresh review changed the candidate")
	}
	v := UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}
	prepareQuestionReview(t, f, repo, stream, false, &v)()
	must(t, r.Pass(ctx))
	state, err = repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	if state.Value != UnitApproved {
		t.Fatalf("fresh review stranded in %+v", state)
	}
}

func TestReviewerQuestionPendingAnswerHoldsLaterVerdict(t *testing.T) {
	t.Parallel()
	f, stream, repo := newReviewFixture(t, "pending-later-verdict")
	r := &reviewers{masons: newMasonController(f.s, repo)}
	ctx := context.Background()
	must(t, r.Pass(ctx))
	prepareQuestionReview(t, f, repo, stream, true, nil)()
	must(t, r.Pass(ctx))
	th, err := repo.Thread(stream, reviewerAgent("resume"))
	must(t, err)
	req := th.Turns[0].Request
	req.ID, req.TurnID, req.At = "request_later-verdict", "later-verdict", f.clock.Now()
	_, err = repo.EnqueueTurn(ctx, req)
	must(t, err)
	v := UnitVerdict{Decision: "satisfactory", Summary: reviewSummary}
	prepareQuestionReview(t, f, repo, stream, false, &v)()
	for _, answered := range []bool{false, true} {
		if answered {
			chiefRecordsAnswer(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
		}
		for range 2 {
			must(t, r.Pass(ctx))
		}
		state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
		must(t, err)
		if state.Value != UnitWaiting {
			t.Fatalf("answered=%v advanced undelivered question: %+v", answered, state)
		}
	}
	must(t, f.s.answers(f.s.cfg, repo).Pass(ctx))
	prepareQuestionReview(t, f, repo, stream, false, &v)()
	for range 3 {
		must(t, r.Pass(ctx))
	}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	must(t, err)
	if state.Value != UnitApproved {
		t.Fatalf("delivered answer stranded in %+v", state)
	}
}

func TestMasonDoneRefusesReviewerAndAmendmentWaits(t *testing.T) {
	t.Parallel()
	f, stream, repo := newReviewFixture(t, "done-other-waits")
	ctx := context.Background()
	r := &reviewers{masons: newMasonController(f.s, repo)}
	must(t, r.Pass(ctx))
	prepareQuestionReview(t, f, repo, stream, true, nil)()
	must(t, r.Pass(ctx))
	chiefAnswers(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
	for _, amendment := range []bool{false, true} {
		if amendment {
			state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
			must(t, err)
			_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-amendment-wait", Revision: 1, Project: repo.Project(), Workstream: stream, Unit: "resume", At: f.clock.Now(), Actor: masonActor, Cause: "amendment_1"}, Subject: trace.UnitSubject("resume"), From: UnitWaiting, To: UnitWaiting, Reason: "Wait for the amendment"}})
			must(t, err)
		}
		for _, agent := range []string{masonAgent("resume"), reviewerAgent("resume")} {
			scope := coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Unit: "resume", Thread: agent, Turn: questions.TurnID("1"), Role: masonRole}
			out, err := (&masonReports{}).Tool(repo, scope).Handle(ctx, json.RawMessage(`{"outcome":"Built and verified.","headline":"Work is complete","happened":"Built and verified the requested behavior."}`))
			must(t, err)
			if strings.Contains(string(out), `"recorded":true`) {
				t.Fatalf("done accepted another role or amendment wait: %s", out)
			}
		}
	}
}
