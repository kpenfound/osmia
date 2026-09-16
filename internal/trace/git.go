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
		return r.checked(name)
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
	if err := r.checkGit(); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + r.directory + "/.git", "--work-tree=" + r.directory, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "gc.auto=0"}, args...)...)
	cmd.Dir = r.directory
	// No inherited Git routing, credentials, config includes, hooks or signing.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Osmia", "GIT_AUTHOR_EMAIL=osmia@localhost", "GIT_COMMITTER_NAME=Osmia", "GIT_COMMITTER_EMAIL=osmia@localhost", "LC_ALL=C"}
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("trace git %s: %w: %s", args[0], err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// commit hashes supplied bytes without Git attributes, filters or target files.
// It does not commit configuration or unrelated files from the work tree.
func (r *Repository) commit(ctx context.Context, paths []string, message string) error {
	parent, err := r.readFile(".git/refs/heads/main")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	old := strings.TrimSpace(string(parent))
	base := "--empty"
	if old != "" {
		base = "HEAD"
	}
	if _, err := r.git(ctx, nil, "read-tree", base); err != nil {
		return err
	}
	for _, name := range paths {
		data, err := r.readFile(name)
		if err != nil {
			return err
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

// checkHistory reports interrupted file/commit updates without attempting to
// repair them. The later recovery layer decides how to reconcile those writes.
func (r *Repository) checkHistory(ctx context.Context) error {
	tree, err := r.git(ctx, nil, "ls-tree", "-rz", "HEAD")
	if err != nil {
		return err
	}
	tracked := map[string]bool{}
	for _, entry := range strings.Split(tree, "\x00") {
		if entry == "" {
			continue
		}
		meta, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[0] != "100644" || fields[1] != "blob" {
			return fmt.Errorf("invalid trace Git tree entry %q", entry)
		}
		if err := relative(name); err != nil {
			return err
		}
		tracked[name] = true
		data, err := r.readFile(name)
		if err != nil {
			return fmt.Errorf("trace history %s: %w", name, err)
		}
		hash := sha1.New()
		fmt.Fprintf(hash, "blob %d\x00", len(data))
		hash.Write(data)
		if fmt.Sprintf("%x", hash.Sum(nil)) != fields[2] {
			return fmt.Errorf("%s: trace files differ from committed history; reconciliation required", name)
		}
	}
	return r.walk(func(name string, entry fs.DirEntry) error {
		if !entry.IsDir() && !tracked[name] && (strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, "/workstream.json") || name == "project.json") {
			return fmt.Errorf("%s: trace file is not committed; reconciliation required", name)
		}
		return nil
	})
}
