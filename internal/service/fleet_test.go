package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
)

type fleetClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fleetClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fleetClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fleetRig struct {
	s     *Service
	c     *Client
	base  string
	clock *fleetClock
	files fixturekit.ReloadFiles
}

const testNodeHeader = "X-Test-Node"

func newFleetRig(t *testing.T, extraConfig string) *fleetRig {
	t.Helper()
	opts := fixture(t)
	fixturekit.WithListen(t, opts.Config.Root, "tailnet = \"osmia\"\n")
	if extraConfig != "" {
		path := opts.Config.Root + "/config.toml"
		top, err := os.ReadFile(path)
		fixturekit.Must(t, err)
		fixturekit.Must(t, os.WriteFile(path, append(top, []byte(extraConfig)...), 0o600))
	}
	withTailnet(t, &opts, "osmia-1")
	clock := &fleetClock{t: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)}
	opts.Fleet = fleet.Options{Now: clock.now, Poll: 300 * time.Millisecond}
	opts.ClientIdentity = func(r *http.Request) (fleet.Identity, error) {
		id := r.Header.Get(testNodeHeader)
		return fleet.Identity{ID: id, Name: "name-of-" + id}, nil
	}
	files := fixturekit.ConfigFiles(t, opts.Config.Root)
	s, c := start(t, opts)
	return &fleetRig{s: s, c: c, base: "http://" + s.TailnetAddr(), clock: clock, files: files}
}

func (r *fleetRig) post(t *testing.T, node, path string, body any) reply {
	t.Helper()
	data, err := json.Marshal(body)
	fixturekit.Must(t, err)
	return send(t, http.MethodPost, r.base+path, "", map[string]string{"Content-Type": "application/json", testNodeHeader: node}, data)
}

func (r *fleetRig) connect(t *testing.T, node string, req api.ClientConnectRequest) (api.ClientConnectResponse, reply) {
	t.Helper()
	out := r.post(t, node, api.ClientConnectPath, req)
	var resp api.ClientConnectResponse
	if out.status == http.StatusOK {
		fixturekit.Must(t, json.Unmarshal(out.body, &resp))
	}
	return resp, out
}

func hello(slots int) api.ClientConnectRequest {
	return api.ClientConnectRequest{Version: api.ClientProtocolVersion, Slots: slots}
}

func decodeReply[T any](t *testing.T, out reply) T {
	t.Helper()
	if out.status != http.StatusOK {
		t.Fatalf("status %d: %s", out.status, out.body)
	}
	var v T
	fixturekit.Must(t, json.Unmarshal(out.body, &v))
	return v
}

func (r *fleetRig) poll(t *testing.T, node, lease string, ack int64) []api.Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp := decodeReply[api.PollResponse](t, r.post(t, node, api.ClientRoute(lease, "poll"), api.PollRequest{Ack: ack, WaitSeconds: 1}))
		if len(resp.Messages) > 0 {
			return resp.Messages
		}
	}
	t.Fatal("the server sent no message")
	return nil
}

func TestAClientOfAnotherProtocolVersionIsRefusedNamingBothAndAMatchingOneIsAccepted(t *testing.T) {
	t.Parallel()
	rig := newFleetRig(t, "")
	_, out := rig.connect(t, "node-1", api.ClientConnectRequest{Version: api.ClientProtocolVersion + 7, Slots: 1})
	var failure api.ErrorResponse
	fixturekit.Must(t, json.Unmarshal(out.body, &failure))
	server, client := api.ClientProtocolVersion, api.ClientProtocolVersion+7
	wantServer, wantClient := "version "+itoa(server), "version "+itoa(client)
	if out.status != http.StatusConflict || failure.Error.Code != api.VersionMismatch || !strings.Contains(failure.Error.Message, wantServer) || !strings.Contains(failure.Error.Message, wantClient) {
		t.Fatalf("mismatch: %d %s", out.status, out.body)
	}
	if got := rig.s.Fleet().Clients(); len(got) != 0 {
		t.Fatalf("a refused client is registered: %+v", got)
	}
	resp, out := rig.connect(t, "node-1", hello(2))
	if out.status != http.StatusOK || resp.Lease == "" || resp.Client.ID != "node-1" || resp.Client.Name != "name-of-node-1" || resp.GraceSeconds != 120 {
		t.Fatalf("connect: %d %s", out.status, out.body)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestASecondConnectionFromTheSameNodeIsRefusedWhileItsLeaseIsHeldAndAcceptedOnceItEnded(t *testing.T) {
	t.Parallel()
	rig := newFleetRig(t, "")
	first, _ := rig.connect(t, "node-1", hello(1))
	_, out := rig.connect(t, "node-1", hello(1))
	var failure api.ErrorResponse
	fixturekit.Must(t, json.Unmarshal(out.body, &failure))
	if out.status != http.StatusConflict || failure.Error.Code != api.ClientConnected {
		t.Fatalf("second connection: %d %s", out.status, out.body)
	}
	if _, out := rig.connect(t, "node-2", hello(1)); out.status != http.StatusOK {
		t.Fatalf("another node: %d %s", out.status, out.body)
	}

	rig.clock.advance(fleet.DefaultGrace - time.Second)
	if _, out := rig.connect(t, "node-1", hello(1)); out.status != http.StatusConflict {
		t.Fatalf("inside the grace window: %d %s", out.status, out.body)
	}
	if hb := rig.post(t, "node-1", api.ClientRoute(first.Lease, "heartbeat"), api.HeartbeatRequest{}); hb.status != http.StatusOK {
		t.Fatalf("heartbeat inside the grace window: %d %s", hb.status, hb.body)
	}
	rig.clock.advance(fleet.DefaultGrace)
	second, out := rig.connect(t, "node-1", hello(1))
	if out.status != http.StatusOK || second.Lease == first.Lease {
		t.Fatalf("after the lease ended: %d %s", out.status, out.body)
	}
	if hb := rig.post(t, "node-1", api.ClientRoute(first.Lease, "heartbeat"), api.HeartbeatRequest{}); hb.status != http.StatusGone || hb.code(t) != api.LeaseEnded {
		t.Fatalf("heartbeat on the ended lease: %d %s", hb.status, hb.body)
	}
}

func TestTheHandoutIsSentOnConnectAndAgainAfterAReloadAndHoldsNoSecret(t *testing.T) {
	secrets := map[string]string{
		"OSMIA_FLEET_DAGGER_COMPUTE": "dagger-cloud-secret-value",
		"OPENROUTER_API_KEY":         "provider-key-secret-value",
		"ANTHROPIC_API_KEY":          "anthropic-secret-value",
		"GITHUB_TOKEN":               "github-secret-value",
		"TS_AUTHKEY":                 "tailnet-secret-value",
	}
	for name, value := range secrets {
		t.Setenv(name, value)
	}
	rig := newFleetRig(t, "[dagger]\nengine = \"cloud\"\ncloud_compute_token_env = \"OSMIA_FLEET_DAGGER_COMPUTE\"\n")
	resp, out := rig.connect(t, "node-1", hello(1))
	if out.status != http.StatusOK {
		t.Fatalf("connect: %d %s", out.status, out.body)
	}
	want := fleet.Handout(rig.s.current())
	if resp.Profiles.Revision != want.Revision || len(resp.Profiles.Roles) == 0 || len(resp.Profiles.Profiles) == 0 {
		t.Fatalf("handout on connect: %+v", resp.Profiles)
	}
	roles := map[string]api.RoleRequirement{}
	for _, role := range resp.Profiles.Roles {
		roles[role.Role] = role
		if role.Backend == "" || role.Model == "" || role.Sandbox == "" || role.Profile == "" || len(role.Chain) == 0 || role.Domains == nil || role.Skills == nil {
			t.Errorf("role %s lacks a requirement: %+v", role.Role, role)
		}
	}
	if mason := roles["mason"]; mason.Dagger == nil || mason.Dagger.Mode != "cloud" {
		t.Fatalf("mason requirement: %+v", mason)
	}
	for name, value := range secrets {
		if strings.Contains(string(out.body), value) || strings.Contains(string(out.body), name) {
			t.Fatalf("the connect response holds %s: %s", name, out.body)
		}
	}

	rig.files.Write(t, strings.Replace(rig.files.TopText, `model = "test"`, `model = "reloaded-model"`, 1))
	_, err := rig.c.Reload(context.Background())
	fixturekit.Must(t, err)
	messages := rig.poll(t, "node-1", resp.Lease, 0)
	last := messages[len(messages)-1]
	if last.Kind != api.MessageProfiles || last.Profiles == nil || last.Profiles.Revision == resp.Profiles.Revision {
		t.Fatalf("after the reload: %+v", messages)
	}
	reloaded := false
	for _, p := range last.Profiles.Profiles {
		reloaded = reloaded || p.Model == "reloaded-model"
	}
	if !reloaded {
		t.Fatalf("the handout after the reload holds the old profiles: %+v", last.Profiles.Profiles)
	}
	data, _ := json.Marshal(messages)
	for name, value := range secrets {
		if strings.Contains(string(data), value) || strings.Contains(string(data), name) {
			t.Fatalf("the handout after the reload holds %s: %s", name, data)
		}
	}
}

func TestAClientsCapabilityReportDrainAndLeaseHealthAreReadableFromTheRegistry(t *testing.T) {
	t.Parallel()
	rig := newFleetRig(t, "")
	resp, _ := rig.connect(t, "node-1", hello(3))
	report := api.CapabilityReport{Revision: resp.Profiles.Revision, Profiles: []api.ProfileCapability{
		{Profile: "default", Runnable: true},
		{Profile: "other", Runnable: false, Problems: []api.ProfileProblem{{Check: "codex --version", Detail: "executable not found", Fix: "install the Codex CLI"}}},
	}}
	got := decodeReply[api.CapabilityResponse](t, rig.post(t, "node-1", api.ClientRoute(resp.Lease, "capabilities"), report))
	if !got.Current {
		t.Fatal("a report of the current handout is not current")
	}
	if drain := decodeReply[api.DrainResponse](t, rig.post(t, "node-1", api.ClientRoute(resp.Lease, "drain"), api.DrainRequest{})); !drain.Draining {
		t.Fatalf("drain: %+v", drain)
	}
	rig.clock.advance(time.Minute)
	client, ok := rig.s.Fleet().Client("node-1")
	if !ok || client.Slots != 3 || !client.Draining || client.Health != fleet.LeaseLate || !client.CanRun("default") || client.CanRun("other") {
		t.Fatalf("registry: %+v", client)
	}
	if fix := client.Capabilities.Profiles[1].Problems[0].Fix; fix != "install the Codex CLI" {
		t.Fatalf("fix: %q", fix)
	}
	hb := decodeReply[api.HeartbeatResponse](t, rig.post(t, "node-1", api.ClientRoute(resp.Lease, "heartbeat"), api.HeartbeatRequest{}))
	if !hb.Draining {
		t.Fatalf("heartbeat: %+v", hb)
	}
	if client, _ = rig.s.Fleet().Client("node-1"); client.Health != fleet.LeaseHealthy {
		t.Fatalf("health after a heartbeat: %s", client.Health)
	}
}

// The server asks a client to read a file of its view over the client's own
// connection: the request arrives with the client's poll, the reply is posted
// back, and the fake client has no listener the server could dial.
func TestTheServerRequestsAClientOverItsOwnConnectionAndGetsTheReply(t *testing.T) {
	t.Parallel()
	rig := newFleetRig(t, "")
	resp, _ := rig.connect(t, "node-1", hello(1))
	type outcome struct {
		result json.RawMessage
		err    error
	}
	got := make(chan outcome, 1)
	go func() {
		result, err := rig.s.Fleet().Call(context.Background(), "node-1", "turn-1", api.OpViewRead, api.ViewReadParams{Path: "README.md"})
		got <- outcome{result, err}
	}()
	messages := rig.poll(t, "node-1", resp.Lease, 0)
	request := messages[0].Request
	var params api.ViewReadParams
	if len(messages) != 1 || request == nil || request.Op != api.OpViewRead || request.Turn != "turn-1" || json.Unmarshal(request.Params, &params) != nil || params.Path != "README.md" {
		t.Fatalf("request: %+v", messages)
	}
	answered := decodeReply[api.ServerReplyResponse](t, rig.post(t, "node-1", api.ClientRoute(resp.Lease, "requests/"+request.ID+"/reply"), api.ServerReply{Result: json.RawMessage(`"the file as the session left it"`)}))
	if !answered.Answered {
		t.Fatal("the reply was not taken")
	}
	out := <-got
	if out.err != nil || string(out.result) != `"the file as the session left it"` {
		t.Fatalf("call: %q %v", out.result, out.err)
	}
}

func TestOfferClaimAndResultTravelOverTheClientsConnectionWithDefinitiveAnswers(t *testing.T) {
	t.Parallel()
	rig := newFleetRig(t, "")
	answers := map[string]api.ViewAnswer{"turn-1": api.ViewAccepted, "turn-2": api.ViewAlreadyRecorded, "turn-3": api.ViewSuperseded}
	rig.s.Fleet().SetHooks(fleet.Hooks{Result: func(_ context.Context, c fleet.Client, turn string, res api.TurnResult) (api.ViewAnswer, error) {
		if c.Identity.ID != "node-1" || res.Result.FinalResponse != "done" {
			t.Errorf("result of %s from %s: %+v", turn, c.Identity.ID, res)
		}
		return answers[turn], nil
	}})
	resp, _ := rig.connect(t, "node-1", hello(1))
	claimed := make(chan fleet.Claim, 1)
	go func() {
		claim, err := rig.s.Fleet().Offer(context.Background(), "node-1", api.TurnOffer{Turn: "turn-1", Role: "mason", Profile: "default", ToolEndpoint: "http://server/mcp", ToolToken: "token", View: api.TurnView{Kind: api.ViewGit, Project: "p", Commit: "abc"}})
		if err != nil {
			t.Error(err)
		}
		claimed <- claim
	}()
	messages := rig.poll(t, "node-1", resp.Lease, 0)
	offer := messages[0].Offer
	if offer == nil || offer.Turn != "turn-1" || offer.Role != "mason" || offer.View.Commit != "abc" || offer.ToolToken != "token" {
		t.Fatalf("offer: %+v", messages)
	}
	claim := decodeReply[api.ClaimResponse](t, rig.post(t, "node-1", api.ClientRoute(resp.Lease, "turns/turn-1/claim"), api.ClaimRequest{Accept: true}))
	if claim.Answer != api.ClaimTaken || !(<-claimed).Accepted {
		t.Fatalf("claim: %+v", claim)
	}
	if again := decodeReply[api.ClaimResponse](t, rig.post(t, "node-1", api.ClientRoute(resp.Lease, "turns/turn-9/claim"), api.ClaimRequest{Accept: true})); again.Answer != api.ClaimGone {
		t.Fatalf("claim of an unknown offer: %+v", again)
	}
	var result api.TurnResult
	result.Result.FinalResponse = "done"
	for _, turn := range []string{"turn-1", "turn-2", "turn-3"} {
		got := decodeReply[api.TurnResultResponse](t, rig.post(t, "node-1", api.ClientRoute(resp.Lease, "turns/"+turn+"/result"), result))
		if got.Answer != answers[turn] {
			t.Fatalf("%s: %+v", turn, got)
		}
	}
	if c, _ := rig.s.Fleet().Client("node-1"); c.Running() != 0 {
		t.Fatalf("slot still held: %+v", c)
	}
}

func TestOnlyTheTailnetListenerServesTheClientProtocolAndOnlyToAnIdentifiedNode(t *testing.T) {
	t.Parallel()
	rig := newFleetRig(t, "")
	body, _ := json.Marshal(hello(1))
	req := httptest.NewRequest(http.MethodPost, api.ClientConnectPath, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rig.s.handle(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("the socket serves the client protocol: %d %s", rec.Code, rec.Body)
	}
	if got := rig.s.Fleet().Clients(); len(got) != 0 {
		t.Fatalf("registered: %+v", got)
	}

	opts := fixture(t)
	fixturekit.WithListen(t, opts.Config.Root, "tailnet = \"osmia\"\n")
	withTailnet(t, &opts, "osmia-1")
	s, _ := start(t, opts)
	out := send(t, http.MethodPost, "http://"+s.TailnetAddr()+api.ClientConnectPath, "", map[string]string{"Content-Type": "application/json"}, body)
	if out.status != http.StatusServiceUnavailable || out.code(t) != api.Unavailable {
		t.Fatalf("a caller the tailnet cannot identify: %d %s", out.status, out.body)
	}
}

func TestClientRoutesAreListed(t *testing.T) {
	t.Parallel()
	listed := map[string]bool{}
	for _, route := range Routes {
		listed[route.Method+" "+route.Path] = true
	}
	for _, key := range []string{
		"POST /v1/clients/connect",
		"POST /v1/clients/{lease}/heartbeat",
		"POST /v1/clients/{lease}/capabilities",
		"POST /v1/clients/{lease}/drain",
		"POST /v1/clients/{lease}/leave",
		"POST /v1/clients/{lease}/poll",
		"POST /v1/clients/{lease}/turns/{turn}/claim",
		"POST /v1/clients/{lease}/turns/{turn}/result",
		"POST /v1/clients/{lease}/requests/{request}/reply",
	} {
		if !listed[key] {
			t.Errorf("%s is not in Routes", key)
		}
	}
}
