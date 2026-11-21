package cli

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validTopLevel = `version = 1
[profiles.default]
agent = "claude"
model = "test"
`

// validateRun runs osmia with args and returns its exit code, stdout and
// stderr. It sets no --root: tests pass the flags they mean.
func validateRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, diag bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(""), &out, &diag)
	return code, out.String(), diag.String()
}

// shortDir is a short temporary directory, resolved through symlinks, so a
// default socket path under it stays within the Unix socket limit.
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

// snapshot lists every path under dir with its mode and, for files, its bytes.
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

func TestConfigValidateWarnsAndStillSucceeds(t *testing.T) {
	dir := shortDir(t)
	file := filepath.Join(dir, "staged.toml")
	writeFile(t, file, validTopLevel+"[roles.librarian]\n")
	code, _, diag := validateRun(t, "config", "validate", "--config", file)
	if code != 0 || !strings.Contains(diag, "roles.librarian is ignored") {
		t.Fatalf("code=%d stderr=%q", code, diag)
	}
}

func TestConfigValidateNamesTheFileAndFieldWithoutQuotingValues(t *testing.T) {
	const secret = "SECRET-VALUE-4f2a"
	for name, c := range map[string]struct{ text, field string }{
		"bad listen.web":      {validTopLevel + "[listen]\nweb = \"" + secret + "\"\n", "listen.web"},
		"unknown key":         {"bogus_setting = \"" + secret + "\"\n" + validTopLevel, "bogus_setting"},
		"wrong type":          {validTopLevel + "[capacity]\nmasons = \"" + secret + "\"\n", "capacity.masons"},
		"out of range":        {strings.Replace(validTopLevel, "version = 1", "version = 2", 1), "version"},
		"bad env name":        {validTopLevel + "[jev]\napi_key_env = \"not a name " + secret + "\"\n", "jev.api_key_env"},
		"malformed active id": {"active_projects = [\"" + secret + "\"]\n" + validTopLevel, "active_projects[0]"},
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

func TestConfigValidateRefusesAnUnreadableFile(t *testing.T) {
	dir := shortDir(t)
	missing := filepath.Join(dir, "missing.toml")
	code, out, diag := validateRun(t, "config", "validate", "--config", missing)
	if code == 0 || out != "" || !strings.HasPrefix(diag, missing+": ") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("the missing file was created: %v", err)
	}
	asDir := filepath.Join(dir, "directory.toml")
	must(t, os.Mkdir(asDir, 0700))
	if code, _, diag := validateRun(t, "config", "validate", "--config", asDir); code == 0 || !strings.HasPrefix(diag, asDir+": ") {
		t.Fatalf("code=%d stderr=%q", code, diag)
	}
}

func TestConfigValidateResolvesTheRootsFileWithoutConfig(t *testing.T) {
	dir := shortDir(t)
	root := filepath.Join(dir, "root")
	writeFile(t, filepath.Join(root, "config.toml"), validTopLevel)
	if code, out, diag := validateRun(t, "--root", root, "config", "validate"); code != 0 || diag != "" || !strings.Contains(out, filepath.Join(root, "config.toml")) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
	}
	// An explicit file wins over a valid root file, and an invalid root file
	// does not matter when --config names a valid one.
	elsewhere := filepath.Join(dir, "elsewhere.toml")
	writeFile(t, elsewhere, validTopLevel)
	writeFile(t, filepath.Join(root, "config.toml"), validTopLevel+"[listen]\nweb = \"example.com:80\"\n")
	if code, _, diag := validateRun(t, "--root", root, "config", "validate"); code == 0 || !strings.HasPrefix(diag, filepath.Join(root, "config.toml")+": listen.web: ") {
		t.Fatalf("root file: code=%d stderr=%q", code, diag)
	}
	if code, _, diag := validateRun(t, "--root", root, "config", "validate", "--config", elsewhere); code != 0 || diag != "" {
		t.Fatalf("--config: code=%d stderr=%q", code, diag)
	}
	// An invalid --config is reported even when the root's own file is valid.
	broken := filepath.Join(dir, "broken.toml")
	writeFile(t, broken, validTopLevel+"[listen]\nweb = \"example.com:80\"\n")
	writeFile(t, filepath.Join(root, "config.toml"), validTopLevel)
	if code, _, diag := validateRun(t, "--root", root, "config", "validate", "--config", broken); code == 0 || !strings.HasPrefix(diag, broken+": listen.web: ") {
		t.Fatalf("--config: code=%d stderr=%q", code, diag)
	}
}

func TestConfigValidateResolvesTheDefaultRootsFile(t *testing.T) {
	dir := shortDir(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	file := filepath.Join(dir, "config", "osmia", "config.toml")
	if code, _, diag := validateRun(t, "config", "validate"); code == 0 || !strings.HasPrefix(diag, file+": ") {
		t.Fatalf("missing default file: code=%d stderr=%q", code, diag)
	}
	writeFile(t, file, validTopLevel)
	if code, out, diag := validateRun(t, "config", "validate"); code != 0 || diag != "" || !strings.Contains(out, file) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
	}
	if _, err := os.Stat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
		t.Fatalf("the default root was created: %v", err)
	}
}

func TestConfigValidateAcceptsActiveProjects(t *testing.T) {
	const first, second = "p_11111111111111111111111111111111", "p_22222222222222222222222222222222"
	dir := shortDir(t)
	base := filepath.Join(dir, "base.toml")
	listed := filepath.Join(dir, "listed.toml")
	writeFile(t, base, validTopLevel)
	// The listed projects have no directory under the root, so a read of a
	// project file would fail the validation.
	writeFile(t, listed, "active_projects = [\""+first+"\", \""+second+"\"]\n"+validTopLevel)
	for _, file := range []string{base, listed} {
		if code, _, diag := validateRun(t, "--root", filepath.Join(dir, "root"), "config", "validate", "--config", file); code != 0 || diag != "" {
			t.Fatalf("%s: code=%d stderr=%q", file, code, diag)
		}
	}
	// A malformed list is still refused.
	writeFile(t, listed, "active_projects = [\""+first+"\", \""+first+"\"]\n"+validTopLevel)
	if code, _, diag := validateRun(t, "config", "validate", "--config", listed); code == 0 || !strings.HasPrefix(diag, listed+": active_projects: ") {
		t.Fatalf("code=%d stderr=%q", code, diag)
	}
}

func TestConfigValidateIsOffline(t *testing.T) {
	const listedProject = "p_33333333333333333333333333333333"
	const sentinel = "SENTINEL-ENV-VALUE-91c7"
	text := "active_projects = [\"" + listedProject + "\"]\n" + validTopLevel + "[jev]\napi_key_env = \"OSMIA_VALIDATE_JEV_KEY\"\n[transcribe]\napi_key_env = \"OSMIA_VALIDATE_TRANSCRIBE_KEY\"\n"
	for name, set := range map[string]bool{"variables unset": false, "variables set to a sentinel": true} {
		t.Run(name, func(t *testing.T) {
			for _, variable := range []string{"OSMIA_VALIDATE_JEV_KEY", "OSMIA_VALIDATE_TRANSCRIBE_KEY"} {
				if set {
					t.Setenv(variable, sentinel)
				} else {
					t.Setenv(variable, "")
					os.Unsetenv(variable)
				}
			}
			dir := shortDir(t)
			root := filepath.Join(dir, "root")
			file := filepath.Join(dir, "staged.toml")
			writeFile(t, file, text)

			// A root that does not exist stays uncreated, and no service
			// runs there: the command needs no socket.
			code, out, diag := validateRun(t, "--root", root, "config", "validate", "--config", file)
			if code != 0 || diag != "" || strings.Contains(out+diag, sentinel) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("the root was created: %v", err)
			}
			if got := snapshot(t, dir); len(got) != 2 {
				t.Fatalf("validation changed the directory: %v", got)
			}

			// An existing root is left as it was, with no trace, runtime
			// record or project file opened or written, and an unreadable
			// project file under it is never read.
			writeFile(t, filepath.Join(root, "projects", listedProject, "config.toml"), "not toml ][")
			writeFile(t, filepath.Join(root, "config.toml"), "version = 7\n")
			before := snapshot(t, dir)
			code, out, diag = validateRun(t, "--root", root, "config", "validate", "--config", file)
			if code != 0 || diag != "" || strings.Contains(out+diag, sentinel) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
			}
			after := snapshot(t, dir)
			if len(before) != len(after) {
				t.Fatalf("validation changed the files: before %d, after %d", len(before), len(after))
			}
			for p, v := range before {
				if after[p] != v {
					t.Fatalf("%s changed", p)
				}
			}
			for _, name := range []string{"trace.db", "osmia.lock", "runtime.json", "osmia.sock"} {
				if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
					t.Fatalf("%s exists after validation: %v", name, err)
				}
			}
		})
	}
}

func TestConfigValidateKeepsItsFormsDistinct(t *testing.T) {
	dir := shortDir(t)
	file := filepath.Join(dir, "staged.toml")
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
	if code, out, _ := validateRun(t, "--help"); code != 0 || !strings.Contains(out, "config validate [--config PATH]") {
		t.Fatalf("usage does not list config validate: %q", out)
	}
}
