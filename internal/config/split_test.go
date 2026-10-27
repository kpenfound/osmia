package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoveProjectGroupPreservesOtherBytes(t *testing.T) {
	for _, key := range []string{
		"group_id", `"group_id"`, "'group_id'",
		`"group\u005fid"`, `"group\U0000005Fid"`,
		`"\u0067\u0072\u006f\u0075\u0070\u005f\u0069\u0064"`,
	} {
		for _, value := range []string{`""`, `"p_0123456789abcdef0123456789abcdef"`, "'p_0123456789abcdef0123456789abcdef'", "\"\"\"p_0123456789abcdef0123456789abcdef\"\"\""} {
			t.Run(key+value, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.toml")
				prefix := "# first\r\nversion = 1\r\nname = '''\ngroup_id = \"not a key\"\n'''\n  "
				suffix := "  # keep inline comment\r\nupstream = 'owner/repo'\r\n[capacity]\nper_workstream = 3 # keep\n"
				original := prefix + key + " = " + value + suffix
				if err := os.WriteFile(path, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err := RemoveProjectGroup(path); err != nil {
						t.Fatal(err)
					}
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != prefix+suffix {
					t.Fatalf("edited unrelated bytes: %q", got)
				}
				if strings.Count(string(got), "#") != 3 {
					t.Fatal("lost comment")
				}
			})
		}
	}
}
