package lifecycle

import (
	"context"
	"os"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
)

func TestSchedulingScansPreserveTraceErrors(t *testing.T) {
	t.Parallel()
	cfg, repo, _, refresh, _ := stopFixture(t)
	missing := config.WorkstreamID("w_00000000000000000000000000000902")
	if _, err := snapshotWorkstreamBase(repo, missing); err == nil {
		t.Fatal("unknown workstream was silently skipped")
	}
	// A directory without a database is damaged state, not a destroyed session.
	path, err := cfg.Root.WorkstreamTrace(repo.Project(), missing)
	must(t, err)
	must(t, os.Mkdir(path, 0700))
	ctx := context.Background()
	for name, pass := range map[string]func(context.Context) error{
		"base":                           func(ctx context.Context) error { return refresh.S.BaseWaitPass(ctx, repo) },
		"edge":                           func(ctx context.Context) error { return refresh.S.EdgeWaitPass(ctx, repo) },
		"base-refresh":                   refresh.Pass,
		"edge-refresh":                   func(ctx context.Context) error { return refresh.S.EdgeResolverPass(ctx, repo) },
		"workgroup-resolution":           (&WorkgroupResolver{S: refresh.S, Repository: repo}).Pass,
		"workgroup-project-spec":         func(ctx context.Context) error { return refresh.S.ProjectSpecDraftPass(ctx, repo) },
		"workgroup-project-spec-refresh": func(ctx context.Context) error { return refresh.S.ProjectSpecRefreshPass(ctx, repo) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := pass(ctx); err == nil {
				t.Fatal("damaged trace was silently skipped")
			}
		})
	}
}
