package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newRegistry(t *testing.T) (*Registry, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)}
	return NewRegistry(Options{Now: c.now, Poll: 300 * time.Millisecond}), c
}

func node(id string) Identity { return Identity{ID: id, Name: "node-" + id} }

func connect(t *testing.T, r *Registry, n Identity, slots int) api.ClientConnectResponse {
	t.Helper()
	resp, err := r.Connect(context.Background(), n, api.ClientConnectRequest{Version: api.ClientProtocolVersion, Slots: slots})
	if err != nil {
		t.Fatalf("connect %s: %v", n.ID, err)
	}
	return resp
}

func apiCode(t *testing.T, err error) (api.Code, string) {
	t.Helper()
	var failure *api.APIError
	if !errors.As(err, &failure) {
		t.Fatalf("got %v, want an API error", err)
	}
	return failure.Code, failure.Message
}

func TestConnectRefusesAnotherProtocolVersionNamingBoth(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	_, err := r.Connect(context.Background(), node("n1"), api.ClientConnectRequest{Version: api.ClientProtocolVersion + 1, Slots: 1})
	code, message := apiCode(t, err)
	server, client := api.ClientProtocolVersion, api.ClientProtocolVersion+1
	if code != api.VersionMismatch || !strings.Contains(message, "version "+strconv.Itoa(server)) || !strings.Contains(message, "version "+strconv.Itoa(client)) {
		t.Fatalf("mismatch refused as %s: %q", code, message)
	}
	if len(r.Clients()) != 0 {
		t.Fatalf("a refused client is registered: %+v", r.Clients())
	}
}

func TestConnectAcceptsAnyNodeAtTheMatchingVersionWithoutATagOrEnrollment(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	for _, id := range []string{"n1", "n2", "tagged-server"} {
		resp := connect(t, r, Identity{ID: id}, 2)
		if resp.Lease == "" || resp.Version != api.ClientProtocolVersion || resp.GraceSeconds != 120 {
			t.Fatalf("connect %s: %+v", id, resp)
		}
	}
	if got := len(r.Clients()); got != 3 {
		t.Fatalf("%d clients registered", got)
	}
}

func TestASecondConnectionFromANodeIsRefusedWhileTheFirstHoldsItsLeaseAndAcceptedAfter(t *testing.T) {
	t.Parallel()
	r, c := newRegistry(t)
	n := node("n1")
	first := connect(t, r, n, 1)
	_, err := r.Connect(context.Background(), n, api.ClientConnectRequest{Version: api.ClientProtocolVersion, Slots: 1})
	if code, _ := apiCode(t, err); code != api.ClientConnected {
		t.Fatalf("a second connection is %s", code)
	}
	c.advance(DefaultGrace - time.Second)
	if _, err := r.Connect(context.Background(), n, api.ClientConnectRequest{Version: api.ClientProtocolVersion, Slots: 1}); err == nil {
		t.Fatal("a second connection is accepted just inside the grace window")
	}
	if got, _ := r.Client(n.ID); got.Lease != first.Lease {
		t.Fatalf("the refused connection replaced the lease: %+v", got)
	}
	c.advance(time.Second)
	second := connect(t, r, n, 1)
	if second.Lease == first.Lease || second.Resumed {
		t.Fatalf("the lease after the first ended: %+v", second)
	}
	if _, err := r.Heartbeat(first.Lease, n, api.HeartbeatRequest{}); err == nil {
		t.Fatal("the ended lease still takes heartbeats")
	} else if code, _ := apiCode(t, err); code != api.LeaseEnded {
		t.Fatalf("heartbeat on an ended lease: %s", code)
	}
}

func TestADifferentNodeConnectsWhileAnotherHoldsALease(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	connect(t, r, node("n1"), 1)
	connect(t, r, node("n2"), 1)
}

func TestALeaseSurvivesMissedHeartbeatsWithinTheGraceWindowAndEndsAfterIt(t *testing.T) {
	t.Parallel()
	r, c := newRegistry(t)
	var ended []string
	r.SetHooks(Hooks{LeaseEnded: func(cl Client, turns []string) { ended = append(ended, cl.Identity.ID+":"+strings.Join(turns, ",")) }})
	n := node("n1")
	lease := connect(t, r, n, 1)
	if got, _ := r.Client(n.ID); got.Health != LeaseHealthy {
		t.Fatalf("a fresh lease is %s", got.Health)
	}
	c.advance(DefaultHeartbeat*lateIntervals + time.Second)
	if got, ok := r.Client(n.ID); !ok || got.Health != LeaseLate {
		t.Fatalf("a lease that missed heartbeats: %+v %v", got, ok)
	}
	c.advance(DefaultGrace - DefaultHeartbeat*lateIntervals - 2*time.Second)
	if _, err := r.Heartbeat(lease.Lease, n, api.HeartbeatRequest{}); err != nil {
		t.Fatalf("a heartbeat within the grace window: %v", err)
	}
	if got, _ := r.Client(n.ID); got.Health != LeaseHealthy {
		t.Fatalf("after the heartbeat the lease is %s", got.Health)
	}
	c.advance(DefaultGrace - time.Second)
	if _, ok := r.Client(n.ID); !ok {
		t.Fatal("the lease ended inside the grace window")
	}
	c.advance(time.Second)
	if _, ok := r.Client(n.ID); ok {
		t.Fatal("the lease outlived the grace window")
	}
	if !slices.Equal(ended, []string{"n1:"}) {
		t.Fatalf("lease ended hook: %v", ended)
	}
}

func claimTurn(t *testing.T, r *Registry, n Identity, lease, turn string) {
	t.Helper()
	offered := make(chan Claim, 1)
	go func() {
		claim, err := r.Offer(context.Background(), n.ID, api.TurnOffer{Turn: turn, Role: "mason", Profile: "main"})
		if err != nil {
			t.Error(err)
		}
		offered <- claim
	}()
	awaitOffer(t, r, n, turn)
	messages := collect(t, r, lease, n, 0)
	if last := messages[len(messages)-1]; last.Offer == nil || last.Offer.Turn != turn {
		t.Fatalf("poll after an offer: %+v", messages)
	}
	answer, err := r.Claim(lease, n, turn, api.ClaimRequest{Accept: true})
	if err != nil || answer != api.ClaimTaken {
		t.Fatalf("claim: %q %v", answer, err)
	}
	if claim := <-offered; !claim.Accepted {
		t.Fatalf("the offer's claim: %+v", claim)
	}
}

func TestATurnReattachesByIDWithinTheGraceWindowAndIsLostAfterIt(t *testing.T) {
	t.Parallel()
	r, c := newRegistry(t)
	var ended []string
	r.SetHooks(Hooks{LeaseEnded: func(cl Client, turns []string) { ended = append(ended, strings.Join(turns, ",")) }})
	n := node("n1")
	first := connect(t, r, n, 2)
	claimTurn(t, r, n, first.Lease, "turn-1")

	c.advance(DefaultGrace - time.Second)
	again, err := r.Connect(context.Background(), n, api.ClientConnectRequest{Version: api.ClientProtocolVersion, Slots: 2, Lease: first.Lease, Running: []string{"turn-1", "turn-unknown"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []api.TurnReattach{{Turn: "turn-1", Answer: api.TurnReattached}, {Turn: "turn-unknown", Answer: api.TurnLost}}
	if again.Lease != first.Lease || !again.Resumed || !slices.Equal(again.Turns, want) {
		t.Fatalf("reconnect within the grace window: %+v", again)
	}
	if got, _ := r.Client(n.ID); len(got.Held) != 1 || got.Held[0].Turn != "turn-1" {
		t.Fatalf("held after reattach: %+v", got.Held)
	}

	c.advance(DefaultGrace)
	if _, ok := r.Client(n.ID); ok {
		t.Fatal("the lease outlived the grace window")
	}
	if !slices.Equal(ended, []string{"turn-1"}) {
		t.Fatalf("turns held when the lease ended: %v", ended)
	}
	late, err := r.Connect(context.Background(), n, api.ClientConnectRequest{Version: api.ClientProtocolVersion, Slots: 2, Lease: first.Lease, Running: []string{"turn-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if late.Resumed || late.Lease == first.Lease || !slices.Equal(late.Turns, []api.TurnReattach{{Turn: "turn-1", Answer: api.TurnLost}}) {
		t.Fatalf("reconnect after the grace window: %+v", late)
	}
}

func TestAServerThatRestartedAsksTheHookWhetherToReattachATurn(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	r.SetHooks(Hooks{Reattach: func(_ context.Context, id Identity, turn string) (string, bool) {
		return "token-for-" + turn, id.ID == "n1" && turn == "turn-1"
	}})
	n := node("n1")
	resp, err := r.Connect(context.Background(), n, api.ClientConnectRequest{Version: api.ClientProtocolVersion, Slots: 1, Lease: "lease-of-the-old-server", Running: []string{"turn-1", "turn-2"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []api.TurnReattach{{Turn: "turn-1", Answer: api.TurnReattached, Token: "token-for-turn-1"}, {Turn: "turn-2", Answer: api.TurnLost}}; !slices.Equal(resp.Turns, want) {
		t.Fatalf("answers: %+v", resp.Turns)
	}
	if got, _ := r.Client(n.ID); len(got.Held) != 1 || got.Slots != 1 || got.Free() != 0 {
		t.Fatalf("the reattached turn holds no slot: %+v", got)
	}
}

func TestDrainMarksTheClientDrainingAndRefusesOffers(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	n := node("n1")
	lease := connect(t, r, n, 2)
	if err := r.Drain(lease.Lease, n); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Client(n.ID)
	if !got.Draining {
		t.Fatalf("not draining: %+v", got)
	}
	if _, err := r.Offer(context.Background(), n.ID, api.TurnOffer{Turn: "t"}); !errors.Is(err, ErrDraining) {
		t.Fatalf("offer to a draining client: %v", err)
	}
	hb, err := r.Heartbeat(lease.Lease, n, api.HeartbeatRequest{})
	if err != nil || !hb.Draining {
		t.Fatalf("heartbeat: %+v %v", hb, err)
	}
	again, err := r.Connect(context.Background(), n, api.ClientConnectRequest{Version: api.ClientProtocolVersion, Slots: 2, Lease: lease.Lease})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Client(n.ID); !got.Draining || again.Lease != lease.Lease {
		t.Fatalf("a reconnect cleared the drain: %+v", got)
	}
}

func TestLeaveEndsTheLeaseAtOnceSoTheNodeConnectsAgain(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	n := node("n1")
	lease := connect(t, r, n, 1)
	if err := r.Leave(lease.Lease, n); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Client(n.ID); ok {
		t.Fatal("a client that left is registered")
	}
	connect(t, r, n, 1)
}

func TestACapabilityReportIsReadableFromTheRegistryWithSlotsHealthAndDraining(t *testing.T) {
	t.Parallel()
	r, c := newRegistry(t)
	r.SetHandout(api.ProfileHandout{Revision: "rev-1"})
	n := node("n1")
	lease := connect(t, r, n, 3)
	report := api.CapabilityReport{Revision: "rev-1", Profiles: []api.ProfileCapability{
		{Profile: "claude-sonnet", Runnable: true},
		{Profile: "codex", Runnable: false, Problems: []api.ProfileProblem{{Check: "codex --version", Detail: "not found", Fix: "install the Codex CLI"}}},
	}}
	current, err := r.Report(lease.Lease, n, report)
	if err != nil || !current {
		t.Fatalf("report: %v %v", current, err)
	}
	if err := r.Drain(lease.Lease, n); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Client(n.ID)
	if !ok || got.Slots != 3 || got.Free() != 3 || !got.Draining || got.Health != LeaseHealthy || got.Identity.Name != "node-n1" || !got.ReportCurrent() {
		t.Fatalf("client: %+v", got)
	}
	if !got.CanRun("claude-sonnet") || got.CanRun("codex") || got.CanRun("absent") {
		t.Fatalf("can run: %+v", got.Capabilities)
	}
	if fix := got.Capabilities.Profiles[1].Problems[0].Fix; fix != "install the Codex CLI" {
		t.Fatalf("fix: %q", fix)
	}
	c.advance(time.Minute)
	if late, _ := r.Client(n.ID); late.Health != LeaseLate {
		t.Fatalf("health a minute without heartbeats: %s", late.Health)
	}
	got.Capabilities.Profiles[0].Runnable = false
	if again, _ := r.Client(n.ID); !again.CanRun("claude-sonnet") {
		t.Fatal("a snapshot shares the report with the registry")
	}
}

func TestAReportOfAnOlderHandoutIsNotCurrent(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	r.SetHandout(api.ProfileHandout{Revision: "rev-1"})
	n := node("n1")
	lease := connect(t, r, n, 1)
	if _, err := r.Report(lease.Lease, n, api.CapabilityReport{Revision: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	r.SetHandout(api.ProfileHandout{Revision: "rev-2"})
	if got, _ := r.Client(n.ID); got.ReportCurrent() {
		t.Fatal("a report of the old handout is current")
	}
	if current, _ := r.Report(lease.Lease, n, api.CapabilityReport{Revision: "rev-1"}); current {
		t.Fatal("a late report of the old handout is current")
	}
}

func TestSetHandoutSendsTheHandoutToConnectedClientsAndToNewOnes(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	r.SetHandout(api.ProfileHandout{Revision: "rev-1"})
	n := node("n1")
	lease := connect(t, r, n, 1)
	if lease.Profiles.Revision != "rev-1" {
		t.Fatalf("handout on connect: %+v", lease.Profiles)
	}
	r.SetHandout(api.ProfileHandout{Revision: "rev-2"})
	r.SetHandout(api.ProfileHandout{Revision: "rev-3"})
	messages, err := r.Poll(context.Background(), lease.Lease, n, api.PollRequest{})
	if err != nil || len(messages) != 1 || messages[0].Kind != api.MessageProfiles || messages[0].Profiles.Revision != "rev-3" {
		t.Fatalf("poll: %+v %v", messages, err)
	}
	if _, err := r.Poll(context.Background(), lease.Lease, n, api.PollRequest{Ack: messages[0].Seq, WaitSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if again, _ := r.Poll(context.Background(), lease.Lease, n, api.PollRequest{Ack: messages[0].Seq}); len(again) != 0 {
		t.Fatalf("an acknowledged message is sent again: %+v", again)
	}
}

func TestAMessageIsSentAgainUntilItIsAcknowledged(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	n := node("n1")
	lease := connect(t, r, n, 1)
	if err := r.Cancel(n.ID, "turn-1", "stop"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		messages, err := r.Poll(context.Background(), lease.Lease, n, api.PollRequest{})
		if err != nil || len(messages) != 1 || messages[0].Cancel == nil || messages[0].Cancel.Turn != "turn-1" || messages[0].Seq != 1 {
			t.Fatalf("poll: %+v %v", messages, err)
		}
	}
	if messages, _ := r.Poll(context.Background(), lease.Lease, n, api.PollRequest{Ack: 1, WaitSeconds: 1}); len(messages) != 0 {
		t.Fatalf("after the ack: %+v", messages)
	}
}

func TestThePollOfAnEarlierConnectionEndsWhenTheClientReconnects(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	r.poll = 30 * time.Second
	n := node("n1")
	lease := connect(t, r, n, 1)
	done := make(chan []api.Message, 1)
	go func() {
		messages, err := r.Poll(context.Background(), lease.Lease, n, api.PollRequest{})
		if err != nil {
			t.Error(err)
		}
		done <- messages
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := r.Connect(context.Background(), n, api.ClientConnectRequest{Version: api.ClientProtocolVersion, Slots: 1, Lease: lease.Lease}); err != nil {
		t.Fatal(err)
	}
	select {
	case messages := <-done:
		if len(messages) != 0 {
			t.Fatalf("messages: %+v", messages)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the earlier poll is still waiting")
	}
}

// The server asks a client to carry out an operation on its own connection:
// the request reaches the client through the client's poll and the reply comes
// back as a request of the client's own, so the server never connects to it.
func TestTheServerSendsARequestOverTheClientsOwnConnectionAndGetsItsReply(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	n := node("n1")
	lease := connect(t, r, n, 1)
	type outcome struct {
		result json.RawMessage
		err    error
	}
	got := make(chan outcome, 1)
	go func() {
		result, err := r.Call(context.Background(), n.ID, "turn-1", api.OpViewRead, api.ViewReadParams{Path: "README.md"})
		got <- outcome{result, err}
	}()
	messages := collect(t, r, lease.Lease, n, 0)
	if len(messages) != 1 || messages[0].Request == nil {
		t.Fatalf("poll: %+v", messages)
	}
	request := messages[0].Request
	var params api.ViewReadParams
	if request.Turn != "turn-1" || request.Op != api.OpViewRead || json.Unmarshal(request.Params, &params) != nil || params.Path != "README.md" {
		t.Fatalf("request: %+v", request)
	}
	answered, err := r.Reply(lease.Lease, n, request.ID, api.ServerReply{Result: json.RawMessage(`"hello\n"`)})
	if err != nil || !answered {
		t.Fatalf("reply: %v %v", answered, err)
	}
	if out := <-got; out.err != nil || string(out.result) != `"hello\n"` {
		t.Fatalf("call: %q %v", out.result, out.err)
	}
	if answered, _ := r.Reply(lease.Lease, n, request.ID, api.ServerReply{}); answered {
		t.Fatal("a second reply is taken")
	}
}

func TestACallReturnsTheFailureTheClientReportsAndFailsWhenTheLeaseEnds(t *testing.T) {
	t.Parallel()
	r, c := newRegistry(t)
	n := node("n1")
	lease := connect(t, r, n, 1)
	failed := make(chan error, 1)
	go func() {
		_, err := r.Call(context.Background(), n.ID, "turn-1", api.OpViewWrite, api.ViewWriteParams{Path: "../x"})
		failed <- err
	}()
	messages := collect(t, r, lease.Lease, n, 0)
	if _, err := r.Reply(lease.Lease, n, messages[0].Request.ID, api.ServerReply{Error: "outside the view"}); err != nil {
		t.Fatal(err)
	}
	var remote *RemoteError
	if err := <-failed; !errors.As(err, &remote) || remote.Message != "outside the view" {
		t.Fatalf("call failure: %v", err)
	}

	lost := make(chan error, 1)
	go func() {
		_, err := r.Call(context.Background(), n.ID, "turn-1", api.OpViewRead, api.ViewReadParams{Path: "a"})
		lost <- err
	}()
	collect(t, r, lease.Lease, n, messages[0].Seq)
	c.advance(DefaultGrace)
	r.Expire()
	select {
	case err := <-lost:
		if !errors.Is(err, ErrLeaseEnded) {
			t.Fatalf("call on an ended lease: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call still waits after the lease ended")
	}
	if _, err := r.Call(context.Background(), n.ID, "turn-1", api.OpViewRead, nil); !errors.Is(err, ErrNoClient) {
		t.Fatalf("call without a lease: %v", err)
	}
}

func TestARequestOnAnotherNodesLeaseIsRefused(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	lease := connect(t, r, node("n1"), 1)
	connect(t, r, node("n2"), 1)
	_, err := r.Heartbeat(lease.Lease, node("n2"), api.HeartbeatRequest{})
	if code, _ := apiCode(t, err); code != api.Forbidden {
		t.Fatalf("heartbeat on another node's lease: %s", code)
	}
}

func TestAnOfferIsClaimedDeclinedOrWithdrawn(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	n := node("n1")
	lease := connect(t, r, n, 2)

	claimTurn(t, r, n, lease.Lease, "turn-1")
	if got, _ := r.Client(n.ID); got.Running() != 1 || got.Free() != 1 {
		t.Fatalf("after a claim: %+v", got)
	}

	declined := make(chan Claim, 1)
	go func() {
		claim, _ := r.Offer(context.Background(), n.ID, api.TurnOffer{Turn: "turn-2"})
		declined <- claim
	}()
	awaitOffer(t, r, n, "turn-2")
	answer, err := r.Claim(lease.Lease, n, "turn-2", api.ClaimRequest{Reason: "no provider login"})
	if err != nil || answer != api.ClaimReleased {
		t.Fatalf("decline: %q %v", answer, err)
	}
	if claim := <-declined; claim.Accepted || claim.Reason != "no provider login" {
		t.Fatalf("declined claim: %+v", claim)
	}

	ctx, cancel := context.WithCancel(context.Background())
	withdrawn := make(chan error, 1)
	go func() {
		_, err := r.Offer(ctx, n.ID, api.TurnOffer{Turn: "turn-3"})
		withdrawn <- err
	}()
	if got := waitFor(t, func() bool { c, _ := r.Client(n.ID); return len(c.Offered) == 1 }); !got {
		t.Fatal("the offer is not pending")
	}
	cancel()
	if err := <-withdrawn; !errors.Is(err, context.Canceled) {
		t.Fatalf("withdrawn offer: %v", err)
	}
	if answer, _ := r.Claim(lease.Lease, n, "turn-3", api.ClaimRequest{Accept: true}); answer != api.ClaimGone {
		t.Fatalf("a claim of a withdrawn offer: %q", answer)
	}
	if got, _ := r.Client(n.ID); got.Running() != 1 {
		t.Fatalf("a withdrawn offer holds a slot: %+v", got)
	}
}

func collect(t *testing.T, r *Registry, lease string, n Identity, ack int64) []api.Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		messages, err := r.Poll(context.Background(), lease, n, api.PollRequest{Ack: ack})
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) > 0 {
			return messages
		}
	}
	t.Fatal("the server sent no message")
	return nil
}

func awaitOffer(t *testing.T, r *Registry, n Identity, turn string) {
	t.Helper()
	if !waitFor(t, func() bool { c, _ := r.Client(n.ID); return slices.Contains(c.Offered, turn) }) {
		t.Fatalf("%s was never offered", turn)
	}
}

func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestHeartbeatsCarryProgressToTheHookAndNameTurnsTheServerDoesNotHold(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	var seen []api.TurnProgress
	r.SetHooks(Hooks{Progress: func(c Client, p api.TurnProgress) { seen = append(seen, p) }})
	n := node("n1")
	lease := connect(t, r, n, 2)
	claimTurn(t, r, n, lease.Lease, "turn-1")
	half := 0.5
	hb, err := r.Heartbeat(lease.Lease, n, api.HeartbeatRequest{Turns: []api.TurnProgress{{Turn: "turn-1", Phase: "editing", Progress: &half}, {Turn: "turn-9", Phase: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hb.Lost, []string{"turn-9"}) || len(seen) != 1 || seen[0].Phase != "editing" || *seen[0].Progress != 0.5 {
		t.Fatalf("heartbeat: %+v, progress %+v", hb, seen)
	}
	if got, _ := r.Client(n.ID); got.Held[0].Progress.Phase != "editing" {
		t.Fatalf("held progress: %+v", got.Held)
	}
}

func TestAResultGetsTheDefinitiveAnswerTheHookGivesAndFreesTheSlot(t *testing.T) {
	t.Parallel()
	r, _ := newRegistry(t)
	n := node("n1")
	lease := connect(t, r, n, 1)
	if _, err := r.Result(context.Background(), lease.Lease, n, "turn-1", api.TurnResult{}); err == nil {
		t.Fatal("a result is taken with no turn execution")
	}
	answers := map[string]api.ViewAnswer{"turn-1": api.ViewAccepted, "turn-2": api.ViewAlreadyRecorded, "turn-3": api.ViewSuperseded}
	var from []string
	r.SetHooks(Hooks{Result: func(_ context.Context, c Client, turn string, _ api.TurnResult) (api.ViewAnswer, error) {
		from = append(from, c.Identity.ID)
		return answers[turn], nil
	}})
	claimTurn(t, r, n, lease.Lease, "turn-1")
	for _, turn := range []string{"turn-1", "turn-2", "turn-3"} {
		got, err := r.Result(context.Background(), lease.Lease, n, turn, api.TurnResult{})
		if err != nil || got != answers[turn] {
			t.Fatalf("%s: %q %v", turn, got, err)
		}
	}
	if c, _ := r.Client(n.ID); c.Running() != 0 || !slices.Equal(from, []string{"n1", "n1", "n1"}) {
		t.Fatalf("after the results: %+v from %v", c, from)
	}
}
