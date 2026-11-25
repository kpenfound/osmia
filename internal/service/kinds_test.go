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
	tools, err := controls.kindChiefTools(repository, scope(stream), time.Now)
	fixturekit.Must(t, err)
	if len(tools) != 1 || tools[0].Name != moveUnitTool {
		t.Fatalf("feature kind tools: %d", len(tools))
	}
	unknown := config.WorkstreamID("w_000000000000000000000000000000b1")
	fixturekit.Must(t, repository.CreateWorkstreamKind(context.Background(), unknown, "", "mystery", time.Now().UTC(), trace.Actor{Kind: "owner", ID: "local"}))
	tools, err = controls.kindChiefTools(repository, scope(unknown), time.Now)
	fixturekit.Must(t, err)
	if len(tools) != 0 {
		t.Fatalf("an unregistered kind got %d tools", len(tools))
	}
}

func TestOrdinaryChiefToolsAreTheKindsOwn(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		kind       string
		has, lacks []string
	}{
		{kindregistry.Feature, []string{moveUnitTool}, []string{climbSteerTool, climbConcludeTool}},
		{kindregistry.HillClimb, []string{climbSteerTool, climbConcludeTool}, []string{moveUnitTool}},
		{"mystery", nil, []string{moveUnitTool, climbSteerTool, climbConcludeTool}},
	} {
		tools := ChiefOrdinaryToolsOf(test.kind)
		for _, name := range test.has {
			if !slices.Contains(tools, name) {
				t.Errorf("%s chief tools lack %s", test.kind, name)
			}
		}
		for _, name := range test.lacks {
			if slices.Contains(tools, name) {
				t.Errorf("%s chief tools hold %s", test.kind, name)
			}
		}
		if !slices.Contains(tools, decideTool) {
			t.Errorf("%s chief tools lack the shared tool %s", test.kind, decideTool)
		}
	}
}
