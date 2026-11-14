//go:build hearsaysmoke

package service

// The Compose smoke run against a real Hearsay, driven by
// scripts/hearsay-compose-smoke.sh (docs/hearsay-compose-smoke.md).
//
// Nothing here fakes Hearsay. The script runs Hearsay under Compose with the
// configuration `osmia project memory` generates and then runs these tests in
// a container on the Compose network:
//
//   - TestHearsaySmokeConfig writes the fragments the memory setup generates,
//     for the fixture project, so Hearsay can be started with them.
//   - TestHearsaySmokeRun drives the project through a sealed spec, an owner
//     ruling on an escalated question and a chief logged assumption, lets the
//     service's own publisher pass post them to Hearsay's connectors listener,
//     and reads them back through Osmia's read path (the tools and the bundle
//     provider the roles get) with each class agent acting for the owner.
//
// Both tests skip unless the script's environment is present.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/bundle"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/hearsay"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

// Names the memory setup generates by default; the script's tokens use them.
const (
	smokeWork      = "osmia-work"
	smokeOwnerSrc  = "osmia-work-owner"
	smokePublisher = "osmia-publisher"
	smokeOwner     = "osmia-owner"
)

var smokeAgents = map[string]config.HearsayAgent{
	"worker":       {ID: "osmia-worker", TokenEnv: "HEARSAY_OSMIA_WORKER_TOKEN"},
	"orchestrator": {ID: "osmia-orchestrator", TokenEnv: "HEARSAY_OSMIA_ORCHESTRATOR_TOKEN"},
	"observer":     {ID: "osmia-observer", TokenEnv: "HEARSAY_OSMIA_OBSERVER_TOKEN"},
}

// smokeHearsay is the Hearsay configuration the run gives Osmia: the API at
// api, the publisher at hooks, the owner as the principal the class agents act
// for.
func smokeHearsay(api, hooks string) config.Hearsay {
	return config.Hearsay{URL: api, Principal: smokeOwner, TokenEnv: "HEARSAY_OWNER_TOKEN", Agents: smokeAgents,
		Publish: &config.HearsayPublish{WorkSource: smokeWork, OwnerSource: smokeOwnerSrc, HooksURL: hooks, Agent: smokePublisher, TokenEnv: "HEARSAY_OSMIA_PUBLISHER_TOKEN"}}
}

func TestHearsaySmokeConfig(t *testing.T) {
	dir := os.Getenv("OSMIA_SMOKE_CONFIG_DIR")
	if dir == "" {
		t.Skip("OSMIA_SMOKE_CONFIG_DIR is not set; scripts/hearsay-compose-smoke.sh runs this test")
	}
	f := newPublisherFixture(t, func(cfg *config.Config) { cfg.Hearsay = smokeHearsay("http://api:8080", "http://connectors:8081") })
	setup, err := buildMemorySetup(f.cfg, f.repo)
	must(t, err)
	for name, content := range setup.Files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		must(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must(t, os.WriteFile(path, append(content, '\n'), 0o644))
	}
	summary, err := json.MarshalIndent(map[string]any{"project": f.repo.Project(), "work_source": setup.WorkSource, "owner_source": setup.OwnerSource, "publisher": setup.Publisher, "principal": setup.Principal, "hearsay_scope": setup.Scope}, "", "  ")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, "memory-setup-summary.json"), append(summary, '\n'), 0o644))
	t.Logf("wrote %d fragments for project %s under %s", len(setup.Files), f.repo.Project(), dir)
}

// recorded is one request the run made, with credentials elided.
type recorded struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	// Who the request was made as: the bearer's role in the run and, for a
	// read, the agent and principal headers.
	Bearer    string          `json:"bearer,omitempty"`
	Principal string          `json:"hearsay_principal,omitempty"`
	Agent     string          `json:"hearsay_agent,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
}

// recorder wraps a transport and keeps what passed through it.
type recorder struct {
	mu     sync.Mutex
	tokens map[string]string // token value -> label
	log    []recorded
	next   http.RoundTripper
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	resp, err := r.next.RoundTrip(req)
	entry := recorded{Method: req.Method, Path: req.URL.Path, Principal: req.Header.Get("Hearsay-Principal"), Agent: req.Header.Get("Hearsay-Agent")}
	if resp != nil {
		entry.Status = resp.StatusCode
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry.Bearer = r.label(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
	if json.Valid(body) {
		entry.Body = body
	}
	if a := req.Header.Get("Hearsay-Agent-Token"); a != "" {
		entry.Agent += " (agent token " + r.label(a) + ")"
	}
	r.log = append(r.log, entry)
	return resp, err
}

func (r *recorder) label(token string) string {
	if token == "" {
		return ""
	}
	if l, ok := r.tokens[token]; ok {
		return l
	}
	return "unknown token"
}

func (r *recorder) take() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.log
	r.log = nil
	return out
}

func smokeEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set", name)
	}
	return v
}

// smokeBundle is the part of Hearsay's get_bundle answer the run reads.
type smokeBundle struct {
	Scope struct {
		ID string `json:"id"`
	} `json:"scope"`
	Anchors []smokeAnchor `json:"anchors"`
	Stances []struct {
		Current  string `json:"current"`
		Tier     string `json:"tier"`
		Decision *struct {
			ID           string `json:"id"`
			Decider      string `json:"decider"`
			Status       string `json:"status"`
			SupersededBy string `json:"superseded_by"`
		} `json:"decision"`
	} `json:"stances"`
}

type smokeAnchor struct {
	L1   string `json:"l1"`
	Kind string `json:"kind"`
	Line string `json:"line"`
}

type smokeLogged struct{ Tier, Decider, Status, Outcome string }

func (b smokeBundle) decisions() map[string]smokeLogged {
	out := map[string]smokeLogged{}
	for _, s := range b.Stances {
		if s.Decision != nil {
			out[s.Decision.ID] = smokeLogged{s.Tier, s.Decision.Decider, s.Decision.Status, s.Current}
		}
	}
	return out
}

// smokeTraversed is the part of a traverse answer the run reads.
type smokeTraversed struct {
	Nodes []smokeNode `json:"nodes"`
}

type smokeNode struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Class    string `json:"class"`
	Line     string `json:"line"`
	Tier     string `json:"tier"`
	Current  any    `json:"current"`
	Decision *struct {
		ID      string `json:"id"`
		Decider string `json:"decider"`
		Status  string `json:"status"`
	} `json:"decision"`
}

func (g smokeTraversed) decisions() map[string]smokeLogged {
	out := map[string]smokeLogged{}
	for _, n := range g.Nodes {
		if n.Type == "topic" && n.Decision != nil {
			out[n.Decision.ID] = smokeLogged{n.Tier, n.Decision.Decider, n.Decision.Status, fmt.Sprint(n.Current)}
		}
	}
	return out
}

func TestHearsaySmokeRun(t *testing.T) {
	apiURL := os.Getenv("OSMIA_SMOKE_API_URL")
	hooksURL := os.Getenv("OSMIA_SMOKE_HOOKS_URL")
	if apiURL == "" || hooksURL == "" {
		t.Skip("OSMIA_SMOKE_API_URL and OSMIA_SMOKE_HOOKS_URL are not set; scripts/hearsay-compose-smoke.sh runs this test")
	}
	evidence := os.Getenv("OSMIA_SMOKE_EVIDENCE")
	if evidence != "" {
		must(t, os.MkdirAll(evidence, 0o755))
	}
	keep := func(name string, v any) {
		t.Helper()
		data, err := json.MarshalIndent(v, "", "  ")
		must(t, err)
		t.Logf("---- %s ----\n%s", name, data)
		if evidence != "" {
			must(t, os.WriteFile(filepath.Join(evidence, name+".json"), append(data, '\n'), 0o644))
		}
	}
	// A caller-visible label for each credential, so the run can show which
	// token each request carried without printing one.
	tokens := map[string]string{}
	for label, env := range map[string]string{
		"publisher token": "HEARSAY_OSMIA_PUBLISHER_TOKEN", "owner token": "HEARSAY_OWNER_TOKEN",
		"worker agent token": "HEARSAY_OSMIA_WORKER_TOKEN", "orchestrator agent token": "HEARSAY_OSMIA_ORCHESTRATOR_TOKEN", "observer agent token": "HEARSAY_OSMIA_OBSERVER_TOKEN",
		"outsider token": "HEARSAY_SMOKE_OUTSIDER_TOKEN",
	} {
		tokens[smokeEnv(t, env)] = label
	}
	direct := &http.Transport{} // no proxy: the services are on the Compose network
	hookLog := &recorder{tokens: tokens, next: direct}
	readLog := &recorder{tokens: tokens, next: direct}

	// The publisher reaches the connectors through a recording relay, so the
	// run can show exactly what was posted and that a second pass posted nothing.
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, err := http.NewRequestWithContext(r.Context(), r.Method, hooksURL+r.URL.Path, bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := (&http.Client{Transport: hookLog}).Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(relay.Close)

	f := newPublisherFixture(t, func(cfg *config.Config) { cfg.Hearsay = smokeHearsay(apiURL, relay.URL) })
	project := string(f.repo.Project())
	stream := fixturekit.Stream
	entity := "work:" + smokeWork + ":" + project
	streamEntity := entity + "#" + string(stream)
	t.Logf("EVIDENCE: container evidence. Hearsay runs under Compose; this test runs in a container on its network; project %s, workstream %s", project, stream)

	// ---- drive the project: a sealed spec, an owner ruling on an escalated
	// question, a chief logged assumption ----
	const specText = "# Upload limits\n\nCap uploads at 10 MiB. Reject anything larger with a clear error.\n"
	fixturekit.RecordHanded(t, f.repo, stream, "stdin", fixturekit.DemoStart)
	f.recordSeal(stream, 1, specText)
	f.ask(stream, "1", "Is the upload cap 10 MiB or unbounded?", hr(1))
	f.escalate(stream, "1", "Is the upload cap 10 MiB or unbounded?", hr(2))
	f.ruling(stream, "1", 1, trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: "Cap uploads at 10 MiB."}, hr(3), trace.Actor{Kind: "owner", ID: "local"})
	// The chief's relay of the ruling to the askers is a later revision; it must not be posted as a ruling.
	f.ruling(stream, "1", 2, trace.Ruling{Decision: trace.DecisionRuling, OwnerResponse: "Cap uploads at 10 MiB.", ReturnedAnswer: "RELAYED-TO-THE-ASKERS", Scope: trace.ScopeLocal}, hr(4), chiefActor)
	const assumption = "Treat the 10 MiB cap as inclusive."
	content := fmt.Sprintf(`{"version":1,"assumptions":[{"workstreams":[%q],"id":"1","text":%q,"actor":{"kind":"agent","id":"chief_of_staff"},"thread":"chief","turn":"t1","at":%q}]}`, string(stream), assumption, hr(5).Format(time.RFC3339))
	f.document(stream, trace.AssumptionsDocument, 1, trace.AssumptionsPath, content, hr(5), chiefActor)

	// ---- the publisher posts them ----
	res, err := f.s.publishPass(context.Background(), f.repo, f.s.current())
	must(t, err)
	posts := hookLog.take()
	keep("publisher-pass-1", map[string]any{"posts": res.Posts, "failures": res.Failures, "unreachable": res.Unreachable, "requests": posts})
	if res.Posts == 0 || len(res.Failures) != 0 || res.Unreachable {
		t.Fatalf("the first pass did not post cleanly: %+v", res)
	}
	type post struct {
		Kind, Source, Author, Decider, Decision, Document, Workstream string
		Repos                                                         any
		Raw                                                           map[string]any
	}
	var sent []post
	for _, p := range posts {
		var raw map[string]any
		must(t, json.Unmarshal(p.Body, &raw))
		if p.Status != http.StatusAccepted {
			t.Errorf("POST %s = %d %s", p.Path, p.Status, p.Body)
		}
		if p.Bearer != "publisher token" {
			t.Errorf("a post carried %q, want the publisher's token", p.Bearer)
		}
		s := func(k string) string { v, _ := raw[k].(string); return v }
		sent = append(sent, post{s("kind"), strings.TrimPrefix(p.Path, "/hooks/"), s("author"), s("decider"), s("decision"), s("document"), s("workstream"), raw["repos"], raw})
	}
	pick := func(kind, field, value string) *post {
		for i := range sent {
			s := sent[i]
			got := map[string]string{"decision": s.Decision, "document": s.Document, "workstream": s.Workstream, "project": ""}[field]
			if s.Kind == kind && got == value {
				return &sent[i]
			}
		}
		return nil
	}
	if p := pick("project", "project", ""); p == nil || p.Source != smokeWork || p.Author != smokeOwner || p.Repos != nil {
		t.Errorf("the project was not posted on the work source with the owner as author and without repos: %+v", p)
	}
	if p := pick("document", "document", string(stream)+"-spec"); p == nil || p.Source != smokeOwnerSrc || p.Author != smokeOwner || p.Workstream != string(stream) {
		t.Errorf("the sealed spec was not posted as a workstream document on the owner source: %+v", p)
	}
	if p := pick("decision", "decision", string(stream)+"-ruling-1"); p == nil || p.Source != smokeOwnerSrc || p.Author != smokeOwner || p.Decider != "owner" {
		t.Errorf("the ruling was not posted on the owner source as the owner's: %+v", p)
	}
	var assumptionPost *post
	for i := range sent {
		if sent[i].Kind == "decision" && sent[i].Raw["outcome"] == assumption {
			assumptionPost = &sent[i]
		}
	}
	if assumptionPost == nil || assumptionPost.Source != smokeWork || assumptionPost.Author != "" || assumptionPost.Decider != "chief_of_staff" {
		t.Errorf("the assumption was not posted on the work source without an author: %+v", assumptionPost)
	}
	for _, p := range sent {
		if p.Kind == "decision" && p.Decision != string(stream)+"-ruling-1" && p.Source == smokeOwnerSrc {
			t.Errorf("an owner-source decision beyond the ruling was posted: %v", p.Raw)
		}
		if strings.Contains(fmt.Sprint(p.Raw), "RELAYED-TO-THE-ASKERS") {
			t.Errorf("the chief's relay was posted: %v", p.Raw)
		}
		if p.Kind != "project" && p.Repos != nil {
			t.Errorf("%s carries repos: %v", p.Kind, p.Raw)
		}
	}
	assumptionID := ""
	if assumptionPost != nil {
		assumptionID = assumptionPost.Decision
	}

	// ---- a second pass with nothing changed posts nothing ----
	res2, err := f.s.publishPass(context.Background(), f.repo, f.s.current())
	must(t, err)
	again := hookLog.take()
	keep("publisher-pass-2", map[string]any{"posts": res2.Posts, "failures": res2.Failures, "requests": again})
	if res2.Posts != 0 || len(again) != 0 || len(res2.Failures) != 0 {
		t.Errorf("a second pass with nothing changed posted %d records (%d requests)", res2.Posts, len(again))
	}

	// ---- read back through Osmia's read path ----
	client := func(h config.Hearsay) hearsay.Client {
		return hearsay.Client{Config: h, HTTP: &http.Client{Transport: readLog}}
	}
	cfg := f.s.current()
	// The turn's own check, which the service backs with the active turn.
	ready := func() error { return nil }
	call := func(c hearsay.Client, role, tool string, args map[string]any) ([]byte, error) {
		for _, tl := range c.Tools(role, entity, ready) {
			if tl.Name == tool {
				raw, _ := json.Marshal(args)
				return tl.Handle(context.Background(), raw)
			}
		}
		return nil, fmt.Errorf("role %s is not offered %s", role, tool)
	}
	traverse := func(c hearsay.Client, role, scope string, edges ...string) ([]byte, error) {
		var steps []map[string]string
		for _, e := range edges {
			steps = append(steps, map[string]string{"edge": e})
		}
		return call(c, role, "traverse", map[string]any{"start": []map[string]string{{"type": "entity", "id": scope}}, "steps": steps})
	}
	owner := client(cfg.Hearsay)

	// Hearsay places a post asynchronously (the distiller and the assertion
	// worker), so the first read waits for the whole log to be served.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		raw, err := call(owner, "mason", "get_bundle", map[string]any{"scope": streamEntity})
		var b smokeBundle
		if err == nil && json.Unmarshal(raw, &b) == nil {
			got := b.decisions()
			if _, ok := got[string(stream)+"-ruling-1"]; ok && len(b.Anchors) > 0 && assumptionID != "" {
				if _, ok := got[assumptionID]; ok {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Hearsay did not serve the posted records within 3 minutes (last error %v)", err)
		}
		time.Sleep(time.Second)
	}
	readLog.take()

	roles := []struct{ role, class string }{{"mason", "worker"}, {"chief_of_staff", "orchestrator"}, {sessionRole, "observer"}}
	for _, r := range roles {
		name := r.class
		// The tools the role is offered.
		names := map[string]bool{}
		for _, tl := range owner.Tools(r.role, entity, ready) {
			names[tl.Name] = true
		}
		if !names["get_bundle"] || !names["traverse"] || (r.class == "observer") == names["assert"] {
			t.Errorf("%s (%s) tools: %v", r.role, name, names)
		}

		// get_bundle on the project's default entity, as a tool call with no scope.
		raw, err := call(owner, r.role, "get_bundle", map[string]any{})
		if err != nil {
			t.Errorf("%s: get_bundle(%s): %v", name, entity, err)
			continue
		}
		var project smokeBundle
		must(t, json.Unmarshal(raw, &project))
		keep("get_bundle-project-"+name, json.RawMessage(raw))
		if project.Scope.ID != entity {
			t.Errorf("%s: the default read was %q, want %q", name, project.Scope.ID, entity)
		}
		if !slices.ContainsFunc(project.Anchors, func(a smokeAnchor) bool { return strings.Contains(a.L1, ":document:charter:") }) {
			t.Errorf("%s: the project's read serves no charter document: %+v", name, project.Anchors)
		}

		// get_bundle on the workstream entity.
		raw, err = call(owner, r.role, "get_bundle", map[string]any{"scope": streamEntity})
		if err != nil {
			t.Errorf("%s: get_bundle(%s): %v", name, streamEntity, err)
			continue
		}
		var b smokeBundle
		must(t, json.Unmarshal(raw, &b))
		keep("get_bundle-workstream-"+name, json.RawMessage(raw))
		got := b.decisions()
		if g := got[string(stream)+"-ruling-1"]; g != (smokeLogged{"ratified", "owner", "current", "Cap uploads at 10 MiB."}) {
			t.Errorf("%s: the ruling is served as %+v", name, g)
		}
		if g := got[assumptionID]; g != (smokeLogged{"inferred", "chief_of_staff", "current", assumption}) {
			t.Errorf("%s: the assumption is served as %+v", name, g)
		}
		if len(got) != 2 {
			t.Errorf("%s: the workstream's log is %v, want the ruling and the assumption (the relay is not a decision)", name, got)
		}
		specAnchor := ""
		for _, a := range b.Anchors {
			if a.Line == "# Upload limits" {
				specAnchor = a.L1
			}
		}
		if specAnchor == "" {
			t.Errorf("%s: no spec anchor among %+v", name, b.Anchors)
		} else if raw, err := call(owner, r.role, "get_l1", map[string]any{"id": specAnchor}); err != nil {
			t.Errorf("%s: get_l1(spec): %v", name, err)
		} else {
			var doc struct {
				RawText       string `json:"raw_text"`
				ArtifactClass string `json:"artifact_class"`
				Source        struct {
					System string `json:"system"`
				} `json:"source"`
			}
			must(t, json.Unmarshal(raw, &doc))
			keep("get_l1-spec-"+name, json.RawMessage(raw))
			if !strings.Contains(doc.RawText, "Cap uploads at 10 MiB") || doc.ArtifactClass != "spec" || doc.Source.System != smokeOwnerSrc {
				t.Errorf("%s: the sealed spec is served as class %q from %q: %q", name, doc.ArtifactClass, doc.Source.System, doc.RawText)
			}
		}

		// traverse: the decision log, and the spec document, on the workstream entity.
		raw, err = traverse(owner, r.role, streamEntity, "topics", "stances")
		if err != nil {
			t.Errorf("%s: traverse(topics, stances): %v", name, err)
		} else {
			var g smokeTraversed
			must(t, json.Unmarshal(raw, &g))
			keep("traverse-workstream-decisions-"+name, json.RawMessage(raw))
			d := g.decisions()
			if d[string(stream)+"-ruling-1"].Tier != "ratified" || d[assumptionID].Tier != "inferred" {
				t.Errorf("%s: traverse serves the log as %v", name, d)
			}
		}
		raw, err = traverse(owner, r.role, streamEntity, "documents")
		if err != nil {
			t.Errorf("%s: traverse(documents): %v", name, err)
		} else {
			var g smokeTraversed
			must(t, json.Unmarshal(raw, &g))
			keep("traverse-workstream-documents-"+name, json.RawMessage(raw))
			if !slices.ContainsFunc(g.Nodes, func(n smokeNode) bool {
				return n.Type == "document" && n.Class == "spec" && n.Line == "# Upload limits"
			}) {
				t.Errorf("%s: traverse serves no spec document: %s", name, raw)
			}
		}

		// The bundle provider the turns use: the default entity of the project,
		// and the workstream's own for a workstream-tied turn.
		provider := hearsay.Provider{Local: bundle.Files{Repository: func(config.ProjectID) (*trace.Repository, error) { return f.repo, nil }},
			Config: f.s.current, Health: &hearsay.Health{}, Client: client}
		out, err := provider.Assemble(context.Background(), f.repo.Project(), bundle.Scope{Role: r.role, Workstream: stream})
		must(t, err)
		var scopes []string
		for _, m := range out.Memory {
			var s smokeBundle
			must(t, json.Unmarshal(m, &s))
			scopes = append(scopes, s.Scope.ID)
		}
		if out.Mode != hearsay.Mode || !slices.Equal(scopes, []string{entity, streamEntity}) {
			t.Errorf("%s: the turn bundle is mode %q with memory for %v (%s), want %q for %v", name, out.Mode, scopes, out.MemoryProblem, hearsay.Mode, []string{entity, streamEntity})
		}
	}

	// ---- none of the reads used the publisher's token, an extra poster or a repo opt-in ----
	reads := readLog.take()
	keep("read-requests", reads)
	for _, r := range reads {
		if r.Bearer != "owner token" || r.Principal != smokeOwner || !strings.HasPrefix(r.Agent, "osmia-") || strings.Contains(r.Agent, "publisher") {
			t.Errorf("a read was made as %+v", r)
		}
	}
	for _, p := range sent {
		if p.Kind == "project" && p.Repos != nil {
			t.Errorf("the project was posted with repos: %v", p.Raw)
		}
	}

	// ---- a principal that is neither the owner nor acting for the owner reads nothing ----
	outsider := client(config.Hearsay{URL: apiURL, Principal: "smoke-outsider", TokenEnv: "HEARSAY_SMOKE_OUTSIDER_TOKEN", Agents: smokeAgents})
	direct1 := func(scope string) (int, []byte) {
		body, _ := json.Marshal(map[string]string{"scope": scope})
		req, _ := http.NewRequest(http.MethodPost, apiURL+"/v1/get_bundle", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Hearsay-Principal", "smoke-outsider")
		req.Header.Set("Authorization", "Bearer "+smokeEnv(t, "HEARSAY_SMOKE_OUTSIDER_TOKEN"))
		resp, err := (&http.Client{Transport: direct}).Do(req)
		must(t, err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	normalize := func(raw []byte, scope string) string { return strings.ReplaceAll(string(raw), scope, "SCOPE") }
	code, unknownRaw := direct1("work:" + smokeWork + ":no-such-project")
	unknown := normalize(unknownRaw, "work:"+smokeWork+":no-such-project")
	asOutsider := map[string]any{"unknown_scope_answer": json.RawMessage(unknownRaw), "unknown_scope_status": code}
	for _, scope := range []string{entity, streamEntity} {
		code, raw := direct1(scope)
		asOutsider["direct "+scope] = json.RawMessage(raw)
		if code != http.StatusOK || normalize(raw, scope) != unknown {
			t.Errorf("the outsider reads get_bundle(%s) directly = %d %s, want what an unknown scope gets", scope, code, raw)
		}
		for _, r := range roles {
			raw, err := call(outsider, r.role, "get_bundle", map[string]any{"scope": scope})
			asOutsider[r.class+" agent acting for the outsider, "+scope] = json.RawMessage(raw)
			if err != nil || normalize(raw, scope) != unknown {
				t.Errorf("the %s agent acting for the outsider reads get_bundle(%s) = %s (%v), want what an unknown scope gets", r.class, scope, raw, err)
			}
			if raw, err := traverse(outsider, r.role, scope, "topics", "stances"); err == nil {
				t.Errorf("the %s agent acting for the outsider traversed %s: %s", r.class, scope, raw)
			} else {
				asOutsider[r.class+" traverse, "+scope] = err.Error()
			}
		}
	}
	keep("outsider-reads", asOutsider)

	// ---- the model stub never saw a decision's text ----
	// (checked by the script from the stub's log)
	if t.Failed() {
		return
	}
	keep("summary", map[string]any{
		"evidence": "container evidence: Hearsay under Compose, Osmia's publisher pass and read path in a container on its network",
		"project":  project, "work_entity": entity, "workstream_entity": streamEntity,
		"first_pass_posts": res.Posts, "second_pass_posts": res2.Posts,
		"posted": func() []string {
			var out []string
			for _, p := range sent {
				out = append(out, p.Source+" "+p.Kind+" "+p.Decision+p.Document+p.Workstream+" author="+p.Author)
			}
			return out
		}(),
	})
}
