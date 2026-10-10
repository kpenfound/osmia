package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/systemone"
)

const (
	traceTests   = "dag+check://go/packages/tests/test?go-package=internal/trace"
	serviceTests = "dag+check://go/packages/tests/test?go-package=internal/service"
	release      = "dag+check://release/version-round-trip"
)

// fakeDagger stands in for the Dagger CLI, which is costly to invoke for
// real (it builds and runs the project's checks) and may not be installed.
// It lists links and records each dagger check it is asked to run, but does
// not exercise whether the real Dagger CLI accepts the arguments smartcheck
// passes it or actually runs the selected checks.
type fakeDagger struct {
	links []string
	exit  int

	mu     sync.Mutex
	checks [][]string
}

func (d *fakeDagger) run(_ context.Context, _ string, stdout, _ io.Writer, args ...string) (int, error) {
	if slices.Equal(args, []string{"list", "checks", "--all", "--format=link"}) {
		fmt.Fprintln(stdout, "Loading modules")
		for _, l := range d.links {
			fmt.Fprintln(stdout, l)
		}
		return 0, nil
	}
	if len(args) == 0 || args[0] != "check" {
		return -1, fmt.Errorf("unexpected dagger %q", args)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.checks = append(d.checks, args[1:])
	return d.exit, nil
}

// fakeJev stands in for the Jev model service reached over the network,
// which is both costly and nondeterministic to call for real. It answers
// with probabilities by check link, or with err. It does not exercise
// whether a real Jev endpoint accepts the request shape smartcheck sends or
// returns well-formed answers for it.
type fakeJev struct {
	probabilities map[string]float64
	err           error

	clients  []systemone.Client
	requests []systemone.Request
}

func (j *fakeJev) evaluate(c systemone.Client, _ context.Context, r systemone.Request) (systemone.Response, error) {
	j.clients, j.requests = append(j.clients, c), append(j.requests, r)
	if j.err != nil {
		return systemone.Response{}, j.err
	}
	answers := map[string]systemone.Answer{}
	for id, q := range r.Questions {
		for link, p := range j.probabilities {
			if strings.Contains(fmt.Sprint(q.Instructions), link+"?") {
				answers[id] = systemone.Answer{Kind: systemone.KindNoul, Noul: p}
			}
		}
	}
	return systemone.Response{Model: "jev-1.13.0", Answers: answers, Usage: systemone.Usage{InputTokens: 900, OutputTokens: 12}}, nil
}

type result struct {
	code           int
	stdout, stderr string
}

func runHarness(t *testing.T, d *fakeDagger, j *fakeJev, env map[string]string, args ...string) result {
	t.Helper()
	var stdout, stderr strings.Builder
	h := harness{stdout: &stdout, stderr: &stderr, getenv: func(k string) string { return env[k] }, dagger: d.run, evaluate: j.evaluate}
	code := h.run(context.Background(), args)
	return result{code, stdout.String(), stderr.String()}
}

var withKey = map[string]string{config.DefaultJevAPIKeyEnv: "test-key"}

// newRepository returns a repository whose main branch has one commit, with
// a feature branch checked out.
func newRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %q: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet", "--initial-branch=main")
	write(t, dir, "internal/trace/trace.go", "package trace\n")
	write(t, dir, "README.md", "# Project\n")
	run("add", ".")
	run("commit", "--quiet", "-m", "initial")
	run("checkout", "--quiet", "-b", "feature")
	return dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An uncommitted change against main runs only the checks Jev selects, and
// without -v prints nothing of the selection.
func TestRunsTheSelectedChecks(t *testing.T) {
	t.Parallel()
	dir := newRepository(t)
	write(t, dir, "internal/trace/trace.go", "package trace\n\nconst Version = 2\n")
	d := &fakeDagger{links: []string{release, serviceTests, traceTests}}
	j := &fakeJev{probabilities: map[string]float64{traceTests: 0.95, serviceTests: 0.55, release: 0.45}}
	r := runHarness(t, d, j, withKey, "-C", filepath.Join(dir, "internal"), "--", "--progress=report")
	if r.code != 0 || r.stderr != "" {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if want := [][]string{{"--failfast", "--progress=report", serviceTests, traceTests}}; !slices.EqualFunc(d.checks, want, slices.Equal) {
		t.Fatalf("ran %q, want %q", d.checks, want)
	}
	c := j.clients[0]
	if c.APIKey != "test-key" || c.Model != config.DefaultJevModel || c.BaseURL != config.DefaultJevURL {
		t.Fatalf("client %+v", c)
	}
	req := j.requests[0]
	state := req.State.(map[string]any)
	if len(req.Questions) != 3 || state["changed_files"] != "- internal/trace/trace.go (+2 -0)\n" || !strings.Contains(state["diff"].(string), "+const Version = 2") {
		t.Fatalf("request %+v", req)
	}
}

func TestVerboseExplainsTheSelection(t *testing.T) {
	t.Parallel()
	dir := newRepository(t)
	write(t, dir, "internal/trace/trace.go", "package trace\n\nconst Version = 2\n")
	d := &fakeDagger{links: []string{release, traceTests}}
	j := &fakeJev{probabilities: map[string]float64{traceTests: 0.95, release: 0.05}}
	r := runHarness(t, d, j, withKey, "-C", dir, "-v")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"base: main (merge base ", "- internal/trace/trace.go (+2 -0)", "checks: 2 links, asking about 2", "jev: jev-1.13.0 answered", "  ✓ 0.95 " + traceTests + "\n    0.05 " + release, "selected 1 of 2 at probability 0.50 or more"} {
		if !strings.Contains(r.stderr, want) {
			t.Fatalf("verbose output lacks %q:\n%s", want, r.stderr)
		}
	}
}

func TestDryRunPrintsTheSelection(t *testing.T) {
	t.Parallel()
	dir := newRepository(t)
	write(t, dir, "internal/trace/trace.go", "package trace\n\nconst Version = 2\n")
	d := &fakeDagger{links: []string{release, traceTests}}
	j := &fakeJev{probabilities: map[string]float64{traceTests: 0.5, release: 0.2}}
	r := runHarness(t, d, j, withKey, "-C", dir, "--dry-run", "--threshold=0.1")
	if r.code != 0 || r.stdout != traceTests+"\n"+release+"\n" || len(d.checks) != 0 {
		t.Fatalf("exit %d, stdout %q, ran %q", r.code, r.stdout, d.checks)
	}
}

// Every check runs whenever Jev does not select any, as in the factory.
func TestFallsBackToEveryCheck(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		jev    *fakeJev
		reason string
	}{
		{"unanswered", &fakeJev{err: &systemone.Error{Kind: systemone.KindRateLimited, Message: "slow down"}}, "Jev did not answer: "},
		{"none likely", &fakeJev{probabilities: map[string]float64{traceTests: 0.45, release: 0.05}}, "no check reached probability 0.50"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := newRepository(t)
			write(t, dir, "NOTES.md", "untracked\n")
			write(t, dir, "README.md", "# Project\n\nMore.\n")
			d := &fakeDagger{links: []string{release, traceTests}, exit: 3}
			r := runHarness(t, d, c.jev, withKey, "-C", dir)
			if r.code != 3 || !strings.Contains(r.stderr, "running every check: "+c.reason) {
				t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
			}
			if want := [][]string{{"--failfast"}}; !slices.EqualFunc(d.checks, want, slices.Equal) {
				t.Fatalf("ran %q, want every check", d.checks)
			}
			if files := c.jev.requests[0].State.(map[string]any)["changed_files"]; files != "- README.md (+2 -0)\n" {
				t.Fatalf("changed files %q", files)
			}
		})
	}
}

func TestNothingChangedRunsNothing(t *testing.T) {
	t.Parallel()
	dir := newRepository(t)
	d, j := &fakeDagger{links: []string{traceTests}}, &fakeJev{}
	r := runHarness(t, d, j, withKey, "-C", dir)
	if r.code != 0 || !strings.Contains(r.stderr, "nothing changed since main") || len(d.checks) != 0 || len(j.requests) != 0 {
		t.Fatalf("exit %d, stderr %q, ran %q", r.code, r.stderr, d.checks)
	}
}

// Without the default Jev API key env var set, smartcheck refuses before
// touching the repository or Dagger.
func TestRefusesWithoutAnAPIKey(t *testing.T) {
	t.Parallel()
	dir := newRepository(t)
	d, j := &fakeDagger{links: []string{traceTests}}, &fakeJev{}
	r := runHarness(t, d, j, map[string]string{"JEV_KEY": "set"}, "-C", dir)
	if r.code != 1 || !strings.Contains(r.stderr, config.DefaultJevAPIKeyEnv+" is not set") || len(d.checks) != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
}

// --api-key-env names the environment variable smartcheck reads instead of
// the default, and --base is accepted as a revision other than main.
func TestHonoursAPIKeyEnvAndBaseFlags(t *testing.T) {
	t.Parallel()
	dir := newRepository(t)
	write(t, dir, "internal/trace/trace.go", "package trace\n\nconst Version = 2\n")
	d := &fakeDagger{links: []string{traceTests}}
	j := &fakeJev{probabilities: map[string]float64{traceTests: 0.95}}
	r := runHarness(t, d, j, map[string]string{"JEV_KEY": "set"}, "-C", dir, "--api-key-env=JEV_KEY", "--base=HEAD")
	if r.code != 0 || r.stderr != "" || j.clients[0].APIKey != "set" || !slices.EqualFunc(d.checks, [][]string{{"--failfast", traceTests}}, slices.Equal) {
		t.Fatalf("exit %d, stderr %q, ran %q", r.code, r.stderr, d.checks)
	}
}

// An unknown --base revision surfaces git's own error and runs nothing.
func TestUnknownBaseRevisionFails(t *testing.T) {
	t.Parallel()
	dir := newRepository(t)
	d, j := &fakeDagger{}, &fakeJev{}
	r := runHarness(t, d, j, withKey, "-C", dir, "--base=trunk")
	if r.code != 1 || !strings.Contains(r.stderr, "git merge-base trunk HEAD") || len(d.checks) != 0 {
		t.Fatalf("exit %d, stderr %q, ran %q", r.code, r.stderr, d.checks)
	}
}

// An out-of-range --threshold is rejected before anything runs.
func TestInvalidThresholdIsRejected(t *testing.T) {
	t.Parallel()
	d, j := &fakeDagger{}, &fakeJev{}
	r := runHarness(t, d, j, withKey, "--threshold=2")
	if r.code != 2 || len(d.checks) != 0 || len(j.requests) != 0 {
		t.Fatalf("exit %d, stderr %q, ran %q", r.code, r.stderr, d.checks)
	}
}
