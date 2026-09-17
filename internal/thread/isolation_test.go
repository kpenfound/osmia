package thread

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	a "github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/coreadapter/adaptertest"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestIsolationFailureIsDurableBeforeExecution(t *testing.T) {
	for _, failure := range []string{"workspace", "host", "container"} {
		t.Run(failure, func(t *testing.T) {
			repo, root, projectConfig := setup(t)
			queueBackend(t, repo, "denied", "claude")
			source, views := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "file"), []byte("data"), 0600); err != nil {
				t.Fatal(err)
			}
			lease := &adaptertest.Lease{Releases: *adaptertest.NewScript[struct{}, struct{}](adaptertest.Reply[struct{}]{})}
			provider := &adaptertest.Workspaces{Script: *adaptertest.NewScript[a.WorkspaceRequest, a.WorkspaceLease](adaptertest.Reply[a.WorkspaceLease]{Value: a.WorkspaceLease{Workspace: a.Workspace{Directory: source, Access: a.ReadOnly}, Lease: lease}})}
			engine := &adaptertest.Engine{Mutate: func(turn *agent.Turn) { turn.VCS = true }}
			boundary := &isolation.Turns{Workspaces: provider, Views: isolation.Views{Directory: views}, Engine: engine, Grants: map[string]a.Capabilities{"mason": {}},
				Select: func(context.Context, a.Scope) (isolation.Selection, error) {
					mode, paths := "none", []string{"file"}
					if failure == "container" {
						mode = "container"
					}
					if failure == "workspace" {
						paths = []string{"../escape"}
					}
					return isolation.Selection{Paths: paths, Execution: a.ExecutionSettings{Mode: mode, Image: "fixture-image"}}, nil
				}}
			runner := Runner{Store: repo, Turns: boundary, Now: func() time.Time { return timestamp.Add(time.Second) }}
			q, runErr := runner.RunNext(context.Background(), stream, "agent", a.PreparedTurn{SessionDirectory: t.TempDir()})
			if runErr == nil || q.Response == nil || q.Response.Failure == "" || !q.Response.Result.IsError || q.CompletedAt.IsZero() {
				t.Fatalf("failure not captured: %+v %v", q, runErr)
			}
			if q.Response.Result.Session != (a.BackendSession{}) {
				t.Fatalf("rejected launch recorded a partial session identity: %+v", q.Response.Result.Session)
			}
			if failure != "workspace" && !errors.Is(runErr, a.ErrUnsupported) {
				t.Fatal(runErr)
			}
			if len(engine.Requests) != 0 {
				t.Fatal("unverified runtime started")
			}
			entries, err := os.ReadDir(views)
			if err != nil || len(entries) != 0 {
				t.Fatal("view leaked")
			}
			repo.Close()
			reopened, err := trace.Open(root, projectConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			thread, err := reopened.Thread(stream, "agent")
			if err != nil || thread.Status != "failed" || thread.Active != "" {
				t.Fatalf("durable state: %+v %v", thread, err)
			}
			responses, err := trace.Read[trace.TurnResponse](reopened, stream)
			if err != nil || len(responses) != 1 || responses[0].Failure != q.Response.Failure {
				t.Fatalf("durable response: %+v %v", responses, err)
			}
		})
	}
}
