package cli

import (
	"strings"
	"testing"
)

func TestProjectPreviewCommandIsUnknown(t *testing.T) {
	code, out, err := invoke(t, "", "project", "preview", project)
	unknownCode, unknownOut, unknownErr := invoke(t, "", "project", "unknown", project)
	if code != 2 || code != unknownCode || out != unknownOut || err != unknownErr {
		t.Fatalf("preview: %d %q %q; unknown: %d %q %q", code, out, err, unknownCode, unknownOut, unknownErr)
	}
	for _, flag := range []string{"--call", "--port", "--path", "--timeout", "--clear"} {
		if _, err := parse([]string{flag}); err == nil {
			t.Fatalf("accepted %s", flag)
		}
	}
	_, help, _ := invoke(t, "", "--help")
	for _, removed := range []string{"project preview", "--call", "--port", "--path", "--timeout", "--clear"} {
		if strings.Contains(help, removed) {
			t.Fatalf("help advertises %s", removed)
		}
	}
}
