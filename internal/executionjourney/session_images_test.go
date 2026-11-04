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

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestSessionShowImageUsesActiveViewAndDestroyDeletesImage(t *testing.T) {
	t.Parallel()
	s, c, project, stream, engine, _ := newSessionChatFixture(t)
	var b bytes.Buffer
	must(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	data := b.Bytes()
	sub, ok := service.InspectForTest(s.core()).Hub().Subscribe()
	if !ok {
		t.Fatal("subscribe")
	}
	defer service.InspectForTest(s.core()).Hub().Unsubscribe(sub)
	sub.Take()
	outside := filepath.Join(t.TempDir(), "outside.png")
	must(t, os.WriteFile(outside, data, 0600))
	var attachment api.FeedImage
	engine.Turn = func(ctx context.Context, req agent.Request) (*agent.Result, error) {
		active := req.Workspace.Directory()
		must(t, os.WriteFile(filepath.Join(active, "fresh.png"), data, 0600))
		persistent := filepath.Join(s.current().Root.String(), sessionsDirectory, string(project), string(stream), "fresh.png")
		if _, err := os.Stat(persistent); !os.IsNotExist(err) {
			t.Error("workspace captured before tool call")
		}
		must(t, os.Symlink(outside, filepath.Join(active, "escape.png")))
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
		must(t, err)
		// Tool JSON is a text item, carrying attachment metadata only.
		must(t, json.Unmarshal([]byte(raw), &attachment))
		var feed api.FeedResponse
		must(t, c.Do(ctx, http.MethodGet, api.Prefix+"/feed/"+string(stream), nil, &feed))
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
		// Remove the escape before capture, which intentionally excludes symlinks.
		must(t, os.Remove(filepath.Join(active, "escape.png")))
		return &agent.Result{ClaudeID: "session-images", ResultText: "Attached the screenshot."}, nil
	}
	_, err := c.SessionMessage(context.Background(), stream, "show the screenshot")
	must(t, err)
	awaitSession(t, c, stream, func(v SessionView) bool { return v.State == trace.SessionIdle })
	if attachment.ID == "" {
		t.Fatal("image tool was not called")
	}
	repo := s.sole().repository
	stored, err := repo.StoredImage(stream, attachment.ID)
	must(t, err)
	if !bytes.Equal(stored.Data, data) {
		t.Fatal("stored bytes differ from active view")
	}
	announced := false
	for _, e := range sub.Take() {
		if e.Kind == api.EventImage && e.Workstream == stream {
			announced = true
		}
	}
	if !announced {
		t.Fatal("image event missing")
	}
	// Destroy is admitted once the completed turn's operation is acknowledged.
	soon(t, "destroy stayed busy", func() bool {
		var out api.SessionDestroyResponse
		err = c.Do(context.Background(), http.MethodPost, api.Prefix+"/sessions/"+string(stream)+"/destroy", nil, &out)
		return err == nil
	})
	if _, err := repo.StoredImage(stream, attachment.ID); err == nil {
		t.Fatal("destroy retained bytes")
	}
	var failure any
	err = c.Do(context.Background(), http.MethodGet, attachment.URL, nil, &failure)
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("destroyed endpoint: %v", err)
	}
}
