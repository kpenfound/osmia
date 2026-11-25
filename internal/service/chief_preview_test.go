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
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

func previewScope(t *testing.T, repo *trace.Repository, stream config.WorkstreamID, now func() time.Time) coreadapter.Scope {
	t.Helper()
	ctx := context.Background()
	_, err := repo.EnsureChiefOfStaff(ctx, stream, now(), ownerActor)
	fixturekit.Must(t, err)
	h := trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request-preview", Revision: 1, Project: repo.Project(), Workstream: stream, At: now(), Actor: ownerActor, Cause: "preview"}
	_, err = repo.EnqueueTurn(ctx, trace.TurnRequest{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "preview", Profile: coreadapter.Profile{Name: "default", Backend: "fake", Model: "fake"}, Prompt: "Show the page"})
	fixturekit.Must(t, err)
	_, err = repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "preview-claim", t.TempDir(), now())
	fixturekit.Must(t, err)
	return coreadapter.Scope{Project: string(repo.Project()), Workstream: string(stream), Thread: trace.ChiefOfStaff, Turn: "preview", Role: trace.ChiefOfStaff}
}

func previewView(t *testing.T) *isolation.FileView {
	t.Helper()
	source := t.TempDir()
	fixturekit.Must(t, os.WriteFile(filepath.Join(source, "status.json"), []byte("{}"), 0600))
	view, err := (isolation.Views{Directory: t.TempDir()}).Create(context.Background(), coreadapter.Workspace{Directory: source, Access: coreadapter.ReadOnly}, []string{"status.json"})
	fixturekit.Must(t, err)
	t.Cleanup(func() { fixturekit.Must(t, view.Release(context.Background())) })
	return view
}

func previewServiceFixture() *previewService {
	return &previewService{Call: []string{"web", "--message", "it's a string; $(never executed)"}, Port: 8080, Path: "/settings", Timeout: "1m"}
}

func previewInvoke(t *testing.T, tool coreadapter.Tool, raw json.RawMessage) previewResult {
	t.Helper()
	data, err := tool.Handle(context.Background(), raw)
	fixturekit.Must(t, err)
	var result previewResult
	fixturekit.Must(t, json.Unmarshal(data, &result))
	return result
}

type previewSource struct {
	f         *shedFixture
	stream    config.WorkstreamID
	repo      *trace.Repository
	cfg       *config.Config
	fake      fixturekit.PreviewCLIs
	candidate string
	head      string
	scope     coreadapter.Scope
	controls  *runtimeControls
}

type previewCase struct {
	name, raw, commit, source string
	inspect                   string
}

func newPreviewSource(t *testing.T) *previewSource {
	t.Helper()
	fake := fixturekit.InstallPreviewCLIs(t, attachmentPNG(t))
	t.Setenv("OSMIA_PREVIEW_TOKEN", "synthetic-preview-cloud-token")
	for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GITHUB_TOKEN", "SSH_AUTH_SOCK", "GH_TOKEN"} {
		t.Setenv(key, "forbidden-preview-credential")
	}
	f, stream, repo, _ := newFinalFixture(t, "preview-source")
	cfg := f.s.about(repo)
	cfg.Dagger = config.DaggerConfig{Engine: "tcp://preview-engine:1234", CloudComputeTokenEnv: "OSMIA_PREVIEW_TOKEN"}
	candidate := moveFeature(t, f, stream, map[string]string{"preview-source.txt": "unit candidate\n"})
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: reportDocument("resume"), Revision: 1, Project: repo.Project(), Workstream: stream, At: f.s.now(), Actor: serviceActor, Cause: "preview"}
	report, _ := json.Marshal(UnitReport{Candidate: candidate})
	fixturekit.Must(t, repo.RecordDocuments(context.Background(), []trace.Document{{Header: h, Path: "units/resume/report.json", Content: string(report)}}))
	head := moveFeature(t, f, stream, map[string]string{"preview-source.txt": "feature head\n"})
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	return &previewSource{f: f, stream: stream, repo: repo, cfg: cfg, fake: fake, candidate: candidate, head: head, scope: previewScope(t, repo, stream, f.s.now), controls: controls}
}

func (p *previewSource) cases() []previewCase {
	return []previewCase{
		{name: "feature head", raw: `{"call":["web","--message","it's a string; $(never executed)"],"port":8080,"path":"/settings?a=1&b=2","viewport":"phone","full_page":true}`, commit: p.head, source: "feature head\n", inspect: `{"path":"preview-source.txt"}`},
		{name: "unit candidate", raw: `{"unit":"resume","call":["web","--message","it's a string; $(never executed)"],"port":8080}`, commit: p.candidate, source: "unit candidate\n", inspect: `{"unit":"resume","path":"preview-source.txt"}`},
	}
}

func (p *previewSource) preview(t *testing.T, raw string) previewResult {
	t.Helper()
	return previewInvoke(t, p.controls.preview(p.cfg, p.repo, p.scope, previewView(t), p.f.s.now), json.RawMessage(raw))
}

func (p *previewSource) environment(t *testing.T) string {
	t.Helper()
	p.preview(t, p.cases()[0].raw)
	env, err := os.ReadFile(p.fake.Environment)
	fixturekit.Must(t, err)
	return string(env)
}

func TestChiefPreviewShowsTheSourceOfTheCommitItNames(t *testing.T) {
	p := newPreviewSource(t)
	for _, tc := range p.cases() {
		t.Run(tc.name, func(t *testing.T) {
			result := p.preview(t, tc.raw)
			if result.ImageID == "" || result.Commit != tc.commit {
				t.Fatalf("preview: %+v", result)
			}
			exported, err := os.ReadFile(p.fake.Source)
			fixturekit.Must(t, err)
			if string(exported) != tc.source {
				t.Fatalf("exported %q", exported)
			}
		})
	}
}

func TestChiefPreviewCommitIsTheOneInspectCodeReads(t *testing.T) {
	p := newPreviewSource(t)
	for _, tc := range p.cases() {
		t.Run(tc.name, func(t *testing.T) {
			result := p.preview(t, tc.raw)
			raw, err := inspectCode(p.cfg, p.repo, p.scope, p.f.s.now).Handle(context.Background(), json.RawMessage(tc.inspect))
			fixturekit.Must(t, err)
			var code struct{ Commit string }
			fixturekit.Must(t, json.Unmarshal(raw, &code))
			if code.Commit != result.Commit {
				t.Fatalf("preview %s differs from inspect %s", result.Commit, code.Commit)
			}
		})
	}
}

func TestChiefPreviewStoresItsImageInTheShownTrace(t *testing.T) {
	p := newPreviewSource(t)
	result := p.preview(t, p.cases()[0].raw)
	image, err := p.repo.StoredImage(p.stream, result.ImageID)
	fixturekit.Must(t, err)
	if image.Workstream != p.stream || !bytes.Equal(image.Data, attachmentPNG(t)) {
		t.Fatal("image not stored in shown trace")
	}
}

func TestChiefPreviewRunsTheCallItWasGivenAsOneArgumentEach(t *testing.T) {
	p := newPreviewSource(t)
	p.preview(t, p.cases()[0].raw)
	call, err := os.ReadFile(p.fake.Call)
	fixturekit.Must(t, err)
	args := strings.Split(strings.TrimSuffix(string(call), "\x00"), "\x00")
	if !slices.Equal(args, append(append([]string{"call"}, previewServiceFixture().Call...), "id")) {
		t.Fatalf("call argv %q", args)
	}
}

func TestChiefPreviewPinsTheServiceSettingsOfACall(t *testing.T) {
	p := newPreviewSource(t)
	for _, tc := range p.cases() {
		t.Run(tc.name, func(t *testing.T) {
			p.preview(t, tc.raw)
			doc, err := latestPreviewDocument(p.repo, p.stream, "preview-"+strings.TrimPrefix(trace.ToolCallID(p.scope, "preview", json.RawMessage(tc.raw)), "tool-"))
			fixturekit.Must(t, err)
			var pin previewOperation
			fixturekit.Must(t, json.Unmarshal([]byte(doc.Content), &pin))
			if !pin.AgentChosen || pin.Preview.Timeout != "5m" || pin.Preview.Port != 8080 {
				t.Fatalf("service pin: %+v", pin)
			}
		})
	}
}

func TestChiefPreviewCapturesTheRootPathWhenNoneIsGiven(t *testing.T) {
	p := newPreviewSource(t)
	raw := p.cases()[1].raw
	p.preview(t, raw)
	doc, err := latestPreviewDocument(p.repo, p.stream, "preview-"+strings.TrimPrefix(trace.ToolCallID(p.scope, "preview", json.RawMessage(raw)), "tool-"))
	fixturekit.Must(t, err)
	var pin previewOperation
	fixturekit.Must(t, json.Unmarshal([]byte(doc.Content), &pin))
	if pin.Capture.Path != "/" {
		t.Fatalf("omitted path captured %q", pin.Capture.Path)
	}
}

func TestChiefPreviewCapturesTheDesktopViewThroughAServiceBinding(t *testing.T) {
	p := newPreviewSource(t)
	p.preview(t, p.cases()[1].raw)
	capture, err := os.ReadFile(p.fake.Capture)
	fixturekit.Must(t, err)
	args := strings.Split(strings.TrimSuffix(string(capture), "\x00"), "\x00")
	if !slices.Contains(args, checks.ChromiumImage) || !slices.Contains(args, "with-service-binding") || slices.Contains(args, "up") {
		t.Fatalf("capture argv %q", args)
	}
	i := slices.Index(args, "--args")
	node, err := csv.NewReader(strings.NewReader(args[i+1])).Read()
	fixturekit.Must(t, err)
	if !strings.Contains(node[2], `"url":"http://preview:8080/"`) || !strings.Contains(node[2], `"width":1440`) || !strings.Contains(node[2], `"fullPage":false`) {
		t.Fatalf("desktop capture %s", node[2])
	}
}

func TestChiefPreviewEnvironmentCarriesTheEngineAndCloudToken(t *testing.T) {
	env := newPreviewSource(t).environment(t)
	for _, want := range []string{"PATH=", "DAGGER_ENGINE=tcp://preview-engine:1234", "_EXPERIMENTAL_DAGGER_RUNNER_HOST=tcp://preview-engine:1234", "DAGGER_CLOUD_TOKEN=synthetic-preview-cloud-token"} {
		if !strings.Contains(env, want) {
			t.Fatalf("missing environment %s", want)
		}
	}
}

func TestChiefPreviewEnvironmentInheritsNoCredentials(t *testing.T) {
	env := newPreviewSource(t).environment(t)
	for _, forbidden := range []string{"ANTHROPIC_API_KEY=", "OPENAI_API_KEY=", "GITHUB_TOKEN=", "SSH_AUTH_SOCK=", "GH_TOKEN="} {
		if strings.Contains(env, forbidden) {
			t.Fatalf("inherited %s", forbidden)
		}
	}
}

func (p *previewSource) restarted(t *testing.T) (view *isolation.FileView, result previewResult, runsBefore, runsAfter []byte) {
	t.Helper()
	for _, tc := range p.cases() {
		p.preview(t, tc.raw)
	}
	p.repo = fixturekit.ReopenTrace(t, p.f.s.cfg, p.repo)
	view = previewView(t)
	tool := p.controls.preview(p.cfg, p.repo, p.scope, view, p.f.s.now)
	before, err := os.ReadFile(p.fake.Runs)
	fixturekit.Must(t, err)
	fixturekit.Must(t, restorePreviewImages(context.Background(), p.cfg, p.controls, p.repo, p.scope, view, p.f.s.now))
	result = previewInvoke(t, tool, json.RawMessage(p.cases()[1].raw))
	after, err := os.ReadFile(p.fake.Runs)
	fixturekit.Must(t, err)
	return view, result, before, after
}

func TestChiefPreviewAfterARestartShowsTheStoredImageWithoutCapturingAgain(t *testing.T) {
	view, result, before, after := newPreviewSource(t).restarted(t)
	if !bytes.Equal(before, after) {
		t.Fatal("restart captured again")
	}
	copied, err := view.Read(result.Path)
	fixturekit.Must(t, err)
	if !bytes.Equal(copied, attachmentPNG(t)) {
		t.Fatal("missing view image")
	}
}

func TestChiefPreviewAfterARestartStoresEachImageAndCompletesEachToolOnce(t *testing.T) {
	p := newPreviewSource(t)
	p.restarted(t)
	images, err := trace.Read[trace.Image](p.repo, p.stream)
	fixturekit.Must(t, err)
	if len(images) != 2 {
		t.Fatalf("duplicate images %d", len(images))
	}
	docs, err := trace.Read[trace.Document](p.repo, p.stream)
	fixturekit.Must(t, err)
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
	fixturekit.Must(t, err)
	for _, doc := range docs {
		if strings.HasPrefix(doc.ID, "preview-") {
			t.Fatal("invalid request pinned a capture")
		}
	}
	chief, err := repo.Thread(stream, trace.ChiefOfStaff)
	fixturekit.Must(t, err)
	q := chief.Turns[len(chief.Turns)-1]
	h := q.Request.Header
	h.Schema, h.ID, h.At = "osmia.trace.turn-response", trace.EventID(q.Request.ID, "response"), f.s.now()
	fixturekit.Must(t, repo.CaptureTurn(context.Background(), q.Claim.Token, trace.TurnResponse{Header: h, AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: scope.Turn, RequestID: q.Request.ID, RequestRevision: 1, Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "preview-session"}, FinalResponse: "Preview is unavailable", StartedAt: q.Claim.At, SessionDirectory: q.Claim.SessionDirectory}}))
	fixturekit.Must(t, repo.CompleteTurn(context.Background(), stream, trace.ChiefOfStaff, scope.Turn, q.Claim.Token, f.s.now()))
}

func seedPreviewOperation(t *testing.T, repo *trace.Repository, scope coreadapter.Scope, raw json.RawMessage, stream config.WorkstreamID, commit string, settings *previewService) previewOperation {
	t.Helper()
	var in previewInput
	fixturekit.Must(t, json.Unmarshal(raw, &in))
	if in.Path == "" {
		in.Path = settings.Path
	}
	if in.Viewport == "" {
		in.Viewport = "desktop"
	}
	op := previewOperation{Scope: scope, Call: raw, Workstream: stream, Commit: commit, AgentChosen: len(in.Call) > 0, Preview: *settings, Capture: checks.PreviewCapture{Call: settings.Call, Port: settings.Port, Path: in.Path, Viewport: in.Viewport, FullPage: in.FullPage}}
	id := "preview-" + strings.TrimPrefix(trace.ToolCallID(scope, "preview", raw), "tool-")
	doc := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: 1, Project: repo.Project(), Workstream: config.WorkstreamID(scope.Workstream), Actor: serviceActor, Cause: scope.Turn}, Path: "tools/" + id + ".json"}
	_, err := repo.BeginTool(context.Background(), scope, coreadapter.Tool{Name: "preview", Effect: coreadapter.ToolMemory}, raw, func() time.Time { return fixturekit.DemoStart })
	fixturekit.Must(t, err)
	fixturekit.Must(t, writePreviewOperation(context.Background(), repo, &doc, op, func() time.Time { return fixturekit.DemoStart }))
	return op
}

type sharedPreview struct {
	fake    fixturekit.PreviewCLIs
	chief   *trace.Repository
	target  *trace.Repository
	host    config.WorkstreamID
	member  config.WorkstreamID
	key     string
	image   trace.Image
	op      previewOperation
	results []sharedPreviewResult
}

type sharedPreviewResult struct {
	result previewResult
	copied []byte
	copies int
}

func sharedPreviewVariants(t *testing.T, check func(t *testing.T, p *sharedPreview)) {
	for _, cross := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-project", true: "cross-project"}[cross], func(t *testing.T) { check(t, resumeSharedPreview(t, cross)) })
	}
}

func resumeSharedPreview(t *testing.T, cross bool) *sharedPreview {
	t.Helper()
	fake := fixturekit.InstallPreviewCLIs(t, attachmentPNG(t))
	s, chief, group := workgroupFeedFixture(t, cross, 2)
	now := func() time.Time { return fixturekit.DemoStart }
	host := group.Members[0]
	scope := previewScope(t, chief, host, now)
	member := fixturekit.Quiet
	target, _, refusal, err := chiefMember(chief, scope, member)
	fixturekit.Must(t, err)
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
	_, err = chief.BeginTool(context.Background(), scope, tool, raw, now)
	fixturekit.Must(t, err)
	image, err := target.RecordImage(context.Background(), trace.Header{Project: target.Project(), Workstream: member, At: fixturekit.DemoStart, Actor: serviceActor, Cause: scope.Turn}, key, "image/png", attachmentPNG(t), scope.Turn)
	fixturekit.Must(t, err)
	fixturekit.Must(t, chief.Close())
	if cross {
		chief, err = trace.OpenWorkgroupTrace(s.cfg.Root, group.ID, []trace.CrossRepoMember{{Workstream: fixturekit.Stream, Project: s.projects[0].repository}, {Workstream: member, Project: s.projects[1].repository}})
	} else {
		chief, err = trace.Open(s.cfg.Root, s.cfg.Project)
		s.projects[0].repository = chief
	}
	fixturekit.Must(t, err)
	t.Cleanup(func() { chief.Close() })
	fixturekit.Must(t, chief.AbandonTurn(context.Background(), host, trace.ChiefOfStaff, scope.Turn, fixturekit.DemoStart))
	target, _, _, err = chiefMember(chief, scope, member)
	fixturekit.Must(t, err)
	resumed := scope
	resumed.Turn = "preview-continuation"

	p := &sharedPreview{fake: fake, chief: chief, target: target, host: host, member: member, key: key, image: image, op: op}
	for range 3 {
		view = previewView(t)
		tool = controls.preview(s.cfg, chief, scope, view, now)
		fixturekit.Must(t, restorePreviewImages(context.Background(), s.cfg, controls, chief, resumed, view, now))
		result := previewInvoke(t, tool, raw)
		copied, err := view.Read(result.Path)
		fixturekit.Must(t, err)
		entries, err := os.ReadDir(filepath.Join(view.Workspace().Directory, "images"))
		fixturekit.Must(t, err)
		p.results = append(p.results, sharedPreviewResult{result: result, copied: copied, copies: len(entries)})
	}
	return p
}

func TestChiefPreviewOfASharedMemberMustNameTheWorkstream(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-project", true: "cross-project"}[cross], func(t *testing.T) {
			fixturekit.InstallPreviewCLIs(t, attachmentPNG(t))
			s, chief, group := workgroupFeedFixture(t, cross, 2)
			now := func() time.Time { return fixturekit.DemoStart }
			scope := previewScope(t, chief, group.Members[0], now)
			controls := &runtimeControls{}
			controls.service.Store(s)
			tool := controls.preview(s.cfg, chief, scope, previewView(t), now)
			if result := previewInvoke(t, tool, json.RawMessage(`{"call":["web"],"port":8080}`)); !strings.Contains(result.Limitation, "qualify workstream") {
				t.Fatalf("unqualified shared preview: %+v", result)
			}
		})
	}
}

func TestChiefPreviewResumedAfterAnAbandonedTurnReturnsTheRecordedImage(t *testing.T) {
	sharedPreviewVariants(t, func(t *testing.T, p *sharedPreview) {
		for _, r := range p.results {
			if r.result.ImageID != p.image.ID {
				t.Fatalf("changed image %+v", r.result)
			}
			if !bytes.Equal(r.copied, p.image.Data) {
				t.Fatal("view differs from receipt")
			}
		}
	})
}

func TestChiefPreviewResumedRepeatedlyCopiesTheImageIntoTheViewOnce(t *testing.T) {
	sharedPreviewVariants(t, func(t *testing.T, p *sharedPreview) {
		for _, r := range p.results {
			if r.copies != 1 {
				t.Fatalf("copies %d", r.copies)
			}
		}
	})
}

func TestChiefPreviewResumedKeepsOneImageAndOneReference(t *testing.T) {
	sharedPreviewVariants(t, func(t *testing.T, p *sharedPreview) {
		images, err := trace.Read[trace.Image](p.target, p.member)
		fixturekit.Must(t, err)
		refs, err := trace.Read[trace.ImageReference](p.chief, p.host)
		fixturekit.Must(t, err)
		if len(images) != 1 || len(refs) != 1 || refs[0].ImageID != p.image.ID || refs[0].Member != p.member {
			t.Fatalf("images %d references %+v", len(images), refs)
		}
	})
}

func TestChiefPreviewResumedCompletesTheToolOnceWithTheRecordedImage(t *testing.T) {
	sharedPreviewVariants(t, func(t *testing.T, p *sharedPreview) {
		docs, err := trace.Read[trace.Document](p.chief, p.host)
		fixturekit.Must(t, err)
		completions := 0
		for _, doc := range docs {
			if doc.ID == p.key && doc.Revision == 2 {
				completions++
			}
		}
		if completions != 1 {
			t.Fatalf("tool completions %d", completions)
		}
		resultDoc, err := latestPreviewDocument(p.chief, p.host, "preview-"+strings.TrimPrefix(p.key, "tool-"))
		fixturekit.Must(t, err)
		op := p.op
		fixturekit.Must(t, json.Unmarshal([]byte(resultDoc.Content), &op))
		if op.Result == nil || op.Result.ImageID != p.image.ID {
			t.Fatalf("unfinished operation %+v", op)
		}
	})
}

func TestChiefPreviewResumedNeverCapturesAgainWhenTheReceiptIsRecorded(t *testing.T) {
	sharedPreviewVariants(t, func(t *testing.T, p *sharedPreview) {
		if _, err := os.Stat(p.fake.Runs); !os.IsNotExist(err) {
			t.Fatal("recorded receipt was captured again")
		}
	})
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
	repo = fixturekit.ReopenTrace(t, f.s.cfg, repo)
	fixturekit.Must(t, repo.AbandonTurn(context.Background(), stream, trace.ChiefOfStaff, scope.Turn, f.s.now()))
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	view := previewView(t)
	resumed := scope
	resumed.Turn = "preview-continuation"
	fixturekit.Must(t, restorePreviewImages(context.Background(), cfg, controls, repo, resumed, view, f.s.now))
	tool := controls.preview(cfg, repo, scope, view, f.s.now)
	result := previewInvoke(t, tool, raw)
	receipt, err := view.Read("previews/results.json")
	fixturekit.Must(t, err)
	if !strings.Contains(string(receipt), result.ImageID) {
		t.Fatal("recovered receipt missing from chief view")
	}
	if result.Commit != commit || result.ImageID == "" {
		t.Fatalf("%+v", result)
	}
	source, err := os.ReadFile(fake.Source)
	fixturekit.Must(t, err)
	if string(source) != "pinned before interruption\n" {
		t.Fatalf("source %q", source)
	}
	capture, err := os.ReadFile(fake.Capture)
	fixturekit.Must(t, err)
	args := strings.Split(strings.TrimSuffix(string(capture), "\x00"), "\x00")
	node, err := csv.NewReader(strings.NewReader(args[slices.Index(args, "--args")+1])).Read()
	fixturekit.Must(t, err)
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
	cfg.Dagger = config.DaggerConfig{Engine: "cloud", CloudComputeTokenEnv: "OSMIA_PREVIEW_TOKEN"}
	moveFeature(t, f, stream, map[string]string{"preview-source.txt": "failure\n"})
	scope := previewScope(t, repo, stream, f.s.now)
	raw := json.RawMessage(`{"call":["broken"],"port":8080}`)
	fixturekit.Must(t, os.WriteFile(fake.Failure, []byte(strings.Repeat("output ", 10000)+secret+"\n"), 0600))
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	tool := controls.preview(cfg, repo, scope, previewView(t), f.s.now)
	result := previewInvoke(t, tool, raw)
	if result.ImageID != "" || len(result.Limitation) > 16<<10 || !strings.Contains(result.Limitation, "[redacted]") || strings.Contains(result.Limitation, secret) {
		t.Fatalf("diagnostic length %d", len(result.Limitation))
	}
	docs, err := trace.Read[trace.Document](repo, stream)
	fixturekit.Must(t, err)
	for _, doc := range docs {
		if strings.Contains(doc.Content, secret) {
			t.Fatalf("secret persisted in %s", doc.ID)
		}
	}
	before, err := os.ReadFile(fake.Runs)
	fixturekit.Must(t, err)
	again := previewInvoke(t, tool, raw)
	after, err := os.ReadFile(fake.Runs)
	fixturekit.Must(t, err)
	if again != result || !bytes.Equal(before, after) {
		t.Fatal("failed operation did not retain diagnostic")
	}
	fixturekit.Must(t, os.Remove(fake.Failure))
	corrected := previewInvoke(t, tool, json.RawMessage(`{"call":["web"],"port":8080}`))
	if corrected.ImageID == "" {
		t.Fatalf("corrected request: %+v", corrected)
	}
	runs, err := os.ReadFile(fake.Runs)
	fixturekit.Must(t, err)
	if strings.Count(string(runs), "capture") != 2 {
		t.Fatal("different call did not run separately")
	}
}

func TestChiefPreviewSlotClockAndMaintenance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Service{checkSlots: &checkSlotPool{limit: 1}}
		run := CheckSlotRun{Project: fixturekit.Project, Workstream: fixturekit.Stream, Preview: true}
		release, err := s.checkSlots.acquire(t.Context(), CheckSlotRun{Unit: "holder"})
		fixturekit.Must(t, err)
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
		fixturekit.Must(t, <-done)
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
		fixturekit.Must(t, <-gateDone)
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
	cfg.Dagger = config.DaggerConfig{Engine: "cloud", CloudComputeTokenEnv: "OSMIA_PREVIEW_TOKEN"}
	secret := "synthetic-preview-timeout-token"
	t.Setenv("OSMIA_PREVIEW_TOKEN", secret)
	commit := moveFeature(t, f, stream, map[string]string{"preview-source.txt": "blocked capture\n"})
	scope := previewScope(t, repo, stream, f.s.now)
	settings := previewServiceFixture()
	settings.Timeout = "1s"
	raw := json.RawMessage(`{"call":["web","--message","it's a string; $(never executed)"],"port":8080}`)
	seedPreviewOperation(t, repo, scope, raw, stream, commit, settings)
	fixturekit.Must(t, os.WriteFile(fake.Block, []byte("browser blocked: "+secret+"\n"), 0600))
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
		fixturekit.Must(t, json.Unmarshal(out, &cancelled))
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
	fixturekit.Must(t, err)
	for _, doc := range docs {
		if strings.HasPrefix(doc.ID, "preview-") {
			t.Fatal("unsafe call pinned for recovery")
		}
	}
}

type symlinkPreview struct {
	fake     fixturekit.PreviewCLIs
	f        *shedFixture
	stream   config.WorkstreamID
	repo     *trace.Repository
	cfg      *config.Config
	scope    coreadapter.Scope
	controls *runtimeControls
	commit   string
}

func newSymlinkPreview(t *testing.T, backend string) *symlinkPreview {
	t.Helper()
	fake := fixturekit.InstallPreviewCLIs(t, attachmentPNG(t))
	f := newDebateFixtureWith(t, 1, 1, "", func(opts *Options) { intakefixture.OnWorkspaces(t, opts.Config.Root, backend) })
	stream, repo := seedBuild(t, f, "preview-symlinks", fixturekit.ValidPlan, backend, f.upstream(t))
	cfg := f.s.about(repo)
	moveFeature(t, f, stream, map[string]string{"preview-source.txt": "symlinks\n"})
	g, err := featureWorkspaces(cfg, repo).Of(stream)
	fixturekit.Must(t, err)
	acquired, err := g.Acquire(context.Background(), vcs.Request{Name: string(stream), Branch: featureBranch(stream)})
	fixturekit.Must(t, err)
	w := acquired.(workspace.Worktree)
	fixturekit.Must(t, os.Symlink(t.TempDir(), filepath.Join(w.Path, "outside")))
	fixturekit.Must(t, os.Symlink("preview-source.txt", filepath.Join(w.Path, "inside")))
	fixturekit.Must(t, os.Symlink("outside", filepath.Join(w.Path, "chain")))
	fixturekit.Must(t, os.Symlink("inside", filepath.Join(w.Path, "inside-chain")))
	for _, name := range []string{"--link", " padded ", "quo'ted", "comma,link"} {
		fixturekit.Must(t, os.Symlink(t.TempDir(), filepath.Join(w.Path, name)))
	}
	tip, _, err := g.Branch(context.Background(), featureBranch(stream))
	fixturekit.Must(t, err)
	commit, err := g.Snapshot(context.Background(), w, tip)
	fixturekit.Must(t, err)
	fixturekit.DemoGit(t, cfg.Project.Clone, "-C", cfg.Project.Clone, "config", "core.symlinks", "false")
	scope := previewScope(t, repo, stream, f.s.now)
	controls := &runtimeControls{}
	controls.service.Store(f.s)
	return &symlinkPreview{fake: fake, f: f, stream: stream, repo: repo, cfg: cfg, scope: scope, controls: controls, commit: commit}
}

func (p *symlinkPreview) tool(t *testing.T) coreadapter.Tool {
	return p.controls.preview(p.cfg, p.repo, p.scope, previewView(t), p.f.s.now)
}

func eachWorkspaceBackend(t *testing.T, check func(t *testing.T, backend string)) {
	for _, backend := range []string{config.WorkspacesGit, config.WorkspacesJujutsu} {
		t.Run(backend, func(t *testing.T) { check(t, backend) })
	}
}

func TestChiefPreviewRefusesCallsNamingASymlinkThatLeavesTheWorkspace(t *testing.T) {
	eachWorkspaceBackend(t, func(t *testing.T, backend string) {
		p := newSymlinkPreview(t, backend)
		tool := p.tool(t)
		for _, value := range []string{"outside", `"outside"`, "a, outside", "--source=outside", "outside/missing", "chain", "chain/missing", "outside#main", "--source=outside#main", "a, outside#main", "--source=a, outside", "--source=\"outside\"", "--source=--link", " padded ", "quo'ted", "comma,link", `"comma,link"`, "--source=comma,link"} {
			raw, _ := json.Marshal(map[string]any{"call": []string{"web", value}, "port": 8080})
			if result := previewInvoke(t, tool, raw); result.Limitation == "" {
				t.Fatalf("symlink accepted: %s", raw)
			}
			if _, err := os.Stat(p.fake.Call); !os.IsNotExist(err) {
				t.Fatal("symlink started Dagger")
			}
		}
	})
}

func TestChiefPreviewAcceptsASymlinkThatStaysInsideTheWorkspace(t *testing.T) {
	eachWorkspaceBackend(t, func(t *testing.T, backend string) {
		result := previewInvoke(t, newSymlinkPreview(t, backend).tool(t), json.RawMessage(`{"call":["web","--source","inside-chain"],"port":8080}`))
		if result.ImageID == "" {
			t.Fatalf("internal symlink refused: %+v", result)
		}
	})
}

func TestChiefPreviewResumedCaptureRunsTheCallItPinned(t *testing.T) {
	eachWorkspaceBackend(t, func(t *testing.T, backend string) {
		p := newSymlinkPreview(t, backend)
		settings := previewServiceFixture()
		settings.Call = []string{"web", "--source", "/etc/passwd", "--other", "outside"}
		raw := json.RawMessage(`{"path":"/pinned"}`)
		seedPreviewOperation(t, p.repo, p.scope, raw, p.stream, p.commit, settings)
		repo := fixturekit.ReopenTrace(t, p.f.s.cfg, p.repo)
		fixturekit.Must(t, repo.AbandonTurn(context.Background(), p.stream, trace.ChiefOfStaff, p.scope.Turn, p.f.s.now()))
		resumed := p.scope
		resumed.Turn = "preview-continuation"
		view := previewView(t)
		fixturekit.Must(t, restorePreviewImages(context.Background(), p.cfg, p.controls, repo, resumed, view, p.f.s.now))
		result := previewInvoke(t, p.controls.preview(p.cfg, repo, p.scope, view, p.f.s.now), raw)
		if result.ImageID == "" {
			t.Fatalf("settings pin refused: %+v", result)
		}
		call, err := os.ReadFile(p.fake.Call)
		fixturekit.Must(t, err)
		if !slices.Equal(strings.Split(strings.TrimSuffix(string(call), "\x00"), "\x00"), append(append([]string{"call"}, settings.Call...), "id")) {
			t.Fatalf("settings pin changed: %s", call)
		}
	})
}

func TestChiefPreviewRefusesANewRequestThatRepeatsAPinnedCallToSkipTheChecks(t *testing.T) {
	eachWorkspaceBackend(t, func(t *testing.T, backend string) {
		p := newSymlinkPreview(t, backend)
		settings := previewServiceFixture()
		settings.Call = []string{"web", "--source", "/etc/passwd", "--other", "outside"}
		raw, _ := json.Marshal(map[string]any{"call": settings.Call, "port": settings.Port})
		if result := previewInvoke(t, p.tool(t), raw); result.Limitation == "" {
			t.Fatal("new request used settings bypass")
		}
		if _, err := os.Stat(p.fake.Call); !os.IsNotExist(err) {
			t.Fatal("refused request ran Dagger")
		}
	})
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
	fixturekit.Must(t, err)
	wrongType := base64.StdEncoding.EncodeToString(bytes.ReplaceAll(encoded, []byte("Service"), []byte("Counter")))
	for i, value := range []string{wrongType, "", "Service@fake", "/etc/passwd", "/etc/passwd/", "env://DAGGER_CLOUD_TOKEN", "file:///etc/passwd", "cmd://command", "not an ID", fixturekit.PreviewServiceID, fixturekit.PreviewRecipeServiceID} {
		fixturekit.Must(t, os.WriteFile(fake.ServiceID, []byte(value+"\n"), 0600))
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
			fixturekit.Must(t, err)
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
	fixturekit.Must(t, json.Unmarshal(tool.InputSchema, &schema))
	for _, field := range []string{"call", "port", "timeout"} {
		if len(schema.Properties[field]) == 0 {
			t.Fatalf("missing input %s", field)
		}
	}
	if !slices.Equal(schema.Required, []string{"call", "port"}) {
		t.Fatalf("required %q", schema.Required)
	}
}

// The service seeds the view it hands a chief-of-staff turn with the images and
// receipts of the thread's earlier previews, so a client that receives the view
// as its starting files already holds them.
func TestTheServiceSeedsAChiefsViewWithItsEarlierPreviews(t *testing.T) {
	p := newPreviewSource(t)
	var first []previewResult
	for _, tc := range p.cases() {
		first = append(first, p.preview(t, tc.raw))
	}
	options := p.f.s.options
	options.controls = p.controls
	bound, err := Enforce(options, Enforcement{}).Threads(p.repo, p.f.s.cfg)
	fixturekit.Must(t, err)
	turns := bound.(thread.Dispatcher).Runner.Turns.(*questions.Turns).Turns.(*verdictTurns).Turns.(*reportingTurns).Turns.(*retriedTurns).Turns.(*isolation.Turns)
	if turns.Seed == nil {
		t.Fatal("the service seeds no view")
	}
	view := previewView(t)
	fixturekit.Must(t, turns.Seed(context.Background(), p.scope, view))
	receipt, err := view.Read("previews/results.json")
	fixturekit.Must(t, err)
	for _, result := range first {
		if !strings.Contains(string(receipt), result.ImageID) {
			t.Fatalf("the receipt of image %s is missing from the seeded view: %s", result.ImageID, receipt)
		}
		if image, err := view.Read(result.Path); err != nil || !bytes.Equal(image, attachmentPNG(t)) {
			t.Fatalf("image %s is missing from the seeded view: %v", result.Path, err)
		}
	}
	other := previewView(t)
	other2 := p.scope
	other2.Role = "mason"
	fixturekit.Must(t, turns.Seed(context.Background(), other2, other))
	if _, err := other.Read("previews/results.json"); err == nil {
		t.Fatal("a turn that is not the chief's was seeded with previews")
	}
}
