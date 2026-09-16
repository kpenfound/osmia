package bundle_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/charter"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	project config.ProjectID    = "p_00000000000000000000000000000001"
	first   config.WorkstreamID = "w_00000000000000000000000000000001"
	second  config.WorkstreamID = "w_00000000000000000000000000000002"
)

var (
	timestamp = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	owner     = trace.Actor{Kind: "owner", ID: "local"}
)

var entities = kb.Map{Version: kb.Version, Entities: []kb.Entity{
	{ID: "internal", Name: "Internal packages", Paths: []string{"internal/**"}},
	{ID: "internal.kb", Name: "Knowledge base", Aliases: []string{"knowledge"}, Paths: []string{"internal/kb/**"}, PartOf: []string{"internal"}},
	{ID: "internal.kb.seed", Name: "Seeding", Paths: []string{"internal/kb/seed.go"}, PartOf: []string{"internal.kb"}},
	{ID: "cmd", Name: "Commands", Paths: []string{"cmd/**"}},
	{ID: "docs", Name: "Documentation"},
}}

type fixture struct {
	repo     *trace.Repository
	dir      string
	provider bundle.Provider
}

func setup(t *testing.T) fixture {
	t.Helper()
	base := t.TempDir()
	root, err := config.ResolveRoot(filepath.Join(base, "osmia"), "")
	if err != nil {
		t.Fatal(err)
	}
	p := config.Project{ID: project, Clone: filepath.Join(base, "target")}
	if err := os.Mkdir(p.Clone, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := kb.Encode(entities)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := trace.CreateSeeded(context.Background(), root, p, timestamp, owner, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	dir, err := root.ProjectTrace(project)
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{repo: repo, dir: dir}
	f.provider = bundle.Files{
		Repository: func(id config.ProjectID) (*trace.Repository, error) {
			if id != project {
				return nil, errors.New("unknown project")
			}
			return repo, nil
		},
		Now: func() time.Time { return timestamp.Add(time.Hour) },
	}
	f.write(t, "charter.md", "# Charter\n\n## Testing\n\n1. Every change has a test.\n2. Run dagger check.\n")
	f.write(t, "kb/internal.md", "# Internal\nHow the internal packages fit.\n")
	f.write(t, "kb/internal.kb.seed.md", "# Seeding\n")
	f.write(t, "kb/zeta.md", "# Zeta\nNo entity names this subsystem.\n")
	return f
}

func (f fixture) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) assemble(t *testing.T, scope bundle.Scope) bundle.Bundle {
	t.Helper()
	b, err := f.provider.Assemble(context.Background(), project, scope)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sources(b bundle.Bundle) []string {
	out := []string{}
	for _, p := range b.Knowledge {
		out = append(out, p.Source)
	}
	return out
}

func ids(b bundle.Bundle) []string {
	out := []string{}
	for _, e := range b.Entities.Entities {
		out = append(out, e.ID)
	}
	return out
}

func TestUnscopedBundleHoldsWholeProject(t *testing.T) {
	f := setup(t)
	b := f.assemble(t, bundle.Scope{})
	if b.Mode != bundle.ModeFile || f.provider.Mode(project) != bundle.ModeFile {
		t.Fatalf("mode %q", b.Mode)
	}
	if got := sources(b); !reflect.DeepEqual(got, []string{"kb/internal.md", "kb/internal.kb.seed.md", "kb/zeta.md"}) {
		t.Fatalf("knowledge %v", got)
	}
	if b.Knowledge[0].Subsystem != "internal" || b.Knowledge[0].Content != "# Internal\nHow the internal packages fit.\n" {
		t.Fatalf("prose %#v", b.Knowledge[0])
	}
	if got := ids(b); !reflect.DeepEqual(got, []string{"cmd", "docs", "internal", "internal.kb", "internal.kb.seed"}) {
		t.Fatalf("entities %v", got)
	}
	if b.Entities.Source != "kb/entities.json" || b.Entities.Record != trace.EntitiesDocument || b.Entities.Revision != 1 || len(b.Entities.Unresolved) != 0 || len(b.Missing) != 0 {
		t.Fatalf("entity section %#v missing %#v", b.Entities, b.Missing)
	}
	want := []charter.Rule{{Number: 1, Text: "Every change has a test.", Heading: "Testing"}, {Number: 2, Text: "Run dagger check.", Heading: "Testing"}}
	if !reflect.DeepEqual(b.Charter.Rules, want) || b.Charter.Source != "charter.md" || b.Charter.Record != "charter" {
		t.Fatalf("charter %#v", b.Charter)
	}
	text := b.Render()
	for _, s := range []string{
		"scope: whole project",
		"source: charter.md (record charter revision 2)",
		"- charter#1 [Testing]: Every change has a test.",
		"### kb/zeta.md (subsystem zeta)\n# Zeta\nNo entity names this subsystem.\n### end of kb/zeta.md",
		"source: kb/entities.json (record kb-entities revision 1)",
		"- internal.kb (Knowledge base): internal/kb/**",
		"- docs (Documentation): no paths",
		"No rulings are recorded.",
	} {
		if !strings.Contains(text, s) {
			t.Errorf("render lacks %q:\n%s", s, text)
		}
	}
}

func TestScopedBundleExpandsPartOfAndNotesMissingProse(t *testing.T) {
	f := setup(t)
	// "knowledge" is an alias of internal.kb, which has no prose of its own;
	// its ancestor internal has, and its part internal.kb.seed has. cmd has no
	// prose anywhere in its lineage, and docs has no paths.
	b := f.assemble(t, bundle.Scope{Entities: []string{"cmd", "knowledge", "docs", "nowhere", "internal.kb"}})
	if got := sources(b); !reflect.DeepEqual(got, []string{"kb/internal.md", "kb/internal.kb.seed.md"}) {
		t.Fatalf("knowledge %v", got)
	}
	if got := ids(b); !reflect.DeepEqual(got, []string{"cmd", "internal.kb", "internal.kb.seed"}) {
		t.Fatalf("entities %v", got)
	}
	if !reflect.DeepEqual(b.Entities.Unresolved, []string{"docs", "nowhere"}) {
		t.Fatalf("unresolved %v", b.Entities.Unresolved)
	}
	if want := []bundle.MissingProse{{Entity: "cmd", Looked: []string{"kb/cmd.md"}}}; !reflect.DeepEqual(b.Missing, want) {
		t.Fatalf("missing %#v", b.Missing)
	}
	text := b.Render()
	for _, s := range []string{
		"scope: entities cmd, knowledge, docs, nowhere, internal.kb",
		"- missing: no prose for entity cmd; looked for kb/cmd.md",
		"- unresolved: docs, nowhere",
	} {
		if !strings.Contains(text, s) {
			t.Errorf("render lacks %q:\n%s", s, text)
		}
	}
	if strings.Contains(text, "zeta") {
		t.Fatalf("scoped bundle holds unrelated prose:\n%s", text)
	}

	// A missing ancestor file is looked for along the whole lineage.
	if err := os.Remove(filepath.Join(f.dir, "kb/internal.md")); err != nil {
		t.Fatal(err)
	}
	b = f.assemble(t, bundle.Scope{Entities: []string{"knowledge"}})
	if want := []bundle.MissingProse{{Entity: "internal.kb", Looked: []string{"kb/internal.kb.md", "kb/internal.md"}}}; !reflect.DeepEqual(b.Missing, want) {
		t.Fatalf("missing %#v", b.Missing)
	}
	if got := sources(b); !reflect.DeepEqual(got, []string{"kb/internal.kb.seed.md"}) {
		t.Fatalf("knowledge %v", got)
	}
}

func TestBundleRecordsCharterEditBeforeIncluding(t *testing.T) {
	f := setup(t)
	b := f.assemble(t, bundle.Scope{})
	if b.Charter.Revision != 2 {
		t.Fatalf("charter revision %d", b.Charter.Revision)
	}
	f.write(t, "charter.md", "1. Only this rule.\n")
	b = f.assemble(t, bundle.Scope{})
	if b.Charter.Revision != 3 || len(b.Charter.Rules) != 1 || b.Charter.Rules[0].Text != "Only this rule." {
		t.Fatalf("charter %#v", b.Charter)
	}
	doc, err := trace.Get[trace.Document](f.repo, "", "charter", 3)
	if err != nil || doc.Content != "1. Only this rule.\n" || doc.Actor != owner || doc.Cause != "owner-edit" || !doc.At.Equal(timestamp.Add(time.Hour)) {
		t.Fatalf("recorded edit %#v %v", doc, err)
	}
	// An unchanged charter records nothing more.
	f.assemble(t, bundle.Scope{})
	if _, err := trace.Get[trace.Document](f.repo, "", "charter", 4); err == nil {
		t.Fatal("unchanged charter recorded a revision")
	}
}

func TestBundleReadsFilesAtEachCall(t *testing.T) {
	f := setup(t)
	scope := bundle.Scope{Entities: []string{"internal"}}
	f.assemble(t, scope)
	f.write(t, "kb/internal.md", "edited\n")
	f.write(t, "kb/internal.kb.md", "new file\n")
	b := f.assemble(t, scope)
	if got := sources(b); !reflect.DeepEqual(got, []string{"kb/internal.md", "kb/internal.kb.md", "kb/internal.kb.seed.md"}) {
		t.Fatalf("knowledge %v", got)
	}
	if b.Knowledge[0].Content != "edited\n" {
		t.Fatalf("stale prose %q", b.Knowledge[0].Content)
	}
}

func ruling(stream config.WorkstreamID, id string, revision int, at time.Time, decision string) trace.Ruling {
	return trace.Ruling{
		Header:           trace.Header{Schema: "osmia.trace.ruling", Version: trace.Version, ID: id, Revision: revision, Project: project, Workstream: stream, At: at, Actor: owner, Cause: "owner-answer"},
		QuestionID:       "q-" + id,
		QuestionRevision: 1,
		Decision:         decision,
		ReturnedAnswer:   "Answer to " + id,
	}
}

func TestBundleDecisions(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if b := f.assemble(t, bundle.Scope{}); len(b.Decisions) != 0 || b.Decisions == nil {
		t.Fatalf("decisions without workstreams %#v", b.Decisions)
	}
	for _, s := range []config.WorkstreamID{second, first} {
		if err := f.repo.CreateWorkstream(ctx, s, timestamp, owner); err != nil {
			t.Fatal(err)
		}
	}
	if b := f.assemble(t, bundle.Scope{Workstream: first}); len(b.Decisions) != 0 || !strings.Contains(b.Render(), "No rulings are recorded.") {
		t.Fatalf("empty workstream decisions %#v", b.Decisions)
	}
	for _, r := range []trace.Ruling{
		ruling(second, "r-late", 1, timestamp.Add(2*time.Minute), "Second stream"),
		ruling(first, "r-b", 1, timestamp.Add(time.Minute), "Keep the API"),
		ruling(first, "r-a", 1, timestamp.Add(time.Minute), "Old decision"),
		ruling(first, "r-a", 2, timestamp.Add(3*time.Minute), "Use SQLite\nfor now"),
	} {
		if err := f.repo.Append(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	type key struct {
		stream   config.WorkstreamID
		id       string
		revision int
	}
	keys := func(b bundle.Bundle) []key {
		out := []key{}
		for _, d := range b.Decisions {
			out = append(out, key{d.Workstream, d.Record, d.Revision})
		}
		return out
	}
	b := f.assemble(t, bundle.Scope{})
	if got, want := keys(b), []key{{first, "r-b", 1}, {first, "r-a", 2}, {second, "r-late", 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("project decisions %v", got)
	}
	d := b.Decisions[1]
	if d.Source != "workstreams/"+string(first)+"/questions/q-r-a/rulings.jsonl" || d.QuestionID != "q-r-a" || d.QuestionRevision != 1 || d.Decision != "Use SQLite\nfor now" || d.ReturnedAnswer != "Answer to r-a" {
		t.Fatalf("decision %#v", d)
	}
	if !strings.Contains(b.Render(), "- workstreams/"+string(first)+"/questions/q-r-a/rulings.jsonl (record r-a revision 2, question q-r-a revision 1)\n  decision: Use SQLite\n    for now\n  answer: Answer to r-a\n") {
		t.Fatalf("render:\n%s", b.Render())
	}
	b = f.assemble(t, bundle.Scope{Workstream: second})
	if got, want := keys(b), []key{{second, "r-late", 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("workstream decisions %v", got)
	}
	if !strings.Contains(b.Render(), "workstream: "+string(second)) {
		t.Fatalf("render:\n%s", b.Render())
	}
}

func TestBundleIsDeterministic(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := f.repo.CreateWorkstream(ctx, first, timestamp, owner); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"r-3", "r-1", "r-2"} {
		if err := f.repo.Append(ctx, ruling(first, id, 1, timestamp, "Decision "+id)); err != nil {
			t.Fatal(err)
		}
	}
	scope := bundle.Scope{Entities: []string{"internal", "cmd", "knowledge"}}
	a := f.assemble(t, scope)
	for range 5 {
		b := f.assemble(t, scope)
		if !reflect.DeepEqual(a, b) || a.Render() != b.Render() {
			t.Fatalf("bundles differ:\n%s\n---\n%s", a.Render(), b.Render())
		}
	}
	var order []string
	for _, d := range a.Decisions {
		order = append(order, d.Record)
	}
	if !reflect.DeepEqual(order, []string{"r-1", "r-2", "r-3"}) {
		t.Fatalf("decision order %v", order)
	}
	if _, err := json.Marshal(a); err != nil {
		t.Fatal(err)
	}
}

func TestBundleFailures(t *testing.T) {
	f := setup(t)
	if _, err := (bundle.Files{}).Assemble(context.Background(), project, bundle.Scope{}); err == nil {
		t.Fatal("provider without a trace assembled a bundle")
	}
	if _, err := f.provider.Assemble(context.Background(), "p_00000000000000000000000000000009", bundle.Scope{}); err == nil {
		t.Fatal("unknown project assembled a bundle")
	}
	if _, err := f.provider.Assemble(context.Background(), project, bundle.Scope{Workstream: first}); err == nil {
		t.Fatal("unknown workstream assembled a bundle")
	}
	if err := os.Symlink("internal.md", filepath.Join(f.dir, "kb/cmd.md")); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []bundle.Scope{{Entities: []string{"cmd"}}, {}} {
		if _, err := f.provider.Assemble(context.Background(), project, scope); err == nil || !strings.Contains(err.Error(), "kb/cmd.md") {
			t.Fatalf("symlinked prose with scope %v: %v", scope, err)
		}
	}
	if err := os.Remove(filepath.Join(f.dir, "kb/cmd.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(f.dir, "kb/cmd.md"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.provider.Assemble(context.Background(), project, bundle.Scope{}); err == nil {
		t.Fatal("directory named like prose was skipped")
	}
}

type fakeTurns func(context.Context, coreadapter.PreparedTurn) (coreadapter.SessionResult, error)

func (f fakeTurns) Run(ctx context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	return f(ctx, p)
}

func TestRenderedBundleTravelsInTurnRequest(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := f.repo.CreateWorkstream(ctx, first, timestamp, owner); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, Revision: 1, ID: "agent", Project: project, Workstream: first, At: timestamp, Actor: owner, Cause: "message"}, Role: "architect", ThreadID: "thread"}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Append(ctx, ruling(first, "r-1", 1, timestamp, "Keep the API")); err != nil {
		t.Fatal(err)
	}
	b := f.assemble(t, bundle.Scope{Entities: []string{"knowledge"}, Workstream: first})
	text := b.Render()
	prompt := text + "\nPlan the change.\n"
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, Revision: 1, ID: "request_1", Project: project, Workstream: first, At: timestamp, Actor: owner, Cause: "owner-message"}, AgentID: "agent", ThreadID: "thread", TurnID: "1", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}, SystemPrompt: "Architect", Prompt: prompt}
	if _, err := f.repo.EnqueueTurn(ctx, req); err != nil {
		t.Fatal(err)
	}
	var got string
	runner := thread.Runner{Store: f.repo, Now: func() time.Time { return timestamp.Add(time.Minute) }, Turns: fakeTurns(func(_ context.Context, p coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
		got = p.Prompt
		return coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "s"}, FinalResponse: "ok", Outcome: &coreadapter.Outcome{Status: "complete"}}, nil
	})}
	if _, err := runner.RunNext(ctx, first, "agent", coreadapter.PreparedTurn{SessionDirectory: filepath.Join(t.TempDir(), "session"), AllowedOutcomes: []string{"complete"}}); err != nil {
		t.Fatal(err)
	}
	if got != prompt || !strings.Contains(got, "(record r-1 revision 1") || !strings.Contains(got, "kb/internal.md") {
		t.Fatalf("turn received:\n%s", got)
	}
	requests, err := trace.Read[trace.TurnRequest](f.repo, first)
	if err != nil || len(requests) != 1 || requests[0].Prompt != prompt {
		t.Fatalf("recorded request %#v %v", requests, err)
	}
}
