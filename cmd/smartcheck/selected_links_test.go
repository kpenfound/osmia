package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/checkselect"
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

// serial: command execution and dry-run selections use the same offered links.
func TestSmartcheckCollectionOptionsReachDagger(t *testing.T) {
	for _, picks := range []int{0, 4} {
		for _, dry := range []bool{false, true} {
			t.Run(fmt.Sprintf("picks=%d/dry=%v", picks, dry), func(t *testing.T) {
				listing, offered := daggerfixture.CollectionOptions()
				cli := daggerfixture.Install(t, listing)
				dir := newRepository(t)
				write(t, dir, "README.md", "changed\n")
				selected := []string{daggerfixture.PackageA, daggerfixture.PackageB, daggerfixture.PathA, daggerfixture.Version}
				j := &fakeJev{probabilities: map[string]float64{}}
				for _, link := range listing.Links {
					score := .01
					if !slices.Contains(offered, link) || slices.Contains(selected, link) {
						score = .9
						if picks > 0 {
							score = .4
						}
						if !slices.Contains(offered, link) {
							score = .99
						}
					}
					j.probabilities[link] = score
				}
				var stdout, stderr strings.Builder
				h := harness{stdout: &stdout, stderr: &stderr, getenv: func(k string) string { return withKey[k] }, dagger: runDagger, evaluate: j.evaluate}
				args := []string{"-C", dir, fmt.Sprintf("--fallback-picks=%d", picks), "-v"}
				if dry {
					args = append(args, "--dry-run")
				}
				if code := h.run(t.Context(), args); code != 0 {
					t.Fatalf("exit %d: %s %s", code, stdout.String(), stderr.String())
				}
				if len(j.requests) != 1 || len(j.requests[0].Questions) != len(offered) {
					t.Fatalf("questions %+v", j.requests)
				}
				for i, link := range offered {
					if q := j.requests[0].Questions[checkselect.Question(i)]; q.Instructions.(map[string]any)["link"] != link {
						t.Fatalf("question %+v, want only offered link %s", q, link)
					}
				}
				slices.Sort(selected)
				if dry {
					if stdout.String() != strings.Join(selected, "\n")+"\n" {
						t.Fatalf("selected %q", stdout.String())
					}
					return
				}
				cli.AssertRun(t, selected)
				grouped, standalone := false, false
				for _, event := range cli.Events(t) {
					if event.Kind != "start" {
						continue
					}
					if slices.Contains(event.Args, daggerfixture.PackageA+"&go-package=b") {
						grouped = true
					}
					if slices.Contains(event.Args, daggerfixture.Version) {
						standalone = slices.Equal(event.Links, []string{daggerfixture.Version})
					}
				}
				if !grouped || !standalone {
					t.Fatalf("grouped=%t standalone=%t", grouped, standalone)
				}
			})
		}
	}
}

// serial: the real command falls back without asking Jev when only a root fits.
func TestSmartcheckCollectionTooManyChecksRunsFull(t *testing.T) {
	var links []string
	for i := range checkselect.MaxQuestions + 1 {
		links = append(links, fmt.Sprintf("%s&go-test=T%d", daggerfixture.PackageA, i))
	}
	cli := daggerfixture.Install(t, daggerfixture.Config{Links: links})
	dir := newRepository(t)
	write(t, dir, "README.md", "changed\n")
	j := &fakeJev{}
	var stdout, stderr strings.Builder
	h := harness{stdout: &stdout, stderr: &stderr, getenv: func(k string) string { return withKey[k] }, dagger: runDagger, evaluate: j.evaluate}
	code := h.run(t.Context(), []string{"-C", dir, "--fallback-picks=2"})
	want := fmt.Sprintf("running every check: %d check links do not narrow to %d", checkselect.MaxQuestions+1, checkselect.MaxQuestions)
	if code != 0 || len(j.requests) != 0 || !strings.Contains(stderr.String(), want) {
		t.Fatalf("exit %d: %s %s; requests %+v", code, stdout.String(), stderr.String(), j.requests)
	}
	cli.AssertRun(t, nil)
}
