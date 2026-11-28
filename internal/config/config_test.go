package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const pid = "p_0123456789abcdef0123456789abcdef"
const wid = "w_0123456789abcdef0123456789abcdef"
const topConfig = `version = 1
[profiles.default]
agent = "claude"
model = "test-model"
`
const projectConfig = `version = 1
upstream = "upstream/repo"
fork = "owner/repo"
clone = "~/clone"
`

func fixture(t *testing.T, top, project string) Options {
	t.Helper()
	home, err := os.MkdirTemp("", "oc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	opts := Options{Home: home}
	for _, d := range []string{rootDirectory(opts), filepath.Dir(configFile(opts))} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, configFile(opts), top)
	write(t, projectFile(opts), project)
	return opts
}

func load(opts Options) (*Config, error) {
	top, err := LoadTopLevel(opts)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(projectFile(opts))
	if err != nil {
		return nil, err
	}
	file, err := top.DecodeProject(pid, opts.Home, data)
	if err != nil {
		return nil, err
	}
	return top.WithLoadedProjects([]Project{file.Project}), nil
}

func rootDirectory(o Options) string { return filepath.Join(o.Home, ".local", "share", "osmia") }
func configFile(o Options) string    { return filepath.Join(o.Home, ".config", "osmia", "config.toml") }
func projectFile(o Options) string   { return filepath.Join(o.Home, "project.toml") }
func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestDefaults(t *testing.T) {
	opts := fixture(t, topConfig, projectConfig)
	c, err := load(opts)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := ResolveRoot("", opts.Home)
	if c.Root != root || c.Listen.Socket != filepath.Join(root.String(), "osmia.sock") {
		t.Fatalf("root/socket: %+v", c)
	}
	if c.Capacity != (Capacity{Masons: 4, Reviewers: 2, Committee: 3, PerWorkstream: 2, Checks: 2, Manager: 3}) || c.Shed != (Shed{3, 3}) || c.Mason.MaxCleanTurns != 3 {
		t.Fatalf("numeric defaults: %+v", c)
	}
	if c.Project.ID != pid || c.Project.BaseBranch != "main" || c.Project.Landing != "commit-per-unit" || c.Project.Capacity.PerWorkstream != 2 {
		t.Fatalf("project: %+v", c.Project)
	}
	if c.EventWindow() != 5*time.Second {
		t.Fatalf("event window default: %v", c.EventWindow())
	}
	if c.Jev != (Jev{URL: "https://openrouter.ai/api", Model: "~typesafe/jev-latest", APIKeyEnv: "OPENROUTER_API_KEY", Timeout: "10s", CheckThreshold: 0.5, DecisionThreshold: 0.9, OwnerThreshold: 0.9}) || c.Jev.Settings().Timeout != 10*time.Second || c.Jev.Settings().Validate() != nil {
		t.Fatalf("jev defaults: %+v", c.Jev)
	}
	if c.Project.UpstreamRebase != "6h" || c.Project.RebaseInterval() != 6*time.Hour {
		t.Fatalf("upstream rebase default: %q %v", c.Project.UpstreamRebase, c.Project.RebaseInterval())
	}
	if c.Project.ChecksTimeout != "15m" || c.Project.CheckTimeout() != 15*time.Minute {
		t.Fatalf("checks timeout default: %q %v", c.Project.ChecksTimeout, c.Project.CheckTimeout())
	}
	if c.Project.Capacity.GitOperations != 4 {
		t.Fatalf("git operation default: %+v", c.Project.Capacity)
	}
	if c.Profiles["default"].Timeout != "45m" || c.Profiles["default"].Effort != "medium" || len(c.Roles) != 7 {
		t.Fatalf("profile defaults: %+v", c)
	}
	p, settings, err := c.Execution("mason", "default")
	if err != nil || p.Timeout != 45*time.Minute || p.Backend != "claude" || settings.Mode != "none" {
		t.Fatalf("adapter inputs: %+v %+v %v", p, settings, err)
	}
	if _, err := os.Stat(c.Listen.Socket); !os.IsNotExist(err) {
		t.Fatalf("loader created socket: %v", err)
	}
	if _, err := os.Stat(c.Project.Clone); !os.IsNotExist(err) {
		t.Fatalf("loader created clone: %v", err)
	}
}

func TestJevSettingsAlwaysEnableTheJudge(t *testing.T) {
	c, err := load(fixture(t, topConfig+"[jev]\nmodel = 'jev-1.13.0'\ntimeout = '3s'\n", projectConfig))
	if err != nil || !c.Jev.Settings().Enabled || c.Jev.Model != "jev-1.13.0" || c.Jev.URL != DefaultJevURL || c.Jev.APIKeyEnv != DefaultJevAPIKeyEnv || c.Jev.Settings().Timeout != 3*time.Second {
		t.Fatalf("jev: %+v %v", c.Jev, err)
	}
}

func TestTranscribeDefaultsToOff(t *testing.T) {
	c, err := load(fixture(t, topConfig, projectConfig))
	if err != nil || c.Transcribe != (Transcribe{Model: "large-v3-turbo", APIKeyEnv: "WHISPER_API_KEY"}) || c.Transcribe.Enabled() {
		t.Fatalf("transcribe defaults: %+v %v", c.Transcribe, err)
	}
	c, err = load(fixture(t, topConfig+"[transcribe]\nurl = ''\n", projectConfig))
	if err != nil || c.Transcribe.Enabled() {
		t.Fatalf("an empty url turns transcription off: %+v %v", c.Transcribe, err)
	}
}

func TestTranscribeSettings(t *testing.T) {
	const url = "http://whisper.tail1234.ts.net:8000/v1/audio/transcriptions"
	c, err := load(fixture(t, topConfig+"[transcribe]\nurl = '"+url+"'\n", projectConfig))
	if err != nil || c.Transcribe != (Transcribe{URL: url, Model: DefaultTranscribeModel, APIKeyEnv: DefaultTranscribeAPIKeyEnv}) || !c.Transcribe.Enabled() {
		t.Fatalf("a url alone keeps the other defaults: %+v %v", c.Transcribe, err)
	}
	c, err = load(fixture(t, topConfig+"[transcribe]\nurl = 'https://whisper.example/v1/audio/transcriptions'\nmodel = 'whisper-1'\napi_key_env = 'MY_WHISPER_KEY'\n", projectConfig))
	if err != nil || c.Transcribe != (Transcribe{URL: "https://whisper.example/v1/audio/transcriptions", Model: "whisper-1", APIKeyEnv: "MY_WHISPER_KEY"}) {
		t.Fatalf("transcribe: %+v %v", c.Transcribe, err)
	}
}

func TestJevCheckThreshold(t *testing.T) {
	for _, c := range []struct {
		value string
		want  float64
	}{{"0.7", 0.7}, {"1", 1}} {
		cfg, err := load(fixture(t, topConfig+"[jev]\ncheck_threshold = "+c.value+"\n", projectConfig))
		if err != nil || cfg.Jev.CheckThreshold != c.want {
			t.Fatalf("check_threshold = %s: %+v %v", c.value, cfg, err)
		}
	}
}

func TestJevDecisionThreshold(t *testing.T) {
	for _, c := range []struct {
		value string
		want  float64
	}{{"0.75", 0.75}, {"1", 1}} {
		cfg, err := load(fixture(t, topConfig+"[jev]\ndecision_threshold = "+c.value+"\n", projectConfig))
		if err != nil || cfg.Jev.DecisionThreshold != c.want {
			t.Fatalf("decision_threshold = %s: %+v %v", c.value, cfg, err)
		}
	}
	cfg, err := load(fixture(t, topConfig, projectConfig))
	if err != nil || cfg.Jev.DecisionThreshold != 0.9 {
		t.Fatalf("default decision threshold = %v %v", cfg.Jev.DecisionThreshold, err)
	}
}

// owner_threshold loads with a default of 0.9 and is independent of
// decision_threshold.
func TestJevOwnerThreshold(t *testing.T) {
	for _, c := range []struct {
		value string
		want  float64
	}{{"0.95", 0.95}, {"1", 1}, {"0.4", 0.4}} {
		cfg, err := load(fixture(t, topConfig+"[jev]\ndecision_threshold = 0.7\nowner_threshold = "+c.value+"\n", projectConfig))
		if err != nil || cfg.Jev.OwnerThreshold != c.want || cfg.Jev.DecisionThreshold != 0.7 {
			t.Fatalf("owner_threshold = %s: %+v %v", c.value, cfg.Jev, err)
		}
	}
	cfg, err := load(fixture(t, topConfig+"[jev]\ndecision_threshold = 0.7\n", projectConfig))
	if err != nil || cfg.Jev.OwnerThreshold != 0.9 || cfg.Jev.DecisionThreshold != 0.7 {
		t.Fatalf("default owner threshold = %+v %v", cfg.Jev, err)
	}
}

// loop.stall_after loads with a default of 45m; zero turns the stall watchdog
// off and leaves the window a failing operation is retried for at 45m; a
// neighbouring key that does not exist is still refused.
func TestLoopStallAfter(t *testing.T) {
	cfg, err := load(fixture(t, topConfig, projectConfig))
	if err != nil || cfg.Loop.StallAfter != "45m" || cfg.StallWindow() != 45*time.Minute || cfg.GiveUpWindow() != 45*time.Minute || cfg.Loop.MaxSessions != 30 {
		t.Fatalf("default stall_after: %+v %v", cfg.Loop, err)
	}
	for _, c := range []struct {
		value         string
		stall, giveUp time.Duration
	}{{"20m", 20 * time.Minute, 20 * time.Minute}, {"1h30m", 90 * time.Minute, 90 * time.Minute}, {"0", 0, 45 * time.Minute}, {"0s", 0, 45 * time.Minute}} {
		cfg, err := load(fixture(t, topConfig+"[loop]\nstall_after = '"+c.value+"'\n", projectConfig))
		if err != nil || cfg.StallWindow() != c.stall || cfg.GiveUpWindow() != c.giveUp || cfg.Loop.MaxSessions != 30 {
			t.Fatalf("stall_after = %q: %+v %v", c.value, cfg, err)
		}
	}
	for _, c := range []struct{ name, loop, want string }{
		{"negative", "stall_after = '-5m'", "loop.stall_after"},
		{"not a duration", "stall_after = 'soon'", "loop.stall_after"},
		{"not a string", "stall_after = true", "loop.stall_after"},
		{"neighbouring key", "stall_afterr = '45m'", "loop.stall_afterr: unknown configuration key"},
		{"another neighbouring key", "stall = '45m'", "loop.stall: unknown configuration key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := load(fixture(t, topConfig+"[loop]\n"+c.loop+"\n", projectConfig))
			if cfg != nil || err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got config=%+v err=%v; want nil and %q", cfg, err, c.want)
			}
		})
	}
}

func TestBudgetLimits(t *testing.T) {
	c, err := load(fixture(t, topConfig+"[budget]\nper_session = '5.00'\nper_unit = '40.00'\nper_day = '150.00'\n", projectConfig))
	if err != nil || c.Budget.SessionLimitUSD() != 5 || c.Budget.PerUnit != "40.00" || c.Budget.PerDay != "150.00" {
		t.Fatalf("budget: %+v %v", c, err)
	}
	p, _, err := c.Execution("mason", "default")
	if err != nil || p.CostLimitUSD != 5 {
		t.Fatalf("execution cap: %+v %v", p, err)
	}
	for _, field := range []string{"per_session", "per_unit", "per_day"} {
		for _, value := range []string{"'0'", "'-1'", "'abc'", "'1e3'", "'NaN'", "''"} {
			_, err := load(fixture(t, topConfig+"[budget]\n"+field+" = "+value+"\n", projectConfig))
			if err == nil || !strings.Contains(err.Error(), "budget."+field) {
				t.Fatalf("%s %s: %v", field, value, err)
			}
		}
	}
	without, err := load(fixture(t, topConfig, projectConfig))
	if err != nil || without.Budget.SessionLimitUSD() != 0 {
		t.Fatalf("absent budget: %+v %v", without, err)
	}
}

func TestExplicit(t *testing.T) {
	top := topConfig + `effort = "high"
timeout = "2m"
max_turns = 8
fallback = "backup"
[profiles.backup]
agent = "codex"
model = "other-model"
[listen]
socket = "local.sock"
[capacity]
masons = 1
checks = 7
manager = 5
reviewers = 1
committee = 1
per_workstream = 5
[shed]
max_rounds = 1
max_bounces = 2
[mason]
max_clean_turns = 2
[events]
window = "250ms"
[roles.mason]
profile = "default"
sandbox = "container"
image = "test-image:1"
`
	opts := fixture(t, top, projectConfig+`name = "A display name"
base_branch = "release/next"
landing = "squash"
[capacity]
per_workstream = 3
`)
	c, err := load(opts)
	if err != nil {
		t.Fatal(err)
	}
	if c.Capacity != (Capacity{Masons: 1, Reviewers: 1, Committee: 1, PerWorkstream: 5, Checks: 7, Manager: 5}) || c.Shed != (Shed{1, 2}) || c.Project.Capacity.PerWorkstream != 3 || c.Project.Landing != "squash" || c.Project.BaseBranch != "release/next" || c.Project.Name != "A display name" {
		t.Fatalf("overrides: %+v", c)
	}
	if c.EventWindow() != 250*time.Millisecond {
		t.Fatalf("event window: %v", c.EventWindow())
	}
	p, settings, err := c.Execution("mason", "backup")
	if err != nil || p.Backend != "codex" || settings.Mode != "container" || settings.Image != "test-image:1" {
		t.Fatalf("fallback: %+v %+v %v", p, settings, err)
	}
	if !strings.HasSuffix(c.Listen.Socket, "/local.sock") {
		t.Fatal(c.Listen.Socket)
	}
	if _, _, err := c.Execution("bogus", "default"); err == nil {
		t.Fatal("unknown role accepted")
	}
	if p, err := c.NamedProfile("backup"); err != nil || p.Name != "backup" || p.Backend != "codex" || p.Model != "other-model" || p.Effort != "medium" || p.Timeout != 45*time.Minute {
		t.Fatalf("named profile: %+v %v", p, err)
	}
	if p, err := c.NamedProfile("default"); err != nil || p.Effort != "high" || p.Timeout != 2*time.Minute || p.MaxTurns != 8 {
		t.Fatalf("named default profile: %+v %v", p, err)
	}
	if _, err := c.NamedProfile("missing"); err == nil {
		t.Fatal("unknown named profile accepted")
	}
	if _, _, err := c.Execution("mason", "missing"); err == nil {
		t.Fatal("unknown profile accepted")
	}
}
func TestLocalSlotsAreUnsetByDefaultAndMayBeZero(t *testing.T) {
	for name, tc := range map[string]struct {
		extra string
		want  *int
	}{
		"unset": {"", nil},
		"zero":  {"[capacity]\nlocal_slots = 0\n", new(0)},
		"three": {"[capacity]\nlocal_slots = 3\n", new(3)},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := load(fixture(t, topConfig+tc.extra, projectConfig))
			if err != nil {
				t.Fatal(err)
			}
			got := c.Capacity.LocalSlots
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Fatalf("local slots %v, want %v", got, tc.want)
			}
			if c.Capacity.Masons != 4 || c.Capacity.PerWorkstream != 2 {
				t.Fatalf("role limits changed: %+v", c.Capacity)
			}
		})
	}
}

func TestInvalid(t *testing.T) {
	cases := []struct{ name, top, project, want string }{
		{"syntax", "version = [", "", "config.toml:"},
		{"version", strings.Replace(topConfig, "version = 1", "version = 2", 1), "", "version:"},
		{"missing version", strings.Replace(topConfig, "version = 1", "", 1), "", "version:"},
		{"unknown", topConfig + "oops = 1\n", "", "profiles.default.oops"},
		{"empty unknown table", topConfig + "[unexpected]\n", "", "unexpected"},
		{"internal field", "Root = 'oops'\n" + topConfig, "", "Root"},
		{"key case", strings.Replace(topConfig, "model =", "Model =", 1), "", "profiles.default.Model"},
		{"socket state collision", topConfig + "[listen]\nsocket = 'runtime.json'\n", "", "direct child"},
		{"duplicate key", topConfig + "model = 'other'\n", "", "config.toml:"},

		{"zero capacity", topConfig + "[capacity]\nmasons = 0\n", "", "capacity.masons"},
		{"negative reviews", topConfig + "[capacity]\nreviewers = -1\n", "", "capacity.reviewers"},
		{"zero checks", topConfig + "[capacity]\nchecks = 0\n", "", "capacity.checks"},
		{"negative checks", topConfig + "[capacity]\nchecks = -1\n", "", "capacity.checks"},
		{"zero manager", topConfig + "[capacity]\nmanager = 0\n", "", "capacity.manager"},
		{"negative manager", topConfig + "[capacity]\nmanager = -1\n", "", "capacity.manager"},
		{"zero deprecated chief", topConfig + "[capacity]\nchief = 0\n", "", "capacity.chief"},
		{"negative deprecated chief", topConfig + "[capacity]\nchief = -1\n", "", "capacity.chief"},
		{"zero committee", topConfig + "[capacity]\ncommittee = 0\n", "", "capacity.committee"},
		{"zero wip", topConfig + "[capacity]\nper_workstream = 0\n", "", "capacity.per_workstream"},
		{"negative local slots", topConfig + "[capacity]\nlocal_slots = -1\n", "", "capacity.local_slots"},
		{"local slots wrong type", topConfig + "[capacity]\nlocal_slots = 'all'\n", "", "config.toml:"},
		{"negative rounds", topConfig + "[shed]\nmax_rounds = -1\n", "", "shed.max_rounds"},
		{"zero bounces", topConfig + "[shed]\nmax_bounces = 0\n", "", "shed.max_bounces"},
		{"zero clean turns", topConfig + "[mason]\nmax_clean_turns = 0\n", "", "mason.max_clean_turns"},
		{"no perspectives", topConfig + "[committee]\nperspectives = []\n", "", "committee.perspectives"},
		{"unknown perspective", topConfig + "[committee]\nperspectives = ['scope', 'style']\n", "", "committee.perspectives[1]"},
		{"unknown committee profile", topConfig + "[committee]\nprofiles = ['default', 'missing']\n", "", "committee.profiles[1]: unknown profile missing"},
		{"committee profile sandbox", topConfig + "[profiles.other]\nagent = 'codex'\nmodel = 'test'\n[roles.committee]\nsandbox = 'claude'\n[committee]\nprofiles = ['other']\n", "", "committee.profiles[0]: claude sandbox"},
		{"unknown committee key", topConfig + "[committee]\nsize = 3\n", "", "committee.size"},
		{"zero event window", topConfig + "[events]\nwindow = \"0s\"\n", "", "events.window"},
		{"malformed event window", topConfig + "[events]\nwindow = \"soon\"\n", "", "events.window"},
		{"unknown events key", topConfig + "[events]\nlimit = 1\n", "", "events.limit"},
		{"wrong type", topConfig + "[capacity]\nmasons = 'many'\n", "", "config.toml:"},
		{"backend", strings.Replace(topConfig, "claude", "unknown", 1), "", "profiles.default.agent"},
		{"model", strings.Replace(topConfig, "test-model", " ", 1), "", "profiles.default.model"},
		{"effort", topConfig + "effort = 'unknown'\n", "", "profiles.default.effort"},
		{"timeout", topConfig + "timeout = '0s'\n", "", "profiles.default.timeout"},
		{"empty timeout", topConfig + "timeout = ''\n", "", "profiles.default.timeout"},
		{"negative turns", topConfig + "max_turns = -1\n", "", "profiles.default.max_turns"},
		{"unsupported turns", strings.Replace(topConfig, "claude", "codex", 1) + "max_turns = 1\n", "", "only claude"},
		{"missing fallback", topConfig + "fallback = 'missing'\n", "", "profiles.default.fallback"},
		{"self cycle", topConfig + "fallback = 'default'\n", "", "fallback cycle"},
		{"indirect cycle", topConfig + "fallback = 'other'\n[profiles.other]\nagent = 'codex'\nmodel = 'test'\nfallback = 'default'\n", "", "fallback cycle"},
		{"unknown role", topConfig + "[roles.fake]\nprofile = 'default'\n", "", "roles.fake"},
		{"designer events profile", topConfig + "[roles.designer]\nevents_profile = 'default'\n", "", "roles.designer.events_profile"},
		{"designer missing binding", topConfig + "[roles.designer]\nprofile = 'missing'\n", "", "roles.designer.profile"},
		{"missing binding", topConfig + "[roles.mason]\nprofile = 'missing'\n", "", "roles.mason.profile"},
		{"invalid sandbox", topConfig + "[roles.mason]\nsandbox = 'unsafe'\n", "", "roles.mason.sandbox"},
		{"missing image", topConfig + "[roles.mason]\nsandbox = 'container'\n", "", "roles.mason.image"},
		{"unused image", topConfig + "[roles.mason]\nimage = 'image'\n", "", "roles.mason.image"},
		{"fallback sandbox", topConfig + "fallback = 'other'\n[profiles.other]\nagent = 'codex'\nmodel = 'test'\n[roles.mason]\nsandbox = 'claude'\n", "", "entire fallback chain"},
		{"tailnet uppercase", topConfig + "[listen]\ntailnet = 'Osmia'\n", "", "listen.tailnet"},
		{"tailnet dotted", topConfig + "[listen]\ntailnet = 'osmia.example'\n", "", "listen.tailnet"},
		{"tailnet underscore", topConfig + "[listen]\ntailnet = 'my_osmia'\n", "", "listen.tailnet"},
		{"tailnet space", topConfig + "[listen]\ntailnet = 'my osmia'\n", "", "listen.tailnet"},
		{"tailnet leading hyphen", topConfig + "[listen]\ntailnet = '-osmia'\n", "", "listen.tailnet"},
		{"tailnet trailing hyphen", topConfig + "[listen]\ntailnet = 'osmia-'\n", "", "listen.tailnet"},
		{"tailnet long", topConfig + "[listen]\ntailnet = '" + strings.Repeat("a", 64) + "'\n", "", "listen.tailnet"},
		{"tailnet wrong type", topConfig + "[listen]\ntailnet = true\n", "", "config.toml:"},
		{"web empty host", topConfig + "[listen]\nweb = ':8080'\n", "", "listen.web"},
		{"web any address", topConfig + "[listen]\nweb = '0.0.0.0:8080'\n", "", "listen.web"},
		{"web any ipv6", topConfig + "[listen]\nweb = '[::]:8080'\n", "", "listen.web"},
		{"web lan address", topConfig + "[listen]\nweb = '192.168.1.10:8080'\n", "", "listen.web"},
		{"web hostname", topConfig + "[listen]\nweb = 'osmia.example:8080'\n", "", "listen.web"},
		{"web localhost suffix", topConfig + "[listen]\nweb = 'localhost.example:8080'\n", "", "listen.web"},
		{"web zoned", topConfig + "[listen]\nweb = '[fe80::1%lo0]:8080'\n", "", "listen.web"},
		{"web no port", topConfig + "[listen]\nweb = '127.0.0.1'\n", "", "listen.web"},
		{"web named port", topConfig + "[listen]\nweb = '127.0.0.1:http'\n", "", "listen.web"},
		{"web large port", topConfig + "[listen]\nweb = '127.0.0.1:65536'\n", "", "listen.web"},
		{"web signed port", topConfig + "[listen]\nweb = '127.0.0.1:+80'\n", "", "listen.web"},
		{"web wrong type", topConfig + "[listen]\nweb = 8080\n", "", "config.toml:"},
		{"hearsay incomplete", topConfig + "[hearsay]\nprincipal = 'owner'\n", "", "required when Hearsay settings are present"},
		{"jev url scheme", topConfig + "[jev]\nurl = 'ftp://openrouter.ai/api'\n", "", "jev.url"},
		{"jev url credentials", topConfig + "[jev]\nurl = 'https://key@openrouter.ai/api'\n", "", "jev.url"},
		{"jev model blank", topConfig + "[jev]\nmodel = ' '\n", "", "jev.model"},
		{"jev key value", topConfig + "[jev]\napi_key_env = 'sk-or-123'\n", "", "jev.api_key_env"},
		{"jev timeout long", topConfig + "[jev]\ntimeout = '5m'\n", "", "jev.timeout"},
		{"jev timeout zero", topConfig + "[jev]\ntimeout = '0s'\n", "", "jev.timeout"},
		{"jev check threshold zero", topConfig + "[jev]\ncheck_threshold = 0\n", "", "jev.check_threshold"},
		{"jev check threshold negative", topConfig + "[jev]\ncheck_threshold = -0.1\n", "", "jev.check_threshold"},
		{"jev check threshold above one", topConfig + "[jev]\ncheck_threshold = 1.5\n", "", "jev.check_threshold"},
		{"jev check threshold nan", topConfig + "[jev]\ncheck_threshold = nan\n", "", "jev.check_threshold"},
		{"jev decision threshold zero", topConfig + "[jev]\ndecision_threshold = 0\n", "", "jev.decision_threshold"},
		{"jev decision threshold negative", topConfig + "[jev]\ndecision_threshold = -0.1\n", "", "jev.decision_threshold"},
		{"jev decision threshold above one", topConfig + "[jev]\ndecision_threshold = 1.01\n", "", "jev.decision_threshold"},
		{"jev decision threshold nan", topConfig + "[jev]\ndecision_threshold = nan\n", "", "jev.decision_threshold"},
		{"jev owner threshold zero", topConfig + "[jev]\nowner_threshold = 0\n", "", "jev.owner_threshold"},
		{"jev owner threshold negative", topConfig + "[jev]\nowner_threshold = -0.5\n", "", "jev.owner_threshold"},
		{"jev owner threshold above one", topConfig + "[jev]\nowner_threshold = 1.2\n", "", "jev.owner_threshold"},
		{"jev owner threshold nan", topConfig + "[jev]\nowner_threshold = nan\n", "", "jev.owner_threshold"},
		{"jev unknown key", topConfig + "[jev]\nclassification = true\n", "", "jev.classification"},
		{"transcribe url scheme", topConfig + "[transcribe]\nurl = 'ftp://whisper.example/v1/audio/transcriptions'\n", "", "transcribe.url"},
		{"transcribe url relative", topConfig + "[transcribe]\nurl = '/v1/audio/transcriptions'\n", "", "transcribe.url"},
		{"transcribe url no host", topConfig + "[transcribe]\nurl = 'http:///v1/audio/transcriptions'\n", "", "transcribe.url"},
		{"transcribe url credentials", topConfig + "[transcribe]\nurl = 'http://user:pass@whisper.example/v1/audio/transcriptions'\n", "", "transcribe.url"},
		{"transcribe url token credentials", topConfig + "[transcribe]\nurl = 'http://token@whisper.example/v1/audio/transcriptions'\n", "", "transcribe.url"},
		{"transcribe url query", topConfig + "[transcribe]\nurl = 'http://whisper.example/v1/audio/transcriptions?key=1'\n", "", "transcribe.url"},
		{"transcribe url fragment", topConfig + "[transcribe]\nurl = 'http://whisper.example/v1/audio/transcriptions#x'\n", "", "transcribe.url"},
		{"transcribe url malformed", topConfig + "[transcribe]\nurl = 'http://exa mple.com/%zz'\n", "", "transcribe.url"},
		{"transcribe model blank", topConfig + "[transcribe]\nmodel = ' '\n", "", "transcribe.model"},
		{"transcribe model spaced", topConfig + "[transcribe]\nmodel = 'large v3'\n", "", "transcribe.model"},
		{"transcribe key value", topConfig + "[transcribe]\napi_key_env = 'sk-123 456'\n", "", "transcribe.api_key_env"},
		{"transcribe key empty", topConfig + "[transcribe]\napi_key_env = ''\n", "", "transcribe.api_key_env"},
		{"transcribe unknown key", topConfig + "[transcribe]\ntimeout = '5s'\n", "", "transcribe.timeout"},
		{"transcribe wrong type", topConfig + "[transcribe]\nurl = 1\n", "", "config.toml:"},
		{"pause override", topConfig + "[pause]\n", "", "runtime overrides belong in runtime.json"},
		{"unregistered kind table", topConfig + "[kinds.mystery]\n", "", "kinds.mystery: unknown configuration key"},
		{"kind without settings takes no key", topConfig + "[kinds.feature]\nskills = ['x']\n", "", "kinds.feature.skills: unknown configuration key"},
		{"priority override", topConfig + "[priority]\n", "", "runtime overrides belong in runtime.json"},
		{"notify unknown key", topConfig + "[notify]\nemail = 'owner@example.com'\n", "", "notify.email: unknown configuration key"},
		{"webhook relative", topConfig + "[notify]\nwebhook = '/hooks/osmia'\n", "", "notify.webhook: must be an absolute http or https URL"},
		{"webhook no host", topConfig + "[notify]\nwebhook = 'https:///hooks'\n", "", "notify.webhook: must be an absolute http or https URL"},
		{"webhook opaque", topConfig + "[notify]\nwebhook = 'https:ntfy.sh'\n", "", "notify.webhook: must be an absolute http or https URL"},
		{"webhook scheme", topConfig + "[notify]\nwebhook = 'ftp://example.com/hook'\n", "", "notify.webhook: scheme must be http or https"},
		{"webhook malformed", topConfig + "[notify]\nwebhook = 'https://exa mple.com/%zz'\n", "", "notify.webhook: must be an absolute http or https URL"},
		{"webhook wrong type", topConfig + "[notify]\nwebhook = 1\n", "", "config.toml:"},
		{"error reports unknown key", topConfig + "[error_reports]\ntoken = 'x'\n", "", "error_reports.token: unknown configuration key"},
		{"error reports repository url", topConfig + "[error_reports]\nrepository = 'https://github.com/kpenfound/osmia'\n", "", "error_reports.repository: must be an owner/name GitHub repository"},
		{"error reports repository no owner", topConfig + "[error_reports]\nrepository = 'osmia'\n", "", "error_reports.repository:"},
		{"error reports repository empty", topConfig + "[error_reports]\nrepository = ''\n", "", "error_reports.repository:"},
		{"error reports repository git suffix", topConfig + "[error_reports]\nrepository = 'kpenfound/osmia.git'\n", "", "error_reports.repository:"},
		{"error reports repository space", topConfig + "[error_reports]\nrepository = 'kpenfound/os mia'\n", "", "error_reports.repository:"},
		{"error reports repository extra segment", topConfig + "[error_reports]\nrepository = 'a/b/c'\n", "", "error_reports.repository:"},
		{"error reports unknown source", topConfig + "[error_reports]\nsources = ['api', 'model']\n", "", "error_reports.sources[1]: expected api, operation, github"},
		{"error reports empty label", topConfig + "[error_reports]\nlabels = ['bug', ' ']\n", "", "error_reports.labels[1]: must not be empty"},
		{"error reports zero hourly cap", topConfig + "[error_reports]\nmax_issues_per_hour = 0\n", "", "error_reports.max_issues_per_hour: must be positive"},
		{"error reports negative daily cap", topConfig + "[error_reports]\nmax_issues_per_day = -1\n", "", "error_reports.max_issues_per_day: must be positive"},
		{"error reports zero comment cap", topConfig + "[error_reports]\nmax_comments_per_day = 0\n", "", "error_reports.max_comments_per_day: must be positive"},
		{"error reports wrong type", topConfig + "[error_reports]\nenabled = 'yes'\n", "", "config.toml:"},
		{"empty socket", topConfig + "[listen]\nsocket = ''\n", "", "listen.socket"},
		{"socket escape", topConfig + "[listen]\nsocket = '../outside.sock'\n", "", "listen.socket"},
		{"socket directory", topConfig + "[listen]\nsocket = '.'\n", "", "direct child"},
		{"long socket", topConfig + "[listen]\nsocket = '" + strings.Repeat("a", 104) + ".sock'\n", "", "103-byte Unix socket limit"},
		{"project version", "", strings.Replace(projectConfig, "version = 1", "version = 2", 1), "version:"},
		{"project unknown", "", projectConfig + "oops = 1\n", "oops"},
		{"project unsupported", "", projectConfig + "hearsay_unknown = 'dagger'\n", "unknown configuration key"},
		{"malformed upstream rebase", "", projectConfig + "upstream_rebase = 'daily'\n", "upstream_rebase:"},
		{"empty upstream rebase", "", projectConfig + "upstream_rebase = ''\n", "upstream_rebase:"},
		{"negative upstream rebase", "", projectConfig + "upstream_rebase = '-6h'\n", "upstream_rebase:"},
		{"short upstream rebase", "", projectConfig + "upstream_rebase = '59s'\n", "upstream_rebase:"},
		{"integer upstream rebase", "", projectConfig + "upstream_rebase = 0\n", "upstream_rebase"},
		{"malformed pull request poll", "", projectConfig + "pull_request_poll = 'often'\n", "pull_request_poll"},
		{"empty pull request poll", "", projectConfig + "pull_request_poll = ''\n", "pull_request_poll"},
		{"zero pull request poll", "", projectConfig + "pull_request_poll = '0'\n", "pull_request_poll"},
		{"negative pull request poll", "", projectConfig + "pull_request_poll = '-10m'\n", "pull_request_poll"},
		{"short pull request poll", "", projectConfig + "pull_request_poll = '4m59.999999999s'\n", "pull_request_poll"},
		{"integer pull request poll", "", projectConfig + "pull_request_poll = 600\n", "pull_request_poll"},
		{"float pull request poll", "", projectConfig + "pull_request_poll = 600.0\n", "pull_request_poll"},
		{"malformed checks timeout", "", projectConfig + "checks_timeout = 'long'\n", "checks_timeout:"},
		{"zero checks timeout", "", projectConfig + "checks_timeout = '0'\n", "checks_timeout:"},
		{"short checks timeout", "", projectConfig + "checks_timeout = '59s'\n", "checks_timeout:"},
		{"long checks timeout", "", projectConfig + "checks_timeout = '25h'\n", "checks_timeout:"},
		{"integer checks timeout", "", projectConfig + "checks_timeout = 30\n", "checks_timeout"},
		{"repository url", "", strings.Replace(projectConfig, "upstream/repo", "https://host/upstream/repo", 1), "upstream:"},
		{"missing clone", "", strings.Replace(projectConfig, `clone = "~/clone"`, "", 1), "clone:"},
		{"clone in root", "", strings.Replace(projectConfig, "~/clone", "~/.local/share/osmia/clone", 1), "non-nested"},
		{"root in clone", "", strings.Replace(projectConfig, "~/clone", "~", 1), "non-nested"},
		{"tilde user", "", strings.Replace(projectConfig, "~/clone", "~other/clone", 1), "home expansion"},
		{"branch", "", projectConfig + "base_branch = 'bad..branch'\n", "base_branch:"},
		{"landing", "", projectConfig + "landing = 'merge'\n", "landing:"},
		{"project wip", "", projectConfig + "[capacity]\nper_workstream = -1\n", "capacity.per_workstream"},
		{"zero git operations", "", projectConfig + "[capacity]\ngit_operations = 0\n", "capacity.git_operations"},
		{"negative git operations", "", projectConfig + "[capacity]\ngit_operations = -1\n", "capacity.git_operations"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			top, project := tc.top, tc.project
			if top == "" {
				top = topConfig
			}
			if project == "" {
				project = projectConfig
			}
			c, err := load(fixture(t, top, project))
			if c != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got config=%+v err=%v; want nil and %q", c, err, tc.want)
			}
		})
	}
}

// listen.tailnet accepts one DNS label; empty disables it.
func TestListenTailnet(t *testing.T) {
	for _, value := range []string{"", "osmia", "o", "osmia-2", "7", strings.Repeat("a", 63)} {
		t.Run(value, func(t *testing.T) {
			c, err := load(fixture(t, topConfig+"[listen]\ntailnet = '"+value+"'\n", projectConfig))
			if err != nil {
				t.Fatal(err)
			}
			if c.Listen.Tailnet != value {
				t.Fatalf("listen.tailnet %q loaded as %q", value, c.Listen.Tailnet)
			}
		})
	}
}

// listen.web accepts localhost and loopback addresses with a numeric port;
// empty disables it.
func TestListenWeb(t *testing.T) {
	for _, value := range []string{"", "localhost:8080", "LocalHost:0", "127.0.0.1:8080", "127.1.2.3:65535", "[::1]:8080", "[::ffff:127.0.0.1]:80"} {
		t.Run(value, func(t *testing.T) {
			c, err := load(fixture(t, topConfig+"[listen]\nweb = '"+value+"'\n", projectConfig))
			if err != nil {
				t.Fatal(err)
			}
			if c.Listen.Web != value {
				t.Fatalf("listen.web %q loaded as %q", value, c.Listen.Web)
			}
		})
	}
}

// upstream_rebase accepts a Go duration of at least a minute, and zero, which
// disables scheduled drift rebases.
func TestUpstreamRebase(t *testing.T) {
	for value, want := range map[string]time.Duration{"1m": time.Minute, "90m": 90 * time.Minute, "24h": 24 * time.Hour, "0": 0, "0s": 0} {
		t.Run(value, func(t *testing.T) {
			c, err := load(fixture(t, topConfig, projectConfig+"upstream_rebase = '"+value+"'\n"))
			if err != nil {
				t.Fatal(err)
			}
			if c.Project.UpstreamRebase != value || c.Project.RebaseInterval() != want {
				t.Fatalf("upstream_rebase %q loaded as %q, interval %v; want %v", value, c.Project.UpstreamRebase, c.Project.RebaseInterval(), want)
			}
		})
	}
}

// checks_timeout accepts a Go duration from a minute to a day.
func TestChecksTimeout(t *testing.T) {
	for value, want := range map[string]time.Duration{"1m": time.Minute, "45m": 45 * time.Minute, "24h": 24 * time.Hour} {
		t.Run(value, func(t *testing.T) {
			c, err := load(fixture(t, topConfig, projectConfig+"checks_timeout = '"+value+"'\n"))
			if err != nil {
				t.Fatal(err)
			}
			if c.Project.ChecksTimeout != value || c.Project.CheckTimeout() != want {
				t.Fatalf("checks_timeout %q loaded as %q, timeout %v; want %v", value, c.Project.ChecksTimeout, c.Project.CheckTimeout(), want)
			}
		})
	}
}

func TestMissingTopLevelFile(t *testing.T) {
	opts := fixture(t, topConfig, projectConfig)
	if err := os.Remove(configFile(opts)); err != nil {
		t.Fatal(err)
	}
	if c, err := load(opts); c != nil || err == nil || !strings.Contains(err.Error(), configFile(opts)) {
		t.Fatalf("%+v %v", c, err)
	}
}
func TestPathsAndIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	r, err := ResolveRoot("", home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.String()); !os.IsNotExist(err) {
		t.Fatal("root was created")
	}
	explicit, err := ResolveRoot(filepath.Join(home, "x", "..", "explicit"), home)
	if err != nil || !strings.HasSuffix(explicit.String(), "/explicit") {
		t.Fatalf("%+v %v", explicit, err)
	}
	p, err := NewProjectID()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseProjectID(string(p))
	if parsed != p || err != nil {
		t.Fatal(parsed, err)
	}
	w, err := NewWorkstreamID()
	if err != nil {
		t.Fatal(err)
	}
	parsedW, err := ParseWorkstreamID(string(w))
	if parsedW != w || err != nil {
		t.Fatal(parsedW, err)
	}
	if !errors.Is(CheckProjectIDs(p, p), ErrCollision) || !errors.Is(CheckWorkstreamIDs(w, w), ErrCollision) {
		t.Fatal("collision not detected")
	}
	for _, bad := range []string{"", "..", "/absolute", "a/b", "p_ABCDEF0123456789abcdef0123456789", wid} {
		if _, err := r.ProjectTrace(ProjectID(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := r.Workstream(p, WorkstreamID("../escape")); err == nil {
		t.Fatal("workstream traversal")
	}
	if path, err := r.Config(); err != nil || path != configFile(Options{Home: home}) {
		t.Fatalf("default config %s %v", path, err)
	}
	if path, err := explicit.Config(); err != nil || path != filepath.Join(explicit.String(), "config.toml") {
		t.Fatalf("explicit config %s %v", path, err)
	}
	for _, get := range []func() (string, error){r.Runtime, r.Socket, func() (string, error) { return r.ProjectTrace(p) }, func() (string, error) { return r.ProjectConfig(p) }, func() (string, error) { return r.Workstream(p, w) }} {
		path, err := get()
		if err != nil || !beneath(r.String(), path) {
			t.Fatalf("%s %v", path, err)
		}
	}
}
func TestSymlinkBoundaries(t *testing.T) {
	for _, target := range []string{"outside", "inside"} {
		t.Run(target, func(t *testing.T) {
			opts := fixture(t, topConfig, projectConfig)
			root := rootDirectory(opts)
			projects := filepath.Join(root, "projects")
			if err := os.MkdirAll(projects, 0700); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(opts.Home, "elsewhere")
			if target == "inside" {
				destination = filepath.Join(root, "alias")
			}
			if err := os.Rename(projects, destination); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(destination, projects); err != nil {
				t.Fatal(err)
			}
			if c, err := load(opts); c != nil || err == nil {
				t.Fatalf("alias accepted: %+v %v", c, err)
			}
		})
	}
	opts := fixture(t, topConfig, projectConfig)
	if err := os.Symlink(rootDirectory(opts), filepath.Join(opts.Home, "clone")); err != nil {
		t.Fatal(err)
	}
	if c, err := load(opts); c != nil || err == nil || !strings.Contains(err.Error(), "non-nested") {
		t.Fatalf("clone alias: %+v %v", c, err)
	}
}
func TestMetadataDoesNotChangeIdentity(t *testing.T) {
	opts := fixture(t, topConfig, projectConfig)
	before, err := load(opts)
	if err != nil {
		t.Fatal(err)
	}
	write(t, projectFile(opts), strings.Replace(projectConfig, "~/clone", "~/renamed-clone", 1)+"name = 'New display name'\n")
	after, err := load(opts)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := before.Root.Workstream(before.Project.ID, WorkstreamID(wid))
	b, _ := after.Root.Workstream(after.Project.ID, WorkstreamID(wid))
	if before.Project.ID != after.Project.ID || a != b {
		t.Fatal("mutable metadata changed identity")
	}
}
func TestBranchNames(t *testing.T) {
	for _, s := range []string{"main", "feature/name", "release-1.2"} {
		if !ValidBranch(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"", "@", "-x", "a b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", "a\\b", "a..b", "a@{b", "a.", "a\nb", "a//b", ".a", "a.lock", "/a", "a/"} {
		if ValidBranch(s) {
			t.Fatal(s)
		}
	}
}

func TestDesignerRoleIsAnOrdinaryRoleThatNeverFallsBackToTheManager(t *testing.T) {
	chief := topConfig + "[profiles.chief]\nagent = 'codex'\nmodel = 'chief-model'\n[roles.manager]\nprofile = 'chief'\nsandbox = 'container'\nimage = 'chief-image'\nskills = ['https://github.com/example/skills#chief-skill']\n"
	c, err := load(fixture(t, chief, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Roles["designer"]; got.Profile != "default" || got.Sandbox != "none" || got.Image != "" || got.EventsProfile != "" || len(got.Skills) != 0 {
		t.Fatalf("an unconfigured designer: %+v", got)
	}
	if got := c.RoleExecution("designer"); got.Mode != "none" || got.Image != "" || len(got.Skills) != 0 {
		t.Fatalf("an unconfigured designer's execution: %+v", got)
	}
	configured := chief + "[roles.designer]\nprofile = 'chief'\nsandbox = 'sbx'\nimage = 'osmia:latest'\nskills = ['https://github.com/example/skills#design-skill']\n"
	c, err = load(fixture(t, configured, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	p, settings, err := c.Execution("designer", "chief")
	if err != nil || p.Backend != "codex" || settings.Mode != "sbx" || settings.Image != "osmia:latest" || !slices.Equal(settings.Skills, []string{"https://github.com/example/skills#design-skill"}) {
		t.Fatalf("a configured designer: %+v %+v %v", p, settings, err)
	}
}

func TestDesignerSettingsAreTheSameWhicheverManagerKeyIsSet(t *testing.T) {
	settings := "profile = 'chief'\nsandbox = 'container'\nimage = 'chief-image'\nskills = ['https://github.com/example/skills#chief-skill']\n"
	base := topConfig + "[profiles.chief]\nagent = 'codex'\nmodel = 'chief-model'\n"
	for name, roles := range map[string]string{
		"canonical key":    "[roles.manager]\n" + settings,
		"deprecated alias": "[roles.chief_of_staff]\n" + settings,
		"no manager role":  "",
	} {
		for _, designer := range []string{"", "[roles.designer]\nprofile = 'chief'\nsandbox = 'sbx'\nimage = 'osmia:latest'\n"} {
			c, err := load(fixture(t, base+roles+designer, projectConfig))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			got := c.Roles["designer"]
			if designer == "" {
				if got.Profile != "default" || got.Sandbox != "none" || got.Image != "" || len(got.Skills) != 0 {
					t.Fatalf("%s: an unconfigured designer: %+v", name, got)
				}
				continue
			}
			if got.Profile != "chief" || got.Sandbox != "sbx" || got.Image != "osmia:latest" || len(got.Skills) != 0 {
				t.Fatalf("%s: a configured designer: %+v", name, got)
			}
		}
	}
}

func TestNamedBindings(t *testing.T) {
	top := strings.Replace(topConfig, "profiles.default", "profiles.custom", 1)
	top = strings.Replace(top, "claude", "opencode", 1)
	for _, role := range roleNames {
		top += "\n[roles." + role + "]\nprofile = 'custom'\n"
	}
	c, err := load(fixture(t, top, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Roles) != 7 || c.Roles["reviewer"].Profile != "custom" || c.Roles["designer"].Profile != "custom" {
		t.Fatalf("bindings: %+v", c.Roles)
	}
}

func TestFilesystemErrors(t *testing.T) {
	for _, name := range []string{"clone file", "root file", "config alias", "dangling clone", "socket file"} {
		t.Run(name, func(t *testing.T) {
			opts := fixture(t, topConfig, projectConfig)
			root := rootDirectory(opts)
			switch name {
			case "clone file":
				write(t, filepath.Join(opts.Home, "clone"), "not a directory")
			case "root file":
				opts.Root = filepath.Join(opts.Home, "file")
				write(t, opts.Root, "not a directory")
			case "config alias":
				opts.Root = root
				if err := os.Symlink(configFile(opts), filepath.Join(root, "config.toml")); err != nil {
					t.Fatal(err)
				}
			case "dangling clone":
				if err := os.Symlink(filepath.Join(opts.Home, "missing"), filepath.Join(opts.Home, "clone")); err != nil {
					t.Fatal(err)
				}
			case "socket file":
				write(t, filepath.Join(root, "osmia.sock"), "not a socket")
			}
			if c, err := load(opts); c != nil || err == nil {
				t.Fatalf("accepted: %+v %v", c, err)
			}
		})
	}
}

// notify.webhook accepts absolute http and https URLs; empty sends nothing.
func TestNotifyWebhook(t *testing.T) {
	for _, value := range []string{"", "https://ntfy.sh/osmia-topic", "http://127.0.0.1:8080/hook?token=x", "HTTPS://hooks.example.com"} {
		t.Run(value, func(t *testing.T) {
			c, err := load(fixture(t, topConfig+"[notify]\nwebhook = '"+value+"'\n", projectConfig))
			if err != nil {
				t.Fatal(err)
			}
			if c.Notify.Webhook != value {
				t.Fatalf("notify.webhook %q loaded as %q", value, c.Notify.Webhook)
			}
		})
	}
	secret := "https://hooks.example.com/services/SECRET"
	_, err := load(fixture(t, topConfig+"[notify]\nwebhook = 'gopher://hooks.example.com/services/SECRET'\n", projectConfig))
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("rejected webhook error %v must not quote %s", err, secret)
	}
}

// Without an [error_reports] section the reporter is off and files to the
// Osmia repository with the documented labels, caps and sources.
func TestErrorReportsDefaults(t *testing.T) {
	c, err := load(fixture(t, topConfig, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	e := c.ErrorReports
	if e.Enabled || e.Repository != "kpenfound/osmia" || !slices.Equal(e.Labels, []string{"factory-error", "automated"}) {
		t.Fatalf("defaults %+v", e)
	}
	if e.MaxIssuesPerHour <= 0 || e.MaxIssuesPerDay <= 0 || e.MaxCommentsPerDay <= 0 {
		t.Fatalf("caps %+v", e)
	}
	if !slices.Equal(e.Sources, []string{"api", "operation", "github"}) {
		t.Fatalf("sources %v", e.Sources)
	}
	for _, source := range ErrorSources {
		if !e.SourceEnabled(source) {
			t.Fatalf("source %s is off by default", source)
		}
	}
}

// Every [error_reports] key is read, and a key left out keeps its default.
func TestErrorReportsSettings(t *testing.T) {
	c, err := load(fixture(t, topConfig+`[error_reports]
enabled = true
repository = "owner/name.repo"
labels = ["bug"]
max_issues_per_hour = 2
max_issues_per_day = 7
max_comments_per_day = 9
sources = ["api", "github"]
`, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	want := ErrorReports{Enabled: true, Repository: "owner/name.repo", Labels: []string{"bug"}, MaxIssuesPerHour: 2, MaxIssuesPerDay: 7, MaxCommentsPerDay: 9, Sources: []string{"api", "github"}}
	if !reflect.DeepEqual(c.ErrorReports, want) {
		t.Fatalf("got %+v want %+v", c.ErrorReports, want)
	}
	if c.ErrorReports.SourceEnabled("operation") || !c.ErrorReports.SourceEnabled("github") {
		t.Fatalf("sources %v", c.ErrorReports.Sources)
	}
	partial, err := load(fixture(t, topConfig+"[error_reports]\nenabled = true\n", projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	if e := partial.ErrorReports; !e.Enabled || e.Repository != "kpenfound/osmia" || len(e.Labels) != 2 || len(e.Sources) != 3 || e.MaxIssuesPerHour != DefaultMaxIssuesPerHour {
		t.Fatalf("partial %+v", e)
	}
}

// The reporter's saved state is a managed file at the top of the root, beside
// the notification ledger.
func TestErrorReportsStatePath(t *testing.T) {
	c, err := load(fixture(t, topConfig, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	reports, err := c.Root.ErrorReports()
	if err != nil {
		t.Fatal(err)
	}
	notifications, err := c.Root.Notifications()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(reports) != "error-reports.json" || filepath.Dir(reports) != filepath.Dir(notifications) || filepath.Dir(reports) != c.Root.String() {
		t.Fatalf("state %s, ledger %s, root %s", reports, notifications, c.Root)
	}
}

// workspaces defaults to auto, accepts auto, git and jujutsu, and rejects
// anything else with an error that names the values it accepts.
func TestWorkspaces(t *testing.T) {
	c, err := load(fixture(t, topConfig, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspaces != WorkspacesAuto {
		t.Fatalf("default workspaces %q", c.Workspaces)
	}
	with := func(value string) string {
		return strings.Replace(topConfig, "[profiles.default]", "workspaces = "+value+"\n[profiles.default]", 1)
	}
	for _, value := range []string{WorkspacesAuto, WorkspacesGit, WorkspacesJujutsu} {
		c, err := load(fixture(t, with("'"+value+"'"), projectConfig))
		if err != nil {
			t.Fatal(err)
		}
		if c.Workspaces != value {
			t.Fatalf("workspaces %q loaded as %q", value, c.Workspaces)
		}
	}
	for _, value := range []string{"'jj'", "'Git'", "''"} {
		_, err := load(fixture(t, with(value), projectConfig))
		var field *FieldError
		if !errors.As(err, &field) || field.Field != "workspaces" || field.Reason != `must be "auto", "git" or "jujutsu"` {
			t.Fatalf("workspaces = %s: %v", value, err)
		}
	}
	if _, err := load(fixture(t, with("1"), projectConfig)); err == nil {
		t.Fatal("workspaces = 1 loaded")
	}
}

func TestXDGDirectories(t *testing.T) {
	home, data, conf := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("XDG_CONFIG_HOME", conf)
	r, err := ResolveRoot("", home)
	if err != nil {
		t.Fatal(err)
	}
	if path, err := r.Config(); err != nil || r.String() != filepath.Join(data, "osmia") || path != filepath.Join(conf, "osmia", "config.toml") {
		t.Fatalf("root %s config %s %v", r, path, err)
	}
	pinned, err := r.Options(home).Resolve()
	if err != nil || pinned != r {
		t.Fatalf("pinned %+v %v, want %+v", pinned, err, r)
	}
	explicit, err := ResolveRoot(filepath.Join(home, "explicit"), home)
	if err != nil {
		t.Fatal(err)
	}
	if path, err := explicit.Config(); err != nil || path != filepath.Join(explicit.String(), "config.toml") {
		t.Fatalf("explicit root read %s %v", path, err)
	}
	t.Setenv("XDG_DATA_HOME", "data")
	t.Setenv("XDG_CONFIG_HOME", "config")
	r, err = ResolveRoot("", home)
	if err != nil {
		t.Fatal(err)
	}
	if path, err := r.Config(); err != nil || r.String() != rootDirectory(Options{Home: home}) || path != configFile(Options{Home: home}) {
		t.Fatalf("root %s config %s %v", r, path, err)
	}
}

// A default config.toml may be a symlink, which loading follows.
func TestSymlinkedConfig(t *testing.T) {
	opts := fixture(t, strings.Replace(topConfig, `"`+pid+`"`, "", 1), projectConfig)
	target := filepath.Join(opts.Home, "dotfiles", "osmia.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(configFile(opts), target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, configFile(opts)); err != nil {
		t.Fatal(err)
	}
	c, err := load(opts)
	if err != nil {
		t.Fatal(err)
	}
	path, err := c.Root.Config()
	if err != nil || path != target {
		t.Fatalf("config %s %v", path, err)
	}
	if info, err := os.Lstat(configFile(opts)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v", err)
	}
	if _, err := load(opts); err != nil {
		t.Fatalf("symlinked config: %v", err)
	}
}

func TestCommitteeAssignments(t *testing.T) {
	c, err := load(fixture(t, topConfig, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Committee.Perspectives, ",") != "correctness,integration,scope" || len(c.Committee.Profiles) != 0 {
		t.Fatalf("default committee: %+v", c.Committee)
	}
	top := topConfig + "[profiles.second]\nagent = 'codex'\nmodel = 'other'\n[committee]\nperspectives = ['scope', 'correctness']\nprofiles = ['default', 'second']\n"
	c, err = load(fixture(t, top, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Committee.Perspectives, ",") != "scope,correctness" || strings.Join(c.Committee.Profiles, ",") != "default,second" {
		t.Fatalf("committee: %+v", c.Committee)
	}
}

func TestPullRequestPollInterval(t *testing.T) {
	for _, value := range []string{"", "5m", "10m", "20m", "1h30m"} {
		t.Run(value, func(t *testing.T) {
			text := projectConfig
			want := 10 * time.Minute
			if value != "" {
				text += "pull_request_poll = '" + value + "'\n"
				want, _ = time.ParseDuration(value)
			}
			c, err := load(fixture(t, topConfig, text))
			if err != nil {
				t.Fatal(err)
			}
			if c.Project.PullRequestPollInterval() != want {
				t.Fatalf("poll interval = %v; want %v", c.Project.PullRequestPollInterval(), want)
			}
			if value == "" && c.Project.PullRequestPoll != "10m" {
				t.Fatalf("default = %q", c.Project.PullRequestPoll)
			}
		})
	}
}

func TestProjectGitOperationCapacity(t *testing.T) {
	c, err := load(fixture(t, topConfig, projectConfig+"[capacity]\ngit_operations = 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Project.Capacity.GitOperations != 2 {
		t.Fatalf("git operation capacity: %+v", c.Project.Capacity)
	}
}

// Serial: the configuration fixture sets environment variables.
func TestListenTailnetHTTPS(t *testing.T) {
	for _, tc := range []struct {
		setting string
		want    bool
	}{{"", true}, {"tailnet_https = true", true}, {"tailnet_https = false", false}} {
		c, err := load(fixture(t, topConfig+"[listen]\n"+tc.setting+"\n", projectConfig))
		if err != nil {
			t.Fatal(err)
		}
		if c.Listen.TailnetHTTPS != tc.want {
			t.Fatalf("%s: HTTPS = %v", tc.setting, c.Listen.TailnetHTTPS)
		}
	}
	if _, err := load(fixture(t, topConfig+"[listen]\ntailnet_https = 'false'\n", projectConfig)); err == nil {
		t.Fatal("accepted non-boolean HTTPS setting")
	}
}

func TestJevCheckFallbackPicks(t *testing.T) {
	for _, value := range []string{"", "check_fallback_picks = 0\n", "check_fallback_picks = 2\n"} {
		cfg, err := load(fixture(t, topConfig+"[jev]\n"+value, projectConfig))
		want := 0
		if strings.Contains(value, "= 2") {
			want = 2
		}
		if err != nil || cfg.Jev.CheckFallbackPicks != want {
			t.Fatalf("%s: %+v %v", value, cfg, err)
		}
	}
	_, err := load(fixture(t, topConfig+"[jev]\ncheck_fallback_picks = -1\n", projectConfig))
	if err == nil || !strings.Contains(err.Error(), "jev.check_fallback_picks") {
		t.Fatalf("negative: %v", err)
	}
}

func TestRetiredKeysFailAsUnknownConfigurationKeys(t *testing.T) {
	for _, tc := range []struct {
		name, top, project, field string
	}{
		{"beekeeper table", topConfig + "[beekeeper]\nname = 'Hive'\n", projectConfig, "beekeeper"},
		{"preview table", topConfig + "[preview]\nport = 1\n", projectConfig, "preview"},
		{"project preview table", topConfig, projectConfig + "[preview]\nport = 1\n", "preview"},
		{"jev.enabled", topConfig + "[jev]\nenabled = true\n", projectConfig, "jev.enabled"},
		{"roles.mason.dagger", topConfig + "[roles.mason]\nsandbox = 'sbx'\n[roles.mason.dagger]\nversion = 'v0.19.0'\n", projectConfig, "roles.mason.dagger"},
		{"roles.librarian", topConfig + "[roles.librarian]\nprofile = 'default'\n", projectConfig, "roles.librarian"},
		{"project group_id", topConfig, projectConfig + "group_id = 'g_0123456789abcdef0123456789abcdef'\n", "group_id"},
		{"project classifier", topConfig, projectConfig + "classifier = 'default'\n", "classifier"},
		{"active_projects", "active_projects = []\n" + topConfig, projectConfig, "active_projects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(fixture(t, tc.top, tc.project))
			var field *FieldError
			if !errors.As(err, &field) || field.Field != tc.field || field.Reason != "unknown configuration key" {
				t.Fatalf("got %#v (%v), want an unknown configuration key error naming %s", field, err, tc.field)
			}
			if text := strings.ToLower(err.Error()); strings.Contains(text, "retired") || strings.Contains(text, "removed") || strings.Contains(text, "ignored") {
				t.Fatalf("the error describes the key as retired: %q", err)
			}
		})
	}
}

// A [kinds.<kind>] table belongs to a registered kind, and a kind that has no
// settings accepts the empty table.
func TestKindsTable(t *testing.T) {
	c, err := load(fixture(t, topConfig+"[kinds.feature]\n", projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Kinds["feature"]; !ok || len(c.KindSkills("feature")) != 0 || len(c.KindSkills("mystery")) != 0 {
		t.Fatalf("kinds: %+v", c.Kinds)
	}
	c, err = load(fixture(t, topConfig, projectConfig))
	if err != nil || len(c.Kinds) != 0 {
		t.Fatalf("default kinds: %+v %v", c, err)
	}
}
