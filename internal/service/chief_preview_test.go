package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

func previewScope(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, now func() time.Time) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	_, err := repo.EnsureChiefOfStaff(ctx, stream, now(), ownerActor)
	must(t, err)
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request-preview", Revision: 1, Project: repo.Project(), Workstream: stream, At: now(), Actor: ownerActor, Cause: "preview"}
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "preview", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Show the page"})
	must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "preview-claim", t.TempDir(), now())
	must(t, err)
	return coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Thread: trace.ChiefOfStaff, Turn: "preview", Role: trace.ChiefOfStaff}
}

func previewView(t *testing.T) *isolation.FileView {
	t.Helper()
	source := t.TempDir()
	must(t, os.WriteFile(filepath.Join(source, "status.json"), []byte("{}"), 0600))
	view, err := (isolation.Views{Directory: t.TempDir()}).Create(context.Background(), coreadapter.Workspace{Directory: source, Access: coreadapter.ReadOnly}, []string{"status.json"})
	must(t, err)
	t.Cleanup(func() { must(t, view.Release(context.Background())) })
	return view
}

func previewServiceFixture() *previewService {
	return &previewService{Call: []string{"web", "--message", "it's a string; $(never executed)"}, Port: 8080, Path: "/settings", Timeout: "1m"}
}

func previewInvoke(t *testing.T, tool coreadapter.Tool, raw json.RawMessage) previewResult {
	t.Helper()
	data, err := tool.Handle(context.Background(), raw)
	must(t, err)
	var result previewResult
	must(t, json.Unmarshal(data, &result))
	return result
}

func TestChiefPreviewExactSourceAndRestart(t *testing.T) {
	fake := fixturekit.InstallPreviewCLIs(t, attachmentPNG(t))
	t.Setenv("OSMIA_PREVIEW_TOKEN", "synthetic-preview-cloud-token")
	for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GITHUB_TOKEN", "SSH_AUTH_SOCK", "GH_TOKEN"} {
		t.Setenv(key, "forbidden-preview-credential")
	}
	f, stream, repo, _ := newFinalFixture(t, "preview-source")
	cfg := f.s.about(repo)
	cfg.Dagger = config.DaggerConfig{Engine: "tcp://preview-engine:1234", CloudTokenEnv: "OSMIA_PREVIEW_TOKEN"}
	candidate := moveFeature(t, f, stream, map[string]string{"preview-source.txt": "unit candidate\n"})
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: reportDocument("resume"), Revision: 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: serviceActor, Cause: "preview"}
	report, _ := json.Marshal(UnitReport{Candidate: candidate})
	must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: "units/resume/report.json", Content: string(report)}}))
	head := moveFeature(t, f, stream, map[string]string{"preview-source.txt": "feature head\n"})
	scope := previewScope(t, repo, stream, f.s.now)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	for _, tc := range []struct {
		raw            json.RawMessage
		commit, source string
	}{
		{json.RawMessage(`{"call":["web","--message","it's a string; $(never executed)"],"port":8080,"path":"/settings?a=1&b=2","viewport":"phone","full_page":true}`), head, "feature head\n"},
		{json.RawMessage(`{"unit":"resume","call":["web","--message","it's a string; $(never executed)"],"port":8080}`), candidate, "unit candidate\n"},
	} {
		tool := controls.preview(cfg, repo, scope, previewView(t), f.s.now)
		result := previewInvoke(t, tool, tc.raw)
		if result.ImageID == "" || result.Commit != tc.commit {
			t.Fatalf("preview: %+v", result)
		}
		doc, err := latestPreviewDocument(repo, stream, "preview-"+strings.TrimPrefix(trace.ToolCallID(scope, "preview", tc.raw), "tool-"))
		must(t, err)
		var pin previewOperation
		must(t, json.Unmarshal([]byte(doc.Content), &pin))
		if !pin.AgentChosen || pin.Preview.Timeout != "5m" || pin.Preview.Port != 8080 {
			t.Fatalf("service pin: %+v", pin)
		}
		if tc.commit == candidate && pin.Capture.Path != "/" {
			t.Fatal("omitted path did not default to /")
		}

		var code struct{ Commit string }
		inspect := json.RawMessage(`{"path":"preview-source.txt"}`)
		if tc.commit == candidate {
			inspect = json.RawMessage(`{"unit":"resume","path":"preview-source.txt"}`)
		}
		raw, err := inspectCode(cfg, repo, scope, f.s.now).Handle(context.Background(), inspect)
		must(t, err)
		must(t, json.Unmarshal(raw, &code))
		if code.Commit != result.Commit {
			t.Fatalf("preview %s differs from inspect %s", result.Commit, code.Commit)
		}
		exported, err := os.ReadFile(fake.Source)
		must(t, err)
		if string(exported) != tc.source {
			t.Fatalf("exported %q", exported)
		}
		image, err := repo.StoredImage(stream, result.ImageID)
		must(t, err)
		if image.Workstream != stream || !bytes.Equal(image.Data, attachmentPNG(t)) {
			t.Fatal("image not stored in shown trace")
		}
		call, err := os.ReadFile(fake.Call)
		must(t, err)
		args := strings.Split(strings.TrimSuffix(string(call), "\x00"), "\x00")
		if !slices.Equal(args, append(append([]string{"call"}, previewServiceFixture().Call...), "id")) {
			t.Fatalf("call argv %q", args)
		}
	}
	capture, err := os.ReadFile(fake.Capture)
	must(t, err)
	args := strings.Split(strings.TrimSuffix(string(capture), "\x00"), "\x00")
	if !slices.Contains(args, checks.ChromiumImage) || !slices.Contains(args, "with-service-binding") || slices.Contains(args, "up") {
		t.Fatalf("capture argv %q", args)
	}
	i := slices.Index(args, "--args")
	node, err := csv.NewReader(strings.NewReader(args[i+1])).Read()
	must(t, err)
	if !strings.Contains(node[2], `"url":"http://preview:8080/"`) || !strings.Contains(node[2], `"width":1440`) || !strings.Contains(node[2], `"fullPage":false`) {
		t.Fatalf("desktop capture %s", node[2])
	}
	env, err := os.ReadFile(fake.Environment)
	must(t, err)
	for _, want := range []string{"PATH=", "DAGGER_ENGINE=tcp://preview-engine:1234", "_EXPERIMENTAL_DAGGER_RUNNER_HOST=tcp://preview-engine:1234", "DAGGER_CLOUD_TOKEN=synthetic-preview-cloud-token"} {
		if !strings.Contains(string(env), want) {
			t.Fatalf("missing environment %s", want)
		}
	}
	for _, forbidden := range []string{"ANTHROPIC_API_KEY=", "OPENAI_API_KEY=", "GITHUB_TOKEN=", "SSH_AUTH_SOCK=", "GH_TOKEN="} {
		if strings.Contains(string(env), forbidden) {
			t.Fatalf("inherited %s", forbidden)
		}
	}
	// Reopening the trace and constructing a fresh tool/view models a restart.
	repo = restartTrace(t, f, repo)
	view := previewView(t)
	tool := controls.preview(cfg, repo, scope, view, f.s.now)
	before, err := os.ReadFile(fake.Runs)
	must(t, err)
	must(t, restorePreviewImages(context.Background(), cfg, controls, repo, scope, view, f.s.now))
	result := previewInvoke(t, tool, json.RawMessage(`{"unit":"resume","call":["web","--message","it's a string; $(never executed)"],"port":8080}`))
	after, err := os.ReadFile(fake.Runs)
	must(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("restart captured again")
	}
	copied, err := view.Read(result.Path)
	must(t, err)
	if !bytes.Equal(copied, attachmentPNG(t)) {
		t.Fatal("missing view image")
	}
	images, err := trace.Read[trace.Image](repo, stream)
	must(t, err)
	if len(images) != 2 {
		t.Fatalf("duplicate images %d", len(images))
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	for _, image := range images {
		completed := 0
		for _, doc := range docs {
			if doc.ID == image.Key && doc.Revision == 2 {
				completed++
			}
		}
		if completed != 1 {
			t.Fatalf("tool completion count %d", completed)
		}
	}
}

func TestChiefPreviewLimitationsCompleteTurn(t *testing.T) {
	f, stream, repo, _ := newFinalFixture(t, "preview-limitations")
	cfg := f.s.about(repo)
	moveFeature(t, f, stream, map[string]string{"preview-source.txt": "validation target\n"})
	scope := previewScope(t, repo, stream, f.s.now)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	tool := controls.preview(cfg, repo, scope, previewView(t), f.s.now)
	fake := fixturekit.InstallPreviewCLIs(t, attachmentPNG(t))
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{}`), json.RawMessage(`{"call":[],"port":8080}`),
		json.RawMessage(`{"call":["web"]}`), json.RawMessage(`{"call":["web"],"port":0}`),
		json.RawMessage(`{"call":["web"],"port":65536}`), json.RawMessage(`{"call":["web"],"port":-1}`),
		json.RawMessage(`{"call":["web"],"port":8080,"path":"relative"}`),
		json.RawMessage(`{"call":["web"],"port":8080,"timeout":"59s"}`),
		json.RawMessage(`{"call":["web"],"port":8080,"timeout":"31m"}`),
		json.RawMessage(`{"call":["web"],"port":8080,"timeout":"invalid"}`),
		json.RawMessage(`{"call":"web","port":8080}`),
		json.RawMessage(`{"call":["web"],"port":8080,"unit":"unknown"}`),
	} {
		if result := previewInvoke(t, tool, raw); result.Limitation == "" {
			t.Fatalf("missing limitation for %s: %+v", raw, result)
		}
	}
	if _, err := os.Stat(fake.Call); !os.IsNotExist(err) {
		t.Fatal("invalid request started Dagger")
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	for _, doc := range docs {
		if strings.HasPrefix(doc.ID, "preview-") {
			t.Fatal("invalid request pinned a capture")
		}
	}
	// Normal results allow the chief to capture and complete its owned reply.
	chief, err := repo.Thread(stream, trace.ChiefOfStaff)
	must(t, err)
	q := chief.Turns[len(chief.Turns)-1]
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.s.now()
	must(t, repo.CaptureTurn(context.Background(), q.Claim.Token, trace.TurnResponse{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: scope.Turn, RequestID: q.Request.ID, RequestRevision: 1, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "preview-session"}, FinalResponse: "Preview is unavailable", StartedAt: q.Claim.At, SessionDirectory: q.Claim.SessionDirectory}}))
	must(t, repo.CompleteTurn(context.Background(), stream, trace.ChiefOfStaff, scope.Turn, q.Claim.Token, f.s.now()))
}

func seedPreviewOperation(t *testing.T, repo *trace.Repository, scope coreadapter.Scope, raw json.RawMessage, stream config.WorkstreamID, commit string, settings *previewService) previewOperation {
	t.Helper()
	var in previewInput
	must(t, json.Unmarshal(raw, &in))
	if in.Path == "" {
		in.Path = settings.Path
	}
	if in.Viewport == "" {
		in.Viewport = "desktop"
	}
	op := previewOperation{Scope: scope, Call: raw, Workstream: stream, Commit: commit, AgentChosen: len(in.Call) > 0, Preview: *settings, Capture: checks.PreviewCapture{Call: settings.Call, Port: settings.Port, Path: in.Path, Viewport: in.Viewport, FullPage: in.FullPage}}
	id := "preview-" + strings.TrimPrefix(trace.ToolCallID(scope, "preview", raw), "tool-")
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: config.WorkstreamID(scope.Workstream), Actor: serviceActor, Cause: scope.Turn}, Path: "tools/" + id + ".json"}
	_, err := repo.BeginTool(context.Background(), scope, coreadapter.Tool{Name: "preview", Effect: coreadapter.ToolMemory}, raw, func() time.Time { return demoStart })
	must(t, err)
	must(t, writePreviewOperation(context.Background(), repo, &doc, op, func() time.Time { return demoStart }))
	return op
}

func TestChiefPreviewReconcilesSharedPublication(t *testing.T) {
	fake := fixturekit.InstallPreviewCLIs(t, attachmentPNG(t))
	for _, cross := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-project", true: "cross-project"}[cross], func(t *testing.T) {
			s, chief, group := workgroupFeedFixture(t, cross, 2)
			now := func() time.Time { return demoStart }
			host := group.Members[0]
			scope := previewScope(t, chief, host, now)
			member := fixturekit.Quiet
			target, _, refusal, err := chiefMember(chief, scope, member)
			must(t, err)
			if refusal != "" {
				t.Fatal(refusal)
			}
			raw, _ := json.Marshal(map[string]any{"workstream": member, "path": "/new-page", "viewport": "phone", "full_page": true})
			op := seedPreviewOperation(t, chief, scope, raw, member, strings.Repeat("a", 40), previewServiceFixture())
			key := trace.ToolCallID(scope, "preview", raw)
			controls := &runtimeControls{}
			controls.service.Store(s)
			view := previewView(t)
			tool := controls.preview(s.cfg, chief, scope, view, now)
			// Crash boundary: the audit started and member receipt exist, while none
			// of the independent follow-up effects has been published.
			_, err = chief.BeginTool(context.Background(), scope, tool, raw, now)
			must(t, err)
			image, err := target.RecordImage(context.Background(), trace.Header{Project: target.Project(), Workstream: member, At: demoStart, Actor: serviceActor, Cause: scope.Turn}, key, "image/png", attachmentPNG(t), scope.Turn)
			must(t, err)
			if result := previewInvoke(t, tool, json.RawMessage(`{"call":["web"],"port":8080}`)); !strings.Contains(result.Limitation, "qualify workstream") {
				t.Fatalf("unqualified shared preview: %+v", result)
			}
			must(t, chief.Close())
			if cross {
				chief, err = trace.OpenWorkgroupTrace(s.cfg.Root, group.ID, []trace.CrossRepoMember{{Workstream: stream, Project: s.projects[0].repository}, {Workstream: member, Project: s.projects[1].repository}})
			} else {
				chief, err = trace.Open(s.cfg.Root, s.cfg.Project)
				s.projects[0].repository = chief
			}
			must(t, err)
			t.Cleanup(func() { chief.Close() })
			must(t, chief.AbandonTurn(context.Background(), host, trace.ChiefOfStaff, scope.Turn, demoStart))
			target, _, _, err = chiefMember(chief, scope, member)
			must(t, err)
			resumed := scope
			resumed.Turn = "preview-continuation"

			for range 3 {
				// A resumed turn creates a fresh view and tool. Restoration must finish
				// publication before the chief reads it, even without another model call.
				view = previewView(t)
				tool = controls.preview(s.cfg, chief, scope, view, now)
				must(t, restorePreviewImages(context.Background(), s.cfg, controls, chief, resumed, view, now))
				result := previewInvoke(t, tool, raw)
				if result.ImageID != image.ID {
					t.Fatalf("changed image %+v", result)
				}
				copied, err := view.Read(result.Path)
				must(t, err)
				if !bytes.Equal(copied, image.Data) {
					t.Fatal("view differs from receipt")
				}
				entries, err := os.ReadDir(filepath.Join(view.Workspace().Directory, "images"))
				must(t, err)
				if len(entries) != 1 {
					t.Fatalf("copies %d", len(entries))
				}
			}
			images, err := trace.Read[trace.Image](target, member)
			must(t, err)
			refs, err := trace.Read[trace.ImageReference](chief, host)
			must(t, err)
			if len(images) != 1 || len(refs) != 1 || refs[0].ImageID != image.ID || refs[0].Member != member {
				t.Fatalf("images %d references %+v", len(images), refs)
			}
			docs, err := trace.Read[trace.Document](chief, host)
			must(t, err)
			completions := 0
			for _, doc := range docs {
				if doc.ID == key && doc.Revision == 2 {
					completions++
				}
			}
			if completions != 1 {
				t.Fatalf("tool completions %d", completions)
			}
			resultDoc, err := latestPreviewDocument(chief, host, "preview-"+strings.TrimPrefix(key, "tool-"))
			must(t, err)
			must(t, json.Unmarshal([]byte(resultDoc.Content), &op))
			if op.Result == nil || op.Result.ImageID != image.ID {
				t.Fatalf("unfinished operation %+v", op)
			}
			if _, err := os.Stat(fake.Runs); !os.IsNotExist(err) {
				t.Fatal("recorded receipt was captured again")
			}
		})
	}
}

func TestChiefPreviewRestartsCaptureWithoutReceipt(t *testing.T) {
	fake := fixturekit.InstallPreviewCLIs(t, attachmentPNG(t))
	f, stream, repo, _ := newFinalFixture(t, "preview-interrupted")
	cfg := f.s.about(repo)
	commit := moveFeature(t, f, stream, map[string]string{"preview-source.txt": "pinned before interruption\n"})
	scope := previewScope(t, repo, stream, f.s.now)
	raw := json.RawMessage(`{"call":["web","--message","it's a string; $(never executed)"],"port":8080,"path":"/phone","viewport":"phone","full_page":true}`)
	seedPreviewOperation(t, repo, scope, raw, stream, commit, previewServiceFixture())
	moveFeature(t, f, stream, map[string]string{"preview-source.txt": "later branch movement\n"})
	repo = restartTrace(t, f, repo)
	must(t, repo.AbandonTurn(context.Background(), stream, trace.ChiefOfStaff, scope.Turn, f.s.now()))
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	view := previewView(t)
	resumed := scope
	resumed.Turn = "preview-continuation"
	must(t, restorePreviewImages(context.Background(), cfg, controls, repo, resumed, view, f.s.now))
	tool := controls.preview(cfg, repo, scope, view, f.s.now)
	result := previewInvoke(t, tool, raw)
	receipt, err := view.Read("previews/results.json")
	must(t, err)
	if !strings.Contains(string(receipt), result.ImageID) {
		t.Fatal("recovered receipt missing from chief view")
	}
	if result.Commit != commit || result.ImageID == "" {
		t.Fatalf("%+v", result)
	}
	source, err := os.ReadFile(fake.Source)
	must(t, err)
	if string(source) != "pinned before interruption\n" {
		t.Fatalf("source %q", source)
	}
	capture, err := os.ReadFile(fake.Capture)
	must(t, err)
	args := strings.Split(strings.TrimSuffix(string(capture), "\x00"), "\x00")
	node, err := csv.NewReader(strings.NewReader(args[slices.Index(args, "--args")+1])).Read()
	must(t, err)
	for _, want := range []string{`"url":"http://preview:8080/phone"`, `"width":390`, `"height":844`, `"fullPage":true`, `"phone":true`} {
		if !strings.Contains(node[2], want) {
			t.Fatalf("missing %s", want)
		}
	}
}

func TestChiefPreviewFailureDiagnostic(t *testing.T) {
	fake := fixturekit.InstallPreviewCLIs(t, attachmentPNG(t))
	secret := "synthetic-preview-diagnostic-token"
	t.Setenv("OSMIA_PREVIEW_TOKEN", secret)
	f, stream, repo, _ := newFinalFixture(t, "preview-failure")
	cfg := f.s.about(repo)
	cfg.Dagger = config.DaggerConfig{Engine: "cloud", CloudTokenEnv: "OSMIA_PREVIEW_TOKEN"}
	moveFeature(t, f, stream, map[string]string{"preview-source.txt": "failure\n"})
	scope := previewScope(t, repo, stream, f.s.now)
	raw := json.RawMessage(`{"call":["broken"],"port":8080}`)
	must(t, os.WriteFile(fake.Failure, []byte(strings.Repeat("output ", 10000)+secret+"\n"), 0600))
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	tool := controls.preview(cfg, repo, scope, previewView(t), f.s.now)
	result := previewInvoke(t, tool, raw)
	if result.ImageID != "" || len(result.Limitation) > 16<<10 || !strings.Contains(result.Limitation, "[redacted]") || strings.Contains(result.Limitation, secret) {
		t.Fatalf("diagnostic length %d", len(result.Limitation))
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	for _, doc := range docs {
		if strings.Contains(doc.Content, secret) {
			t.Fatalf("secret persisted in %s", doc.ID)
		}
	}
	before, err := os.ReadFile(fake.Runs)
	must(t, err)
	again := previewInvoke(t, tool, raw)
	after, err := os.ReadFile(fake.Runs)
	must(t, err)
	if again != result || !bytes.Equal(before, after) {
		t.Fatal("failed operation did not retain diagnostic")
	}
	must(t, os.Remove(fake.Failure))
	corrected := previewInvoke(t, tool, json.RawMessage(`{"call":["web"],"port":8080}`))
	if corrected.ImageID == "" {
		t.Fatalf("corrected request: %+v", corrected)
	}
	runs, err := os.ReadFile(fake.Runs)
	must(t, err)
	if strings.Count(string(runs), "capture") != 2 {
		t.Fatal("different call did not run separately")
	}
}

func TestChiefPreviewSlotClockAndMaintenance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Service{checkSlots: &checkSlotPool{limit: 1}}
		run := CheckSlotRun{Project: project, Workstream: stream, Preview: true}
		release, err := s.checkSlots.acquire(t.Context(), CheckSlotRun{Unit: "holder"})
		must(t, err)
		done := make(chan error, 1)
		go func() {
			_, err := s.withPreviewSlot(t.Context(), run, "5m", func(ctx context.Context) ([]byte, error) {
				deadline, _ := ctx.Deadline()
				if time.Until(deadline) != 5*time.Minute {
					return nil, errors.New("timeout spent while waiting")
				}
				return []byte("captured"), nil
			})
			done <- err
		}()
		synctest.Wait()
		time.Sleep(2 * time.Minute)
		if len(s.CheckSlots().Waiting) != 1 || !s.CheckSlots().Waiting[0].Preview {
			t.Fatal("preview not queued")
		}
		release()
		synctest.Wait()
		must(t, <-done)
		entered, finish := make(chan struct{}), make(chan struct{})
		gateDone := make(chan error, 1)
		go func() {
			gateDone <- s.RestartEngine(t.Context(), gateEngine{run: func(context.Context) error { close(entered); <-finish; return nil }})
		}()
		<-entered
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			_, err := s.withPreviewSlot(ctx, run, "1m", func(context.Context) ([]byte, error) { return nil, errors.New("capture ran during maintenance") })
			done <- err
		}()
		synctest.Wait()
		time.Sleep(2 * time.Minute)
		if s.CheckSlots().Used != 0 || len(s.CheckSlots().Waiting) != 1 {
			t.Fatal("maintenance admitted preview")
		}
		cancel()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting cancel: %v", err)
		}
		close(finish)
		synctest.Wait()
		must(t, <-gateDone)
		go func() {
			_, err := s.withPreviewSlot(t.Context(), run, "5m", func(ctx context.Context) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() })
			done <- err
		}()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout: %v", err)
		}
		if s.CheckSlots().Used != 0 {
			t.Fatal("timeout leaked slot")
		}
	})
}

func TestChiefPreviewTimedOutAndCancelledCapture(t *testing.T) {
	fake := fixturekit.InstallPreviewCLIs(t, attachmentPNG(t))
	f, stream, repo, _ := newFinalFixture(t, "preview-timeout")
	cfg := f.s.about(repo)
	cfg.Dagger = config.DaggerConfig{Engine: "cloud", CloudTokenEnv: "OSMIA_PREVIEW_TOKEN"}
	secret := "synthetic-preview-timeout-token"
	t.Setenv("OSMIA_PREVIEW_TOKEN", secret)
	commit := moveFeature(t, f, stream, map[string]string{"preview-source.txt": "blocked capture\n"})
	scope := previewScope(t, repo, stream, f.s.now)
	settings := previewServiceFixture()
	// New requests bound this to at least one minute; shorten only
	// the seeded operation so the offline cancellation test finishes promptly.
	settings.Timeout = "1s"
	raw := json.RawMessage(`{"call":["web","--message","it's a string; $(never executed)"],"port":8080}`)
	seedPreviewOperation(t, repo, scope, raw, stream, commit, settings)
	must(t, os.WriteFile(fake.Block, []byte("browser blocked: "+secret+"\n"), 0600))
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	tool := controls.preview(cfg, repo, scope, previewView(t), f.s.now)
	result := previewInvoke(t, tool, raw)
	if !strings.Contains(result.Limitation, "deadline exceeded") || strings.Contains(result.Limitation, secret) || len(result.Limitation) > 16<<10 {
		t.Fatalf("timeout diagnostic %q", result.Limitation)
	}
	if f.s.CheckSlots().Used != 0 {
		t.Fatal("timeout retained slot")
	}
	raw = json.RawMessage(`{"call":["web","--message","it's a string; $(never executed)"],"port":8080,"path":"/cancel"}`)
	settings.Timeout = "1m"
	seedPreviewOperation(t, repo, scope, raw, stream, commit, settings)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan json.RawMessage, 1)
	go func() {
		out, err := tool.Handle(ctx, raw)
		if err != nil {
			out = json.RawMessage(`{}`)
		}
		done <- out
	}()
	deadline := time.After(5 * time.Second)
	for {
		runs, _ := os.ReadFile(fake.Runs)
		if strings.Count(string(runs), "capture") == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("capture did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case out := <-done:
		var cancelled previewResult
		must(t, json.Unmarshal(out, &cancelled))
		if !strings.Contains(cancelled.Limitation, "canceled") {
			t.Fatalf("cancellation %s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("running capture was not cancelled")
	}
	if f.s.CheckSlots().Used != 0 {
		t.Fatal("cancellation retained slot")
	}
}

func TestChiefPreviewRefusesUnsafeArguments(t *testing.T) {
	fake := fixturekit.InstallPreviewCLIs(t, attachmentPNG(t))
	f, stream, repo, _ := newFinalFixture(t, "preview-arguments")
	cfg := f.s.about(repo)
	moveFeature(t, f, stream, map[string]string{"preview-source.txt": "arguments\n"})
	scope := previewScope(t, repo, stream, f.s.now)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	tool := controls.preview(cfg, repo, scope, previewView(t), f.s.now)
	for _, value := range []string{
		"-m", "--mod", "-o", "--output", "--mod=x", "-mx", "-oresult", "--output=result",
		"--workdir=inside", "--interactive=true", "--auto-apply=true", "--dot-output=graph", "--dotOutput=graph", "--dot.output=graph", "--dot output=graph", "--allow-llm=all", "--no-mod=true", "--json=true",
		"env://X", "file://secret", "cmd://command", `e"nv://X`, "image:latest",
		"/etc/passwd", "~/secret", "../secret", "a/../secret", `"/etc/passwd"`, "--source=/etc/passwd", `--source="/etc/passwd"`, `"--source=/etc/passwd"`,
		"a,/etc/passwd", "a, /etc/passwd", "--source=a, /etc/passwd", "a, env://X", "a, ../secret",
		"DAGGER_CLOUD_TOKEN", `"DAGGER_CLOUD_TOKEN"`, "--token=DAGGER_CLOUD_TOKEN", "a, DAGGER_CLOUD_TOKEN", "DAGGER_CLOUD_TOKEN?cacheKey=preview", "--token=DAGGER_CLOUD_TOKEN?cacheKey=preview", "DAGGER_CLOUD_TOKEN?cacheKey=a,b", "--token=DAGGER_CLOUD_TOKEN?cacheKey=a,b", "PATH", "HOME",
		"export", "terminal", "up", "apply", "tunnel", "find-up", "a\nb", "a\tb", "a\x00b", "--", "",
	} {
		t.Run(value, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"call": []string{"web", "--value", value, "service"}, "port": 8080})
			if result := previewInvoke(t, tool, raw); result.Limitation == "" {
				t.Fatalf("unsafe call accepted: %+v", result)
			}
			if _, err := os.Stat(fake.Call); !os.IsNotExist(err) {
				t.Fatal("unsafe call started Dagger")
			}
		})
	}
	result := previewInvoke(t, tool, json.RawMessage(`{"call":["web","--name"],"port":8080}`))
	if result.Limitation == "" {
		t.Fatal("trailing flag accepted")
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	must(t, err)
	for _, doc := range docs {
		if strings.HasPrefix(doc.ID, "preview-") {
			t.Fatal("unsafe call pinned for recovery")
		}
	}
}

func TestChiefPreviewSymlinksAndSettingsPins(t *testing.T) {
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) {
			fake := fixturekit.InstallPreviewCLIs(t, attachmentPNG(t))
			f := newDebateFixtureWith(t, 1, 1, "", func(opts *Options) { onWorkspaces(t, *opts, backend) })
			stream, repo := seedBuild(t, f, "preview-symlinks", validPlan, backend, f.upstream(t))
			cfg := f.s.about(repo)
			moveFeature(t, f, stream, map[string]string{"preview-source.txt": "symlinks\n"})
			g, err := featureWorkspaces(cfg, repo).Of(stream)
			must(t, err)
			acquired, err := g.Acquire(context.Background(), vcs.Request{Name: string(stream), Branch: featureBranch(stream)})
			must(t, err)
			w := acquired.(workspace.Worktree)
			must(t, os.Symlink(t.TempDir(), filepath.Join(w.Path, "outside")))
			must(t, os.Symlink("preview-source.txt", filepath.Join(w.Path, "inside")))
			must(t, os.Symlink("outside", filepath.Join(w.Path, "chain")))
			must(t, os.Symlink("inside", filepath.Join(w.Path, "inside-chain")))
			for _, name := range []string{"--link", " padded ", "quo'ted", "comma,link"} {
				must(t, os.Symlink(t.TempDir(), filepath.Join(w.Path, name)))
			}
			tip, _, err := g.Branch(context.Background(), featureBranch(stream))
			must(t, err)
			commit, err := g.Snapshot(context.Background(), w, tip)
			must(t, err)
			fixturekit.DemoGit(t, cfg.Project.Clone, "-C", cfg.Project.Clone, "config", "core.symlinks", "false")
			scope := previewScope(t, repo, stream, f.s.now)
			controls := &runtimeControls{}
			controls.service.Store(f.s)
			tool := controls.preview(cfg, repo, scope, previewView(t), f.s.now)
			for _, value := range []string{"outside", `"outside"`, "a, outside", "--source=outside", "outside/missing", "chain", "chain/missing", "outside#main", "--source=outside#main", "a, outside#main", "--source=a, outside", "--source=\"outside\"", "--source=--link", " padded ", "quo'ted", "comma,link", `"comma,link"`, "--source=comma,link"} {
				raw, _ := json.Marshal(map[string]any{"call": []string{"web", value}, "port": 8080})
				if result := previewInvoke(t, tool, raw); result.Limitation == "" {
					t.Fatalf("symlink accepted: %s", raw)
				}
				if _, err := os.Stat(fake.Call); !os.IsNotExist(err) {
					t.Fatal("symlink started Dagger")
				}
			}
			if result := previewInvoke(t, tool, json.RawMessage(`{"call":["web","--source","inside-chain"],"port":8080}`)); result.ImageID == "" {
				t.Fatalf("internal symlink refused: %+v", result)
			}
			// Settings-pinned documents hold raw input without service arguments
			// and have no agent_chosen field.
			settings := previewServiceFixture()
			settings.Call = []string{"web", "--source", "/etc/passwd", "--other", "outside"}
			raw := json.RawMessage(`{"path":"/pinned"}`)
			seedPreviewOperation(t, repo, scope, raw, stream, commit, settings)
			repo = restartTrace(t, f, repo)
			must(t, repo.AbandonTurn(context.Background(), stream, trace.ChiefOfStaff, scope.Turn, f.s.now()))
			resumed := scope
			resumed.Turn = "preview-continuation"
			view := previewView(t)
			must(t, restorePreviewImages(context.Background(), cfg, controls, repo, resumed, view, f.s.now))
			result := previewInvoke(t, controls.preview(cfg, repo, scope, view, f.s.now), raw)
			if result.ImageID == "" {
				t.Fatalf("settings pin refused: %+v", result)
			}
			call, err := os.ReadFile(fake.Call)
			must(t, err)
			if !slices.Equal(strings.Split(strings.TrimSuffix(string(call), "\x00"), "\x00"), append(append([]string{"call"}, settings.Call...), "id")) {
				t.Fatalf("settings pin changed: %s", call)
			}
			before := bytes.Clone(call)
			raw, _ = json.Marshal(map[string]any{"call": settings.Call, "port": settings.Port})
			result = previewInvoke(t, controls.preview(cfg, repo, scope, view, f.s.now), raw)
			if result.Limitation == "" {
				t.Fatal("new request used settings bypass")
			}
			after, err := os.ReadFile(fake.Call)
			must(t, err)
			if !bytes.Equal(before, after) {
				t.Fatal("refused request ran Dagger")
			}
		})
	}
}

func TestChiefPreviewVerifiesServiceID(t *testing.T) {
	fake := fixturekit.InstallPreviewCLIs(t, attachmentPNG(t))
	f, stream, repo, _ := newFinalFixture(t, "preview-service-id")
	moveFeature(t, f, stream, map[string]string{"preview-source.txt": "service ID\n"})
	scope := previewScope(t, repo, stream, f.s.now)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	tool := controls.preview(f.s.about(repo), repo, scope, previewView(t), f.s.now)
	encoded, err := base64.StdEncoding.DecodeString(fixturekit.PreviewServiceID)
	must(t, err)
	wrongType := base64.StdEncoding.EncodeToString(bytes.ReplaceAll(encoded, []byte("Service"), []byte("Counter")))
	for i, value := range []string{wrongType, "", "Service@fake", "/etc/passwd", "/etc/passwd/", "env://DAGGER_CLOUD_TOKEN", "file:///etc/passwd", "cmd://command", "not an ID", fixturekit.PreviewServiceID, fixturekit.PreviewRecipeServiceID} {
		must(t, os.WriteFile(fake.ServiceID, []byte(value+"\n"), 0600))
		raw, _ := json.Marshal(map[string]any{"call": []string{"web"}, "port": 8080, "path": fmt.Sprintf("/%d", i)})
		result := previewInvoke(t, tool, raw)
		if value != fixturekit.PreviewServiceID && value != fixturekit.PreviewRecipeServiceID {
			if !strings.Contains(result.Limitation, "Service ID") {
				t.Fatalf("invalid ID accepted: %+v", result)
			}
			if _, err := os.Stat(fake.Capture); !os.IsNotExist(err) {
				t.Fatal("invalid ID reached capture")
			}
		} else {
			if result.ImageID == "" {
				t.Fatalf("valid ID refused: %+v", result)
			}
			capture, err := os.ReadFile(fake.Capture)
			must(t, err)
			args := strings.Split(string(capture), "\x00")
			if args[slices.Index(args, "--service")+1] != value {
				t.Fatal("Service ID changed")
			}
		}
	}
}

func TestChiefPreviewToolContract(t *testing.T) {
	tool := (&runtimeControls{}).preview(nil, nil, coreadapter.Scope{Role: trace.ChiefOfStaff}, nil, time.Now)
	for _, instruction := range []string{"inspect_code", "committed", "call", "port", "Dagger"} {
		if !strings.Contains(tool.Description, instruction) {
			t.Fatalf("missing guidance %s", instruction)
		}
	}
	var schema struct {
		Properties map[string]json.RawMessage
		Required   []string
	}
	must(t, json.Unmarshal(tool.InputSchema, &schema))
	for _, field := range []string{"call", "port", "timeout"} {
		if len(schema.Properties[field]) == 0 {
			t.Fatalf("missing input %s", field)
		}
	}
	if !slices.Equal(schema.Required, []string{"call", "port"}) {
		t.Fatalf("required %q", schema.Required)
	}
}
