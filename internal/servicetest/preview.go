package servicetest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/seal"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// PreviewJourney binds the production chief tool host to offline capture commands.
// The member has a sealed build and a feature head beyond its base; its sibling
// keeps the shared conversation alive after the member is archived.
type PreviewJourney struct {
	*Project
	Member, Sibling config.WorkstreamID
	Group           trace.Workgroup
	PNG             []byte
	CLIs            fixturekit.PreviewCLIs
	Commit          string
}

const previewPrompt = "Show me the new settings page."
const previewSource = "built settings page\n"

func NewPreviewJourney(t *testing.T) *PreviewJourney {
	t.Helper()
	f := &PreviewJourney{}
	var data bytes.Buffer
	fixturekit.Must(t, png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 1200, 800))))
	f.PNG = data.Bytes()
	f.CLIs = fixturekit.InstallPreviewCLIs(t, f.PNG)
	engine := &fixturekit.SessionEngine{Sessions: &fixturekit.DemoSessions{ByKey: map[string]*mcp.ClientSession{}}}
	engine.Turn = func(ctx context.Context, req agent.Request) (*agent.Result, error) {
		if !strings.HasSuffix(req.Prompt, previewPrompt) {
			return &agent.Result{ClaudeID: "preview-chief", ResultText: "Ready."}, nil
		}
		engine.Sessions.Mu.Lock()
		tools := engine.Sessions.ByKey[req.Env[coreadapter.TokenEnvironment]]
		engine.Sessions.Mu.Unlock()
		if tools == nil {
			return nil, fmt.Errorf("chief has no scoped tool host")
		}
		raw, err := fixturekit.CallTool(ctx, tools, "preview", map[string]any{"workstream": f.Member})
		if err != nil {
			return nil, err
		}
		var result struct {
			ImageID                              string `json:"image_id"`
			Workstream, Commit, Path, Limitation string
		}
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return nil, err
		}
		if result.ImageID == "" || result.Limitation != "" || result.Workstream != string(f.Member) || result.Commit != f.Commit {
			return nil, fmt.Errorf("preview result: %s", raw)
		}
		if req.Grants == nil || req.Grants.VCS {
			return nil, fmt.Errorf("chief has no confined read-only grants")
		}
		copies := 0
		for _, mount := range req.Grants.Mounts {
			if mount.Access != agent.ReadOnly {
				continue
			}
			copied, err := os.ReadFile(filepath.Join(mount.Path, result.Path))
			if err == nil && bytes.Equal(copied, f.PNG) {
				copies++
			}
		}
		if copies != 1 {
			return nil, fmt.Errorf("chief read-only image copies: %d", copies)
		}
		return &agent.Result{ClaudeID: "preview-chief", ResultText: "Settings preview: " + result.ImageID}, nil
	}
	f.Project = NewProject(t, func(opts *service.Options, _ *service.TestHooks) {
		WithWeb(t, *opts, "127.0.0.1:0")
		*opts = service.EnforceThreadsForTest(*opts, service.Enforcement{Engine: engine, Hosts: &coreadapter.MCPHost{Transport: &fixturekit.DemoTransport{Sessions: engine.Sessions}}})
	})
	Mutation(t, f.C, http.MethodPut, "pause", api.PauseRequest{Target: runtime.Target{Scope: "factory"}, Mode: "soft", Source: runtime.PauseOwner})
	base := strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", f.Clone, "rev-parse", "HEAD"))
	var repo *trace.Repository
	f.Sibling, repo = f.SeedBuild(t, "preview-sibling", fixturekit.ValidPlan, config.WorkspacesGit, base)
	f.Member = f.recordBuild(t, repo, "preview-member", fixturekit.ValidPlan, config.WorkspacesGit, base)
	sealed, _, found, err := seal.Latest(repo, f.Member)
	fixturekit.Must(t, err)
	if !found {
		t.Fatal("member has no sealed workspace")
	}
	fixturekit.Must(t, os.WriteFile(filepath.Join(sealed.Workspace, "preview-source.txt"), []byte(previewSource), 0600))
	fixturekit.DemoGit(t, f.Clone, "-C", sealed.Workspace, "add", "preview-source.txt")
	fixturekit.DemoGit(t, f.Clone, "-C", sealed.Workspace, "-c", "user.name=Factory", "-c", "user.email=factory@example.invalid", "commit", "-qm", "Build settings page")
	f.Commit = strings.TrimSpace(fixturekit.DemoGit(t, f.Clone, "-C", sealed.Workspace, "rev-parse", "HEAD"))
	f.Group, err = trace.CreateWorkgroup(context.Background(), repo, []config.WorkstreamID{f.Sibling, f.Member}, time.Now().UTC(), fixturekit.OwnerActor, "preview-fixture")
	fixturekit.Must(t, err)
	fixturekit.Must(t, repo.Close())
	f.Start(t)
	return f
}

// Capture configures previews through the public API, then sends an owner message
// whose chief turn calls the production preview tool through MCP.
func (f *PreviewJourney) Capture(t *testing.T) api.FeedImage {
	t.Helper()
	ctx := context.Background()
	var saved api.ProjectPreviewResponse
	fixturekit.Must(t, f.C.Do(ctx, http.MethodPut, api.Prefix+"/projects/preview/"+string(f.ID), map[string]any{"call": []string{"web"}, "port": 8080}, &saved))
	if saved.Preview == nil || saved.Preview.Path != "/" || saved.Preview.Timeout != "5m" {
		t.Fatalf("preview defaults: %+v", saved)
	}
	wait, release := ServiceChanges(t, f.S)
	defer release()
	message, err := f.C.Send(ctx, f.Sibling, previewPrompt)
	fixturekit.Must(t, err)
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		conversation, err := f.C.Conversation(ctx, f.Sibling)
		fixturekit.Must(t, err)
		completed := false
		for _, entry := range conversation.Entries {
			if entry.Turn != message.Turn {
				continue
			}
			if entry.State == api.TurnFailed {
				thread, err := f.Repository(t).ChiefOfStaffThread(f.Sibling)
				fixturekit.Must(t, err)
				for _, turn := range thread.Turns {
					if turn.Request.TurnID == message.Turn && turn.Response != nil {
						t.Fatalf("chief preview failed: %s", turn.Response.Failure)
					}
				}
				t.Fatalf("chief preview failed: %+v", conversation)
			}
			if entry.Kind == "response" && entry.State == api.TurnDone {
				if !strings.HasPrefix(entry.Text, "Settings preview: ") {
					t.Fatalf("chief did not preview the page: %+v", entry)
				}
				completed = true
			}
		}
		if completed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("chief preview did not complete: %+v", conversation)
		}
		wait()
	}
	var feed api.FeedResponse
	fixturekit.Must(t, f.C.Do(ctx, http.MethodGet, api.Prefix+"/feed/"+string(f.Member), nil, &feed))
	var images []api.FeedImage
	for _, entry := range feed.Entries {
		if entry.Kind == "image" && entry.Image != nil {
			images = append(images, *entry.Image)
		}
	}
	if len(images) != 1 || images[0].URL == "" {
		t.Fatalf("member images: %+v", images)
	}
	source, err := os.ReadFile(f.CLIs.Source)
	fixturekit.Must(t, err)
	if string(source) != previewSource {
		t.Fatalf("exported source: %q", source)
	}
	return images[0]
}

func (f *PreviewJourney) ArchiveMember(t *testing.T) {
	t.Helper()
	_, err := f.C.Abandon(context.Background(), f.Member, "Finished viewing the settings page")
	fixturekit.Must(t, err)
	_, err = f.C.Archive(context.Background(), f.Member)
	fixturekit.Must(t, err)
}
