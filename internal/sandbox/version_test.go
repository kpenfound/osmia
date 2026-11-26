package sandbox

import "testing"

func TestAtLeast(t *testing.T) {
	for _, tt := range []struct {
		have, minimum string
		want          bool
	}{
		{"2.1.321", "2.1.321", true},
		{"2.1.322", "2.1.321", true},
		{"2.1.1000", "2.1.321", true},
		{"2.2.0", "2.1.321", true},
		{"3.0.0", "2.1.321", true},
		{"2.1.320", "2.1.321", false},
		{"2.0.9999", "2.1.321", false},
		{"1.99.99", "2.1.321", false},
		{"0.173.0-alpha.4", "0.136.0", true},
		{"0.136.0+build", "0.136.0", true},
		{"0.135.0", "0.136.0", false},
		{"", "0.136.0", false},
		{"unknown", "0.136.0", false},
		{"2.1", "2.1.321", false},
		{"2.1.x", "2.1.321", false},
	} {
		if got := atLeast(tt.have, tt.minimum); got != tt.want {
			t.Errorf("atLeast(%q, %q) = %v, want %v", tt.have, tt.minimum, got, tt.want)
		}
	}
}

func TestFirstVersion(t *testing.T) {
	for output, want := range map[string]string{
		"2.1.321 (Claude Code)":      "2.1.321",
		"codex-cli 0.173.0-alpha.4":  "0.173.0-alpha.4",
		"claude 2.1.321\n":           "2.1.321",
		"warning\nversion 1.2.3\n":   "1.2.3",
		"no version at all":          "",
		"":                           "",
		"build 12 of 2.1 (Claude)":   "",
		"Claude Code v2.1.400 (new)": "2.1.400",
	} {
		if got := firstVersion(output); got != want {
			t.Errorf("firstVersion(%q) = %q, want %q", output, got, want)
		}
	}
}

func TestVersionField(t *testing.T) {
	for output, want := range map[string]string{
		"version:     v0.19.0":                "v0.19.0",
		"dagger\nversion: v0.19.0 (engine)\n": "v0.19.0",
		"dagger v0.19.0":                      "",
		"version:":                            "",
		"":                                    "",
	} {
		if got := versionField(output); got != want {
			t.Errorf("versionField(%q) = %q, want %q", output, got, want)
		}
	}
}

func TestGuestScriptsAreEmbedded(t *testing.T) {
	for name, text := range map[string]string{
		"removal": GuestRemoval(), "probe": GuestProbe(), "claude": ClaudeInstall(), "dagger": DaggerInstall(),
	} {
		if text == "" || text[len(text)-1] != '\n' {
			t.Errorf("%s script is empty or has no final newline", name)
		}
	}
}
