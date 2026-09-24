package trace

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
)

const publicationFile = ".git/osmia-publication.json"

type publication struct {
	Version int      `json:"version"`
	Parent  string   `json:"parent"`
	Commit  string   `json:"commit"`
	Paths   []string `json:"paths"`
	Removed []string `json:"removed,omitempty"`
}

func (r *Repository) boundary(name string) error {
	if r.failPublication != nil {
		return r.failPublication(name)
	}
	return nil
}

// publish prepares immutable objects before updating the single authoritative
// ref. The journal makes interrupted ordinary-file materialization recoverable.
// The repository lock must be held by the caller.
func (r *Repository) publish(ctx context.Context, files map[string][]byte) error {
	return r.publishTree(ctx, files, nil)
}

// publishTree is publish with paths that leave the tree: they are removed from
// the commit and, once the ref is published, from disk.
func (r *Repository) publishTree(ctx context.Context, files map[string][]byte, removed []string) error {
	parent, err := r.readFile(".git/refs/heads/main")
	if err != nil {
		return err
	}
	p := publication{Version: 1, Parent: strings.TrimSpace(string(parent))}
	if err := r.checkGit(); err != nil {
		return err
	}
	for name := range files {
		p.Paths = append(p.Paths, name)
	}
	sort.Strings(p.Paths)
	p.Removed = append([]string{}, removed...)
	sort.Strings(p.Removed)
	index := ".git/osmia-index-" + rand.Text()
	defer r.dir.Remove(index)
	defer r.dir.Remove(index + ".lock")
	git := func(input []byte, args ...string) (string, error) {
		b, err := r.gitBytes(ctx, input, r.directory+"/"+index, args...)
		return strings.TrimSpace(string(b)), err
	}
	if _, err := git(nil, "read-tree", p.Parent); err != nil {
		return err
	}
	for _, name := range p.Paths {
		oid, err := git(files[name], "hash-object", "-w", "--stdin")
		if err != nil {
			return err
		}
		if _, err := git(nil, "update-index", "--add", "--cacheinfo", "100644,"+oid+","+name); err != nil {
			return err
		}
	}
	for _, name := range p.Removed {
		if _, err := git(nil, "update-index", "--force-remove", name); err != nil {
			return err
		}
	}
	tree, err := git(nil, "write-tree")
	if err != nil {
		return err
	}
	p.Commit, err = git(nil, "commit-tree", tree, "-p", p.Parent, "-m", "Record workflow transaction")
	if err != nil {
		return err
	}
	if err := r.boundary("objects-written"); err != nil {
		return err
	}
	// Flush objects and their directory entries before publishing any reference.
	if err := r.syncObjects(); err != nil {
		return err
	}
	if err := r.boundary("objects-synced"); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := r.writeFile(publicationFile, data); err != nil {
		return err
	}
	if err := r.boundary("journal-written"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := r.readFile(".git/refs/heads/main")
	if err != nil {
		return err
	}
	if string(current) != string(parent) {
		return ErrConflict
	}
	if err := r.boundary("before-ref"); err != nil {
		return err
	}
	if err := r.writeFile(".git/refs/heads/main", []byte(p.Commit+"\n")); err != nil {
		return err
	}
	if err := r.boundary("ref-published"); err != nil {
		return err
	}
	// After publication cancellation cannot roll back a committed transaction.
	return r.recoverPublication(context.WithoutCancel(ctx))
}

// syncObjects flushes every object this handle has not flushed yet, and the
// directories that hold them. The first call flushes the whole store, which
// covers objects an earlier process wrote without publishing them.
func (r *Repository) syncObjects() error {
	r.gitMu.Lock()
	defer r.gitMu.Unlock()
	if r.synced == nil {
		r.synced = map[string]bool{}
	}
	var dirs, files []string
	changed := map[string]bool{}
	err := fs.WalkDir(r.dir.FS(), ".git/objects", func(name string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := r.checkedEntry(name, e); err != nil {
			return err
		}
		if e.IsDir() {
			dirs = append(dirs, name)
			return nil
		}
		if r.synced[name] {
			return nil
		}
		f, err := r.dir.Open(name)
		if err != nil {
			return err
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return err
		}
		files = append(files, name)
		for dir := path.Dir(name); dir != ".git"; dir = path.Dir(dir) {
			changed[dir] = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if !changed[dirs[i]] {
			continue
		}
		if err := syncDir(r.dir, dirs[i]); err != nil {
			return err
		}
	}
	for _, name := range files {
		r.synced[name] = true
	}
	return nil
}

var objectID = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (r *Repository) recoverPublication(ctx context.Context) error {
	data, err := r.readFile(publicationFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var p publication
	if err := decode(data, &p); err != nil {
		return err
	}
	if p.Version != 1 || !objectID.MatchString(p.Parent) || !objectID.MatchString(p.Commit) || len(p.Paths) == 0 {
		return fmt.Errorf("invalid workflow publication journal")
	}
	seen := map[string]bool{}
	for _, name := range append(append([]string{}, p.Paths...), p.Removed...) {
		if err := publicationPath(name); err != nil || seen[name] {
			return fmt.Errorf("invalid workflow publication path %q", name)
		}
		if err := r.checked(name); err != nil {
			return err
		}
		seen[name] = true
	}
	current, err := r.readFile(".git/refs/heads/main")
	if err != nil {
		return err
	}
	switch strings.TrimSpace(string(current)) {
	case p.Parent: // No ordinary files are changed before publication.
	case p.Commit:
		// Publication may have stopped after rename but before directory sync.
		// Make the observed ref durable before clearing its recovery journal.
		if err := syncDir(r.dir, ".git/refs/heads"); err != nil {
			return err
		}
		if err := r.boundary("recovery-ref-synced"); err != nil {
			return err
		}
		if err := r.checkGit(); err != nil {
			return err
		}
		for _, name := range p.Paths {
			data, err := r.gitBytes(ctx, nil, "", "cat-file", "blob", p.Commit+":"+name)
			if err != nil {
				return err
			}
			if err := r.writeFile(name, data); err != nil {
				return err
			}
			if err := r.boundary("materialized:" + path.Base(name)); err != nil {
				return err
			}
		}
		for _, name := range p.Removed {
			if err := r.dir.Remove(name); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := r.boundary("removed:" + path.Base(name)); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("workflow publication ref differs from both journal identities")
	}
	if err := r.boundary("before-journal-removal"); err != nil {
		return err
	}
	if err := r.dir.Remove(publicationFile); err != nil {
		return err
	}
	return syncDir(r.dir, ".git")
}

// publicationPath accepts the ordinary files the publication journal may
// materialize or remove: workflow, transition, question, shed, unit, final
// review, drift rebase and owned agent files of a workstream, private role notes, and project documents other than
// the charter.
func publicationPath(name string) error {
	parts := strings.Split(name, "/")
	switch {
	case parts[0] == "workstreams" && len(parts) >= 2:
		if _, err := config.ParseWorkstreamID(parts[1]); err != nil {
			return err
		}
		if len(parts) == 3 && (parts[2] == "workflow.json" || parts[2] == "events.jsonl" || parts[2] == "documents.jsonl" || parts[2] == "spec.md" || parts[2] == "plan.json" || parts[2] == "seal.json") {
			return nil
		}
		if len(parts) == 5 && parts[2] == "agents" && key(parts[3]) && (parts[4] == "identity.jsonl" || parts[4] == "log.jsonl") {
			return nil
		}
		if len(parts) == 5 && parts[2] == "questions" && key(parts[3]) && (parts[4] == "question.jsonl" || parts[4] == "rulings.jsonl") {
			return nil
		}
		if len(parts) == 5 && parts[2] == "amendments" && key(parts[3]) && parts[4] == "request.jsonl" {
			return nil
		}
		if len(parts) >= 5 && (amendmentDraftPath(parts[2:]) || amendmentRoundPath(parts[2:])) {
			return nil
		}
		if shedPath(parts[2:]) || unitPath(parts[2:]) || finalPath(parts[2:]) || driftPath(parts[2:]) || charterProposalPath(parts[2:]) {
			return nil
		}
	case len(parts) == 2 && parts[0] == "notes" && strings.HasSuffix(parts[1], ".md") && key(strings.TrimSuffix(parts[1], ".md")):
		return nil
	case name == "documents.jsonl" || name == EntitiesPath || name == "kb/sources.json":
		return nil
	case len(parts) == 2 && parts[0] == "kb" && strings.HasSuffix(parts[1], ".md") && key(strings.TrimSuffix(parts[1], ".md")):
		return nil
	}
	return fmt.Errorf("invalid workflow publication path %q", name)
}
