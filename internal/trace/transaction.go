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
	parent, err := r.readFile(".git/refs/heads/main")
	if err != nil {
		return err
	}
	p := publication{Version: 1, Parent: strings.TrimSpace(string(parent))}
	for name := range files {
		p.Paths = append(p.Paths, name)
	}
	sort.Strings(p.Paths)
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

func (r *Repository) syncObjects() error {
	var dirs []string
	err := fs.WalkDir(r.dir.FS(), ".git/objects", func(name string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := r.checked(name); err != nil {
			return err
		}
		if e.IsDir() {
			dirs = append(dirs, name)
			return nil
		}
		f, err := r.dir.Open(name)
		if err != nil {
			return err
		}
		return errors.Join(f.Sync(), f.Close())
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncDir(r.dir, dirs[i]); err != nil {
			return err
		}
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
	for _, name := range p.Paths {
		parts := strings.Split(name, "/")
		if len(parts) != 3 || parts[0] != "workstreams" || (parts[2] != "workflow.json" && parts[2] != "events.jsonl") || seen[name] {
			return fmt.Errorf("invalid workflow publication path %q", name)
		}
		if _, err := config.ParseWorkstreamID(parts[1]); err != nil {
			return err
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
