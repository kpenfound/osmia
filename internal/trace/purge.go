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

// PurgeProgress reports one phase of a PurgeWorkstreams call: preparing,
// rewriting history (with a 0..1 completion fraction once the number of
// commits to rewrite is known), publishing the rewritten result and pruning
// the objects it replaces. A nil PurgeProgress is a no-op.
type PurgeProgress func(phase string, fraction *float64)

func (p PurgeProgress) report(phase string, fraction *float64) {
	if p != nil {
		p(phase, fraction)
	}
}

// PurgeWorkstream removes one terminal workstream through PurgeWorkstreams.
func (r *Repository) PurgeWorkstream(ctx context.Context, stream config.WorkstreamID) error {
	return r.PurgeWorkstreams(ctx, []config.WorkstreamID{stream}, nil)
}

// PurgeWorkstreams permanently removes terminal workstreams in one history
// rewrite and prune. Retained workstreams' revisions remain intact. The
// caller must durably retain every archive title before calling this method.
//
// The rewrite is prepared against a known parent revision of refs/heads/main
// without holding the repository lock or operationMu: Git never modifies or
// removes an object once written, so adding the rewrite's new objects cannot
// corrupt a concurrent read or write. Only a short, locked critical section
// then checks that main still carries that revision, or rewrites the few
// commits landed on it since and carries them forward, before writing the
// purge journal and publishing. Pruning the objects the rewrite replaces
// runs afterward, still without the repository lock, fenced only against
// writes that create Git objects, for the short time pruning itself takes;
// a read never waits on it. purgeMu also keeps pruning from running at all
// while a rewrite is being prepared or published, so pruning only ever
// collects objects that were already unreachable before that rewrite began.
// progress, when not nil, is reported as each phase runs.
func (r *Repository) PurgeWorkstreams(ctx context.Context, streams []config.WorkstreamID, progress PurgeProgress) error {
	streams = slices.Compact(slices.Sorted(slices.Values(streams)))
	if err := config.CheckWorkstreamIDs(streams...); err != nil {
		return err
	}
	if len(streams) == 0 {
		return nil
	}
	// Finish whatever an earlier, interrupted call left pending before
	// preparing a new rewrite against a revision that assumes it is done.
	if err := r.FinishPurge(ctx, progress); err != nil {
		return err
	}
	progress.report("preparing", nil)
	if err := r.rewriteAndPublish(ctx, streams, progress); err != nil {
		return err
	}
	return r.FinishPurge(ctx, progress)
}

// rewriteAndPublish prepares the history rewrite and publishes it, holding
// purgeMu for the whole attempt. The rewrite's commits and trees are
// unreachable from any ref until publishPurge flips main to them; purgeMu
// keeps prunePurged's gc, which would otherwise be free to run concurrently
// without the repository lock, from collecting them first. It is released
// before returning, so the trailing FinishPurge call in PurgeWorkstreams,
// whose prunePurged step takes purgeMu itself, never contends with itself.
func (r *Repository) rewriteAndPublish(ctx context.Context, streams []config.WorkstreamID, progress PurgeProgress) error {
	r.purgeMu.Lock()
	defer r.purgeMu.Unlock()
	selected, parent, err := r.preparePurgeTargets(ctx, streams)
	if err != nil || len(selected) == 0 {
		return err
	}
	mapped, trees := map[string]string{}, map[string]string{}
	if err := r.rewritePurgedHistory(ctx, parent, selected, mapped, trees, progress); err != nil {
		return err
	}
	progress.report("publishing", nil)
	return r.publishPurge(ctx, streams, selected, parent, mapped, trees)
}

// preparePurgeTargets validates streams against the current trace and
// returns the ones that still exist as pending archives, with the Git
// revision of refs/heads/main their rewrite is prepared against. It holds
// the repository lock only for these fast checks, never for the rewrite.
func (r *Repository) preparePurgeTargets(ctx context.Context, streams []config.WorkstreamID) (map[config.WorkstreamID]bool, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.recoverPurge(ctx); err != nil {
		return nil, "", err
	}
	r.forget()
	if _, err := r.checkHistory(ctx); err != nil {
		return nil, "", err
	}
	selected := map[config.WorkstreamID]bool{}
	for _, stream := range streams {
		if _, err := r.dir.Stat("workstreams/" + string(stream)); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, "", err
		}
		selected[stream] = true
	}
	if len(selected) == 0 {
		return nil, "", nil
	}
	if err := r.validatePurgeable(selected); err != nil {
		return nil, "", err
	}
	if err := r.checkGit(); err != nil {
		return nil, "", err
	}
	refs, err := r.git(ctx, nil, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(refs) != "refs/heads/main" {
		return nil, "", fmt.Errorf("trace purge requires only the service-owned main reference")
	}
	parent, err := r.git(ctx, nil, "rev-parse", "refs/heads/main")
	if err != nil {
		return nil, "", err
	}
	return selected, parent, nil
}

// validatePurgeable requires r.mu. It refuses a purge whose selection would
// still retain a dependent of one of its workstreams, or whose workstreams
// are not all terminal, idle, and free of a pending operation or an
// operation attempt in flight.
func (r *Repository) validatePurgeable(selected map[config.WorkstreamID]bool) error {
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
	for stream := range selected {
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
	return nil
}

// rewritePurgedHistory rewrites every commit walked back from parent,
// dropping the directory of each stream in selected, into mapped. It takes
// no lock, as PurgeWorkstreams describes, and propagates ctx's cancellation
// into every Git command it runs.
func (r *Repository) rewritePurgedHistory(ctx context.Context, parent string, selected map[config.WorkstreamID]bool, mapped, trees map[string]string, progress PurgeProgress) error {
	progress.report("rewriting history", nil)
	if err := r.boundary("purge-rewriting"); err != nil {
		return err
	}
	commits, err := r.git(ctx, nil, "rev-list", "--reverse", "--topo-order", parent)
	if err != nil {
		return err
	}
	fields := strings.Fields(commits)
	for i, oid := range fields {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, ok := mapped[oid]; !ok {
			if err := r.rewriteCommit(ctx, oid, selected, mapped, trees); err != nil {
				return err
			}
		}
		fraction := float64(i+1) / float64(len(fields))
		progress.report("rewriting history", &fraction)
	}
	return r.syncObjects()
}

// rewriteCommit rewrites one commit's tree, dropping the directory of each
// stream in selected, and its parent links, recording the result in
// mapped[oid]. Every parent of oid must already be mapped.
func (r *Repository) rewriteCommit(ctx context.Context, oid string, selected map[config.WorkstreamID]bool, mapped, trees map[string]string) error {
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
	return err
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

// maxPurgeRevalidation bounds how many times publishPurge rewrites commits
// landed on main while the rewrite was being prepared, before it defers the
// purge to a later pass instead of contending with a busy trace indefinitely.
const maxPurgeRevalidation = 8

// publishPurge checks, in one short critical section, that main still
// carries the revision the rewrite was prepared against, or rewrites and
// carries forward the few commits landed on it since, then writes the purge
// journal. Publication itself (the reference swap) is left to FinishPurge,
// which every trace read and write already calls before it proceeds.
//
// It returns ErrConflict, losing no commit and changing nothing durable,
// when the trace keeps moving faster than the rewrite can follow, or when a
// commit carried forward is no longer purgeable (for example, because it
// made a new workstream depend on one being purged); the caller's next
// attempt, or the next scheduled pass, starts over from a fresh revision.
func (r *Repository) publishPurge(ctx context.Context, streams []config.WorkstreamID, selected map[config.WorkstreamID]bool, parent string, mapped, trees map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.recoverPurge(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.checkGit(); err != nil {
		return err
	}
	head, err := r.git(ctx, nil, "rev-parse", "refs/heads/main")
	if err != nil {
		return err
	}
	for attempt := 0; head != parent; attempt++ {
		if attempt >= maxPurgeRevalidation {
			return fmt.Errorf("%w: trace keeps changing faster than the purge can follow", ErrConflict)
		}
		if err := r.validatePurgeable(selected); err != nil {
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
		carried, err := r.git(ctx, nil, "rev-list", "--reverse", "--topo-order", parent+".."+head)
		if err != nil {
			return err
		}
		fields := strings.Fields(carried)
		if len(fields) == 0 {
			return fmt.Errorf("%w: trace main is not a descendant of the purge's prepared revision", ErrConflict)
		}
		for _, oid := range fields {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, ok := mapped[oid]; ok {
				continue
			}
			if err := r.rewriteCommit(ctx, oid, selected, mapped, trees); err != nil {
				return err
			}
		}
		parent = head
		if head, err = r.git(ctx, nil, "rev-parse", "refs/heads/main"); err != nil {
			return err
		}
	}
	if err := r.syncObjects(); err != nil {
		return err
	}
	p := purgeJournal{Workstreams: streams, Parent: head, Commit: mapped[head]}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := r.writeFile(purgeFile, data); err != nil {
		return err
	}
	return r.boundary("purge-journal-written")
}

// FinishPurge completes any purge whose journal remains after an earlier
// call to PurgeWorkstreams was interrupted: resolving main and the removed
// workstreams' directories under the repository lock, which every trace
// read and write needs before it proceeds, and then, without that lock,
// expiring the reflog and pruning now-unreachable objects, fenced only
// against writes that create Git objects. It is a no-op when no purge is
// pending, so a caller may call it on every pass to make sure a prune that
// stalled or was interrupted eventually completes even when no new
// workstream is ready to purge.
func (r *Repository) FinishPurge(ctx context.Context, progress PurgeProgress) error {
	r.mu.Lock()
	err := r.recoverPurge(ctx)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	return r.prunePurged(ctx, progress)
}

// recoverPurge resolves main and the purged workstreams' directories to a
// pending purge's result. It requires the repository lock, and recovers a
// purge interrupted at any point after its journal was written, whether by
// this process restarting or by an earlier call from this same handle.
// Calling it again once it has already resolved them, while the purge's
// objects are still being pruned, does no further work.
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
	changed := head == p.Parent
	if changed {
		if err := r.writeFile(".git/refs/heads/main", []byte(p.Commit+"\n")); err != nil {
			return err
		}
	}
	if err := r.boundary("purge-ref-published"); err != nil {
		return err
	}
	for _, stream := range p.Workstreams {
		name := "workstreams/" + string(stream)
		if _, err := r.dir.Lstat(name); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := r.dir.RemoveAll(name); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		if err := syncDir(r.dir, "workstreams"); err != nil {
			return err
		}
		r.changed()
		r.forget()
		r.recordFiles, r.verified, r.workflows = nil, nil, nil
		r.gitMu.Lock()
		r.tree, r.treeHead, r.synced = nil, "", nil
		r.gitMu.Unlock()
	}
	return r.boundary("purge-files-removed")
}

// prunePurged finishes a pending purge once its reference and files are
// already resolved: it expires the reflog and prunes now-unreachable
// objects, including the output the purged workstreams held, then clears
// the journal. Neither the index, the reflogs nor packed or unreachable
// objects may retain the deleted output.
//
// It takes no repository lock: resolving a pending purge for a read or
// write is recoverPurge's job, done under the lock before this runs, and
// needs nothing further from pruning. purgeMu and pruneMu fence it only
// against a concurrent rewrite's unpublished objects and against writes
// that create Git objects, for the time pruning itself takes, so one
// stalled indefinitely, as a test may do, still does not hold up a read, or
// a write that is not creating a Git object.
//
// The destructive work gc --prune=now would do in one opaque call is run as
// its own constituent steps instead, each reported as further pruning
// progress: no single step is left silent for longer than a prune of a
// realistically sized trace takes, which would otherwise show the backend
// as degraded while a healthy prune was still running. repack -a -d packs
// every reachable object into one new pack and drops the old ones, which
// drops any object the purge's rewrite left behind that is not reachable
// from the republished main; prune --expire=now then removes any
// unreachable object still loose. Together they remove exactly what a
// single gc --prune=now would have, with the same reachability result.
func (r *Repository) prunePurged(ctx context.Context, progress PurgeProgress) error {
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
	if !objectID.MatchString(p.Commit) {
		return fmt.Errorf("invalid trace purge journal")
	}
	progress.report("pruning", nil)
	r.purgeMu.Lock()
	defer r.purgeMu.Unlock()
	// prunePurged is the one caller that holds the prune fence rather than
	// waiting to acquire it against someone else holding it, so it passes a
	// context with no OwnerCommand marker: lockPruneMu always waits for it
	// unconditionally here, exactly as a plain mutex would.
	if err := r.lockPruneMu(ctx); err != nil {
		return err
	}
	defer r.unlockPruneMu()
	if err := ctx.Err(); err != nil {
		return err
	}
	// purgeMu and pruneMu are held from here through the destructive steps
	// below, so a test that stalls at this boundary observes the same
	// exclusion a concurrent rewrite's unpublished objects and a concurrent
	// object-creating write rely on: both block here, rather than racing the
	// steps below, for as long as the stall lasts.
	if err := r.boundary("purge-pruning"); err != nil {
		return err
	}
	steps := [][]string{
		{"read-tree", p.Commit},
		{"reflog", "expire", "--expire=now", "--all"},
		{"repack", "-a", "-d"},
		{"prune", "--expire=now"},
	}
	for i, args := range steps {
		fraction := float64(i) / float64(len(steps))
		progress.report("pruning", &fraction)
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
