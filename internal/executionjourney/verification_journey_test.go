package executionjourney

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	config "github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verification"
	"github.com/kpenfound/osmia/internal/verifytool"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	journeyTraceFeature = `---
feature: Trace
paths:
  - internal/trace/**
flows:
  - home-page
  - add-note
---

# Trace

## What exists

The trace store.

## How a user reaches it

Through the landing page.

## How to drive it

Run the home-page and add-note flows.

## What usually breaks

Notes that are not escaped.
`
	journeyNotesFeature = `---
feature: Notes
paths:
  - internal/notes/**
flows:
  - add-note
---

# Notes

## What exists

Notes.

## How a user reaches it

The Notes link.

## How to drive it

Run add-note.

## What usually breaks

The redirect.
`
	journeyMapIndex = "# Verification feature map\n\nFeatures, in the order of a full regression sweep:\n\n1. [notes](notes.md): adding notes.\n2. [trace](trace.md): the trace store.\n"
	journeySettings = "\n[verification]\ncall = [\"verification-fixture\"]\nmap_dir = \"docs/verification\"\nlever_paths = [\"lever/**\"]\n"
)

var journeyPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 64)...)

const journeySnapshot = "- document [ref=e1]:\n  - heading \"Verification fixture\" [ref=e2]\n  - code [ref=e3]: 0123456789abcdef\n"

func leverReport(claims ...string) string {
	return `{"version":1,"claims":[` + strings.Join(claims, ",") + `]}`
}

func journeyClaim(name, status, detail, flow string, evidence ...string) string {
	quoted, _ := json.Marshal(evidence)
	tag := ""
	if flow != "" {
		tag = `,"flow":"` + flow + `"`
	}
	return fmt.Sprintf(`{"name":%q,"status":%q,"detail":%q,"evidence":%s%s}`, name, status, detail, quoted, tag)
}

type journeyLever struct{ log, mark string }

func installJourneyLever(t *testing.T) journeyLever {
	t.Helper()
	bin, work := t.TempDir(), t.TempDir()
	outputs := map[string]string{
		"home": leverReport(
			journeyClaim("step 01 goto .", "pass", "loaded the page with HTTP 200", "home-page", "log.txt"),
			journeyClaim("step 02 snapshot", "pass", "saved the accessibility tree", "home-page", "snapshots/02-snapshot.yml", "log.txt"),
			journeyClaim("step 03 screenshot", "pass", "saved a PNG of the viewport", "home-page", "screenshots/03-screenshot.png", "log.txt")),
		"add": leverReport(
			journeyClaim("step 01 goto notes", "pass", "loaded the notes page", "add-note", "log.txt"),
			journeyClaim("step 02 click text=Add note", "fail", "no element matches text=Add note", "add-note", "snapshots/02-snapshot.yml", "log.txt")),
		"adhoc": leverReport(
			journeyClaim("step 01 goto notes", "pass", "loaded the notes page", "", "log.txt"),
			journeyClaim("step 02 snapshot", "pass", "saved the accessibility tree", "", "snapshots/02-snapshot.yml", "log.txt"),
			journeyClaim("step 03 screenshot", "pass", "saved a PNG of the viewport", "", "screenshots/03-screenshot.png", "log.txt")),
		"home-fail": leverReport(
			journeyClaim("step 01 goto .", "fail", "the page answered HTTP 500", "home-page", "log.txt")),
		"add-inconclusive": leverReport(
			journeyClaim("step 01 goto notes", "inconclusive", "the page never settled", "add-note", "log.txt")),
	}
	for name, report := range outputs {
		dir := filepath.Join(work, name)
		for file, data := range map[string][]byte{
			"report.json":                   []byte(report),
			"log.txt":                       []byte("goto: ok\n"),
			"snapshots/02-snapshot.yml":     []byte(journeySnapshot),
			"screenshots/03-screenshot.png": journeyPNG,
		} {
			fixturekit.Must(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o700))
			fixturekit.Must(t, os.WriteFile(filepath.Join(dir, file), data, 0o600))
		}
	}
	lever := journeyLever{log: filepath.Join(work, "log"), mark: filepath.Join(work, "final")}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %[1]q
case "$*" in
  *" list") printf '%%s' '["home-page","add-note"]'; exit 0;;
esac
src=%[2]q/adhoc
case "$*" in
  *--flow=home-page*) src=%[2]q/home; [ -f %[3]q ] && src=%[2]q/home-fail;;
  *--flow=add-note*) src=%[2]q/add; [ -f %[3]q ] && src=%[2]q/add-inconclusive;;
esac
for last; do :; done
cp -R "$src"/. "$last"
`, lever.log, work, lever.mark)
	fixturekit.Must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return lever
}

func (l journeyLever) calls() []string {
	data, err := os.ReadFile(l.log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func (l journeyLever) runs() []string {
	return slices.DeleteFunc(l.calls(), func(call string) bool { return strings.HasSuffix(call, " list") })
}

func (l journeyLever) startFinalReview() error { return os.WriteFile(l.mark, nil, 0o600) }

type journeyProblems struct {
	mu   sync.Mutex
	list []string
}

func (p *journeyProblems) add(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.list = append(p.list, fmt.Sprintf(format, args...))
}

func (p *journeyProblems) require(ok bool, format string, args ...any) {
	if !ok {
		p.add(format, args...)
	}
}

func (p *journeyProblems) check(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.list) != 0 {
		t.Fatal(strings.Join(p.list, "\n"))
	}
}

func journeyToolNames(ctx context.Context, tools *mcp.ClientSession) ([]string, error) {
	listed, err := tools.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	return names, nil
}

func journeyVerify(ctx context.Context, tools *mcp.ClientSession, call ...string) (verifytool.Result, error) {
	body, err := fixturekit.CallTool(ctx, tools, "verify", map[string]any{"call": call})
	if err != nil {
		return verifytool.Result{}, err
	}
	var result verifytool.Result
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		return verifytool.Result{}, fmt.Errorf("verify returned %q: %w", body, err)
	}
	return result, nil
}

func journeyRead(ctx context.Context, tools *mcp.ClientSession, name string) (string, error) {
	body, err := fixturekit.CallTool(ctx, tools, "file_read", map[string]any{"path": name})
	if err != nil {
		return "", err
	}
	var text string
	if json.Unmarshal([]byte(body), &text) == nil {
		return text, nil
	}
	return body, nil
}

func journeyResult(req agent.Request, text string) (*agent.Result, error) {
	return &agent.Result{ClaudeID: "session-" + req.Name, ResultText: text, SessionDir: req.SessionDir, NumTurns: 1}, nil
}

func commitToClone(t *testing.T, f *journey.ShedFixture, files map[string]string, message string) {
	t.Helper()
	home := filepath.Dir(f.Clone)
	for name, content := range files {
		path := filepath.Join(f.Clone, filepath.FromSlash(name))
		fixturekit.Must(t, os.MkdirAll(filepath.Dir(path), 0o700))
		fixturekit.Must(t, os.WriteFile(path, []byte(content), 0o600))
	}
	fixturekit.DemoGit(t, home, "-C", f.Clone, "add", "--all")
	fixturekit.DemoGit(t, home, "-C", f.Clone, "-c", "user.name=Owner", "-c", "user.email=owner@example.invalid", "commit", "-qm", message)
	fixturekit.DemoGit(t, home, "-C", f.Clone, "push", "-q", "upstream", "HEAD:refs/heads/main")
}

func journeyDocument[T any](t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID, id string) (T, bool) {
	t.Helper()
	var out T
	docs := f.Documents(t, stream, id)
	if len(docs) == 0 {
		return out, false
	}
	fixturekit.Must(t, json.Unmarshal([]byte(docs[len(docs)-1].Content), &out))
	return out, true
}

type journeyState struct {
	reviews atomic.Int32
	adhoc   atomic.Pointer[verifytool.Result]
	entered chan struct{}
}

func TestVerificationJourney(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("setting=%t", enabled), func(t *testing.T) { runVerificationJourney(t, enabled) })
	}
}

func runVerificationJourney(t *testing.T, enabled bool) {
	lever := installJourneyLever(t)
	problems := &journeyProblems{}
	state := &journeyState{entered: make(chan struct{})}
	f, masons := journey.NewMasonFixture(t, 1, intakefixture.IndependentPlan)
	defer f.Stop(t)

	var stream config.WorkstreamID
	fork, _ := journey.HostService(t, f, func() config.WorkstreamID { return stream })
	f.Stop(t)
	files := map[string]string{
		"internal/trace/doc.go": "package trace\n",
		"internal/notes/doc.go": "package notes\n",
		"lever/main.go":         "package main\n",
	}
	if enabled {
		fixturekit.EditProjectSettings(t, f.Opts.Config.Root, f.Project, func(text string) string { return text + journeySettings })
		files["docs/verification/README.md"] = journeyMapIndex
		files["docs/verification/trace.md"] = journeyTraceFeature
		files["docs/verification/notes.md"] = journeyNotesFeature
	}
	commitToClone(t, f, files, "the fixture project")
	f.Start(t)

	declared := [][]string{{"verify", "--flow=home-page"}, {"verify", "--flow=add-note"}, {"verify", "--run-id", "mine"}}
	noted := func(play func(context.Context, agent.Request, *mcp.ClientSession) error) func(context.Context, agent.Request, *mcp.ClientSession) error {
		return func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
			err := play(ctx, req, tools)
			if err != nil {
				problems.add("mason turn %s: %v", req.Name, err)
			}
			return err
		}
	}
	masons.Mu.Lock()
	masons.Play[execution.MasonTurnID("resume")] = noted(func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		pointed := strings.Contains(req.Prompt, "docs/verification/trace.md") && strings.Contains(req.Prompt, "## Verification")
		problems.require(pointed == enabled, "the mason prompt names the feature file: %t, want %t:\n%s", pointed, enabled, req.Prompt)
		problems.require(!strings.Contains(req.Prompt, "docs/verification/notes.md"), "the mason is pointed at a feature its footprint does not touch")
		for name, content := range map[string]string{"internal/trace/resume.go": "package trace\n// resume\n", "lever/main.go": "package main\n// the lever\n"} {
			path := filepath.Join(req.Workspace.Directory(), filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				return err
			}
		}
		args := map[string]any{"outcome": "Built"}
		if enabled {
			args["invocations"] = declared
		}
		recorded, reason, err := intakefixture.Done(ctx, tools, args)
		if err != nil || !recorded {
			return fmt.Errorf("done refused: %q %v", reason, err)
		}
		return nil
	})
	masons.Play[execution.MasonTurnID("dedupe")] = noted(func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) error {
		if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/dedupe.go"), []byte("package trace\n// dedupe\n"), 0o600); err != nil {
			return err
		}
		return intakefixture.ReportDone("Skips")(ctx, req, tools)
	})
	masons.Mu.Unlock()

	f.Engine.Mu.Lock()
	chief := f.Engine.Turns["*"]
	f.Engine.Turns["*"] = func(ctx context.Context, req agent.Request, turn *agent.Turn, tools *mcp.ClientSession) (*agent.Result, error) {
		switch {
		case strings.HasPrefix(req.Name, intake.MasonAgent("resume")+"-revise-"):
			return resumeRework(ctx, req, tools, enabled, problems, state)
		case strings.HasPrefix(req.Name, intake.ReviewerAgent("resume")+"-"):
			return resumeReview(ctx, req, tools, enabled, lever, problems, state)
		case strings.HasPrefix(req.Name, intake.ReviewerAgent("dedupe")+"-"):
			return journeyVerdict(ctx, req, tools, problems, "satisfactory")
		case strings.HasPrefix(req.Name, "final-"):
			return finalSweep(ctx, req, tools, enabled, lever, problems)
		}
		return chief(ctx, req, turn, tools)
	}
	f.Engine.Mu.Unlock()

	stream, _ = f.BuiltAs(t, "verification-journey")
	if enabled {
		select {
		case <-state.entered:
		case <-time.After(fixturekit.DemoTimeout):
			t.Fatal("the reviewer never reached its interruption")
		}
		unfinishReviewerCall(t, f, stream)
		f.Stop(t)
		f.Start(t)
	}
	failFast := func() {
		problems.mu.Lock()
		defer problems.mu.Unlock()
		if len(problems.list) != 0 {
			t.Fatal(strings.Join(problems.list, "\n"))
		}
	}
	for _, unit := range []string{"resume", "dedupe"} {
		intakefixture.Soon(t, "unit "+unit+" to merge", func() bool {
			failFast()
			got, err := f.UnitState(stream, unit)
			return err == nil && got == intake.UnitMerged
		})
	}
	intakefixture.Soon(t, "the final review's report", func() bool {
		failFast()
		_, found, err := delivery.LatestFinalReport(f.Repository(), stream)
		return err == nil && found
	})
	problems.check(t)
	masons.Check(t)
	assertJourneyRecords(t, f, stream, enabled, lever, state, fork)
}

func journeyVerdict(ctx context.Context, req agent.Request, tools *mcp.ClientSession, problems *journeyProblems, decision string) (*agent.Result, error) {
	findings := []api.ReviewFinding{}
	if decision != "satisfactory" {
		findings = append(findings, api.ReviewFinding{Severity: "material", Evidence: "the add-note flow fails in the service's rerun, and the ad-hoc run did not exercise the form", Action: "make the Add note control reachable"})
	}
	body, err := fixturekit.CallTool(ctx, tools, execution.VerdictTool, map[string]any{"decision": decision, "summary": fixturekit.ReviewSummary, "findings": findings})
	problems.require(err == nil && strings.Contains(body, `"recorded":true`), "verdict %s of %s: %v", body, req.Name, err)
	return journeyResult(req, "Reviewed")
}

func resumeRework(ctx context.Context, req agent.Request, tools *mcp.ClientSession, enabled bool, problems *journeyProblems, state *journeyState) (*agent.Result, error) {
	names, err := journeyToolNames(ctx, tools)
	if err != nil {
		return nil, err
	}
	problems.require(!slices.Contains(names, "verify"), "the mason is offered verify: %v", names)
	if enabled {
		adhoc := state.adhoc.Load()
		if adhoc == nil {
			problems.add("the reviewer ran no verify call before sending back")
			return journeyResult(req, "Revised")
		}
		for _, want := range []string{adhoc.RunID, adhoc.Commit, "step 03 screenshot", "saved a PNG of the viewport", verifytool.Ref(adhoc.RunID, "screenshots/03-screenshot.png"), verifytool.Ref(adhoc.RunID, "log.txt")} {
			problems.require(strings.Contains(req.Prompt, want), "the send-back lacks %q:\n%s", want, req.Prompt)
		}
		body, err := journeyRead(ctx, tools, verifytool.Ref(adhoc.RunID, "snapshots/02-snapshot.yml"))
		problems.require(err == nil && strings.Contains(body, "0123456789abcdef"), "the retained snapshot reads %q in the mason's view: %v", body, err)
		body, err = journeyRead(ctx, tools, verifytool.Ref(adhoc.RunID, "log.txt"))
		problems.require(err == nil && strings.Contains(body, "goto: ok"), "the retained log reads %q in the mason's view: %v", body, err)
		data, err := os.ReadFile(filepath.Join(req.Workspace.Directory(), verifytool.Ref(adhoc.RunID, "screenshots/03-screenshot.png")))
		problems.require(err == nil && bytes.Equal(data, journeyPNG), "the screenshot is not in the mason's view: %v", err)
	} else {
		problems.require(!strings.Contains(req.Prompt, "Verify run") && !strings.Contains(req.Prompt, verifytool.EvidenceDir), "a project without the setting got verification in a send-back:\n%s", req.Prompt)
	}
	if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "internal/trace/resume.go"), []byte("package trace\n// resume, revised\n"), 0o600); err != nil {
		return nil, err
	}
	recorded, reason, err := intakefixture.Done(ctx, tools, map[string]any{"outcome": "Revised"})
	if err != nil || !recorded {
		problems.add("the revision's done was refused: %q %v", reason, err)
		return nil, fmt.Errorf("done refused: %q %v", reason, err)
	}
	return journeyResult(req, "Revised")
}

func resumeReview(ctx context.Context, req agent.Request, tools *mcp.ClientSession, enabled bool, lever journeyLever, problems *journeyProblems, state *journeyState) (*agent.Result, error) {
	names, err := journeyToolNames(ctx, tools)
	if err != nil {
		return nil, err
	}
	problems.require(slices.Contains(names, "verify") == enabled, "the reviewer is offered %v, want verify offered: %t", names, enabled)
	round := int(state.reviews.Add(1))
	if !enabled {
		problems.require(!strings.Contains(req.Prompt, "service-recorded") && !strings.Contains(req.Prompt, "## Verification"), "the review bundle of a project without the setting has verification content:\n%s", req.Prompt)
		if round == 1 {
			return journeyVerdict(ctx, req, tools, problems, "material_findings")
		}
		return journeyVerdict(ctx, req, tools, problems, "satisfactory")
	}
	switch round {
	case 1:
		for _, want := range []string{
			"## Verification: service-recorded reruns", "Declared invocations: 3. Rerun: 2. Skipped: 0. Refused: 1.",
			"Declared invocation 3: refused, no run", "- Trace (docs/verification/trace.md)",
			"flow home-page: pass", "flow add-note: fail", "no element matches text=Add note",
			"Flag: the diff touches the lever's source",
		} {
			problems.require(strings.Contains(req.Prompt, want), "the review bundle lacks %q:\n%s", want, req.Prompt)
		}
		problems.require(!strings.Contains(req.Prompt, "flow add-note: pass"), "a flow with a failed claim reads as a pass")
		result, err := journeyVerify(ctx, tools, "verify", "--steps=goto notes; snapshot; screenshot")
		if err != nil {
			return nil, err
		}
		state.adhoc.Store(&result)
		problems.require(result.Limitation == "" && result.RunID != "" && result.Status == verification.Pass && len(result.Images) == 1, "the ad-hoc verify result %+v", result)
		for _, name := range []string{"log.txt", "snapshots/02-snapshot.yml"} {
			body, err := journeyRead(ctx, tools, verifytool.Ref(result.RunID, name))
			problems.require(err == nil && body != "", "the reviewer cannot read %s: %v", name, err)
		}
		close(state.entered)
		<-ctx.Done()
		return nil, ctx.Err()
	case 2:
		adhoc := state.adhoc.Load()
		body, err := journeyRead(ctx, tools, verifytool.EvidenceDir+"/results.json")
		if err != nil {
			problems.add("the recovered turn has no results.json: %v", err)
		} else {
			var recovered []verifytool.Result
			err := json.Unmarshal([]byte(body), &recovered)
			problems.require(err == nil && len(recovered) == 1 && adhoc != nil && recovered[0].RunID == adhoc.RunID, "recovered results (error %v, %d results, ad-hoc %+v): %s", err, len(recovered), adhoc, body)
		}
		if adhoc != nil {
			text, err := journeyRead(ctx, tools, verifytool.Ref(adhoc.RunID, "log.txt"))
			problems.require(err == nil && strings.Contains(text, "goto: ok"), "the recovered log reads %q: %v", text, err)
		}
		problems.require(len(lever.runs()) == 3, "recovering the interrupted call ran the lever: %q", lever.runs())
		return journeyVerdict(ctx, req, tools, problems, "material_findings")
	}
	return journeyVerdict(ctx, req, tools, problems, "satisfactory")
}

func finalSweep(ctx context.Context, req agent.Request, tools *mcp.ClientSession, enabled bool, lever journeyLever, problems *journeyProblems) (*agent.Result, error) {
	names, err := journeyToolNames(ctx, tools)
	if err != nil {
		return nil, err
	}
	problems.require(slices.Contains(names, "verify") == enabled, "the final reviewer is offered %v, want verify offered: %t", names, enabled)
	if enabled {
		for _, want := range []string{"## Verification: regression sweep", "run each one with verify on the feature-branch head", "1. add-note (Notes, Trace)", "2. home-page (Trace)"} {
			problems.require(strings.Contains(req.Prompt, want), "the final reviewer's prompt lacks %q:\n%s", want, req.Prompt)
		}
		if err := lever.startFinalReview(); err != nil {
			return nil, err
		}
		for _, flow := range []string{"add-note", "home-page"} {
			result, err := journeyVerify(ctx, tools, "verify", "--flow="+flow)
			if err != nil {
				return nil, err
			}
			problems.require(result.Limitation == "" && result.RunID != "" && result.Status != verification.Pass, "final run of %s: %+v", flow, result)
		}
	} else {
		problems.require(!strings.Contains(req.Prompt, "regression sweep"), "the final reviewer of a project without the setting got a sweep")
	}
	body, err := fixturekit.CallTool(ctx, tools, service.FinalReportTool, map[string]any{"summary": "The feature works as a whole.", "findings": []any{}})
	problems.require(err == nil && strings.Contains(body, `"recorded":true`), "final report %s: %v", body, err)
	return journeyResult(req, "Read")
}

func unfinishReviewerCall(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID) {
	t.Helper()
	repo := f.Repository()
	ops, latest, err := verifytool.Operations(repo, stream)
	fixturekit.Must(t, err)
	for i := len(ops) - 1; i >= 0; i-- {
		op := ops[i]
		if op.Rerun != nil {
			continue
		}
		op.Finished = false
		data, err := json.Marshal(op)
		fixturekit.Must(t, err)
		doc := latest[op.ID]
		doc.Revision++
		doc.Content = string(data)
		fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{doc}))
		return
	}
	t.Fatal("the reviewer's verify call is not recorded")
}

func runIDs(calls []string) []string {
	var ids []string
	for _, call := range calls {
		fields := strings.Fields(call)
		if i := slices.Index(fields, "--run-id"); i >= 0 && i+1 < len(fields) {
			ids = append(ids, fields[i+1])
		}
	}
	return ids
}

func assertJourneyRecords(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID, enabled bool, lever journeyLever, state *journeyState, fork string) {
	t.Helper()
	repo := f.Repository()
	home := filepath.Dir(f.Clone)
	if status := strings.TrimSpace(fixturekit.DemoGit(t, home, "-C", f.Clone, "status", "--porcelain")); status != "" {
		t.Fatalf("the target repository's working tree changed:\n%s", status)
	}
	if _, err := os.Stat(filepath.Join(f.Clone, verifytool.EvidenceDir)); err == nil {
		t.Fatal("verification evidence was written into the target repository")
	}
	if tree := fixturekit.DemoGit(t, home, "-C", fork, "ls-tree", "-r", "--name-only", intake.FeatureBranch(stream)); strings.Contains(tree, verifytool.EvidenceDir) {
		t.Fatalf("the feature branch holds verification evidence:\n%s", tree)
	}
	measureDocs := func(id string) int { return len(f.Documents(t, stream, id)) }
	if !enabled {
		if calls := lever.calls(); len(calls) != 0 {
			t.Fatalf("a project without the setting ran the lever: %q", calls)
		}
		ops, _, err := verifytool.Operations(repo, stream)
		fixturekit.Must(t, err)
		if len(ops) != 0 {
			t.Fatalf("a project without the setting recorded verify runs: %+v", ops)
		}
		for _, id := range []string{execution.UnitMeasureDocument("resume"), execution.UnitMeasureDocument("dedupe"), execution.FeaturesDocument("resume"), execution.FeaturesDocument("dedupe"), delivery.FinalMeasureDocument(1)} {
			if n := measureDocs(id); n != 0 {
				t.Fatalf("a project without the setting recorded %s %d times", id, n)
			}
		}
		return
	}

	calls := lever.runs()
	ids := runIDs(calls)
	if len(calls) != 5 || len(ids) != 5 || len(slices.Compact(slices.Sorted(slices.Values(ids)))) != 5 {
		t.Fatalf("lever runs %q with run IDs %v: want five distinct runs", calls, ids)
	}
	for i, want := range []string{"verify --flow=home-page", "verify --flow=add-note", "verify --steps=goto notes; snapshot; screenshot", "verify --flow=add-note", "verify --flow=home-page"} {
		if !strings.Contains(calls[i], " "+want+" export --path ") {
			t.Fatalf("lever run %d is %q, want %q", i+1, calls[i], want)
		}
	}
	for _, call := range calls {
		if strings.Contains(call, "mine") {
			t.Fatalf("a refused invocation reached the lever: %q", call)
		}
	}
	ops, _, err := verifytool.Operations(repo, stream)
	fixturekit.Must(t, err)
	var reruns, reviewer []verifytool.Operation
	for _, op := range ops {
		switch {
		case op.Rerun != nil:
			reruns = append(reruns, op)
		case op.Unit == "resume":
			reviewer = append(reviewer, op)
		}
	}
	if len(reruns) != 3 || reruns[0].Rerun.Disposition != verifytool.RerunRan || reruns[1].Rerun.Disposition != verifytool.RerunRan || reruns[2].Rerun.Disposition != verifytool.RerunRefused || reruns[2].RunID != "" {
		t.Fatalf("rerun records %+v", reruns)
	}
	for _, op := range reruns[:2] {
		kept, ok, err := verifytool.Retained(repo, stream, op.RunID)
		fixturekit.Must(t, err)
		if !ok || !slices.Contains(kept.Files, "report.json") || !slices.Contains(kept.Files, "log.txt") {
			t.Fatalf("rerun %s kept %+v", op.RunID, kept)
		}
	}
	adhoc := state.adhoc.Load()
	if adhoc == nil || len(reviewer) != 1 {
		t.Fatalf("reviewer runs %+v, ad-hoc result %+v", reviewer, adhoc)
	}
	var recorded *verifytool.Operation
	for i := range reviewer {
		if reviewer[i].RunID == adhoc.RunID {
			recorded = &reviewer[i]
		}
	}
	if recorded == nil || !slices.Equal(recorded.Args, []string{"verify", "--steps=goto notes; snapshot; screenshot"}) || recorded.Commit != adhoc.Commit || !recorded.Finished {
		t.Fatalf("the ad-hoc verify call is recorded as %+v", recorded)
	}
	kept, ok, err := verifytool.Retained(repo, stream, adhoc.RunID)
	fixturekit.Must(t, err)
	if !ok || !slices.Contains(kept.Files, "log.txt") || !slices.Contains(kept.Files, "screenshots/03-screenshot.png") || !slices.Contains(kept.Images, "screenshots/03-screenshot.png") {
		t.Fatalf("the ad-hoc run kept %+v", kept)
	}

	unit, ok := journeyDocument[verification.UnitMeasure](t, f, stream, execution.UnitMeasureDocument("resume"))
	if !ok || unit.Declared.Runs.Pass != 1 || unit.Declared.Runs.Fail != 1 || unit.Declared.Refused != 1 || unit.Reviewer.Runs.Pass != 1 || unit.Reviewer.UntaggedRuns != 1 ||
		!slices.Equal(unit.Declared.Flows, []string{"add-note", "home-page"}) || len(unit.Reviewer.Flows) != 0 || !unit.LeverTouched || unit.MapTouched || unit.Runs.Inconclusive != 0 {
		t.Fatalf("resume's measure %+v", unit)
	}
	for _, name := range []string{"resume", "dedupe"} {
		association, ok := journeyDocumentAssociation(t, f, stream, name)
		if !ok || len(association.Features) != 1 || association.Features[0].Feature != "Trace" || !slices.Equal(association.Flows, []string{"home-page", "add-note"}) {
			t.Fatalf("%s's feature association %+v", name, association)
		}
	}
	final, ok := journeyDocument[verification.FinalMeasure](t, f, stream, delivery.FinalMeasureDocument(1))
	if !ok || final.Review != 1 || final.Runs.Pass != 0 || final.Runs.Fail != 1 || final.Runs.Inconclusive != 1 || final.Unattributed() != 0 ||
		len(final.Failed) != 1 || final.Failed[0].Flow != "home-page" || !final.Failed[0].Shared || !slices.Equal(final.Failed[0].Units, []string{"dedupe", "resume"}) ||
		len(final.Inconclusive) != 1 || final.Inconclusive[0].Flow != "add-note" || !slices.Equal(final.Inconclusive[0].Units, []string{"dedupe", "resume"}) ||
		!slices.Equal(final.UnitsFailing, []string{"dedupe", "resume"}) || !slices.Equal(final.UnitsInconclusive, []string{"dedupe", "resume"}) {
		t.Fatalf("the final measure %+v", final)
	}
}

func journeyDocumentAssociation(t *testing.T, f *journey.ShedFixture, stream config.WorkstreamID, unit string) (execution.FeatureAssociation, bool) {
	t.Helper()
	return journeyDocument[execution.FeatureAssociation](t, f, stream, execution.FeaturesDocument(unit))
}
