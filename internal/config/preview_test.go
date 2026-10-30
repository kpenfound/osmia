package config

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestPreviewConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, table, field string }{
		{"off", "", ""},
		{"defaults", "[preview]\ncall = [\"serve\"]\nport = 8080\n", ""},
		{"explicit", "[preview]\ncall = [\"serve\", \"--dev\"]\nport = 65535\npath = \"/settings\"\ntimeout = \"30m\"\n", ""},
		{"empty table", "[preview]\n", "preview.call"},
		{"no call", "[preview]\nport = 80\n", "preview.call"},
		{"empty call", "[preview]\ncall = []\nport = 80\n", "preview.call"},
		{"no port", "[preview]\ncall = [\"serve\"]\n", "preview.port"},
		{"zero port", "[preview]\ncall = [\"serve\"]\nport = 0\n", "preview.port"},
		{"large port", "[preview]\ncall = [\"serve\"]\nport = 65536\n", "preview.port"},
		{"unknown", "[preview]\ncall = [\"serve\"]\nport = 80\nextra = true\n", "preview.extra"},
		{"nested", "[preview]\ncall = [\"serve\"]\nport = 80\n[preview.extra]\na = true\n", "preview.extra"},
		{"call type", "[preview]\ncall = \"serve\"\nport = 80\n", "preview.call"},
		{"argv type", "[preview]\ncall = [1]\nport = 80\n", "preview.call"},
		{"port type", "[preview]\ncall = [\"serve\"]\nport = \"80\"\n", "preview.port"},
		{"path type", "[preview]\ncall = [\"serve\"]\nport = 80\npath = 1\n", "preview.path"},
		{"timeout type", "[preview]\ncall = [\"serve\"]\nport = 80\ntimeout = 2\n", "preview.timeout"},
		{"path", "[preview]\ncall = [\"serve\"]\nport = 80\npath = \"settings\"\n", "preview.path"},
		{"empty path", "[preview]\ncall = [\"serve\"]\nport = 80\npath = \"\"\n", "preview.path"},
		{"short timeout", "[preview]\ncall = [\"serve\"]\nport = 80\ntimeout = \"59s\"\n", "preview.timeout"},
		{"long timeout", "[preview]\ncall = [\"serve\"]\nport = 80\ntimeout = \"31m\"\n", "preview.timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := fixture(t, topConfig, projectConfig+tc.table)
			c, err := Load(opts)
			if tc.field != "" {
				if err == nil || !strings.Contains(err.Error(), tc.field) {
					t.Fatalf("want %s: %v", tc.field, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "off" {
				if c.Project.Preview != nil {
					t.Fatal(c.Project.Preview)
				}
				return
			}
			p := c.Project.Preview
			if p == nil || len(p.Call) == 0 {
				t.Fatalf("preview: %+v", p)
			}
			if tc.name == "defaults" && (p.Path != "/" || p.Timeout != "5m") {
				t.Fatal(p)
			}
			if tc.name == "explicit" && (p.Port != 65535 || p.Path != "/settings" || p.Timeout != "30m") {
				t.Fatal(p)
			}
		})
	}
}

func TestPreviewTextEdit(t *testing.T) {
	before := projectConfig + "name = '''a multi-line\n[preview]\nlooks like a table\n'''\n# preserve this\n"
	after := "\n# next table\n[capacity] # comment\nper_workstream = 3\ngit_operations = 2\n"
	for _, table := range []string{
		"[preview]\ncall = [\n 'serve', # argument\n '--dev',\n]\nport = 80\n",
		"['preview'] # header\ncall = ['serve']\nport = 80\n",
		"preview = { call = ['serve'], port = 80 }\n",
		"preview.call = ['serve']\npreview.port = 80\n",
	} {
		opts := fixture(t, topConfig, before+table+after)
		cfg, err := Load(opts)
		if err != nil {
			t.Fatal(err)
		}
		path, _ := cfg.Root.ProjectConfig(pid)
		want := &Preview{Call: []string{"web", "--port=81"}, Port: 81, Path: "/test", Timeout: "1m"}
		saved, err := cfg.Project.SavePreview(path, want)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != before+after+previewTable(want) {
			t.Fatalf("other bytes changed:\n%s", data)
		}
		info, _ := os.Stat(path)
		same, err := saved.SavePreview(path, want)
		if err != nil {
			t.Fatal(err)
		}
		now, _ := os.Stat(path)
		if !os.SameFile(info, now) {
			t.Fatal("idempotent save replaced file")
		}
		if _, err := same.SavePreview(path, nil); err != nil {
			t.Fatal(err)
		}
		data, _ = os.ReadFile(path)
		if string(data) != before+after {
			t.Fatalf("clear changed other bytes:\n%s", data)
		}
	}
}

func TestPreviewByteBaseline(t *testing.T) {
	opts := fixture(t, topConfig, projectConfig)
	cfg, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := cfg.Root.ProjectConfig(pid)
	p := cfg.Project
	want := &Preview{Call: []string{"serve"}, Port: 80, Path: "/", Timeout: "5m"}
	p, err = p.SavePreview(path, want)
	if err != nil {
		t.Fatal(err)
	}
	p, err = p.SavePreview(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Preview != nil {
		t.Fatal(p.Preview)
	}
	write(t, path, projectConfig+"# edit\n")
	if _, err := p.SavePreview(path, nil); !errors.Is(err, ErrProjectDrift) {
		t.Fatal(err)
	}
	cfg, err = Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	p, err = cfg.Project.SavePreview(path, want)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Load(opts)
	if err != nil || !reflect.DeepEqual(again.Project.Preview, p.Preview) {
		t.Fatalf("%+v %v", again, err)
	}
}
