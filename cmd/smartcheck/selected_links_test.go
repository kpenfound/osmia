package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/servicetest/daggerfixture"
)

func TestDaggerFixtureProcess(t *testing.T) { daggerfixture.Process() }

// serial: h.run uses its real command path, with only Jev stubbed.
func TestSmartcheckSelectedLinksReachDagger(t *testing.T) {
	for _, picks := range []int{0, 3} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("picks=%d/fail=%v", picks, failed), func(t *testing.T) {
				links := []string{daggerfixture.PackageA, daggerfixture.TestB, daggerfixture.Version}
				config := daggerfixture.Config{Links: links}
				if failed {
					config.Fail = daggerfixture.TestB
				}
				cli := daggerfixture.Install(t, config)
				dir := newRepository(t)
				write(t, dir, "internal/trace/trace.go", "package trace\nconst Version = 2\n")
				score := .9
				if picks > 0 {
					score = .4
				}
				jev := &fakeJev{probabilities: map[string]float64{links[0]: score, links[1]: score - .1, links[2]: score - .15}}
				var stdout, stderr strings.Builder
				h := harness{stdout: &stdout, stderr: &stderr, getenv: func(k string) string { return withKey[k] }, dagger: runDagger, evaluate: jev.evaluate}
				code := h.run(context.Background(), []string{"-C", dir, fmt.Sprintf("--fallback-picks=%d", picks), "--", "--progress=report", "--env=fixture"})
				want := 0
				if failed {
					want = 3
				}
				if code != want {
					t.Fatalf("exit %d want %d: %s %s", code, want, stdout.String(), stderr.String())
				}
				wantLinks := links
				if failed {
					wantLinks = links[:2]
				}
				cli.AssertRun(t, wantLinks)
				for _, e := range cli.Events(t) {
					if !slices.Contains(e.Args, "--progress=report") || !slices.Contains(e.Args, "--env=fixture") {
						t.Fatalf("caller args lost: %q", e.Args)
					}
				}
				events := cli.Events(t)
				wantEvents := 6
				if failed {
					wantEvents = 4
				}
				if len(events) != wantEvents {
					t.Fatalf("events %+v", events)
				}
			})
		}
	}
}

// serial: smartcheck preserves Dagger's refusal and stops without service retries.
func TestSmartcheckLateRefusalStops(t *testing.T) {
	links := []string{daggerfixture.PackageA, daggerfixture.TestB, daggerfixture.Version}
	cli := daggerfixture.Install(t, daggerfixture.Config{Links: links, RefuseOn: "go-package=b"})
	dir := newRepository(t)
	write(t, dir, "internal/trace/trace.go", "package trace\nconst Version = 2\n")
	jev := &fakeJev{probabilities: map[string]float64{links[0]: .4, links[1]: .3, links[2]: .2}}
	var stdout, stderr strings.Builder
	h := harness{stdout: &stdout, stderr: &stderr, getenv: func(k string) string { return withKey[k] }, dagger: runDagger, evaluate: jev.evaluate}
	code := h.run(t.Context(), []string{"-C", dir, "--fallback-picks=3", "--", "--progress=report"})
	if code != 1 || !strings.Contains(stderr.String(), daggerfixture.Refusal) {
		t.Fatalf("exit %d: %s %s", code, stdout.String(), stderr.String())
	}
	if events := cli.Events(t); len(events) != 4 || !slices.Contains(events[2].Args, daggerfixture.TestB) {
		t.Fatalf("reran or fell back: %+v", events)
	}
}
