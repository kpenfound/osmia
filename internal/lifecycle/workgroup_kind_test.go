package lifecycle_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/journey"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestDraftWorkgroupRefusesAMemberOfAnUnregisteredKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCrossRepoWiringFixture(t, map[string]string{trace.Manager: trace.Manager})
	s, err := journey.Start(ctx, f.opts)
	fixturekit.Must(t, err)
	defer func() { fixturekit.Must(t, s.Close()) }()
	repo, err := s.Repository(f.project2)
	fixturekit.Must(t, err)
	mystery := config.WorkstreamID("w_00000000000000000000000000000c01")
	fixturekit.Must(t, repo.CreateWorkstreamKind(ctx, mystery, "", "mystery", f.clock.Now(), fixturekit.OwnerActor))
	_, fault := s.Lifecycle().DraftWorkgroup(ctx, []config.WorkstreamID{f.stream1, mystery})
	if fault == nil || fault.Code != api.Validation || !strings.Contains(fault.Message, "mystery") {
		t.Fatalf("a workgroup with a member of an unregistered kind: %+v", fault)
	}
	if id, err := repo.CrossRepoWorkgroupOf(mystery); err != nil || id != "" {
		t.Fatalf("the refused workgroup was joined: %q %v", id, err)
	}
}
