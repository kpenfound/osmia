package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/workspace"
)

func TestApproveSendsThePresentedDescriptionAndMessages(t *testing.T) {
	opts := fixture(t)
	root := opts.Config.Root
	ln, err := net.Listen("unix", filepath.Join(root, "fake.sock"))
	must(t, err)
	messages := []workspace.DeliveryMessage{{Commit: "abc", Message: "Draft"}, {Commit: "def", Message: "Second draft"}}
	sent := make(chan wire.DeliveryDecision, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(wire.DeliveryPresentation{Report: wire.FinalReport{Review: 1, Commit: "abc"}, ReviewRevision: 2, DraftHash: "hash", Draft: "Presented description", Messages: messages})
			return
		}
		var decision wire.DeliveryDecision
		if err := json.NewDecoder(r.Body).Decode(&decision); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		sent <- decision
		json.NewEncoder(w).Encode(wire.DeliveryApproval{Review: 1, Commit: "abc", Messages: decision.Messages})
	})}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	code, out, diag := invoke(t, root, "approve", stream, "--socket", "fake.sock")
	if code != 0 || !strings.Contains(out, "ready for review") {
		t.Fatalf("approve: %d %q %s", code, out, diag)
	}
	decision := <-sent
	if decision.Description == nil || *decision.Description != "Presented description" {
		t.Fatalf("description was not pinned: %+v", decision)
	}
	if len(decision.Messages) != len(messages) || decision.Messages[0].Commit != "abc" || decision.Messages[0].Message != "Draft" || decision.ReviewRevision != 2 || decision.DraftHash != "hash" {
		t.Fatalf("decision: %+v", decision)
	}
}

func TestApproveRefusesTheRemovedMessageFlags(t *testing.T) {
	opts := fixture(t)
	for _, flag := range []string{"--message-file", "--messages-file"} {
		if code, _, _ := invoke(t, opts.Config.Root, "approve", stream, flag, "messages.txt"); code == 0 {
			t.Fatalf("approve accepted %s", flag)
		}
	}
}

func TestDeliveryShowsTheCheckResult(t *testing.T) {
	opts := fixture(t)
	root := opts.Config.Root
	ln, err := net.Listen("unix", filepath.Join(root, "fake.sock"))
	must(t, err)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(wire.DeliveryPresentation{Report: wire.FinalReport{Review: 1, Commit: "abc"}, Draft: "Description", Checks: &wire.DeliveryChecks{State: wire.ChecksStateFailed, Summary: "Checks failed: go-test"}})
	})}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	code, out, diag := invoke(t, root, "delivery", stream, "--socket", "fake.sock")
	if code != 0 || !strings.Contains(out, "Checks: Checks failed: go-test\n") {
		t.Fatalf("delivery: %d %q %s", code, out, diag)
	}
}
