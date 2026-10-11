package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
)

// ErrMigrationIncomplete reports that a project's trace could not finish
// moving from the shared layout, every workstream's records inside the
// project's own trace repository, to the per-workstream layout. The caller
// must not open or schedule the project; the shared trace is left exactly as
// it was and stays authoritative, and a later call to Migrate resumes or
// retries from where this one left off.
var ErrMigrationIncomplete = errors.New("project trace migration is incomplete")

// migrationInterrupt lets tests simulate a crash at a named boundary step of
// Migrate, the way failPublication does for an ordinary publication. It is
// nil in production.
var migrationInterrupt func(string) error

// Migrate moves project's trace from the shared layout to the per-workstream
// layout, if it has not finished already: every workstream still recorded
// inside the project's own trace repository gets its own dedicated trace
// repository, under the Osmia root and outside the project's own and outside
// any target repository, and the project's own trace repository keeps only
// durable project knowledge once every workstream is moved. Callers open the
// project's trace with Open only after Migrate returns nil; while it returns
// ErrMigrationIncomplete the project must not be scheduled, though other
// projects are unaffected.
//
// The move is resumable. A crash before, during or after any step, and a
// second call once nothing is left to do, both leave every retained
// workstream's trace readable from exactly one place, with no record lost or
// duplicated. For each workstream still in the shared layout, Migrate builds
// the workstream's own trace repository and verifies that it holds the same
// records, latest content and revision history as the shared trace before
// relying on it; an archived workstream that has not been cleaned up is
// moved the same way as any other, so archive cleanup's knowledge-
// incorporation and dependency checks keep reading a real trace for it.
// Only once every workstream's copy is verified does Migrate reclaim the
// obsolete shared storage: it rewrites the project's own history to drop the
// workstream paths and prunes the objects that history alone kept, while
// keeping the charter, knowledge base, notes and decision history intact. A
// verification failure leaves the shared trace exactly as it was; nothing is
// reclaimed until every workstream's copy is proven.
//
// logf receives migration start, each workstream's copy, verification and
// switch, reclamation, completion and failure, naming only the project and
// workstream; it never receives transcript content or credentials. A nil
// logf discards them.
func Migrate(ctx context.Context, root config.Root, project config.ProjectID, logf func(format string, args ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := config.CheckProjectIDs(project); err != nil {
		return err
	}
	directory, err := root.ProjectTrace(project)
	if err != nil {
		return err
	}
	shared, err := hasSharedLayout(directory)
	if err != nil || !shared {
		return err
	}
	pr, err := openStoreDirectory(root, project, "", directory)
	if err != nil {
		return err
	}
	pr.failPublication = migrationInterrupt
	ok := false
	defer func() {
		if !ok {
			pr.Close()
		}
	}()
	if err := pr.checkGit(); err != nil {
		return err
	}
	if err := pr.acquire(); err != nil {
		return err
	}
	if err := pr.recoverPublication(ctx); err != nil {
		return err
	}
	logf("migration: project %s: starting migration from the shared trace layout", project)
	streams, err := legacyWorkstreamIDs(pr)
	if err != nil {
		return fmt.Errorf("%w: %s: list legacy workstreams: %v", ErrMigrationIncomplete, project, err)
	}
	for _, stream := range streams {
		if err := migrateOneWorkstream(ctx, root, project, stream, pr, logf); err != nil {
			return err
		}
	}
	if err := reclaimSharedWorkstreams(ctx, pr, logf); err != nil {
		logf("migration: project %s: reclaiming the shared trace storage failed: %v", project, err)
		return fmt.Errorf("%w: %s: reclaim the shared trace storage: %v", ErrMigrationIncomplete, project, err)
	}
	logf("migration: project %s: migration complete", project)
	ok = true
	return pr.Close()
}

// hasSharedLayout reports whether a project's own trace directory still
// holds the shared layout's "workstreams" directory. It does not require the
// directory to exist at all: a project with no trace yet, or one whose
// migration already reclaimed the shared storage, both report false.
func hasSharedLayout(directory string) (bool, error) {
	info, err := os.Lstat(filepath.Join(directory, "workstreams"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s/workstreams: expected a directory", directory)
	}
	return true, nil
}

// legacyWorkstreamIDs lists the workstreams the shared layout holds inside
// pr's own trace directory, under "workstreams", in a stable order.
func legacyWorkstreamIDs(pr *Repository) ([]config.WorkstreamID, error) {
	entries, err := fs.ReadDir(pr.dir.FS(), "workstreams")
	if err != nil {
		return nil, err
	}
	ids := make([]config.WorkstreamID, 0, len(entries))
	for _, e := range entries {
		id, err := config.ParseWorkstreamID(e.Name())
		if err != nil || !e.IsDir() {
			return nil, fmt.Errorf("%s: not a legacy workstream trace directory", e.Name())
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

// migrateOneWorkstream copies stream's records into its own dedicated trace
// repository if that has not happened yet, verifies the copy against the
// shared trace, and switches authority to it. Once this returns nil, stream
// is read only from its own repository; the shared copy is reclaimed later,
// together with every other workstream's.
func migrateOneWorkstream(ctx context.Context, root config.Root, project config.ProjectID, stream config.WorkstreamID, pr *Repository, logf func(string, ...any)) error {
	newDirectory, err := root.WorkstreamTrace(project, stream)
	if err != nil {
		return err
	}
	switch _, err := os.Lstat(newDirectory); {
	case os.IsNotExist(err):
		logf("migration: project %s: workstream %s: copying its trace into its own repository", project, stream)
		if err := copyWorkstreamTrace(ctx, root, project, stream, pr); err != nil {
			logf("migration: project %s: workstream %s: copy failed: %v", project, stream, err)
			return fmt.Errorf("%w: %s: workstream %s: copy its trace: %v", ErrMigrationIncomplete, project, stream, err)
		}
		if err := pr.boundary("migration-workstream-copied:" + string(stream)); err != nil {
			return err
		}
		logf("migration: project %s: workstream %s: copied", project, stream)
	case err != nil:
		return err
	}
	if err := verifyMigratedWorkstream(ctx, root, project, stream, pr); err != nil {
		logf("migration: project %s: workstream %s: verification failed: %v", project, stream, err)
		return fmt.Errorf("%w: %s: workstream %s: verify its trace: %v", ErrMigrationIncomplete, project, stream, err)
	}
	if err := pr.boundary("migration-workstream-verified:" + string(stream)); err != nil {
		return err
	}
	logf("migration: project %s: workstream %s: verified", project, stream)
	// Nothing further makes the copy authoritative: every reader of the
	// per-workstream layout already discovers workstreams from their own
	// dedicated directories alone, so a verified copy is switched to the
	// instant it exists. This step is named so a crash can be interrupted
	// and resumed right after a workstream is switched, as after any other.
	if err := pr.boundary("migration-workstream-switched:" + string(stream)); err != nil {
		return err
	}
	logf("migration: project %s: workstream %s: switched to its own trace repository", project, stream)
	return nil
}

// copyWorkstreamTrace builds stream's own dedicated trace repository from
// its subtree of the shared trace, staged outside WorkstreamTraces and moved
// into place by one atomic rename, exactly as creating a new workstream
// does, so a directory listing never observes it before it is complete.
func copyWorkstreamTrace(ctx context.Context, root config.Root, project config.ProjectID, stream config.WorkstreamID, pr *Repository) error {
	prefix := "workstreams/" + string(stream)
	newDirectory, err := root.WorkstreamTrace(project, stream)
	if err != nil {
		return err
	}
	base, err := root.WorkstreamTraces(project)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	staging, err := root.WorkstreamStaging()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0700); err != nil {
		return err
	}
	build, err := os.MkdirTemp(staging, "ws-"+string(stream)+"-")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(build)
		}
	}()
	s, err := openStoreDirectory(root, project, stream, build)
	if err != nil {
		return err
	}
	if err := func() error {
		defer s.Close()
		if err := s.dir.Mkdir(".git", 0700); err != nil {
			return err
		}
		for _, name := range []string{".git/objects", ".git/refs/heads"} {
			if err := s.mkdir(name); err != nil {
				return err
			}
		}
		for _, f := range []struct{ name, data string }{{".git/config", gitConfig}, {".git/HEAD", "ref: refs/heads/main\n"}, {".git/osmia.lock", ""}} {
			if err := s.writeFile(f.name, []byte(f.data)); err != nil {
				return err
			}
		}
		if err := s.acquire(); err != nil {
			return err
		}
		var paths []string
		found := map[string]bool{}
		if err := pr.walkDir(prefix, func(parent *os.Root, name string, entry fs.DirEntry) error {
			if err := checkedEntry(parent, name, entry); err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			data, err := readEntry(parent, name, entry)
			if err != nil {
				return err
			}
			if err := s.writeFile(name, data); err != nil {
				return err
			}
			paths = append(paths, name)
			found[name] = true
			return nil
		}); err != nil {
			return err
		}
		if !found[prefix+"/workstream.json"] {
			return fmt.Errorf("%s: the shared trace holds no workstream manifest", prefix)
		}
		for _, name := range []string{"documents.jsonl", "events.jsonl", "ledger.jsonl"} {
			name = prefix + "/" + name
			if found[name] {
				continue
			}
			if err := s.writeFile(name, nil); err != nil {
				return err
			}
			paths = append(paths, name)
		}
		slices.Sort(paths)
		return s.commit(ctx, paths, "Migrate workstream trace from the shared layout")
	}(); err != nil {
		return err
	}
	if err := os.Rename(build, newDirectory); err != nil {
		if _, statErr := os.Lstat(newDirectory); statErr == nil {
			return fmt.Errorf("%w: workstream %s already has its own trace repository", ErrConflict, stream)
		}
		return err
	}
	ok = true
	return nil
}

// verifyMigratedWorkstream reads stream's subtree of the shared trace and
// its own dedicated trace repository independently, so a resumed migration
// re-checks a copy made in an earlier process the same way it checks one
// just made, and reports whether they hold exactly the same files with the
// same bytes, and whether the copy reads as a valid trace in its own right.
func verifyMigratedWorkstream(ctx context.Context, root config.Root, project config.ProjectID, stream config.WorkstreamID, pr *Repository) error {
	prefix := "workstreams/" + string(stream)
	shared := map[string][]byte{}
	if err := pr.walkDir(prefix, func(parent *os.Root, name string, entry fs.DirEntry) error {
		if err := checkedEntry(parent, name, entry); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := readEntry(parent, name, entry)
		if err != nil {
			return err
		}
		shared[name] = data
		return nil
	}); err != nil {
		return fmt.Errorf("read the shared trace: %w", err)
	}
	for _, name := range []string{"documents.jsonl", "events.jsonl", "ledger.jsonl"} {
		name = prefix + "/" + name
		if _, ok := shared[name]; !ok {
			shared[name] = nil
		}
	}
	s, err := openWorkstreamDirectory(root, project, stream)
	if err != nil {
		return fmt.Errorf("open the migrated trace: %w", err)
	}
	defer s.Close()
	if err := s.checkGit(); err != nil {
		return fmt.Errorf("the migrated trace: %w", err)
	}
	if err := s.acquire(); err != nil {
		return fmt.Errorf("the migrated trace: %w", err)
	}
	if err := s.recoverPublication(ctx); err != nil {
		return fmt.Errorf("the migrated trace: %w", err)
	}
	copied := map[string][]byte{}
	if err := s.walk(func(parent *os.Root, name string, entry fs.DirEntry) error {
		if entry.IsDir() {
			return nil
		}
		data, err := readEntry(parent, name, entry)
		if err != nil {
			return err
		}
		copied[name] = data
		return nil
	}); err != nil {
		return fmt.Errorf("read the migrated trace: %w", err)
	}
	if len(shared) != len(copied) {
		return fmt.Errorf("the migrated trace holds %d files, the shared trace holds %d", len(copied), len(shared))
	}
	for name, data := range shared {
		other, ok := copied[name]
		if !ok || !bytes.Equal(data, other) {
			return fmt.Errorf("%s: the migrated trace does not match the shared trace", name)
		}
	}
	if _, _, err := s.scan(); err != nil {
		return fmt.Errorf("the migrated trace does not read as a valid trace: %w", err)
	}
	if _, err := s.checkHistory(ctx); err != nil {
		return fmt.Errorf("the migrated trace's history does not check out: %w", err)
	}
	return nil
}

// migrationReclaimFile journals the one-time history rewrite that drops the
// shared layout's workstream paths from the project's own trace.
const migrationReclaimFile = ".git/osmia-migration-reclaim.json"

type migrationReclaimJournal struct {
	Parent string `json:"parent"`
	Commit string `json:"commit"`
}

// reclaimSharedWorkstreams rewrites pr's history to drop every "workstreams"
// entry, keeping the charter, knowledge base, notes and decision history,
// and prunes the objects that history alone held. It resumes an interrupted
// rewrite before checking whether one is still needed, so calling it when
// the shared storage is already reclaimed, or when an earlier rewrite was
// interrupted, both leave pr with no "workstreams" directory and no
// unreachable objects from it.
func reclaimSharedWorkstreams(ctx context.Context, pr *Repository, logf func(string, ...any)) error {
	if err := recoverMigrationReclaim(ctx, pr); err != nil {
		return err
	}
	switch _, err := pr.dir.Stat("workstreams"); {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return err
	}
	logf("migration: project %s: reclaiming the obsolete shared trace storage", pr.project)
	pr.forget()
	if _, err := pr.checkHistory(ctx); err != nil {
		return err
	}
	if err := pr.checkGit(); err != nil {
		return err
	}
	refs, err := pr.git(ctx, nil, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return err
	}
	if strings.TrimSpace(refs) != "refs/heads/main" {
		return fmt.Errorf("trace reclamation requires only the service-owned main reference")
	}
	parent, err := pr.git(ctx, nil, "rev-parse", "refs/heads/main")
	if err != nil {
		return err
	}
	commits, err := pr.git(ctx, nil, "rev-list", "--reverse", "--topo-order", parent)
	if err != nil {
		return err
	}
	mapped, trees := map[string]string{}, map[string]string{}
	for _, oid := range strings.Fields(commits) {
		raw, err := pr.gitBytes(ctx, nil, "", "cat-file", "commit", oid)
		if err != nil {
			return err
		}
		header, message, ok := bytes.Cut(raw, []byte("\n\n"))
		if !ok {
			return fmt.Errorf("invalid trace commit")
		}
		lines := strings.Split(string(header), "\n")
		for i, line := range lines {
			key, value, _ := strings.Cut(line, " ")
			switch key {
			case "tree":
				tree, err := stripWorkstreamsTree(ctx, pr, value, trees)
				if err != nil {
					return err
				}
				lines[i] = "tree " + tree
			case "parent":
				replacement, ok := mapped[value]
				if !ok {
					return fmt.Errorf("trace parent missing during reclamation")
				}
				lines[i] = "parent " + replacement
			}
		}
		rewritten := append([]byte(strings.Join(lines, "\n")+"\n\n"), message...)
		mapped[oid], err = pr.git(ctx, rewritten, "hash-object", "-t", "commit", "-w", "--stdin")
		if err != nil {
			return err
		}
	}
	if err := pr.syncObjects(); err != nil {
		return err
	}
	j := migrationReclaimJournal{Parent: parent, Commit: mapped[parent]}
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := pr.writeFile(migrationReclaimFile, data); err != nil {
		return err
	}
	if err := pr.boundary("migration-reclaim-journal-written"); err != nil {
		return err
	}
	return recoverMigrationReclaim(ctx, pr)
}

// stripWorkstreamsTree returns tree with its top-level "workstreams" entry
// removed, or tree itself when it has none.
func stripWorkstreamsTree(ctx context.Context, r *Repository, tree string, cache map[string]string) (string, error) {
	if rewritten, ok := cache[tree]; ok {
		return rewritten, nil
	}
	raw, err := r.gitBytes(ctx, nil, "", "ls-tree", "-z", tree)
	if err != nil {
		return "", err
	}
	var kept []byte
	changed := false
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		_, name, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok {
			return "", fmt.Errorf("invalid trace tree")
		}
		if string(name) == "workstreams" {
			changed = true
			continue
		}
		kept = append(kept, entry...)
		kept = append(kept, 0)
	}
	result := tree
	if changed {
		result, err = r.git(ctx, kept, "mktree", "-z")
		if err != nil {
			return "", err
		}
	}
	cache[tree] = result
	return result, nil
}

// recoverMigrationReclaim resumes an interrupted reclamation before any
// trace read or write.
func recoverMigrationReclaim(ctx context.Context, r *Repository) error {
	data, err := r.readFile(migrationReclaimFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var j migrationReclaimJournal
	if err := decode(data, &j); err != nil {
		return err
	}
	if !objectID.MatchString(j.Parent) || !objectID.MatchString(j.Commit) {
		return fmt.Errorf("invalid trace migration reclaim journal")
	}
	if err := r.checkGit(); err != nil {
		return err
	}
	head, err := r.git(ctx, nil, "rev-parse", "refs/heads/main")
	if err != nil {
		return err
	}
	if head != j.Parent && head != j.Commit {
		return fmt.Errorf("trace changed during reclamation")
	}
	if err := r.writeFile(".git/refs/heads/main", []byte(j.Commit+"\n")); err != nil {
		return err
	}
	if err := r.boundary("migration-reclaim-ref-published"); err != nil {
		return err
	}
	if err := r.dir.RemoveAll("workstreams"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := syncDir(r.dir, "."); err != nil {
		return err
	}
	r.changed()
	r.forget()
	r.recordFiles, r.verified, r.workflows = nil, nil, nil
	r.gitMu.Lock()
	r.tree, r.treeHead, r.synced = nil, "", nil
	r.gitMu.Unlock()
	if err := r.boundary("migration-reclaim-files-removed"); err != nil {
		return err
	}
	// Neither the index, reflogs nor packed unreachable objects may retain
	// the workstream paths this history rewrite dropped.
	for _, args := range [][]string{{"read-tree", j.Commit}, {"reflog", "expire", "--expire=now", "--all"}, {"-c", "gc.packRefs=false", "gc", "--prune=now"}} {
		if _, err := r.git(ctx, nil, args...); err != nil {
			return err
		}
	}
	if err := r.syncObjects(); err != nil {
		return err
	}
	if err := r.boundary("migration-reclaim-objects-pruned"); err != nil {
		return err
	}
	if err := r.dir.Remove(migrationReclaimFile); err != nil {
		return err
	}
	return syncDir(r.dir, ".git")
}
