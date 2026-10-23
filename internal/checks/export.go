package checks

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// prepareIdentity builds metadata separately so failure leaves the provider's
// pinned export untouched. It never writes the export's files or source clone.
func prepareIdentity(ctx context.Context, dir string, env []string, identity Identity) error {
	if len(identity.Candidate) != 40 && len(identity.Candidate) != 64 {
		return fmt.Errorf("candidate is not a full Git commit ID")
	}
	for _, c := range identity.Candidate {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return fmt.Errorf("candidate is not a full Git commit ID")
		}
	}
	remote, err := url.Parse(identity.Remote)
	if err != nil || remote.Scheme != "https" || remote.Host == "" || remote.User != nil || remote.RawQuery != "" || remote.Fragment != "" {
		return fmt.Errorf("upstream must be a credential-free HTTPS URL")
	}
	if !filepath.IsAbs(identity.Repository) {
		return fmt.Errorf("candidate source must be a local absolute clone path")
	}
	repo, err := os.MkdirTemp(filepath.Dir(dir), ".osmia-check-identity-")
	if err != nil {
		return fmt.Errorf("create identity metadata: %w", err)
	}
	defer os.RemoveAll(repo)
	run := func(operation string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			// Arguments and Git diagnostics can contain source repository
			// configuration. The operation alone is enough to explain fallback.
			return "", fmt.Errorf("git identity preparation (%s): %w", operation, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	format := "sha1"
	if len(identity.Candidate) == 64 {
		format = "sha256"
	}
	if _, err := run("init", "init", "--quiet", "--template=", "--object-format="+format, repo); err != nil {
		return err
	}
	git := func(args ...string) (string, error) {
		return run(args[0], append([]string{"-C", repo}, args...)...)
	}
	if _, err := git("check-ref-format", "refs/heads/"+identity.Branch); err != nil || identity.Branch == "" {
		return fmt.Errorf("candidate branch is not a valid Git branch")
	}
	if _, err := git("fetch", "--quiet", "--depth=1", "--no-tags", "--", identity.Repository, identity.Candidate); err != nil {
		return fmt.Errorf("candidate commit is unavailable in the workspace clone: %w", err)
	}
	head, err := git("rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil || head != identity.Candidate {
		return fmt.Errorf("workspace commit does not match the candidate ID")
	}
	if _, err := git("update-ref", "refs/heads/"+identity.Branch, head); err != nil {
		return err
	}
	if _, err := git("symbolic-ref", "HEAD", "refs/heads/"+identity.Branch); err != nil {
		return err
	}
	if _, err := git("remote", "add", "origin", identity.Remote); err != nil {
		return err
	}
	tags, err := run("tag", "-C", identity.Repository, "tag", "--points-at", identity.Candidate)
	if err != nil {
		return err
	}
	for _, tag := range strings.Fields(tags) {
		ref := "refs/tags/" + tag
		if _, err := git("fetch", "--quiet", "--depth=1", "--no-tags", "--", identity.Repository, ref+":"+ref); err != nil {
			return err
		}
		target, err := git("rev-parse", "--verify", ref+"^{commit}")
		if err != nil || target != head {
			return fmt.Errorf("candidate tag changed during export preparation")
		}
	}
	// Index the existing export, including ignored files, to verify its entire
	// tree. No checkout or reset can replace the pinned files on fallback.
	worktree := func(args ...string) (string, error) {
		return run(args[0], append([]string{"-C", dir, "--git-dir=" + filepath.Join(repo, ".git"), "--work-tree=" + dir}, args...)...)
	}
	if _, err := worktree("add", "--all", "--force", "--", "."); err != nil {
		return err
	}
	tree, err := worktree("write-tree")
	if err != nil {
		return err
	}
	expected, err := git("rev-parse", "HEAD^{tree}")
	if err != nil || tree != expected {
		return fmt.Errorf("exported tree does not match the candidate commit")
	}
	// Listing may already have initialized an anonymous repository here.
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		return fmt.Errorf("remove anonymous export metadata: %w", err)
	}
	if err := os.Rename(filepath.Join(repo, ".git"), filepath.Join(dir, ".git")); err != nil {
		return fmt.Errorf("install candidate export metadata: %w", err)
	}
	return nil
}
