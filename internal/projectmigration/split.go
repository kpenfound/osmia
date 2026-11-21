package projectmigration

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/plan"
	"github.com/kpenfound/osmia/internal/trace"
)

// Split is one project the split frees from its group: the project, the
// project whose charter it takes (empty when it names none) and the pinned
// revision of that charter.
type Split struct {
	Project config.Project   `json:"project"`
	Source  config.ProjectID `json:"source"`
	Charter trace.Document   `json:"charter"`
}

type splitJournal struct {
	Version int       `json:"version"`
	At      time.Time `json:"at"`
	Copies  []Split   `json:"copies"`
}

func journalPath(root config.Root) string {
	return filepath.Join(root.String(), "project-splits.json")
}

// readJournal reads the journal of a split an earlier run began. found is
// false when there is none.
func readJournal(root config.Root) (journal splitJournal, found bool, err error) {
	data, err := os.ReadFile(journalPath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return splitJournal{}, false, nil
	}
	if err != nil {
		return splitJournal{}, false, err
	}
	if err := json.Unmarshal(data, &journal); err != nil {
		return splitJournal{}, false, err
	}
	if journal.Version != 1 {
		return splitJournal{}, false, errors.New("unsupported project split journal")
	}
	return journal, true, nil
}

// finishJournalled finishes the split an earlier run journalled, if any. It
// reads only the journal and the projects it names.
func (p *Pipeline) finishJournalled(ctx context.Context) error {
	journal, found, err := readJournal(p.cfg.Root)
	if err != nil || !found {
		return err
	}
	return p.apply(ctx, journal)
}

// discover finds the listed projects that name a group, pins their source
// charters, journals the split and carries it out. Only projects the top-level
// file lists are read or rewritten.
func (p *Pipeline) discover(ctx context.Context) error {
	if journal, found, err := readJournal(p.cfg.Root); err != nil {
		return err
	} else if found {
		return p.apply(ctx, journal)
	}
	journal := splitJournal{Version: 1, At: time.Now().UTC()}
	for _, name := range p.cfg.ActiveProjects {
		id, err := config.ParseProjectID(name)
		if err != nil {
			return err
		}
		path, err := p.cfg.Root.ProjectConfig(id)
		if err != nil {
			return err
		}
		source, present, err := config.ProjectGroupSource(path)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if id == source {
			return fmt.Errorf("project %s groups itself", id)
		}
		member, err := p.cfg.WithProject(id, p.home)
		if err != nil {
			return err
		}
		if source == "" {
			journal.Copies = append(journal.Copies, Split{Project: member.Project})
			continue
		}
		origin, err := p.cfg.WithProject(source, p.home)
		if err != nil {
			return err
		}
		if err := MigrateTrace(ctx, p.cfg.Root, source); err != nil {
			return err
		}
		r, err := trace.Open(p.cfg.Root, origin.Project)
		if err != nil {
			if r != nil {
				r.Close()
			}
			return err
		}
		charter, readErr := r.Charter(ctx)
		closeErr := r.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		journal.Copies = append(journal.Copies, Split{Project: member.Project, Source: source, Charter: charter})
	}
	if len(journal.Copies) == 0 {
		return nil
	}
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	path := journalPath(p.cfg.Root)
	if err := writeFileSynced(path, data); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if err := p.step("split-intent-written"); err != nil {
		return err
	}
	return p.apply(ctx, journal)
}

// apply carries out a journalled split: each copy's charter is reconciled by
// its pinned source revision and its group assignment removed, and the journal
// goes last, so a run interrupted anywhere finishes the same effects.
func (p *Pipeline) apply(ctx context.Context, journal splitJournal) error {
	for _, copy := range journal.Copies {
		if copy.Source != "" {
			if err := CopyCharter(ctx, p.cfg.Root, copy, journal.At); err != nil {
				return err
			}
			if err := p.step("split-charter-copied:" + string(copy.Project.ID)); err != nil {
				return err
			}
		}
		path, err := p.cfg.Root.ProjectConfig(copy.Project.ID)
		if err != nil {
			return err
		}
		if err := config.RemoveProjectGroup(path); err != nil {
			return err
		}
		if err := p.step("split-config-rewritten:" + string(copy.Project.ID)); err != nil {
			return err
		}
	}
	journalFile := journalPath(p.cfg.Root)
	if err := os.Remove(journalFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := syncDirectory(filepath.Dir(journalFile)); err != nil {
		return err
	}
	return p.step("split-intent-removed")
}

// SyncDirectory makes the entries of the directory at path durable.
func SyncDirectory(path string) error { return syncDirectory(path) }

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	err = dir.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}

// writeFileSynced replaces path with data through a synced temporary file
// beside it, so a reader sees the old contents or the new ones.
func writeFileSynced(path string, data []byte) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"-"+rand.Text()+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// MigrateTrace moves project id's trace to its database layout when it is
// not there yet.
func MigrateTrace(ctx context.Context, root config.Root, id config.ProjectID) error {
	directory, err := root.ProjectTrace(id)
	if err != nil {
		return err
	}
	return trace.Migrate(ctx, root, id, plan.DraftCheck(directory), nil)
}

// CopyCharter reconciles a pinned charter copy by its source revision: the
// destination gets one revision carrying the split's provenance, however many
// times this runs.
func CopyCharter(ctx context.Context, root config.Root, copy Split, at time.Time) error {
	if err := MigrateTrace(ctx, root, copy.Project.ID); err != nil {
		return err
	}
	r, err := trace.Open(root, copy.Project)
	if err != nil {
		if r != nil {
			r.Close()
		}
		return err
	}
	cause := fmt.Sprintf("split-project:%s:charter#%d", copy.Source, copy.Charter.Revision)
	docs, err := trace.Read[trace.Document](r, "")
	revision := 0
	found := false
	for _, d := range docs {
		if d.Path == "charter.md" {
			if d.Revision > revision {
				revision = d.Revision
			}
			if d.Cause == cause {
				found = true
			}
		}
	}
	if err == nil && !found {
		d := copy.Charter
		d.Project = copy.Project.ID
		d.Revision = revision + 1
		d.At = at
		d.Actor = trace.Actor{Kind: "service", ID: "osmia"}
		d.Cause = cause
		err = r.Append(ctx, d)
	}
	return errors.Join(err, r.Close())
}
