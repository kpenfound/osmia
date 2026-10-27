package service

import (
	"context"
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

type projectSplit struct {
	Project config.Project   `json:"project"`
	Source  config.ProjectID `json:"source"`
	Charter trace.Document   `json:"charter"`
}
type projectSplits struct {
	Version int            `json:"version"`
	At      time.Time      `json:"at"`
	Copies  []projectSplit `json:"copies"`
}

// splitProjects pins the source charter before changing any destination. The
// journal survives config removal and each copied revision carries a unique
// provenance cause, so restart can reconcile every effect independently.
func splitProjects(ctx context.Context, cfg *config.Config, home string, step func(string) error) error {
	journal := filepath.Join(cfg.Root.String(), "project-splits.json")
	data, err := os.ReadFile(journal)
	var pending projectSplits
	if err == nil {
		if err := json.Unmarshal(data, &pending); err != nil {
			return err
		}
		if pending.Version != 1 {
			return errors.New("unsupported project split journal")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		pending = projectSplits{Version: 1, At: time.Now().UTC()}
		paths, err := filepath.Glob(filepath.Join(cfg.Root.String(), "projects", "*", "config.toml"))
		if err != nil {
			return err
		}
		for _, path := range paths {
			source, present, err := config.ProjectGroupSource(path)
			if err != nil {
				return err
			}
			if !present {
				continue
			}
			id, err := config.ParseProjectID(filepath.Base(filepath.Dir(path)))
			if err != nil {
				return err
			}
			if id == source {
				return fmt.Errorf("project %s groups itself", id)
			}
			memberCfg, err := cfg.AddProject(id, home)
			if err != nil {
				return err
			}
			if source == "" {
				pending.Copies = append(pending.Copies, projectSplit{Project: memberCfg.For(id).Project})
				continue
			}
			sourceCfg, err := cfg.AddProject(source, home)
			if err != nil {
				return err
			}
			if err := migrateSplitTrace(ctx, cfg.Root, source); err != nil {
				return err
			}
			r, err := trace.Open(cfg.Root, sourceCfg.For(source).Project)
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
			pending.Copies = append(pending.Copies, projectSplit{Project: memberCfg.For(id).Project, Source: source, Charter: charter})
		}
		if len(pending.Copies) == 0 {
			return nil
		}
		data, err := json.Marshal(pending)
		if err != nil {
			return err
		}
		if err := writeFileSynced(journal, data); err != nil {
			return err
		}
		if err := syncSplitDirectory(filepath.Dir(journal)); err != nil {
			return err
		}
		if err := step("split-intent-written"); err != nil {
			return err
		}
	}
	for _, copy := range pending.Copies {
		if copy.Source != "" {
			if err := copySplitCharter(ctx, cfg.Root, copy, pending.At); err != nil {
				return err
			}
			if err := step("split-charter-copied:" + string(copy.Project.ID)); err != nil {
				return err
			}

		}
		path, err := cfg.Root.ProjectConfig(copy.Project.ID)
		if err != nil {
			return err
		}
		if err := config.RemoveProjectGroup(path); err != nil {
			return err
		}
		if err := step("split-config-rewritten:" + string(copy.Project.ID)); err != nil {
			return err
		}
	}
	if err := os.Remove(journal); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := syncSplitDirectory(filepath.Dir(journal)); err != nil {
		return err
	}
	return step("split-intent-removed")
}

func syncSplitDirectory(path string) error {
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

func migrateSplitTrace(ctx context.Context, root config.Root, id config.ProjectID) error {
	directory, err := root.ProjectTrace(id)
	if err != nil {
		return err
	}
	return trace.Migrate(ctx, root, id, plan.DraftCheck(directory), nil)
}

// copySplitCharter reconciles a pinned copy by its source revision.
func copySplitCharter(ctx context.Context, root config.Root, copy projectSplit, at time.Time) error {
	if err := migrateSplitTrace(ctx, root, copy.Project.ID); err != nil {
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
