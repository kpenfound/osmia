package errorreport

import (
	"encoding/json"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func testConfig(repo string) Config {
	return Config{
		Repository:        repo,
		Labels:            []string{"factory-error", "automated"},
		MaxIssuesPerHour:  5,
		MaxIssuesPerDay:   20,
		MaxCommentsPerDay: 10,
		Version:           "v1.2.3",
		Commit:            "abc1234",
	}
}

func testReport(fp string, at time.Time) Report {
	return Report{Source: SourceGitHub, Template: "pulls.find", Status: 502, Message: "boom " + fp, Time: at, Fingerprint: fp}
}

// fp returns a distinct fingerprint for n.
func fp(n int) string {
	return "00000000000000" + string(rune('a'+n/10)) + string(rune('a'+n%10))
}

func step(st *State, cfg Config, rep Report, check *Check, now time.Time) Decision {
	return Step(st, cfg, Input{Report: &rep, Check: check}, now)
}

// fileIssue drives an occurrence to a created issue numbered issue.
func fileIssue(t *testing.T, st *State, cfg Config, fingerprint string, at time.Time, issue int) Decision {
	t.Helper()
	d := step(st, cfg, testReport(fingerprint, at), nil, at)
	if d.Kind != OpenIssue && d.Kind != OpenLinkedIssue {
		t.Fatalf("decision = %s, want a new issue", d.Kind)
	}
	if err := st.CompleteCreate(*d.Intent, issue, at); err != nil {
		t.Fatal(err)
	}
	return d
}

// recur drives an occurrence of a fingerprint with a saved issue through the
// state check, with the issue reported as state, and returns the decision
// made with the check.
func recur(t *testing.T, st *State, cfg Config, fingerprint string, at time.Time, state IssueState) Decision {
	t.Helper()
	rep := testReport(fingerprint, at)
	first := Step(st, cfg, Input{Report: &rep}, at)
	if first.Kind != CheckState {
		t.Fatalf("decision = %s, want a state check", first.Kind)
	}
	check := &Check{Repository: cfg.Repository, Issue: first.Issue, State: state, At: at}
	return Step(st, cfg, Input{Report: &rep, Check: check}, at)
}

func TestOneIssuePerFingerprintAndOneCommentPerDay(t *testing.T) {
	t.Parallel()
	st, cfg := NewState(), testConfig(targetRepo)
	d := step(st, cfg, testReport(fp(1), t0), nil, t0)
	if d.Kind != OpenIssue || d.Intent == nil || d.Intent.Kind != IntentCreate || d.Intent.Filing.Count != 1 {
		t.Fatalf("first occurrence = %+v", d)
	}
	if len(d.Intent.Labels) != 2 {
		t.Errorf("create intent labels = %v", d.Intent.Labels)
	}
	// While the create is unresolved, nothing else is written.
	if d := step(st, cfg, testReport(fp(1), t0.Add(time.Minute)), nil, t0.Add(time.Minute)); d.Kind != Reconcile {
		t.Fatalf("decision = %s, want reconcile", d.Kind)
	}
	if err := st.CompleteCreate(st.IntentsFor(targetRepo)[0], 7, t0); err != nil {
		t.Fatal(err)
	}

	// The first repeat comments, with the count and last seen time.
	at := t0.Add(2 * time.Minute)
	d = recur(t, st, cfg, fp(1), at, IssueOpen)
	if d.Kind != Comment || d.Issue != 7 || d.Intent.Day != "2026-11-20" || d.Intent.Filing.Count != 2 || !d.Intent.Filing.LastSeen.Equal(at) {
		t.Fatalf("first repeat = %+v", d)
	}
	if err := st.CompleteComment(*d.Intent); err != nil {
		t.Fatal(err)
	}
	// Later the same day it only counts.
	for i := 3; i <= 4; i++ {
		at = at.Add(time.Minute)
		if d := recur(t, st, cfg, fp(1), at, IssueOpen); d.Kind != Count || d.Intent != nil {
			t.Fatalf("repeat %d = %+v, want a count only", i, d)
		}
	}
	rec := st.Record(targetRepo, fp(1))
	if rec.Count != 4 || !rec.LastSeen.Equal(at) || rec.Issue != 7 {
		t.Fatalf("record = %+v", rec)
	}
	// The next day it comments again, once.
	next := time.Date(2026, 11, 21, 0, 5, 0, 0, time.UTC)
	d = recur(t, st, cfg, fp(1), next, IssueOpen)
	if d.Kind != Comment || d.Intent.Day != "2026-11-21" || d.Intent.Filing.Count != 5 {
		t.Fatalf("next day = %+v", d)
	}
	if err := st.CompleteComment(*d.Intent); err != nil {
		t.Fatal(err)
	}
	if d := recur(t, st, cfg, fp(1), next.Add(time.Hour), IssueOpen); d.Kind != Count {
		t.Fatalf("second repeat of the next day = %s", d.Kind)
	}
	if got := len(st.Intents); got != 0 {
		t.Fatalf("%d intents left", got)
	}
}

func TestClosedIssueLeadsToANewLinkedIssue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(*State, *Config)
	}{
		{"plain", func(*State, *Config) {}},
		{"the day's comment was already posted", func(st *State, _ *Config) {
			rec := *st.Record(targetRepo, fp(1))
			rec.LastCommentDay = "2026-11-20"
			st.setRecord(targetRepo, fp(1), rec)
		}},
		{"the comment cap is spent", func(st *State, cfg *Config) {
			st.CommentDay, st.CommentsToday = "2026-11-20", cfg.MaxCommentsPerDay
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, cfg := NewState(), testConfig(targetRepo)
			fileIssue(t, st, cfg, fp(1), t0, 7)
			tt.setup(st, &cfg)
			at := t0.Add(time.Hour)
			d := recur(t, st, cfg, fp(1), at, IssueClosed)
			if d.Kind != OpenLinkedIssue || d.Issue != 7 || d.Intent.Kind != IntentCreate || d.Intent.Filing.Previous != 7 {
				t.Fatalf("decision = %+v, want a new issue linking 7", d)
			}
			if d.Intent.Filing.Count != 1 || !d.Intent.Filing.FirstSeen.Equal(at) {
				t.Errorf("new issue covers %d occurrences from %s", d.Intent.Filing.Count, d.Intent.Filing.FirstSeen)
			}
			if err := st.CompleteCreate(*d.Intent, 9, at); err != nil {
				t.Fatal(err)
			}
			rec := st.Record(targetRepo, fp(1))
			if rec.Issue != 9 || rec.Previous != 7 || rec.State != IssueOpen || rec.LastCommentDay != "" {
				t.Fatalf("record = %+v", rec)
			}
			// The new issue groups the next occurrence; the closed one is never used again.
			if d := recur(t, st, cfg, fp(1), at.Add(time.Minute), IssueOpen); d.Issue != 9 {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
}

func TestClosedIssueIsNeverCommentedOnOrCounted(t *testing.T) {
	t.Parallel()
	st, cfg := NewState(), testConfig(targetRepo)
	fileIssue(t, st, cfg, fp(1), t0, 7)
	d := recur(t, st, cfg, fp(1), t0.Add(time.Minute), IssueClosed)
	if d.Kind != OpenLinkedIssue || d.Intent == nil || d.Intent.Kind != IntentCreate {
		t.Fatalf("closed issue produced %+v", d)
	}
	for _, kind := range []Kind{Comment, Count, CheckState, AwaitCheck} {
		if d.Kind == kind {
			t.Fatalf("closed issue produced %s", kind)
		}
	}
}

func TestIssueCapsCountSkippedEventsIntoTheNextFiling(t *testing.T) {
	t.Parallel()
	st, cfg := NewState(), testConfig(targetRepo)
	cfg.MaxIssuesPerHour, cfg.MaxIssuesPerDay = 2, 3
	fileIssue(t, st, cfg, fp(1), t0, 1)
	fileIssue(t, st, cfg, fp(2), t0.Add(time.Minute), 2)

	// The hourly cap is spent: events count, no issue.
	at := t0.Add(2 * time.Minute)
	d := step(st, cfg, testReport(fp(3), at), nil, at)
	if d.Kind != OverCap || d.Cap != CapIssuesPerHour || d.Intent != nil || len(st.Intents) != 0 {
		t.Fatalf("decision = %+v", d)
	}
	d = step(st, cfg, testReport(fp(3), at.Add(time.Minute)), nil, at.Add(time.Minute))
	if d.Kind != OverCap {
		t.Fatalf("decision = %s", d.Kind)
	}
	if rec := st.Record(targetRepo, fp(3)); rec.Count != 2 || rec.SkippedCaps != 2 || rec.Issue != 0 {
		t.Fatalf("record = %+v", rec)
	}

	// An hour later the window has moved: the issue is opened with the counts.
	later := t0.Add(62 * time.Minute)
	d = step(st, cfg, testReport(fp(3), later), nil, later)
	if d.Kind != OpenIssue || d.Intent.Filing.Count != 3 || d.Intent.Filing.SkippedCaps != 2 || !d.Intent.Filing.FirstSeen.Equal(at) {
		t.Fatalf("decision = %+v", d)
	}
	if err := st.CompleteCreate(*d.Intent, 3, later); err != nil {
		t.Fatal(err)
	}
	if rec := st.Record(targetRepo, fp(3)); rec.SkippedCaps != 0 {
		t.Fatalf("skipped counts were not cleared by the filing: %+v", rec)
	}

	// The daily cap (3) is now spent, though the hour is free.
	later = later.Add(2 * time.Hour)
	if d := step(st, cfg, testReport(fp(4), later), nil, later); d.Kind != OverCap || d.Cap != CapIssuesPerDay {
		t.Fatalf("decision = %+v", d)
	}
	// A day after the first issue the window frees up.
	later = t0.Add(25 * time.Hour)
	d = step(st, cfg, testReport(fp(4), later), nil, later)
	if d.Kind != OpenIssue || d.Intent.Filing.SkippedCaps != 1 {
		t.Fatalf("decision = %+v", d)
	}
}

func TestZeroCapsAllowNoWrite(t *testing.T) {
	t.Parallel()
	st, cfg := NewState(), testConfig(targetRepo)
	cfg.MaxIssuesPerHour = 0
	if d := step(st, cfg, testReport(fp(1), t0), nil, t0); d.Kind != OverCap {
		t.Fatalf("decision = %s", d.Kind)
	}
	cfg = testConfig(targetRepo)
	fileIssue(t, st, cfg, fp(2), t0, 2)
	cfg.MaxCommentsPerDay = 0
	if d := recur(t, st, cfg, fp(2), t0.Add(time.Minute), IssueOpen); d.Kind != OverCap || d.Cap != CapCommentsPerDay {
		t.Fatalf("decision = %+v", d)
	}
}

func TestCommentCapCountsSkippedEventsIntoTheNextComment(t *testing.T) {
	t.Parallel()
	st, cfg := NewState(), testConfig(targetRepo)
	cfg.MaxCommentsPerDay = 1
	fileIssue(t, st, cfg, fp(1), t0, 1)
	fileIssue(t, st, cfg, fp(2), t0, 2)
	at := t0.Add(time.Minute)
	d := recur(t, st, cfg, fp(1), at, IssueOpen)
	if d.Kind != Comment {
		t.Fatalf("decision = %s", d.Kind)
	}
	if err := st.CompleteComment(*d.Intent); err != nil {
		t.Fatal(err)
	}
	// The second issue's comment is over the cap, twice.
	for i := 0; i < 2; i++ {
		at = at.Add(time.Minute)
		if d := recur(t, st, cfg, fp(2), at, IssueOpen); d.Kind != OverCap || d.Cap != CapCommentsPerDay {
			t.Fatalf("decision = %+v", d)
		}
	}
	next := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	d = recur(t, st, cfg, fp(2), next, IssueOpen)
	if d.Kind != Comment || d.Intent.Filing.SkippedCaps != 2 || d.Intent.Filing.Count != 4 {
		t.Fatalf("decision = %+v", d)
	}
	if err := st.CompleteComment(*d.Intent); err != nil {
		t.Fatal(err)
	}
	if rec := st.Record(targetRepo, fp(2)); rec.SkippedCaps != 0 || rec.LastCommentDay != "2026-11-21" {
		t.Fatalf("record = %+v", rec)
	}
}

func TestQueueDropsAreReportedByTheNextFiling(t *testing.T) {
	t.Parallel()
	st, cfg := NewState(), testConfig(targetRepo)
	st.NoteQueueDrops(3)
	st.NoteQueueDrops(-2)
	d := step(st, cfg, testReport(fp(1), t0), nil, t0)
	if d.Intent.Filing.SkippedDropped != 3 {
		t.Fatalf("filing = %+v", d.Intent.Filing)
	}
	st.NoteQueueDrops(1) // dropped while the write was in flight
	if err := st.CompleteCreate(*d.Intent, 1, t0); err != nil {
		t.Fatal(err)
	}
	if st.QueueDrops != 1 {
		t.Fatalf("queue drops = %d, want only the ones no filing reported", st.QueueDrops)
	}
}

func TestIntentsMustBeReconciledBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	for _, kind := range []IntentKind{IntentCreate, IntentComment} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			st, cfg := NewState(), testConfig(targetRepo)
			fileIssue(t, st, cfg, fp(1), t0, 7)
			var intent Intent
			if kind == IntentCreate {
				intent = *step(st, cfg, testReport(fp(2), t0), nil, t0).Intent
			} else {
				intent = *recur(t, st, cfg, fp(1), t0.Add(time.Minute), IssueOpen).Intent
			}
			if intent.Kind != kind {
				t.Fatalf("intent = %+v", intent)
			}
			// Another fingerprint, or the same one, a new event or an awaiting
			// recurrence: all must reconcile first.
			for _, in := range []Input{
				{Report: ptr(testReport(fp(3), t0.Add(time.Hour)))},
				{Report: ptr(testReport(fp(1), t0.Add(time.Hour)))},
				{},
			} {
				d := Decide(st, cfg, in, t0.Add(time.Hour))
				if d.Kind != Reconcile || d.Intent == nil || d.Intent.Kind != kind || d.Intent.Fingerprint != intent.Fingerprint {
					t.Fatalf("decision = %+v, want reconcile of %+v", d, intent)
				}
			}
			// Once resolved, writes go on.
			switch kind {
			case IntentCreate:
				err := st.CompleteCreate(intent, 8, t0)
				if err != nil {
					t.Fatal(err)
				}
			default:
				if err := st.CompleteComment(intent); err != nil {
					t.Fatal(err)
				}
			}
			if d := Decide(st, cfg, Input{Report: ptr(testReport(fp(5), t0.Add(time.Hour)))}, t0.Add(time.Hour)); d.Kind != OpenIssue {
				t.Fatalf("decision = %s", d.Kind)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestAttemptsAndAbandonedIntents(t *testing.T) {
	t.Parallel()
	st, cfg := NewState(), testConfig(targetRepo)
	d := step(st, cfg, testReport(fp(1), t0), nil, t0)
	if n := st.NoteAttempt(*d.Intent); n != 1 {
		t.Fatalf("attempts = %d", n)
	}
	if n := st.NoteAttempt(*d.Intent); n != 2 {
		t.Fatalf("attempts = %d", n)
	}
	st.AbandonIntent(*d.Intent)
	if len(st.Intents) != 0 || st.NoteAttempt(*d.Intent) != 0 {
		t.Fatal("the intent was not cleared")
	}
	// Given up, the next occurrence is decided afresh and keeps its count.
	d = step(st, cfg, testReport(fp(1), t0.Add(time.Hour)), nil, t0.Add(time.Hour))
	if d.Kind != OpenIssue || d.Intent.Filing.Count != 2 {
		t.Fatalf("decision = %+v", d)
	}
}

func TestDeferredStateChecksAreOfferedAgain(t *testing.T) {
	t.Parallel()
	st, cfg := NewState(), testConfig(targetRepo)
	cfg.CheckInterval = time.Hour
	fileIssue(t, st, cfg, fp(1), t0, 7) // the issue was read as open at t0

	// A recurrence inside the spacing cannot check the issue: it waits.
	at := t0.Add(10 * time.Minute)
	d := step(st, cfg, testReport(fp(1), at), nil, at)
	if d.Kind != AwaitCheck || d.Issue != 7 {
		t.Fatalf("decision = %+v", d)
	}
	if aw := st.AwaitingFor(targetRepo); len(aw) != 1 || aw[0].Fingerprint != fp(1) || aw[0].Issue != 7 || !aw[0].At.Equal(at) {
		t.Fatalf("awaiting = %+v", aw)
	}
	// It survives a save and a load.
	path := t.TempDir() + "/state.json"
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	// Another recurrence does not duplicate the entry.
	at2 := t0.Add(20 * time.Minute)
	if d := step(st, cfg, testReport(fp(1), at2), nil, at2); d.Kind != AwaitCheck {
		t.Fatalf("decision = %s", d.Kind)
	}
	if len(st.Awaiting) != 1 {
		t.Fatalf("awaiting = %+v", st.Awaiting)
	}

	// With no further event, nothing is offered until the read is allowed.
	d = Step(st, cfg, Input{}, t0.Add(30*time.Minute))
	if d.Kind != Idle || !d.NotBefore.Equal(t0.Add(time.Hour)) {
		t.Fatalf("decision = %+v, want idle until %s", d, t0.Add(time.Hour))
	}
	// Then the check is offered again on its own, and the closed issue gets a new linked one.
	ready := t0.Add(time.Hour)
	d = Step(st, cfg, Input{}, ready)
	if d.Kind != CheckState || d.Issue != 7 || d.Fingerprint != fp(1) {
		t.Fatalf("decision = %+v", d)
	}
	check := &Check{Repository: targetRepo, Issue: 7, State: IssueClosed, At: ready}
	d = Step(st, cfg, Input{Check: check}, ready)
	if d.Kind != OpenLinkedIssue || d.Issue != 7 || d.Intent.Filing.Previous != 7 || d.Intent.Filing.Count != 2 || !d.Intent.Filing.FirstSeen.Equal(at) {
		t.Fatalf("decision = %+v", d)
	}
	if len(st.Awaiting) != 0 {
		t.Fatalf("awaiting = %+v", st.Awaiting)
	}
	if err := st.CompleteCreate(*d.Intent, 9, ready); err != nil {
		t.Fatal(err)
	}
	if d := Step(st, cfg, Input{}, ready.Add(time.Hour)); d.Kind != Idle || !d.NotBefore.IsZero() {
		t.Fatalf("decision = %+v", d)
	}
}

func TestDeferredCheckOfAnOpenIssueResolvesWithoutANewIssue(t *testing.T) {
	t.Parallel()
	st, cfg := NewState(), testConfig(targetRepo)
	cfg.CheckInterval = time.Hour
	fileIssue(t, st, cfg, fp(1), t0, 7)
	at := t0.Add(10 * time.Minute)
	if d := step(st, cfg, testReport(fp(1), at), nil, at); d.Kind != AwaitCheck {
		t.Fatalf("decision = %s", d.Kind)
	}
	ready := t0.Add(time.Hour)
	if d := Step(st, cfg, Input{}, ready); d.Kind != CheckState {
		t.Fatalf("decision = %s", d.Kind)
	}
	d := Step(st, cfg, Input{Check: &Check{Repository: targetRepo, Issue: 7, State: IssueOpen, At: ready}}, ready)
	if d.Kind != Comment || d.Issue != 7 || d.Intent.Filing.Count != 2 {
		t.Fatalf("decision = %+v", d)
	}
	if len(st.Awaiting) != 0 {
		t.Fatalf("awaiting = %+v", st.Awaiting)
	}
	if rec := st.Record(targetRepo, fp(1)); rec.Pending != 0 || rec.Issue != 7 {
		t.Fatalf("record = %+v", rec)
	}
}

func TestFailedReadsSpaceTheNextCheck(t *testing.T) {
	t.Parallel()
	st, cfg := NewState(), testConfig(targetRepo)
	cfg.CheckInterval = time.Hour
	fileIssue(t, st, cfg, fp(1), t0, 7)
	at := t0.Add(2 * time.Hour)
	rep := testReport(fp(1), at)
	if d := Step(st, cfg, Input{Report: &rep}, at); d.Kind != CheckState {
		t.Fatalf("decision = %s", d.Kind)
	}
	st.NoteCheckAttempt(targetRepo, fp(1), at) // the read failed
	d := Step(st, cfg, Input{Report: &rep}, at.Add(time.Minute))
	if d.Kind != AwaitCheck {
		t.Fatalf("decision = %s", d.Kind)
	}
}

func TestOtherRepositoriesIssuesAreNeverUsed(t *testing.T) {
	t.Parallel()
	const repoB = "kpenfound/other"
	st := NewState()
	cfgA, cfgB := testConfig(targetRepo), testConfig(repoB)
	fileIssue(t, st, cfgA, fp(1), t0, 7)

	// Target B: the fingerprint has no issue there, whatever A's number is.
	at := t0.Add(time.Minute)
	d := step(st, cfgB, testReport(fp(1), at), nil, at)
	if d.Kind != OpenIssue || d.Repository != repoB || d.Intent.Repository != repoB || d.Intent.Filing.Previous != 0 || d.Intent.Filing.Count != 1 {
		t.Fatalf("decision = %+v", d)
	}
	// B's intent is the only one reconciled while B is the target.
	if d := Decide(st, cfgB, Input{}, at); d.Kind != Reconcile || !sameRepo(d.Intent.Repository, repoB) {
		t.Fatalf("decision = %+v", d)
	}
	// B gets an issue with the same number as A's.
	if err := st.CompleteCreate(st.IntentsFor(repoB)[0], 7, at); err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Minute)
	rep := testReport(fp(1), later)
	first := Step(st, cfgB, Input{Report: &rep}, later)
	if first.Kind != CheckState || first.Issue != 7 || first.Repository != repoB {
		t.Fatalf("decision = %+v", first)
	}
	// A check read in A is not used for B's issue, though the numbers collide.
	wrong := &Check{Repository: targetRepo, Issue: 7, State: IssueClosed, At: later}
	if d := Step(st, cfgB, Input{Report: &rep, Check: wrong}, later); d.Kind != CheckState {
		t.Fatalf("a check from another repository was used: %+v", d)
	}
	if rec := st.Record(repoB, fp(1)); rec.Issue != 7 || rec.State != IssueOpen {
		t.Fatalf("record = %+v", rec)
	}

	// Back to A: the fingerprint groups with its A issue again; no duplicate.
	back := later.Add(time.Minute)
	rep = testReport(fp(1), back)
	first = Step(st, cfgA, Input{Report: &rep}, back)
	if first.Kind != CheckState || first.Issue != 7 || first.Repository != targetRepo {
		t.Fatalf("decision = %+v", first)
	}
	d = Step(st, cfgA, Input{Report: &rep, Check: &Check{Repository: targetRepo, Issue: 7, State: IssueOpen, At: back}}, back)
	if d.Kind != Comment || d.Issue != 7 || d.Intent.Repository != targetRepo {
		t.Fatalf("decision = %+v", d)
	}
	if err := st.CompleteComment(*d.Intent); err != nil {
		t.Fatal(err)
	}
	if st.Record(targetRepo, fp(1)).Count != 2 || st.Record(repoB, fp(1)).Count != 1 {
		t.Fatalf("counts crossed repositories: A %+v B %+v", st.Record(targetRepo, fp(1)), st.Record(repoB, fp(1)))
	}
}

func TestIntentsOfOtherRepositoriesYieldNoWrite(t *testing.T) {
	t.Parallel()
	const repoB = "kpenfound/other"
	st := NewState()
	cfgA, cfgB := testConfig(targetRepo), testConfig(repoB)
	d := step(st, cfgA, testReport(fp(1), t0), nil, t0)
	intentA := *d.Intent

	// While B is the target, A's unresolved intent is not reconciled and
	// does not stop B's writes; no decision mentions A.
	at := t0.Add(time.Minute)
	d = step(st, cfgB, testReport(fp(1), at), nil, at)
	if d.Kind != OpenIssue || d.Intent.Repository != repoB {
		t.Fatalf("decision = %+v", d)
	}
	if err := st.CompleteCreate(*d.Intent, 3, at); err != nil {
		t.Fatal(err)
	}
	if d := Step(st, cfgB, Input{}, at); d.Kind != Idle {
		t.Fatalf("decision = %+v", d)
	}
	if got := st.IntentsFor(targetRepo); len(got) != 1 || got[0].Attempts != 0 {
		t.Fatalf("A's intent was touched: %+v", got)
	}
	// Back to A the intent is reconciled before anything is written.
	d = Decide(st, cfgA, Input{Report: ptr(testReport(fp(2), at))}, at)
	if d.Kind != Reconcile || d.Intent.Fingerprint != intentA.Fingerprint || !sameRepo(d.Intent.Repository, targetRepo) {
		t.Fatalf("decision = %+v", d)
	}
}

func TestRepositoryMatchingIgnoresCase(t *testing.T) {
	t.Parallel()
	st := NewState()
	fileIssue(t, st, testConfig("KPenfound/Osmia"), fp(1), t0, 7)
	if st.Record("kpenfound/osmia", fp(1)) == nil {
		t.Fatal("repository names are case-insensitive on GitHub")
	}
}

func TestDecideDoesNotChangeTheState(t *testing.T) {
	t.Parallel()
	st, cfg := NewState(), testConfig(targetRepo)
	fileIssue(t, st, cfg, fp(1), t0, 7)
	before, _ := json.Marshal(st)
	rep := testReport(fp(1), t0.Add(time.Minute))
	for _, in := range []Input{
		{Report: &rep},
		{Report: &rep, Check: &Check{Repository: targetRepo, Issue: 7, State: IssueClosed, At: t0}},
		{},
	} {
		Decide(st, cfg, in, t0.Add(time.Minute))
	}
	if after, _ := json.Marshal(st); string(after) != string(before) {
		t.Fatalf("Decide changed the state:\n%s\n%s", before, after)
	}
}
