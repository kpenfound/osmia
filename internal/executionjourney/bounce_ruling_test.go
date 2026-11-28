package executionjourney

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const bounceUnit = "acceptance-without-full-checks"
const bounceNote = "Verify the retry token against the durable checkpoint."
const bounceFinding = "Handle the missing retry token"
const bounceCheck = "dag://go/packages/tests/test?go-package=internal/trace&go-test=TestRetry"

type bounceFixture struct {
	f         *journey.ShedFixture
	graph     string
	findings  []api.ReviewFinding
	stream    config.WorkstreamID
	passed    *atomic.Int64
	delivered chan agent.Request
	resolved  chan error
}

func newBounceFixture(t *testing.T, history string, configure ...func(*journey.Options)) *bounceFixture {
	t.Helper()
	return prepareBounceFixture(t, history, true, configure...)
}

func prepareBounceFixture(t *testing.T, history string, build bool, configure ...func(*journey.Options)) *bounceFixture {
	t.Helper()
	graph := `{"version":2,"units":[{"id":"` + bounceUnit + `","title":"Prove the retry","task":"Prove the retry", "acceptance":["spec#1 holds, shown by TestRetry (new-test)", "spec#2 holds, shown by no duplicates (reviewer-judgement)"], "criteria":["spec#1", "spec#2"],"depends_on":[],"footprint":["internal.trace"]}]}`
	outcomes := fixturekit.Passing()
	failed := fixturekit.CheckOutcome{Result: api.CheckResult{ExitCode: 1, Output: fixturekit.FailedReport(bounceCheck)}}
	if history == "checks" {
		outcomes = []fixturekit.CheckOutcome{failed}
	}
	if history == "mixed" {
		outcomes = []fixturekit.CheckOutcome{failed, {Result: api.CheckResult{Output: fixturekit.PassedReport}}, failed}
	}
	checker := &fixturekit.FakeChecks{Outcomes: outcomes}
	passed, prepare := journey.CountingSchedule()
	f, _ := journey.NewMasonFixturePrepared(t, 1, graph, func(o *journey.Options) {
		prepare(o)
		o.ReviewChecks = checker
		for _, fn := range configure {
			fn(o)
		}
	})
	b := &bounceFixture{f: f, graph: graph, passed: passed, delivered: make(chan agent.Request, 8), resolved: make(chan error, 8)}
	var builds atomic.Int32
	var commanded, escalated atomic.Bool
	f.Engine.Mu.Lock()
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		result := &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Recorded", SessionDir: req.SessionDir, NumTurns: 1}
		if strings.HasPrefix(req.Name, intake.MasonAgent(bounceUnit)+"-") || strings.HasPrefix(req.Name, intake.ReviewerAgent(bounceUnit)+"-") {
			if strings.Contains(req.Prompt, bounceNote) {
				b.delivered <- req
				_, err := fixturekit.CallTool(ctx, tools, "ask", map[string]any{"question": "Please confirm the checkpoint evidence."})
				return result, err
			}
			if strings.HasPrefix(req.Name, intake.MasonAgent(bounceUnit)+"-") {
				n := builds.Add(1)
				if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), fixturekit.MasonWrote), []byte(fmt.Sprintf("package trace\n// candidate %d\n", n)), 0644); err != nil {
					return nil, err
				}
				return result, intakefixture.ReportDone("Built the retry")(ctx, req, tools)
			}
			body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": "material_findings", "summary": fixturekit.ReviewSummary, "findings": []api.ReviewFinding{{Severity: "material", Evidence: "Retry token missing", Action: bounceFinding}}})
			if err == nil && !strings.Contains(body, `"recorded":true`) {
				err = fmt.Errorf("verdict: %s", body)
			}
			return result, err
		}
		if strings.Contains(req.Prompt, "rule bounce now:") && commanded.CompareAndSwap(false, true) {
			decision := "review"
			if strings.Contains(req.Prompt, "rule bounce now: revise") {
				decision = "revise"
			}
			owner := strings.Contains(req.Prompt, "owner_decided")
			_ = intakefixture.DecideContest(f.Repository(), config.WorkstreamID(intakefixture.SessionStream(req)), bounceUnit, execution.UnitContest)
			body, err := fixturekit.CallTool(ctx, tools, execution.ResolveContestedTool, map[string]any{"unit": bounceUnit, "decision": decision, "note": bounceNote, "owner_decided": owner})
			if err == nil && !strings.Contains(body, `"recorded":true`) {
				err = fmt.Errorf("resolve_contested: %s", body)
			}
			if err == nil {
				st, e := f.Repository().Workflow(config.WorkstreamID(intakefixture.SessionStream(req)), trace.UnitSubject(bounceUnit))
				if e != nil {
					err = e
				} else if st.Value == intake.UnitContested {
					err = errors.New("tool reported recorded while still contested")
				}
			}
			b.resolved <- err
			return result, err
		}
		if strings.Contains(req.Prompt, "escalate bounce now") && escalated.CompareAndSwap(false, true) {
			_ = intakefixture.DecideContest(f.Repository(), config.WorkstreamID(intakefixture.SessionStream(req)), bounceUnit, execution.UnitContest)
			body, err := fixturekit.CallTool(ctx, tools, execution.ResolveContestedTool, map[string]any{"unit": bounceUnit, "decision": "escalate", "note": "The retry needs the owner's decision."})
			if err == nil && !strings.Contains(body, `"recorded":true`) {
				err = fmt.Errorf("escalate: %s", body)
			}
			b.resolved <- err
			return result, err
		}
		return result, nil
	}
	f.Engine.Mu.Unlock()
	if !build {
		return b
	}
	b.stream = seedBuilding(f, t, "bounce-"+history, graph)
	b.awaitUnit(t, intake.UnitContested)
	r := &journey.Reviewers{Masons: journey.NewMasonController(f.S, f.Repository())}
	result, ok, err := r.Driver().StoredResult(b.stream, bounceUnit, trace.WorkflowState{})
	fixturekit.Must(t, err)
	if !ok || result.Bounces != 3 {
		t.Fatalf("send-backs: %+v", result)
	}
	b.findings = result.Verdict.Findings
	if history == "checks" && result.Checks == 0 || history == "reviews" && result.Checks != 0 {
		t.Fatalf("wrong send-back source: %+v", result)
	}
	return b
}

func (b *bounceFixture) rule(t *testing.T, decision, entry string) {
	t.Helper()
	if entry == "owner" {
		_, err := b.f.C.RuleContested(context.Background(), b.stream, bounceUnit, decision, bounceNote)
		fixturekit.Must(t, err)
	} else {
		message := "rule bounce now: " + decision
		if entry == "owner_decided" {
			message += " owner_decided"
		}
		_, err := b.f.C.Send(context.Background(), b.stream, message)
		fixturekit.Must(t, err)
		fixturekit.Must(t, intakefixture.AwaitCheck(t, b.resolved))
	}
	st, err := b.f.Repository().Workflow(b.stream, trace.UnitSubject(bounceUnit))
	fixturekit.Must(t, err)
	if st.Value == intake.UnitContested {
		t.Fatalf("recorded ruling left %+v", st)
	}
}

func (b *bounceFixture) deliveredOnce(t *testing.T, decision string) {
	t.Helper()
	req := intakefixture.AwaitCheck(t, b.delivered)
	if !strings.Contains(req.Prompt, bounceNote) {
		t.Fatalf("prompt lacks ruling: %s", req.Prompt)
	}
	if decision == "revise" {
		if !strings.HasPrefix(req.Name, intake.MasonAgent(bounceUnit)+"-revise-") {
			t.Fatalf("next role: %s", req.Name)
		}
		if len(b.findings) == 0 {
			t.Fatal("fixture has no send-back findings")
		}
		findings, err := json.MarshalIndent(b.findings, "", "  ")
		fixturekit.Must(t, err)
		if !strings.Contains(req.Prompt, string(findings)) {
			t.Fatalf("revision %s lacks recorded findings %s", req.Name, findings)
		}

	}

	if decision == "review" && !strings.HasPrefix(req.Name, intake.ReviewerAgent(bounceUnit)+"-review-") {
		t.Fatalf("next role: %s", req.Name)
	}
	b.awaitUnit(t, intake.UnitWaiting)
	b.awaitPasses(t)
	b.assertOnce(t, decision)
	b.stop(t)
	b.f.Start(t)
	b.awaitPasses(t)
	b.assertOnce(t, decision)
	select {
	case req := <-b.delivered:
		t.Fatalf("duplicate delivered turn: %s", req.Name)
	default:
	}
}

func (b *bounceFixture) assertOnce(t *testing.T, decision string) {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](b.f.Repository(), b.stream)
	fixturekit.Must(t, err)
	n := 0
	for _, tr := range transitions {
		if tr.Subject == trace.UnitSubject(bounceUnit) && tr.From == intake.UnitContested && tr.To != intake.UnitContested {
			want := intake.UnitReviewing
			if decision == "revise" {
				want = intake.UnitImplementing
			}
			if tr.To != want {
				t.Fatalf("ruling moved to %s, wanted %s", tr.To, want)
			}
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d transitions out of contested", n)
	}
	role := intake.ReviewerAgent(bounceUnit)
	if decision == "revise" {
		role = intake.MasonAgent(bounceUnit)
	}
	th, err := b.f.Repository().Thread(b.stream, role)
	fixturekit.Must(t, err)
	n = 0
	for _, turn := range th.Turns {
		if strings.Contains(turn.Request.Prompt, bounceNote) && !strings.HasPrefix(turn.Request.TurnID, "answer_") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d applicable queued turns: %+v", n, th.Turns)
	}
}

func TestBounceRecoveryBypassesOtherTransitions(t *testing.T) {
	t.Parallel()
	unit := bounceUnit
	subject := trace.UnitSubject(unit)
	for _, transition := range []trace.Transition{
		{},
		{Subject: subject, From: intake.UnitReady, To: intake.UnitImplementing},
		{Subject: subject, From: intake.UnitReviewing, To: intake.UnitImplementing},
		{Subject: subject, From: intake.UnitContested, To: intake.UnitImplementing, Header: trace.Header{Cause: "move-1"}},
		{Subject: subject, From: intake.UnitContested, To: intake.UnitImplementing, Header: trace.Header{Cause: subject + "-mason-ruling-1"}},
		{Subject: trace.FeatureSubject, Header: trace.Header{Cause: "shed-round-1"}},
	} {
		m := &journey.Masons{}
		fixturekit.Must(t, m.Driver().ResumeBounceRevision(context.Background(), "", unit, transition))
		r := &journey.Reviewers{Masons: m}
		fixturekit.Must(t, r.Driver().ResumeContested(context.Background(), "", unit, trace.WorkflowState{}, transition))
	}
}

func TestBounceRulingsReachTheNextRole(t *testing.T) {
	t.Parallel()
	for _, history := range []string{"reviews", "checks", "mixed"} {
		for _, decision := range []string{"review", "revise"} {
			for _, entry := range []string{"owner", "chief", "owner_decided"} {
				t.Run(history+"/"+decision+"/"+entry, func(t *testing.T) {
					t.Parallel()
					b := newBounceFixture(t, history)
					defer b.stop(t)
					contest, _, err := execution.UnitContest(b.f.Repository(), b.stream, bounceUnit)
					fixturekit.Must(t, err)
					b.rule(t, decision, entry)
					ruling, found, err := execution.LatestContestedRuling(b.f.Repository(), b.stream, bounceUnit, 3)
					fixturekit.Must(t, err)
					by := "owner"
					if entry == "chief" {
						by = "chief of staff"
					}
					if !found || execution.ContestRuler(ruling) != by {
						t.Fatalf("ruling attribution: %+v", ruling)
					}
					if entry != "owner" {
						_, decisions, err := execution.ManagerDecisions(b.f.Repository(), b.stream, bounceUnit)
						fixturekit.Must(t, err)
						if len(decisions) != 1 || decisions[0].Contest != contest.ID || decisions[0].Decision != decision {
							t.Fatalf("chief attribution: %+v", decisions)
						}
					}
					b.deliveredOnce(t, decision)
					if entry != "owner" {
						_, decisions, err := execution.ManagerDecisions(b.f.Repository(), b.stream, bounceUnit)
						fixturekit.Must(t, err)
						if len(decisions) != 1 {
							t.Fatalf("duplicate chief attribution: %+v", decisions)
						}
					}
				})
			}
		}
	}
}

func TestBounceEscalationReachesTheOwnersInbox(t *testing.T) {
	t.Parallel()
	for _, history := range []string{"reviews", "checks", "mixed"} {
		t.Run(history, func(t *testing.T) {
			t.Parallel()
			b := newBounceFixture(t, history)
			defer b.stop(t)
			_, err := b.f.C.Send(context.Background(), b.stream, "escalate bounce now")
			fixturekit.Must(t, err)
			fixturekit.Must(t, intakefixture.AwaitCheck(t, b.resolved))
			entries := b.f.InboxEntries(t, b.stream, api.InboxContested)
			if len(entries) != 1 || entries[0].Unit != bounceUnit {
				t.Fatalf("owner inbox: %+v", entries)
			}
			_, decisions, err := execution.ManagerDecisions(b.f.Repository(), b.stream, bounceUnit)
			fixturekit.Must(t, err)
			if len(decisions) != 1 || decisions[0].Decision != execution.EscalateContest {
				t.Fatalf("escalation attribution: %+v", decisions)
			}
		})
	}
}

func TestBounceChiefDecisionValidation(t *testing.T) {
	t.Parallel()
	for _, history := range []string{"reviews", "checks", "mixed"} {
		t.Run(history, func(t *testing.T) {
			t.Parallel()
			b := newBounceFixture(t, history)
			defer b.stop(t)
			repo := b.f.Repository()
			contest, held, err := execution.UnitContest(repo, b.stream, bounceUnit)
			fixturekit.Must(t, err)
			if !held || !execution.BounceContest(contest, bounceUnit) {
				t.Fatalf("send-back contest: %+v", contest)
			}
			decision := func(id string) trace.Document {
				return execution.ManagerDecisionDocument(repo, b.stream, bounceUnit, id, "escalate", "Investigate the checks.", "events_1", fixturekit.DemoStart)
			}

			earlier := decision(execution.CheckDocumentID(bounceUnit, 3) + "-contested")
			fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{earlier}))
			d := decision(contest.ID)
			fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{d}))

			if err := repo.RecordDocuments(context.Background(), []trace.Document{d}); !errors.Is(err, trace.ErrConflict) {
				t.Fatalf("repeated decision write: %v", err)
			}
		})
	}
}

func TestBounceOwnerDecisionAnswersChiefEscalation(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{"review", "revise"} {
		t.Run(decision, func(t *testing.T) {
			t.Parallel()
			b := newBounceFixture(t, "mixed")
			defer b.stop(t)
			_, err := b.f.C.Send(context.Background(), b.stream, "escalate bounce now")
			fixturekit.Must(t, err)
			fixturekit.Must(t, intakefixture.AwaitCheck(t, b.resolved))
			b.rule(t, decision, "owner_decided")
			b.deliveredOnce(t, decision)
			docs, decisions, err := execution.ManagerDecisions(b.f.Repository(), b.stream, bounceUnit)
			fixturekit.Must(t, err)
			if len(decisions) != 2 || docs[0].ID != docs[1].ID || docs[1].Revision != 2 || decisions[1].Decision != decision {
				t.Fatalf("escalation and owner ruling: %+v %+v", docs, decisions)
			}
		})
	}
}

func TestSavedBounceRulingsRecoverAndDeliverOnce(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{"review", "revise"} {
		for _, retry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retry=%t", decision, retry), func(t *testing.T) {
				t.Parallel()
				b := prepareBounceFixture(t, "reviews", false)
				base := strings.TrimSpace(fixturekit.DemoGit(t, b.f.Clone, "-C", b.f.Clone, "rev-parse", "HEAD"))
				stream, repo := journey.SeedBuild(t, b.f, "saved-bounce", b.graph, config.WorkspacesGit, base)
				b.stream = stream
				journey.SeedChecking(t, b.f, repo, stream, bounceUnit, "Built the retry")
				r := &journey.Reviewers{Masons: journey.NewMasonController(b.f.S, repo)}
				_, identity, err := r.Masons.CandidateEvidence(context.Background(), stream, bounceUnit)
				fixturekit.Must(t, err)

				b.f.S.Cfg.Shed.MaxBounces = 1
				c := &journey.Checkers{Masons: r.Masons}
				state, err := repo.Workflow(stream, trace.UnitSubject(bounceUnit))
				fixturekit.Must(t, err)
				fixturekit.Must(t, c.Driver().Fail(context.Background(), stream, bounceUnit, state, execution.UnitCheckRun{Unit: bounceUnit, Run: 1, Candidate: identity.Candidate.Revision, Base: identity.Candidate.BaseRevision, DiffSHA256: identity.DiffSHA256, Status: checkrun.ChecksFailed, Selection: &api.CheckSelection{Mode: checkrun.SelectionFull}, Failed: []string{bounceCheck}}))
				result, ok, err := r.Driver().StoredResult(stream, bounceUnit, trace.WorkflowState{})
				fixturekit.Must(t, err)
				b.findings = result.Verdict.Findings
				if !ok || intakefixture.UnitState(t, repo, stream, bounceUnit) != intake.UnitContested {
					t.Fatal("missing held candidate")
				}
				if _, checked, err := execution.LatestRunFor(repo, stream, bounceUnit, identity); err != nil || checked {
					t.Fatalf("candidate already has checks: %t %v", checked, err)
				}

				ruling := api.ContestedRuling{Decision: decision, Note: bounceNote, Bounces: result.Bounces, Candidate: result.Identity.Candidate.Revision}
				data, _ := json.Marshal(ruling)
				doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: fmt.Sprintf("%s-ruling-%d", trace.UnitSubject(bounceUnit), result.Bounces), Revision: 1, Project: repo.Project(), Workstream: b.stream, Unit: bounceUnit, At: fixturekit.DemoStart, Actor: intake.OwnerActor, Cause: execution.ReviewDocument(bounceUnit)}, Path: execution.ContestedRulingPath(bounceUnit, result.Bounces), Content: string(data)}
				fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{doc}))
				if retry {
					other := "review"
					if decision == "review" {
						other = "revise"
					}
					out, apiErr := recordContestedRuling(b.f.S, context.Background(), repo, b.stream, bounceUnit, api.ContestedRulingRequest{Decision: other, Note: "A later request must preserve the saved decision."}, intake.OwnerActor)
					if apiErr != nil {
						t.Fatal(apiErr)
					}
					if out.Ruling.Decision != decision || out.Ruling.Note != bounceNote {
						t.Fatalf("retry changed a recorded ruling: %+v", out)
					}
				}
				fixturekit.Must(t, repo.Close())
				b.f.Start(t)
				defer b.stop(t)
				b.deliveredOnce(t, decision)
				if decision == "review" {
					runs, err := execution.CheckRuns(b.f.Repository(), stream)
					fixturekit.Must(t, err)
					if len(runs) != 1 || runs[0].Status != checkrun.ChecksPassed {
						t.Fatalf("review skipped required checks: %+v", runs)
					}
				}

			})
		}
	}
}

func TestBounceRulingKeysAreBounded(t *testing.T) {
	t.Parallel()
	c := journey.NewContestFixture(t, "bounce-keys")
	for _, unit := range []string{bounceUnit, strings.Repeat("u", 64), strings.Repeat("u", 128)} {
		contest := trace.UnitSubject(unit) + "-contested-checks-7-12"
		d := execution.ManagerDecisionDocument(c.Repo, c.Stream, unit, contest, "escalate", "Investigate the checks.", "events_1", fixturekit.DemoStart)
		if len(d.ID) > 128 {
			t.Fatalf("unbounded decision key: %s", d.ID)
		}
		unbounded := d
		unbounded.ID = trace.UnitSubject(unit) + "-chief-" + contest
		unbounded.Path = "units/" + trace.UnitSubject(unit) + "/chief-" + contest + ".json"
		if len(unbounded.ID) > 128 {
			if err := c.Repo.RecordDocuments(context.Background(), []trace.Document{unbounded}); err == nil {
				t.Fatal("trace accepted an oversized chief decision key")
			}
		}
		fixturekit.Must(t, c.Repo.RecordDocuments(context.Background(), []trace.Document{d}))
		_, decisions, err := execution.ManagerDecisions(c.Repo, c.Stream, unit)
		fixturekit.Must(t, err)
		if len(decisions) != 1 || decisions[0].Contest != contest {
			t.Fatalf("decisions: %+v", decisions)
		}
	}
}

func TestBounceRulingAndChiefDecisionCommitTogether(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{"review", "revise"} {
		t.Run(decision, func(t *testing.T) {
			t.Parallel()
			var inject atomic.Bool
			b := newBounceFixture(t, "mixed", func(o *journey.Options) {
				o.Boundary = func(_ context.Context, step string) error {
					if inject.Load() && strings.HasPrefix(step, "bounce-ruling-committed:") {
						return errors.New("interrupted after ruling commit")
					}
					return nil
				}
			})
			defer b.stop(t)
			commits := make(chan trace.Commit, 16)
			b.f.Repository().Observe(func(c trace.Commit) {
				if slices.ContainsFunc(c.Paths, func(p string) bool { return strings.HasSuffix(p, "/ruling-3.json") }) {
					commits <- c
				}
			})
			inject.Store(true)
			b.rule(t, decision, "chief")
			commit := intakefixture.AwaitCheck(t, commits)
			if commit.Workflow == nil || !slices.ContainsFunc(commit.Paths, func(p string) bool { return strings.Contains(p, "/chief-") }) {
				t.Fatalf("ruling split from chief decision or move: %+v", commit)
			}
			b.deliveredOnce(t, decision)
			_, decisions, err := execution.ManagerDecisions(b.f.Repository(), b.stream, bounceUnit)
			fixturekit.Must(t, err)
			if len(decisions) != 1 {
				t.Fatalf("chief decisions: %+v", decisions)
			}
		})
	}
}

func TestBounceRulingFailureLeavesTheContestAvailable(t *testing.T) {
	t.Parallel()
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			t.Parallel()
			var inject atomic.Bool
			var b *bounceFixture
			b = newBounceFixture(t, "mixed", func(o *journey.Options) {
				o.Boundary = func(ctx context.Context, step string) error {
					if !strings.HasPrefix(step, "bounce-ruling-recording:") || !inject.CompareAndSwap(true, false) {
						return nil
					}
					if !conflict {
						return errors.New("ruling write unavailable")
					}

					repo := b.f.Repository()
					state, err := repo.Workflow(b.stream, trace.UnitSubject(bounceUnit))
					if err != nil {
						return err
					}
					h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-concurrent-1", Revision: 1, Project: repo.Project(), Workstream: b.stream, At: fixturekit.DemoStart, Actor: execution.ReviewerActor, Cause: execution.ReviewDocument(bounceUnit), Unit: bounceUnit}
					_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(bounceUnit), From: intake.UnitContested, To: intake.UnitContested, Reason: "concurrent contest update"}})
					return err
				}
			})
			defer b.stop(t)
			inject.Store(true)
			_, err := b.f.C.RuleContested(context.Background(), b.stream, bounceUnit, "revise", bounceNote)
			if err == nil {
				t.Fatal("injected write failure or conflict was reported recorded")
			}
			_, found, e := execution.LatestContestedRuling(b.f.Repository(), b.stream, bounceUnit, 3)
			fixturekit.Must(t, e)
			if found {
				t.Fatal("failed move left a ruling")
			}
			b.rule(t, "revise", "owner")

			b.deliveredOnce(t, "revise")
		})
	}
}

func TestBounceRulingReconcilesAControllersStaleContest(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{"review", "revise"} {
		t.Run(decision, func(t *testing.T) {
			t.Parallel()
			b := newBounceFixture(t, "mixed")
			defer b.stop(t)
			repo := b.f.Repository()
			stale, err := repo.Workflow(b.stream, trace.UnitSubject(bounceUnit))
			fixturekit.Must(t, err)
			b.rule(t, decision, "owner")

			b.f.Clock.Now()
			r := &journey.Reviewers{Masons: journey.NewMasonController(b.f.S, repo)}
			fixturekit.Must(t, r.Driver().ResumeContested(context.Background(), b.stream, bounceUnit, stale))
			b.deliveredOnce(t, decision)
		})
	}
}

func TestBounceRulingRecoveryRejectsADifferentTransition(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{"review", "revise"} {
		for _, field := range []string{"contest", "candidate", "send-backs", "decision", "note"} {
			t.Run(decision+"/"+field, func(t *testing.T) {
				t.Parallel()
				b := newBounceFixture(t, "mixed")
				b.stop(t)
				repo, err := trace.Open(b.f.S.Cfg.Root, config.Project{ID: b.f.Project, Clone: b.f.Clone})
				fixturekit.Must(t, err)
				defer repo.Close()
				r := &journey.Reviewers{Masons: journey.NewMasonController(b.f.S, repo)}
				state, err := repo.Workflow(b.stream, trace.UnitSubject(bounceUnit))
				fixturekit.Must(t, err)
				result, ok, err := r.Driver().StoredResult(b.stream, bounceUnit, state)
				fixturekit.Must(t, err)
				if !ok {
					t.Fatal("missing send-back result")
				}
				ruling := api.ContestedRuling{Decision: decision, Note: bounceNote, Bounces: result.Bounces, Candidate: result.Identity.Candidate.Revision}
				to := map[string]string{"review": intake.UnitReviewing, "revise": intake.UnitImplementing}[decision]
				id := fmt.Sprintf("%s-%s-ruling-%d", trace.UnitSubject(bounceUnit), to, result.Bounces)
				reason := fmt.Sprintf("owner ruled %s on contested candidate %s after %d send-backs: %s", decision, ruling.Candidate, ruling.Bounces, ruling.Note)
				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: b.stream, Unit: bounceUnit, At: b.f.Clock.Now(), Actor: intake.OwnerActor, Cause: fmt.Sprintf("%s-ruling-%d", trace.UnitSubject(bounceUnit), result.Bounces)}
				recorded := trace.Transition{Header: h, Subject: trace.UnitSubject(bounceUnit), From: intake.UnitContested, To: to, Reason: reason}
				switch field {
				case "contest":
					recorded.Cause = "another-contest-ruling"
				case "candidate":
					recorded.Reason = strings.Replace(reason, ruling.Candidate, "another-candidate", 1)
				case "send-backs":
					recorded.Reason = strings.Replace(reason, fmt.Sprintf("after %d send-backs", ruling.Bounces), "after 99 send-backs", 1)
				case "decision":
					recorded.To = map[string]string{intake.UnitReviewing: intake.UnitImplementing, intake.UnitImplementing: intake.UnitReviewing}[to]
				case "note":
					recorded.Reason = strings.Replace(reason, bounceNote, "A different direction.", 1)
				}

				_, err = repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: recorded})
				fixturekit.Must(t, err)
				err = r.Driver().ApplyBounceRuling(context.Background(), b.stream, bounceUnit, state, result, ruling, nil)
				if !errors.Is(err, trace.ErrConflict) {
					t.Fatalf("different %s reconciled: %v", field, err)
				}
				transitions, err := trace.Read[trace.Transition](repo, b.stream)
				fixturekit.Must(t, err)
				n := 0
				for _, tr := range transitions {
					if tr.ID == id {
						n++
					}
				}
				if n != 1 {
					t.Fatalf("conflict wrote %d transitions", n)
				}
			})
		}
	}
}

func TestBounceRulingRecoveryAtRoleDeliveryBoundaries(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{"review", "revise"} {
		for _, boundary := range []string{"bounce-ruling-committed:", "unit-role-enqueue:", "unit-role-queued:"} {
			t.Run(decision+"/"+boundary, func(t *testing.T) {
				t.Parallel()
				var armed atomic.Bool
				var hit atomic.Bool
				reached := make(chan struct{}, 1)
				b := newBounceFixture(t, "mixed", func(o *journey.Options) {
					o.Boundary = func(ctx context.Context, step string) error {
						if !armed.Load() {
							return nil
						}
						selected := strings.HasPrefix(step, boundary)

						if boundary == "bounce-ruling-committed:" && strings.HasPrefix(step, "unit-role-enqueue:") {
							<-ctx.Done()
							return ctx.Err()
						}
						if selected {
							if hit.CompareAndSwap(false, true) {
								reached <- struct{}{}
							}
							<-ctx.Done()
							return ctx.Err()
						}
						return nil
					}
				})
				defer b.stop(t)
				armed.Store(true)

				_, err := b.f.C.Send(context.Background(), b.stream, "rule bounce now: "+decision)
				fixturekit.Must(t, err)
				intakefixture.AwaitCheck(t, reached)
				b.stop(t)
				armed.Store(false)
				b.f.Start(t)
				b.deliveredOnce(t, decision)
				_, decisions, err := execution.ManagerDecisions(b.f.Repository(), b.stream, bounceUnit)
				fixturekit.Must(t, err)
				if len(decisions) != 1 || decisions[0].Decision != decision {
					t.Fatalf("chief attribution after interruption: %+v", decisions)
				}
			})
		}
	}
}

func (b *bounceFixture) awaitUnit(t *testing.T, want string) {
	t.Helper()
	wait, close := journey.ServiceChanges(t, b.f.S)
	defer close()
	until := time.Now().Add(2 * time.Minute)
	for {
		state, err := b.f.Repository().Workflow(b.stream, trace.UnitSubject(bounceUnit))
		fixturekit.Must(t, err)
		if state.Value == want {
			return
		}
		if time.Now().After(until) {
			t.Fatalf("unit stayed %s, wanted %s", state.Value, want)
		}
		wait()
	}
}

func (b *bounceFixture) awaitPasses(t *testing.T) {
	t.Helper()
	intakefixture.AwaitFurtherPasses(t, b.passed, 3)
}

func (b *bounceFixture) stop(t *testing.T) {
	t.Helper()
	b.f.C.Close()
	stopped := make(chan error, 1)
	go func() { stopped <- b.f.S.Close() }()
	select {
	case err := <-stopped:
		fixturekit.Must(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("service did not stop")
	}
}

func TestOrdinarySendBackRecoversFindingsAfterRestart(t *testing.T) {
	t.Parallel()
	for _, history := range []string{"reviews", "checks"} {
		for _, recoverer := range []string{"service", "mason-finish"} {
			t.Run(history+"/"+recoverer, func(t *testing.T) {
				t.Parallel()
				var interrupted atomic.Bool
				interrupted.Store(history == "checks")
				reached := make(chan struct{}, 1)
				b := prepareBounceFixture(t, history, false, func(o *journey.Options) {
					o.Boundary = func(ctx context.Context, step string) error {
						if interrupted.Load() && strings.HasPrefix(step, "unit-role-enqueue:") {
							select {
							case reached <- struct{}{}:
							default:
							}
							<-ctx.Done()
							return ctx.Err()
						}
						return nil
					}
				})
				defer b.stop(t)
				b.f.Engine.Mu.Lock()
				original := b.f.Engine.Turns["*"]
				b.f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
					if strings.HasPrefix(req.Name, intake.MasonAgent(bounceUnit)+"-revise-") {
						b.delivered <- req
						_, err := fixturekit.CallTool(ctx, tools, "ask", map[string]any{"question": "Please confirm the send-back evidence."})
						return &agent.Result{ClaudeID: "ordinary-revision", ResultText: "Asked", SessionDir: req.SessionDir, NumTurns: 1}, err
					}
					result, err := original(ctx, req, turn, tools)
					if strings.HasPrefix(req.Name, intake.ReviewerAgent(bounceUnit)+"-") {
						interrupted.Store(true)
					}
					return result, err
				}
				b.f.Engine.Mu.Unlock()
				b.stream = seedBuilding(b.f, t, "ordinary-"+history+"-"+recoverer, b.graph)
				intakefixture.AwaitCheck(t, reached)
				state, err := b.f.Repository().Workflow(b.stream, trace.UnitSubject(bounceUnit))
				fixturekit.Must(t, err)
				if state.Value != intake.UnitImplementing {
					t.Fatalf("send-back not committed: %+v", state)
				}
				r := &journey.Reviewers{Masons: journey.NewMasonController(b.f.S, b.f.Repository())}
				result, ok, err := r.Driver().StoredResult(b.stream, bounceUnit, state)
				fixturekit.Must(t, err)
				if !ok || result.Bounces != 1 || result.Bounces >= b.f.S.Cfg.Shed.MaxBounces || result.Checks > 0 != (history == "checks") {
					t.Fatalf("not an ordinary %s send-back: %+v", history, result)
				}
				th, err := b.f.Repository().Thread(b.stream, intake.MasonAgent(bounceUnit))
				fixturekit.Must(t, err)
				if len(th.Turns) != 1 || th.Turns[0].Response.Result.Outcome == nil || th.Turns[0].Response.Result.Outcome.Status != execution.MasonDone {
					t.Fatalf("expected only the old done turn before shutdown: %+v", th.Turns)
				}
				b.stop(t)
				interrupted.Store(false)
				if recoverer == "mason-finish" {

					repo, err := trace.Open(b.f.S.Cfg.Root, b.f.S.Cfg.Project)
					fixturekit.Must(t, err)
					m := journey.NewMasonController(b.f.S, repo)
					building, found, err := m.Read(b.stream)
					fixturekit.Must(t, err)
					if !found {
						t.Fatal("missing building workstream")
					}
					moved, blocked, err := m.Finish(context.Background(), building, bounceUnit)
					fixturekit.Must(t, err)
					if moved || blocked {
						t.Fatalf("mason reused old done: moved %t, blocked %t", moved, blocked)
					}
					fixturekit.Must(t, repo.Close())
				}
				b.f.Start(t)
				req := intakefixture.AwaitCheck(t, b.delivered)
				findings, err := json.MarshalIndent(result.Verdict.Findings, "", "  ")
				fixturekit.Must(t, err)
				if !strings.Contains(req.Prompt, string(findings)) || !strings.Contains(req.Prompt, result.Identity.Candidate.Revision) || strings.Contains(req.Prompt, bounceNote) {
					t.Fatalf("wrong recovered findings: %s", req.Prompt)
				}
				b.awaitUnit(t, intake.UnitWaiting)
				assertOnce := func() {
					t.Helper()
					thread, err := b.f.Repository().Thread(b.stream, intake.MasonAgent(bounceUnit))
					fixturekit.Must(t, err)
					if len(thread.Turns) != 2 || thread.Turns[1].Request.TurnID != req.Name {
						t.Fatalf("expected initial done and exactly one findings turn: %+v", thread.Turns)
					}
					transitions, err := trace.Read[trace.Transition](b.f.Repository(), b.stream)
					fixturekit.Must(t, err)
					n := 0
					for _, tr := range transitions {
						if tr.Subject == trace.UnitSubject(bounceUnit) && tr.From == intake.UnitImplementing && tr.To == execution.UnitChecking {
							n++
						}
					}
					if n != 1 {
						t.Fatalf("unchanged candidate returned to checking: %d transitions", n)
					}
					runs, err := execution.CheckRuns(b.f.Repository(), b.stream)
					fixturekit.Must(t, err)
					if len(runs) != 1 {
						t.Fatalf("unchanged candidate rechecked: %+v", runs)
					}
				}
				b.awaitPasses(t)
				assertOnce()
				b.stop(t)
				b.f.Start(t)
				b.awaitPasses(t)
				assertOnce()
				select {
				case duplicate := <-b.delivered:
					t.Fatalf("duplicate findings delivery: %s", duplicate.Name)
				default:
				}
			})
		}
	}
}
