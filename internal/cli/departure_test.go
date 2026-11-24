package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "github.com/kpenfound/osmia/internal/api"
)

func historyEvent(id, summary, decidedBy string, departed bool) wire.TraceEvent {
	return wire.TraceEvent{Ref: wire.TraceRef{Kind: "transition", ID: id, Revision: 1}, Summary: summary, DecidedBy: decidedBy, DefaultTo: "reviewing", Departed: departed}
}

func TestTraceLabelsOnlyTransitionsJevDepartedOn(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	socket := filepath.Join(root, "fixture.sock")
	listener, err := net.Listen("unix", socket)
	must(t, err)
	t.Cleanup(func() { listener.Close() })
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != wire.Prefix+"/trace/"+stream+"/unit/a" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(wire.UnitTrace{Workstream: stream, Unit: "a", Feature: "building", History: []wire.TraceEvent{
			historyEvent("departed", "unit-a: checking -> implementing: sent back", "jev", true),
			historyEvent("followed", "unit-a: implementing -> checking: reported", "code", false),
			historyEvent("fallback", "unit-a: checking -> reviewing: checks passed", "fallback", false),
			historyEvent("pinned", "unit-a: reviewing -> approved: owner approved", "owner", false),
			historyEvent("gate", "unit-a: approved -> merged: gate", "jev_gate", false),
			historyEvent("verdict", "unit-a: reviewing -> approved: satisfactory", "reviewer", false),
			historyEvent("unrouted", "unit-a: planned -> ready: dependencies merged", "", false),
		}})
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	code, out, diag := invoke(t, root, "--socket", socket, "trace", stream, "unit", "a")
	if code != 0 || diag != "" {
		t.Fatalf("code=%d out=%s err=%s", code, out, diag)
	}
	if !strings.Contains(out, "    unit-a: checking -> implementing: sent back (decided by Jev)\n") {
		t.Fatalf("the departure is not labelled:\n%s", out)
	}
	if strings.Count(out, "decided by Jev") != 1 {
		t.Fatalf("a transition other than the departure is labelled:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "unit-a: ") && !strings.Contains(line, "sent back") && strings.Contains(line, "(") {
			t.Fatalf("an extra note on %q", line)
		}
	}
}

func TestFeedLabelsOnlyTransitionsJevDepartedOn(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	showFeed(&out, []wire.FeedEntry{
		{Kind: "transition", At: at, Transition: &wire.FeedTransition{Subject: "unit-a", From: "checking", To: "implementing", DecidedBy: "jev", DefaultTo: "reviewing", Departed: true}},
		{Kind: "transition", At: at, Transition: &wire.FeedTransition{Subject: "unit-a", From: "checking", To: "reviewing", DecidedBy: "code", DefaultTo: "reviewing"}},
		{Kind: "transition", At: at, Transition: &wire.FeedTransition{Subject: "unit-a", From: "reviewing", To: "approved", DecidedBy: "fallback", DefaultTo: "approved"}},
		{Kind: "transition", At: at, Transition: &wire.FeedTransition{Subject: "feature", From: "", To: "handed", DecidedBy: "owner", DefaultTo: "handed"}},
		{Kind: "transition", At: at, Transition: &wire.FeedTransition{Subject: "feature", From: "handed", To: "ratified", DecidedBy: "jev_gate", DefaultTo: "ratified"}},
	})
	text := out.String()
	if !strings.Contains(text, "  unit-a: checking -> implementing (decided by Jev)\n") {
		t.Fatalf("the departure is not labelled:\n%s", text)
	}
	if strings.Count(text, "decided by Jev") != 1 || !strings.Contains(text, "  unit-a: checking -> reviewing\n") || !strings.Contains(text, "  feature: none -> handed\n") {
		t.Fatalf("feed output:\n%s", text)
	}
}
