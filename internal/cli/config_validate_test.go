package cli

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validTopLevel = `version = 1
[profiles.default]
agent = "claude"
model = "test"
`

func validateRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, diag bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(""), &out, &diag)
	return code, out.String(), diag.String()
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cv-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	dir, err = filepath.EvalSymlinks(dir)
	must(t, err)
	return dir
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	must(t, os.WriteFile(path, []byte(text), 0600))
}

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	must(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		content := ""
		if !d.IsDir() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			content = string(data)
		}
		got[p] = info.Mode().String() + "\x00" + content
		return nil
	}))
	return got
}

func TestConfigValidateAcceptsAValidFile(t *testing.T) {
	dir := shortDir(t)
	file := filepath.Join(dir, "staged.toml")
	writeFile(t, file, validTopLevel)
	code, out, diag := validateRun(t, "config", "validate", "--config", file)
	if code != 0 || diag != "" || !strings.Contains(out, file) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
	}
}

func TestConfigValidateAcceptsRoleSkillLists(t *testing.T) {
	const base = "https://github.com/kpenfound/skills#"
	list := func(names ...string) string {
		return "[\"" + base + strings.Join(names, "\", \""+base) + "\"]"
	}
	file := filepath.Join(shortDir(t), "staged.toml")
	writeFile(t, file, validTopLevel+
		"[roles.mason]\nskills = "+list("behavioral-testing", "simple-code", "no-deltas", "lint-discipline", "unslop", "typescript-best-practices")+"\n"+
		"[roles.reviewer]\nskills = "+list("pr-review", "simple-code", "no-deltas", "behavioral-testing")+"\n")
	code, out, diag := validateRun(t, "config", "validate", "--config", file)
	if code != 0 || diag != "" || !strings.Contains(out, file) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
	}
}

func TestConfigValidateNamesTheFileAndFieldWithoutQuotingValues(t *testing.T) {
	const secret = "SECRET-VALUE-4f2a"
	for name, c := range map[string]struct{ text, field string }{
		"bad listen.web": {validTopLevel + "[listen]\nweb = \"" + secret + "\"\n", "listen.web"},
		"unknown key":    {"bogus_setting = \"" + secret + "\"\n" + validTopLevel, "bogus_setting"},
		"wrong type":     {validTopLevel + "[capacity]\nmasons = \"" + secret + "\"\n", "capacity.masons"},
		"out of range":   {strings.Replace(validTopLevel, "version = 1", "version = 2", 1), "version"},
		"bad env name":   {validTopLevel + "[jev]\napi_key_env = \"not a name " + secret + "\"\n", "jev.api_key_env"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := shortDir(t)
			file := filepath.Join(dir, "staged.toml")
			writeFile(t, file, c.text)
			code, out, diag := validateRun(t, "config", "validate", "--config", file)
			if code == 0 || out != "" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
			}
			if !strings.HasPrefix(diag, file+": "+c.field+": ") {
				t.Fatalf("stderr does not name the file and %s: %q", c.field, diag)
			}
			if strings.Contains(diag, secret) {
				t.Fatalf("stderr quotes a value from the file: %q", diag)
			}
		})
	}
}

func TestConfigValidateRefusesAMissingFileAndCreatesNothing(t *testing.T) {
	missing := filepath.Join(shortDir(t), "missing.toml")
	code, out, diag := validateRun(t, "config", "validate", "--config", missing)
	if code == 0 || out != "" || !strings.HasPrefix(diag, missing+": ") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("the missing file was created: %v", err)
	}
}

func TestConfigValidateRefusesADirectory(t *testing.T) {
	asDir := filepath.Join(shortDir(t), "directory.toml")
	must(t, os.Mkdir(asDir, 0700))
	if code, _, diag := validateRun(t, "config", "validate", "--config", asDir); code == 0 || !strings.HasPrefix(diag, asDir+": ") {
		t.Fatalf("code=%d stderr=%q", code, diag)
	}
}

func validateRootWithFile(t *testing.T, rootConfig string) (dir, root string) {
	t.Helper()
	dir = shortDir(t)
	root = filepath.Join(dir, "root")
	writeFile(t, filepath.Join(root, "config.toml"), rootConfig)
	return dir, root
}

func TestConfigValidateAcceptsTheRootsFileWithoutConfig(t *testing.T) {
	_, root := validateRootWithFile(t, validTopLevel)
	if code, out, diag := validateRun(t, "--root", root, "config", "validate"); code != 0 || diag != "" || !strings.Contains(out, filepath.Join(root, "config.toml")) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
	}
}

func TestConfigValidateNamesTheRootsFileWhenItIsInvalid(t *testing.T) {
	_, root := validateRootWithFile(t, validTopLevel+"[listen]\nweb = \"example.com:80\"\n")
	if code, _, diag := validateRun(t, "--root", root, "config", "validate"); code == 0 || !strings.HasPrefix(diag, filepath.Join(root, "config.toml")+": listen.web: ") {
		t.Fatalf("root file: code=%d stderr=%q", code, diag)
	}
}

func TestConfigValidateChecksTheFileNamedByConfigInsteadOfTheRootsFile(t *testing.T) {
	dir, root := validateRootWithFile(t, validTopLevel+"[listen]\nweb = \"example.com:80\"\n")
	elsewhere := filepath.Join(dir, "elsewhere.toml")
	writeFile(t, elsewhere, validTopLevel)
	if code, _, diag := validateRun(t, "--root", root, "config", "validate", "--config", elsewhere); code != 0 || diag != "" {
		t.Fatalf("--config: code=%d stderr=%q", code, diag)
	}
}

func TestConfigValidateNamesTheFileNamedByConfigWhenItIsInvalid(t *testing.T) {
	dir, root := validateRootWithFile(t, validTopLevel)
	broken := filepath.Join(dir, "broken.toml")
	writeFile(t, broken, validTopLevel+"[listen]\nweb = \"example.com:80\"\n")
	if code, _, diag := validateRun(t, "--root", root, "config", "validate", "--config", broken); code == 0 || !strings.HasPrefix(diag, broken+": listen.web: ") {
		t.Fatalf("--config: code=%d stderr=%q", code, diag)
	}
}

func useDefaultConfigHome(t *testing.T) (dir, file string) {
	t.Helper()
	dir = shortDir(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	return dir, filepath.Join(dir, "config", "osmia", "config.toml")
}

func TestConfigValidateNamesTheMissingDefaultFile(t *testing.T) {
	_, file := useDefaultConfigHome(t)
	if code, _, diag := validateRun(t, "config", "validate"); code == 0 || !strings.HasPrefix(diag, file+": ") {
		t.Fatalf("missing default file: code=%d stderr=%q", code, diag)
	}
}

func TestConfigValidateAcceptsTheDefaultRootsFile(t *testing.T) {
	_, file := useDefaultConfigHome(t)
	writeFile(t, file, validTopLevel)
	if code, out, diag := validateRun(t, "config", "validate"); code != 0 || diag != "" || !strings.Contains(out, file) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
	}
}

func TestConfigValidateDoesNotCreateTheDefaultRoot(t *testing.T) {
	dir, file := useDefaultConfigHome(t)
	writeFile(t, file, validTopLevel)
	validateRun(t, "config", "validate")
	if _, err := os.Stat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
		t.Fatalf("the default root was created: %v", err)
	}
}

const offlineConfig = validTopLevel + "[jev]\napi_key_env = \"OSMIA_VALIDATE_JEV_KEY\"\n[transcribe]\napi_key_env = \"OSMIA_VALIDATE_TRANSCRIBE_KEY\"\n"

const offlineSentinel = "SENTINEL-ENV-VALUE-91c7"

func forEachEnvironmentState(t *testing.T, check func(t *testing.T)) {
	t.Helper()
	for name, set := range map[string]bool{"variables unset": false, "variables set to a sentinel": true} {
		t.Run(name, func(t *testing.T) {
			for _, variable := range []string{"OSMIA_VALIDATE_JEV_KEY", "OSMIA_VALIDATE_TRANSCRIBE_KEY"} {
				if set {
					t.Setenv(variable, offlineSentinel)
				} else {
					t.Setenv(variable, "")
					os.Unsetenv(variable)
				}
			}
			check(t)
		})
	}
}

func TestConfigValidateNeedsNoEnvironmentSecrets(t *testing.T) {
	forEachEnvironmentState(t, func(t *testing.T) {
		dir := shortDir(t)
		file := filepath.Join(dir, "staged.toml")
		writeFile(t, file, offlineConfig)
		code, out, diag := validateRun(t, "--root", filepath.Join(dir, "root"), "config", "validate", "--config", file)
		if code != 0 || diag != "" || strings.Contains(out+diag, offlineSentinel) {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
		}
	})
}

func TestConfigValidateCreatesNoRoot(t *testing.T) {
	forEachEnvironmentState(t, func(t *testing.T) {
		dir := shortDir(t)
		root := filepath.Join(dir, "root")
		file := filepath.Join(dir, "staged.toml")
		writeFile(t, file, offlineConfig)
		validateRun(t, "--root", root, "config", "validate", "--config", file)
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("the root was created: %v", err)
		}
		if got := snapshot(t, dir); len(got) != 2 {
			t.Fatalf("validation changed the directory: %v", got)
		}
	})
}

func TestConfigValidateLeavesAnExistingRootUntouched(t *testing.T) {
	forEachEnvironmentState(t, func(t *testing.T) {
		dir := shortDir(t)
		root := filepath.Join(dir, "root")
		file := filepath.Join(dir, "staged.toml")
		writeFile(t, file, offlineConfig)
		writeFile(t, filepath.Join(root, "config.toml"), "version = 7\n")
		before := snapshot(t, dir)
		code, out, diag := validateRun(t, "--root", root, "config", "validate", "--config", file)
		if code != 0 || diag != "" || strings.Contains(out+diag, offlineSentinel) {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
		}
		if after := snapshot(t, dir); !reflect.DeepEqual(before, after) {
			t.Fatalf("validation changed the files: before %v, after %v", before, after)
		}
	})
}

func TestConfigValidateStartsNoServiceInTheRoot(t *testing.T) {
	forEachEnvironmentState(t, func(t *testing.T) {
		dir := shortDir(t)
		root := filepath.Join(dir, "root")
		file := filepath.Join(dir, "staged.toml")
		writeFile(t, file, offlineConfig)
		writeFile(t, filepath.Join(root, "config.toml"), "version = 7\n")
		validateRun(t, "--root", root, "config", "validate", "--config", file)
		for _, name := range []string{"trace.db", "osmia.lock", "runtime.json", "osmia.sock"} {
			if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
				t.Fatalf("%s exists after validation: %v", name, err)
			}
		}
	})
}

func TestConfigValidateRefusesInvalidArguments(t *testing.T) {
	file := filepath.Join(shortDir(t), "staged.toml")
	writeFile(t, file, validTopLevel)
	for _, args := range [][]string{
		{"config", "validate", "extra"},
		{"config", "validate", "--json", "--config", file},
		{"config", "validate", "--socket", "x.sock"},
		{"config", "--config", file},
		{"status", "--config", file},
		{"config", "validate", "--config"},
		{"config", "validate", "--config", file, "--config", file},
	} {
		if code, out, diag := validateRun(t, args...); code != 2 || out != "" || !strings.Contains(diag, "invalid arguments") {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, out, diag)
		}
	}
}

func TestUsageListsConfigValidate(t *testing.T) {
	if code, out, _ := validateRun(t, "--help"); code != 0 || !strings.Contains(out, "config validate [--config PATH]") {
		t.Fatalf("usage does not list config validate: %q", out)
	}
}

func TestConfigValidateAcceptsTheManagerKeysWithoutAWarning(t *testing.T) {
	file := filepath.Join(shortDir(t), "staged.toml")
	writeFile(t, file, validTopLevel+"[capacity]\nmanager = 4\n[roles.manager]\nprofile = \"default\"\n")
	code, out, diag := validateRun(t, "config", "validate", "--config", file)
	if code != 0 || diag != "" || !strings.Contains(out, file+": valid") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
	}
}

func TestConfigValidateWarnsOncePerDeprecatedManagerKeyAndStaysValid(t *testing.T) {
	file := filepath.Join(shortDir(t), "staged.toml")
	writeFile(t, file, validTopLevel+"[capacity]\nchief = 4\n[roles.chief_of_staff]\nprofile = \"default\"\n")
	code, out, diag := validateRun(t, "config", "validate", "--config", file)
	if code != 0 || !strings.Contains(out, file+": valid") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
	}
	for _, want := range []string{"warning: " + file + ": capacity.chief: deprecated, use capacity.manager\n", "warning: " + file + ": roles.chief_of_staff: deprecated, use roles.manager\n"} {
		if strings.Count(diag, want) != 1 {
			t.Fatalf("stderr lacks one %q: %q", want, diag)
		}
	}
	if strings.Count(diag, "warning:") != 2 {
		t.Fatalf("stderr: %q", diag)
	}
}

func TestConfigValidateRefusesBothFormsOfAManagerKey(t *testing.T) {
	for name, c := range map[string]struct{ text, first, second string }{
		"capacity": {"[capacity]\nchief = 4\nmanager = 4\n", "capacity.manager", "capacity.chief"},
		"roles":    {"[roles.chief_of_staff]\nprofile = \"default\"\n[roles.manager]\nprofile = \"default\"\n", "roles.manager", "roles.chief_of_staff"},
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(shortDir(t), "staged.toml")
			writeFile(t, file, validTopLevel+c.text)
			code, out, diag := validateRun(t, "config", "validate", "--config", file)
			if code == 0 || out != "" || !strings.HasPrefix(diag, file+": "+c.first+": ") || !strings.Contains(diag, c.second) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
			}
		})
	}
}
