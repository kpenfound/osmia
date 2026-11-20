package errorreport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMissingStateFileLoadsEmpty(t *testing.T) {
	t.Parallel()
	st, err := LoadState(filepath.Join(t.TempDir(), "error-reports.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || len(st.Repositories) != 0 || len(st.Intents) != 0 || len(st.Awaiting) != 0 || st.QueueDrops != 0 {
		t.Fatalf("state = %+v", st)
	}
}

func TestStateRoundTripsThroughAnAtomicSave(t *testing.T) {
	t.Parallel()
	const repoB = "kpenfound/other"
	dir := t.TempDir()
	path := filepath.Join(dir, "error-reports.json")
	st := NewState()
	cfgA, cfgB := testConfig(targetRepo), testConfig(repoB)
	cfgA.CheckInterval = time.Hour
	fileIssue(t, st, cfgA, fp(1), t0, 7)
	fileIssue(t, st, cfgB, fp(1), t0, 7)
	// An awaiting check in A, an unresolved comment intent in B and a create
	// intent in A, with counts, a cap window and queue drops.
	at := t0.Add(5 * time.Minute)
	if d := step(st, cfgA, testReport(fp(1), at), nil, at); d.Kind != AwaitCheck {
		t.Fatalf("decision = %s", d.Kind)
	}
	cfgB.MaxCommentsPerDay = 3
	if d := recur(t, st, cfgB, fp(1), at, IssueOpen); d.Kind != Comment {
		t.Fatalf("decision = %s", d.Kind)
	}
	if d := step(st, cfgA, testReport(fp(2), at), nil, at); d.Kind != OpenIssue {
		t.Fatalf("decision = %s", d.Kind)
	}
	st.NoteQueueDrops(4)
	st.NoteAttempt(st.IntentsFor(targetRepo)[0])

	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(st)
	got, _ := json.Marshal(loaded)
	if string(got) != string(want) {
		t.Fatalf("state changed across save and load:\n%s\n%s", want, got)
	}
	if len(loaded.Intents) != 2 || len(loaded.Awaiting) != 1 || len(loaded.IssueTimes) != 3 || loaded.QueueDrops != 4 {
		t.Fatalf("loaded = %+v", loaded)
	}
	in := loaded.IntentsFor(targetRepo)[0]
	if in.Kind != IntentCreate || in.Attempts != 1 || len(in.Labels) != 2 || in.Filing.Report.Fingerprint != fp(2) || in.RecordedAt.IsZero() {
		t.Fatalf("intent = %+v", in)
	}
	// What was loaded decides like what was saved.
	for _, cfg := range []Config{cfgA, cfgB} {
		a, b := Decide(st, cfg, Input{}, at.Add(time.Minute)), Decide(loaded, cfg, Input{}, at.Add(time.Minute))
		if a.Kind != b.Kind || a.Fingerprint != b.Fingerprint {
			t.Fatalf("decisions differ: %+v %+v", a, b)
		}
	}

	// No temporary file is left beside the state.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "error-reports.json" {
		t.Fatalf("directory = %v, %v", entries, err)
	}
	// Saving again replaces the file.
	loaded.NoteQueueDrops(1)
	if err := loaded.Save(path); err != nil {
		t.Fatal(err)
	}
	again, err := LoadState(path)
	if err != nil || again.QueueDrops != 5 {
		t.Fatalf("state = %+v, %v", again, err)
	}
}

func TestSaveKeepsTheOldStateWhenItCannotWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "error-reports.json")
	st := NewState()
	st.NoteQueueDrops(2)
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	st.NoteQueueDrops(1)
	if err := st.Save(filepath.Join(dir, "missing", "error-reports.json")); err == nil {
		t.Fatal("saving into a missing directory succeeded")
	}
	loaded, err := LoadState(path)
	if err != nil || loaded.QueueDrops != 2 {
		t.Fatalf("state = %+v, %v", loaded, err)
	}
}

func TestLoadRejectsDamagedState(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"not json":    "{",
		"old version": `{"version": 99}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "error-reports.json")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadState(path); err == nil {
				t.Fatal("LoadState succeeded")
			}
		})
	}
}

func TestRecordsAreOnlyFoundUnderTheirRepository(t *testing.T) {
	t.Parallel()
	st := NewState()
	fileIssue(t, st, testConfig(targetRepo), fp(1), t0, 7)
	if st.Record("kpenfound/other", fp(1)) != nil || st.Record("", fp(1)) != nil {
		t.Fatal("a record was found under another repository")
	}
	if st.Record(targetRepo, fp(1)).Issue != 7 {
		t.Fatal("record missing")
	}
	data, _ := json.Marshal(st)
	if !strings.Contains(string(data), `"repositories":{"kpenfound/osmia"`) {
		t.Fatalf("state is not keyed by repository: %s", data)
	}
}

func TestIntentSinceAllowsForClockSkew(t *testing.T) {
	t.Parallel()
	in := Intent{RecordedAt: t0}
	if !in.Since().Equal(t0.Add(-ClockSkew)) {
		t.Fatalf("since = %s", in.Since())
	}
	if !in.CreatedByIntent(t0.Add(-time.Minute)) || in.CreatedByIntent(t0.Add(-time.Hour)) || !in.CreatedByIntent(t0.Add(time.Second)) {
		t.Fatal("CreatedByIntent does not follow the skew margin")
	}
}

func TestMatchersRecogniseWritesRegardlessOfLabels(t *testing.T) {
	t.Parallel()
	st, cfg := NewState(), testConfig(targetRepo)
	cfg.Labels = []string{"first"}
	d := step(st, cfg, testReport(fp(1), t0), nil, t0)
	issue, err := RenderIssue(d.Intent.Filing)
	if err != nil {
		t.Fatal(err)
	}
	// The labels change before the retry; the body is found just the same.
	cfg.Labels = []string{"something", "else"}
	if !MatchesIssue(issue.Body, d.Intent.Fingerprint) {
		t.Fatal("the issue is not recognised")
	}
	other := step(st, testConfig("kpenfound/other"), testReport(fp(2), t0), nil, t0)
	otherIssue, err := RenderIssue(other.Intent.Filing)
	if err != nil {
		t.Fatal(err)
	}
	if MatchesIssue(otherIssue.Body, d.Intent.Fingerprint) || MatchesIssue(issue.Body, other.Intent.Fingerprint) {
		t.Fatal("an issue was recognised for another fingerprint")
	}
}
