package service

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
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

func previewSettings() *config.Preview {
	return &config.Preview{Call: []string{"web", "--message", "a string; $(never executed)"}, Port: 8080, Path: "/settings", Timeout: "1m"}
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
	cfg.Project.Preview = previewSettings()
	for i := range cfg.Projects {
		if cfg.Projects[i].ID == repo.Project() {
			cfg.Projects[i].Preview = cfg.Project.Preview
		}
	}
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
		{json.RawMessage(`{"path":"/settings?a=1&b=2","viewport":"phone","full_page":true}`), head, "feature head\n"},
		{json.RawMessage(`{"unit":"resume"}`), candidate, "unit candidate\n"},
	} {
		tool := controls.preview(cfg, repo, scope, previewView(t), f.s.now)
		result := previewInvoke(t, tool, tc.raw)
		if result.ImageID == "" || result.Commit != tc.commit {
			t.Fatalf("preview: %+v", result)
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
		if !slices.Equal(args, append(append([]string{"call"}, cfg.Project.Preview.Call...), "id")) {
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
	if !strings.Contains(node[2], `"url":"http://preview:8080/settings"`) || !strings.Contains(node[2], `"width":1440`) || !strings.Contains(node[2], `"fullPage":false`) {
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
	result := previewInvoke(t, tool, json.RawMessage(`{"unit":"resume"}`))
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
	scope := previewScope(t, repo, stream, f.s.now)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	tool := controls.preview(cfg, repo, scope, previewView(t), f.s.now)
	if result := previewInvoke(t, tool, json.RawMessage(`{}`)); !strings.Contains(result.Limitation, "no [preview]") {
		t.Fatalf("%+v", result)
	}
	cfg.Project.Preview = previewSettings()
	for i := range cfg.Projects {
		cfg.Projects[i].Preview = cfg.Project.Preview
	}
	for _, raw := range []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`{"unit":"unknown"}`)} {
		if result := previewInvoke(t, tool, raw); result.Limitation == "" {
			t.Fatalf("missing limitation: %+v", result)
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

func seedPreviewOperation(t *testing.T, repo *trace.Repository, scope coreadapter.Scope, raw json.RawMessage, stream config.WorkstreamID, commit string, settings *config.Preview) previewOperation {
	t.Helper()
	var in previewInput
	must(t, json.Unmarshal(raw, &in))
	if in.Path == "" {
		in.Path = settings.Path
	}
	if in.Viewport == "" {
		in.Viewport = "desktop"
	}
	op := previewOperation{Scope: scope, Call: raw, Workstream: stream, Commit: commit, Preview: *settings, Capture: checks.PreviewCapture{Call: settings.Call, Port: settings.Port, Path: in.Path, Viewport: in.Viewport, FullPage: in.FullPage}}
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
			op := seedPreviewOperation(t, chief, scope, raw, member, strings.Repeat("a", 40), previewSettings())
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
			if result := previewInvoke(t, tool, json.RawMessage(`{}`)); !strings.Contains(result.Limitation, "qualify workstream") {
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
	raw := json.RawMessage(`{"path":"/phone","viewport":"phone","full_page":true}`)
	seedPreviewOperation(t, repo, scope, raw, stream, commit, previewSettings())
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
	commit := moveFeature(t, f, stream, map[string]string{"preview-source.txt": "failure\n"})
	scope := previewScope(t, repo, stream, f.s.now)
	raw := json.RawMessage(`{}`)
	seedPreviewOperation(t, repo, scope, raw, stream, commit, previewSettings())
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
}

func TestChiefPreviewSlotClockAndMaintenance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Service{checkSlots: &checkSlotPool{limit: 1}}
		run := CheckSlotRun{Project: project, Workstream: stream, Preview: true}
		release, err := s.checkSlots.acquire(t.Context(), CheckSlotRun{Unit: "holder"})
		must(t, err)
		done := make(chan error, 1)
		go func() {
			_, err := s.withPreviewSlot(t.Context(), run, "1m", func(ctx context.Context) ([]byte, error) {
				deadline, _ := ctx.Deadline()
				if time.Until(deadline) != time.Minute {
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
			_, err := s.withPreviewSlot(t.Context(), run, "1m", func(ctx context.Context) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() })
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
	settings := previewSettings()
	// The production loader bounds this to at least one minute; shorten only
	// the seeded operation so the offline cancellation test finishes promptly.
	settings.Timeout = "1s"
	raw := json.RawMessage(`{}`)
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
	raw = json.RawMessage(`{"path":"/cancel"}`)
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
