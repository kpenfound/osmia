package skills

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for _, c := range []struct{ in, url, ref, subdir, name string }{
		{"https://github.com/acme/skills", "https://github.com/acme/skills", "", "", "skills"},
		{"https://github.com/acme/skills.git@v1.2", "https://github.com/acme/skills.git", "v1.2", "", "skills"},
		{"https://github.com/acme/skills#skills/tdd/", "https://github.com/acme/skills", "", "skills/tdd", "skills-tdd"},
		{"git@github.com:acme/My_Plugin.git@main#a/b", "git@github.com:acme/My_Plugin.git", "main", "a/b", "my-plugin-b"},
	} {
		s, err := Parse(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if s.URL != c.url || s.Ref != c.ref || s.Subdir != c.subdir || s.Name != c.name {
			t.Errorf("%s: got %+v", c.in, s)
		}
	}
	for _, bad := range []string{"", "  ", "#skills", "https://github.com/acme/skills#", "https://github.com/acme/skills#a/../../b", "--upload-pack=x", "https://github.com/acme/skills@-x"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestParseRefresh(t *testing.T) {
	for _, c := range []struct {
		in     string
		always bool
		after  time.Duration
	}{{"", false, 24 * time.Hour}, {"never", false, 0}, {"always", true, 0}, {"90m", false, 90 * time.Minute}} {
		always, after, err := ParseRefresh(c.in)
		if err != nil || always != c.always || after != c.after {
			t.Errorf("%q: %v %v %v", c.in, always, after, err)
		}
	}
	for _, bad := range []string{"daily", "-1h", "24"} {
		if _, _, err := ParseRefresh(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// fakeGit stands in for the git binary: it clones by copying the fixture
// directory the URL names and records every command, so it leaves
// unverified that the real git CLI accepts the arguments Manager builds and
// reaches a remote over the network. TestPrepareClonesWithGit exercises that
// against the real git on PATH instead.
type fakeGit struct {
	mu       sync.Mutex
	fixtures map[string]string
	calls    []string
}

func (g *fakeGit) run(_ context.Context, dir string, args ...string) (string, error) {
	g.mu.Lock()
	g.calls = append(g.calls, strings.Join(args, " "))
	g.mu.Unlock()
	if args[0] != "clone" {
		return "", nil
	}
	url, dest := args[len(args)-2], args[len(args)-1]
	return "", os.CopyFS(dest, os.DirFS(g.fixtures[url]))
}

func (g *fakeGit) count(prefix string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtures lays out a single skill, a skills collection, a plugin that also
// carries hooks, MCP servers and agents, and a plugin without skills.
func fixtures(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "single", "SKILL.md"), "---\nname: single\n---\n")
	write(t, filepath.Join(dir, "single", ".git", "HEAD"), "")
	write(t, filepath.Join(dir, "coll", "skills", "a", "SKILL.md"), "")
	write(t, filepath.Join(dir, "coll", "skills", "b", "SKILL.md"), "")
	write(t, filepath.Join(dir, "coll", ".git", "HEAD"), "")
	write(t, filepath.Join(dir, "plug", ".claude-plugin", "plugin.json"), `{"name":"plug"}`)
	write(t, filepath.Join(dir, "plug", "hooks", "hooks.json"), `{"hooks":{}}`)
	write(t, filepath.Join(dir, "plug", ".mcp.json"), `{"mcpServers":{}}`)
	write(t, filepath.Join(dir, "plug", "agents", "a.md"), "")
	write(t, filepath.Join(dir, "plug", "skills", "c", "SKILL.md"), "")
	write(t, filepath.Join(dir, "plug", ".git", "HEAD"), "")
	write(t, filepath.Join(dir, "bare", ".claude-plugin", "plugin.json"), `{"name":"bare"}`)
	write(t, filepath.Join(dir, "bare", "hooks", "hooks.json"), `{"hooks":{}}`)
	write(t, filepath.Join(dir, "bare", ".git", "HEAD"), "")
	return map[string]string{
		"https://x/single": filepath.Join(dir, "single"),
		"https://x/coll":   filepath.Join(dir, "coll"),
		"https://y/coll":   filepath.Join(dir, "coll"),
		"https://x/plug":   filepath.Join(dir, "plug"),
		"https://x/bare":   filepath.Join(dir, "bare"),
	}
}

func manager(t *testing.T, git *fakeGit) *Manager {
	t.Helper()
	m := NewManager(t.TempDir())
	m.Git = git.run
	m.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return m
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Type()&os.ModeSymlink == 0 {
			rel, _ := filepath.Rel(dir, path)
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// WalkDir does not follow the skills/ symlinks, so list what they expose.
	for _, link := range []string{"skills", "skills/*"} {
		matches, _ := filepath.Glob(filepath.Join(dir, link))
		for _, m := range matches {
			if info, err := os.Lstat(m); err == nil && info.Mode()&os.ModeSymlink != 0 {
				exposed, _ := os.ReadDir(m)
				rel, _ := filepath.Rel(dir, m)
				for _, e := range exposed {
					names = append(names, filepath.ToSlash(rel)+"/"+e.Name())
				}
			}
		}
	}
	slices.Sort(names)
	return names
}

func TestPrepareExposesOnlySkills(t *testing.T) {
	for _, c := range []struct {
		ref  string
		want []string
	}{
		{"https://x/single", []string{".claude-plugin/plugin.json", "skills/single/.git", "skills/single/SKILL.md"}},
		{"https://x/coll", []string{".claude-plugin/plugin.json", "skills/a", "skills/b"}},
		{"https://x/coll#skills/b", []string{".claude-plugin/plugin.json", "skills/coll-b/SKILL.md"}},
		// A plugin's hooks, MCP servers and agents stay in the clone.
		{"https://x/plug", []string{".claude-plugin/plugin.json", "skills/c"}},
	} {
		t.Run(c.ref, func(t *testing.T) {
			m := manager(t, &fakeGit{fixtures: fixtures(t)})
			dirs, err := m.Prepare(context.Background(), []string{c.ref})
			if err != nil {
				t.Fatal(err)
			}
			if len(dirs) != 1 || !strings.HasPrefix(dirs[0], filepath.Join(m.Dir, "plugins")+string(filepath.Separator)) {
				t.Fatalf("dirs %v", dirs)
			}
			if got := entries(t, dirs[0]); !slices.Equal(got, c.want) {
				t.Fatalf("plugin holds %v, want %v", got, c.want)
			}
		})
	}
}

func TestPrepareRefusesWhatIsNotASkill(t *testing.T) {
	for _, ref := range []string{"https://x/bare", "https://x/coll#skills/missing", "https://x/coll#.git"} {
		m := manager(t, &fakeGit{fixtures: fixtures(t)})
		if dirs, err := m.Prepare(context.Background(), []string{ref}); err == nil {
			t.Errorf("%s prepared %v", ref, dirs)
		}
	}
}

func TestPrepareRefusesASubdirectoryLeavingTheClone(t *testing.T) {
	fx := fixtures(t)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "SKILL.md"), "")
	if err := os.Symlink(outside, filepath.Join(fx["https://x/coll"], "out")); err != nil {
		t.Fatal(err)
	}
	m := manager(t, &fakeGit{fixtures: fx})
	if dirs, err := m.Prepare(context.Background(), []string{"https://x/coll#out"}); err == nil {
		t.Fatalf("prepared %v", dirs)
	}
}

func TestReferencesWithOneNameKeepSeparateWrappers(t *testing.T) {
	m := manager(t, &fakeGit{fixtures: fixtures(t)})
	dirs, err := m.Prepare(context.Background(), []string{"https://x/coll", "https://y/coll"})
	if err != nil {
		t.Fatal(err)
	}
	if dirs[0] == dirs[1] {
		t.Fatalf("both references share %s", dirs[0])
	}
}

func TestRefreshPolicy(t *testing.T) {
	for _, c := range []struct {
		policy string
		pulls  int
	}{{"never", 0}, {"always", 2}, {"1h", 1}, {"", 0}, {"invalid", 0}} {
		t.Run(c.policy, func(t *testing.T) {
			git := &fakeGit{fixtures: fixtures(t)}
			m := manager(t, git)
			now := time.Now()
			m.Now = func() time.Time { return now }
			m.Refresh = func() string { return c.policy }
			prepare := func() {
				if _, err := m.Prepare(context.Background(), []string{"https://x/single"}); err != nil {
					t.Fatal(err)
				}
			}
			prepare()
			prepare()
			now = now.Add(90 * time.Minute)
			prepare()
			if git.count("clone") != 1 || git.count("pull") != c.pulls {
				t.Fatalf("calls %v", git.calls)
			}
		})
	}
}

func TestFailedRefreshKeepsTheClone(t *testing.T) {
	git := &fakeGit{fixtures: fixtures(t)}
	m := manager(t, git)
	m.Refresh = func() string { return "always" }
	if _, err := m.Prepare(context.Background(), []string{"https://x/single@v1"}); err != nil {
		t.Fatal(err)
	}
	m.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
		return "", exec.ErrNotFound
	}
	if _, err := m.Prepare(context.Background(), []string{"https://x/single@v1"}); err != nil {
		t.Fatalf("failed pull stopped the turn: %v", err)
	}
}

func TestPrepareKeepsAWrapperInUse(t *testing.T) {
	m := manager(t, &fakeGit{fixtures: fixtures(t)})
	dirs, err := m.Prepare(context.Background(), []string{"https://x/single"})
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dirs[0], "marker")
	write(t, marker, "")
	if _, err := m.Prepare(context.Background(), []string{"https://x/single"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("an unchanged wrapper was rebuilt")
	}
}

func TestConcurrentPrepareClonesOnce(t *testing.T) {
	git := &fakeGit{fixtures: fixtures(t)}
	m := manager(t, git)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Prepare(context.Background(), []string{"https://x/coll", "https://x/single"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if git.count("clone") != 2 {
		t.Fatalf("calls %v", git.calls)
	}
}

// A clone of a local repository through the git on PATH, pinned to a tag.
func TestPrepareClonesWithGit(t *testing.T) {
	repo := t.TempDir()
	write(t, filepath.Join(repo, "skills", "tdd", "SKILL.md"), "---\nname: tdd\n---\n")
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "skills"}, {"tag", "v1"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	m := NewManager(t.TempDir())
	m.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	m.Refresh = func() string { return "always" }
	ref := "file://" + filepath.ToSlash(repo) + "@v1#skills/tdd"
	spec, err := Parse(ref)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		dirs, err := m.Prepare(context.Background(), []string{ref})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dirs[0], "skills", spec.Name, "SKILL.md")); err != nil {
			t.Fatal(err)
		}
	}
}
