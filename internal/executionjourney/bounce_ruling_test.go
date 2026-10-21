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
	f         *shedFixture
	graph     string
	findings  []ReviewFinding
	stream    config.WorkstreamID
	passed    *atomic.Int64
	delivered chan agent.Request
	resolved  chan error
}

func newBounceFixture(t *testing.T, history string, configure ...func(*Options)) *bounceFixture {
	t.Helper()
	return prepareBounceFixture(t, history, true, configure...)
}

func prepareBounceFixture(t *testing.T, history string, build bool, configure ...func(*Options)) *bounceFixture {
	t.Helper()
	graph := `{"version":1,"units":[{"id":"` + bounceUnit + `","title":"Prove the retry","addresses":[{"criterion":"spec#1","proof":{"kind":"new-test","name":"TestRetry"}},{"criterion":"spec#2","proof":{"kind":"reviewer-judgement","name":"no duplicates"}}],"depends_on":[],"footprint":["internal.trace"]}]}`
	outcomes := passing()
	failed := checkOutcome{Result: CheckResult{ExitCode: 1, Output: failedReport(bounceCheck)}}
	if history == "checks" {
		outcomes = []checkOutcome{failed}
	}
	if history == "mixed" {
		outcomes = []checkOutcome{failed, {Result: CheckResult{Output: passedReport}}, failed}
	}
	checks := &fakeChecks{Outcomes: outcomes}
	passed, prepare := countingSchedule()
	f, _ := newMasonFixturePrepared(t, 1, graph, func(o *Options) {
		prepare(o)
		o.reviewChecks = checks
		for _, fn := range configure {
			fn(o)
		}
	})
	b := &bounceFixture{f: f, graph: graph, passed: passed, delivered: make(chan agent.Request, 8), resolved: make(chan error, 8)}
	var builds atomic.Int32
	var commanded, escalated atomic.Bool
	f.engine.Mu.Lock()
	f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, _ *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		result := &agent.Result{ClaudeID: "session-" + req.Name, ResultText: "Recorded", SessionDir: req.SessionDir, NumTurns: 1}
		if strings.HasPrefix(req.Name, masonAgent(bounceUnit)+"-") || strings.HasPrefix(req.Name, reviewerAgent(bounceUnit)+"-") {
			if strings.Contains(req.Prompt, bounceNote) {
				b.delivered <- req
				_, err := callTool(ctx, tools, "ask", map[string]any{"question": "Please confirm the checkpoint evidence."})
				return result, err
			}
			if strings.HasPrefix(req.Name, masonAgent(bounceUnit)+"-") {
				n := builds.Add(1)
				if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), masonWrote), []byte(fmt.Sprintf("package trace\n// candidate %d\n", n)), 0644); err != nil {
					return nil, err
				}
				return result, reportDone("Built the retry")(ctx, req, tools)
			}
			body, err := callTool(ctx, tools, verdictTool, map[string]any{"decision": "material_findings", "summary": reviewSummary, "findings": []ReviewFinding{{Severity: "material", Evidence: "Retry token missing", Action: bounceFinding}}})
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
			body, err := callTool(ctx, tools, resolveContestedTool, map[string]any{"unit": bounceUnit, "decision": decision, "note": bounceNote, "owner_decided": owner})
			if err == nil && !strings.Contains(body, `"recorded":true`) {
				err = fmt.Errorf("resolve_contested: %s", body)
			}
			if err == nil {
				st, e := f.repository().Workflow(config.WorkstreamID(sessionStream(req)), trace.UnitSubject(bounceUnit))
				if e != nil {
					err = e
				} else if st.Value == UnitContested {
					err = errors.New("tool reported recorded while still contested")
				}
			}
			b.resolved <- err
			return result, err
		}
		if strings.Contains(req.Prompt, "escalate bounce now") && escalated.CompareAndSwap(false, true) {
			body, err := callTool(ctx, tools, resolveContestedTool, map[string]any{"unit": bounceUnit, "decision": "escalate", "note": "The retry needs the owner's decision."})
			if err == nil && !strings.Contains(body, `"recorded":true`) {
				err = fmt.Errorf("escalate: %s", body)
			}
			b.resolved <- err
			return result, err
		}
		return result, nil
	}
	f.engine.Mu.Unlock()
	if !build {
		return b
	}
	b.stream = f.seedBuilding(t, "bounce-"+history, graph)
	b.awaitUnit(t, UnitContested)
	r := &reviewers{masons: newMasonController(f.s, f.repository())}
	result, ok, err := r.storedResult(b.stream, bounceUnit, trace.WorkflowState{})
	must(t, err)
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
		_, err := b.f.c.RuleContested(context.Background(), b.stream, bounceUnit, decision, bounceNote)
		must(t, err)
	} else {
		message := "rule bounce now: " + decision
		if entry == "owner_decided" {
			message += " owner_decided"
		}
		_, err := b.f.c.Send(context.Background(), b.stream, message)
		must(t, err)
		must(t, awaitCheck(t, b.resolved))
	}
	st, err := b.f.repository().Workflow(b.stream, trace.UnitSubject(bounceUnit))
	must(t, err)
	if st.Value == UnitContested {
		t.Fatalf("recorded ruling left %+v", st)
	}
}

func (b *bounceFixture) deliveredOnce(t *testing.T, decision string) {
	t.Helper()
	req := awaitCheck(t, b.delivered)
	if !strings.Contains(req.Prompt, bounceNote) {
		t.Fatalf("prompt lacks ruling: %s", req.Prompt)
	}
	if decision == "revise" {
		if !strings.HasPrefix(req.Name, masonAgent(bounceUnit)+"-revise-") {
			t.Fatalf("next role: %s", req.Name)
		}
		if len(b.findings) == 0 {
			t.Fatal("fixture has no send-back findings")
		}
		findings, err := json.MarshalIndent(b.findings, "", "  ")
		must(t, err)
		if !strings.Contains(req.Prompt, string(findings)) {
			t.Fatalf("revision %s lacks recorded findings %s", req.Name, findings)
		}

	}

	if decision == "review" && !strings.HasPrefix(req.Name, reviewerAgent(bounceUnit)+"-review-") {
		t.Fatalf("next role: %s", req.Name)
	}
	b.awaitUnit(t, UnitWaiting)
	b.awaitPasses(t)
	b.assertOnce(t, decision)
	b.stop(t)
	b.f.start(t)
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
	transitions, err := trace.Read[trace.Transition](b.f.repository(), b.stream)
	must(t, err)
	n := 0
	for _, tr := range transitions {
		if tr.Subject == trace.UnitSubject(bounceUnit) && tr.From == UnitContested && tr.To != UnitContested {
			want := UnitReviewing
			if decision == "revise" {
				want = UnitImplementing
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
	role := reviewerAgent(bounceUnit)
	if decision == "revise" {
		role = masonAgent(bounceUnit)
	}
	th, err := b.f.repository().Thread(b.stream, role)
	must(t, err)
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
	unit := bounceUnit
	subject := trace.UnitSubject(unit)
	for _, transition := range []trace.Transition{
		{},
		{Subject: subject, From: UnitReady, To: UnitImplementing},
		{Subject: subject, From: UnitReviewing, To: UnitImplementing},
		{Subject: subject, From: UnitContested, To: UnitImplementing, Header: trace.Header{Cause: "move-1"}},
		{Subject: subject, From: UnitContested, To: UnitImplementing, Header: trace.Header{Cause: subject + "-mason-ruling-1"}},
		{Subject: trace.FeatureSubject, Header: trace.Header{Cause: "shed-round-1"}},
	} {
		m := &masons{}
		must(t, m.resumeBounceRevision(context.Background(), "", unit, transition))
		r := &reviewers{masons: m}
		must(t, r.resumeContested(context.Background(), "", unit, trace.WorkflowState{}, transition))
	}
}

func TestBounceRulingsReachTheNextRole(t *testing.T) {
	for _, history := range []string{"reviews", "checks", "mixed"} {
		for _, decision := range []string{"review", "revise"} {
			for _, entry := range []string{"owner", "chief", "owner_decided"} {
				t.Run(history+"/"+decision+"/"+entry, func(t *testing.T) {
					t.Parallel()
					b := newBounceFixture(t, history)
					defer b.stop(t)
					contest, _, err := unitContest(b.f.repository(), b.stream, bounceUnit)
					must(t, err)
					b.rule(t, decision, entry)
					ruling, found, err := latestContestedRuling(b.f.repository(), b.stream, bounceUnit, 3)
					must(t, err)
					by := "owner"
					if entry == "chief" {
						by = "chief of staff"
					}
					if !found || contestRuler(ruling) != by {
						t.Fatalf("ruling attribution: %+v", ruling)
					}
					if entry != "owner" {
						_, decisions, err := chiefDecisions(b.f.repository(), b.stream, bounceUnit)
						must(t, err)
						if len(decisions) != 1 || decisions[0].Contest != contest.ID || decisions[0].Decision != decision {
							t.Fatalf("chief attribution: %+v", decisions)
						}
					}
					b.deliveredOnce(t, decision)
					if entry != "owner" {
						_, decisions, err := chiefDecisions(b.f.repository(), b.stream, bounceUnit)
						must(t, err)
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
	for _, history := range []string{"reviews", "checks", "mixed"} {
		t.Run(history, func(t *testing.T) {
			t.Parallel()
			b := newBounceFixture(t, history)
			defer b.stop(t)
			_, err := b.f.c.Send(context.Background(), b.stream, "escalate bounce now")
			must(t, err)
			must(t, awaitCheck(t, b.resolved))
			entries := b.f.inboxEntries(t, b.stream, InboxContested)
			if len(entries) != 1 || entries[0].Unit != bounceUnit {
				t.Fatalf("owner inbox: %+v", entries)
			}
			_, decisions, err := chiefDecisions(b.f.repository(), b.stream, bounceUnit)
			must(t, err)
			if len(decisions) != 1 || decisions[0].Decision != escalateContest {
				t.Fatalf("escalation attribution: %+v", decisions)
			}
		})
	}
}

func TestBounceChiefDecisionValidation(t *testing.T) {
	for _, history := range []string{"reviews", "checks", "mixed"} {
		t.Run(history, func(t *testing.T) {
			t.Parallel()
			b := newBounceFixture(t, history)
			defer b.stop(t)
			repo := b.f.repository()
			contest, held, err := unitContest(repo, b.stream, bounceUnit)
			must(t, err)
			if !held || !bounceContest(contest, bounceUnit) {
				t.Fatalf("send-back contest: %+v", contest)
			}
			legacy := func(id string) trace.Document {
				d := chiefDecisionDocument(repo, b.stream, bounceUnit, id, "escalate", "Investigate the checks.", "events_1", demoStart)
				d.ID = trace.UnitSubject(bounceUnit) + "-chief-" + id
				d.Path = "units/" + trace.UnitSubject(bounceUnit) + "/chief-" + id + ".json"
				return d
			}

			earlier := legacy(checkDocumentID(bounceUnit, 3) + "-contested")
			must(t, repo.RecordDocuments(context.Background(), []trace.Document{earlier}))
			d := legacy(contest.ID)
			must(t, repo.RecordDocuments(context.Background(), []trace.Document{d}))
			t.Logf("contest %s, chief ID %s (%d bytes), path %s: accepted after %s", contest.ID, d.ID, len(d.ID), d.Path, earlier.Path)

			err = repo.RecordDocuments(context.Background(), []trace.Document{d})
			if !errors.Is(err, trace.ErrConflict) {
				t.Fatalf("repeated decision write: %v", err)
			}
			t.Logf("repeated decision write: %v", err)
		})
	}
}

func TestBounceOwnerDecisionAnswersChiefEscalation(t *testing.T) {
	for _, decision := range []string{"review", "revise"} {
		t.Run(decision, func(t *testing.T) {
			t.Parallel()
			b := newBounceFixture(t, "mixed")
			defer b.stop(t)
			_, err := b.f.c.Send(context.Background(), b.stream, "escalate bounce now")
			must(t, err)
			must(t, awaitCheck(t, b.resolved))
			b.rule(t, decision, "owner_decided")
			b.deliveredOnce(t, decision)
			docs, decisions, err := chiefDecisions(b.f.repository(), b.stream, bounceUnit)
			must(t, err)
			if len(decisions) != 2 || docs[0].ID != docs[1].ID || docs[1].Revision != 2 || decisions[1].Decision != decision {
				t.Fatalf("escalation and owner ruling: %+v %+v", docs, decisions)
			}
		})
	}
}

func TestSavedBounceRulingsRecoverAndDeliverOnce(t *testing.T) {
	for _, decision := range []string{"review", "revise"} {
		for _, retry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retry=%t", decision, retry), func(t *testing.T) {
				t.Parallel()
				b := prepareBounceFixture(t, "reviews", false)
				base := strings.TrimSpace(demoGit(t, b.f.clone, "-C", b.f.clone, "rev-parse", "HEAD"))
				stream, repo := seedBuild(t, b.f, "saved-bounce", b.graph, config.WorkspacesGit, base)
				b.stream = stream
				seedChecking(t, b.f, repo, stream, bounceUnit, "Built the retry")
				r := &reviewers{masons: newMasonController(b.f.s, repo)}
				_, identity, err := r.candidateEvidence(context.Background(), stream, bounceUnit)
				must(t, err)

				b.f.s.cfg.Shed.MaxBounces = 1
				c := &checkers{masons: r.masons}
				state, err := repo.Workflow(stream, trace.UnitSubject(bounceUnit))
				must(t, err)
				must(t, c.fail(context.Background(), stream, bounceUnit, state, UnitCheckRun{Unit: bounceUnit, Run: 1, Candidate: identity.Candidate.Revision, Base: identity.Candidate.BaseRevision, DiffSHA256: identity.DiffSHA256, Status: ChecksFailed, Selection: &CheckSelection{Mode: checkrun.SelectionFull}, Failed: []string{bounceCheck}}))
				result, ok, err := r.storedResult(stream, bounceUnit, trace.WorkflowState{})
				must(t, err)
				b.findings = result.Verdict.Findings
				if !ok || unitState(t, repo, stream, bounceUnit) != UnitContested {
					t.Fatal("missing held candidate")
				}
				if _, checked, err := latestRunFor(repo, stream, bounceUnit, identity); err != nil || checked {
					t.Fatalf("candidate already has checks: %t %v", checked, err)
				}

				ruling := ContestedRuling{Decision: decision, Note: bounceNote, Bounces: result.Bounces, Candidate: result.Identity.Candidate.Revision}
				data, _ := json.Marshal(ruling)
				doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: fmt.Sprintf("%s-ruling-%d", trace.UnitSubject(bounceUnit), result.Bounces), Revision: 1, Project: repo.Project(), Workstream: b.stream, Unit: bounceUnit, At: demoStart, Actor: ownerActor, Cause: reviewDocument(bounceUnit)}, Path: contestedRulingPath(bounceUnit, result.Bounces), Content: string(data)}
				must(t, repo.RecordDocuments(context.Background(), []trace.Document{doc}))
				if retry {
					other := "review"
					if decision == "review" {
						other = "revise"
					}
					out, api := b.f.s.recordContestedRuling(context.Background(), repo, b.stream, bounceUnit, ContestedRulingRequest{Decision: other, Note: "A later request must preserve the saved decision."}, ownerActor)
					if api != nil {
						t.Fatal(api)
					}
					if out.Ruling.Decision != decision || out.Ruling.Note != bounceNote {
						t.Fatalf("retry changed a recorded ruling: %+v", out)
					}
				}
				must(t, repo.Close())
				b.f.start(t)
				defer b.stop(t)
				b.deliveredOnce(t, decision)
				if decision == "review" {
					runs, err := checkRuns(b.f.repository(), stream)
					must(t, err)
					if len(runs) != 1 || runs[0].Status != ChecksPassed {
						t.Fatalf("review skipped required checks: %+v", runs)
					}
				}

			})
		}
	}
}

func TestBounceRulingKeysAreBoundedAndLegacyDecisionsRemainReadable(t *testing.T) {
	t.Parallel()
	c := newContestFixture(t, "bounce-keys")
	for _, unit := range []string{bounceUnit, strings.Repeat("u", 64), strings.Repeat("u", 128)} {
		contest := trace.UnitSubject(unit) + "-contested-checks-7-12"
		d := chiefDecisionDocument(c.repo, c.stream, unit, contest, "escalate", "Investigate the checks.", "events_1", demoStart)
		if len(d.ID) > 128 {
			t.Fatalf("unbounded decision key: %s", d.ID)
		}
		legacy := d
		legacy.ID = trace.UnitSubject(unit) + "-chief-" + contest
		legacy.Path = "units/" + trace.UnitSubject(unit) + "/chief-" + contest + ".json"
		if len(legacy.ID) > 128 {
			err := c.repo.RecordDocuments(context.Background(), []trace.Document{legacy})
			if err == nil {
				t.Fatal("trace accepted an oversized chief decision key")
			}
			t.Logf("legacy chief key length %d: %v", len(legacy.ID), err)
		}
		must(t, c.repo.RecordDocuments(context.Background(), []trace.Document{d}))
		_, decisions, err := chiefDecisions(c.repo, c.stream, unit)
		must(t, err)
		if len(decisions) != 1 || decisions[0].Contest != contest {
			t.Fatalf("decisions: %+v", decisions)
		}
	}

	oldID := trace.UnitSubject(bounceUnit) + "-chief-" + trace.UnitSubject(bounceUnit) + "-contested-12"
	if len(oldID) != 90 {
		t.Fatalf("ordinary verdict chief key length %d", len(oldID))
	}
	legacy := chiefDecisionDocument(c.repo, c.stream, "resume", "unit-resume-contested-12", "review", "Read the retry.", "events_1", demoStart)
	legacy.ID = "unit-resume-chief-unit-resume-contested-12"
	legacy.Path = "units/unit-resume/chief-unit-resume-contested-12.json"
	must(t, c.repo.RecordDocuments(context.Background(), []trace.Document{legacy}))
	_, decisions, err := chiefDecisions(c.repo, c.stream, "resume")
	must(t, err)
	if len(decisions) != 1 || decisions[0].Decision != "review" {
		t.Fatalf("legacy decision: %+v", decisions)
	}
}

func TestBounceRulingAndChiefDecisionCommitTogether(t *testing.T) {
	for _, decision := range []string{"review", "revise"} {
		t.Run(decision, func(t *testing.T) {
			t.Parallel()
			var inject atomic.Bool
			b := newBounceFixture(t, "mixed", func(o *Options) {
				o.boundary = func(_ context.Context, step string) error {
					if inject.Load() && strings.HasPrefix(step, "bounce-ruling-committed:") {
						return errors.New("interrupted after ruling commit")
					}
					return nil
				}
			})
			defer b.stop(t)
			commits := make(chan trace.Commit, 16)
			b.f.repository().Observe(func(c trace.Commit) {
				if slices.ContainsFunc(c.Paths, func(p string) bool { return strings.HasSuffix(p, "/ruling-3.json") }) {
					commits <- c
				}
			})
			inject.Store(true)
			b.rule(t, decision, "chief")
			commit := awaitCheck(t, commits)
			if commit.Workflow == nil || !slices.ContainsFunc(commit.Paths, func(p string) bool { return strings.Contains(p, "/chief-") }) {
				t.Fatalf("ruling split from chief decision or move: %+v", commit)
			}
			b.deliveredOnce(t, decision)
			_, decisions, err := chiefDecisions(b.f.repository(), b.stream, bounceUnit)
			must(t, err)
			if len(decisions) != 1 {
				t.Fatalf("chief decisions: %+v", decisions)
			}
		})
	}
}

func TestBounceRulingFailureLeavesTheContestAvailable(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			t.Parallel()
			var inject atomic.Bool
			var b *bounceFixture
			b = newBounceFixture(t, "mixed", func(o *Options) {
				o.boundary = func(ctx context.Context, step string) error {
					if !strings.HasPrefix(step, "bounce-ruling-recording:") || !inject.CompareAndSwap(true, false) {
						return nil
					}
					if !conflict {
						return errors.New("ruling write unavailable")
					}

					repo := b.f.repository()
					state, err := repo.Workflow(b.stream, trace.UnitSubject(bounceUnit))
					if err != nil {
						return err
					}
					h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "fixture-concurrent-1", Revision: 1, Project: repo.Project(), Workstream: b.stream, At: demoStart, Actor: reviewerActor, Cause: reviewDocument(bounceUnit), Unit: bounceUnit}
					_, err = repo.Transact(ctx, trace.Transaction{ExpectedVersion: state.Version, Transition: trace.Transition{Header: h, Subject: trace.UnitSubject(bounceUnit), From: UnitContested, To: UnitContested, Reason: "concurrent contest update"}})
					return err
				}
			})
			defer b.stop(t)
			inject.Store(true)
			_, err := b.f.c.RuleContested(context.Background(), b.stream, bounceUnit, "revise", bounceNote)
			if err == nil {
				t.Fatal("injected write failure or conflict was reported recorded")
			}
			_, found, e := latestContestedRuling(b.f.repository(), b.stream, bounceUnit, 3)
			must(t, e)
			if found {
				t.Fatal("failed move left a ruling")
			}
			b.rule(t, "revise", "owner")

			b.deliveredOnce(t, "revise")
		})
	}
}

func TestBounceRulingReconcilesAControllersStaleContest(t *testing.T) {
	for _, decision := range []string{"review", "revise"} {
		t.Run(decision, func(t *testing.T) {
			t.Parallel()
			b := newBounceFixture(t, "mixed")
			defer b.stop(t)
			repo := b.f.repository()
			stale, err := repo.Workflow(b.stream, trace.UnitSubject(bounceUnit))
			must(t, err)
			b.rule(t, decision, "owner")

			b.f.clock.Now()
			r := &reviewers{masons: newMasonController(b.f.s, repo)}
			must(t, r.resumeContested(context.Background(), b.stream, bounceUnit, stale))
			b.deliveredOnce(t, decision)
		})
	}
}

func TestBounceRulingRecoveryRejectsADifferentTransition(t *testing.T) {
	for _, decision := range []string{"review", "revise"} {
		for _, field := range []string{"contest", "candidate", "send-backs", "decision", "note"} {
			t.Run(decision+"/"+field, func(t *testing.T) {
				t.Parallel()
				b := newBounceFixture(t, "mixed")
				b.stop(t)
				repo, err := trace.Open(b.f.s.cfg.Root, config.Project{ID: b.f.project, Clone: b.f.clone})
				must(t, err)
				defer repo.Close()
				r := &reviewers{masons: newMasonController(b.f.s, repo)}
				state, err := repo.Workflow(b.stream, trace.UnitSubject(bounceUnit))
				must(t, err)
				result, ok, err := r.storedResult(b.stream, bounceUnit, state)
				must(t, err)
				if !ok {
					t.Fatal("missing send-back result")
				}
				ruling := ContestedRuling{Decision: decision, Note: bounceNote, Bounces: result.Bounces, Candidate: result.Identity.Candidate.Revision}
				to := map[string]string{"review": UnitReviewing, "revise": UnitImplementing}[decision]
				id := fmt.Sprintf("%s-%s-ruling-%d", trace.UnitSubject(bounceUnit), to, result.Bounces)
				reason := fmt.Sprintf("owner ruled %s on contested candidate %s after %d send-backs: %s", decision, ruling.Candidate, ruling.Bounces, ruling.Note)
				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: b.stream, Unit: bounceUnit, At: b.f.clock.Now(), Actor: ownerActor, Cause: fmt.Sprintf("%s-ruling-%d", trace.UnitSubject(bounceUnit), result.Bounces)}
				recorded := trace.Transition{Header: h, Subject: trace.UnitSubject(bounceUnit), From: UnitContested, To: to, Reason: reason}
				switch field {
				case "contest":
					recorded.Cause = "another-contest-ruling"
				case "candidate":
					recorded.Reason = strings.Replace(reason, ruling.Candidate, "another-candidate", 1)
				case "send-backs":
					recorded.Reason = strings.Replace(reason, fmt.Sprintf("after %d send-backs", ruling.Bounces), "after 99 send-backs", 1)
				case "decision":
					recorded.To = map[string]string{UnitReviewing: UnitImplementing, UnitImplementing: UnitReviewing}[to]
				case "note":
					recorded.Reason = strings.Replace(reason, bounceNote, "A different direction.", 1)
				}

				_, err = repo.Transact(context.Background(), trace.Transaction{ExpectedVersion: state.Version, Transition: recorded})
				must(t, err)
				err = r.applyBounceRuling(context.Background(), b.stream, bounceUnit, state, result, ruling, nil)
				if !errors.Is(err, trace.ErrConflict) {
					t.Fatalf("different %s reconciled: %v", field, err)
				}
				transitions, err := trace.Read[trace.Transition](repo, b.stream)
				must(t, err)
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
	for _, decision := range []string{"review", "revise"} {
		for _, boundary := range []string{"bounce-ruling-committed:", "unit-role-enqueue:", "unit-role-queued:"} {
			t.Run(decision+"/"+boundary, func(t *testing.T) {
				t.Parallel()
				var armed atomic.Bool
				var hit atomic.Bool
				reached := make(chan struct{}, 1)
				b := newBounceFixture(t, "mixed", func(o *Options) {
					o.boundary = func(ctx context.Context, step string) error {
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

				_, err := b.f.c.Send(context.Background(), b.stream, "rule bounce now: "+decision)
				must(t, err)
				awaitCheck(t, reached)
				b.stop(t)
				armed.Store(false)
				b.f.start(t)
				b.deliveredOnce(t, decision)
				_, decisions, err := chiefDecisions(b.f.repository(), b.stream, bounceUnit)
				must(t, err)
				if len(decisions) != 1 || decisions[0].Decision != decision {
					t.Fatalf("chief attribution after interruption: %+v", decisions)
				}
			})
		}
	}
}

func (b *bounceFixture) awaitUnit(t *testing.T, want string) {
	t.Helper()
	wait, close := serviceChanges(t, b.f.s)
	defer close()
	until := time.Now().Add(2 * time.Minute)
	for {
		state, err := b.f.repository().Workflow(b.stream, trace.UnitSubject(bounceUnit))
		must(t, err)
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
	target := b.passed.Load() + 3
	until := time.Now().Add(30 * time.Second)
	for b.passed.Load() < target {
		if time.Now().After(until) {
			t.Fatalf("completed passes %d, wanted %d", b.passed.Load(), target)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (b *bounceFixture) stop(t *testing.T) {
	t.Helper()
	b.f.c.Close()
	stopped := make(chan error, 1)
	go func() { stopped <- b.f.s.Close() }()
	select {
	case err := <-stopped:
		must(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("service did not stop")
	}
}

func TestOrdinarySendBackRecoversFindingsAfterRestart(t *testing.T) {
	for _, history := range []string{"reviews", "checks"} {
		for _, recoverer := range []string{"service", "mason-finish"} {
			t.Run(history+"/"+recoverer, func(t *testing.T) {
				t.Parallel()
				var interrupted atomic.Bool
				interrupted.Store(history == "checks")
				reached := make(chan struct{}, 1)
				b := prepareBounceFixture(t, history, false, func(o *Options) {
					o.boundary = func(ctx context.Context, step string) error {
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
				b.f.engine.Mu.Lock()
				original := b.f.engine.Turns["*"]
				b.f.engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
					if strings.HasPrefix(req.Name, masonAgent(bounceUnit)+"-revise-") {
						b.delivered <- req
						_, err := callTool(ctx, tools, "ask", map[string]any{"question": "Please confirm the send-back evidence."})
						return &agent.Result{ClaudeID: "ordinary-revision", ResultText: "Asked", SessionDir: req.SessionDir, NumTurns: 1}, err
					}
					result, err := original(ctx, req, turn, tools)
					if strings.HasPrefix(req.Name, reviewerAgent(bounceUnit)+"-") {
						interrupted.Store(true)
					}
					return result, err
				}
				b.f.engine.Mu.Unlock()
				b.stream = b.f.seedBuilding(t, "ordinary-"+history+"-"+recoverer, b.graph)
				awaitCheck(t, reached)
				state, err := b.f.repository().Workflow(b.stream, trace.UnitSubject(bounceUnit))
				must(t, err)
				if state.Value != UnitImplementing {
					t.Fatalf("send-back not committed: %+v", state)
				}
				r := &reviewers{masons: newMasonController(b.f.s, b.f.repository())}
				result, ok, err := r.storedResult(b.stream, bounceUnit, state)
				must(t, err)
				if !ok || result.Bounces != 1 || result.Bounces >= b.f.s.cfg.Shed.MaxBounces || result.Checks > 0 != (history == "checks") {
					t.Fatalf("not an ordinary %s send-back: %+v", history, result)
				}
				th, err := b.f.repository().Thread(b.stream, masonAgent(bounceUnit))
				must(t, err)
				if len(th.Turns) != 1 || th.Turns[0].Response.Result.Outcome == nil || th.Turns[0].Response.Result.Outcome.Status != masonDone {
					t.Fatalf("expected only the old done turn before shutdown: %+v", th.Turns)
				}
				b.stop(t)
				interrupted.Store(false)
				if recoverer == "mason-finish" {

					repo, err := trace.Open(b.f.s.cfg.Root, b.f.s.cfg.Project)
					must(t, err)
					m := newMasonController(b.f.s, repo)
					building, found, err := m.read(b.stream)
					must(t, err)
					if !found {
						t.Fatal("missing building workstream")
					}
					moved, blocked, err := m.finish(context.Background(), building, bounceUnit)
					must(t, err)
					if moved || blocked {
						t.Fatalf("mason reused old done: moved %t, blocked %t", moved, blocked)
					}
					must(t, repo.Close())
				}
				b.f.start(t)
				req := awaitCheck(t, b.delivered)
				findings, err := json.MarshalIndent(result.Verdict.Findings, "", "  ")
				must(t, err)
				if !strings.Contains(req.Prompt, string(findings)) || !strings.Contains(req.Prompt, result.Identity.Candidate.Revision) || strings.Contains(req.Prompt, bounceNote) {
					t.Fatalf("wrong recovered findings: %s", req.Prompt)
				}
				b.awaitUnit(t, UnitWaiting)
				assertOnce := func() {
					t.Helper()
					thread, err := b.f.repository().Thread(b.stream, masonAgent(bounceUnit))
					must(t, err)
					if len(thread.Turns) != 2 || thread.Turns[1].Request.TurnID != req.Name {
						t.Fatalf("expected initial done and exactly one findings turn: %+v", thread.Turns)
					}
					transitions, err := trace.Read[trace.Transition](b.f.repository(), b.stream)
					must(t, err)
					n := 0
					for _, tr := range transitions {
						if tr.Subject == trace.UnitSubject(bounceUnit) && tr.From == UnitImplementing && tr.To == UnitChecking {
							n++
						}
					}
					if n != 1 {
						t.Fatalf("unchanged candidate returned to checking: %d transitions", n)
					}
					runs, err := checkRuns(b.f.repository(), b.stream)
					must(t, err)
					if len(runs) != 1 {
						t.Fatalf("unchanged candidate rechecked: %+v", runs)
					}
				}
				b.awaitPasses(t)
				assertOnce()
				b.stop(t)
				b.f.start(t)
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
