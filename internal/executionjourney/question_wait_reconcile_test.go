package executionjourney

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/questions"
	reviewtools "github.com/kpenfound/osmia/internal/review"
	"github.com/kpenfound/osmia/internal/trace"
)

func prepareQuestionReview(t *testing.T, f *journey.ShedFixture, repo *trace.Repository, stream config.WorkstreamID, ask bool, verdict *service.UnitVerdict) func() {
	t.Helper()
	agent := intake.ReviewerAgent("resume")
	q := journey.ClaimOldestTurn(t, f, repo, stream, agent)
	scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Unit: "resume", Thread: agent, Turn: q.Request.TurnID, Role: intake.ReviewerRole}
	reports := &execution.ReviewerReports{}
	fake := fixturekit.TurnsFunc(func(ctx context.Context, _ coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		if ask {
			list, err := questions.Tools(repo, agent, scope, f.Clock.Now)
			fixturekit.Must(t, err)
			out, err := intakefixture.AskTool(t, list, questions.AskTool).Handle(ctx, json.RawMessage(`{"question":"Which test evidence applies?"}`))
			fixturekit.Must(t, err)
			if !strings.Contains(string(out), `"recorded":true`) {
				t.Fatalf("ask: %s", out)
			}
		}
		if verdict != nil {
			raw, err := json.Marshal(verdict)
			fixturekit.Must(t, err)
			out, err := reports.Tool(scope).Handle(ctx, raw)
			fixturekit.Must(t, err)
			if !strings.Contains(string(out), `"recorded":true`) {
				t.Fatalf("verdict: %s", out)
			}
		}
		return coreadapter.SessionResult{}, nil
	})
	wrapped := &questions.Turns{Turns: execution.NewVerdictTurns(fake, reports), Repository: repo}
	result, err := wrapped.Run(context.Background(), coreadapter.PreparedTurn{Scope: scope})
	fixturekit.Must(t, err)
	return func() { journey.CompleteClaimedTurn(t, f, repo, stream, agent, q, result.Outcome) }
}

func TestReviewerQuestionAnswerBeforeWaiting(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewReviewFixture(t, "answer-before-wait")
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
	ctx := context.Background()
	fixturekit.Must(t, r.Pass(ctx))
	complete := prepareQuestionReview(t, f, repo, stream, true, nil)
	journey.ChiefAnswers(t, f, repo, stream, "1", "Use the recorded checks.", "spec#1")
	complete()
	verdict := service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}
	prepareQuestionReview(t, f, repo, stream, false, &verdict)()
	for range 4 {
		fixturekit.Must(t, r.Pass(ctx))
	}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if state.Value != intake.UnitApproved {
		t.Fatalf("answered reviewer stranded in %+v", state)
	}
}

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
				f, stream, repo := journey.NewReviewFixture(t, "question-order")
				defer func() { fixturekit.Must(t, repo.Close()) }()
				r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
				ctx := context.Background()
				fixturekit.Must(t, r.Pass(ctx))
				complete := prepareQuestionReview(t, f, repo, stream, true, nil)
				verdict := service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}
				for _, event := range order {
					switch event {
					case 'C':
						complete()
					case 'D':
						journey.ChiefAnswers(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
					case 'W':
						fixturekit.Must(t, r.Pass(ctx))
					case 'V':
						prepareQuestionReview(t, f, repo, stream, false, &verdict)()
					}

					if restart && strings.Contains(order[:strings.IndexRune(order, event)+1], "C") {
						fixturekit.Must(t, repo.Close())
						var err error
						repo, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
						fixturekit.Must(t, err)
						r = &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
					}
				}
				for range 4 {
					fixturekit.Must(t, r.Pass(ctx))
				}
				state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
				fixturekit.Must(t, err)
				if state.Value != intake.UnitApproved {
					t.Fatalf("order %s stranded reviewer in %+v", order, state)
				}
				before, err := trace.Read[trace.Transition](repo, stream)
				fixturekit.Must(t, err)
				notices, err := repo.Outbox(stream)
				fixturekit.Must(t, err)
				thread, err := repo.Thread(stream, intake.ReviewerAgent("resume"))
				fixturekit.Must(t, err)
				for range 3 {
					fixturekit.Must(t, r.Pass(ctx))
					fixturekit.Must(t, f.S.Answers(f.S.Cfg, repo).Pass(ctx))
				}
				after, err := trace.Read[trace.Transition](repo, stream)
				fixturekit.Must(t, err)
				later, err := repo.Thread(stream, intake.ReviewerAgent("resume"))
				fixturekit.Must(t, err)
				afterNotices, err := repo.Outbox(stream)
				fixturekit.Must(t, err)
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
			f, stream, repo := journey.NewReviewFixture(t, "later-verdict")
			defer func() { fixturekit.Must(t, repo.Close()) }()
			if decision == "contested" {
				f.S.Cfg.Shed.MaxBounces = 1
			}
			r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
			ctx := context.Background()
			fixturekit.Must(t, r.Pass(ctx))
			prepareQuestionReview(t, f, repo, stream, true, nil)()
			fixturekit.Must(t, r.Pass(ctx))
			journey.ChiefAnswers(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")

			prepareQuestionReview(t, f, repo, stream, false, nil)()
			th, err := repo.Thread(stream, intake.ReviewerAgent("resume"))
			fixturekit.Must(t, err)
			req := th.Turns[0].Request
			req.ID, req.TurnID, req.At = "request_review-following", "review-following", f.Clock.Now()
			_, err = repo.EnqueueTurn(ctx, req)
			fixturekit.Must(t, err)
			v := service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}
			if decision != "satisfactory" {
				v.Decision = "material_findings"
				v.Findings = []api.ReviewFinding{{Severity: "material", Evidence: "Retry fails", Action: "Handle retry"}}
			}
			prepareQuestionReview(t, f, repo, stream, false, &v)()
			history, err := trace.Read[trace.Transition](repo, stream)
			fixturekit.Must(t, err)
			fixturekit.Must(t, repo.Close())
			repo, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
			fixturekit.Must(t, err)
			r = &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
			fixturekit.Must(t, r.Pass(ctx))
			state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
			fixturekit.Must(t, err)
			if state.Value == intake.UnitReviewing {

				th, err = repo.Thread(stream, intake.ReviewerAgent("resume"))
				fixturekit.Must(t, err)
				last := th.Turns[len(th.Turns)-1]
				if !last.CompletedAt.IsZero() {
					t.Fatal("reviewing without a queued review")
				}
				oldID, err := reviewtools.IdentityInPrompt(req.Prompt)
				fixturekit.Must(t, err)
				newID, err := reviewtools.IdentityInPrompt(last.Request.Prompt)
				fixturekit.Must(t, err)
				if execution.StaleReview(oldID, newID) != "" {
					t.Fatal("candidate changed during question wait")
				}
				prepareQuestionReview(t, f, repo, stream, false, &v)()
				fixturekit.Must(t, r.Pass(ctx))
			}
			state, err = repo.Workflow(stream, trace.UnitSubject("resume"))
			fixturekit.Must(t, err)
			want := intake.UnitApproved
			if decision == "material_findings" {
				want = intake.UnitImplementing
			}
			if decision == "contested" {
				want = intake.UnitContested
			}
			if state.Value != want {
				t.Fatalf("got %+v, want %s", state, want)
			}
			after, err := trace.Read[trace.Transition](repo, stream)
			fixturekit.Must(t, err)
			if !reflect.DeepEqual(history, after[:len(history)]) {
				t.Fatal("recovery rewrote history")
			}
			for range 3 {
				fixturekit.Must(t, r.Pass(ctx))
			}
			if decision == "material_findings" {
				mason, err := repo.Thread(stream, intake.MasonAgent("resume"))
				fixturekit.Must(t, err)
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
			f, _ := journey.NewMasonFixture(t, 1, intakefixture.IndependentPlan)
			base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
			stream, repo := journey.SeedBuild(t, f, "mason-question-done", intakefixture.IndependentPlan, config.WorkspacesGit, base)
			m := journey.NewMasonController(f.S, repo)
			ctx := context.Background()
			fixturekit.Must(t, m.Pass(ctx))
			agent := intake.MasonAgent("resume")
			q := journey.ClaimOldestTurn(t, f, repo, stream, agent)
			scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Unit: "resume", Thread: agent, Turn: q.Request.TurnID, Role: intake.MasonRole}
			list, err := questions.Tools(repo, agent, scope, f.Clock.Now)
			fixturekit.Must(t, err)
			out, err := intakefixture.AskTool(t, list, questions.AskTool).Handle(ctx, json.RawMessage(`{"question":"Which checks apply?"}`))
			fixturekit.Must(t, err)
			if !strings.Contains(string(out), `"recorded":true`) {
				t.Fatal(string(out))
			}
			if !tc.early {
				journey.CompleteClaimedTurn(t, f, repo, stream, agent, q, &coreadapter.Outcome{Status: questions.Waiting})
				fixturekit.Must(t, m.Pass(ctx))
				state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
				fixturekit.Must(t, err)
				if state.Value != intake.UnitWaiting {
					t.Fatalf("did not wait: %+v", state)
				}
			}
			reports := &execution.MasonReports{}
			if !tc.early {
				out, err = reports.Tool(repo, scope).Handle(ctx, json.RawMessage(`{"outcome":"Built and verified.","headline":"Work is complete","happened":"Built and verified the requested behavior."}`))
				fixturekit.Must(t, err)
				if strings.Contains(string(out), `"recorded":true`) {
					t.Fatal("done accepted unanswered wait")
				}
			}
			journey.ChiefRecordsAnswer(t, f, repo, stream, "1", "Use the recorded checks.", "spec#1")
			if !tc.early {
				out, err = reports.Tool(repo, scope).Handle(ctx, json.RawMessage(`{"outcome":"Built and verified.","headline":"Work is complete","happened":"Built and verified the requested behavior."}`))
				fixturekit.Must(t, err)
				if strings.Contains(string(out), `"recorded":true`) {
					t.Fatal("done accepted undelivered answer")
				}
			}
			fixturekit.Must(t, f.S.Answers(f.S.Cfg, repo).Pass(ctx))
			if !tc.early {
				out, err = reports.Tool(repo, scope).Handle(ctx, json.RawMessage(`{"outcome":"Built and verified.","headline":"Work is complete","happened":"Built and verified the requested behavior."}`))
				fixturekit.Must(t, err)
				if strings.Contains(string(out), `"recorded":true`) {
					t.Fatal("done accepted a turn other than the delivered answer")
				}
			}
			if tc.early {
				journey.CompleteClaimedTurn(t, f, repo, stream, agent, q, &coreadapter.Outcome{Status: questions.Waiting})
			}
			if tc.late {
				journey.CaptureTurn(t, f, repo, stream, agent, nil)
				queueFollowingTurn(t, f, repo, stream, agent, "resume", "mason-following")
			}
			q = journey.ClaimOldestTurn(t, f, repo, stream, agent)
			scope.Turn = q.Request.TurnID
			if tc.late {
				fixturekit.Must(t, m.Pass(ctx))
			}
			fake := fixturekit.TurnsFunc(func(ctx context.Context, _ coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
				out, err := reports.Tool(repo, scope).Handle(ctx, json.RawMessage(`{"outcome":"Built and verified.","headline":"Work is complete","happened":"Built and verified the requested behavior."}`))
				fixturekit.Must(t, err)
				if !strings.Contains(string(out), `"recorded":true`) {
					t.Fatalf("done after delivered answer: %s", out)
				}
				return coreadapter.SessionResult{}, nil
			})
			wrapper := &questions.Turns{Turns: execution.NewReportingTurns(fake, reports), Repository: repo}
			result, err := wrapper.Run(ctx, coreadapter.PreparedTurn{Scope: scope})
			fixturekit.Must(t, err)
			journey.CompleteClaimedTurn(t, f, repo, stream, agent, q, result.Outcome)

			fixturekit.Must(t, repo.Close())
			repo, err = trace.Open(f.S.Cfg.Root, f.S.Cfg.Project)
			fixturekit.Must(t, err)
			defer repo.Close()
			m = journey.NewMasonController(f.S, repo)
			for range 3 {
				fixturekit.Must(t, m.Pass(ctx))
			}
			state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
			fixturekit.Must(t, err)
			if state.Value != execution.UnitChecking {
				t.Fatalf("done stranded in %+v", state)
			}
			journey.RunChecks(t, f.S, repo, stream)
			state, err = repo.Workflow(stream, trace.UnitSubject("resume"))
			fixturekit.Must(t, err)
			if state.Value != intake.UnitReviewing {
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
			f, stream, repo := journey.NewReviewFixture(t, "wait-provenance")
			r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
			ctx := context.Background()
			fixturekit.Must(t, r.Pass(ctx))
			prepareQuestionReview(t, f, repo, stream, true, nil)()
			fixturekit.Must(t, r.Pass(ctx))
			journey.ChiefAnswers(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
			v := service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}
			prepareQuestionReview(t, f, repo, stream, false, &v)()
			state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
			fixturekit.Must(t, err)
			actor, cause := intake.OwnerActor, trace.QuestionSubject("1")+"_"+trace.QuestionOpen
			if ambiguous {
				actor, cause = execution.ReviewerActor, "unidentified-question"
			}
			if tc.foreign {
				actor = intake.OwnerActor
			}
			_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-wait-provenance", Revision: 1, Project: repo.Project(), Workstream: stream, Unit: "resume", At: f.Clock.Now(), Actor: actor, Cause: cause}, Subject: trace.UnitSubject("resume"), From: intake.UnitWaiting, To: intake.UnitWaiting, Reason: "Wait on the question"}})
			fixturekit.Must(t, err)
			for range 3 {
				fixturekit.Must(t, r.Pass(ctx))
			}
			state, err = repo.Workflow(stream, trace.UnitSubject("resume"))
			fixturekit.Must(t, err)
			want := intake.UnitApproved
			if ambiguous {
				want = intake.UnitContested
			}
			if state.Value != want {
				t.Fatalf("wait stranded in %+v, want %s", state, want)
			}
			if ambiguous {
				last, err := execution.LatestUnitTransition(repo, stream, "resume")
				fixturekit.Must(t, err)
				if !strings.Contains(last.Reason, "does not identify") {
					t.Fatalf("contest lacks reason: %+v", last)
				}
				notices, err := repo.Outbox(stream)
				fixturekit.Must(t, err)
				found := false
				for _, notice := range notices {
					if strings.Contains(notice.Event.Body, "cannot reconcile answered question wait") {
						found = true
					}
				}
				if !found {
					t.Fatal("contest has no notice for the chief and owner feed")
				}
				f.S.SetSole(&journey.ActiveProject{Repository: repo})
				_, apiErr := recordContestedRuling(f.S, ctx, repo, stream, "resume", api.ContestedRulingRequest{Decision: "review", Note: "Review the same candidate again."}, intake.ChiefActor)
				if apiErr != nil {
					t.Fatalf("chief cannot resolve question contest: %+v", apiErr)
				}
			}
		})
	}
}

func TestReviewerQuestionRequiresAnswerDelivery(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewReviewFixture(t, "pending-answer")
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
	ctx := context.Background()
	fixturekit.Must(t, r.Pass(ctx))

	v := service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}
	prepareQuestionReview(t, f, repo, stream, true, &v)()
	fixturekit.Must(t, r.Pass(ctx))
	for _, answered := range []bool{false, true} {
		if answered {
			journey.ChiefRecordsAnswer(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
		}
		for range 3 {
			fixturekit.Must(t, r.Pass(ctx))
		}
		state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
		fixturekit.Must(t, err)
		if state.Value != intake.UnitWaiting {
			t.Fatalf("answered=%v without delivery advanced to %+v", answered, state)
		}
	}
	fixturekit.Must(t, f.S.Answers(f.S.Cfg, repo).Pass(ctx))
	prepareQuestionReview(t, f, repo, stream, false, &v)()
	for range 3 {
		fixturekit.Must(t, r.Pass(ctx))
	}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if state.Value != intake.UnitApproved {
		t.Fatalf("delivered answer stranded in %+v", state)
	}
}

func TestReviewerQuestionStaleVerdictNeverApproves(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"candidate", "base", "spec", "plan"} {
		t.Run(field, func(t *testing.T) {
			f, stream, repo := journey.NewReviewFixture(t, "stale-question")
			r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
			ctx := context.Background()
			fixturekit.Must(t, r.Pass(ctx))
			prepareQuestionReview(t, f, repo, stream, true, nil)()
			fixturekit.Must(t, r.Pass(ctx))
			journey.ChiefAnswers(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
			v := service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}
			prepareQuestionReview(t, f, repo, stream, false, &v)()

			docs, err := trace.Read[trace.Document](repo, stream)
			fixturekit.Must(t, err)
			id := execution.ReportDocument("resume")
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
				var report execution.UnitReport
				fixturekit.Must(t, json.Unmarshal([]byte(latest.Content), &report))
				if field == "candidate" {
					report.Candidate = report.Base
				} else {
					report.Base = report.Candidate
				}
				raw, err := json.Marshal(report)
				fixturekit.Must(t, err)
				latest.Content = string(raw)
			}
			latest.Revision++
			latest.At = f.Clock.Now()
			latest.Cause = "fixture-input-change"
			fixturekit.Must(t, repo.RecordDocuments(ctx, []trace.Document{latest}))
			fixturekit.Must(t, r.Pass(ctx))
			state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
			fixturekit.Must(t, err)
			if state.Value == intake.UnitApproved || state.Value == intake.UnitWaiting {
				t.Fatalf("stale %s verdict produced %+v", field, state)
			}
			last, err := execution.LatestUnitTransition(repo, stream, "resume")
			fixturekit.Must(t, err)
			if !strings.Contains(last.Reason, "stale") {
				t.Fatalf("missing refusal reason: %s", last.Reason)
			}
		})
	}
}

func TestReviewerEarlyAnswerWithoutVerdictQueuesFreshReview(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewReviewFixture(t, "early-answer-no-verdict")
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
	ctx := context.Background()
	fixturekit.Must(t, r.Pass(ctx))
	finish := prepareQuestionReview(t, f, repo, stream, true, nil)
	journey.ChiefAnswers(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
	finish()
	prepareQuestionReview(t, f, repo, stream, false, nil)()
	for range 4 {
		fixturekit.Must(t, r.Pass(ctx))
	}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	th, err := repo.Thread(stream, intake.ReviewerAgent("resume"))
	fixturekit.Must(t, err)
	last := th.Turns[len(th.Turns)-1]
	if state.Value != intake.UnitReviewing || len(th.Turns) != 3 || !last.CompletedAt.IsZero() {
		t.Fatalf("no fresh review after answer: %+v, %+v", state, th)
	}
	origin, err := reviewtools.IdentityInPrompt(th.Turns[0].Request.Prompt)
	fixturekit.Must(t, err)
	fresh, err := reviewtools.IdentityInPrompt(last.Request.Prompt)
	fixturekit.Must(t, err)
	if execution.StaleReview(origin, fresh) != "" {
		t.Fatal("fresh review changed the candidate")
	}
	v := service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}
	prepareQuestionReview(t, f, repo, stream, false, &v)()
	fixturekit.Must(t, r.Pass(ctx))
	state, err = repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if state.Value != intake.UnitApproved {
		t.Fatalf("fresh review stranded in %+v", state)
	}
}

func TestReviewerQuestionPendingAnswerHoldsLaterVerdict(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewReviewFixture(t, "pending-later-verdict")
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
	ctx := context.Background()
	fixturekit.Must(t, r.Pass(ctx))
	prepareQuestionReview(t, f, repo, stream, true, nil)()
	fixturekit.Must(t, r.Pass(ctx))
	th, err := repo.Thread(stream, intake.ReviewerAgent("resume"))
	fixturekit.Must(t, err)
	req := th.Turns[0].Request
	req.ID, req.TurnID, req.At = "request_later-verdict", "later-verdict", f.Clock.Now()
	_, err = repo.EnqueueTurn(ctx, req)
	fixturekit.Must(t, err)
	v := service.UnitVerdict{Decision: "satisfactory", Summary: fixturekit.ReviewSummary}
	prepareQuestionReview(t, f, repo, stream, false, &v)()
	for _, answered := range []bool{false, true} {
		if answered {
			journey.ChiefRecordsAnswer(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
		}
		for range 2 {
			fixturekit.Must(t, r.Pass(ctx))
		}
		state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
		fixturekit.Must(t, err)
		if state.Value != intake.UnitWaiting {
			t.Fatalf("answered=%v advanced undelivered question: %+v", answered, state)
		}
	}
	fixturekit.Must(t, f.S.Answers(f.S.Cfg, repo).Pass(ctx))
	prepareQuestionReview(t, f, repo, stream, false, &v)()
	for range 3 {
		fixturekit.Must(t, r.Pass(ctx))
	}
	state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if state.Value != intake.UnitApproved {
		t.Fatalf("delivered answer stranded in %+v", state)
	}
}

func TestMasonDoneRefusesReviewerAndAmendmentWaits(t *testing.T) {
	t.Parallel()
	f, stream, repo := journey.NewReviewFixture(t, "done-other-waits")
	ctx := context.Background()
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, repo)}
	fixturekit.Must(t, r.Pass(ctx))
	prepareQuestionReview(t, f, repo, stream, true, nil)()
	fixturekit.Must(t, r.Pass(ctx))
	journey.ChiefAnswers(t, f, repo, stream, "1", "Use recorded checks.", "spec#1")
	for _, amendment := range []bool{false, true} {
		if amendment {
			state, err := repo.Workflow(stream, trace.UnitSubject("resume"))
			fixturekit.Must(t, err)
			_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-amendment-wait", Revision: 1, Project: repo.Project(), Workstream: stream, Unit: "resume", At: f.Clock.Now(), Actor: intake.MasonActor, Cause: "amendment_1"}, Subject: trace.UnitSubject("resume"), From: intake.UnitWaiting, To: intake.UnitWaiting, Reason: "Wait for the amendment"}})
			fixturekit.Must(t, err)
		}
		for _, agent := range []string{intake.MasonAgent("resume"), intake.ReviewerAgent("resume")} {
			scope := coreadapter.Scope{Project: string(f.Project), Workstream: string(stream), Unit: "resume", Thread: agent, Turn: questions.TurnID("1"), Role: intake.MasonRole}
			out, err := (&execution.MasonReports{}).Tool(repo, scope).Handle(ctx, json.RawMessage(`{"outcome":"Built and verified.","headline":"Work is complete","happened":"Built and verified the requested behavior."}`))
			fixturekit.Must(t, err)
			if strings.Contains(string(out), `"recorded":true`) {
				t.Fatalf("done accepted another role or amendment wait: %s", out)
			}
		}
	}
}
