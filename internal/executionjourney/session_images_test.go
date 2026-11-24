package executionjourney

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"github.com/kpenfound/osmia/internal/servicetest/journey"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type shownImage struct {
	c          *journey.Client
	repo       *trace.Repository
	stream     config.WorkstreamID
	attachment api.FeedImage
	data       []byte
	announced  func() bool
}

func showSessionImage(t *testing.T) shownImage {
	t.Helper()
	s, c, project, stream, engine, _ := newSessionChatFixture(t)
	var b bytes.Buffer
	fixturekit.Must(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	data := b.Bytes()
	sub, ok := service.InspectForTest(s.Core()).Hub().Subscribe()
	if !ok {
		t.Fatal("subscribe")
	}
	t.Cleanup(func() { service.InspectForTest(s.Core()).Hub().Unsubscribe(sub) })
	sub.Take()
	outside := filepath.Join(t.TempDir(), "outside.png")
	fixturekit.Must(t, os.WriteFile(outside, data, 0600))
	var attachment api.FeedImage
	engine.Turn = func(ctx context.Context, req agent.Request) (*agent.Result, error) {
		active := req.Workspace.Directory()
		fixturekit.Must(t, os.WriteFile(filepath.Join(active, "fresh.png"), data, 0600))
		persistent := filepath.Join(s.Current().Root.String(), service.SessionsDirectory, string(project), string(stream), "fresh.png")
		if _, err := os.Stat(persistent); !os.IsNotExist(err) {
			t.Error("workspace captured before tool call")
		}
		fixturekit.Must(t, os.Symlink(outside, filepath.Join(active, "escape.png")))
		engine.Sessions.Mu.Lock()
		tools := engine.Sessions.ByKey[req.Env[coreadapter.TokenEnvironment]]
		engine.Sessions.Mu.Unlock()
		if tools == nil {
			t.Error("show_image has no MCP host")
			return &agent.Result{ClaudeID: "session"}, nil
		}
		for _, path := range []string{outside, "escape.png"} {
			if _, err := fixturekit.CallTool(ctx, tools, "show_image", map[string]any{"path": path}); err == nil {
				t.Error("outside image accepted")
			}
		}
		raw, err := fixturekit.CallTool(ctx, tools, "show_image", map[string]any{"path": "fresh.png"})
		fixturekit.Must(t, err)

		fixturekit.Must(t, json.Unmarshal([]byte(raw), &attachment))
		var feed api.FeedResponse
		fixturekit.Must(t, c.Do(ctx, http.MethodGet, api.Prefix+"/feed/"+string(stream), nil, &feed))
		count := 0
		for _, e := range feed.Entries {
			if e.Kind == "image" {
				count++
				if e.Image.ID != attachment.ID {
					t.Error("feed image identity")
				}
			}
		}
		if count != 1 {
			t.Errorf("active feed images %d", count)
		}

		fixturekit.Must(t, os.Remove(filepath.Join(active, "escape.png")))
		return &agent.Result{ClaudeID: "session-images", ResultText: "Attached the screenshot."}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "show the screenshot")
	fixturekit.Must(t, err)
	fixturekit.AwaitSession(t, c, stream, func(v api.SessionView) bool { return v.State == trace.SessionIdle })
	if attachment.ID == "" {
		t.Fatal("image tool was not called")
	}
	return shownImage{c: c, repo: s.Sole().Repository, stream: stream, attachment: attachment, data: data, announced: func() bool {
		for _, e := range sub.Take() {
			if e.Kind == api.EventImage && e.Workstream == stream {
				return true
			}
		}
		return false
	}}
}

func TestSessionShowImageStoresTheActiveViewsBytesAndAnnouncesIt(t *testing.T) {
	t.Parallel()
	shown := showSessionImage(t)
	stored, err := shown.repo.StoredImage(shown.stream, shown.attachment.ID)
	fixturekit.Must(t, err)
	if !bytes.Equal(stored.Data, shown.data) {
		t.Fatal("stored bytes differ from active view")
	}
	if !shown.announced() {
		t.Fatal("image event missing")
	}
}

func TestDestroyingASessionDeletesItsImages(t *testing.T) {
	t.Parallel()
	shown := showSessionImage(t)
	var err error
	intakefixture.Soon(t, "destroy stayed busy", func() bool {
		var out api.SessionDestroyResponse
		err = shown.c.Do(context.Background(), http.MethodPost, api.Prefix+"/sessions/"+string(shown.stream)+"/destroy", nil, &out)
		return err == nil
	})
	if _, err := shown.repo.StoredImage(shown.stream, shown.attachment.ID); err == nil {
		t.Fatal("destroy retained bytes")
	}
	var failure any
	err = shown.c.Do(context.Background(), http.MethodGet, shown.attachment.URL, nil, &failure)
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("destroyed endpoint: %v", err)
	}
}
