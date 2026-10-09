package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
)

const purgeFile = ".git/osmia-purge.json"

type purgeJournal struct {
	Workstream  config.WorkstreamID   `json:"workstream,omitempty"`
	Workstreams []config.WorkstreamID `json:"workstreams,omitempty"`
	Parent      string                `json:"parent"`
	Commit      string                `json:"commit"`
}

// PurgeWorkstream removes one terminal workstream through PurgeWorkstreams.
func (r *Repository) PurgeWorkstream(ctx context.Context, stream config.WorkstreamID) error {
	return r.PurgeWorkstreams(ctx, []config.WorkstreamID{stream})
}

// PurgeWorkstreams permanently removes terminal workstreams in one history
// rewrite and prune. Retained workstreams' revisions remain intact. The caller
// must durably retain every archive title before calling this method.
// It can run inside Serialize; the trace mutex fences reads and writes.
func (r *Repository) PurgeWorkstreams(ctx context.Context, streams []config.WorkstreamID) error {
	streams = slices.Compact(slices.Sorted(slices.Values(streams)))
	if err := config.CheckWorkstreamIDs(streams...); err != nil {
		return err
	}
	if len(streams) == 0 {
		return nil
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
	selected := map[config.WorkstreamID]bool{}
	for _, stream := range streams {
		if _, err := r.dir.Stat("workstreams/" + string(stream)); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		selected[stream] = true
	}
	if len(selected) == 0 {
		return nil
	}
	records, _, err := r.scan()
	if err != nil {
		return err
	}
	dependencies, err := bases(records)
	if err != nil {
		return err
	}
	for stream, base := range dependencies {
		if selected[base.Base] && !selected[stream] {
			return fmt.Errorf("workstream is still a dependency")
		}
	}
	for _, stream := range streams {
		if !selected[stream] {
			continue
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
				tree, err := r.purgeTree(ctx, value, selected, true, trees)
				if err != nil {
					return err
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
	p := purgeJournal{Workstreams: streams, Parent: parent, Commit: mapped[parent]}
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

func (r *Repository) purgeTree(ctx context.Context, tree string, selected map[config.WorkstreamID]bool, root bool, cache map[string]string) (string, error) {
	key := fmt.Sprintf("%t/%s", root, tree)
	if rewritten, ok := cache[key]; ok {
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
		meta, name, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok {
			return "", fmt.Errorf("invalid trace tree")
		}
		if !root && selected[config.WorkstreamID(name)] {
			changed = true
			continue
		}
		if root && string(name) == "workstreams" {
			fields := strings.Fields(string(meta))
			if len(fields) != 3 || fields[1] != "tree" {
				return "", fmt.Errorf("invalid workstream tree")
			}
			child, err := r.purgeTree(ctx, fields[2], selected, false, cache)
			if err != nil {
				return "", err
			}
			changed = changed || child != fields[2]
			entry = []byte(fields[0] + " tree " + child + "\t" + string(name))
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
	cache[key] = result
	return result, nil
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
	if p.Workstream != "" {
		if len(p.Workstreams) != 0 {
			return fmt.Errorf("invalid trace purge journal")
		}
		p.Workstreams = []config.WorkstreamID{p.Workstream}
	}
	if len(p.Workstreams) == 0 || config.CheckWorkstreamIDs(p.Workstreams...) != nil || !objectID.MatchString(p.Parent) || !objectID.MatchString(p.Commit) {
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
	for _, stream := range p.Workstreams {
		if err := r.dir.RemoveAll("workstreams/" + string(stream)); err != nil {
			return err
		}
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
