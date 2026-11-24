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

type landServer struct {
	root string
	sent chan wire.DeliveryLandDecision
	path *string
}

func startLandServer(t *testing.T) landServer {
	t.Helper()
	opts := fixture(t)
	root := opts.Config.Root
	ln, err := net.Listen("unix", filepath.Join(root, "fake.sock"))
	must(t, err)
	l := landServer{root: root, sent: make(chan wire.DeliveryLandDecision, 1), path: new(string)}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(wire.DeliveryPresentation{Report: wire.FinalReport{Review: 1, Commit: "abc"}, ReviewRevision: 2, DraftHash: "hash", CanLand: true, Recommendation: "land"})
			return
		}
		*l.path = r.URL.Path
		var decision wire.DeliveryLandDecision
		if err := json.NewDecoder(r.Body).Decode(&decision); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		l.sent <- decision
		json.NewEncoder(w).Encode(wire.DeliveryLandResponse{Outcome: wire.LandOutcomeLanded, Commit: "abc"})
	})}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	return l
}

func TestDeliveryNamesTheLandCommandAndTheRecommendation(t *testing.T) {
	l := startLandServer(t)
	code, out, diag := invoke(t, l.root, "delivery", stream, "--socket", "fake.sock")
	if code != 0 || !strings.Contains(out, "osmia land "+stream) || !strings.Contains(out, "Jev recommends: land\n") {
		t.Fatalf("delivery: %d %q %s", code, out, diag)
	}
}

func TestLandAcceptReportsTheLandedCommit(t *testing.T) {
	l := startLandServer(t)
	code, out, diag := invoke(t, l.root, "land", stream, "--accept", "--socket", "fake.sock")
	if code != 0 || !strings.Contains(out, "Landed workstream "+stream) || !strings.Contains(out, "abc") || diag != "" {
		t.Fatalf("land: %d %q %s", code, out, diag)
	}
}

func TestLandSendsTheDecisionAsPresented(t *testing.T) {
	l := startLandServer(t)
	successful(t, l.root, "land", stream, "--accept", "--socket", "fake.sock")
	decision := <-l.sent
	if want := (wire.DeliveryLandDecision{Review: 1, ReviewRevision: 2, Commit: "abc", DraftHash: "hash", AcceptBlocking: true}); decision != want || *l.path != "/v1/delivery/"+stream+"/land" {
		t.Fatalf("the decision %+v sent to %s, want %+v", decision, *l.path, want)
	}
}

func TestLandJSONReportsTheOutcome(t *testing.T) {
	l := startLandServer(t)
	code, out, _ := invoke(t, l.root, "land", stream, "--json", "--socket", "fake.sock")
	if code != 0 || !strings.Contains(out, `"outcome":"landed"`) {
		t.Fatalf("land --json: %d %q", code, out)
	}
}

func TestLandReportsWhatLandingDid(t *testing.T) {
	for outcome, want := range map[string]string{
		wire.LandOutcomeHeld:      "is held for now and nothing was pushed",
		wire.LandOutcomeFellBack:  "was marked ready for review",
		wire.LandOutcomeBaseMoved: "lands on the new commit by itself",
		wire.LandOutcomePending:   "is not known yet",
	} {
		var out strings.Builder
		showLanding(&out, "w_x", wire.DeliveryLandResponse{Outcome: outcome, Commit: "abc", Reason: "because"})
		if !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "because") {
			t.Errorf("%s: %q does not say %q", outcome, out.String(), want)
		}
	}
}

func TestLandRefusesBadArguments(t *testing.T) {
	opts := fixture(t)
	for _, args := range [][]string{{"land"}, {"land", stream, "extra"}} {
		if code, _, _ := invoke(t, opts.Config.Root, args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func TestInboxOffersApproveAndLandOnlyOnAProjectThatLands(t *testing.T) {
	entry := wire.InboxEntry{Kind: wire.InboxDelivery, Workstream: stream, Revision: 2, Options: []string{"approve"}, Answer: wire.InboxAnswer{Method: "POST", Path: "/v1/delivery/" + stream, Body: map[string]any{"review": 1, "commit": "abc"}}}
	var out strings.Builder
	showInbox(&out, wire.InboxResponse{Entries: []wire.InboxEntry{entry}})
	if strings.Contains(out.String(), "osmia land") {
		t.Fatalf("a project that delivers by pull request is offered landing:\n%s", out.String())
	}
	entry.Options = []string{"approve", "land"}
	entry.Land = &wire.InboxAnswer{Method: "POST", Path: "/v1/delivery/" + stream + "/land", Body: entry.Answer.Body}
	out.Reset()
	showInbox(&out, wire.InboxResponse{Entries: []wire.InboxEntry{entry}})
	if !strings.Contains(out.String(), "osmia land "+stream+" to approve and land it on the base branch") || !strings.Contains(out.String(), "Options: approve, land") {
		t.Fatalf("the inbox does not offer landing:\n%s", out.String())
	}
}
