//go:build unix

package isolation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	a "github.com/kpenfound/osmia/internal/coreadapter"
)

const (
	tddRef    = "https://github.com/acme/skills#skills/tdd"
	reviewRef = "https://github.com/acme/skills#skills/review"
)

type skillCache struct {
	dir, outside string
	tdd, review  string
}

func newSkillCache(t *testing.T) skillCache {
	t.Helper()
	dir, outside := t.TempDir(), t.TempDir()
	put(t, dir, "repos/skills/skills/tdd/SKILL.md", "tdd skill, see references/guide.md")
	put(t, dir, "repos/skills/skills/tdd/references/guide.md", "tdd guide")
	put(t, dir, "repos/skills/skills/tdd/.git/config", "[core]")
	put(t, dir, "repos/skills/skills/review/SKILL.md", "review skill")
	put(t, dir, "repos/skills/skills/other/SKILL.md", "a skill the turn was not given")
	put(t, dir, "plugins/skills-tdd/.claude-plugin/plugin.json", "{}")
	put(t, outside, "secret.txt", "secret")
	put(t, outside, "dir/secret.txt", "secret")
	link := func(target, name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	link(filepath.Join(dir, "repos/skills/skills/tdd"), "plugins/skills-tdd/skills")
	link(filepath.Join(dir, "repos/skills/skills/review"), "plugins/skills-review/skills/review")
	link(filepath.Join(dir, "repos/skills/skills/other"), "plugins/skills-other/skills/other")
	link(filepath.Join(outside, "secret.txt"), "repos/skills/skills/tdd/escape.md")
	link(filepath.Join(outside, "dir"), "repos/skills/skills/tdd/references/escape")
	link("references/guide.md", "repos/skills/skills/tdd/inside.md")
	link("../review/SKILL.md", "repos/skills/skills/tdd/sibling.md")
	return skillCache{dir: dir, outside: outside, tdd: filepath.Join(dir, "plugins/skills-tdd"), review: filepath.Join(dir, "plugins/skills-review")}
}

func readInTurn(t *testing.T, cache skillCache, refs []string, paths []string) map[string]string {
	t.Helper()
	r, _, h, engine, input := fixture(t, "reviewer", "none")
	engine.Skills = []string{cache.dir}
	engine.SkillPlugins = map[string]string{tddRef: cache.tdd, reviewRef: cache.review, "https://github.com/acme/skills#skills/other": filepath.Join(cache.dir, "plugins/skills-other")}
	r.Select = func(context.Context, a.Scope) (Selection, error) {
		return Selection{Paths: []string{"src"}, Execution: a.ExecutionSettings{Mode: "none", Skills: refs}}, nil
	}
	out := map[string]string{}
	engine.OnRun = func() error {
		read := h.requests[0].Tools[0]
		if read.Name != "file_read" {
			t.Fatalf("tool %s", read.Name)
		}
		for _, path := range paths {
			raw, _ := json.Marshal(map[string]string{"path": path})
			result, err := read.Handle(context.Background(), raw)
			if err != nil {
				out[path] = "error: " + err.Error()
				continue
			}
			var text string
			if err := json.Unmarshal(result, &text); err != nil {
				t.Fatal(err)
			}
			out[path] = text
		}
		return nil
	}
	if _, err := r.Run(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFileReadReadsTheTurnsSkillDirectories(t *testing.T) {
	cache := newSkillCache(t)
	paths := []string{
		filepath.Join(cache.tdd, "skills/SKILL.md"),
		filepath.Join(cache.tdd, "skills/references/guide.md"),
		filepath.Join(cache.tdd, "skills/references"),
		filepath.Join(cache.tdd, "skills/inside.md"),
		filepath.Join(cache.review, "skills/review/SKILL.md"),
		filepath.Join(cache.review, "skills"),
		filepath.Join(cache.dir, "repos/skills/skills/tdd/SKILL.md"),
		"src/file",
	}
	got := readInTurn(t, cache, []string{tddRef, reviewRef}, paths)
	want := []string{"tdd skill, see references/guide.md", "tdd guide", "guide.md", "tdd guide", "review skill", "review/", "tdd skill, see references/guide.md", "original"}
	for i, path := range paths {
		if got[path] != want[i] {
			t.Errorf("%s: got %q, want %q", path, got[path], want[i])
		}
	}
}

func TestFileReadRefusesWhatIsNotTheTurnsSkills(t *testing.T) {
	cache := newSkillCache(t)
	refused := map[string]string{
		"traversal out of a skill directory":    filepath.Join(cache.tdd, "skills/../../../../../../etc/passwd"),
		"traversal into a sibling skill":        filepath.Join(cache.tdd, "skills/../../skills-other/skills/other/SKILL.md"),
		"traversal through the skill":           cache.tdd + "/skills/references/../../../skills/other/SKILL.md",
		"symlink to a file outside":             filepath.Join(cache.tdd, "skills/escape.md"),
		"symlink to a directory outside":        filepath.Join(cache.tdd, "skills/references/escape/secret.txt"),
		"symlink to a sibling skill":            filepath.Join(cache.tdd, "skills/sibling.md"),
		"plugin metadata":                       filepath.Join(cache.tdd, ".claude-plugin/plugin.json"),
		"version control metadata":              filepath.Join(cache.tdd, "skills/.git/config"),
		"a skill the turn was not given":        filepath.Join(cache.dir, "plugins/skills-other/skills/other/SKILL.md"),
		"a not given skill where it resolves":   filepath.Join(cache.dir, "repos/skills/skills/other/SKILL.md"),
		"a clone outside the given directories": filepath.Join(cache.dir, "repos/skills/skills"),
		"the cache":                             cache.dir,
		"another absolute path":                 "/etc/passwd",
		"a file outside the cache":              filepath.Join(cache.outside, "secret.txt"),
	}
	paths := make([]string, 0, len(refused))
	for _, path := range refused {
		paths = append(paths, path)
	}
	got := readInTurn(t, cache, []string{tddRef, reviewRef}, paths)
	for name, path := range refused {
		if !strings.HasPrefix(got[path], "error: ") {
			t.Errorf("%s: %s read as %q", name, path, got[path])
		}
	}
}

func TestFileReadOnlyReadsSkillsTheTurnWasGiven(t *testing.T) {
	cache := newSkillCache(t)
	got := readInTurn(t, cache, []string{reviewRef}, []string{
		filepath.Join(cache.review, "skills/review/SKILL.md"),
		filepath.Join(cache.tdd, "skills/SKILL.md"),
		filepath.Join(cache.dir, "repos/skills/skills/tdd/SKILL.md"),
	})
	if got[filepath.Join(cache.review, "skills/review/SKILL.md")] != "review skill" {
		t.Fatalf("given skill %q", got[filepath.Join(cache.review, "skills/review/SKILL.md")])
	}
	for _, path := range []string{filepath.Join(cache.tdd, "skills/SKILL.md"), filepath.Join(cache.dir, "repos/skills/skills/tdd/SKILL.md")} {
		if !strings.HasPrefix(got[path], "error: ") {
			t.Errorf("%s read as %q", path, got[path])
		}
	}
}

func TestFileReadWithoutSkillsReadsOnlyTheView(t *testing.T) {
	cache := newSkillCache(t)
	paths := []string{filepath.Join(cache.tdd, "skills/SKILL.md"), filepath.Join(cache.dir, "repos/skills/skills/tdd/SKILL.md"), "/etc/passwd", "src/file"}
	got := readInTurn(t, cache, nil, paths)
	for _, path := range paths[:3] {
		if got[path] != "error: file view requires a relative path without traversal" {
			t.Errorf("%s: %q", path, got[path])
		}
	}
	if got["src/file"] != "original" {
		t.Fatalf("view file %q", got["src/file"])
	}
}

func TestFileReadKeepsTheViewsOwnRefusalsOnATurnWithSkills(t *testing.T) {
	cache := newSkillCache(t)
	got := readInTurn(t, cache, []string{tddRef}, []string{"../outside", "src/../../outside", ".git", "src/file"})
	for _, path := range []string{"../outside", "src/../../outside", ".git"} {
		if !strings.HasPrefix(got[path], "error: ") {
			t.Errorf("%s read as %q", path, got[path])
		}
	}
	if got["src/file"] != "original" {
		t.Fatalf("view file %q", got["src/file"])
	}
}

func TestSkillReadsAddNoGrant(t *testing.T) {
	cache := newSkillCache(t)
	r, _, h, engine, input := fixture(t, "reviewer", "none")
	engine.Skills = []string{cache.dir}
	engine.SkillPlugins = map[string]string{tddRef: cache.tdd}
	r.Select = func(context.Context, a.Scope) (Selection, error) {
		return Selection{Paths: []string{"src"}, Execution: a.ExecutionSettings{Mode: "none", Skills: []string{tddRef}}}, nil
	}
	if _, err := r.Run(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	grants := engine.Requests[0].Grants
	if len(h.requests[0].Tools) != 1 || h.requests[0].Tools[0].Name != "file_read" {
		t.Fatalf("tools %+v grants %+v", h.requests[0].Tools, grants)
	}
	for _, tool := range []string{"Bash", "Write", "Edit"} {
		if slices.Contains(grants.Tools, tool) {
			t.Fatalf("read-only turn granted %s: %v", tool, grants.Tools)
		}
	}
	mounted := false
	for _, mount := range grants.Mounts {
		if mount.Path == cache.dir {
			mounted = mount.Access == agent.ReadOnly
		}
	}
	if !mounted {
		t.Fatalf("skill cache not mounted read-only: %+v", grants.Mounts)
	}
}
