package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestProjectPreviewTableIsIgnored(t *testing.T) {
	opts := fixture(t, topConfig, projectConfig)
	baseline, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	path, err := baseline.Root.ProjectConfig(pid)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"[preview]\n",
		"# owner formatting\n[preview]\ncall = ['unused', '--source=/outside']\nport = 8080\npath = '/unused'\ntimeout = '30m'\n",
		"[preview]\ncall = 1\nport = 'ignored'\npath = false\ntimeout = 'invalid'\n",
	} {
		text := projectConfig + table
		write(t, path, text)
		for i := 0; i < 2; i++ {
			loaded, err := Load(opts)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded.Projects, baseline.Projects) || !reflect.DeepEqual(loaded.Project, baseline.Project) {
				t.Fatalf("ignored table changed effective projects: %+v", loaded.Projects)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != text {
				t.Fatal("loading rewrote the project file")
			}
		}
	}
	for _, extra := range []string{"unknown = true\n", "[unknown]\n", "[preview]\nunknown = true\n"} {
		write(t, path, projectConfig+extra)
		if _, err := Load(opts); err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("unknown key accepted: %v", err)
		}
	}
}
