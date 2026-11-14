package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestHearsayConfigurationRequiresDelegatedCredentialReferences(t *testing.T) {
	valid := func() Hearsay {
		return Hearsay{URL: "https://memory.example/api", Principal: "owner", TokenEnv: "OWNER_TOKEN", Agents: map[string]HearsayAgent{"worker": {ID: "worker", TokenEnv: "WORKER_TOKEN"}, "observer": {ID: "observer", TokenEnv: "OBSERVER_TOKEN"}, "orchestrator": {ID: "chief", TokenEnv: "CHIEF_TOKEN"}}}
	}
	if err := valid().validate("config.toml"); err != nil {
		t.Fatal(err)
	}
	if err := (Hearsay{}).validate("config.toml"); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Hearsay){
		func(h *Hearsay) { h.URL = "https://user:secret@memory.example" },
		func(h *Hearsay) { h.URL = "https://memory.example?token=secret" },
		func(h *Hearsay) { h.TokenEnv = "inline secret" },
		func(h *Hearsay) { delete(h.Agents, "worker") },
		func(h *Hearsay) { h.Agents["steward"] = HearsayAgent{ID: "owner", TokenEnv: "OWNER_TOKEN"} },
		func(h *Hearsay) { h.Principal = "owner\r\nHeader: secret" },
	} {
		h := valid()
		mutate(&h)
		if err := h.validate("config.toml"); err == nil {
			t.Fatal("invalid Hearsay configuration accepted")
		}
	}
}

const hearsayTop = topConfig + `
[hearsay]
url = "https://memory.example/api"
principal = "owner"
token_env = "OWNER_TOKEN"
[hearsay.agents.worker]
id = "osmia-worker"
token_env = "WORKER_TOKEN"
[hearsay.agents.orchestrator]
id = "osmia-chief"
token_env = "CHIEF_TOKEN"
[hearsay.agents.observer]
id = "osmia-observer"
token_env = "OBSERVER_TOKEN"
`

const publishTable = `
[hearsay.publish]
work_source = "osmia-work"
owner_source = "osmia-owner"
hooks_url = "https://hooks.example"
agent = "osmia-publisher"
token_env = "PUBLISHER_TOKEN"
`

func TestHearsayPublishLoadsWhenComplete(t *testing.T) {
	c, err := Load(fixture(t, hearsayTop+publishTable, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	want := HearsayPublish{WorkSource: "osmia-work", OwnerSource: "osmia-owner", HooksURL: "https://hooks.example", Agent: "osmia-publisher", TokenEnv: "PUBLISHER_TOKEN"}
	if c.Hearsay.Publish == nil || *c.Hearsay.Publish != want || !c.Hearsay.Publishing() {
		t.Fatalf("publish: %+v", c.Hearsay.Publish)
	}
}

func TestHearsayPublishAbsentDoesNotEnablePublishing(t *testing.T) {
	c, err := Load(fixture(t, hearsayTop, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Hearsay.Publish != nil || c.Hearsay.Publishing() || c.Project.HearsayDeclareRepo {
		t.Fatalf("publishing enabled by default: %+v", c.Hearsay)
	}
	if c.HearsayScope() != "" || c.HearsayReads(nil, "w_x") != nil {
		t.Fatal("reads enabled without a work source or an override")
	}
	if _, err := Load(fixture(t, topConfig, projectConfig)); err != nil {
		t.Fatal(err)
	}
}

func TestHearsayPublishFailsAtLoadNamingTheField(t *testing.T) {
	without := func(key string) string {
		var out []string
		for _, line := range strings.Split(publishTable, "\n") {
			if !strings.HasPrefix(line, key+" ") {
				out = append(out, line)
			}
		}
		return strings.Join(out, "\n")
	}
	replace := func(old, new string) string { return strings.Replace(publishTable, old, new, 1) }
	for _, tc := range []struct{ name, top, field string }{
		{"work source missing", hearsayTop + without("work_source"), "hearsay.publish.work_source"},
		{"owner source missing", hearsayTop + without("owner_source"), "hearsay.publish.owner_source"},
		{"hooks url missing", hearsayTop + without("hooks_url"), "hearsay.publish.hooks_url"},
		{"agent missing", hearsayTop + without("agent"), "hearsay.publish.agent"},
		{"token env missing", hearsayTop + without("token_env"), "hearsay.publish.token_env"},
		{"empty table", hearsayTop + "\n[hearsay.publish]\n", "hearsay.publish.work_source"},
		{"work source invalid", hearsayTop + replace(`"osmia-work"`, `"Not A Source"`), "hearsay.publish.work_source"},
		{"owner source invalid", hearsayTop + replace(`"osmia-owner"`, `"a:b"`), "hearsay.publish.owner_source"},
		{"sources equal", hearsayTop + replace(`"osmia-owner"`, `"osmia-work"`), "hearsay.publish.owner_source"},
		{"hooks url not http", hearsayTop + replace(`https://hooks.example`, `ftp://hooks.example`), "hearsay.publish.hooks_url"},
		{"hooks url credentials", hearsayTop + replace(`https://hooks.example`, `https://user:secret@hooks.example`), "hearsay.publish.hooks_url"},
		{"hooks url query", hearsayTop + replace(`https://hooks.example`, `https://hooks.example?token=secret`), "hearsay.publish.hooks_url"},
		{"agent is the owner", hearsayTop + replace(`"osmia-publisher"`, `"owner"`), "hearsay.publish.agent"},
		{"agent is a class agent", hearsayTop + replace(`"osmia-publisher"`, `"osmia-worker"`), "hearsay.publish.agent"},
		{"agent invalid", hearsayTop + replace(`"osmia-publisher"`, `"a\nb"`), "hearsay.publish.agent"},
		{"token env invalid", hearsayTop + replace(`"PUBLISHER_TOKEN"`, `"not a name"`), "hearsay.publish.token_env"},
		{"token env shared with the owner", hearsayTop + replace(`"PUBLISHER_TOKEN"`, `"OWNER_TOKEN"`), "hearsay.publish.token_env"},
		{"token env shared with a class agent", hearsayTop + replace(`"PUBLISHER_TOKEN"`, `"WORKER_TOKEN"`), "hearsay.publish.token_env"},
		{"unknown key", hearsayTop + publishTable + "extra = \"x\"\n", "hearsay.publish.extra"},
		{"token value inline", hearsayTop + publishTable + "token = \"s3cret-value\"\n", "hearsay.publish.token"},
		{"wrong type", hearsayTop + replace(`"https://hooks.example"`, `7`), "hearsay.publish.hooks_url"},
		{"without hearsay url", topConfig + publishTable, "hearsay.url"},
	} {
		_, err := Load(fixture(t, tc.top, projectConfig))
		var field *FieldError
		// The decoder names a mistyped key by its own spelling of the path.
		if !errors.As(err, &field) || field.Field != tc.field && !(tc.name == "wrong type" && strings.HasSuffix(field.Field, "hooks_url")) {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: error carries a value: %v", tc.name, err)
		}
	}
}

func TestHearsayRepoOptInDefaultsOffAndNamesItsField(t *testing.T) {
	c, err := Load(fixture(t, topConfig, projectConfig+"hearsay_declare_repo = true\n"))
	if err != nil || !c.Project.HearsayDeclareRepo {
		t.Fatalf("opt-in: %+v %v", c.Project, err)
	}
	_, err = Load(fixture(t, topConfig, projectConfig+"hearsay_declare_repo = \"yes\"\n"))
	var field *FieldError
	if !errors.As(err, &field) || field.Field != "hearsay_declare_repo" {
		t.Fatalf("wrong type: %v", err)
	}
}

func TestHearsayScopeAndEntitiesMustBeEntityIDs(t *testing.T) {
	for _, tc := range []struct{ name, project, field string }{
		{"old scope name", projectConfig + "hearsay_scope = \"osmia-ab12\"\n", "hearsay_scope"},
		{"old generated name", projectConfig + "hearsay_scope = \"osmia-0123456789abcdef-0123456789abcdef\"\n", "hearsay_scope"},
		{"whitespace", projectConfig + "hearsay_scope = \"work:a b\"\n", "hearsay_scope"},
		{"empty body", projectConfig + "hearsay_scope = \"work:\"\n", "hearsay_scope"},
		{"old entity scope name", projectConfig + "[hearsay_entities]\nstorage = \"osmia-ab12\"\n", "hearsay_entities"},
	} {
		_, err := Load(fixture(t, topConfig, tc.project))
		var field *FieldError
		if !errors.As(err, &field) || field.Field != tc.field {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	c, err := Load(fixture(t, topConfig, projectConfig+"hearsay_scope = \"code:owner/repo\"\n[hearsay_entities]\nstorage = \"code:owner/repo:storage\"\n"))
	if err != nil || c.Project.HearsayScope != "code:owner/repo" {
		t.Fatalf("entity ids: %v", err)
	}
}

func TestHearsayReadsResolveEntityIDs(t *testing.T) {
	id := ProjectID(pid)
	cfg := func(scope string, entities map[string]string, publish bool) *Config {
		c := &Config{Hearsay: Hearsay{URL: "https://memory.example"}, Project: Project{ID: id, HearsayScope: scope, HearsayEntities: entities}}
		if publish {
			c.Hearsay.Publish = &HearsayPublish{WorkSource: "osmia-work"}
		}
		return c
	}
	base := "work:osmia-work:" + pid
	for _, tc := range []struct {
		name      string
		cfg       *Config
		footprint []string
		stream    WorkstreamID
		want      []string
	}{
		{"project-wide default", cfg("", nil, true), nil, "", []string{base}},
		{"workstream-tied default", cfg("", nil, true), nil, "w_1", []string{base, base + "#w_1"}},
		{"override replaces the project default", cfg("code:owner/repo", nil, true), nil, "", []string{"code:owner/repo"}},
		{"override beside the workstream entity", cfg("code:owner/repo", nil, true), nil, "w_1", []string{"code:owner/repo", base + "#w_1"}},
		{"override without a work source", cfg("code:owner/repo", nil, false), nil, "w_1", []string{"code:owner/repo"}},
		{"mapped footprint", cfg("", map[string]string{"storage": "code:storage"}, true), []string{"storage", "other"}, "w_1", []string{"code:storage", base + "#w_1"}},
		{"unmapped footprint", cfg("", map[string]string{"storage": "code:storage"}, true), []string{"other"}, "", []string{base}},
		{"no work source and no override", cfg("", nil, false), nil, "w_1", nil},
		{"no url", &Config{Project: Project{ID: id, HearsayScope: "code:x"}}, nil, "", nil},
	} {
		if got := tc.cfg.HearsayReads(tc.footprint, tc.stream); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
