package fixturekit

import (
	"context"
	"time"

	"github.com/kpenfound/osmia/internal/api"
)

const (
	SmartTestRoot   = "dag+check://?check=go/modules/packages/tests/test"
	SmartStaleRoot  = "dag+check://?check=go/modules/packages/generate/stale"
	SmartStandalone = "dag+check://?check=fixture/standalone"
	SmartUncovered  = "dag+check://?check=unknown"
	SmartPackageA   = "dag+check://?check=test&go-module=.&go-package=a"
	SmartPackageB   = "dag+check://?check=test&go-module=.&go-package=b"
)

// ScopedFakeChecks supplies Dagger's unexpanded and scoped listings alongside
// the ordinary check runner fake. Calls and ListingTime measure listing work.
type ScopedFakeChecks struct {
	*FakeChecks
	Roots       []string
	Members     map[string][]string
	TopErr      error
	ScopedErr   error
	Calls       []string
	ListingTime time.Duration
}

func (f *ScopedFakeChecks) List(ctx context.Context, dir string, identity api.CheckIdentity) ([]string, error) {
	start := time.Now()
	defer func() { f.ListingTime += time.Since(start) }()
	f.Calls = append(f.Calls, "all")
	return f.FakeChecks.List(ctx, dir, identity)
}

func (f *ScopedFakeChecks) ListTopLevel(context.Context, string, api.CheckIdentity) ([]string, error) {
	start := time.Now()
	defer func() { f.ListingTime += time.Since(start) }()
	f.Calls = append(f.Calls, "top")
	return f.Roots, f.TopErr
}

func (f *ScopedFakeChecks) ListScoped(_ context.Context, _ string, _ api.CheckIdentity, root string) ([]string, error) {
	start := time.Now()
	defer func() { f.ListingTime += time.Since(start) }()
	f.Calls = append(f.Calls, root)
	return f.Members[root], f.ScopedErr
}

// SmartChecks uses the real Dagger shapes: full-path roots, short check names
// on expanded Go items, a standalone check, and an unmatched expanded link.
func SmartChecks() *ScopedFakeChecks {
	tests := []string{SmartPackageA + "&go-test=TestOne", SmartPackageA + "&go-test=TestTwo", SmartPackageB + "&go-test=TestOne", SmartPackageB + "&go-test=TestTwo"}
	stale := []string{"dag+check://?check=stale&go-module=.&go-package=a", "dag+check://?check=stale&go-module=.&go-package=b"}
	links := append(append([]string{SmartStandalone, SmartUncovered}, tests...), stale...)
	return &ScopedFakeChecks{
		FakeChecks: &FakeChecks{Links: links, Outcomes: Passing()},
		Roots:      []string{SmartStandalone, SmartStaleRoot, SmartTestRoot},
		Members:    map[string][]string{SmartStandalone: {SmartStandalone}, SmartStaleRoot: stale, SmartTestRoot: tests},
	}
}
