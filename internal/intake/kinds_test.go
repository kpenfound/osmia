package intake

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	featureStream config.WorkstreamID = "w_00000000000000000000000000000a01"
	legacyStream  config.WorkstreamID = "w_00000000000000000000000000000a02"
	unknownStream config.WorkstreamID = "w_00000000000000000000000000000a03"
	sessionStream config.WorkstreamID = "w_00000000000000000000000000000a04"
)

func kindsFixture(t *testing.T) *trace.Repository {
	t.Helper()
	_, _, repo := fixturekit.Activity(t)
	t.Cleanup(func() { repo.Close() })
	ctx := context.Background()
	fixturekit.Must(t, repo.CreateWorkstreamKind(ctx, featureStream, "", "feature", fixturekit.DemoStart, OwnerActor))
	fixturekit.Must(t, repo.CreateWorkstream(ctx, legacyStream, fixturekit.DemoStart, OwnerActor))
	fixturekit.Must(t, repo.CreateWorkstreamKind(ctx, unknownStream, "", "mystery", fixturekit.DemoStart, OwnerActor))
	fixturekit.Must(t, repo.CreateSessionWorkstream(ctx, sessionStream, "default", "base", "branch", "", fixturekit.DemoStart, OwnerActor))
	return repo
}

func TestWorkstreamKindResolvesStoredKinds(t *testing.T) {
	repo := kindsFixture(t)
	for _, c := range []struct {
		stream config.WorkstreamID
		stored string
		known  bool
	}{{featureStream, "feature", true}, {legacyStream, "feature", true}, {unknownStream, "mystery", false}, {sessionStream, trace.SessionKind, false}} {
		_, stored, ok, err := WorkstreamKind(repo, c.stream)
		if err != nil || stored != c.stored || ok != c.known {
			t.Fatalf("%s: stored %q registered %v %v", c.stream, stored, ok, err)
		}
	}
}

func TestIsSessionKeepsItsMeaning(t *testing.T) {
	repo := kindsFixture(t)
	for stream, want := range map[config.WorkstreamID]bool{featureStream: false, legacyStream: false, unknownStream: false, sessionStream: true} {
		if got, err := IsSession(repo, stream); err != nil || got != want {
			t.Fatalf("IsSession(%s) = %v %v", stream, got, err)
		}
	}
}

func TestAWorkstreamOfAnUnregisteredKindWaitsAndIsNeverFeature(t *testing.T) {
	repo := kindsFixture(t)
	running, err := NonSessionWorkstreams(repo)
	if err != nil || slices.Contains(running, unknownStream) || !slices.Contains(running, featureStream) || !slices.Contains(running, legacyStream) || slices.Contains(running, sessionStream) {
		t.Fatalf("workstreams that move work: %v %v", running, err)
	}
	observed, err := ObservedWorkstreams(repo)
	if err != nil || !slices.Contains(observed, unknownStream) || !slices.Contains(observed, featureStream) || slices.Contains(observed, sessionStream) {
		t.Fatalf("observed workstreams: %v %v", observed, err)
	}
}

func TestAnUnregisteredKindRaisesExactlyOneNoticeAcrossPasses(t *testing.T) {
	repo := kindsFixture(t)
	ctx := context.Background()
	for range 4 {
		fixturekit.Must(t, KindUnrecognisedPass(ctx, repo))
	}
	notices := func(stream config.WorkstreamID) []trace.OutboxEntry {
		entries, err := repo.Outbox(stream)
		fixturekit.Must(t, err)
		return slices.DeleteFunc(entries, func(e trace.OutboxEntry) bool { return e.TransitionID != KindUnrecognisedTransition })
	}
	got := notices(unknownStream)
	if len(got) != 1 || got[0].Event.Kind != trace.NoticeKind || !strings.Contains(got[0].Event.Body, `"mystery"`) || !strings.Contains(got[0].Event.Body, "never run as another kind") {
		t.Fatalf("notices of the unregistered kind: %+v", got)
	}
	for _, stream := range []config.WorkstreamID{featureStream, legacyStream, sessionStream} {
		if n := notices(stream); len(n) != 0 {
			t.Fatalf("%s raised %+v", stream, n)
		}
	}
	if state, err := repo.Workflow(unknownStream, KindUnrecognisedSubject); err != nil || state.Value != "unrecognised" {
		t.Fatalf("subject state: %+v %v", state, err)
	}
	if state, err := repo.Workflow(unknownStream, trace.FeatureSubject); err != nil || state.Value != "" {
		t.Fatalf("the waiting workstream moved: %+v %v", state, err)
	}
}

func TestDependencyKindsRefuseKindsThatCannotBeABase(t *testing.T) {
	repo := kindsFixture(t)
	if api := CheckDependencyKinds(repo, featureStream, legacyStream, sessionStream); api != nil {
		t.Fatalf("feature workstreams: %+v", api)
	}
	if api := CheckDependencyKinds(repo, featureStream, unknownStream); api == nil || api.Code != Validation || !strings.Contains(api.Message, "mystery") {
		t.Fatalf("an unregistered kind: %+v", api)
	}
}

func TestFeatureHandInPassesItsKindChecks(t *testing.T) {
	if api := checkHandInKind("feature", 3, "w_00000000000000000000000000000a01"); api != nil {
		t.Fatalf("a feature hand-in of three projects with a base: %+v", api)
	}
}
