package service

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/redact"
	"github.com/kpenfound/osmia/internal/trace"
)

const hearsayTables = `
[hearsay]
url = "https://memory.example/api"
principal = "owner"
token_env = "TEST_HEARSAY_OWNER_TOKEN"
[hearsay.agents.worker]
id = "osmia-worker"
token_env = "TEST_HEARSAY_WORKER_TOKEN"
[hearsay.agents.orchestrator]
id = "osmia-chief"
token_env = "TEST_HEARSAY_CHIEF_TOKEN"
[hearsay.agents.observer]
id = "osmia-observer"
token_env = "TEST_HEARSAY_OBSERVER_TOKEN"
[hearsay.publish]
work_source = "osmia-work"
owner_source = "osmia-owner"
hooks_url = "https://hooks.example"
agent = "osmia-publisher"
token_env = "TEST_HEARSAY_PUBLISHER_TOKEN"
`

// The publisher's token value is read from the environment only to be
// registered. It never reaches the configuration API, a load or reload error,
// or the service log.
func TestPublisherTokenNeverAppearsInConfigResponsesErrorsOrLogs(t *testing.T) {
	const token = "publisher-token-0f9e8d7c6b5a"
	t.Setenv("TEST_HEARSAY_PUBLISHER_TOKEN", token)
	opts := fixture(t)
	var logs bytes.Buffer
	opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	files := configFiles(t, opts)
	hooks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("a post did not carry the publisher's token")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer hooks.Close()
	tables := strings.Replace(hearsayTables, "https://hooks.example", hooks.URL, 1)
	files.write(t, files.topText+tables)
	s, c := start(t, opts)
	ctx := context.Background()

	if !slices.Contains(redact.KnownSecrets(), token) {
		t.Fatal("the publisher token is not registered for redaction")
	}
	view, err := c.Configuration(ctx)
	fixturekit.Must(t, err)
	raw, err := json.Marshal(view)
	fixturekit.Must(t, err)
	if strings.Contains(string(raw), token) || !strings.Contains(string(raw), `"token_env":"TEST_HEARSAY_PUBLISHER_TOKEN"`) || view.Effective.Hearsay.Publish == nil {
		t.Fatalf("configuration response: %s", raw)
	}
	if !s.current().Hearsay.Publishing() {
		t.Fatal("a complete publish table did not enable publishing")
	}

	bad := strings.Replace(files.topText+tables, `hooks_url = "`+hooks.URL+`"`, `hooks_url = "https://user:`+token+`@hooks.example"`, 1)
	files.write(t, bad)
	_, err = c.Reload(ctx)
	if err == nil || !strings.Contains(err.Error(), "hearsay.publish.hooks_url") || strings.Contains(err.Error(), token) {
		t.Fatalf("reload error: %v", err)
	}
	view, err = c.Configuration(ctx)
	fixturekit.Must(t, err)
	raw, _ = json.Marshal(view)
	if strings.Contains(string(raw), token) || view.LastError == nil || view.LastError.Field != "hearsay.publish.hooks_url" {
		t.Fatalf("configuration after the failed reload: %s", raw)
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("the service log carries the token:\n%s", logs.String())
	}
}

func TestPublishAbsentLeavesConfigurationUnpublished(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	files := configFiles(t, opts)
	withoutPublish, _, _ := strings.Cut(hearsayTables, "[hearsay.publish]")
	files.write(t, files.topText+withoutPublish)
	s, c := start(t, opts)
	view, err := c.Configuration(context.Background())
	fixturekit.Must(t, err)
	if view.Effective.Hearsay.URL == "" || view.Effective.Hearsay.Publish != nil || view.Effective.Hearsay.Publishing() || s.current().HearsayScope() != "" {
		t.Fatalf("publishing enabled without the table: %+v", view.Effective.Hearsay)
	}
}

func memoryTurnConfig(t *testing.T, hearsay config.Hearsay, scope string) (*config.Config, *trace.Repository) {
	t.Helper()
	_, cfg := conversationFixture(t, "memory-tools-")
	repo, err := trace.Open(cfg.Root, cfg.Project)
	fixturekit.Must(t, err)
	t.Cleanup(func() { repo.Close() })
	cfg.Hearsay = hearsay
	cfg.Project.HearsayScope = scope
	cfg.Projects = slices.Clone(cfg.Projects)
	cfg.Projects[0] = cfg.Project
	return cfg, repo
}

func memoryTurnTools(t *testing.T, cfg *config.Config, repo *trace.Repository) (map[string][]string, map[string][]string) {
	t.Helper()
	turns := memoryTurns(&isolation.Turns{Grants: map[string]coreadapter.Capabilities{
		trace.ChiefOfStaff: {}, masonRole: {}, reviewerRole: {}, architectRole: {}, committeeRole: {},
		sessionRole: {Tools: []string{"show_image"}},
	}}, cfg, repo)
	granted, scoped := map[string][]string{}, map[string][]string{}
	for role, grant := range turns.Grants {
		granted[role] = grant.Tools
		tools, err := turns.Scoped(context.Background(), coreadapter.Scope{Project: string(repo.Project()), Role: role})
		fixturekit.Must(t, err)
		for _, tool := range tools {
			scoped[role] = append(scoped[role], tool.Name)
		}
	}
	return granted, scoped
}

var hearsayReadTools = []string{"get_bundle", "resolve", "stance_history", "get_l1", "get_l0", "search", "traverse"}

func TestHearsayReadToolsFollowTheResolvedEntityAndTheRoleClass(t *testing.T) {
	t.Parallel()
	publishing := config.Hearsay{URL: "https://memory.example", Principal: "owner", TokenEnv: "OWNER", Agents: map[string]config.HearsayAgent{
		"worker": {ID: "w", TokenEnv: "W"}, "orchestrator": {ID: "o", TokenEnv: "O"}, "observer": {ID: "b", TokenEnv: "B"}},
		Publish: &config.HearsayPublish{WorkSource: "osmia-work", OwnerSource: "osmia-owner", HooksURL: "https://hooks.example", Agent: "p", TokenEnv: "P"}}
	cfg, repo := memoryTurnConfig(t, publishing, "")
	granted, scoped := memoryTurnTools(t, cfg, repo)
	for role, names := range map[string]map[string][]string{"granted": granted, "scoped": scoped} {
		for _, class := range []struct {
			role   string
			assert bool
		}{{trace.ChiefOfStaff, true}, {masonRole, true}, {reviewerRole, true}, {architectRole, true}, {committeeRole, true}, {sessionRole, false}} {
			for _, read := range hearsayReadTools {
				if !slices.Contains(names[class.role], read) {
					t.Errorf("%s %s lacks %s: %v", role, class.role, read, names[class.role])
				}
			}
			if slices.Contains(names[class.role], "assert") != class.assert || slices.Contains(names[class.role], "watch") {
				t.Errorf("%s %s write tools: %v", role, class.role, names[class.role])
			}
		}
		if slices.Contains(names[sessionRole], projectContextTool) {
			t.Errorf("%s session: %v", role, names[sessionRole])
		}
	}
	if !slices.Contains(granted[sessionRole], "show_image") {
		t.Fatalf("the session lost its own tool: %v", granted[sessionRole])
	}

	override := publishing
	override.Publish = nil
	cfg, repo = memoryTurnConfig(t, override, "code:owner/repo")
	granted, _ = memoryTurnTools(t, cfg, repo)
	if !slices.Contains(granted[masonRole], "traverse") || !slices.Contains(granted[sessionRole], "get_bundle") {
		t.Fatalf("override grants: %v", granted)
	}

	cfg, repo = memoryTurnConfig(t, override, "")
	granted, scoped = memoryTurnTools(t, cfg, repo)
	for role, names := range map[string][]string{"granted": slices.Concat(granted[masonRole], granted[sessionRole], granted[trace.ChiefOfStaff]), "scoped": slices.Concat(scoped[masonRole], scoped[sessionRole])} {
		for _, name := range names {
			if slices.Contains(hearsayReadTools, name) || name == "assert" {
				t.Errorf("%s: file mode granted %s", role, name)
			}
		}
	}
	if slices.Contains(granted[masonRole], "traverse") {
		t.Fatalf("file mode grants: %v", granted)
	}
}
