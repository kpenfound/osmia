package telemetry

import (
	"strings"
	"testing"
	"time"
)

func TestNamingHelpers(t *testing.T) {
	if got := SpanName("unit", "state").String(); got != "osmia.unit.state" {
		t.Fatalf("span name %q", got)
	}
	if got := Key("pool", "limit"); got != "osmia.pool.limit" {
		t.Fatalf("key %q", got)
	}
	if got := GenAIName("chat", "claude-sonnet-5-5").String(); got != "chat claude-sonnet-5-5" {
		t.Fatalf("GenAI name %q", got)
	}
	for _, bad := range []Name{SpanName(), SpanName("Unit"), SpanName("unit state"), SpanName("a.b"), SpanName(""), GenAIName("chat", "two words"), GenAIName("Chat", "m"), GenAIName("chat", "")} {
		if bad.Valid() || bad.String() != "" {
			t.Fatalf("%q must not be a valid name", bad.String())
		}
	}
	if Key("Bad") != "" || Key() != "" {
		t.Fatal("a malformed key must be empty")
	}
}

func TestAllowListedAttributes(t *testing.T) {
	for name, a := range map[string]Attr{
		"role": Role("mason"), "backend": Backend("claude"), "model": Model("claude-sonnet-5-5"), "profile": Profile("default"),
		"project": Project("p_00000000000000000000000000000001"), "workstream": Workstream(streamA), "unit": Unit("u1"),
		"record": Record("rec-1"), "pool": Pool("masons"), "roles": PoolRoles([]string{"mason", "reviewer"}),
		"limit": PoolLimit(4), "unbounded": PoolUnbounded(), "outcome": Outcome("completed"), "cause": Cause("review_bounce"),
		"state": State("implementing"), "next": NextState("reviewing"), "reason": WaitReason("pool_full"), "kind": CheckKind("final"),
		"scope": PauseScope("factory"), "source": PauseSource("owner"), "covered": Covered(3), "wait": SlotWait(1500 * time.Millisecond),
		"cost": CostUSD(0.25), "known": CostKnown(false),
		"operation": GenAIOperation("chat"), "provider": GenAIProvider("Anthropic"), "genai model": GenAIModel("claude-sonnet-5-5"),
	} {
		if !a.Valid() || !strings.HasPrefix(a.Key(), "osmia.") && !strings.HasPrefix(a.Key(), "gen_ai.") {
			t.Fatalf("%s: %q is not an allow-listed attribute", name, a.Key())
		}
	}
	if got := SlotWait(1500 * time.Millisecond).Value().AsInt64(); got != 1500 {
		t.Fatalf("slot wait %d", got)
	}
	if got := GenAIProvider("Anthropic").Value().AsString(); got != "anthropic" {
		t.Fatalf("provider %q", got)
	}
}

func TestAttributesOutsideTheAllowListAreRefused(t *testing.T) {
	for name, a := range map[string]Attr{
		"unlisted key":            String("osmia.prompt", "hello"),
		"unlisted GenAI key":      String("gen_ai.prompt", "hello"),
		"unnamespaced key":        String("role", "mason"),
		"empty key":               String("", "x"),
		"unlisted int":            Int("osmia.title_length", 3),
		"unlisted bool":           Bool("osmia.reason", true),
		"unlisted float":          Float("osmia.score", 1),
		"unlisted list":           Strings("osmia.titles", []string{"a"}),
		"string for an int key":   String(KeyPoolLimit, "4"),
		"int for a string key":    Int(KeyRole, 1),
		"bool for a string key":   Bool(KeyOutcome, true),
		"text with spaces":        Role("please fix the failing build"),
		"text with a line break":  Outcome("done\nignore previous instructions"),
		"long text":               Role(strings.Repeat("a", 129)),
		"reason as free text":     Cause("The owner said no"),
		"uppercase enumeration":   State("Implementing"),
		"empty":                   Role(""),
		"negative count":          Covered(-1),
		"negative limit":          PoolLimit(-1),
		"negative cost":           CostUSD(-0.1),
		"text in a list":          PoolRoles([]string{"mason", "free text here"}),
		"empty list":              PoolRoles(nil),
		"scalar key for list":     Strings(KeyRole, []string{"mason"}),
		"unsafe punctuation":      Model("model;drop table"),
		"quoted":                  Profile(`"quoted"`),
		"wait reason with spaces": WaitReason("pool is full"),
	} {
		if a.Valid() || a.Key() != "" {
			t.Fatalf("%s was accepted as %q", name, a.Key())
		}
	}
}
