package executionjourney

import (
	"encoding/json"
	"fmt"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/servicetest/intakefixture"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	checkrun "github.com/kpenfound/osmia/internal/checks"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	cloudToken = "trace-link-test-token"

	localTrace = "https://dagger.cloud/example/traces/local111"
	cloudTrace = "https://dagger.cloud/example/traces/cloud222"
	lateTrace  = "https://dagger.cloud/example/traces/later333"
)

const (
	fakePasses     = "printf 'CHECKS  ✔ 1 passed\\n✔ dag://go/test 1s OK\\n'\n"
	fakeFails      = "printf 'CHECKS  ✘ 1 failed\\n✘ dag://go/test 1s ERROR\\n    wrong answer\\n'\nexit 3\n"
	fakeEngineLost = "echo 'Error: the engine session was lost' >&2\nexit 1\n"

	fakeHangs = ": > \"$dir/printed\"\nexec sleep 60\n"

	fakeHolds = ": > \"$dir/printed\"\nwhile [ ! -f \"$dir/release\" ]; do sleep 0.05; done\n" + fakePasses
)

func fakeLink(url string, stderr bool) string {
	line := "echo 'Full trace at " + url + "'"
	if stderr {
		line += " >&2"
	}
	return line + "\n"
}

type fakeDagger struct{ dir string }

func installFakeDagger(t *testing.T, scripts map[string][]string) fakeDagger {
	t.Helper()
	dir := t.TempDir()
	for engine, snippets := range scripts {
		for i, snippet := range snippets {
			fixturekit.Must(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s.%d", engine, i+1)), []byte(snippet), 0600))
		}
		fixturekit.Must(t, os.WriteFile(filepath.Join(dir, engine+".max"), []byte(strconv.Itoa(len(snippets))), 0600))
	}
	bin := t.TempDir()
	script := `#!/bin/sh
dir='` + dir + `'
if [ "$1" = list ]; then echo 'dag+check://go/test'; exit 0; fi
case "${DAGGER_ENGINE-}" in
"") engine=local ;;
cloud) engine=cloud ;;
*) engine=address ;;
esac
n=$(cat "$dir/$engine.n" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "$dir/$engine.n"
max=$(cat "$dir/$engine.max")
[ "$n" -le "$max" ] || n=$max
. "$dir/$engine.$n"
`
	fixturekit.Must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return fakeDagger{dir: dir}
}

func (d fakeDagger) marker(name string) string { return filepath.Join(d.dir, name) }

func (d fakeDagger) touch(t *testing.T, name string) {
	t.Helper()
	fixturekit.Must(t, os.WriteFile(d.marker(name), nil, 0600))
}

func (d fakeDagger) await(t *testing.T, name string) {
	t.Helper()
	intakefixture.Soon(t, "the fake dagger to write "+name, func() bool {
		_, err := os.Stat(d.marker(name))
		return err == nil
	})
}

func localChecks(token string) checkrun.DaggerChecks { return checkrun.DaggerChecks{CloudToken: token} }

func cloudChecks(token string) checkrun.DaggerChecks {
	return checkrun.DaggerChecks{Engine: "cloud", CloudToken: token}
}

type linkedChecksService struct {
	f       *shedFixture
	stream  config.WorkstreamID
	timeout string
	open    atomic.Bool
	ticks   chan time.Time
	running bool
}

func startLinkedChecksService(t *testing.T, f *shedFixture, repo *trace.Repository, stream config.WorkstreamID, checker checkrun.ReviewChecks, timeout string) *linkedChecksService {
	t.Helper()
	fixturekit.Must(t, repo.Close())
	r := &linkedChecksService{f: f, stream: stream, timeout: timeout, ticks: make(chan time.Time, 1)}
	f.opts.Reconciliation.Ticks = r.ticks
	f.opts.Reconciliation.Hold = func(_ config.WorkstreamID, op coreadapter.Operation) bool {
		return !r.open.Load() || op.Action != service.CheckAction
	}
	f.opts.reviewChecks = checker
	r.start(t)
	t.Cleanup(func() {
		if r.running {
			r.stop(t)
		}
	})
	return r
}

func (r *linkedChecksService) start(t *testing.T) {
	t.Helper()
	r.f.start(t)
	r.running = true
	if r.timeout != "" {
		cfg := r.f.s.current()
		cfg.Project.ChecksTimeout = r.timeout
		for i := range cfg.Projects {
			cfg.Projects[i].ChecksTimeout = r.timeout
		}
	}
}

func (r *linkedChecksService) stop(t *testing.T) {
	t.Helper()
	r.running = false
	r.f.stop(t)
}

func (r *linkedChecksService) allow() {
	r.open.Store(true)
	select {
	case r.ticks <- r.f.clock.Now():
	default:
	}
}

func (r *linkedChecksService) repo() *trace.Repository { return r.f.repository() }

func (r *linkedChecksService) awaitRun(t *testing.T) execution.UnitCheckRun {
	t.Helper()
	intakefixture.Soon(t, "the service to record the check run", func() bool {
		runs, err := execution.CheckRuns(r.repo(), r.stream)
		fixturekit.Must(t, err)
		return len(runs) == 1
	})
	return onlyRun(t, r.repo(), r.stream)
}

func (r *linkedChecksService) revisions(t *testing.T) []execution.UnitCheckSelection {
	t.Helper()
	var revisions []execution.UnitCheckSelection
	for _, d := range intakefixture.StreamDocuments(t, r.repo(), r.stream, execution.CheckSelectionDocument("resume", 1)) {
		var start execution.UnitCheckSelection
		fixturekit.Must(t, json.Unmarshal([]byte(d.Content), &start))
		if d.Revision != len(revisions)+1 {
			t.Fatalf("start record revision %d after %d revisions", d.Revision, len(revisions))
		}
		revisions = append(revisions, start)
	}
	return revisions
}

func (r *linkedChecksService) links(t *testing.T) string {
	t.Helper()
	var links []string
	for _, start := range r.revisions(t) {
		links = append(links, start.TraceURL)
	}
	return strings.Join(links, ",")
}

func linkList(links ...string) string { return strings.Join(links, ",") }

func (r *linkedChecksService) awaitLinks(t *testing.T, links ...string) {
	t.Helper()
	intakefixture.Soon(t, "the start record to carry "+linkList(links...), func() bool { return r.links(t) == linkList(links...) })
}

func (r *linkedChecksService) entry(t *testing.T) *api.FeedUnitChecks {
	t.Helper()
	feed, apiErr := r.f.s.feedList(string(r.stream))
	if apiErr != nil {
		t.Fatalf("feed: %+v", apiErr)
	}
	var found *api.FeedUnitChecks
	for _, e := range feed.Entries {
		if e.Kind == "unit_checks" {
			if found != nil {
				t.Fatalf("two unit_checks entries: %+v and %+v", found, e.UnitChecks)
			}
			found = e.UnitChecks
		}
	}
	if found == nil {
		t.Fatal("the feed has no unit_checks entry")
	}
	return found
}

func (r *linkedChecksService) feedJSON(t *testing.T) string {
	t.Helper()
	feed, apiErr := r.f.s.feedList(string(r.stream))
	if apiErr != nil {
		t.Fatalf("feed: %+v", apiErr)
	}
	data, err := json.Marshal(feed)
	fixturekit.Must(t, err)
	return string(data)
}

func newLinkedChecksService(t *testing.T, key string, checker checkrun.ReviewChecks, timeout string) *linkedChecksService {
	t.Helper()
	f, stream, repo := newChecksFixture(t, key, checker)
	return startLinkedChecksService(t, f, repo, stream, checker, timeout)
}

func newFallbackLinkedChecksService(t *testing.T, key string, local checkrun.ReviewChecks, timeout string) *linkedChecksService {
	t.Helper()
	t.Setenv("OSMIA_FALLBACK_TEST_TOKEN", cloudToken)
	f, stream, repo := newFallbackChecksFixture(t, key, fallbackConfig, local, nil)
	return startLinkedChecksService(t, f, repo, stream, local, timeout)
}

func TestServiceUnitChecksThatTimeOutKeepTheLinkTheyPrinted(t *testing.T) {
	for name, stderr := range map[string]bool{"stderr": true, "stdout": false} {
		t.Run(name, func(t *testing.T) {
			installFakeDagger(t, map[string][]string{"local": {fakeLink(localTrace, stderr) + fakeHangs}})
			r := newLinkedChecksService(t, "trace-timeout-"+name, localChecks(cloudToken), "2s")
			r.allow()
			run := r.awaitRun(t)
			if run.Status != checkrun.ChecksIncomplete || !strings.Contains(run.Error, "checks_timeout") || run.TraceURL != localTrace {
				t.Fatalf("recorded run %+v", run)
			}
			if run.Evidence == nil || len(run.Evidence.Traces) != 1 || run.Evidence.Traces[0].URL != localTrace {
				t.Fatalf("recorded evidence %+v", run.Evidence)
			}
			if got := r.links(t); got != linkList("", localTrace) {
				t.Fatalf("start record revisions carry %q", got)
			}
			if entry := r.entry(t); entry.State != checkrun.ChecksIncomplete || entry.TraceURL != localTrace {
				t.Fatalf("feed entry %+v", entry)
			}
		})
	}
}

func TestServiceUnitChecksRecordTheLinkWhileTheRunGoesOn(t *testing.T) {
	fake := installFakeDagger(t, map[string][]string{"local": {fakeLink(localTrace, true) + fakeHolds}})
	r := newLinkedChecksService(t, "trace-running", localChecks(cloudToken), "")
	r.allow()
	r.awaitLinks(t, "", localTrace)
	if runs, err := execution.CheckRuns(r.repo(), r.stream); err != nil || len(runs) != 0 {
		t.Fatalf("a result was recorded while the run goes on: %+v %v", runs, err)
	}
	if entry := r.entry(t); entry.State != "running" || entry.TraceURL != localTrace || entry.FinishedAt != nil {
		t.Fatalf("running feed entry %+v", entry)
	}
	fake.touch(t, "release")
	run := r.awaitRun(t)
	if run.Status != checkrun.ChecksPassed || run.TraceURL != localTrace {
		t.Fatalf("recorded run %+v", run)
	}
	if got := r.links(t); got != linkList("", localTrace) {
		t.Fatalf("a link that did not change wrote another revision: %q", got)
	}
	if entry := r.entry(t); entry.State != checkrun.ChecksPassed || entry.TraceURL != localTrace {
		t.Fatalf("finished feed entry %+v", entry)
	}
}

func TestServiceStoppedUnitChecksLeaveTheLinkOnTheStartRecord(t *testing.T) {
	fake := installFakeDagger(t, map[string][]string{"local": {
		fakeLink(localTrace, true) + fakeHangs,
		fakePasses,
	}})
	r := newLinkedChecksService(t, "trace-stopped", localChecks(cloudToken), "")
	r.allow()
	fake.await(t, "printed")
	r.stop(t)

	r.open.Store(false)
	r.start(t)
	if runs, err := execution.CheckRuns(r.repo(), r.stream); err != nil || len(runs) != 0 {
		t.Fatalf("a stopped run was recorded: %+v %v", runs, err)
	}
	revisions := r.revisions(t)
	if got := r.links(t); got != linkList("", localTrace) {
		t.Fatalf("start record revisions carry %q", got)
	}
	if entry := r.entry(t); entry.State != "running" || entry.TraceURL != localTrace || entry.FinishedAt != nil {
		t.Fatalf("feed entry of the stopped run %+v", entry)
	}

	r.allow()
	run := r.awaitRun(t)
	if run.Status != checkrun.ChecksPassed || run.TraceURL != localTrace || run.Selection == nil || !run.StartedAt.Equal(revisions[0].StartedAt) {
		t.Fatalf("the run after the restart %+v", run)
	}
	if got := r.links(t); got != linkList("", localTrace) {
		t.Fatalf("start record revisions after the restart carry %q", got)
	}
	if entry := r.entry(t); entry.State != checkrun.ChecksPassed || entry.TraceURL != localTrace {
		t.Fatalf("feed entry after the restart %+v", entry)
	}
}

func TestServiceFailedUnitChecksKeepTheLatestLinkThatWasPrinted(t *testing.T) {
	t.Run("a rerun that prints no link", func(t *testing.T) {
		installFakeDagger(t, map[string][]string{"local": {fakeLink(localTrace, false) + fakeFails, fakeFails}})
		r := newLinkedChecksService(t, "trace-failed", localChecks(cloudToken), "")
		r.allow()
		run := r.awaitRun(t)
		if run.Status != checkrun.ChecksFailed || run.TraceURL != localTrace || len(run.Failed) != 1 {
			t.Fatalf("recorded run %+v", run)
		}
		if entry := r.entry(t); entry.State != checkrun.ChecksFailed || entry.TraceURL != localTrace {
			t.Fatalf("feed entry %+v", entry)
		}
	})
	t.Run("a rerun that prints another", func(t *testing.T) {
		installFakeDagger(t, map[string][]string{"local": {fakeLink(localTrace, false) + fakeFails, fakeLink(lateTrace, false) + fakeFails}})
		r := newLinkedChecksService(t, "trace-failed-later", localChecks(cloudToken), "")
		r.allow()
		run := r.awaitRun(t)
		if run.Status != checkrun.ChecksFailed || run.TraceURL != lateTrace {
			t.Fatalf("recorded run %+v", run)
		}
		if got := r.links(t); got != linkList("", localTrace, lateTrace) {
			t.Fatalf("start record revisions carry %q", got)
		}
		if entry := r.entry(t); entry.TraceURL != lateTrace {
			t.Fatalf("feed entry %+v", entry)
		}
	})
}

func TestServicePassedUnitChecksCarryTheirLink(t *testing.T) {
	installFakeDagger(t, map[string][]string{"local": {fakeLink(localTrace, true) + fakePasses}})
	r := newLinkedChecksService(t, "trace-passed", localChecks(cloudToken), "")
	r.allow()
	run := r.awaitRun(t)
	if run.Status != checkrun.ChecksPassed || run.TraceURL != localTrace {
		t.Fatalf("recorded run %+v", run)
	}
	if entry := r.entry(t); entry.State != checkrun.ChecksPassed || entry.TraceURL != localTrace {
		t.Fatalf("feed entry %+v", entry)
	}
}

func TestServiceUnitChecksCarryTheirLinkInEveryEngineMode(t *testing.T) {
	for name, tc := range map[string]struct {
		checks checkrun.DaggerChecks
		engine string
	}{
		"auto":           {checkrun.DaggerChecks{CloudToken: cloudToken, Engine: "auto"}, "local"},
		"empty":          {localChecks(cloudToken), "local"},
		"runner address": {checkrun.DaggerChecks{CloudToken: cloudToken, Engine: "tcp://127.0.0.1:9"}, "address"},
		"cloud":          {cloudChecks(cloudToken), "cloud"},
	} {
		t.Run(name, func(t *testing.T) {
			installFakeDagger(t, map[string][]string{tc.engine: {fakeLink(localTrace, true) + fakePasses}})
			r := newLinkedChecksService(t, "trace-engine", tc.checks, "")
			r.allow()
			if run := r.awaitRun(t); run.Status != checkrun.ChecksPassed || run.TraceURL != localTrace {
				t.Fatalf("recorded run %+v", run)
			}
			if entry := r.entry(t); entry.TraceURL != localTrace {
				t.Fatalf("feed entry %+v", entry)
			}
		})
	}
}

func TestServiceUnitChecksLinkToTheCloudRunAfterTheFallback(t *testing.T) {
	for name, tc := range map[string]struct {
		cloud   string
		timeout string
		status  string
		want    string
		links   string
	}{
		"cloud passes":      {fakeLink(cloudTrace, false) + fakePasses, "", checkrun.ChecksPassed, cloudTrace, linkList("", localTrace, cloudTrace)},
		"cloud times out":   {fakeLink(cloudTrace, false) + fakeHangs, "2s", checkrun.ChecksIncomplete, cloudTrace, linkList("", localTrace, cloudTrace)},
		"cloud prints none": {fakePasses, "", checkrun.ChecksPassed, localTrace, linkList("", localTrace)},
	} {
		t.Run(name, func(t *testing.T) {
			installFakeDagger(t, map[string][]string{
				"local": {fakeLink(localTrace, true) + fakeEngineLost},
				"cloud": {tc.cloud},
			})
			r := newFallbackLinkedChecksService(t, "trace-fallback", localChecks(cloudToken), tc.timeout)
			r.allow()
			run := r.awaitRun(t)
			if run.Status != tc.status || len(run.Engines.CloudRuns()) != 1 || run.TraceURL != tc.want {
				t.Fatalf("recorded run %+v engines %+v", run, run.Engines)
			}
			if got := r.links(t); got != tc.links {
				t.Fatalf("start record revisions carry %q, want %q", got, tc.links)
			}
			if entry := r.entry(t); entry.State != tc.status || entry.TraceURL != tc.want {
				t.Fatalf("feed entry %+v", entry)
			}
		})
	}
}

func TestServiceUnitChecksWithoutATokenRecordNoLink(t *testing.T) {
	installFakeDagger(t, map[string][]string{"local": {"echo 'Visit https://dagger.cloud/example/traces/prompt999 to set up' >&2\n" + fakePasses}})
	r := newLinkedChecksService(t, "trace-no-token", localChecks(""), "")
	r.allow()
	run := r.awaitRun(t)
	if run.Status != checkrun.ChecksPassed || run.TraceURL != "" || (run.Evidence != nil && len(run.Evidence.Traces) > 0) {
		t.Fatalf("recorded run %+v", run)
	}
	if got := r.links(t); got != "" {
		t.Fatalf("start record revisions carry %q", got)
	}
	if feed := r.feedJSON(t); strings.Contains(feed, "trace_url") || strings.Contains(feed, "prompt999") {
		t.Fatalf("the feed shows a link or a note: %s", feed)
	}
	docs, err := trace.Read[trace.Document](r.repo(), r.stream)
	fixturekit.Must(t, err)
	for _, d := range docs {
		if (strings.HasSuffix(d.Path, "-selection.json") || strings.HasSuffix(d.Path, "checks-1.json")) && strings.Contains(d.Content, "trace_url") {
			t.Fatalf("%s carries a trace_url: %s", d.Path, d.Content)
		}
	}
}
