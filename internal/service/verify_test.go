package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/verification"
	"github.com/kpenfound/osmia/internal/verifytool"
)

const verifyReport = `{"version":1,"claims":[
 {"name":"cart total","status":"pass","detail":"shows 3","evidence":["shots/cart.png","logs/console.txt"],"flow":"checkout"},
 {"name":"coupon","status":"fail","detail":"no coupon field","evidence":["logs/console.txt"],"flow":"checkout"},
 {"name":"banner","status":"inconclusive","detail":"slow page","evidence":[]}]}`

type leverFake struct {
	log, source, slow string
	png               []byte
}

func installLever(t *testing.T) leverFake {
	t.Helper()
	png := attachmentPNG(t)
	return installLeverFiles(t, map[string][]byte{"report.json": []byte(verifyReport), "shots/cart.png": png, "logs/console.txt": []byte("console: ready\n")}, png)
}

func installLeverFiles(t *testing.T, files map[string][]byte, png []byte) leverFake {
	t.Helper()
	bin, work := t.TempDir(), t.TempDir()
	out := filepath.Join(work, "out")
	f := leverFake{log: filepath.Join(work, "log"), source: filepath.Join(work, "source"), slow: filepath.Join(work, "slow"), png: png}
	fixturekit.Must(t, os.MkdirAll(out, 0o700))
	for name, data := range files {
		fixturekit.Must(t, os.MkdirAll(filepath.Dir(filepath.Join(out, name)), 0o700))
		fixturekit.Must(t, os.WriteFile(filepath.Join(out, name), data, 0o600))
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
cat verify-source.txt >> %q 2>/dev/null
if [ -f %q ]; then exec sleep 120; fi
for last; do :; done
cp -R %q/. "$last"
`, f.log, f.source, f.slow, out)
	fixturekit.Must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

func (f leverFake) runs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.log)
	if os.IsNotExist(err) {
		return nil
	}
	fixturekit.Must(t, err)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

type verifyRig struct {
	f         *shedFixture
	stream    config.WorkstreamID
	repo      *trace.Repository
	cfg       *config.Config
	lever     leverFake
	candidate string
	head      string
	scope     coreadapter.Scope
	controls  *runtimeControls
}

func newVerifyRig(t *testing.T) *verifyRig {
	t.Helper()
	return newVerifyRigWith(t, installLever(t))
}

func newVerifyRigWith(t *testing.T, lever leverFake) *verifyRig {
	t.Helper()
	t.Setenv("OSMIA_VERIFY_TOKEN", "synthetic-verify-cloud-token")
	f, stream, repo, _ := newFinalFixture(t, "verify-source")
	cfg := f.s.about(repo)
	cfg.Dagger = config.DaggerConfig{Engine: "tcp://verify-engine:1234", CloudComputeTokenEnv: "OSMIA_VERIFY_TOKEN"}
	cfg.Project.Verification = verification.Setting{Call: []string{"lever"}, MapDir: verification.DefaultMapDir}
	candidate := moveFeature(t, f, stream, map[string]string{"verify-source.txt": "unit candidate\n"})
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: reportDocument("resume"), Revision: 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: serviceActor, Cause: "verify"}
	report, _ := json.Marshal(UnitReport{Candidate: candidate})
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: "units/resume/report.json", Content: string(report)}}))
	head := moveFeature(t, f, stream, map[string]string{"verify-source.txt": "feature head\n"})
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	return &verifyRig{f: f, stream: stream, repo: repo, cfg: cfg, lever: lever, candidate: candidate, head: head, scope: previewScope(t, repo, stream, f.s.now), controls: controls}
}

func verifyInvoke(t *testing.T, tool coreadapter.Tool, raw string) verifytool.Result {
	t.Helper()
	data, err := tool.Handle(context.Background(), json.RawMessage(raw))
	fixturekit.Must(t, err)
	var result verifytool.Result
	fixturekit.Must(t, json.Unmarshal(data, &result))
	return result
}

func readView(t *testing.T, view interface{ Read(string) ([]byte, error) }, name string) []byte {
	t.Helper()
	data, err := view.Read(name)
	fixturekit.Must(t, err)
	return data
}

func TestVerifyRunsTheLeverOnTheFeatureHeadAndRetainsEveryReferencedFile(t *testing.T) {
	r := newVerifyRig(t)
	view := previewView(t)
	tool := r.controls.verify(r.cfg, r.repo, r.scope, view, r.f.s.now)
	result := verifyInvoke(t, tool, `{"call":["flow","--name=checkout"]}`)
	if result.Limitation != "" || result.RunID == "" || result.Commit != r.head || result.Status != verification.Fail || len(result.Claims) != 3 {
		t.Fatalf("result %+v", result)
	}
	runs := r.lever.runs(t)
	if len(runs) != 1 || !strings.HasPrefix(runs[0], "call lever --run-id "+result.RunID+" flow --name=checkout export --path ") {
		t.Fatalf("lever runs %q", runs)
	}
	if source, _ := os.ReadFile(r.lever.source); string(source) != "feature head\n" {
		t.Fatalf("the lever ran against %q", source)
	}
	if claim := result.Claims[0]; claim.Flow != "checkout" || claim.Status != verification.Pass || !slices.Equal(claim.Evidence, []string{".osmia-verify/" + result.RunID + "/shots/cart.png", ".osmia-verify/" + result.RunID + "/logs/console.txt"}) {
		t.Fatalf("claim %+v", claim)
	}
	if len(result.Flows) != 1 || result.Flows[0].Flow != "checkout" || result.Flows[0].Status != verification.Fail || result.Untagged != 1 {
		t.Fatalf("flows %+v untagged %d", result.Flows, result.Untagged)
	}
	for _, ref := range []string{result.Claims[0].Evidence[0]} {
		if !bytes.Equal(readView(t, view, ref), r.lever.png) {
			t.Fatalf("image at %s differs", ref)
		}
	}
	if len(result.Images) != 1 || !bytes.Equal(readView(t, view, result.Images[0]), r.lever.png) || !strings.HasPrefix(result.Images[0], "images/") {
		t.Fatalf("images %v", result.Images)
	}
	if got := string(readView(t, view, result.Claims[0].Evidence[1])); got != "console: ready\n" {
		t.Fatalf("text evidence %q", got)
	}
	if got := readView(t, view, result.Report); !bytes.Contains(got, []byte(`"version":1`)) {
		t.Fatalf("report %s", got)
	}
	ops, _, err := verifytool.Operations(r.repo, r.stream)
	fixturekit.Must(t, err)
	if len(ops) != 1 || ops[0].RunID != result.RunID || ops[0].Commit != r.head || !slices.Equal(ops[0].Args, []string{"flow", "--name=checkout"}) || !ops[0].Finished {
		t.Fatalf("recorded %+v", ops)
	}
	if r.f.s.CheckSlots().Used != 0 {
		t.Fatal("the run kept its slot")
	}
}

func TestVerifyOfAUnitCandidateRunsOnThatCommit(t *testing.T) {
	r := newVerifyRig(t)
	tool := r.controls.verify(r.cfg, r.repo, r.scope, previewView(t), r.f.s.now)
	result := verifyInvoke(t, tool, `{"unit":"resume","call":["flow"]}`)
	if result.Limitation != "" || result.Commit != r.candidate {
		t.Fatalf("result %+v", result)
	}
	if source, _ := os.ReadFile(r.lever.source); string(source) != "unit candidate\n" {
		t.Fatalf("the lever ran against %q", source)
	}
	if limited := verifyInvoke(t, tool, `{"unit":"missing","call":["flow"]}`); !strings.Contains(limited.Limitation, "no recorded candidate") {
		t.Fatalf("unknown unit: %+v", limited)
	}
}

func TestVerifyTwoNewCallsOnTheSameCommitAndArgumentsEachRunWithADistinctRunID(t *testing.T) {
	r := newVerifyRig(t)
	view := previewView(t)
	tool := r.controls.verify(r.cfg, r.repo, r.scope, view, r.f.s.now)
	first := verifyInvoke(t, tool, `{"call":["flow","--name=checkout"]}`)
	second := verifyInvoke(t, tool, `{"call":["flow","--name=checkout"]}`)
	if first.RunID == "" || second.RunID == "" || first.RunID == second.RunID || first.Commit != second.Commit {
		t.Fatalf("runs %+v %+v", first, second)
	}
	runs := r.lever.runs(t)
	if len(runs) != 2 || !strings.Contains(runs[0], "--run-id "+first.RunID+" ") || !strings.Contains(runs[1], "--run-id "+second.RunID+" ") {
		t.Fatalf("lever runs %q", runs)
	}
	ops, _, err := verifytool.Operations(r.repo, r.stream)
	fixturekit.Must(t, err)
	if len(ops) != 2 || ops[0].RunID != first.RunID || ops[1].RunID != second.RunID || ops[0].Commit != r.head || ops[1].Commit != r.head {
		t.Fatalf("recorded %+v", ops)
	}
	for _, id := range []string{first.RunID, second.RunID} {
		readView(t, view, verifytool.Ref(id, "shots/cart.png"))
	}
}

func TestVerifyRefusalsReturnALimitationAndStartNoDagger(t *testing.T) {
	r := newVerifyRig(t)
	tool := r.controls.verify(r.cfg, r.repo, r.scope, previewView(t), r.f.s.now)
	for name, raw := range map[string]string{
		"shorthand option":  `{"call":["flow","-x"]}`,
		"export step":       `{"call":["flow","export"]}`,
		"provider scheme":   `{"call":["flow","--url=http://example.com"]}`,
		"parent path":       `{"call":["flow","--steps=../x"]}`,
		"run id":            `{"call":["flow","--run-id","mine"]}`,
		"run id with value": `{"call":["flow","--run-id=mine"]}`,
		"empty":             `{"call":[]}`,
		"timeout too short": `{"call":["flow"],"timeout":"59s"}`,
		"timeout too long":  `{"call":["flow"],"timeout":"31m"}`,
		"timeout invalid":   `{"call":["flow"],"timeout":"soon"}`,
		"unknown field":     `{"call":["flow"],"port":80}`,
		"named workstream":  `{"call":["flow"],"workstream":"w_other"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if result := verifyInvoke(t, tool, raw); result.Limitation == "" || result.RunID != "" {
				t.Fatalf("accepted: %+v", result)
			}
		})
	}
	if runs := r.lever.runs(t); len(runs) != 0 {
		t.Fatalf("dagger ran: %q", runs)
	}
	ops, _, err := verifytool.Operations(r.repo, r.stream)
	fixturekit.Must(t, err)
	if len(ops) != 0 {
		t.Fatalf("refused calls were recorded: %+v", ops)
	}
	for _, raw := range []string{`{"call":["flow"],"timeout":"1m"}`, `{"call":["flow"],"timeout":"30m"}`} {
		if result := verifyInvoke(t, tool, raw); result.Limitation != "" {
			t.Fatalf("%s: %+v", raw, result)
		}
	}
}

func TestVerifyWithoutTheSettingRunsNothing(t *testing.T) {
	r := newVerifyRig(t)
	r.cfg.Project.Verification = verification.Setting{}
	tool := r.controls.verify(r.cfg, r.repo, r.scope, previewView(t), r.f.s.now)
	if result := verifyInvoke(t, tool, `{"call":["flow"]}`); !strings.Contains(result.Limitation, "no verification setting") {
		t.Fatalf("result %+v", result)
	}
	if runs := r.lever.runs(t); len(runs) != 0 {
		t.Fatalf("dagger ran: %q", runs)
	}
}

func seedVerifyOperation(t *testing.T, r *verifyRig, raw string, timeout string) verifytool.Operation {
	t.Helper()
	var in verifytool.Input
	fixturekit.Must(t, json.Unmarshal([]byte(raw), &in))
	key := trace.ToolCallID(r.scope, verifytool.ToolName, json.RawMessage(raw))
	id := "verify-" + strings.TrimPrefix(key, "tool-")
	op := verifytool.Operation{ID: id, Scope: r.scope, Call: json.RawMessage(raw), Workstream: r.stream, Commit: r.head, Args: in.Call, Timeout: timeout, RunID: verifytool.NewRunID()}
	_, err := r.repo.BeginTool(context.Background(), r.scope, coreadapter.Tool{Name: verifytool.ToolName, Effect: coreadapter.ToolMemory}, json.RawMessage(raw), r.f.s.now)
	fixturekit.Must(t, err)
	data, _ := json.Marshal(op)
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: r.repo.Project(), Workstream: r.stream, At: r.f.s.now(), Actor: serviceActor, Cause: r.scope.Turn}, Path: "tools/" + id + ".json", Content: string(data)}
	fixturekit.Must(t, r.repo.RecordDocuments(context.Background(), []trace.Document{doc}))
	return op
}

func TestVerifyHoldsAChecksSlotWhileRunningAndIsCancelledAtItsTimeout(t *testing.T) {
	r := newVerifyRig(t)
	fixturekit.Must(t, os.WriteFile(r.lever.slow, nil, 0o600))
	raw := `{"call":["flow","--name=slow"]}`
	seedVerifyOperation(t, r, raw, "1s")
	tool := r.controls.verify(r.cfg, r.repo, r.scope, previewView(t), r.f.s.now)
	sawSlot := make(chan bool, 1)
	go func() {
		deadline := time.After(5 * time.Second)
		for {
			if status := r.f.s.CheckSlots(); status.Used == 1 && len(status.Active) == 1 && status.Active[0].Verify {
				sawSlot <- true
				return
			}
			select {
			case <-deadline:
				sawSlot <- false
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	result := verifyInvoke(t, tool, raw)
	if !<-sawSlot {
		t.Fatal("the run did not hold a checks slot while it ran")
	}
	if !strings.Contains(result.Limitation, "timed out after 1s") || result.RunID == "" {
		t.Fatalf("timeout result %+v", result)
	}
	if r.f.s.CheckSlots().Used != 0 {
		t.Fatal("the timed out run kept its slot")
	}
	if runs := r.lever.runs(t); len(runs) != 1 {
		t.Fatalf("lever runs %q", runs)
	}
}

func unfinishVerifyCall(t *testing.T, r *verifyRig) {
	t.Helper()
	ops, latest, err := verifytool.Operations(r.repo, r.stream)
	fixturekit.Must(t, err)
	op := ops[len(ops)-1]
	op.Finished = false
	data, _ := json.Marshal(op)
	doc := latest[op.ID]
	doc.Revision++
	doc.Content = string(data)
	fixturekit.Must(t, r.repo.RecordDocuments(context.Background(), []trace.Document{doc}))
}

func TestVerifyReplayAfterAnInterruptedTurnRunsNothingAndRestoresTheFiles(t *testing.T) {
	r := newVerifyRig(t)
	raw := `{"call":["flow","--name=checkout"]}`
	first := verifyInvoke(t, r.controls.verify(r.cfg, r.repo, r.scope, previewView(t), r.f.s.now), raw)
	if first.Limitation != "" {
		t.Fatalf("first %+v", first)
	}
	unfinishVerifyCall(t, r)
	replayView := previewView(t)
	replayed := verifyInvoke(t, r.controls.verify(r.cfg, r.repo, r.scope, replayView, r.f.s.now), raw)
	if replayed.RunID != first.RunID || replayed.Status != first.Status || len(replayed.Claims) != len(first.Claims) {
		t.Fatalf("replay %+v, first %+v", replayed, first)
	}
	if got := readView(t, replayView, first.Claims[0].Evidence[0]); !bytes.Equal(got, r.lever.png) {
		t.Fatal("the replayed image is not in the view")
	}

	unfinishVerifyCall(t, r)
	resumed := r.scope
	resumed.Turn = "preview-continuation"
	view := previewView(t)
	fixturekit.Must(t, verifytool.Restore(context.Background(), r.repo, resumed, view, r.f.s.now))
	if got := readView(t, view, first.Claims[0].Evidence[0]); !bytes.Equal(got, r.lever.png) {
		t.Fatal("the image was not restored")
	}
	if got := readView(t, view, first.Images[0]); !bytes.Equal(got, r.lever.png) {
		t.Fatal("the image under images/ was not restored")
	}
	if got := string(readView(t, view, first.Claims[0].Evidence[1])); got != "console: ready\n" {
		t.Fatalf("text evidence %q", got)
	}
	var recovered []verifytool.Result
	fixturekit.Must(t, json.Unmarshal(readView(t, view, verifytool.EvidenceDir+"/results.json"), &recovered))
	if len(recovered) != 1 || recovered[0].RunID != first.RunID {
		t.Fatalf("recovered %+v", recovered)
	}
	if runs := r.lever.runs(t); len(runs) != 1 {
		t.Fatalf("recovery started a lever run: %q", runs)
	}
}

func TestVerifyRestoreLeavesFinishedCallsOfEarlierTurnsAlone(t *testing.T) {
	r := newVerifyRig(t)
	first := verifyInvoke(t, r.controls.verify(r.cfg, r.repo, r.scope, previewView(t), r.f.s.now), `{"call":["flow"]}`)
	later := r.scope
	later.Turn = "later"
	view := previewView(t)
	fixturekit.Must(t, verifytool.Restore(context.Background(), r.repo, later, view, r.f.s.now))
	if _, err := view.Read(first.Claims[0].Evidence[0]); err == nil {
		t.Fatal("a finished call of an earlier turn was restored")
	}
}

func TestVerifyIsOfferedOnlyToTheManagerReviewersAndTheFinalReviewerOfAProjectWithTheSetting(t *testing.T) {
	f, stream, repository := newReviewFixture(t, "verify-offered")
	scopes := map[string]coreadapter.Scope{
		"manager":       {Project: string(f.project), Workstream: string(stream), Role: trace.Manager, Thread: trace.Manager, Turn: "turn"},
		"unit reviewer": {Project: string(f.project), Workstream: string(stream), Role: reviewerRole, Thread: reviewerAgent("resume"), Unit: "resume", Turn: "turn"},
		"drift review":  {Project: string(f.project), Workstream: string(stream), Role: reviewerRole, Thread: driftReviewerAgent, Turn: "turn"},
		"mason":         {Project: string(f.project), Workstream: string(stream), Role: masonRole, Thread: masonAgent("resume"), Unit: "resume", Turn: "turn"},
		"session":       {Project: string(f.project), Workstream: string(stream), Role: sessionRole, Thread: "session", Turn: "turn"},
	}
	want := map[string]bool{"manager": true, "unit reviewer": true}
	bound, err := Enforce(f.s.options, Enforcement{}).Threads(repository, f.s.cfg)
	fixturekit.Must(t, err)
	turns := bound.(thread.Dispatcher).Runner.Turns.(*questions.Turns).Turns.(*verdictTurns).Turns.(*reportingTurns).Turns.(*retriedTurns).Turns.(*isolation.Turns)
	for _, enabled := range []bool{true, false} {
		f.s.cfg.Project.Verification = verification.Setting{}
		if enabled {
			f.s.cfg.Project.Verification = verification.Setting{Call: []string{"lever"}}
		}
		for name, scope := range scopes {
			offered := slices.Contains(turns.Offered(context.Background(), scope), verifytool.ToolName)
			if offered != (enabled && want[name]) {
				t.Errorf("%s with setting %v: granted %v", name, enabled, offered)
			}
			view, err := turns.ViewTools(context.Background(), scope, previewView(t))
			fixturekit.Must(t, err)
			var names []string
			for _, tool := range view {
				names = append(names, tool.Name)
			}
			if slices.Contains(names, verifytool.ToolName) != (enabled && want[name]) {
				t.Errorf("%s with setting %v: view tools %v", name, enabled, names)
			}
			if name == "manager" && !slices.Contains(names, "preview") {
				t.Errorf("manager view tools %v lose preview with setting %v", names, enabled)
			}
		}
	}
	for role, grant := range turns.Grants {
		if slices.Contains(grant.Tools, verifytool.ToolName) {
			t.Errorf("the %s grant names verify without a setting", role)
		}
	}
}

func TestVerifyIsOfferedToTheFinalReviewerOnlyForAProjectWithTheSetting(t *testing.T) {
	r := newVerifyRig(t)
	finals := &delivery.FinalReviewer{S: r.f.s.delivery(), Repository: r.repo}
	report := delivery.FinalReport{Reader: "reader", Commit: r.head}
	in := delivery.FinalReviewInput{}
	granted := func() bool {
		return slices.Contains(finals.Turns(r.stream, in, report).Grants[committeeRole].Tools, verifytool.ToolName)
	}
	if !granted() {
		t.Fatal("the final reviewer of a project with the setting is not given verify")
	}
	r.cfg.Project.Verification = verification.Setting{}
	if granted() {
		t.Fatal("the final reviewer of a project without the setting is given verify")
	}
}

func verifyThread(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, role, thread, unit, turn string, now func() time.Time) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	fixturekit.Must(t, repo.CreateThread(ctx, trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: thread, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: now(), Actor: serviceActor, Cause: "verify"}, Role: role, ThreadID: thread}))
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request-" + turn, Revision: 1, Project: repo.Project(), Workstream: stream, Unit: unit, At: now(), Actor: serviceActor, Cause: "verify"}
	_, err := repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: thread, ThreadID: thread, TurnID: turn, Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "verify"})
	fixturekit.Must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, thread, turn+"-claim", t.TempDir(), now())
	fixturekit.Must(t, err)
	return coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Thread: thread, Turn: turn, Role: role, Unit: unit}
}

func TestVerifyByAReviewerAndByTheFinalReviewerRunsOnTheirOwnCommit(t *testing.T) {
	for _, tc := range []struct {
		name, role, thread, unit, commit string
	}{
		{"unit reviewer", reviewerRole, "verify_reviewer", "resume", ""},
		{"final reviewer", committeeRole, "reader", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newVerifyRig(t)
			commit := r.candidate
			if tc.role == committeeRole {
				commit = r.head
			}
			scope := verifyThread(t, r.repo, r.stream, tc.role, tc.thread, tc.unit, "turn-1", r.f.s.now)
			view := previewView(t)
			tool := verifytool.Tool(verifytool.Env{Config: r.cfg, Repo: r.repo, Scope: scope, View: view, Now: r.f.s.now, Slots: r.controls.verifySlots,
				Resolve: func(ctx context.Context, in verifytool.Input) (verifytool.Target, string, error) {
					g, err := featureWorkspaces(r.cfg, r.repo).Of(r.stream)
					return verifytool.Target{Repo: r.repo, Stream: r.stream, Config: r.cfg, Unit: scope.Unit, Commit: commit, Export: g.ExportValidated}, "", err
				}})
			result := verifyInvoke(t, tool, `{"call":["flow"]}`)
			if result.Limitation != "" || result.Commit != commit {
				t.Fatalf("result %+v", result)
			}
			if named := verifyInvoke(t, tool, `{"call":["flow"],"unit":"resume"}`); named.Limitation == "" {
				t.Fatalf("a %s named a unit: %+v", tc.name, named)
			}
			readView(t, view, result.Claims[0].Evidence[0])
			ops, _, err := verifytool.Operations(r.repo, r.stream)
			fixturekit.Must(t, err)
			if len(ops) != 1 || ops[0].Scope.Role != tc.role || ops[0].Commit != commit {
				t.Fatalf("recorded %+v", ops)
			}
			if got, want := string(r.sourceRan(t)), map[bool]string{true: "feature head\n", false: "unit candidate\n"}[tc.role == committeeRole]; got != want {
				t.Fatalf("ran against %q, want %q", got, want)
			}
		})
	}
}

func (r *verifyRig) sourceRan(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(r.lever.source)
	fixturekit.Must(t, err)
	return data
}

func TestVerifyResultsLeaveTheUnitsReviewStateAlone(t *testing.T) {
	f, stream, repository := newReviewFixture(t, "verify-state")
	before, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	lever := installLever(t)
	t.Setenv("OSMIA_VERIFY_TOKEN", "synthetic-verify-cloud-token")
	cfg := f.s.about(repository)
	cfg.Dagger = config.DaggerConfig{Engine: "tcp://verify-engine:1234", CloudComputeTokenEnv: "OSMIA_VERIFY_TOKEN"}
	cfg.Project.Verification = verification.Setting{Call: []string{"lever"}}
	scope := verifyThread(t, repository, stream, reviewerRole, "verify_reviewer", "resume", "review-1", f.s.now)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	g, err := featureWorkspaces(cfg, repository).Of(stream)
	fixturekit.Must(t, err)
	head, _, err := g.Branch(context.Background(), featureBranch(stream))
	fixturekit.Must(t, err)
	tool := verifytool.Tool(verifytool.Env{Config: cfg, Repo: repository, Scope: scope, View: previewView(t), Now: f.s.now, Slots: controls.verifySlots,
		Resolve: func(context.Context, verifytool.Input) (verifytool.Target, string, error) {
			return verifytool.Target{Repo: repository, Stream: stream, Config: cfg, Unit: "resume", Commit: head, Export: g.ExportValidated}, "", nil
		}})
	if result := verifyInvoke(t, tool, `{"call":["flow"]}`); result.Status != verification.Fail || len(lever.runs(t)) != 1 {
		t.Fatalf("result %+v", result)
	}
	after, err := repository.Workflow(stream, trace.UnitSubject("resume"))
	fixturekit.Must(t, err)
	if after.Value != before.Value || after.Version != before.Version {
		t.Fatalf("the unit moved from %+v to %+v", before, after)
	}
}

func TestAnEnforcedManagerTurnHoldsVerifyOnlyWithTheSettingAndKeepsPreview(t *testing.T) {
	f, stream, repository := newReviewFixture(t, "manager-verify")
	for _, enabled := range []bool{false, true} {
		f.s.cfg.Project.Verification = verification.Setting{}
		if enabled {
			f.s.cfg.Project.Verification = verification.Setting{Call: []string{"lever"}}
		}
		turns, engine, hosts := enforcedTurns(t, f, repository)
		got := runEnforced(t, turns, engine, hosts, coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: trace.Manager, Thread: trace.Manager, Turn: fmt.Sprintf("turn-%v", enabled)}, "claude")
		if slices.Contains(got.MCP, verifytool.ToolName) != enabled || !slices.Contains(got.MCP, "preview") {
			t.Fatalf("setting %v: manager tools %v", enabled, got.MCP)
		}
		want := ManagerOrdinaryToolsOf("feature")
		if enabled {
			want = append(want, verifytool.ToolName)
		}
		names := slices.Clone(got.MCP)
		slices.Sort(names)
		slices.Sort(want)
		if !slices.Equal(names, want) {
			t.Fatalf("setting %v: manager tools %v, want %v", enabled, names, want)
		}
	}
}

func TestVerifyOfARunWithoutAReportIsInconclusiveWithItsReasonAndCoversNoFlow(t *testing.T) {
	r := newVerifyRigWith(t, installLeverFiles(t, map[string][]byte{"logs/console.txt": []byte("no report\n")}, nil))
	result := verifyInvoke(t, r.controls.verify(r.cfg, r.repo, r.scope, previewView(t), r.f.s.now), `{"call":["flow"]}`)
	if result.Status != verification.Inconclusive || !strings.Contains(result.Rejected, "report.json is missing") || len(result.Claims) != 0 || len(result.Flows) != 0 || result.Limitation != "" {
		t.Fatalf("result %+v", result)
	}
}
