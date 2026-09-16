package kb

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// writeTree creates files (and their directories) under dir; a name ending in
// "/" is an empty directory.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if name[len(name)-1] == '/' {
			if err := os.MkdirAll(full, 0755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(files[name]), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// snapshot records every entry's type, mode, modification time and content.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		sum := ""
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum = fmt.Sprintf("%x", sha256.Sum256(data))
		}
		out[p] = fmt.Sprintf("%v %d %s", info.Mode(), info.ModTime().UnixNano(), sum)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var fixtureFiles = map[string]string{
	".git/HEAD":           "ref: refs/heads/main\n",
	".github/CODEOWNERS":  "# Owners\n*       @org/everyone\n/internal/          @org/core # core team\ninternal/trace/     @org/trace @alice\ndocs/               @org/docs\n/docs/api.md        @org/api\n/missing/file.go    @org/nobody\n/.github/           @org/infra\n/src/[bad           @org/bad\n",
	"CODEOWNERS":          "* @ignored\n",
	".hidden/x.go":        "package x\n",
	"README.md":           "# Fixture\n",
	"cmd/tool/main.go":    "package main\n",
	"cmd/.cache/":         "",
	"docs/api.md":         "# API\n",
	"docs/guide/":         "",
	"internal/trace/t.go": "package trace\n",
	"internal/kb/kb.go":   "package kb\n",
	"internal/Web_UI/":    "",
	"pkg/trace/":          "",
	"web/app/":            "",
	"odd[name]/":          "",
}

const fixtureSeed = `{
  "version": 1,
  "entities": [
    {
      "id": "cmd",
      "name": "cmd",
      "aliases": [],
      "paths": [
        "cmd"
      ],
      "owners": [
        "@org/everyone"
      ],
      "part_of": []
    },
    {
      "id": "cmd.tool",
      "name": "cmd/tool",
      "aliases": [
        "tool"
      ],
      "paths": [
        "cmd/tool"
      ],
      "owners": [
        "@org/everyone"
      ],
      "part_of": [
        "cmd"
      ]
    },
    {
      "id": "docs",
      "name": "docs",
      "aliases": [],
      "paths": [
        "docs"
      ],
      "owners": [
        "@org/docs"
      ],
      "part_of": []
    },
    {
      "id": "docs.api.md",
      "name": "docs/api.md",
      "aliases": [
        "api.md"
      ],
      "paths": [
        "docs/api.md"
      ],
      "owners": [
        "@org/api"
      ],
      "part_of": [
        "docs"
      ]
    },
    {
      "id": "internal",
      "name": "internal",
      "aliases": [],
      "paths": [
        "internal"
      ],
      "owners": [
        "@org/core"
      ],
      "part_of": []
    },
    {
      "id": "internal.kb",
      "name": "internal/kb",
      "aliases": [
        "kb"
      ],
      "paths": [
        "internal/kb"
      ],
      "owners": [
        "@org/core"
      ],
      "part_of": [
        "internal"
      ]
    },
    {
      "id": "internal.trace",
      "name": "internal/trace",
      "aliases": [],
      "paths": [
        "internal/trace"
      ],
      "owners": [
        "@alice",
        "@org/trace"
      ],
      "part_of": [
        "internal"
      ]
    },
    {
      "id": "internal.web-ui",
      "name": "internal/Web_UI",
      "aliases": [
        "Web_UI"
      ],
      "paths": [
        "internal/Web_UI"
      ],
      "owners": [
        "@org/core"
      ],
      "part_of": [
        "internal"
      ]
    },
    {
      "id": "pkg",
      "name": "pkg",
      "aliases": [],
      "paths": [
        "pkg"
      ],
      "owners": [
        "@org/everyone"
      ],
      "part_of": []
    },
    {
      "id": "pkg.trace",
      "name": "pkg/trace",
      "aliases": [],
      "paths": [
        "pkg/trace"
      ],
      "owners": [
        "@org/everyone"
      ],
      "part_of": [
        "pkg"
      ]
    },
    {
      "id": "web",
      "name": "web",
      "aliases": [],
      "paths": [
        "web"
      ],
      "owners": [
        "@org/everyone"
      ],
      "part_of": []
    }
  ]
}
`

func seedBytes(t *testing.T, clone string) []byte {
	t.Helper()
	m, err := Seed(clone)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSeedFromCodeownersAndStructure(t *testing.T) {
	clone := t.TempDir()
	writeTree(t, clone, fixtureFiles)
	// A symlinked directory is not followed.
	if err := os.Symlink(t.TempDir(), filepath.Join(clone, "linked")); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, clone)
	got := seedBytes(t, clone)
	if string(got) != fixtureSeed {
		t.Fatalf("seed:\n%s", got)
	}
	if again := seedBytes(t, clone); string(again) != string(got) {
		t.Fatal("seeding the same clone twice differs")
	}
	if after := snapshot(t, clone); !reflect.DeepEqual(before, after) {
		t.Fatal("seeding changed the clone")
	}
	// A clone with the same files written in another order seeds the same bytes.
	other := t.TempDir()
	names := make([]string, 0, len(fixtureFiles))
	for name := range fixtureFiles {
		names = append(names, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names {
		writeTree(t, other, map[string]string{name: fixtureFiles[name]})
	}
	if string(seedBytes(t, other)) != fixtureSeed {
		t.Fatal("seed depends on creation order")
	}
}

func TestSeedReadOnlyClone(t *testing.T) {
	clone := t.TempDir()
	writeTree(t, clone, map[string]string{"CODEOWNERS": "* @a\n", "src/x.go": "package x\n"})
	if err := os.Chmod(filepath.Join(clone, "src"), 0555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(clone, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(clone, 0755); os.Chmod(filepath.Join(clone, "src"), 0755) })
	m, err := Seed(clone)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entities) != 1 || m.Entities[0].ID != "src" || !reflect.DeepEqual(m.Entities[0].Owners, []string{"@a"}) {
		t.Fatalf("seed: %#v", m)
	}
}

func TestSeedCodeownersPrecedence(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"github first", map[string]string{".github/CODEOWNERS": "* @github\n", "CODEOWNERS": "* @root\n", "docs/CODEOWNERS": "* @docs\n"}, []string{"@github"}},
		{"root second", map[string]string{"CODEOWNERS": "* @root\n", "docs/CODEOWNERS": "* @docs\n"}, []string{"@root"}},
		{"docs third", map[string]string{"docs/CODEOWNERS": "* @docs\n"}, []string{"@docs"}},
		{"directory is not a file", map[string]string{".github/CODEOWNERS/": "", "CODEOWNERS": "* @root\n", "docs/x.md": ""}, []string{"@root"}},
		{"none", map[string]string{"docs/readme.md": ""}, []string{}},
		{"unowned by a later rule", map[string]string{"CODEOWNERS": "* @root\n/docs/\n", "docs/x.md": ""}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clone := t.TempDir()
			writeTree(t, clone, c.files)
			m, err := Seed(clone)
			if err != nil {
				t.Fatal(err)
			}
			docs, ok := m.Lookup("docs")
			if !ok || !reflect.DeepEqual(docs.Owners, c.want) {
				t.Fatalf("docs: %#v", m)
			}
		})
	}
}

func TestTranslateCodeownersPatterns(t *testing.T) {
	for in, want := range map[string]struct {
		pattern string
		literal bool
	}{
		"*":              {"**/*", false},
		"/":              {"**", false},
		"**":             {"**", false},
		"*.js":           {"**/*.js", false},
		"docs/":          {"**/docs", false},
		"/docs/":         {"docs", true},
		"docs/api/":      {"docs/api", true},
		"apps/**":        {"apps", true},
		"**/logs":        {"**/logs", false},
		"/build/logs/*":  {"build/logs/*", false},
		"src/**/test.go": {"src/**/test.go", false},
	} {
		got, literal, ok := translate(in)
		if !ok || got != want.pattern || literal != want.literal {
			t.Errorf("translate(%q) = %q %v %v, want %q %v", in, got, literal, ok, want.pattern, want.literal)
		}
	}
	for _, in := range []string{"/src/[bad", "../x", "a/./b"} {
		if _, _, ok := translate(in); ok {
			t.Errorf("translate(%q) accepted", in)
		}
	}
}

func TestSeedIDCollisionsAndAliases(t *testing.T) {
	clone := t.TempDir()
	writeTree(t, clone, map[string]string{"a_b/": "", "a-b/": "", "cmd/x/": "", "pkg/x/": "", "pkg/cmd/": "", "internal/y/": ""})
	m, err := Seed(clone)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(m); err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, e := range m.Entities {
		got[e.Primary()] = append([]string{e.ID}, e.Aliases...)
	}
	want := map[string][]string{
		"a-b": {"a-b"}, "a_b": {"a-b-2"},
		"cmd": {"cmd"}, "cmd/x": {"cmd.x"}, "pkg": {"pkg"}, "pkg/x": {"pkg.x"}, "pkg/cmd": {"pkg.cmd"},
		"internal": {"internal"}, "internal/y": {"internal.y", "y"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestSeedSkipsPathsWithoutValidID(t *testing.T) {
	clone := t.TempDir()
	writeTree(t, clone, map[string]string{
		"CODEOWNERS":         "/_config/ @cfg\n/src/_gen/ @gen\n",
		"_site/":             "",
		"__tests__/":         "",
		"@types/":            "",
		"élan/":              "",
		"-dash/":             "",
		"_config/":           "",
		"packages/@types/":   "",
		"packages/_private/": "",
		"src/_gen/":          "",
		"src/ok/":            "",
	})
	m, err := Seed(clone)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Encode(m); err != nil {
		t.Fatalf("seed does not encode: %v", err)
	}
	var got []string
	for _, e := range m.Entities {
		got = append(got, e.ID+"="+e.Primary())
	}
	want := []string{"packages=packages", "packages.-types=packages/@types", "packages.-private=packages/_private", "src=src", "src.-gen=src/_gen"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestSeedIgnoresCodeownersOutsideClone(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "CODEOWNERS")
	if err := os.WriteFile(outside, []byte("* @outside\n"), 0644); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(base, "clone")
	writeTree(t, clone, map[string]string{".github/": "", "CODEOWNERS": "* @root\n", "docs/": ""})
	if err := os.Symlink(outside, filepath.Join(clone, ".github", "CODEOWNERS")); err != nil {
		t.Fatal(err)
	}
	m, err := Seed(clone)
	if err != nil {
		t.Fatal(err)
	}
	docs, ok := m.Lookup("docs")
	if !ok || !reflect.DeepEqual(docs.Owners, []string{"@root"}) {
		t.Fatalf("docs: %#v", m)
	}
}
