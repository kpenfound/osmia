package trace

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
)

const gitConfig = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n\tlogallrefupdates = true\n"

func (r *Repository) checkGit() error {
	if _, err := r.root.ProjectTrace(r.project); err != nil {
		return err
	}
	if err := r.checked(".git"); err != nil {
		return err
	}
	info, err := r.dir.Stat(".git")
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("trace .git must be a dedicated directory, not a linked worktree")
	}
	err = fs.WalkDir(r.dir.FS(), ".git", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == ".git/commondir" || name == ".git/objects/info/alternates" || name == ".git/objects/info/http-alternates" {
			return fmt.Errorf("%s: external Git storage is forbidden", name)
		}
		return r.checkedEntry(name, entry)
	})
	if err != nil {
		return err
	}
	data, err := r.readFile(".git/config")
	if err != nil {
		return err
	}
	if string(data) != gitConfig {
		return fmt.Errorf("trace .git/config differs from the isolated local configuration")
	}
	head, err := r.readFile(".git/HEAD")
	if err != nil {
		return err
	}
	if string(head) != "ref: refs/heads/main\n" {
		return fmt.Errorf("unexpected trace HEAD")
	}
	return nil
}
func (r *Repository) git(ctx context.Context, input []byte, args ...string) (string, error) {
	out, err := r.gitBytes(ctx, input, "", args...)
	return strings.TrimSpace(string(out)), err
}

// gitBytes runs Git without checking the repository; callers run checkGit
// once before the Git commands of an operation.
func (r *Repository) gitBytes(ctx context.Context, input []byte, index string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + r.directory + "/.git", "--work-tree=" + r.directory, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "gc.auto=0"}, args...)...)
	cmd.Dir = r.directory
	// No inherited Git routing, credentials, config includes, hooks or signing.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Osmia", "GIT_AUTHOR_EMAIL=osmia@localhost", "GIT_COMMITTER_NAME=Osmia", "GIT_COMMITTER_EMAIL=osmia@localhost", "LC_ALL=C"}
	if index != "" {
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
	}
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// A cancelled context kills the command; report the cancellation
		// rather than the signal so callers recognize it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("trace git %s: %w", args[0], ctxErr)
		}
		return nil, fmt.Errorf("trace git %s: %w: %s", args[0], err, out)
	}
	return out, nil
}

// commit hashes supplied bytes without Git attributes, filters or target files.
// It does not commit configuration or unrelated files from the work tree.
func (r *Repository) commit(ctx context.Context, paths []string, message string) error {
	return r.commitContent(ctx, paths, nil, message)
}

// commitContent commits paths like commit, taking the bytes of any path in
// content from there instead of from the work tree.
func (r *Repository) commitContent(ctx context.Context, paths []string, content map[string][]byte, message string) error {
	parent, err := r.readFile(".git/refs/heads/main")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	old := strings.TrimSpace(string(parent))
	if err := r.checkGit(); err != nil {
		return err
	}
	base := "--empty"
	if old != "" {
		base = "HEAD"
	}
	if _, err := r.git(ctx, nil, "read-tree", base); err != nil {
		return err
	}
	for _, name := range paths {
		data, ok := content[name]
		if !ok {
			if data, err = r.readFile(name); err != nil {
				return err
			}
		}
		oid, err := r.git(ctx, data, "hash-object", "-w", "--stdin")
		if err != nil {
			return err
		}
		if _, err := r.git(ctx, nil, "update-index", "--add", "--cacheinfo", "100644,"+oid+","+name); err != nil {
			return err
		}
	}
	tree, err := r.git(ctx, nil, "write-tree")
	if err != nil {
		return err
	}
	args := []string{"commit-tree", tree, "-m", message}
	if old != "" {
		args = append(args, "-p", old)
	}
	oid, err := r.git(ctx, nil, args...)
	if err != nil {
		return err
	}
	if old == "" {
		old = strings.Repeat("0", 40)
	}
	_, err = r.git(ctx, nil, "update-ref", "refs/heads/main", oid, old)
	return err
}

// checkHistory finishes journaled workflow publication and reports other
// uncommitted file changes without guessing how to reconcile them. The owner
// edits charter.md and a workstream's spec.md and plan.json directly; Charter
// and OwnerDocuments record those edits, so a difference there is expected.
func (r *Repository) checkHistory(ctx context.Context) error {
	if err := r.recoverPublication(ctx); err != nil {
		return err
	}
	blobs, err := r.headTree(ctx)
	if err != nil {
		return err
	}
	tracked := map[string]bool{}
	for name, oid := range blobs {
		tracked[name] = true
		if ownerEdited(name) {
			continue
		}
		data, err := r.readFile(name)
		if err != nil {
			return fmt.Errorf("trace history %s: %w", name, err)
		}
		hash := sha1.New()
		fmt.Fprintf(hash, "blob %d\x00", len(data))
		hash.Write(data)
		if fmt.Sprintf("%x", hash.Sum(nil)) != oid {
			return fmt.Errorf("%s: trace files differ from committed history; reconciliation required", name)
		}
	}
	return r.walk(func(name string, entry fs.DirEntry) error {
		if !entry.IsDir() && !tracked[name] && (strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, "/workstream.json") || name == "project.json" || strings.HasSuffix(name, "/workflow.json")) {
			return fmt.Errorf("%s: trace file is not committed; reconciliation required", name)
		}
		return nil
	})
}

// headTree returns the blob identity of every file committed at HEAD. A
// commit's tree never changes, so the listing is kept until HEAD moves.
func (r *Repository) headTree(ctx context.Context) (map[string]string, error) {
	ref, err := r.readFile(".git/refs/heads/main")
	if err != nil {
		return nil, err
	}
	head := strings.TrimSpace(string(ref))
	if !objectID.MatchString(head) {
		return nil, fmt.Errorf("invalid trace HEAD %q", head)
	}
	r.gitMu.Lock()
	defer r.gitMu.Unlock()
	if r.tree != nil && r.treeHead == head {
		return r.tree, nil
	}
	if err := r.checkGit(); err != nil {
		return nil, err
	}
	tree, err := r.git(ctx, nil, "ls-tree", "-rz", head)
	if err != nil {
		return nil, err
	}
	blobs := map[string]string{}
	for _, entry := range strings.Split(tree, "\x00") {
		if entry == "" {
			continue
		}
		meta, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[0] != "100644" || fields[1] != "blob" {
			return nil, fmt.Errorf("invalid trace Git tree entry %q", entry)
		}
		if err := relative(name); err != nil {
			return nil, err
		}
		blobs[name] = fields[2]
	}
	r.tree, r.treeHead = blobs, head
	return blobs, nil
}
