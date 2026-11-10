package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/status"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestFinishNoticeWithOptionalOwnerAction(t *testing.T) {
	t.Parallel()
	base := "Unit parser is checking: its mason reported done on turn first; its report is units/parser/report.json revision 2."
	for _, tc := range []struct {
		name        string
		card        *coreadapter.Card
		ownerAction string
		want        string
	}{
		{"no card", nil, "", base},
		{"headline only", &coreadapter.Card{Headline: "Parser is ready"}, "", base + " Headline: Parser is ready"},
		{"card with owner action", &coreadapter.Card{Headline: "Parser is ready", NeedsYou: "Review the candidate."}, "Review the candidate.", base + " Headline: Parser is ready; Needs you: Review the candidate."},
		{"owner action without card", nil, "Review the candidate.", base + " Headline: " + ownerActionHeadline + "; Needs you: Review the candidate."},
		{"owner action that cannot be shown", &coreadapter.Card{Headline: "Parser is ready"}, ownerActionWording, base + " Headline: Parser is ready; Needs you: " + ownerActionWording},
	} {
		if got := finishNotice("parser", "first", "units/parser/report.json", 2, tc.card, tc.ownerAction); got != tc.want {
			t.Fatalf("%s: notice %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestServiceWrittenOwnerActionTextMeetsTheCardRules(t *testing.T) {
	t.Parallel()
	card := coreadapter.Card{Headline: ownerActionHeadline, Happened: "The unit is checking.", NeedsYou: ownerActionWording}
	if got, err := status.CheckCard(card, nil); err != nil || got != card {
		t.Fatalf("service-written card %+v: %+v, %v", card, got, err)
	}
}

type idleTurns struct{}

func (idleTurns) Run(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return coreadapter.SessionResult{}, nil
}

// doneThrough starts the unit resume, has its mason call done with args, ends
// the mason's turn with the outcome the reporting runner makes of it, and
// finishes the unit. It returns the done result, the stored report and the
// chief-of-staff event of the move to checking.
func doneThrough(t *testing.T, args map[string]any) (map[string]any, UnitReport, trace.Event) {
	t.Helper()
	ctx := context.Background()
	f := newBuildFixture(t, "card")
	m := f.controller()
	b, found, err := m.read(stream)
	must(t, err)
	if !found {
		t.Fatal("fixture workstream is not building")
	}
	started, blocked, err := m.start(ctx, b, "resume")
	must(t, err)
	if !started || blocked {
		t.Fatal("fixture unit resume did not start")
	}
	w, _, err := f.units().open(ctx, stream, "resume")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(w.Path, masonWrote), []byte("package trace\n"), 0644))

	scope := coreadapter.Scope{Project: string(f.repository.Project()), Workstream: string(stream), Unit: "resume", Thread: masonAgent("resume"), Turn: "t", Role: masonRole}
	reports := &masonReports{}
	raw, err := json.Marshal(args)
	must(t, err)
	out, err := reports.tool(f.repository, scope).Handle(ctx, raw)
	must(t, err)
	var result map[string]any
	must(t, json.Unmarshal(out, &result))
	if result["recorded"] != true {
		t.Fatalf("done refused: %s", out)
	}
	ran, err := (&reportingTurns{Turns: idleTurns{}, reports: reports}).Run(ctx, coreadapter.PreparedTurn{Scope: scope})
	must(t, err)
	turns := intakefixture.TurnScenario{Root: func() config.Root { return f.cfg.Root }, Project: f.repository.Project(), Now: f.clock.Now, ForemanActor: foremanActor}
	turns.CaptureTurn(t, f.repository, stream, masonAgent("resume"), ran.Outcome)

	b, _, err = m.read(stream)
	must(t, err)
	moved, blocked, err := m.finish(ctx, b, "resume")
	must(t, err)
	if !moved || blocked {
		t.Fatalf("the unit did not move to checking: moved %t, blocked %t", moved, blocked)
	}
	docs, err := trace.Read[trace.Document](f.repository, stream)
	must(t, err)
	var report UnitReport
	for _, d := range docs {
		if d.ID == reportDocument("resume") {
			must(t, json.Unmarshal([]byte(d.Content), &report))
		}
	}
	outbox, err := f.repository.Outbox(stream)
	must(t, err)
	for _, entry := range outbox {
		if entry.TransitionID == checkingTransitionID("resume", 1) && intakefixture.ChiefEvent(entry.Event) {
			return result, report, entry.Event
		}
	}
	t.Fatal("the move to checking told the chief of staff nothing")
	return nil, UnitReport{}, trace.Event{}
}

func TestDoneRecordsEveryReportWhateverItsCardHolds(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("The storage client resumes uploads ", 8)
	for _, tc := range []struct {
		name        string
		args        map[string]any
		check       func(t *testing.T, card *coreadapter.Card)
		adjustments []string
		kind        string
		needs       string
	}{
		{
			name: "a complete card",
			args: map[string]any{"outcome": "Built", "headline": " Uploads  resume ", "happened": "Chunks resume.", "needs_you": ""},
			check: func(t *testing.T, card *coreadapter.Card) {
				if card == nil || *card != (coreadapter.Card{Headline: "Uploads resume", Happened: "Chunks resume."}) {
					t.Fatalf("card %+v", card)
				}
			},
			kind: trace.ProgressKind,
		},
		{
			name: "a happened field over its limit",
			args: map[string]any{"outcome": "Built", "headline": "Uploads resume", "happened": long},
			check: func(t *testing.T, card *coreadapter.Card) {
				if card == nil || utf8.RuneCountInString(card.Happened) > 140 || !strings.HasSuffix(card.Happened, "…") || strings.ContainsAny(card.Happened, "\r\n") {
					t.Fatalf("card %+v", card)
				}
			},
			adjustments: []string{"happened shortened from"},
			kind:        trace.ProgressKind,
		},
		{
			name: "over-length headline and needs_you",
			args: map[string]any{"outcome": "Built", "headline": strings.Repeat("Uploads resume ", 10), "happened": "Chunks resume.", "needs_you": long},
			check: func(t *testing.T, card *coreadapter.Card) {
				if card == nil || utf8.RuneCountInString(card.Headline) > 64 || utf8.RuneCountInString(card.NeedsYou) > 140 || !strings.HasSuffix(card.Headline, "…") || !strings.HasSuffix(card.NeedsYou, "…") {
					t.Fatalf("card %+v", card)
				}
			},
			adjustments: []string{"headline shortened from", "needs_you shortened from"},
			kind:        trace.NoticeKind,
			needs:       "The storage client resumes uploads",
		},
		{
			name: "multi-line fields",
			args: map[string]any{"outcome": "Built", "headline": "Uploads\nresume", "happened": "Chunks\r\nresume.", "needs_you": "Review\nit."},
			check: func(t *testing.T, card *coreadapter.Card) {
				if card == nil || *card != (coreadapter.Card{Headline: "Uploads resume", Happened: "Chunks resume.", NeedsYou: "Review it."}) {
					t.Fatalf("card %+v", card)
				}
			},
			adjustments: []string{"headline joined into one line", "happened joined into one line", "needs_you joined into one line"},
			kind:        trace.NoticeKind,
			needs:       "Review it.",
		},
		{
			name: "an identifier in happened",
			args: map[string]any{"outcome": "Built", "headline": "Uploads resume", "happened": "Fixed parser.go"},
			check: func(t *testing.T, card *coreadapter.Card) {
				if card != nil {
					t.Fatalf("card %+v", card)
				}
			},
			adjustments: []string{"happened left out", "no card recorded"},
			kind:        trace.ProgressKind,
		},
		{
			name: "no card at all",
			args: map[string]any{"outcome": "Built"},
			check: func(t *testing.T, card *coreadapter.Card) {
				if card != nil {
					t.Fatalf("card %+v", card)
				}
			},
			adjustments: []string{"no card recorded: headline and happened cannot be shown"},
			kind:        trace.ProgressKind,
		},
		{
			name: "a needs_you that cannot be shown",
			args: map[string]any{"outcome": "Built", "headline": "Uploads resume", "happened": "Chunks resume.", "needs_you": "Read https://example.com/review"},
			check: func(t *testing.T, card *coreadapter.Card) {
				if card == nil || *card != (coreadapter.Card{Headline: "Uploads resume", Happened: "Chunks resume."}) {
					t.Fatalf("card %+v", card)
				}
			},
			adjustments: []string{"needs_you left out"},
			kind:        trace.NoticeKind,
			needs:       ownerActionWording,
		},
		{
			name: "a valid needs_you with no headline",
			args: map[string]any{"outcome": "Built", "happened": "Chunks resume.", "needs_you": "Review the candidate."},
			check: func(t *testing.T, card *coreadapter.Card) {
				if card != nil {
					t.Fatalf("card %+v", card)
				}
			},
			adjustments: []string{"no card recorded: headline cannot be shown"},
			kind:        trace.NoticeKind,
			needs:       "Review the candidate.",
		},
		{
			name: "a valid needs_you with a happened that cannot be shown",
			args: map[string]any{"outcome": "Built", "headline": "Uploads resume", "happened": "Fixed /etc/hosts", "needs_you": "Review the candidate."},
			check: func(t *testing.T, card *coreadapter.Card) {
				if card != nil {
					t.Fatalf("card %+v", card)
				}
			},
			adjustments: []string{"happened left out", "no card recorded"},
			kind:        trace.NoticeKind,
			needs:       "Review the candidate.",
		},
		{
			name: "a needs_you that cannot be shown with no headline",
			args: map[string]any{"outcome": "Built", "happened": "Chunks resume.", "needs_you": "Ask claude-opus-4 about it"},
			check: func(t *testing.T, card *coreadapter.Card) {
				if card != nil {
					t.Fatalf("card %+v", card)
				}
			},
			adjustments: []string{"needs_you left out", "no card recorded"},
			kind:        trace.NoticeKind,
			needs:       ownerActionWording,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result, report, event := doneThrough(t, tc.args)
			tc.check(t, report.Card)
			if next, _ := result["next"].(string); !strings.HasPrefix(next, "End your turn now.") {
				t.Fatalf("done result %+v", result)
			}
			listed, _ := result["adjustments"].([]any)
			for _, want := range tc.adjustments {
				found := false
				for _, got := range listed {
					found = found || strings.Contains(got.(string), want)
				}
				if !found {
					t.Fatalf("adjustments %v do not name %q", listed, want)
				}
			}
			if len(tc.adjustments) == 0 && len(listed) != 0 {
				t.Fatalf("unexpected adjustments %v", listed)
			}
			if event.Kind != tc.kind {
				t.Fatalf("the move to checking is a %s, want %s: %s", event.Kind, tc.kind, event.Body)
			}
			if tc.needs != "" && !strings.Contains(event.Body, "Needs you: "+tc.needs) {
				t.Fatalf("the notice does not carry %q: %s", tc.needs, event.Body)
			}
			if tc.needs == "" && strings.Contains(event.Body, "Needs you") {
				t.Fatalf("a routine move names an owner action: %s", event.Body)
			}
			if report.Outcome != "Built" {
				t.Fatalf("report %+v", report)
			}
		})
	}
}

func TestDoneStillRefusesAReportItsRulesReject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBuildFixture(t, "refusals")
	m := f.controller()
	scope := coreadapter.Scope{Project: string(f.repository.Project()), Workstream: string(stream), Unit: "resume", Thread: masonAgent("resume"), Turn: "t", Role: masonRole}
	reports := &masonReports{}
	call := func(args map[string]any) string {
		t.Helper()
		raw, err := json.Marshal(args)
		must(t, err)
		out, err := reports.tool(f.repository, scope).Handle(ctx, raw)
		must(t, err)
		return string(out)
	}
	card := map[string]any{"headline": "Uploads resume", "happened": "Chunks resume."}
	with := func(outcome string, extra map[string]any) map[string]any {
		args := map[string]any{"outcome": outcome}
		for k, v := range card {
			args[k] = v
		}
		for k, v := range extra {
			args[k] = v
		}
		return args
	}
	if out := call(with("Built", nil)); !strings.Contains(out, `"recorded":false`) || !strings.Contains(out, "not implementing") {
		t.Fatalf("done before the unit started: %s", out)
	}
	b, _, err := m.read(stream)
	must(t, err)
	started, _, err := m.start(ctx, b, "resume")
	must(t, err)
	if !started {
		t.Fatal("fixture unit resume did not start")
	}
	for name, args := range map[string]map[string]any{
		"empty outcome":  with(" ", nil),
		"empty learning": with("Built", map[string]any{"learnings": []string{"Uses chunks.", " "}}),
	} {
		if out := call(args); !strings.Contains(out, `"recorded":false`) {
			t.Fatalf("%s: %s", name, out)
		}
		if _, ok := reports.accepted[turnKey(scope)]; ok {
			t.Fatalf("%s was accepted", name)
		}
	}
	if out := call(with("Built", map[string]any{"needs_you": "Fixed parser.go"})); !strings.Contains(out, `"recorded":true`) {
		t.Fatalf("an acceptable report: %s", out)
	}
	if out := call(with("Again", nil)); out != `{"recorded":false,"reason":"this turn already reported its unit done; end the turn"}` {
		t.Fatalf("second done: %s", out)
	}
	if got := reports.accepted[turnKey(scope)]; got.Report.Outcome != "Built" || got.OwnerAction != ownerActionWording {
		t.Fatalf("accepted %+v", got)
	}
}
