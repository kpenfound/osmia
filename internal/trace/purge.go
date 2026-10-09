package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
)

const purgeFile = ".git/osmia-purge.json"

type purgeJournal struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Parent     string              `json:"parent"`
	Commit     string              `json:"commit"`
}

// PurgeWorkstream permanently removes a terminal workstream from the files
// and every Git revision. Other workstreams' revisions remain intact. The
// caller must durably retain its archive title before calling this method.
// It can run inside Serialize; the trace mutex fences reads and writes.
func (r *Repository) PurgeWorkstream(ctx context.Context, stream config.WorkstreamID) error {
	if err := config.CheckWorkstreamIDs(stream); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.recoverPurge(ctx); err != nil {
		return err
	}
	r.forget()
	if _, err := r.checkHistory(ctx); err != nil {
		return err
	}
	if _, err := r.dir.Stat("workstreams/" + string(stream)); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	records, _, err := r.scan()
	if err != nil {
		return err
	}
	dependencies, err := bases(records)
	if err != nil {
		return err
	}
	for _, base := range dependencies {
		if base.Base == stream {
			return fmt.Errorf("workstream is still a dependency")
		}
	}
	log, v, err := r.loadWorkflow(stream)
	if err != nil {
		return err
	}
	if state := v.states[FeatureSubject].Value; state != "delivered" && state != "abandoned" {
		return fmt.Errorf("only terminal workstreams can be purged")
	}
	for _, th := range log.Threads {
		if th.Active != "" {
			return fmt.Errorf("workstream has an active turn")
		}
		for _, q := range th.Turns {
			if q.CompletedAt.IsZero() && q.Response == nil {
				return fmt.Errorf("workstream has an unfinished turn")
			}
		}
	}
	for _, op := range v.operations {
		if op.Result == nil || !op.Acknowledged {
			return fmt.Errorf("workstream has a pending operation")
		}
	}
	for key := range r.attempts {
		if strings.HasPrefix(key, string(stream)+"/") {
			return fmt.Errorf("workstream has an operation attempt in flight")
		}
	}
	if err := r.checkGit(); err != nil {
		return err
	}
	refs, err := r.git(ctx, nil, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return err
	}
	if strings.TrimSpace(refs) != "refs/heads/main" {
		return fmt.Errorf("trace purge requires only the service-owned main reference")
	}
	parent, err := r.git(ctx, nil, "rev-parse", "refs/heads/main")
	if err != nil {
		return err
	}
	commits, err := r.git(ctx, nil, "rev-list", "--reverse", "--topo-order", parent)
	if err != nil {
		return err
	}
	mapped, trees := map[string]string{}, map[string]string{}
	for _, oid := range strings.Fields(commits) {
		raw, err := r.gitBytes(ctx, nil, "", "cat-file", "commit", oid)
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
				tree, ok := trees[value]
				if !ok {
					tree, err = r.purgeTree(ctx, value, []string{"workstreams", string(stream)})
					if err != nil {
						return err
					}
					trees[value] = tree
				}
				lines[i] = "tree " + tree
			case "parent":
				replacement, ok := mapped[value]
				if !ok {
					return fmt.Errorf("trace parent missing during purge")
				}
				lines[i] = "parent " + replacement
			}
		}
		rewritten := append([]byte(strings.Join(lines, "\n")+"\n\n"), message...)
		mapped[oid], err = r.git(ctx, rewritten, "hash-object", "-t", "commit", "-w", "--stdin")
		if err != nil {
			return err
		}
	}
	if err := r.syncObjects(); err != nil {
		return err
	}
	p := purgeJournal{stream, parent, mapped[parent]}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := r.writeFile(purgeFile, data); err != nil {
		return err
	}
	if err := r.boundary("purge-journal-written"); err != nil {
		return err
	}
	// The journal is the durable intent; recovery completes it even if the
	// process stops before publishing the rewritten reference.
	return r.recoverPurge(ctx)
}

func (r *Repository) purgeTree(ctx context.Context, tree string, parts []string) (string, error) {
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
		meta, name, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok {
			return "", fmt.Errorf("invalid trace tree")
		}
		if string(name) == parts[0] {
			changed = true
			if len(parts) == 1 {
				continue
			}
			fields := strings.Fields(string(meta))
			if len(fields) != 3 || fields[1] != "tree" {
				return "", fmt.Errorf("invalid workstream tree")
			}
			child, err := r.purgeTree(ctx, fields[2], parts[1:])
			if err != nil {
				return "", err
			}
			entry = []byte(fields[0] + " tree " + child + "\t" + string(name))
		}
		kept = append(kept, entry...)
		kept = append(kept, 0)
	}
	if !changed {
		return tree, nil
	}
	return r.git(ctx, kept, "mktree", "-z")
}

// recoverPurge resumes an authorized deletion before any trace read or write.
func (r *Repository) recoverPurge(ctx context.Context) error {
	data, err := r.readFile(purgeFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var p purgeJournal
	if err := decode(data, &p); err != nil {
		return err
	}
	if config.CheckWorkstreamIDs(p.Workstream) != nil || !objectID.MatchString(p.Parent) || !objectID.MatchString(p.Commit) {
		return fmt.Errorf("invalid trace purge journal")
	}
	if err := r.checkGit(); err != nil {
		return err
	}
	head, err := r.git(ctx, nil, "rev-parse", "refs/heads/main")
	if err != nil {
		return err
	}
	if head != p.Parent && head != p.Commit {
		return fmt.Errorf("trace changed during purge")
	}
	if err := r.writeFile(".git/refs/heads/main", []byte(p.Commit+"\n")); err != nil {
		return err
	}
	if err := r.boundary("purge-ref-published"); err != nil {
		return err
	}
	if err := r.dir.RemoveAll("workstreams/" + string(p.Workstream)); err != nil {
		return err
	}
	if err := syncDir(r.dir, "workstreams"); err != nil {
		return err
	}
	r.changed()
	r.forget()
	r.recordFiles, r.verified, r.workflows = nil, nil, nil
	r.gitMu.Lock()
	r.tree, r.treeHead, r.synced = nil, "", nil
	r.gitMu.Unlock()
	if err := r.boundary("purge-files-removed"); err != nil {
		return err
	}
	// Neither the index, reflogs nor packed unreachable objects may retain
	// the deleted output. Keep main loose, as normal publication requires it.
	for _, args := range [][]string{{"read-tree", p.Commit}, {"reflog", "expire", "--expire=now", "--all"}, {"-c", "gc.packRefs=false", "gc", "--prune=now"}} {
		if _, err := r.git(ctx, nil, args...); err != nil {
			return err
		}
	}
	if err := r.syncObjects(); err != nil {
		return err
	}
	if err := r.boundary("purge-objects-pruned"); err != nil {
		return err
	}
	if err := r.dir.Remove(purgeFile); err != nil {
		return err
	}
	return syncDir(r.dir, ".git")
}
