package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestProjectWithoutVerificationTableIsDisabled(t *testing.T) {
	c, err := load(fixture(t, topConfig, projectConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Project.Verification.Enabled() || c.Project.Verification.Call != nil || c.Project.Verification.LeverPaths != nil {
		t.Fatalf("setting %+v", c.Project.Verification)
	}
}

func TestVerificationTableLoadsItsArgumentsDirectoryAndGlobs(t *testing.T) {
	c, err := load(fixture(t, topConfig, projectConfig+`[verification]
call = ["lever", "--source=."]
map_dir = "docs/features"
lever_paths = ["lever/**", "dagger.json"]
`))
	if err != nil {
		t.Fatal(err)
	}
	v := c.Project.Verification
	if !v.Enabled() || !slices.Equal(v.Call, []string{"lever", "--source=."}) || v.MapDir != "docs/features" || v.Directory() != "docs/features" || !slices.Equal(v.LeverPaths, []string{"lever/**", "dagger.json"}) {
		t.Fatalf("setting %+v", v)
	}
}

func TestVerificationMapDirectoryDefaultsToDocsVerification(t *testing.T) {
	c, err := load(fixture(t, topConfig, projectConfig+"[verification]\ncall = [\"lever\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if v := c.Project.Verification; v.MapDir != "docs/verification" {
		t.Fatalf("setting %+v", v)
	}
}

func TestInvalidVerificationSettingsFailLoadNamingTheField(t *testing.T) {
	for name, tc := range map[string]struct{ table, field string }{
		"no call":                 {"[verification]\nmap_dir = \"docs/verification\"\n", "verification.call"},
		"empty call":              {"[verification]\ncall = []\n", "verification.call"},
		"shorthand option":        {"[verification]\ncall = [\"lever\", \"-x\"]\n", "verification.call"},
		"scheme":                  {"[verification]\ncall = [\"lever\", \"--source=https://example.com/x\"]\n", "verification.call"},
		"parent path":             {"[verification]\ncall = [\"lever\", \"--source=../x\"]\n", "verification.call"},
		"host step":               {"[verification]\ncall = [\"lever\", \"export\"]\n", "verification.call"},
		"run id":                  {"[verification]\ncall = [\"lever\", \"--run-id\", \"fixed\"]\n", "verification.call"},
		"run id spelled apart":    {"[verification]\ncall = [\"lever\", \"--run_id=fixed\"]\n", "verification.call"},
		"absolute map":            {"[verification]\ncall = [\"lever\"]\nmap_dir = \"/etc\"\n", "verification.map_dir"},
		"escaping map":            {"[verification]\ncall = [\"lever\"]\nmap_dir = \"../docs\"\n", "verification.map_dir"},
		"nested escaping map":     {"[verification]\ncall = [\"lever\"]\nmap_dir = \"docs/../../x\"\n", "verification.map_dir"},
		"glob map":                {"[verification]\ncall = [\"lever\"]\nmap_dir = \"docs/*\"\n", "verification.map_dir"},
		"escaping lever glob":     {"[verification]\ncall = [\"lever\"]\nlever_paths = [\"../lever/**\"]\n", "verification.lever_paths"},
		"absolute lever glob":     {"[verification]\ncall = [\"lever\"]\nlever_paths = [\"/lever\"]\n", "verification.lever_paths"},
		"malformed lever glob":    {"[verification]\ncall = [\"lever\"]\nlever_paths = [\"lever/[\"]\n", "verification.lever_paths"},
		"unknown key":             {"[verification]\ncall = [\"lever\"]\nmap = \"docs\"\n", "verification.map"},
		"call has the wrong type": {"[verification]\ncall = \"lever\"\n", "verification.call"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(fixture(t, topConfig, projectConfig+tc.table))
			var field *FieldError
			if !errors.As(err, &field) || field.Field != tc.field {
				t.Fatalf("error %v, want a field error naming %s", err, tc.field)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error %q does not name %s", err, tc.field)
			}
		})
	}
}
