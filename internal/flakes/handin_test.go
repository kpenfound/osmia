package flakes

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

func localEvidence() Evidence {
	return Evidence{
		Engine:      EngineLocal,
		Flaky:       []string{groupA + "&go-test=TestOne", groupB + "&go-test=TestOther"},
		CoRun:       []string{groupA + "&go-test=TestTwo", "dag+check://?check=lint&staticcheck-project=."},
		Parallelism: 6,
		Output:      "== local engine ==\nFAIL internal/a TestOne",
		OutputPath:  "units/u1/checks-1-output.txt",
		Diagnostics: []string{"--- FAIL: TestOne [passed on rerun]\n    a_test.go:12: timeout"},
		Timings:     []Timing{{Check: groupA + "&go-test=TestOne", Duration: 2500 * time.Millisecond}},
	}
}

func recordWith(t *testing.T, f *fixture, group string, o OccurrenceInput) Group {
	t.Helper()
	if _, _, err := f.store.Record(context.Background(), in(group, o)); err != nil {
		t.Fatal(err)
	}
	g, _, err := f.store.Group(group)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestHandInHoldsEveryEvidenceItemOfALocalUnitRun(t *testing.T) {
	f := newFixture(t)
	o := unitRun("1", groupA+"&go-test=TestOne")
	o.Evidence = localEvidence()
	g := recordWith(t, f, groupA, o)
	text, err := RenderHandIn(g, o.Run, "")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"flaky link":                  "`" + groupA + "&go-test=TestOne`",
		"flaky link of another group": "`" + groupB + "&go-test=TestOther`",
		"run record":                  "`units/u1/checks-1.json`",
		"originating unit":            "unit `u1` of workstream `" + string(origin) + "`",
		"captured output":             "FAIL internal/a TestOne",
		"output path":                 "`units/u1/checks-1-output.txt`",
		"rerun diagnostics":           "--- FAIL: TestOne [passed on rerun]",
		"co-run check":                "`" + groupA + "&go-test=TestTwo`",
		"co-run lint":                 "staticcheck-project",
		"parallelism":                 "Parallelism: 6",
		"engine":                      "`local`",
		"timings":                     "### Timings",
		"a timing":                    "2.5s",
		"group":                       "Flake group: `" + groupA + "`",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%s: %q is missing from the hand-in:\n%s", name, want, text)
		}
	}
	if strings.Contains(text, "not reported") || strings.Contains(text, "Earlier history") {
		t.Errorf("the hand-in reports missing items or history that there are none of:\n%s", text)
	}
}

func TestHandInOfAFinalReviewOnCloudStatesWhatCloudDidNotReport(t *testing.T) {
	f := newFixture(t)
	o := OccurrenceInput{
		Run: "final/checks-2.json", Source: Source{Workstream: origin, FinalReview: true}, Links: []string{"test/internal/a/TestOne"},
		Evidence: Evidence{
			Engine: EngineCloud, Flaky: []string{"test/internal/a/TestOne"}, CoRun: []string{"lint", "test/internal/a/TestOne", "build"},
			Diagnostics: []string{"Failed in 2m33s [passed on rerun]"},
			Traces:      []Trace{{Check: "test/internal/a/TestOne", Role: "failed", Ref: "0a1b2c3d4e5f"}, {Check: "test/internal/a/TestOne", Role: "rerun", Ref: "ffff0000"}},
			Unavailable: []string{"output", "parallelism"},
		},
	}
	g := recordWith(t, f, groupA, o)
	text, err := RenderHandIn(g, o.Run, "")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"final review":          "the final review of workstream `" + string(origin) + "`",
		"run record":            "`final/checks-2.json`",
		"output not reported":   "captured output is not reported by cloud; read `dagger trace 0a1b2c3d4e5f` (test/internal/a/TestOne, failed), `dagger trace ffff0000`",
		"parallelism not given": "parallelism of the invocation is not reported by cloud; read `dagger trace 0a1b2c3d4e5f`",
		"engine":                "`cloud`",
		"co-run checks":         "- `build`",
		"diagnostics":           "Failed in 2m33s [passed on rerun]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%s: %q is missing from the hand-in:\n%s", name, want, text)
		}
	}
	if strings.Contains(text, "Timings") {
		t.Errorf("timings that were not recorded are shown:\n%s", text)
	}
	o.Evidence.Unavailable, o.Evidence.Diagnostics, o.Evidence.CoRun = nil, nil, nil
	o.Run = "final/checks-3.json"
	g = recordWith(t, f, groupA, o)
	text, _ = RenderHandIn(g, o.Run, "")
	for _, want := range []string{"captured output is not reported by cloud", "`[passed on rerun]` diagnostics are not reported by cloud", "other checks that ran in the same invocation are not reported by cloud", "parallelism of the invocation is not reported by cloud"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q is missing:\n%s", want, text)
		}
	}
}

func TestHandInWithoutTracesPointsAtTheRunRecord(t *testing.T) {
	f := newFixture(t)
	o := unitRun("1", groupA+"&go-test=TestOne")
	o.Evidence = Evidence{Engine: EngineLocal}
	text, err := RenderHandIn(recordWith(t, f, groupA, o), o.Run, "")
	if err != nil || !strings.Contains(text, "not reported by local; read the run record `units/u1/checks-1.json`") {
		t.Fatalf("%v:\n%s", err, text)
	}
	o.Evidence = Evidence{}
	o.Run = "units/u1/checks-2.json"
	text, _ = RenderHandIn(recordWith(t, f, groupA, o), o.Run, "")
	if !strings.Contains(text, "not reported by the engine; read the run record") || !strings.Contains(text, "Engine: not recorded.") {
		t.Fatalf("an unrecorded engine:\n%s", text)
	}
}

func TestHandInCarriesTheGroupsEarlierHistory(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := recordOne(t, f, groupA, "1")
	second := recordOne(t, f, groupA, "2")
	if _, err := f.store.Decline(ctx, groupA, first.Run, "the flaky test was added by the unit itself"); err != nil {
		t.Fatal(err)
	}
	d, _ := f.store.Reserve(ctx, groupA, second.Run)
	f.store.Opened(ctx, groupA, second.Run, "")
	f.store.CloseWorkstream(ctx, d.Workstream)
	third := recordOne(t, f, groupA, "3")
	g, _, _ := f.store.Group(groupA)

	text, err := RenderHandIn(g, third.Run, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Earlier history of this flake group",
		"`units/u1/checks-1.json`; it was declined: the flaky test was added by the unit itself",
		"`units/u1/checks-2.json`; it opened flake-fix workstream `" + string(d.Workstream) + "`",
		"the flake-fix workstream `" + string(d.Workstream) + "` of this group was delivered or abandoned",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%q is missing:\n%s", want, text)
		}
	}
	if first := strings.Index(text, "checks-1.json`; it was"); first > strings.Index(text, "checks-2.json`; it opened") {
		t.Error("history is out of order")
	}
	if strings.Contains(text, "checks-3.json`; ") {
		t.Errorf("the occurrence is in its own history:\n%s", text)
	}
	if _, err := RenderHandIn(g, "units/none.json", ""); err == nil {
		t.Error("rendered an unknown occurrence")
	}
}

func TestHandInKeepsTheManagersNotesInTheirOwnSection(t *testing.T) {
	f := newFixture(t)
	o := recordOne(t, f, groupA, "1")
	g, _, _ := f.store.Group(groupA)
	notes := "Check whether `internal/a` shares a port with `internal/b`.\n\n## Not a heading of the hand-in"
	text, _ := RenderHandIn(g, o.Run, notes)
	i := strings.Index(text, NotesHeading)
	if i < 0 || strings.LastIndex(text, NotesHeading) != i || !strings.HasSuffix(strings.TrimSpace(text), notes) {
		t.Fatalf("the notes are not alone at the end:\n%s", text)
	}
	before := text[:i]
	if strings.Contains(before, "shares a port") {
		t.Errorf("the notes leak into the evidence:\n%s", before)
	}
	again, _ := RenderHandIn(g, o.Run, "")
	if !strings.HasSuffix(strings.TrimSpace(again), "None.") || again[:strings.Index(again, NotesHeading)] != before {
		t.Errorf("the service-written part changes with the notes")
	}
}

func TestHandInOfAnUnresolvedCloudName(t *testing.T) {
	f := newFixture(t)
	o := OccurrenceInput{Run: "final/checks-1.json", Source: Source{Workstream: origin, FinalReview: true}, Links: []string{"test-split/test-cli-engine"}, Evidence: Evidence{Engine: EngineCloud}}
	o.Unresolved = true
	g := recordWith(t, f, "test-split/test-cli-engine", o)
	text, _ := RenderHandIn(g, o.Run, "")
	if !strings.Contains(text, "a Dagger Cloud check name that matches no known check") {
		t.Errorf("%s", text)
	}
}

func TestHandInFencesOutputThatHoldsFences(t *testing.T) {
	f := newFixture(t)
	o := unitRun("1", groupA+"&go-test=TestOne")
	o.Evidence.Output = "before\n```\ninside\n```\nafter"
	text, _ := RenderHandIn(recordWith(t, f, groupA, o), o.Run, "")
	if !strings.Contains(text, "````\nbefore\n```\ninside\n```\nafter\n````") {
		t.Errorf("fences:\n%s", text)
	}
}

var (
	requiresChange  = regexp.MustCompile(`(?i)\b(must|should|need to|needs to|has to|have to|required to|shall|please|always)\b[^.\n]*\b(chang|modif|edit|updat|touch|fix)\w*\b[^.\n]*\b(test|file|\.go)\b`)
	requiresRepeats = regexp.MustCompile(`(?i)consecutive|deterministic|\bN times\b|\bin a row\b|\b\d+ (successful |passing )?(runs|passes|times)\b|reproduc\w* (it |the flake )?(reliably|consistently)`)
	instructsAFile  = regexp.MustCompile(`(?i)\b(change|modify|edit|update|rewrite|delete|remove|skip|disable|retry|add a retry to)\b[^.\n]*(named test|the test|the flaky test|\.go\b|\bfile\b)`)
)

func TestHandInNeverRequiresChangingTheTestOrAFileOrRepeatedPasses(t *testing.T) {
	f := newFixture(t)
	local := unitRun("1", groupA+"&go-test=TestOne")
	local.Evidence = localEvidence()
	cloud := OccurrenceInput{Run: "final/checks-2.json", Source: Source{Workstream: origin, FinalReview: true}, Links: []string{"test/internal/a/TestOne"},
		Evidence: Evidence{Engine: EngineCloud, Traces: []Trace{{Ref: "abc"}}}}
	for _, o := range []OccurrenceInput{local, cloud} {
		g := recordWith(t, f, groupA, o)
		text, err := RenderHandIn(g, o.Run, "")
		if err != nil {
			t.Fatal(err)
		}
		instructions := text[strings.Index(text, "## What this workstream is for"):strings.Index(text, NotesHeading)]
		for name, re := range map[string]*regexp.Regexp{"a required change": requiresChange, "required repeats": requiresRepeats, "an instruction to change a file or test": instructsAFile} {
			if m := re.FindString(instructions); m != "" {
				t.Errorf("%s: the hand-in says %q:\n%s", name, m, instructions)
			}
		}
		if m := requiresRepeats.FindString(text); m != "" {
			t.Errorf("the hand-in says %q", m)
		}
	}
}

func TestHandInStatesTheFramingScopeOutcomeAndReviewerGuidance(t *testing.T) {
	f := newFixture(t)
	o := recordOne(t, f, groupA, "1")
	g, _, _ := f.store.Group(groupA)
	text, _ := RenderHandIn(g, o.Run, "")
	flat := strings.Join(strings.Fields(text), " ")
	for name, want := range map[string]string{
		"symptom framing":       "where it was seen, which is not necessarily where the fault is",
		"cause scope":           "Find the cause of the flakiness these links show and fix it",
		"anywhere the cause is": "anywhere else it lives",
		"outcome":               "the cause it identified is addressed, and the reasoning and the evidence for that cause are recorded",
		"reviewer guidance":     "do not raise a finding only because the named test is unchanged, or because the flake could not be reproduced on demand",
		"reviewers still judge": "Judge whether the stated cause is plausible and supported by the evidence, and whether the change addresses it",
		"stress runs optional":  "are welcome as evidence; they are not required",
		"no change needed":      `the existing "no change needed" escalation to the owner applies`,
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("%s: %q is missing:\n%s", name, want, text)
		}
	}
}
