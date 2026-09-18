package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/workspace"
)

const (
	masonRole = "mason"
	// unitsDirectory is the directory under the root holding the unit
	// workspaces, by project, workstream and unit.
	unitsDirectory = "units"
)

// unitBranch names the branch of a unit's workspace. It lies outside osmia/,
// where the workstream's feature branch osmia/<workstream> already is a ref.
func unitBranch(stream config.WorkstreamID, unit string) string {
	return "osmia-unit/" + string(stream) + "/" + unit
}

// unitWorkspaces are the workspaces of a project's units: one Git worktree per
// workstream and unit, <root>/units/<project>/<workstream>/<unit> on
// unitBranch, created from the workstream's feature branch. Nothing here
// removes one, so a unit's work stays in its workspace between turns and
// across restarts. They are also the workspaces a mason turn is lent: the
// turn works on a copy without VCS metadata, which capture copies back.
type unitWorkspaces struct{ git *workspace.Git }

var _ coreadapter.Workspaces = unitWorkspaces{}

func (s *Service) unitWorkspaces() unitWorkspaces { return newUnitWorkspaces(s.current()) }

// newUnitWorkspaces returns the unit workspaces of the configured project.
func newUnitWorkspaces(cfg *config.Config) unitWorkspaces {
	return unitWorkspaces{git: &workspace.Git{Clone: cfg.Project.Clone, Directory: filepath.Join(cfg.Root.String(), unitsDirectory, string(cfg.Project.ID))}}
}

func unitName(stream config.WorkstreamID, unit string) string {
	return string(stream) + "/" + unit
}

// open returns the unit's workspace and the feature branch commit it descends
// from, creating the workspace from the tip of the feature branch when the
// clone has none. A workspace already there is returned with what it holds.
func (u unitWorkspaces) open(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, error) {
	feature := featureBranch(stream)
	if _, exists, err := u.git.Branch(ctx, feature); err != nil {
		return workspace.Worktree{}, "", err
	} else if !exists {
		return workspace.Worktree{}, "", fmt.Errorf("the clone has no feature branch %s", feature)
	}
	acquired, err := u.git.Acquire(ctx, vcs.Request{Name: unitName(stream, unit), Ref: feature, Branch: unitBranch(stream, unit)})
	if err != nil {
		return workspace.Worktree{}, "", err
	}
	w := acquired.(workspace.Worktree)
	base, err := u.git.MergeBase(ctx, feature, w.Branch)
	if err != nil {
		return workspace.Worktree{}, "", err
	}
	return w, base, nil
}

// find returns the unit's workspace and the feature branch commit it
// descends from when the clone has it, and creates nothing.
func (u unitWorkspaces) find(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, bool, error) {
	w, found, err := u.git.Workspace(ctx, unitName(stream, unit))
	if err != nil || !found {
		return workspace.Worktree{}, "", false, err
	}
	base, err := u.git.MergeBase(ctx, featureBranch(stream), w.Branch)
	if err != nil {
		return workspace.Worktree{}, "", false, err
	}
	return w, base, true, nil
}

// snapshot commits what the unit's workspace holds as its candidate, a
// commit that descends from the feature branch, and returns it.
func (u unitWorkspaces) snapshot(ctx context.Context, stream config.WorkstreamID, unit string) (string, error) {
	w, _, found, err := u.find(ctx, stream, unit)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("unit %s of workstream %s has no workspace", unit, stream)
	}
	return u.git.Snapshot(ctx, w, featureBranch(stream))
}

// Acquire lends a mason turn its unit's workspace, which must exist. The
// lease keeps the workspace: it holds the unit's work until the unit lands.
func (u unitWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	if err := ctx.Err(); err != nil {
		return coreadapter.WorkspaceLease{}, err
	}
	scope := req.Scope
	if scope.Role != masonRole || scope.Workstream == "" || scope.Unit == "" {
		return coreadapter.WorkspaceLease{}, errors.New("a unit workspace is lent to a mason turn of a unit alone")
	}
	stream := config.WorkstreamID(scope.Workstream)
	w, _, found, err := u.find(ctx, stream, scope.Unit)
	if err != nil {
		return coreadapter.WorkspaceLease{}, err
	}
	if !found {
		return coreadapter.WorkspaceLease{}, fmt.Errorf("unit %s of workstream %s has no workspace", scope.Unit, stream)
	}
	return coreadapter.WorkspaceLease{Workspace: coreadapter.Workspace{ID: unitName(stream, scope.Unit), Directory: w.Path, Access: req.Access}, Lease: noLease{}}, nil
}

// paths selects every entry at the top of a unit's workspace but its VCS
// metadata, so a mason turn's view is all of the workspace's files.
func (u unitWorkspaces) paths(w workspace.Worktree) ([]string, error) {
	entries, err := os.ReadDir(w.Path)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if !isolation.VCSMetadata(entry.Name()) {
			paths = append(paths, entry.Name())
		}
	}
	return paths, nil
}

// selection selects the whole of the turn's unit workspace, but its VCS
// metadata, as the mason turn's view, run with execution.
func (u unitWorkspaces) selection(ctx context.Context, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	stream := config.WorkstreamID(scope.Workstream)
	w, _, found, err := u.find(ctx, stream, scope.Unit)
	if err != nil {
		return isolation.Selection{}, err
	}
	if !found {
		return isolation.Selection{}, fmt.Errorf("unit %s of workstream %s has no workspace", scope.Unit, stream)
	}
	paths, err := u.paths(w)
	return isolation.Selection{Paths: paths, Execution: execution}, err
}

// capture copies a mason turn's view back into its unit's workspace, whatever
// the turn's result: the workspace then holds exactly the view's files, and
// its VCS metadata is left as it is.
func (u unitWorkspaces) capture(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView, _ coreadapter.SessionResult) error {
	stream := config.WorkstreamID(scope.Workstream)
	w, _, found, err := u.find(ctx, stream, scope.Unit)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("unit %s of workstream %s has no workspace", scope.Unit, stream)
	}
	return mirror(view.Workspace().Directory, w.Path)
}

// mirror makes dst hold the regular files and directories of src, and
// nothing else outside its VCS metadata, which it leaves as it is. VCS
// metadata, symlinks and special files in src are not copied. A file keeps its
// owner's execute bit.
func mirror(src, dst string) error {
	from, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer to.Close()
	err = fs.WalkDir(from.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || name == "." {
			return err
		}
		info, err := from.Lstat(name)
		if err != nil {
			return err
		}
		if !copied(name, info) {
			if info.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		existing, err := to.Lstat(name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return err
		case existing.IsDir() != info.IsDir() || !existing.IsDir() && !existing.Mode().IsRegular():
			if err := to.RemoveAll(name); err != nil {
				return err
			}
		}
		if info.IsDir() {
			return to.MkdirAll(name, 0755)
		}
		data, err := from.ReadFile(name)
		if err != nil {
			return err
		}
		perm := os.FileMode(0644)
		if info.Mode().Perm()&0100 != 0 {
			perm = 0755
		}
		if err := to.WriteFile(name, data, perm); err != nil {
			return err
		}
		return to.Chmod(name, perm)
	})
	if err != nil {
		return err
	}
	return fs.WalkDir(to.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || name == "." {
			return err
		}
		if isolation.VCSMetadata(entry.Name()) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := from.Lstat(name)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err == nil && copied(name, info) {
			return nil
		}
		if err := to.RemoveAll(name); err != nil {
			return err
		}
		if entry.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
}

// copied reports whether mirror copies an entry of its source: a directory
// or a regular file that is no VCS metadata.
func copied(name string, info fs.FileInfo) bool {
	return !isolation.VCSMetadata(filepath.Base(name)) && (info.IsDir() || info.Mode().IsRegular())
}
