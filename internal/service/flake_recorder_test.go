package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/flakes"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/trace"
)

const flakeFixStream config.WorkstreamID = "w_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

const (
	flakyTraceTest  = "dag://go/packages/tests/test?go-package=internal/trace&go-test=TestFlaky"
	flakyTraceOther = "dag://go/packages/tests/test?go-package=internal/trace&go-test=TestOther"
	flakyWebTest    = "dag://go/packages/tests/test?go-package=internal/web&go-test=TestFlaky"
)

var errFlakeStop = errors.New("interrupted for the test")

// flakyOutcome is a check run that passed with the links flaky.
func flakyOutcome(links ...string) checkOutcome {
	output := "CHECKS  ✔ " + fmt.Sprint(len(links)) + " passed\n"
	for _, link := range links {
		output += "✔ " + link + " 1.0s OK\n"
	}
	return checkOutcome{Result: api.CheckResult{Output: output, Flaky: links}}
}

// newFlakeFixture is a workstream whose resume unit is checking, with the
// setting on and checks that return the outcomes.
func newFlakeFixture(t *testing.T, key string, outcomes ...checkOutcome) (*shedFixture, config.WorkstreamID, *trace.Repository) {
	t.Helper()
	checks := &fakeChecks{Outcomes: outcomes}
	f, _ := newMasonFixturePrepared(t, 1, independentPlan, func(opts *Options) { opts.reviewChecks = checks })
	f.s.cfg.Project.FlakeFixes = true
	base := strings.TrimSpace(demoGit(t, f.clone, "-C", f.clone, "rev-parse", "HEAD"))
	stream, repository := seedBuild(t, f, key, independentPlan, config.WorkspacesGit, base)
	seedChecking(t, f, repository, stream, "resume", resumeReport)
	return f, stream, repository
}

func flakeRecorderOf(f *shedFixture, repository *trace.Repository) *flakeRecorder {
	return &flakeRecorder{s: f.s, cfg: f.s.cfg, repository: repository}
}

// flakeStoreOf is the project's flake record.
func flakeStoreOf(repository *trace.Repository) *flakes.Store {
	return &flakes.Store{Repository: repository}
}

func createFlakeFix(t *testing.T, repository *trace.Repository) {
	t.Helper()
	must(t, repository.CreateWorkstream(context.Background(), flakeFixStream, time.Now(), trace.Actor{Kind: "service", ID: "fixture"}))
}

// openGroupOf makes workstream the open flake-fix workstream of the group of
// link, through an earlier occurrence.
func openGroupOf(t *testing.T, repository *trace.Repository, link string, workstream config.WorkstreamID) string {
	t.Helper()
	ctx := context.Background()
	group := flakes.GroupFlaky(nil, []string{link})[0]
	store := flakeStoreOf(repository)
	run := flakes.RunKey("w_cccccccccccccccccccccccccccccccc", "units/earlier/checks-1.json")
	_, _, err := store.Record(ctx, flakes.OccurrenceInput{Group: group.Group, Run: run, Source: flakes.Source{Workstream: "w_cccccccccccccccccccccccccccccccc", Unit: "earlier"}, Links: []string{link}})
	must(t, err)
	_, err = store.Reserve(ctx, group.Group, run)
	must(t, err)
	_, err = store.Opened(ctx, group.Group, run, workstream)
	must(t, err)
	return group.Group
}

// flakePosts are the transitions and outbox bodies of stream whose subject
// starts with prefix.
func flakePosts(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, prefix string) ([]trace.Transition, []string) {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](repository, stream)
	must(t, err)
	var posted []trace.Transition
	for _, tr := range transitions {
		if strings.HasPrefix(tr.Subject, prefix) {
			posted = append(posted, tr)
		}
	}
	entries, err := repository.Outbox(stream)
	must(t, err)
	var bodies []string
	for _, e := range entries {
		if strings.HasPrefix(e.TransitionID, prefix) {
			if e.Event.Kind != trace.NoticeKind {
				t.Fatalf("a flake post is a %s, not a notice", e.Event.Kind)
			}
			bodies = append(bodies, e.Event.Body)
		}
	}
	return posted, bodies
}

func flakeNoticeBodies(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []string {
	t.Helper()
	// A notice is one transaction, but the transitions and the outbox are two
	// reads, so a notice committed between them shows in only one.
	var posted []trace.Transition
	var bodies []string
	for range 100 {
		posted, bodies = flakePosts(t, repository, stream, "flake-notice-")
		if len(posted) == len(bodies) {
			return bodies
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%d notice transitions but %d notice events", len(posted), len(bodies))
	return bodies
}

func flakeSurfacings(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []string {
	t.Helper()
	_, bodies := flakePosts(t, repository, stream, "flake-evidence-")
	return bodies
}

func flakeOccurrences(t *testing.T, repository *trace.Repository) []flakes.Occurrence {
	t.Helper()
	occurrences, err := flakeStoreOf(repository).Occurrences()
	must(t, err)
	return occurrences
}

func passFlakes(t *testing.T, f *shedFixture, repository *trace.Repository) {
	t.Helper()
	must(t, flakeRecorderOf(f, repository).Pass(context.Background()))
}

// unitHistory is the unit's transitions without their reasons.
func unitHistory(t *testing.T, repository *trace.Repository, stream config.WorkstreamID) []string {
	t.Helper()
	transitions, err := trace.Read[trace.Transition](repository, stream)
	must(t, err)
	var out []string
	for _, tr := range transitions {
		if !strings.HasPrefix(tr.Subject, "flake-") {
			out = append(out, tr.Subject+" "+tr.From+" -> "+tr.To)
		}
	}
	return out
}

func TestFlakeRecorderNoticesAUnitRunThatPassedWithAFlakyCheck(t *testing.T) {
	t.Parallel()
	f, stream, repository := newFlakeFixture(t, "flake-pass", flakyOutcome(flakyTraceTest))
	runChecks(t, f.s, repository, stream)
	if got := intakefixture.UnitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
	passFlakes(t, f, repository)

	occurrences := flakeOccurrences(t, repository)
	if len(occurrences) != 1 || !slices.Equal(occurrences[0].Links, []string{flakyTraceTest}) || occurrences[0].Source != (flakes.Source{Workstream: stream, Unit: "resume"}) || occurrences[0].Run != flakes.RunKey(stream, "units/resume/checks-1.json") || occurrences[0].Noticed == nil {
		t.Fatalf("occurrences %+v", occurrences)
	}
	if occurrences[0].Evidence.Engine != api.CheckEngineLocal || occurrences[0].Evidence.Parallelism == 0 || len(occurrences[0].Evidence.CoRun) == 0 {
		t.Fatalf("the occurrence holds no co-run evidence: %+v", occurrences[0].Evidence)
	}
	notices := flakeNoticeBodies(t, repository, stream)
	group := flakes.GroupFlaky(nil, []string{flakyTraceTest})[0].Group
	if len(notices) != 1 || !strings.Contains(notices[0], "Awaiting your decision (1)") || !strings.Contains(notices[0], group) || !strings.Contains(notices[0], flakyTraceTest) || !strings.Contains(notices[0], occurrences[0].ID) || !strings.Contains(notices[0], flakeFixToolName+" with action \"open\"") || !strings.Contains(notices[0], flakeFixToolName+" with action \"decline\"") || !strings.Contains(notices[0], "units/resume/checks-1.json") {
		t.Fatalf("notices %q", notices)
	}

	// Another recorder, as after a restart, and another pass add nothing.
	passFlakes(t, f, repository)
	passFlakes(t, f, repository)
	if again := flakeOccurrences(t, repository); len(again) != 1 || len(flakeNoticeBodies(t, repository, stream)) != 1 {
		t.Fatalf("a repeated pass recorded %+v", again)
	}
}

func TestFlakeRecorderNoticesAUnitRunThatFailedWithAFailedAndAFlakyCheck(t *testing.T) {
	t.Parallel()
	const flaky, failed = "dag://go/packages/tests/test?go-package=internal/trace&go-test=TestFlaky", "dag://go/lint"
	output := "CHECKS  ✘ 2 failed\n✘ " + flaky + " 1s ERROR\n✘ " + failed + " 1s ERROR\nCHECKS  ✘ 1 failed  ✔ 1 passed\n✘ " + failed + " 1s ERROR\n✔ " + flaky + " 1s OK\n"
	f, stream, repository := newFlakeFixture(t, "flake-fail", checkOutcome{Result: api.CheckResult{ExitCode: 1, Output: output, Flaky: []string{flaky}}})
	runChecks(t, f.s, repository, stream)
	if got := intakefixture.UnitState(t, repository, stream, "resume"); got != UnitImplementing {
		t.Fatalf("unit is %s, want implementing", got)
	}
	passFlakes(t, f, repository)
	occurrences := flakeOccurrences(t, repository)
	if len(occurrences) != 1 || !slices.Equal(occurrences[0].Links, []string{flaky}) {
		t.Fatalf("occurrences %+v", occurrences)
	}
	if notices := flakeNoticeBodies(t, repository, stream); len(notices) != 1 || strings.Contains(notices[0], failed) {
		t.Fatalf("notices %q", notices)
	}
}

func TestFlakeRecorderListsTwoGroupsInOneNotice(t *testing.T) {
	t.Parallel()
	f, stream, repository := newFlakeFixture(t, "flake-two-groups", flakyOutcome(flakyTraceTest, flakyTraceOther, flakyWebTest))
	runChecks(t, f.s, repository, stream)
	passFlakes(t, f, repository)

	occurrences := flakeOccurrences(t, repository)
	if len(occurrences) != 2 {
		t.Fatalf("occurrences %+v, want one for each of the two groups", occurrences)
	}
	notices := flakeNoticeBodies(t, repository, stream)
	if len(notices) != 1 || !strings.Contains(notices[0], "Awaiting your decision (2)") || strings.Contains(notices[0], "For information") {
		t.Fatalf("notices %q", notices)
	}
	for _, o := range occurrences {
		if !strings.Contains(notices[0], o.ID) {
			t.Fatalf("the notice lacks occurrence %s: %s", o.ID, notices[0])
		}
	}
	for _, link := range []string{flakyTraceTest, flakyTraceOther, flakyWebTest} {
		if !strings.Contains(notices[0], link) {
			t.Fatalf("the notice lacks %s: %s", link, notices[0])
		}
	}
	for _, o := range occurrences {
		if len(o.Links) == 2 && !slices.Equal(o.Links, []string{flakyTraceTest, flakyTraceOther}) {
			t.Fatalf("sibling tests of one package fall in one group: %+v", o)
		}
	}
}

func TestFlakeRecorderJoinsAGroupWithAnOpenWorkstream(t *testing.T) {
	t.Parallel()
	f, stream, repository := newFlakeFixture(t, "flake-mixed", flakyOutcome(flakyTraceTest, flakyWebTest))
	createFlakeFix(t, repository)
	openGroup := openGroupOf(t, repository, flakyTraceTest, flakeFixStream)
	runChecks(t, f.s, repository, stream)
	transitions, err := trace.Read[trace.Transition](repository, stream)
	must(t, err)
	if !slices.ContainsFunc(transitions, func(tr trace.Transition) bool {
		return tr.To == UnitReviewing && strings.Contains(tr.Reason, "flake-fix workstream "+string(flakeFixStream)+" is open for flake group "+openGroup)
	}) {
		t.Fatalf("the pass reason does not name the open workstream: %+v", transitions)
	}
	passFlakes(t, f, repository)
	passFlakes(t, f, repository)

	notices := flakeNoticeBodies(t, repository, stream)
	if len(notices) != 1 || !strings.Contains(notices[0], "Awaiting your decision (1)") || !strings.Contains(notices[0], "For information (1)") || !strings.Contains(notices[0], "`"+string(flakeFixStream)+"`") {
		t.Fatalf("notices %q", notices)
	}
	informational := notices[0][strings.Index(notices[0], "For information"):]
	actionable := notices[0][:strings.Index(notices[0], "For information")]
	if !strings.Contains(informational, flakyTraceTest) || strings.Contains(informational, flakyWebTest) || !strings.Contains(actionable, flakyWebTest) || strings.Contains(actionable, flakyTraceTest) {
		t.Fatalf("the occurrences are not split by decision: %s", notices[0])
	}
	surfacings := flakeSurfacings(t, repository, flakeFixStream)
	if len(surfacings) != 1 || !strings.Contains(surfacings[0], flakyTraceTest) || strings.Contains(surfacings[0], flakyWebTest) || !strings.Contains(surfacings[0], openGroup) {
		t.Fatalf("surfacings %q", surfacings)
	}
	var joined, pending int
	for _, o := range flakeOccurrences(t, repository) {
		switch {
		case o.Run == flakes.RunKey(stream, "units/resume/checks-1.json") && o.Decision.State == flakes.Joined && o.Decision.Workstream == flakeFixStream && len(o.Surfaced) == 1 && o.Noticed != nil:
			joined++
		case o.Run == flakes.RunKey(stream, "units/resume/checks-1.json") && o.Decision.State == flakes.Pending && o.Noticed != nil:
			pending++
		}
	}
	if joined != 1 || pending != 1 {
		t.Fatalf("%d joined and %d pending occurrences: %+v", joined, pending, flakeOccurrences(t, repository))
	}
	if posted, _ := flakePosts(t, repository, stream, "flake-evidence-"); len(posted) != 0 {
		t.Fatalf("the originating workstream received a surfacing: %+v", posted)
	}
}

func TestFlakeRecorderOnlyInformsWhenEveryGroupHasAnOpenWorkstream(t *testing.T) {
	t.Parallel()
	f, stream, repository := newFlakeFixture(t, "flake-all-open", flakyOutcome(flakyTraceTest, flakyWebTest))
	createFlakeFix(t, repository)
	other := config.WorkstreamID("w_dddddddddddddddddddddddddddddddd")
	must(t, repository.CreateWorkstream(context.Background(), other, time.Now(), trace.Actor{Kind: "service", ID: "fixture"}))
	openGroupOf(t, repository, flakyTraceTest, flakeFixStream)
	openGroupOf(t, repository, flakyWebTest, other)
	runChecks(t, f.s, repository, stream)
	passFlakes(t, f, repository)

	notices := flakeNoticeBodies(t, repository, stream)
	if len(notices) != 1 || strings.Contains(notices[0], "Awaiting your decision") || !strings.Contains(notices[0], "For information (2)") || !strings.Contains(notices[0], string(flakeFixStream)) || !strings.Contains(notices[0], string(other)) {
		t.Fatalf("notices %q", notices)
	}
	for _, target := range []config.WorkstreamID{flakeFixStream, other} {
		if surfacings := flakeSurfacings(t, repository, target); len(surfacings) != 1 {
			t.Fatalf("surfacings of %s: %q", target, surfacings)
		}
	}
}

func TestFlakeRecorderRecordsAFlakeFixRunWithoutNoticingOrSurfacing(t *testing.T) {
	t.Parallel()
	f, stream, repository := newFlakeFixture(t, "flake-own-run", flakyOutcome(flakyTraceTest, flakyWebTest))
	other := config.WorkstreamID("w_dddddddddddddddddddddddddddddddd")
	must(t, repository.CreateWorkstream(context.Background(), other, time.Now(), trace.Actor{Kind: "service", ID: "fixture"}))
	// The checking workstream is the open flake-fix workstream of one group,
	// and another workstream is that of the other.
	openGroupOf(t, repository, flakyTraceTest, stream)
	openGroupOf(t, repository, flakyWebTest, other)
	runChecks(t, f.s, repository, stream)
	passFlakes(t, f, repository)
	passFlakes(t, f, repository)

	var own int
	for _, o := range flakeOccurrences(t, repository) {
		if o.Source.Workstream == stream {
			own++
			if !o.FromFlakeFix || o.Noticed != nil || len(o.Surfaced) != 0 || o.Decision.State != flakes.Pending {
				t.Fatalf("occurrence of a flake-fix run %+v", o)
			}
		}
	}
	if own != 2 {
		t.Fatalf("%d occurrences recorded, want 2: %+v", own, flakeOccurrences(t, repository))
	}
	if notices := flakeNoticeBodies(t, repository, stream); len(notices) != 0 {
		t.Fatalf("a flake-fix run sent %q", notices)
	}
	if surfacings := flakeSurfacings(t, repository, other); len(surfacings) != 0 {
		t.Fatalf("a flake-fix run surfaced %q", surfacings)
	}
}

func TestFlakeRecorderDoesNothingWithTheSettingOff(t *testing.T) {
	t.Parallel()
	f, stream, repository := newFlakeFixture(t, "flake-off", flakyOutcome(flakyTraceTest))
	f.s.cfg.Project.FlakeFixes = false
	runChecks(t, f.s, repository, stream)
	if got := intakefixture.UnitState(t, repository, stream, "resume"); got != UnitReviewing {
		t.Fatalf("unit is %s, want reviewing", got)
	}
	passFlakes(t, f, repository)
	if occurrences := flakeOccurrences(t, repository); len(occurrences) != 0 {
		t.Fatalf("occurrences %+v", occurrences)
	}
	posted, _ := flakePosts(t, repository, stream, "flake-")
	if len(posted) != 0 {
		t.Fatalf("posts %+v", posted)
	}
}

func TestFlakeRecorderIgnoresSessionCheckRuns(t *testing.T) {
	t.Parallel()
	f, _, repository := newFlakeFixture(t, "flake-session", flakyOutcome(flakyTraceTest))
	ctx := context.Background()
	session := config.WorkstreamID("w_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	must(t, repository.CreateSessionWorkstream(ctx, session, "default", "abc123", "session-branch", "try", time.Now(), trace.Actor{Kind: "owner", ID: "owner"}))
	now := time.Now()
	must(t, recordSessionCheckRun(ctx, repository, repository.Project(), session, SessionCheckRun{Run: 1, Commit: "abc123", Status: ChecksPassed, Flaky: []string{flakyTraceTest}, StartedAt: now, FinishedAt: now}, ""))
	passFlakes(t, f, repository)
	if occurrences := flakeOccurrences(t, repository); len(occurrences) != 0 {
		t.Fatalf("a session check run recorded %+v", occurrences)
	}
	if posted, _ := flakePosts(t, repository, session, "flake-"); len(posted) != 0 {
		t.Fatalf("a session check run sent %+v", posted)
	}
}

// cloudFinalRun records the final-review check run a Dagger Cloud result
// with two flaky checks leaves.
func cloudFinalRun(t *testing.T, repository *trace.Repository, stream config.WorkstreamID, review int) FinalCheckRun {
	t.Helper()
	at := time.Now()
	run := FinalCheckRun{Review: review, Operation: "final-review-op", Commit: "abc123", Source: api.CheckSourcePullRequest, PullRequest: 7, Push: 1, PushedCommit: "abc123", Status: ChecksPassed,
		Flaky: []string{"test:a", "test:b"}, StartedAt: at, FinishedAt: at,
		Evidence: &api.CheckEvidence{Engine: api.CheckEngineCloud, CoRun: []string{"lint", "test:a", "test:b"},
			Traces: []api.CheckTraceRef{
				{Kind: api.CheckTraceFailed, Check: "test:a", TraceID: "trace-a"},
				{Kind: api.CheckTraceRerun, Check: "test:a", TraceID: "fresh-a"},
				{Kind: api.CheckTraceFailed, Check: "test:b", TraceID: "trace-b"},
				{Kind: api.CheckTraceRun, URL: "https://dagger.cloud/traces/run"},
			},
			Unavailable: []api.CheckUnavailable{
				{Item: api.CheckItemOutput, Engine: api.CheckEngineCloud, Where: "dagger trace trace-a"},
				{Item: api.CheckItemParallelism, Engine: api.CheckEngineCloud, Where: "dagger trace trace-a"},
			}}}
	data, err := json.MarshalIndent(run, "", "  ")
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: delivery.FinalChecksDocument(review), Revision: 1, Project: repository.Project(), Workstream: stream, At: at, Actor: trace.Actor{Kind: "service", ID: "final-review"}, Cause: "final-review-op"}
	must(t, repository.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: delivery.FinalChecksPath(review), Content: string(data) + "\n"}}))
	return run
}

func TestFlakeRecorderNoticesAFinalReviewCloudRunAndKeepsItsEvidence(t *testing.T) {
	t.Parallel()
	f, stream, repository := newFlakeFixture(t, "flake-final", flakyOutcome(flakyTraceTest))
	cloudFinalRun(t, repository, stream, 1)
	passFlakes(t, f, repository)
	passFlakes(t, f, repository)

	notices := flakeNoticeBodies(t, repository, stream)
	if len(notices) != 1 || !strings.Contains(notices[0], "Awaiting your decision (2)") || !strings.Contains(notices[0], "The final review of workstream "+string(stream)) || !strings.Contains(notices[0], "final/checks-1.json") {
		t.Fatalf("notices %q", notices)
	}
	store := flakeStoreOf(repository)
	group, found, err := store.Group("test:a")
	must(t, err)
	if !found {
		t.Fatalf("no record for the group of test:a: %+v", flakeOccurrences(t, repository))
	}
	o, ok := group.Find(flakes.RunKey(stream, "final/checks-1.json"))
	if !ok || !o.Source.FinalReview || o.Source.Unit != "" || !group.Unresolved || o.Evidence.Engine != api.CheckEngineCloud {
		t.Fatalf("occurrence %+v of group %+v", o, group)
	}
	text, err := flakes.RenderHandIn(group, o.Run, "")
	must(t, err)
	for _, want := range []string{
		"`cloud`", "the final review of workstream `" + string(stream) + "`", "`final/checks-1.json`",
		"- `lint`", "- `test:a`", "- `test:b`",
		"`dagger trace trace-a` (test:a, failed)", "`dagger trace fresh-a` (test:a, rerun)", "`https://dagger.cloud/traces/run` (run)",
		"The captured output is not reported by cloud; read ",
		"The parallelism of the invocation is not reported by cloud; read ",
		"The `[passed on rerun]` diagnostics are not reported by cloud; read ",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the hand-in lacks %q:\n%s", want, text)
		}
	}
}

func TestFlakeRecorderResumesAfterEveryInterruption(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"flake-occurrence-recorded", "flake-before-notice", "flake-surfaced-before-mark", "flake-notice-sent"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			f, stream, repository := newFlakeFixture(t, "flake-interrupted-"+step, flakyOutcome(flakyTraceTest, flakyWebTest))
			createFlakeFix(t, repository)
			openGroupOf(t, repository, flakyTraceTest, flakeFixStream)
			// The source unit has advanced before the recorder sees its run.
			runChecks(t, f.s, repository, stream)
			if got := intakefixture.UnitState(t, repository, stream, "resume"); got != UnitReviewing {
				t.Fatalf("unit is %s, want reviewing", got)
			}
			stopped := 0
			f.s.setBoundary(func(name string) error {
				if name == step {
					stopped++
					return errFlakeStop
				}
				return nil
			})
			must(t, flakeRecorderOf(f, repository).Pass(context.Background()))
			if stopped == 0 {
				t.Fatalf("the recorder never reached %s", step)
			}
			f.s.setBoundary(nil)

			// A new recorder, as after a restart, finishes the run.
			passFlakes(t, f, repository)
			passFlakes(t, f, repository)
			occurrences := flakeOccurrences(t, repository)
			var ours []flakes.Occurrence
			for _, o := range occurrences {
				if o.Source.Workstream == stream {
					ours = append(ours, o)
				}
			}
			if len(ours) != 2 {
				t.Fatalf("%d occurrences of the run, want 2: %+v", len(ours), occurrences)
			}
			for _, o := range ours {
				if o.Noticed == nil {
					t.Fatalf("occurrence not marked noticed: %+v", o)
				}
				if o.Decision.State == flakes.Joined && len(o.Surfaced) != 1 {
					t.Fatalf("joined occurrence surfaced %d times: %+v", len(o.Surfaced), o)
				}
			}
			if notices := flakeNoticeBodies(t, repository, stream); len(notices) != 1 {
				t.Fatalf("notices %q", notices)
			}
			if surfacings := flakeSurfacings(t, repository, flakeFixStream); len(surfacings) != 1 {
				t.Fatalf("surfacings %q", surfacings)
			}
		})
	}
}

func TestFlakeRecorderResumesAFinalReviewRunAfterInterruptions(t *testing.T) {
	t.Parallel()
	f, stream, repository := newFlakeFixture(t, "flake-final-interrupted", flakyOutcome(flakyTraceTest))
	cloudFinalRun(t, repository, stream, 1)
	f.s.setBoundary(func(name string) error {
		if name == "flake-occurrence-recorded" {
			return errFlakeStop
		}
		return nil
	})
	passFlakes(t, f, repository)
	f.s.setBoundary(nil)
	passFlakes(t, f, repository)
	if occurrences := flakeOccurrences(t, repository); len(occurrences) != 2 {
		t.Fatalf("occurrences %+v", occurrences)
	}
	if notices := flakeNoticeBodies(t, repository, stream); len(notices) != 1 {
		t.Fatalf("notices %q", notices)
	}
}

func TestFlakeFailuresNeverChangeTheOriginatingFlow(t *testing.T) {
	t.Parallel()
	control, controlStream, controlRepository := newFlakeFixture(t, "flake-control", checkOutcome{Result: api.CheckResult{Output: "CHECKS  ✔ 1 passed\n✔ " + flakyTraceTest + " 1.0s OK\n"}})
	control.s.cfg.Project.FlakeFixes = false
	runChecks(t, control.s, controlRepository, controlStream)
	want := unitHistory(t, controlRepository, controlStream)

	for _, step := range []string{"flake-occurrence-recorded", "flake-before-notice"} {
		f, stream, repository := newFlakeFixture(t, "flake-failing-"+step, flakyOutcome(flakyTraceTest))
		f.s.setBoundary(func(name string) error {
			if name == step {
				return errFlakeStop
			}
			return nil
		})
		runChecks(t, f.s, repository, stream)
		for range 3 {
			if err := flakeRecorderOf(f, repository).Pass(context.Background()); err != nil {
				t.Fatalf("the recorder returned %v at %s", err, step)
			}
		}
		if got := unitHistory(t, repository, stream); !slices.Equal(got, want) {
			t.Fatalf("with %s failing, the unit's transitions are %q, want %q", step, got, want)
		}
		if notices := flakeNoticeBodies(t, repository, stream); len(notices) != 0 {
			t.Fatalf("%s: a notice went out while the path failed: %q", step, notices)
		}

		f.s.setBoundary(nil)
		passFlakes(t, f, repository)
		if notices := flakeNoticeBodies(t, repository, stream); len(notices) != 1 {
			t.Fatalf("%s: notices %q after the failure cleared", step, notices)
		}
		if occurrences := flakeOccurrences(t, repository); len(occurrences) != 1 || occurrences[0].Noticed == nil {
			t.Fatalf("%s: occurrences %+v after the failure cleared", step, occurrences)
		}
		if got := unitHistory(t, repository, stream); !slices.Equal(got, want) {
			t.Fatalf("after %s cleared, the unit's transitions are %q, want %q", step, got, want)
		}
	}
}

func TestFlakeRecordingIsAScheduleStepThatNeverFailsTheSchedule(t *testing.T) {
	t.Parallel()
	f, stream, repository := newFlakeFixture(t, "flake-schedule", flakyOutcome(flakyTraceTest))
	runChecks(t, f.s, repository, stream)
	stages, err := f.s.stages(f.s.cfg, repository)
	must(t, err)
	i := slices.IndexFunc(stages.hooks, func(h scheduleHook) bool { return h.name == "flakes" })
	if i < 0 {
		t.Fatalf("the schedule has no flakes step: %+v", stages.hooks)
	}
	f.s.setBoundary(func(name string) error {
		if name == "flake-occurrence-recorded" {
			return errFlakeStop
		}
		return nil
	})
	if err := stages.hooks[i].pass(context.Background()); err != nil {
		t.Fatalf("a failing flake side effect failed the schedule: %v", err)
	}
	if notices := flakeNoticeBodies(t, repository, stream); len(notices) != 0 {
		t.Fatalf("notices %q", notices)
	}
	f.s.setBoundary(nil)
	must(t, stages.hooks[i].pass(context.Background()))
	if notices := flakeNoticeBodies(t, repository, stream); len(notices) != 1 {
		t.Fatalf("notices %q", notices)
	}
}
