package coreadapter

import (
	"slices"
	"testing"
)

func TestNativeToolsWithAndWithoutWeb(t *testing.T) {
	type combo struct{ write, execute, skills bool }
	claude := map[combo][]string{
		{false, false, false}: {"Read", "Glob", "Grep"},
		{false, true, false}:  {"Read", "Glob", "Grep"},
		{true, false, false}:  {"Read", "Glob", "Grep", "Write", "Edit"},
		{true, true, false}:   {"Read", "Glob", "Grep", "Write", "Edit", "Bash"},
		{false, false, true}:  {"Read", "Glob", "Grep", "Skill"},
		{false, true, true}:   {"Read", "Glob", "Grep", "Skill"},
		{true, false, true}:   {"Read", "Glob", "Grep", "Write", "Edit", "Skill"},
		{true, true, true}:    {"Read", "Glob", "Grep", "Write", "Edit", "Bash", "Skill"},
	}
	codexList := func(write, execute bool) []string {
		switch {
		case write && execute:
			return []string{"apply_patch", "shell"}
		case write:
			return []string{"apply_patch"}
		}
		return []string{}
	}
	opencodeList := func(write, execute bool) []string {
		switch {
		case write && execute:
			return []string{"read", "glob", "grep", "list", "edit", "bash"}
		case write:
			return []string{"read", "glob", "grep", "list", "edit"}
		}
		return []string{"read", "glob", "grep", "list"}
	}
	web := map[string][]string{
		"claude":   {"WebSearch", "WebFetch"},
		"":         {"WebSearch", "WebFetch"},
		"codex":    {"web_search"},
		"opencode": {"websearch", "webfetch"},
	}
	for _, backend := range []string{"claude", "", "codex", "opencode"} {
		for _, write := range []bool{false, true} {
			for _, execute := range []bool{false, true} {
				for _, skills := range []bool{false, true} {
					var want []string
					switch backend {
					case "claude", "":
						want = claude[combo{write, execute, skills}]
					case "codex":
						want = codexList(write, execute)
					case "opencode":
						want = opencodeList(write, execute)
					}
					c := Capabilities{WriteFiles: write, Execute: execute, Network: true, Tools: []string{"x"}}
					if got := nativeTools(backend, c, skills); !slices.Equal(got, want) || got == nil {
						t.Fatalf("%q write=%v execute=%v skills=%v: %v, want %v", backend, write, execute, skills, got, want)
					}
					c.Web = true
					wantWeb := append(slices.Clone(want), web[backend]...)
					if got := nativeTools(backend, c, skills); !slices.Equal(got, wantWeb) {
						t.Fatalf("%q web write=%v execute=%v skills=%v: %v, want %v", backend, write, execute, skills, got, wantWeb)
					}
				}
			}
		}
	}
}

func TestNetworkGrantsNoWebTools(t *testing.T) {
	for _, backend := range []string{"claude", "codex", "opencode"} {
		with := nativeTools(backend, Capabilities{Network: true}, false)
		without := nativeTools(backend, Capabilities{}, false)
		if !slices.Equal(with, without) {
			t.Fatalf("%s: network changed native tools: %v", backend, with)
		}
	}
}
