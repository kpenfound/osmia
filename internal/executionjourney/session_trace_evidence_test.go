package executionjourney

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	sessionTraceToken = "OSMIA_SESSION_TRACE_TEST_TOKEN"
	sessionTraceA     = "https://dagger.cloud/example/traces/aaaa1111"
	sessionTraceB     = "https://dagger.cloud/example/traces/bbbb2222"
	sessionTraceC     = "https://dagger.cloud/example/traces/cccc3333"
)

// installTraceDagger puts a fake dagger CLI on PATH whose `dagger check`
// behaves as the mode file next to it says, and returns the function that
// sets the mode. It prints Dagger's trace link line on stderr, with or
// without a Cloud token, the way a setup prompt would.
func installTraceDagger(t *testing.T) func(mode string) {
	t.Helper()
	dir := t.TempDir()
	modeFile := filepath.Join(dir, "mode")
	script := fmt.Sprintf(`#!/bin/sh
set -u
case "$1" in
 version) echo 'dagger v0.20.5'; exit 0 ;;
 list) printf 'dag+check://go/test\n'; exit 0 ;;
 check) ;;
 *) exit 94 ;;
esac
link() { echo "View trace at https://dagger.cloud/example/traces/$1" >&2; }
case "$(cat %q)" in
 block) link aaaa1111; exec sleep 600 ;;
 pass)
  link aaaa1111
  link bbbb2222
  printf '\nCHECKS  ✔ 1 passed\n✔ dag+check://go/test 1s OK\n' ;;
 fail)
  link aaaa1111
  printf '! exit code 1\n\nCHECKS  ✘ 1 failed\n✘ dag+check://go/test 1.0s ERROR\n'
  exit 1 ;;
 fallback)
  if [ "${DAGGER_ENGINE-}" = cloud ]; then link cccc3333; else link aaaa1111; fi
  echo 'Error: the engine session was lost'
  exit 1 ;;
esac
`, modeFile)
	must(t, os.WriteFile(filepath.Join(dir, "dagger"), []byte(script), 0700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func(mode string) { must(t, os.WriteFile(modeFile, []byte(mode), 0600)) }
}

// sessionTraceFixture is a session service whose checks run the fake dagger
// CLI through the production composition. A token configures the Dagger
// Cloud token the checks receive; dagger holds further keys of the [dagger]
// table.
func sessionTraceFixture(t *testing.T, token, dagger string) (*Service, *Client, Options, config.ProjectID, config.WorkstreamID) {
	t.Helper()
	extra := ""
	if token != "" {
		t.Setenv(sessionTraceToken, token)
		extra = "\n[dagger]\ncloud_compute_token_env = '" + sessionTraceToken + "'\n" + dagger
	}
	s, c, opts, project, w, _, _ := newSessionControlsFixture(t, func(o *Options) {
		if extra != "" {
			appendConfig(t, o, extra)
		}
	})
	service.InspectForTest(s.core()).SetReviewChecks(nil)
	return s, c, opts, project, w
}

func sessionFeedRaw(t *testing.T, c *Client, w config.WorkstreamID) []byte {
	t.Helper()
	var raw json.RawMessage
	must(t, c.Do(t.Context(), "GET", api.Prefix+"/feed/"+string(w), nil, &raw))
	return raw
}

// sessionCheckEntries returns the feed's session_checks entries by run.
func sessionCheckEntries(t *testing.T, c *Client, w config.WorkstreamID) map[int]api.FeedSessionChecks {
	t.Helper()
	var feed api.FeedResponse
	must(t, json.Unmarshal(sessionFeedRaw(t, c, w), &feed))
	out := map[int]api.FeedSessionChecks{}
	for _, e := range feed.Entries {
		if e.SessionChecks != nil {
			if _, dup := out[e.SessionChecks.Run]; dup {
				t.Fatalf("two feed entries for session check run %d", e.SessionChecks.Run)
			}
			out[e.SessionChecks.Run] = *e.SessionChecks
		}
	}
	return out
}

func awaitSessionCheckEntry(t *testing.T, c *Client, w config.WorkstreamID, run int, ok func(api.FeedSessionChecks) bool) api.FeedSessionChecks {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if e, found := sessionCheckEntries(t, c, w)[run]; found && ok(e) {
			return e
		}
		if time.Now().After(deadline) {
			t.Fatalf("no matching session_checks entry for run %d: %s", run, sessionFeedRaw(t, c, w))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func sessionTraceDocuments(t *testing.T, s *Service, project config.ProjectID, w config.WorkstreamID) map[string]trace.Document {
	t.Helper()
	repo, err := s.repository(project)
	must(t, err)
	docs, err := trace.Read[trace.Document](repo, w)
	must(t, err)
	out := map[string]trace.Document{}
	for _, d := range docs {
		if strings.HasPrefix(d.Path, "session-checks/") && d.Revision >= out[d.Path].Revision {
			out[d.Path] = d
		}
	}
	return out
}

// Serial: the fake dagger CLI is on the process PATH.
func TestSessionCheckLinkOfAnInterruptedRunStaysInTheFeedAcrossRestart(t *testing.T) {
	mode := installTraceDagger(t)
	s, c, opts, project, w := sessionTraceFixture(t, "session-trace-secret", "")
	mode("block")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.SessionCheck(ctx, w); done <- err }()
	awaitSessionCheckEntry(t, c, w, 1, func(e api.FeedSessionChecks) bool { return e.TraceURL == sessionTraceA })
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled request returned a result")
	}
	must(t, s.Close())
	s, c = start(t, opts)
	service.InspectForTest(s.core()).SetReviewChecks(nil)

	interrupted := sessionCheckEntries(t, c, w)
	first, found := interrupted[1]
	if len(interrupted) != 1 || !found || first.TraceURL != sessionTraceA || first.State != "running" || first.FinishedAt != nil {
		t.Fatalf("the interrupted run after a restart: %s", sessionFeedRaw(t, c, w))
	}
	docs := sessionTraceDocuments(t, s, project, w)
	if _, finished := docs["session-checks/checks-1.json"]; finished || docs["session-checks/checks-1-started.json"].Path == "" {
		t.Fatalf("records of the interrupted run: %v", docs)
	}
	mode("pass")
	run, err := c.SessionCheck(t.Context(), w)
	must(t, err)
	if run.Run != 2 || run.Status != ChecksPassed || run.TraceURL != sessionTraceB {
		t.Fatalf("the run after the restart: %+v", run)
	}
	entries := sessionCheckEntries(t, c, w)
	if len(entries) != 2 || entries[1].TraceURL != sessionTraceA || entries[1].State != "running" || entries[1].FinishedAt != nil || entries[1].StartedAt != first.StartedAt {
		t.Fatalf("the interrupted run's entry changed: %s", sessionFeedRaw(t, c, w))
	}
	if e := entries[2]; e.State != ChecksPassed || e.TraceURL != sessionTraceB || e.FinishedAt == nil {
		t.Fatalf("the new run's entry: %+v", e)
	}
}

// Serial: the fake dagger CLI is on the process PATH.
func TestSessionCheckResultCarriesTheLatestPrintedLinkAndSupersedesTheEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, mode, dagger, status, link string
	}{
		{"passed", "pass", "", ChecksPassed, sessionTraceB},
		{"failed", "fail", "", ChecksFailed, sessionTraceA},
		{"incomplete after the cloud fallback", "fallback", "check_fallback = 'cloud'\n", ChecksIncomplete, sessionTraceC},
		{"incomplete without the cloud fallback", "fallback", "", ChecksIncomplete, sessionTraceA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode := installTraceDagger(t)
			s, c, _, project, w := sessionTraceFixture(t, "session-trace-secret", tc.dagger)
			mode(tc.mode)
			run, err := c.SessionCheck(t.Context(), w)
			must(t, err)
			if run.Status != tc.status || run.TraceURL != tc.link {
				t.Fatalf("result: %+v", run)
			}
			docs := sessionTraceDocuments(t, s, project, w)
			var stored api.SessionCheckRun
			must(t, json.Unmarshal([]byte(docs["session-checks/checks-1.json"].Content), &stored))
			if stored.TraceURL != tc.link || stored.Status != tc.status {
				t.Fatalf("result record: %+v", stored)
			}
			entries := sessionCheckEntries(t, c, w)
			if e := entries[1]; len(entries) != 1 || e.State != tc.status || e.TraceURL != tc.link || e.FinishedAt == nil {
				t.Fatalf("feed after the result: %s", sessionFeedRaw(t, c, w))
			}
		})
	}
}

// Serial: the fake dagger CLI is on the process PATH.
func TestSessionCheckWithoutACloudTokenRecordsNoLink(t *testing.T) {
	mode := installTraceDagger(t)
	s, c, _, project, w := sessionTraceFixture(t, "", "")
	mode("pass")
	run, err := c.SessionCheck(t.Context(), w)
	must(t, err)
	if run.Status != ChecksPassed || run.TraceURL != "" {
		t.Fatalf("result: %+v", run)
	}
	for path := range sessionTraceDocuments(t, s, project, w) {
		if strings.Contains(path, "started") {
			t.Fatalf("unfinished evidence without a token: %s", path)
		}
	}
	if raw := string(sessionFeedRaw(t, c, w)); strings.Contains(raw, "trace_url") || strings.Contains(raw, "dagger.cloud") {
		t.Fatalf("feed without a token: %s", raw)
	}
}

func TestSessionCheckResultIsRecordedWhenTheRequestIsCancelledAfterTheRunEnds(t *testing.T) {
	t.Parallel()
	s, c, _, _, w, _, _ := newSessionControlsFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	service.InspectForTest(s.core()).SetReviewChecks(checkFunc(func(checkCtx context.Context, dir string) (CheckResult, error) {
		// The run finishes cleanly, but only once the cancellation has reached
		// the service: the result is written with the request already gone.
		cancel()
		<-checkCtx.Done()
		return CheckResult{Output: passedReport}, nil
	}))
	if _, err := c.SessionCheck(ctx, w); err == nil {
		t.Fatal("a cancelled request returned a result")
	}
	e := awaitSessionCheckEntry(t, c, w, 1, func(e api.FeedSessionChecks) bool { return e.State == ChecksPassed })
	if e.FinishedAt == nil {
		t.Fatalf("entry: %+v", e)
	}
}
