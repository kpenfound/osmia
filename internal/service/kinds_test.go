package service

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	kindregistry "github.com/kpenfound/osmia/internal/kinds"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestChiefGrantHoldsEveryKindsChiefTools(t *testing.T) {
	t.Parallel()
	for _, name := range kindregistry.ChiefToolNames() {
		if !slices.Contains(chiefGrant.Tools, name) || !slices.Contains(ChiefOrdinaryTools, name) {
			t.Errorf("the chief's grant lacks the kind tool %s", name)
		}
	}
}

func TestKindChiefToolsFollowTheWorkstreamsKind(t *testing.T) {
	t.Parallel()
	f, stream, repository := newReviewFixture(t, "kind-chief-tools")
	controls := &runtimeControls{}
	scope := func(stream config.WorkstreamID) coreadapter.Scope {
		return coreadapter.Scope{Project: string(f.project), Workstream: string(stream), Role: trace.ChiefOfStaff, Thread: trace.ChiefOfStaff, Turn: "turn"}
	}
	tools, err := controls.kindChiefTools(repository, scope(stream))
	fixturekit.Must(t, err)
	if len(tools) != 1 || tools[0].Name != moveUnitTool {
		t.Fatalf("feature kind tools: %d", len(tools))
	}
	unknown := config.WorkstreamID("w_000000000000000000000000000000b1")
	fixturekit.Must(t, repository.CreateWorkstreamKind(context.Background(), unknown, "", "mystery", time.Now().UTC(), trace.Actor{Kind: "owner", ID: "local"}))
	tools, err = controls.kindChiefTools(repository, scope(unknown))
	fixturekit.Must(t, err)
	if len(tools) != 0 {
		t.Fatalf("an unregistered kind got %d tools", len(tools))
	}
}
