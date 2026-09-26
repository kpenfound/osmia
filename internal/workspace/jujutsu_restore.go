package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// checkpointsName names the directory in the repository's .jj directory that
// holds the checkpoints of operations in progress.
const checkpointsName = "osmia-checkpoints"

// checkpoint is the repository's state before one attempt of an operation:
// the operation-log entry it starts from and what the provider recorded
// about each workspace then, by name.
type checkpoint struct {
	Operation  string              `json:"operation"`
	Entry      string              `json:"entry"`
	Recorded   time.Time           `json:"recorded"`
	Workspaces map[string]metadata `json:"workspaces"`
}

func (j *Jujutsu) checkpoints() string {
	return filepath.Join(j.repository(), ".jj", checkpointsName)
}

func (j *Jujutsu) checkpointPath(operation string) string {
	sum := sha256.Sum256([]byte(operation))
	return filepath.Join(j.checkpoints(), hex.EncodeToString(sum[:16])+".json")
}

// Checkpoint snapshots every workspace of the repository, so the files each
// holds are in its working-copy commit, then durably records the
// operation-log entry the repository is at and the provider's metadata of
// each workspace, the replay in progress there included. The snapshots
// commit at the given time. A checkpoint recorded before for the operation
// is replaced. A repository not made yet records none: nothing of it is
// there to restore.
func (j *Jujutsu) Checkpoint(ctx context.Context, operation string, at time.Time) error {
	if exists, err := j.initialized(); err != nil || !exists {
		return err
	}
	live, err := j.live(ctx)
	if err != nil {
		return err
	}
	c := checkpoint{Operation: operation, Recorded: time.Now().UTC(), Workspaces: map[string]metadata{}}
	for _, name := range live {
		if err := j.snapshotLive(ctx, j.path(name), stamp(at)); err != nil {
			return err
		}
		meta, err := readMetadata(j.path(name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		c.Workspaces[name] = meta
	}
	if c.Entry, err = j.run(ctx, j.repository(), nil, "--ignore-working-copy", "operation", "log", "--no-graph", "--limit", "1", "--template", "id"); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(j.checkpoints(), 0700); err != nil {
		return err
	}
	return writeDurably(j.checkpointPath(operation), data)
}

// Settle drops the operation's checkpoint.
func (j *Jujutsu) Settle(_ context.Context, operation string) error {
	if err := os.Remove(j.checkpointPath(operation)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Recover restores the repository to the oldest operation-log entry a
// checkpoint still records, drops every checkpoint and returns the
// operations they were taken for, oldest first. No mason edit is lost: every
// workspace is snapshotted before the restore, so what its files held stays
// in the operation log, and one whose files the restore takes back to the
// checkpoint gets the files it held then, which the checkpoint's snapshot
// holds. Each workspace gets back the metadata recorded then, and
// the repository imports the clone's branches as they are, since a restore
// moves no branch of the clone. A workspace, with no replay in progress,
// whose branch moved while it held nothing of its own, or to a commit
// holding exactly its files, as a snapshot does, is then moved onto the
// branch. A workspace a checkpoint does not know is no longer one of the
// repository's, as Workspace reports.
func (j *Jujutsu) Recover(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(j.checkpoints())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var found []checkpoint
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(j.checkpoints(), entry.Name()))
		if err != nil {
			return nil, err
		}
		var c checkpoint
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("checkpoint %s: %w", entry.Name(), err)
		}
		found = append(found, c)
	}
	if len(found) == 0 {
		return nil, nil
	}
	slices.SortStableFunc(found, func(a, b checkpoint) int { return a.Recorded.Compare(b.Recorded) })
	oldest := found[0]
	if err := j.restore(ctx, oldest); err != nil {
		return nil, fmt.Errorf("restore the workspaces of %s to operation %s: %w", j.Directory, oldest.Entry, err)
	}
	var operations []string
	for _, c := range found {
		if err := j.Settle(ctx, c.Operation); err != nil {
			return nil, err
		}
		operations = append(operations, c.Operation)
	}
	return operations, nil
}

// restore restores the repository to the checkpoint as Recover describes.
func (j *Jujutsu) restore(ctx context.Context, c checkpoint) error {
	live, err := j.live(ctx)
	if err != nil {
		return err
	}
	for _, name := range live {
		if err := j.snapshotLive(ctx, j.path(name), nil); err != nil {
			return err
		}
	}
	if _, err := j.run(ctx, j.repository(), nil, "--ignore-working-copy", "operation", "restore", c.Entry); err != nil {
		return err
	}
	if live, err = j.live(ctx); err != nil {
		return err
	}
	for _, name := range live {
		if meta, ok := c.Workspaces[name]; ok {
			if err := writeMetadata(j.path(name), meta); err != nil {
				return err
			}
		}
		if _, err := j.run(ctx, j.path(name), nil, "workspace", "update-stale"); err != nil {
			return err
		}
	}
	if err := j.importBranches(ctx); err != nil {
		return err
	}
	for _, name := range live {
		if err := j.realign(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// realign moves the workspace named name onto the commit its branch points
// at when the branch moved from under it and the move loses nothing of the
// workspace's: it holds nothing of its own, or exactly the files of that
// commit. A workspace replaying, or with no branch, stays where it is.
func (j *Jujutsu) realign(ctx context.Context, name string) error {
	meta, err := readMetadata(j.path(name))
	if errors.Is(err, os.ErrNotExist) || (err == nil && meta.Replay != nil) {
		return nil
	}
	if err != nil {
		return err
	}
	head, found, err := j.git().Branch(ctx, meta.Branch)
	if err != nil || !found {
		return err
	}
	w := j.worktree(name, meta.Branch)
	current, err := j.describe(ctx, w.Path, false, "@")
	if err != nil || slices.Equal(current.parents, []string{head}) {
		return err
	}
	if !current.empty {
		same, err := j.sameTree(ctx, current.commit, head)
		if err != nil || !same {
			return err
		}
	}
	return j.checkout(ctx, w, head)
}

// sameTree reports whether two commits hold the same files.
func (j *Jujutsu) sameTree(ctx context.Context, a, b string) (bool, error) {
	g := j.git()
	ta, err := g.run(ctx, "rev-parse", "--verify", "--end-of-options", a+"^{tree}")
	if err != nil {
		return false, err
	}
	tb, err := g.run(ctx, "rev-parse", "--verify", "--end-of-options", b+"^{tree}")
	return ta == tb, err
}

// live returns the names of the repository's workspaces whose directories
// are there.
func (j *Jujutsu) live(ctx context.Context) ([]string, error) {
	names, err := j.registered(ctx)
	if err != nil {
		return nil, err
	}
	var live []string
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(j.path(name), ".jj")); err == nil {
			live = append(live, name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return live, nil
}

// snapshotLive records the files of the workspace at path in its
// working-copy commit. A workspace a jj command stopped updating after its
// operation was recorded is stale: that command snapshotted its files first,
// so they are in the operation log already, and it is left for the restore
// to update.
func (j *Jujutsu) snapshotLive(ctx context.Context, path string, settings []string) error {
	_, err := j.run(ctx, path, settings, "util", "snapshot")
	var jjErr *JJError
	if errors.As(err, &jjErr) && strings.Contains(jjErr.Stderr, "working copy is stale") {
		return nil
	}
	return err
}

// writeDurably replaces the file at path with data in one rename, once data
// is on disk.
func writeDurably(path string, data []byte) error {
	f, err := os.OpenFile(path+".new", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(path+".new", path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
