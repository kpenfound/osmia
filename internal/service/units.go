package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/trace"
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

// unitWorkspaces are the workspaces of a project's units: one workspace per
// workstream and unit, <root>/units/<project>/<workstream>/<unit> on
// unitBranch, created from the workstream's feature branch. Nothing here
// removes one, so a unit's work stays in its workspace between turns and
// across restarts. They are also the workspaces a mason turn is lent: the
// turn works on a copy without VCS metadata, which capture copies back.
type unitWorkspaces struct{ streamWorkspaces }

var _ coreadapter.Workspaces = unitWorkspaces{}

// newUnitWorkspaces returns the unit workspaces of the configured project,
// each workstream's on the backend the repository records for it.
func newUnitWorkspaces(cfg *config.Config, repository *trace.Repository) unitWorkspaces {
	return unitWorkspaces{streamWorkspaces{cfg: cfg, repository: repository, directory: unitsDirectory}}
}

func unitName(stream config.WorkstreamID, unit string) string {
	return string(stream) + "/" + unit
}

// open returns the unit's workspace and the feature branch commit it descends
// from, creating the workspace from the tip of the feature branch when the
// clone has none. A workspace already there is returned with what it holds.
func (u unitWorkspaces) open(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, error) {
	feature := featureBranch(stream)
	g, err := u.of(stream)
	if err != nil {
		return workspace.Worktree{}, "", err
	}
	if _, exists, err := g.Branch(ctx, feature); err != nil {
		return workspace.Worktree{}, "", err
	} else if !exists {
		return workspace.Worktree{}, "", fmt.Errorf("the clone has no feature branch %s", feature)
	}
	acquired, err := g.Acquire(ctx, vcs.Request{Name: unitName(stream, unit), Ref: feature, Branch: unitBranch(stream, unit)})
	if err != nil {
		return workspace.Worktree{}, "", err
	}
	w := acquired.(workspace.Worktree)
	base, err := g.MergeBase(ctx, feature, w.Branch)
	if err != nil {
		return workspace.Worktree{}, "", err
	}
	return w, base, nil
}

// find returns the unit's workspace and the feature branch commit it
// descends from when the clone has it, and creates nothing.
func (u unitWorkspaces) find(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, bool, error) {
	g, err := u.of(stream)
	if err != nil {
		return workspace.Worktree{}, "", false, err
	}
	w, found, err := g.Workspace(ctx, unitName(stream, unit))
	if err != nil || !found {
		return workspace.Worktree{}, "", false, err
	}
	base, err := g.MergeBase(ctx, featureBranch(stream), w.Branch)
	if err != nil {
		return workspace.Worktree{}, "", false, err
	}
	return w, base, true, nil
}

// behind reports whether the unit has a workspace that does not descend from
// the tip of its workstream's feature branch, as after a landing moved the
// branch, until the workspace is rebased onto it.
func (u unitWorkspaces) behind(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	g, err := u.of(stream)
	if err != nil {
		return false, err
	}
	w, found, err := g.Workspace(ctx, unitName(stream, unit))
	if err != nil || !found {
		return false, err
	}
	tip, exists, err := g.Branch(ctx, featureBranch(stream))
	if err != nil || !exists {
		return false, err
	}
	descends, err := g.Ancestor(ctx, tip, w.Branch)
	return !descends, err
}

// snapshot commits what the unit's workspace holds as its candidate, a
// commit that descends from the feature branch, and returns it with the
// workspace and the feature branch commit the workspace descends from.
func (u unitWorkspaces) snapshot(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, string, error) {
	w, base, found, err := u.find(ctx, stream, unit)
	if err != nil {
		return workspace.Worktree{}, "", "", err
	}
	if !found {
		return workspace.Worktree{}, "", "", fmt.Errorf("unit %s of workstream %s has no workspace", unit, stream)
	}
	g, err := u.of(stream)
	if err != nil {
		return workspace.Worktree{}, "", "", err
	}
	candidate, err := g.Snapshot(ctx, w, featureBranch(stream))
	return w, base, candidate, err
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

// paths selects every directory and regular file at the top of a unit's
// workspace but its VCS metadata, so a mason turn's view is all of the
// workspace's files but its symlinks and special files, which a view never
// holds.
func (u unitWorkspaces) paths(w workspace.Worktree) ([]string, error) {
	return viewPaths(w.Path)
}

// viewPaths selects every directory and regular file at the top of the
// workspace at dir but its VCS metadata.
func viewPaths(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if !isolation.VCSMetadata(entry.Name()) && (entry.IsDir() || entry.Type().IsRegular()) {
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
// the turn's result: the workspace then holds the view's files, and its VCS
// metadata, symlinks and special files, and the directories that hold them, are
// left as they are; a view entry in their place is not copied back.
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
// nothing else outside what it keeps. It keeps dst's VCS metadata, symlinks
// and special files as they are, and the directories that hold them; an entry
// of src in their place is not copied. VCS metadata, symlinks and special files
// in src are not copied. A file keeps its owner's execute bit.
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
	if _, err := prune(from, to, ".", true); err != nil {
		return err
	}
	return fs.WalkDir(from.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || name == "." {
			return err
		}
		info, err := from.Lstat(name)
		if err != nil {
			return err
		}
		skip := func() error {
			if info.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !copied(name, info) {
			return skip()
		}
		// What prune left in the way of this entry is what dst keeps.
		existing, err := to.Lstat(name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return err
		case existing.IsDir() != info.IsDir() || !existing.IsDir() && !existing.Mode().IsRegular():
			return skip()
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
}

// prune removes from the directory dir of dst every regular file and
// directory that src does not hold as the same kind of entry, and reports
// whether dir still holds anything. inSrc tells whether src holds dir as a
// directory. It keeps VCS metadata, symlinks and special files, and the
// directories that hold them.
func prune(from, to *os.Root, dir string, inSrc bool) (bool, error) {
	entries, err := fs.ReadDir(to.FS(), dir)
	if err != nil {
		return false, err
	}
	kept := false
	for _, entry := range entries {
		name := path.Join(dir, entry.Name())
		info, err := to.Lstat(name)
		if err != nil {
			return false, err
		}
		if isolation.VCSMetadata(entry.Name()) || !info.IsDir() && !info.Mode().IsRegular() {
			kept = true
			continue
		}
		var source fs.FileInfo
		if inSrc {
			if source, err = from.Lstat(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return false, err
			}
		}
		same := source != nil && copied(name, source) && source.IsDir() == info.IsDir()
		if !info.IsDir() {
			if same {
				kept = true
			} else if err := to.Remove(name); err != nil {
				return false, err
			}
			continue
		}
		holds, err := prune(from, to, name, same)
		if err != nil {
			return false, err
		}
		if same || holds {
			kept = true
		} else if err := to.Remove(name); err != nil {
			return false, err
		}
	}
	return kept, nil
}

// copied reports whether mirror copies an entry of its source: a directory
// or a regular file that is no VCS metadata.
func copied(name string, info fs.FileInfo) bool {
	return !isolation.VCSMetadata(filepath.Base(name)) && (info.IsDir() || info.Mode().IsRegular())
}
