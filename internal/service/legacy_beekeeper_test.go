package service

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// TestStartOnFreshRootCreatesNoBeekeeperDirectory shows that starting the
// service on a root that has never held a Beekeeper creates no "beekeeper"
// directory and no shadow project: the removed feature leaves nothing
// behind on a fresh install.
func TestStartOnFreshRootCreatesNoBeekeeperDirectory(t *testing.T) {
	t.Parallel()
	opts, _ := projectFixture(t)
	start(t, opts)
	if _, err := os.Stat(filepath.Join(opts.Config.Root, "beekeeper")); !os.IsNotExist(err) {
		t.Fatalf("a fresh root got a beekeeper directory: %v", err)
	}
}

// TestStartWithAPreexistingBeekeeperDirectoryLeavesItUntouched shows that
// starting the service on a root that already holds a "beekeeper"
// directory, left behind by an earlier version, starts normally and leaves
// that directory's contents byte-for-byte unchanged: the service neither
// reads it nor deletes it.
func TestStartWithAPreexistingBeekeeperDirectoryLeavesItUntouched(t *testing.T) {
	t.Parallel()
	opts, _ := projectFixture(t)
	dir := filepath.Join(opts.Config.Root, "beekeeper")
	must(t, os.MkdirAll(filepath.Join(dir, "workstreams", "w_bebebebebebebebebebebebebebebebe"), 0700))
	must(t, os.WriteFile(filepath.Join(dir, "project.json"), []byte(`{"legacy":"beekeeper state"}`), 0600))
	must(t, os.WriteFile(filepath.Join(dir, "workstreams", "w_bebebebebebebebebebebebebebebebe", "workstream.json"), []byte(`{"legacy":"thread"}`), 0600))
	before := snapshot(t, dir)

	start(t, opts)

	if after := snapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("beekeeper directory changed:\nbefore %v\nafter  %v", before, after)
	}
}

// TestTopLevelConfigurationWithABeekeeperSectionStartsTheService shows that
// a top-level configuration still carrying a [beekeeper] section, left
// behind from an earlier version, loads without error and the service
// starts with it (spec#4): the section has no effect.
func TestTopLevelConfigurationWithABeekeeperSectionStartsTheService(t *testing.T) {
	t.Parallel()
	opts, _ := projectFixture(t)
	path := filepath.Join(opts.Config.Root, "config.toml")
	top, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(top, []byte("[beekeeper]\nname = \"Hive\"\nprofile = \"default\"\nsandbox = \"none\"\n")...), 0600))

	_, c := start(t, opts)
	health, err := c.Health(context.Background())
	must(t, err)
	if !health.Ready {
		t.Fatalf("service did not start ready with a leftover [beekeeper] section: %+v", health)
	}
}

// TestLegacyBeekeeperAttributedTurnReadsThroughEveryView builds a registered
// project's trace holding an ordinary owner chief-of-staff exchange and a
// chief-of-staff turn whose request actor has the legacy "beekeeper" kind,
// and shows that the status, feed, trace and conversation views all read it
// without error (spec#5). The legacy turn keeps its Beekeeper attribution,
// the trace machinery still returns it, and it appears in the feed as it
// did before the change: a chief-of-staff turn the owner didn't send, shown
// as a session rather than a message. The conversation view keeps its
// existing owner-only filtering and still shows the owner's own message and
// response.
func TestLegacyBeekeeperAttributedTurnReadsThroughEveryView(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts, _ := conversationFixture(t, "lb-")
	s, _ := start(t, opts)
	repo := s.sole().repository
	at := func(seconds int) time.Time { return demoStart.Add(time.Duration(seconds) * time.Second) }
	profile := coreadapter.Profile{Name: "default", Backend: "fake", Model: "test"}

	_, err := repo.EnsureChiefOfStaff(ctx, stream, at(0), serviceActor)
	must(t, err)

	// An ordinary owner exchange, completed.
	ownerReq := trace.TurnRequest{
		Header:  trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_owner_1", Revision: 1, Project: project, Workstream: stream, At: at(1), Actor: ownerActor, Cause: "owner-message"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "message_owner_1", Profile: profile, Prompt: "Where do we stand?",
	}
	_, err = repo.EnqueueTurn(ctx, ownerReq)
	must(t, err)
	ownerClaim, err := repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token_owner_1", t.TempDir(), at(2))
	must(t, err)
	ownerResp := trace.TurnResponse{
		Header:  trace.Header{Schema: "osmia.trace.turn-response", Version: trace.Version, ID: "response_owner_1", Revision: 1, Project: project, Workstream: stream, At: at(3), Actor: serviceActor, Cause: "owner-message"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: ownerReq.TurnID, RequestID: ownerReq.ID, RequestRevision: 1,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session-owner"}, SessionDirectory: ownerClaim.Claim.SessionDirectory, StartedAt: at(2), Duration: time.Second, FinalResponse: "All good."},
	}
	must(t, repo.CaptureTurn(ctx, ownerClaim.Claim.Token, ownerResp))
	must(t, repo.CompleteTurn(ctx, stream, trace.ChiefOfStaff, ownerReq.TurnID, ownerClaim.Claim.Token, at(4)))

	// A legacy chief-of-staff turn an earlier version attributed to the
	// retired Beekeeper, completed the same way.
	legacyBeekeeperActor := trace.Actor{Kind: "beekeeper", ID: "beekeeper"}
	beekeeperReq := trace.TurnRequest{
		Header:  trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_beekeeper_1", Revision: 1, Project: project, Workstream: stream, At: at(5), Actor: legacyBeekeeperActor, Cause: "beekeeper-message"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "bk_legacy_1", Profile: profile, Prompt: "List the units.",
	}
	_, err = repo.EnqueueTurn(ctx, beekeeperReq)
	must(t, err)
	beekeeperClaim, err := repo.ClaimTurn(ctx, stream, trace.ChiefOfStaff, "token_beekeeper_1", t.TempDir(), at(6))
	must(t, err)
	if beekeeperClaim.Request.TurnID != beekeeperReq.TurnID {
		t.Fatalf("claimed the wrong turn: %+v", beekeeperClaim)
	}
	beekeeperResp := trace.TurnResponse{
		Header:  trace.Header{Schema: "osmia.trace.turn-response", Version: trace.Version, ID: "response_beekeeper_1", Revision: 1, Project: project, Workstream: stream, At: at(7), Actor: serviceActor, Cause: "beekeeper-message"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: beekeeperReq.TurnID, RequestID: beekeeperReq.ID, RequestRevision: 1,
		Result: coreadapter.SessionResult{Session: coreadapter.BackendSession{Backend: "fake", ID: "session-beekeeper"}, SessionDirectory: beekeeperClaim.Claim.SessionDirectory, StartedAt: at(6), Duration: time.Second, FinalResponse: "Three units are implementing."},
	}
	must(t, repo.CaptureTurn(ctx, beekeeperClaim.Claim.Token, beekeeperResp))
	must(t, repo.CompleteTurn(ctx, stream, trace.ChiefOfStaff, beekeeperReq.TurnID, beekeeperClaim.Claim.Token, at(8)))

	// The trace machinery still returns the turn, with its legacy
	// attribution kept exactly.
	th, err := repo.ChiefOfStaffThread(stream)
	must(t, err)
	var found *trace.QueuedTurn
	for i := range th.Turns {
		if th.Turns[i].Request.TurnID == beekeeperReq.TurnID {
			found = &th.Turns[i]
		}
	}
	if found == nil {
		t.Fatal("the trace no longer returns the legacy beekeeper-attributed turn")
	}
	if found.Request.Actor != legacyBeekeeperActor {
		t.Fatalf("legacy beekeeper attribution not kept: %+v", found.Request.Actor)
	}

	// The status, feed, trace and conversation views all read the trace
	// without error.
	s.statusList()
	if _, api := s.workstreamStatus(string(stream)); api != nil {
		t.Fatalf("status view: %v", api)
	}
	feed, api := s.feedList(string(stream))
	if api != nil {
		t.Fatalf("feed view: %v", api)
	}
	if _, api := s.traceView(ctx, string(stream), "", ""); api != nil {
		t.Fatalf("trace view: %v", api)
	}
	conv, api := s.conversationList(string(stream))
	if api != nil {
		t.Fatalf("conversation view: %v", api)
	}

	// The legacy turn appears in the feed as a session entry, as it did
	// before the change: a chief-of-staff turn the owner didn't send.
	var sawBeekeeperSession bool
	for _, e := range feed.Entries {
		if e.Kind == "session" && e.Session != nil && e.Session.Turn == beekeeperReq.TurnID {
			sawBeekeeperSession = true
			if e.Session.State != "done" || e.Session.Summary != "Three units are implementing." {
				t.Fatalf("legacy beekeeper turn in the feed: %+v", e.Session)
			}
		}
	}
	if !sawBeekeeperSession {
		t.Fatalf("the feed dropped the legacy beekeeper-attributed turn: %+v", feed.Entries)
	}

	// The conversation view keeps its existing owner-only filtering: it
	// shows the owner's own message and response, and nothing from the
	// legacy beekeeper-attributed turn.
	if len(conv.Entries) != 2 ||
		conv.Entries[0].Turn != ownerReq.TurnID || conv.Entries[0].Kind != "message" || conv.Entries[0].Text != "Where do we stand?" ||
		conv.Entries[1].Turn != ownerReq.TurnID || conv.Entries[1].Kind != "response" || conv.Entries[1].Text != "All good." {
		t.Fatalf("conversation view: %+v", conv.Entries)
	}
	for _, e := range conv.Entries {
		if e.Turn == beekeeperReq.TurnID {
			t.Fatalf("conversation view leaked the legacy beekeeper-attributed turn: %+v", e)
		}
	}
}
