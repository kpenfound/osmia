package trace

import (
	"bytes"
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
	r.forget()
	if _, err := r.checkHistory(ctx); err != nil {
		return err
	}
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
	blobs := map[string]string{}
	for _, name := range p.Paths {
		if blobs[name], err = git(files[name], "hash-object", "-w", "--stdin"); err != nil {
			return err
		}
	}
	if _, err := git(indexInfo(p.Paths, blobs, p.Removed), "update-index", "-z", "--index-info"); err != nil {
		return err
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
	if err := r.finishPublication(context.WithoutCancel(ctx), &published{commit: p.Commit, files: files}); err != nil {
		return err
	}
	r.advanceTree(p.Parent, p.Commit, blobs, p.Removed)
	r.observed(Commit{Paths: append(p.Paths, p.Removed...), Content: files})
	return nil
}

// indexInfo is the NUL-terminated update-index --index-info input that sets
// each of paths to its blob and removes each of removed, in that order.
func indexInfo(paths []string, blobs map[string]string, removed []string) []byte {
	var b bytes.Buffer
	for _, name := range paths {
		fmt.Fprintf(&b, "100644 %s\t%s\x00", blobs[name], name)
	}
	for _, name := range removed {
		fmt.Fprintf(&b, "0 %s\t%s\x00", strings.Repeat("0", 40), name)
	}
	return b.Bytes()
}

// syncObjects flushes every object this handle has not flushed yet, and the
// directories that hold them. The first call flushes the whole store, which
// covers objects an earlier process wrote without publishing them. It follows
// checkGit in the same publication, so it inspects only the objects it flushes.
func (r *Repository) syncObjects() error {
	r.gitMu.Lock()
	defer r.gitMu.Unlock()
	if r.synced == nil {
		r.synced = map[string]bool{}
	}
	var dirs, files []string
	changed := map[string]bool{}
	err := r.walkDir(".git/objects", func(parent *os.Root, name string, e fs.DirEntry) error {
		if e.IsDir() {
			dirs = append(dirs, name)
			return nil
		}
		// The publication's checkGit inspected the objects already flushed.
		if r.synced[name] {
			return nil
		}
		if err := checkedEntry(parent, name, e); err != nil {
			return err
		}
		f, err := parent.Open(e.Name())
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
	return r.finishPublication(ctx, nil)
}

// published is a publication this handle has just committed, with the bytes
// of each file it committed.
type published struct {
	commit string
	files  map[string][]byte
}

// finishPublication completes the journaled publication. When the journal
// records own, the committed bytes come from own; otherwise they are read
// from Git.
func (r *Repository) finishPublication(ctx context.Context, own *published) error {
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
		files := map[string][]byte{}
		if own != nil && own.commit == p.Commit {
			files = own.files
		}
		for _, name := range p.Paths {
			if _, ok := files[name]; !ok {
				files = nil
				break
			}
		}
		if files == nil {
			if err := r.checkGit(); err != nil {
				return err
			}
		}
		for _, name := range p.Paths {
			data, ok := files[name]
			if !ok {
				if data, err = r.gitBytes(ctx, nil, "", "cat-file", "blob", p.Commit+":"+name); err != nil {
					return err
				}
			}
			if err := r.writeFile(name, data); err != nil {
				return err
			}
			if err := r.boundary("materialized:" + path.Base(name)); err != nil {
				return err
			}
		}
		for _, name := range p.Removed {
			r.changed()
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
// review, drift rebase and owned agent files of a workstream, private role
// notes, project notices, project documents, and the base and own
// pull-request-outcome observations of a workstream.
func publicationPath(name string) error {
	parts := strings.Split(name, "/")
	switch {
	case parts[0] == "workstreams" && len(parts) >= 2:
		if _, err := config.ParseWorkstreamID(parts[1]); err != nil {
			return err
		}
		if len(parts) == 3 && (parts[2] == "workflow.json" || parts[2] == "events.jsonl" || parts[2] == "documents.jsonl" || parts[2] == "spec.md" || parts[2] == "plan.json" || parts[2] == "seal.json" || (parts[2] == "base.json" || parts[2] == "base-observation.json" || parts[2] == "own-pull-request-outcome.json")) {
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
		if shedPath(parts[2:]) || unitPath(parts[2:]) || finalPath(parts[2:]) || driftPath(parts[2:]) || toolPath(parts[2:]) || judgmentPath(parts[2:]) || sealingPath(parts[2:]) || charterProposalPath(parts[2:]) {
			return nil
		}
	case len(parts) == 2 && parts[0] == "notes" && strings.HasSuffix(parts[1], ".md") && key(strings.TrimSuffix(parts[1], ".md")):
		return nil
	case memoryPath(parts):
		return nil
	case name == "charter.md":
		return nil
	case len(parts) == 2 && parts[0] == "notices" && strings.HasSuffix(parts[1], ".json") && key(strings.TrimSuffix(parts[1], ".json")):
		return nil
	case name == "documents.jsonl" || name == EntitiesPath || name == "kb/sources.json":
		return nil
	case len(parts) == 2 && parts[0] == "kb" && strings.HasSuffix(parts[1], ".md") && key(strings.TrimSuffix(parts[1], ".md")):
		return nil
	}
	return fmt.Errorf("invalid workflow publication path %q", name)
}
