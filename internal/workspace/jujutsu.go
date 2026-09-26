package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/busybees/core/vcs"
)

// Jujutsu is the Jujutsu provider of one clone. Its repository lives under
// Directory and keeps its commits in the clone's Git store, so every commit it
// makes is a commit of the clone that plain Git reads, while the clone's
// checkout gets no .jj and its format does not change. Every workspace is a
// Jujutsu workspace under Directory, named by the request, on the branch the
// request names.
//
// Branches are the clone's Git branches. The provider reads and moves them in
// the clone, each move a compare-and-swap, has Jujutsu import them, and names
// commits to Jujutsu by their IDs, never by branch. Operations on recorded
// commits rather than on a workspace, which are reading history, Squash,
// Rebase, RebaseFrom, Replay, Export and the remotes, run on the same Git
// store exactly as Git runs them, so both providers make the same commits
// from the same arguments. The exception is a Rebase or RebaseFrom that
// conflicts: Jujutsu makes that commit, holding its conflicts stored rather
// than as markers, and a workspace on it materializes them.
type Jujutsu struct {
	Clone     string
	Directory string
	// JJ is the jj executable; empty runs jj from PATH.
	JJ string
}

var _ Provider = (*Jujutsu)(nil)

// MinimumJJ is the oldest jj release the Jujutsu provider supports.
const MinimumJJ = "0.45.0"

var (
	// ErrJJMissing is a jj that cannot be found or run.
	ErrJJMissing = errors.New("jj is missing")
	// ErrJJTooOld is a jj older than MinimumJJ.
	ErrJJTooOld = errors.New("jj is too old")
)

// CheckJJ runs the jj executable, or jj from PATH when executable is empty,
// and returns the version it reports. A jj that cannot be found or run
// returns an error wrapping ErrJJMissing, and one older than MinimumJJ an
// error wrapping ErrJJTooOld that names the version found.
func CheckJJ(ctx context.Context, executable string) (string, error) {
	if executable == "" {
		executable = "jj"
	}
	path, err := exec.LookPath(executable)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrJJMissing, err)
	}
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = []string{"JJ_CONFIG=" + os.DevNull}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %s --version: %v", ErrJJMissing, path, err)
	}
	reported := strings.TrimSpace(string(out))
	version, ok := strings.CutPrefix(reported, "jj ")
	found, parsed := parseVersion(version)
	if !ok || !parsed {
		return "", fmt.Errorf("%s reports version %q, which is not a jj release", path, reported)
	}
	minimum, _ := parseVersion(MinimumJJ)
	if slices.Compare(found, minimum) < 0 {
		return version, fmt.Errorf("%w: found jj %s, Osmia needs %s or later", ErrJJTooOld, version, MinimumJJ)
	}
	return version, nil
}

// parseVersion reads the major, minor and patch numbers of a release such as
// 0.45.1 or 0.45.1-7c41cdeb.
func parseVersion(version string) ([]int, bool) {
	release, _, _ := strings.Cut(version, "-")
	release, _, _ = strings.Cut(release, "+")
	fields := strings.Split(release, ".")
	if len(fields) != 3 {
		return nil, false
	}
	numbers := make([]int, 3)
	for i, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil || n < 0 {
			return nil, false
		}
		numbers[i] = n
	}
	return numbers, true
}

// Check reports whether the provider's jj is supported, as CheckJJ does.
func (j *Jujutsu) Check(ctx context.Context) (string, error) { return CheckJJ(ctx, j.JJ) }

// repositoryName names the directory under Directory that holds the Jujutsu
// repository. A leading dot keeps it apart from every workspace name.
const repositoryName = ".jujutsu"

// metadataName names the file in a workspace's .jj directory that records the
// branch the workspace is on and the replay in progress there.
const metadataName = "osmia.json"

// jjSettings are the settings every jj command runs with; the owner's own jj
// configuration does not apply. Commits carry no change-id header but those
// Carry makes, so any other commit holds what the same commit made by Git
// holds; conflicts are written
// into files with Git's markers; every file is tracked whatever its size; and
// importing a branch that moved abandons nothing a workspace may stand on.
var jjSettings = []string{
	"user.name=Osmia",
	"user.email=osmia@localhost",
	"git.write-change-id-header=false",
	"git.abandon-unreachable-commits=false",
	"ui.conflict-marker-style=git",
	"snapshot.auto-track=all()",
	"snapshot.max-new-file-size=1099511627776",
}

// stamp returns the settings that commit what a command rewrites at the given
// time, as Git's replay does.
func stamp(at time.Time) []string {
	return []string{"debug.commit-timestamp=" + strconv.Quote(at.UTC().Format(time.RFC3339))}
}

// metadata is what the provider records about one of its workspaces: its
// branch, the change ID of its first working-copy commit, and the replay in
// progress there.
type metadata struct {
	Branch string  `json:"branch"`
	Change string  `json:"change,omitempty"`
	Replay *replay `json:"replay,omitempty"`
}

// replay is a replay in progress in a workspace: the commits of its branch,
// from Head back to where it meets Onto, each copied onto the copy of the one
// before it, the first onto Onto. A copy is named by its change ID, which
// stays the same while the copy is resolved and its descendants rebased.
type replay struct {
	Onto  string `json:"onto"`
	Head  string `json:"head"`
	Steps []step `json:"steps"`
}

type step struct {
	Original string `json:"original"`
	Change   string `json:"change"`
	// Empty is an original that changes nothing, which is kept.
	Empty bool `json:"empty,omitempty"`
}

func (j *Jujutsu) git() *Git               { return &Git{Clone: j.Clone, Directory: j.Directory} }
func (j *Jujutsu) repository() string      { return filepath.Join(j.Directory, repositoryName) }
func (j *Jujutsu) path(name string) string { return filepath.Join(j.Directory, name) }

func (j *Jujutsu) worktree(name, branch string) Worktree {
	return Worktree{Path: j.path(name), Branch: branch, mounts: []string{j.repository(), filepath.Join(j.Clone, ".git")}}
}

// initialize makes the repository when Directory has none: a Jujutsu
// repository on the clone's Git store with no workspace of its own. It is
// made aside and renamed into place, so an interrupted one is made again.
func (j *Jujutsu) initialize(ctx context.Context) error {
	if exists, err := j.initialized(); err != nil || exists {
		return err
	}
	clone, err := filepath.Abs(j.Clone)
	if err != nil {
		return err
	}
	staging := j.repository() + ".new"
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0700); err != nil {
		return err
	}
	if _, err := j.run(ctx, "", nil, "git", "init", "--git-repo", clone, staging); err != nil {
		return err
	}
	if _, err := j.run(ctx, staging, nil, "--ignore-working-copy", "workspace", "forget", "default"); err != nil {
		return err
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != ".jj" {
			if err := os.RemoveAll(filepath.Join(staging, entry.Name())); err != nil {
				return err
			}
		}
	}
	return os.Rename(staging, j.repository())
}

func (j *Jujutsu) initialized() (bool, error) {
	_, err := os.Stat(filepath.Join(j.repository(), ".jj", "repo"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// importBranches has the repository take in the clone's branches as they are.
func (j *Jujutsu) importBranches(ctx context.Context) error {
	_, err := j.run(ctx, j.repository(), nil, "--ignore-working-copy", "git", "import")
	return err
}

// moveBranch points the clone's branch at commit to, only from commit from,
// and has the repository import it.
func (j *Jujutsu) moveBranch(ctx context.Context, branch, to, from string) error {
	if _, err := j.git().runEnv(ctx, identity, "update-ref", "-m", "osmia: jujutsu", "refs/heads/"+branch, to, from); err != nil {
		return err
	}
	return j.importBranches(ctx)
}

// registered returns the names of the repository's workspaces.
func (j *Jujutsu) registered(ctx context.Context) ([]string, error) {
	out, err := j.run(ctx, j.repository(), nil, "--ignore-working-copy", "workspace", "list", "-T", `name ++ "\0"`)
	if err != nil {
		return nil, err
	}
	return splitNull(out), nil
}

// forget removes a workspace's directory, then the repository's record of it.
func (j *Jujutsu) forget(ctx context.Context, name string) error {
	if err := os.RemoveAll(j.path(name)); err != nil {
		return err
	}
	_, err := j.run(ctx, j.repository(), nil, "--ignore-working-copy", "workspace", "forget", name)
	return err
}

// Workspace returns the workspace named name when the repository has one. One
// whose directory is gone, or whose making stopped before its branch was
// recorded, is forgotten first, so it can be made again.
func (j *Jujutsu) Workspace(ctx context.Context, name string) (Worktree, bool, error) {
	if exists, err := j.initialized(); err != nil || !exists {
		return Worktree{}, false, err
	}
	names, err := j.registered(ctx)
	if err != nil || !slices.Contains(names, name) {
		return Worktree{}, false, err
	}
	meta, err := readMetadata(j.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return Worktree{}, false, j.forget(ctx, name)
	}
	if err != nil {
		return Worktree{}, false, err
	}
	return j.worktree(name, meta.Branch), true, nil
}

// Acquire returns the workspace named by the request on its branch, creating
// what is missing: the repository, the branch from the request's ref when the
// clone has no such branch, and the workspace on the branch's commit when the
// repository has no such workspace. A workspace already there is returned as
// it is when it is on the branch, so a repeated request creates nothing
// twice, and one whose making stopped before the repository registered it is
// removed and made again. The request needs a branch; a directory in the way
// that is no workspace of the repository is left alone and reported.
func (j *Jujutsu) Acquire(ctx context.Context, req vcs.Request) (vcs.Workspace, error) {
	if req.Name == "" || req.Branch == "" {
		return nil, errors.New("a workspace request needs a name and a branch")
	}
	if err := j.initialize(ctx); err != nil {
		return nil, err
	}
	existing, found, err := j.Workspace(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if found {
		if existing.Branch != req.Branch {
			return nil, fmt.Errorf("workspace %s is on branch %q, not %q", existing.Path, existing.Branch, req.Branch)
		}
		return existing, nil
	}
	path := j.path(req.Name)
	if j.unregistered(path) {
		if err := os.RemoveAll(path); err != nil {
			return nil, err
		}
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("%s exists and is no workspace of the clone; move it away", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	g := j.git()
	tip, exists, err := g.Branch(ctx, req.Branch)
	if err != nil {
		return nil, err
	}
	if !exists {
		if req.Ref == "" {
			return nil, fmt.Errorf("branch %s does not exist and the request names no ref to create it from", req.Branch)
		}
		if tip, err = g.run(ctx, "rev-parse", "--verify", "--end-of-options", req.Ref+"^{commit}"); err != nil {
			return nil, err
		}
		if _, err := g.run(ctx, "check-ref-format", "--branch", req.Branch); err != nil {
			return nil, err
		}
		if err := j.moveBranch(ctx, req.Branch, tip, ""); err != nil {
			return nil, err
		}
	} else if err := j.importBranches(ctx); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if _, err := j.run(ctx, j.repository(), nil, "workspace", "add", "--name", req.Name, "--revision", tip, path); err != nil {
		return nil, err
	}
	change, err := j.run(ctx, path, nil, "--ignore-working-copy", "log", "--no-graph", "--revisions", "@", "--template", "change_id")
	if err != nil {
		return nil, err
	}
	if err := writeMetadata(path, metadata{Branch: req.Branch, Change: change}); err != nil {
		return nil, err
	}
	return j.worktree(req.Name, req.Branch), nil
}

// unregistered reports whether the directory at path is a workspace of the
// repository that the repository has no record of: what a workspace's
// making left when it stopped before registering the workspace.
func (j *Jujutsu) unregistered(path string) bool {
	target, err := os.ReadFile(filepath.Join(path, ".jj", "repo"))
	if err != nil {
		return false
	}
	repo := strings.TrimSpace(string(target))
	if !filepath.IsAbs(repo) {
		repo = filepath.Join(path, ".jj", repo)
	}
	return sameFile(repo, filepath.Join(j.repository(), ".jj", "repo"))
}

// sameFile reports whether two paths name the same file once their symbolic
// links are resolved.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// Release removes the workspace, whatever it holds; the branch stays.
func (j *Jujutsu) Release(ctx context.Context, w vcs.Workspace) error {
	if w == nil || w.Directory() == "" {
		return errors.New("no workspace to release")
	}
	name, err := filepath.Rel(j.Directory, w.Directory())
	if err != nil {
		return err
	}
	name = filepath.ToSlash(name)
	names, err := j.registered(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(names, name) {
		return fmt.Errorf("%s is no workspace of the clone", w.Directory())
	}
	return j.forget(ctx, name)
}

// Prune forgets the workspaces whose directories are gone. The repository
// is the provider's own, so every workspace it has is.
func (j *Jujutsu) Prune(ctx context.Context) error {
	if exists, err := j.initialized(); err != nil || !exists {
		return err
	}
	names, err := j.registered(ctx)
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(j.path(name), ".jj")); errors.Is(err, os.ErrNotExist) {
			if err := j.forget(ctx, name); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return nil
}

// revision is what the provider reads of one commit in the repository.
type revision struct {
	commit      string
	parents     []string
	empty       bool
	description string
}

// describe reads one revision, in the workspace or repository at dir. A
// snapshot first records the files of the workspace in its working-copy
// commit.
func (j *Jujutsu) describe(ctx context.Context, dir string, snapshot bool, rev string) (revision, error) {
	args := []string{"log", "--no-graph", "--revisions", rev, "--template", `commit_id ++ "\n" ++ parents.map(|p| p.commit_id()).join(" ") ++ "\n" ++ if(empty, "empty", "changed") ++ "\n" ++ description`}
	if !snapshot {
		args = append([]string{"--ignore-working-copy"}, args...)
	}
	out, _, err := j.runOutput(ctx, dir, nil, args...)
	if err != nil {
		return revision{}, err
	}
	fields := strings.SplitN(out, "\n", 4)
	if len(fields) != 4 {
		return revision{}, fmt.Errorf("jj log of %s printed %q", rev, out)
	}
	return revision{commit: fields[0], parents: strings.Fields(fields[1]), empty: fields[2] == "empty", description: fields[3]}, nil
}

// head returns the commit the workspace's branch points at.
func (j *Jujutsu) head(ctx context.Context, w Worktree) (string, error) {
	head, found, err := j.git().Branch(ctx, w.Branch)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("the branch %s of workspace %s does not exist", w.Branch, w.Path)
	}
	return head, nil
}

// Snapshot commits the workspace's full tree, untracked files included and
// files the repository ignores left out, on top of the commit its branch
// points at, moves the branch to it and returns it. The owner's own excludes
// file does not apply, so what the snapshot holds depends on the repository
// alone. A workspace whose tree is the one of its commit is not committed
// again: its commit is the snapshot. The workspace must descend from base,
// which is checked before anything is committed. A snapshot committed before
// an interruption kept its branch from moving is completed, with what the
// workspace gained since.
func (j *Jujutsu) Snapshot(ctx context.Context, w Worktree, base string) (string, error) {
	if w.Path == "" {
		return "", errors.New("no workspace to snapshot")
	}
	head, err := j.head(ctx, w)
	if err != nil {
		return "", err
	}
	descends, err := j.git().Ancestor(ctx, base, head)
	if err != nil {
		return "", err
	}
	if !descends {
		return "", fmt.Errorf("workspace %s is at %s, which does not descend from %s", w.Path, head, base)
	}
	if err := j.importBranches(ctx); err != nil {
		return "", err
	}
	current, err := j.describe(ctx, w.Path, true, "@")
	if err != nil {
		return "", err
	}
	message := "Snapshot of " + w.Branch
	if slices.Equal(current.parents, []string{head}) {
		if current.empty {
			return head, nil
		}
		if _, err := j.run(ctx, w.Path, nil, "commit", "--message", message); err != nil {
			return "", err
		}
	} else {
		parent, err := j.describe(ctx, w.Path, false, "@-")
		if err != nil {
			return "", err
		}
		if len(current.parents) != 1 || !slices.Equal(parent.parents, []string{head}) || strings.TrimSuffix(parent.description, "\n") != message {
			return "", fmt.Errorf("workspace %s is not on its branch %s at %s", w.Path, w.Branch, head)
		}
		if !current.empty {
			if _, err := j.run(ctx, w.Path, nil, "squash", "--use-destination-message"); err != nil {
				return "", err
			}
		}
	}
	snapshot, err := j.describe(ctx, w.Path, false, "@-")
	if err != nil {
		return "", err
	}
	return snapshot.commit, j.moveBranch(ctx, w.Branch, snapshot.commit, head)
}

// checkout puts the workspace on a new working-copy commit on commit, with
// commit's files; what the workspace held besides is dropped. Files the
// repository ignores stay unless commit tracks them. A workspace already
// there with nothing of its own is left as it is.
func (j *Jujutsu) checkout(ctx context.Context, w Worktree, commit string) error {
	current, err := j.describe(ctx, w.Path, true, "@")
	if err != nil {
		return err
	}
	if slices.Equal(current.parents, []string{commit}) && current.empty {
		return nil
	}
	if _, err := j.run(ctx, w.Path, nil, "new", commit); err != nil {
		return err
	}
	if current.empty {
		return nil
	}
	_, err = j.run(ctx, w.Path, nil, "abandon", current.commit+" ~ ::(@ | bookmarks() | remote_bookmarks())")
	return err
}

// Move points the workspace's branch at commit to, from commit from, and
// makes its files those of to. A workspace whose branch is already at to has
// its files made those of to again, which completes a move interrupted
// between the two; one at any other commit is refused. Files the repository
// ignores stay unless to tracks them.
func (j *Jujutsu) Move(ctx context.Context, w Worktree, from, to string) error {
	head, err := j.head(ctx, w)
	if err != nil {
		return err
	}
	if head != to {
		if head != from {
			return fmt.Errorf("workspace %s is at %s, not %s", w.Path, head, from)
		}
		if err := j.moveBranch(ctx, w.Branch, to, from); err != nil {
			return err
		}
	} else if err := j.importBranches(ctx); err != nil {
		return err
	}
	return j.checkout(ctx, w, to)
}

// Advance fast-forwards the workspace's branch, with its files, from commit
// from to commit to, which must descend from it; what the workspace holds of
// its own stays on top. A workspace whose branch is already at to has its
// files brought onto to, which completes an advance interrupted between the
// two; one at any other commit is refused.
func (j *Jujutsu) Advance(ctx context.Context, w Worktree, from, to string) error {
	head, err := j.head(ctx, w)
	if err != nil {
		return err
	}
	if head != to {
		if head != from {
			return fmt.Errorf("workspace %s is at %s, not %s", w.Path, head, from)
		}
		descends, err := j.git().Ancestor(ctx, from, to)
		if err != nil {
			return err
		}
		if !descends {
			return fmt.Errorf("workspace %s cannot fast-forward from %s to %s", w.Path, from, to)
		}
		if err := j.moveBranch(ctx, w.Branch, to, from); err != nil {
			return err
		}
	} else if err := j.importBranches(ctx); err != nil {
		return err
	}
	current, err := j.describe(ctx, w.Path, true, "@")
	if err != nil || slices.Equal(current.parents, []string{to}) {
		return err
	}
	_, err = j.run(ctx, w.Path, nil, "rebase", "--revisions", "@", "--onto", to)
	return err
}

// ReplayIn replays the workspace's branch onto onto in the workspace itself,
// as Replay does, and returns the commit its branch then points at. Each
// commit is copied onto the copy of the one before it, keeping its message
// and author, committed by Osmia at the given time. A replay that conflicts
// stops at the first copy that does, with the workspace on that copy and the
// conflicted paths, sorted, holding conflict markers there, and returns those
// paths; ContinueReplay goes on once they are resolved. The branch moves
// when the replay ends. A workspace that already descends from onto is left
// as it is, and one with changes its branch does not hold is refused.
func (j *Jujutsu) ReplayIn(ctx context.Context, w Worktree, onto string, at time.Time) (string, []string, error) {
	meta, err := readMetadata(w.Path)
	if err != nil {
		return "", nil, err
	}
	if meta.Replay != nil {
		return "", nil, fmt.Errorf("workspace %s is replaying already", w.Path)
	}
	head, err := j.head(ctx, w)
	if err != nil {
		return "", nil, err
	}
	g := j.git()
	if descends, err := g.Ancestor(ctx, onto, head); err != nil || descends {
		return head, nil, err
	}
	if err := j.importBranches(ctx); err != nil {
		return "", nil, err
	}
	current, err := j.describe(ctx, w.Path, true, "@")
	if err != nil {
		return "", nil, err
	}
	if !slices.Equal(current.parents, []string{head}) || !current.empty {
		return "", nil, fmt.Errorf("workspace %s holds changes its branch %s does not; snapshot it before replaying", w.Path, w.Branch)
	}
	out, err := g.run(ctx, "rev-list", "--reverse", "--no-merges", onto+".."+head, "--")
	if err != nil {
		return "", nil, err
	}
	r := &replay{Onto: onto, Head: head}
	parent := onto
	for _, original := range strings.Fields(out) {
		described, err := j.describe(ctx, j.repository(), false, original)
		if err != nil {
			return "", nil, err
		}
		change, err := j.duplicate(ctx, original, parent, at)
		if err != nil {
			return "", nil, err
		}
		r.Steps = append(r.Steps, step{Original: original, Change: change, Empty: described.empty})
		parent = change
	}
	meta.Replay = r
	if err := writeMetadata(w.Path, meta); err != nil {
		return "", nil, err
	}
	return j.proceed(ctx, w, meta, at)
}

// duplicate copies commit original onto the commit parent names and returns
// the copy's change ID. Nothing else moves.
func (j *Jujutsu) duplicate(ctx context.Context, original, parent string, at time.Time) (string, error) {
	settings := append(stamp(at), "templates.commit_summary=change_id")
	_, stderr, err := j.runOutput(ctx, j.repository(), settings, "--ignore-working-copy", "duplicate", original, "--onto", parent)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "Duplicated ") {
			if fields := strings.Fields(line); len(fields) == 4 && fields[2] == "as" {
				return fields[3], nil
			}
		}
	}
	return "", fmt.Errorf("jj duplicate of %s named no copy: %s", original, strings.TrimSpace(stderr))
}

// stepState is where one copy of a replay stands.
type stepState struct {
	commit          string
	conflict, empty bool
}

// steps reads the replay's copies that are still there, by change ID.
func (j *Jujutsu) steps(ctx context.Context, r *replay) (map[string]stepState, error) {
	states := map[string]stepState{}
	if len(r.Steps) == 0 {
		return states, nil
	}
	var revsets []string
	for _, s := range r.Steps {
		revsets = append(revsets, "present("+s.Change+")")
	}
	out, err := j.run(ctx, j.repository(), nil, "--ignore-working-copy", "log", "--no-graph", "--revisions", strings.Join(revsets, " | "), "--template", `change_id ++ " " ++ commit_id ++ " " ++ if(conflict, "conflict", "clean") ++ " " ++ if(empty, "empty", "changed") ++ "\n"`)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) == 4 {
			states[fields[0]] = stepState{commit: fields[1], conflict: fields[2] == "conflict", empty: fields[3] == "empty"}
		}
	}
	return states, nil
}

// conflicts returns the paths a revision holds conflicted, sorted.
func (j *Jujutsu) conflicts(ctx context.Context, rev string) ([]string, error) {
	out, err := j.run(ctx, j.repository(), nil, "--ignore-working-copy", "file", "list", "--revision", rev, "--template", `if(conflict, path ++ "\0")`)
	if err != nil {
		return nil, err
	}
	paths := splitNull(out)
	slices.Sort(paths)
	return paths, nil
}

// proceed takes the replay in the workspace as far as it goes: a copy that
// is empty though its original was not is dropped, the first copy that
// conflicts is where it stops, with the workspace on it, and with none left
// the branch moves to the last copy and the workspace onto it.
func (j *Jujutsu) proceed(ctx context.Context, w Worktree, meta metadata, at time.Time) (string, []string, error) {
	r := meta.Replay
	states, err := j.steps(ctx, r)
	if err != nil {
		return "", nil, err
	}
	for _, s := range r.Steps {
		state, ok := states[s.Change]
		if !ok {
			continue
		}
		if state.conflict {
			current, err := j.describe(ctx, w.Path, false, "@")
			if err != nil {
				return "", nil, err
			}
			if current.commit != state.commit {
				if _, err := j.run(ctx, w.Path, stamp(at), "edit", s.Change); err != nil {
					return "", nil, err
				}
			}
			conflicts, err := j.conflicts(ctx, s.Change)
			return "", conflicts, err
		}
		if state.empty && !s.Empty {
			if _, err := j.run(ctx, w.Path, stamp(at), "abandon", s.Change); err != nil {
				return "", nil, err
			}
		}
	}
	if states, err = j.steps(ctx, r); err != nil {
		return "", nil, err
	}
	tip := r.Onto
	for _, s := range r.Steps {
		if state, ok := states[s.Change]; ok {
			tip = state.commit
		}
	}
	head, err := j.head(ctx, w)
	if err != nil {
		return "", nil, err
	}
	if head != tip {
		if err := j.moveBranch(ctx, w.Branch, tip, r.Head); err != nil {
			return "", nil, err
		}
	}
	if err := j.checkout(ctx, w, tip); err != nil {
		return "", nil, err
	}
	meta.Replay = nil
	return tip, nil, writeMetadata(w.Path, meta)
}

// ContinueReplay records the workspace's files in the copy the replay
// stopped at and goes on with the replay ReplayIn stopped, as it stopped: to
// the next copy that conflicts, whose paths it returns, or to the end, whose
// commit it returns. A path whose file still holds the conflict it was given
// stays conflicted. A replay step whose resolution leaves its commit's change
// empty drops the commit.
func (j *Jujutsu) ContinueReplay(ctx context.Context, w Worktree, at time.Time) (string, []string, error) {
	meta, err := readMetadata(w.Path)
	if err != nil {
		return "", nil, err
	}
	if meta.Replay == nil {
		return "", nil, fmt.Errorf("workspace %s is not replaying", w.Path)
	}
	if _, err := j.run(ctx, w.Path, stamp(at), "util", "snapshot"); err != nil {
		return "", nil, err
	}
	return j.proceed(ctx, w, meta, at)
}

// Replaying returns the commit of the workspace's branch whose copy the
// replay in the workspace stopped at, with the paths that copy holds
// conflicted as it was stopped, whatever the workspace's files hold since,
// and whether a replay is in progress there. A replay in progress with no
// copy in conflict returns "".
func (j *Jujutsu) Replaying(ctx context.Context, w Worktree) (string, []string, bool, error) {
	meta, err := readMetadata(w.Path)
	if err != nil || meta.Replay == nil {
		return "", nil, false, err
	}
	states, err := j.steps(ctx, meta.Replay)
	if err != nil {
		return "", nil, false, err
	}
	for _, s := range meta.Replay.Steps {
		if state, ok := states[s.Change]; ok && state.conflict {
			conflicts, err := j.conflicts(ctx, s.Change)
			return s.Original, conflicts, true, err
		}
	}
	return "", nil, true, nil
}

// MarkedFiles returns the paths, of those given, whose file in the workspace
// holds a conflict marker line, as Git's MarkedFiles finds them.
func (j *Jujutsu) MarkedFiles(w Worktree, paths []string) ([]string, error) {
	return markedFiles(w.Path, paths)
}

// Branch returns the commit a branch of the clone points at, and whether the
// branch exists.
func (j *Jujutsu) Branch(ctx context.Context, name string) (string, bool, error) {
	return j.git().Branch(ctx, name)
}

// Ancestor reports whether commit is reachable from tip.
func (j *Jujutsu) Ancestor(ctx context.Context, commit, tip string) (bool, error) {
	return j.git().Ancestor(ctx, commit, tip)
}

// MergeBase returns the best common ancestor of two commits.
func (j *Jujutsu) MergeBase(ctx context.Context, a, b string) (string, error) {
	return j.git().MergeBase(ctx, a, b)
}

// Commit reads the tree, parents and message of a commit.
func (j *Jujutsu) Commit(ctx context.Context, revision string) (Commit, error) {
	return j.git().Commit(ctx, revision)
}

// Diff reads two recorded commits as Git's Diff does.
func (j *Jujutsu) Diff(ctx context.Context, base, candidate string) (string, error) {
	return j.git().Diff(ctx, base, candidate)
}

// ChangedPaths lists the paths changed between two recorded commits as Git's
// ChangedPaths does.
func (j *Jujutsu) ChangedPaths(ctx context.Context, base, candidate string) ([]string, error) {
	return j.git().ChangedPaths(ctx, base, candidate)
}

// Markers returns the paths, of those given, whose file in commit still holds
// a conflict marker line, as Git's Markers does.
func (j *Jujutsu) Markers(ctx context.Context, commit string, paths []string) ([]string, error) {
	return j.git().Markers(ctx, commit, paths)
}

// Export writes the tracked regular files of a commit's tree into dir as
// Git's Export does.
func (j *Jujutsu) Export(ctx context.Context, commit, dir string) error {
	return j.git().Export(ctx, commit, dir)
}

// Squash makes the commit Git's Squash makes from the same arguments.
func (j *Jujutsu) Squash(ctx context.Context, base, candidate, message string, at time.Time) (string, error) {
	return j.git().Squash(ctx, base, candidate, message, at)
}

// Rebase rebases head onto onto from their merge base, as RebaseFrom does.
func (j *Jujutsu) Rebase(ctx context.Context, onto, head, message string, at time.Time) (string, []string, error) {
	return j.RebaseFrom(ctx, "", onto, head, message, at)
}

// RebaseFrom makes the commit Git's RebaseFrom makes from the same arguments
// when head holds no stored conflict and Git's merge is clean. Otherwise
// Jujutsu makes the commit: the change head holds since base, or since
// head's merge base with onto when base is empty, rebased onto onto with
// message, committed by Osmia at the given time. It holds each path the rebase conflicts in as a
// stored conflict, which StoredConflicts reads and a workspace on the commit
// materializes with Git-style markers: the lines between "<<<<<<<" and
// "|||||||" are onto's, those between "|||||||" and "=======" base's, and
// those between "=======" and ">>>>>>>" head's, and a side that deleted the
// file holds no lines. It returns the paths Jujutsu holds conflicted, sorted.
// Each such rebase makes a commit of its own, even from the same arguments.
func (j *Jujutsu) RebaseFrom(ctx context.Context, base, onto, head, message string, at time.Time) (string, []string, error) {
	g := j.git()
	stored, err := j.StoredConflicts(ctx, head)
	if err != nil {
		return "", nil, err
	}
	if len(stored) == 0 {
		commit, conflicts, err := g.RebaseFrom(ctx, base, onto, head, message, at)
		if err != nil || len(conflicts) == 0 {
			return commit, conflicts, err
		}
	}
	if base == "" {
		if base, err = g.MergeBase(ctx, onto, head); err != nil {
			return "", nil, err
		}
	}
	if err := j.initialize(ctx); err != nil {
		return "", nil, err
	}
	if err := j.importBranches(ctx); err != nil {
		return "", nil, err
	}
	change, err := j.create(ctx, base, message, at)
	if err != nil {
		return "", nil, err
	}
	if _, err := j.run(ctx, j.repository(), stamp(at), "--ignore-working-copy", "restore", "--from", head, "--into", change); err != nil {
		return "", nil, err
	}
	if _, err := j.run(ctx, j.repository(), stamp(at), "--ignore-working-copy", "rebase", "--revisions", change, "--onto", onto); err != nil {
		return "", nil, err
	}
	rebased, err := j.describe(ctx, j.repository(), false, change)
	if err != nil {
		return "", nil, err
	}
	conflicts, err := j.conflicts(ctx, change)
	return rebased.commit, conflicts, err
}

// create makes an empty commit on parent with message and returns its change
// ID. Nothing else moves.
func (j *Jujutsu) create(ctx context.Context, parent, message string, at time.Time) (string, error) {
	settings := append(stamp(at), "templates.commit_summary=change_id")
	_, stderr, err := j.runOutput(ctx, j.repository(), settings, "--ignore-working-copy", "new", "--no-edit", parent, "--message", message)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(stderr, "\n") {
		if change, ok := strings.CutPrefix(line, "Created new commit "); ok && len(strings.Fields(change)) == 1 {
			return change, nil
		}
	}
	return "", fmt.Errorf("jj new on %s named no commit: %s", parent, strings.TrimSpace(stderr))
}

// StoredConflicts returns the paths commit holds as stored conflicts,
// sorted. The repository reads the commits it made and those on the branches
// it imported; any other commit reads as holding none.
func (j *Jujutsu) StoredConflicts(ctx context.Context, commit string) ([]string, error) {
	if exists, err := j.initialized(); err != nil || !exists {
		return nil, err
	}
	out, err := j.run(ctx, j.repository(), nil, "--ignore-working-copy", "log", "--no-graph", "--revisions", "present("+commit+")", "--template", `if(conflict, "conflict")`)
	if err != nil || out != "conflict" {
		return nil, err
	}
	return j.conflicts(ctx, commit)
}

// changePattern is a full Jujutsu change ID.
var changePattern = regexp.MustCompile(`^[k-z]{32}$`)

// Change returns the change ID of the workspace's first working-copy
// commit, which the first snapshot of the workspace commits. A workspace
// whose metadata records no change returns "".
func (j *Jujutsu) Change(_ context.Context, w Worktree) (string, error) {
	meta, err := readMetadata(w.Path)
	return meta.Change, err
}

// commitIDPattern is a full Git commit ID, SHA-1 or SHA-256.
var commitIDPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// ChangeOf returns the change ID the repository gives commit: one it made,
// or one on a branch it imported. A commit it does not know returns "". A
// commit that is not a full commit ID is refused.
func (j *Jujutsu) ChangeOf(ctx context.Context, commit string) (string, error) {
	if !commitIDPattern.MatchString(commit) {
		return "", fmt.Errorf("%q is not a full commit ID", commit)
	}
	if exists, err := j.initialized(); err != nil || !exists {
		return "", err
	}
	return j.run(ctx, j.repository(), nil, "--ignore-working-copy", "log", "--no-graph", "--revisions", "present("+commit+")", "--template", "change_id")
}

// Carry returns commit with a change-id header naming change in place of any
// it had, so that the repository gives it that change ID once it imports a
// branch at it, as ChangeOf reads it then. Everything else commit records
// stays, stored conflicts included, and the same arguments make the same
// commit. A commit that already names change is returned as it is.
func (j *Jujutsu) Carry(ctx context.Context, commit, change string) (string, error) {
	if !changePattern.MatchString(change) {
		return "", fmt.Errorf("%q is not a change ID", change)
	}
	g := j.git()
	raw, err := g.runInRaw(ctx, j.Clone, nil, "cat-file", "commit", commit)
	if err != nil {
		return "", err
	}
	header, message, found := strings.Cut(raw, "\n\n")
	if !found {
		return "", fmt.Errorf("commit %s has no message", commit)
	}
	var lines []string
	for _, line := range strings.Split(header, "\n") {
		if line == "change-id "+change {
			return commit, nil
		}
		if !strings.HasPrefix(line, "change-id ") {
			lines = append(lines, line)
		}
	}
	lines = append(lines, "change-id "+change)
	carried, err := g.runInput(ctx, j.Clone, nil, strings.NewReader(strings.Join(lines, "\n")+"\n\n"+message), "hash-object", "-t", "commit", "-w", "--stdin")
	return strings.TrimSpace(carried), err
}

// Replay makes the commit Git's Replay makes from the same arguments, and
// reports the same conflicted paths, in a temporary detached worktree under
// Directory that it removes.
func (j *Jujutsu) Replay(ctx context.Context, head, onto string, at time.Time) (string, []string, error) {
	return j.git().Replay(ctx, head, onto, at)
}

// Remote returns the name of the clone's remote whose URL names the given
// owner/repository, as Git's Remote does.
func (j *Jujutsu) Remote(ctx context.Context, repository string) (string, error) {
	return j.git().Remote(ctx, repository)
}

// Fetch fetches one branch of a remote into the clone's remote-tracking ref
// as Git's Fetch does, and returns the commit it points at.
func (j *Jujutsu) Fetch(ctx context.Context, remote, branch string) (string, error) {
	return j.git().Fetch(ctx, remote, branch)
}

// RemoteBranch asks a remote which commit one of its branches points at, as
// Git's RemoteBranch does.
func (j *Jujutsu) RemoteBranch(ctx context.Context, remote, branch string) (string, bool, error) {
	return j.git().RemoteBranch(ctx, remote, branch)
}

// Push points a remote's branch at commit only while it is at expected, as
// Git's Push does.
func (j *Jujutsu) Push(ctx context.Context, remote, commit, branch, expected string) error {
	return j.git().Push(ctx, remote, commit, branch, expected)
}

// readMetadata reads what the provider recorded about the workspace at path.
func readMetadata(path string) (metadata, error) {
	data, err := os.ReadFile(filepath.Join(path, ".jj", metadataName))
	if err != nil {
		return metadata{}, err
	}
	var meta metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return metadata{}, fmt.Errorf("workspace %s: %w", path, err)
	}
	return meta, nil
}

// writeMetadata replaces what the provider recorded about the workspace at
// path in one rename.
func writeMetadata(path string, meta metadata) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	file := filepath.Join(path, ".jj", metadataName)
	if err := os.WriteFile(file+".new", data, 0600); err != nil {
		return err
	}
	return os.Rename(file+".new", file)
}

func splitNull(out string) []string {
	var fields []string
	for _, field := range strings.Split(out, "\x00") {
		if field != "" {
			fields = append(fields, field)
		}
	}
	return fields
}

// run runs jj in dir, on the repository or workspace there when dir is not
// empty, with jjSettings and the extra settings, and returns its standard
// output. The owner's jj configuration and Git excludes file do not apply.
func (j *Jujutsu) run(ctx context.Context, dir string, settings []string, args ...string) (string, error) {
	out, _, err := j.runOutput(ctx, dir, settings, args...)
	return strings.TrimSpace(out), err
}

func (j *Jujutsu) runOutput(ctx context.Context, dir string, settings []string, args ...string) (string, string, error) {
	executable := j.JJ
	if executable == "" {
		executable = "jj"
	}
	full := []string{"--no-pager", "--color=never"}
	if dir != "" {
		full = append(full, "--repository", dir)
	}
	for _, setting := range append(slices.Clone(jjSettings), settings...) {
		full = append(full, "--config", setting)
	}
	cmd := exec.CommandContext(ctx, executable, append(full, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"JJ_CONFIG=" + os.DevNull, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.excludesFile", "GIT_CONFIG_VALUE_0=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	for _, name := range []string{"PATH", "HOME", "TMPDIR"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", "", fmt.Errorf("jj %s: %w", args[0], ctxErr)
		}
		return stdout.String(), stderr.String(), &JJError{Args: args, Err: err, Stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.String(), stderr.String(), nil
}

// JJError is a jj command that failed, with what it said.
type JJError struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *JJError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("jj %s: %v", strings.Join(e.Args, " "), e.Err)
	}
	return fmt.Sprintf("jj %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
}
func (e *JJError) Unwrap() error { return e.Err }
