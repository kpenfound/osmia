package service

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/lifecycle"
	"github.com/kpenfound/osmia/internal/runtime"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestWorkgroupResolutionPreservesTraceErrorLogging(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"unknown-workstream", "unknown-member-with-design", "damaged-trace", "missing-trace-after-discovery"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(fixture(t).Config)
			must(t, err)
			repo, err := trace.Create(context.Background(), cfg.Root, cfg.Project, time.Now(), serviceActor)
			must(t, err)
			t.Cleanup(func() { repo.Close() })
			missing := config.WorkstreamID("w_00000000000000000000000000000902")
			switch failure {
			case "unknown-workstream":
				_, err = trace.EnsureWorkgroups(context.Background(), repo, []config.WorkstreamID{missing}, time.Now(), serviceActor, "unknown-member")
				must(t, err)
			case "unknown-member-with-design":
				design := config.WorkstreamID("w_00000000000000000000000000000903")
				must(t, repo.CreateWorkstream(context.Background(), design, time.Now(), serviceActor))
				h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: "open-design", Revision: 1, Project: repo.Project(), Workstream: design, At: time.Now(), Actor: serviceActor, Cause: "fixture"}
				_, err = repo.Transact(context.Background(), trace.Transaction{Transition: trace.Transition{Header: h, Subject: intake.DesignSessionSubject, To: intake.DesignOpenValue, Reason: "open design"}})
				must(t, err)
				_, err = trace.CreateWorkgroup(context.Background(), repo, []config.WorkstreamID{missing, design}, time.Now(), serviceActor, "unknown-member")
				must(t, err)
			case "damaged-trace":
				path, err := cfg.Root.WorkstreamTrace(repo.Project(), missing)
				must(t, err)
				must(t, os.MkdirAll(path, 0700))
			case "missing-trace-after-discovery":
				must(t, repo.CreateWorkstream(context.Background(), missing, time.Now(), serviceActor))
			}
			controller := lifecycle.New(lifecycle.Dependencies{
				Current: func() *config.Config { return cfg },
				Now:     time.Now,
				Store:   func() *runtime.Store { return nil },
				Step: func(step string) error {
					if failure == "missing-trace-after-discovery" && step == "workgroup-resolution-discovered" {
						_, err := repo.MarkWorkstreamTraceForDeletion(missing)
						return err
					}
					return nil
				},
			})
			resolver := &lifecycle.WorkgroupResolver{S: controller, Repository: repo}
			p := &pipeline{}
			p.current.Store(&stages{cfg: cfg, hooks: []scheduleHook{{"workgroup-resolution", resolver.Pass}}})
			var logs bytes.Buffer
			ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)))
			err = p.schedule(ctx)
			if err == nil {
				t.Fatal("workgroup resolution silently skipped invalid trace state")
			}
			if failure == "missing-trace-after-discovery" && !strings.Contains(err.Error(), "cross-repo workgroup") {
				t.Fatalf("did not exercise cross-repo locator read: %v", err)
			}
			requireFactoryLog(t, decodeFactoryLog(t, logs.String()), "reconciliation pass hook failed", map[string]any{
				"project": string(repo.Project()), "hook": "workgroup-resolution",
			})
			if !strings.Contains(logs.String(), `"level":"ERROR"`) {
				t.Fatalf("trace error did not reach ERROR logging: %s", logs.String())
			}
		})
	}
}
