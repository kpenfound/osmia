package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
)

func recoveryEntry(unit string, version uint64) wire.InboxEntry {
	move := wire.RecoveryMove{Occurrence: "occurrence-" + unit, Machine: "unit", Subject: "unit-" + unit, Unit: unit, From: "implementing", Version: version, Target: "ready", Origin: wire.RecoveryStall, Reason: "stalled"}
	return wire.InboxEntry{
		Kind: wire.InboxRecovery, Project: "p", Workstream: stream, Number: 1, Unit: unit, Recovery: &move,
		Answer: wire.InboxAnswer{Method: "POST", Path: wire.Prefix + "/recovery/" + stream, Body: map[string]any{
			"occurrence": move.Occurrence, "subject": move.Subject, "unit": unit, "from": move.From, "version": move.Version, "target": move.Target,
		}},
	}
}

func recoveryServer(t *testing.T, entries ...wire.InboxEntry) (root, socket string, requests func() []wire.RecoveryRequest) {
	t.Helper()
	root = t.TempDir()
	socket = filepath.Join(root, "fixture.sock")
	listener, err := net.Listen("unix", socket)
	must(t, err)
	t.Cleanup(func() { listener.Close() })
	var mu sync.Mutex
	var received []wire.RecoveryRequest
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == wire.Prefix+"/inbox":
			json.NewEncoder(w).Encode(wire.InboxResponse{Entries: entries})
		case r.Method == http.MethodPost && r.URL.Path == wire.Prefix+"/recovery/"+stream:
			var req wire.RecoveryRequest
			must(t, json.NewDecoder(r.Body).Decode(&req))
			mu.Lock()
			received = append(received, req)
			mu.Unlock()
			json.NewEncoder(w).Encode(wire.RecoveryResponse{Workstream: stream, Occurrence: req.Occurrence, Subject: req.Subject, Unit: req.Unit, From: req.From, Target: req.Target, Detail: "The move was applied."})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return root, socket, func() []wire.RecoveryRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]wire.RecoveryRequest(nil), received...)
	}
}

func TestRecoverCommandApprovesTheMoveTheCardPins(t *testing.T) {
	t.Parallel()
	root, socket, requests := recoveryServer(t, recoveryEntry("a", 7))
	code, out, diag := invoke(t, root, "--socket", socket, "recover", stream)
	if code != 0 || diag != "" || !strings.Contains(out, "Approved the recovery move of "+stream+": unit-a from implementing to ready") {
		t.Fatalf("code=%d out=%q diag=%q", code, out, diag)
	}
	got := requests()
	want := wire.RecoveryRequest{Occurrence: "occurrence-a", Subject: "unit-a", Unit: "a", From: "implementing", Version: 7, Target: "ready"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("the approval did not pin the card's move: %+v", got)
	}
}

func TestRecoverCommandApprovesNothingWithoutExactlyOneCard(t *testing.T) {
	t.Parallel()
	root, socket, _ := recoveryServer(t)
	code, out, diag := invoke(t, root, "--socket", socket, "recover", stream)
	if code != 1 || out != "" || !strings.Contains(diag, "no recovery move of workstream "+stream+" waits for you") {
		t.Fatalf("no card: %d %q %q", code, out, diag)
	}
	root, socket, requests := recoveryServer(t, recoveryEntry("a", 3), recoveryEntry("b", 4))
	code, out, diag = invoke(t, root, "--socket", socket, "recover", stream)
	if code != 1 || out != "" || !strings.Contains(diag, "2 recovery moves of workstream "+stream+" wait for you") || !strings.Contains(diag, "a") {
		t.Fatalf("two cards: %d %q %q", code, out, diag)
	}
	if len(requests()) != 0 {
		t.Fatalf("an ambiguous command approved a move: %+v", requests())
	}
	code, _, diag = invoke(t, root, "--socket", socket, "recover", stream, "b")
	if code != 0 || diag != "" {
		t.Fatalf("naming the unit: %d %q", code, diag)
	}
	if got := requests(); len(got) != 1 || got[0].Unit != "b" || got[0].Version != 4 {
		t.Fatalf("the named card was not the one approved: %+v", got)
	}
	for _, args := range [][]string{{"recover"}, {"recover", stream, "a", "extra"}} {
		if code, _, _ := invoke(t, root, append([]string{"--socket", socket}, args...)...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
