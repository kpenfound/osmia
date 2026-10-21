package fixturekit

import (
	"context"
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// conversationFixture creates a Project with a trace holding two workstreams
// and a charter rule.
func Conversation(t *testing.T, prefix string) (config.Options, *config.Config) {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", prefix)
	Must(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	opts := ConfigAt(t, home)
	cfg, err := config.Load(opts)
	Must(t, err)
	Must(t, os.MkdirAll(cfg.Project.Clone, 0700))
	DemoGit(t, home, "-C", cfg.Project.Clone, "init", "-q")
	owner := trace.Actor{Kind: "owner", ID: "local"}
	repo, err := trace.Create(ctx, cfg.Root, cfg.Project, DemoStart, owner)
	Must(t, err)
	Must(t, repo.CreateWorkstream(ctx, Stream, DemoStart, owner))
	Must(t, repo.CreateWorkstream(ctx, Quiet, DemoStart, owner))
	RecordCharter(t, repo, trace.CharterTemplate+"\n## Rules\n\n1. Keep the upload API stable.\n")
	Must(t, repo.Close())
	return opts, cfg
}
