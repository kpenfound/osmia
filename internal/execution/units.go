package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/questions"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/kpenfound/osmia/internal/workspace"
)

func UnitBranch(stream config.WorkstreamID, unit string) string {
	return "osmia-unit/" + string(stream) + "/" + unit
}

type unitWorkspaces struct{ streamWorkspaces }

var _ coreadapter.Workspaces = unitWorkspaces{}

func NewUnitWorkspaces(cfg *config.Config, repository *trace.Repository) unitWorkspaces {
	return unitWorkspaces{streamWorkspaces{Cfg: cfg, Repository: repository, Directory: unitsDirectory}}
}

func UnitName(stream config.WorkstreamID, unit string) string {
	return string(stream) + "/" + unit
}

func (u unitWorkspaces) open(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, error) {
	feature := featureBranch(stream)
	g, err := u.Of(stream)
	if err != nil {
		return workspace.Worktree{}, "", err
	}
	if _, exists, err := g.Branch(ctx, feature); err != nil {
		return workspace.Worktree{}, "", err
	} else if !exists {
		return workspace.Worktree{}, "", fmt.Errorf("the clone has no feature branch %s", feature)
	}
	acquired, err := g.Acquire(ctx, vcs.Request{Name: UnitName(stream, unit), Ref: feature, Branch: UnitBranch(stream, unit)})
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

// UnitChange is the document units/<unit>/change.json: the change ID of the
// unit's workspace on Jujutsu, recorded when its mason starts. Every rebase
// of the workspace makes a commit carrying it, so the unit keeps one change
// through its rebases.
type UnitChange struct {
	Unit   string `json:"unit"`
	Branch string `json:"branch"`
	Change string `json:"change"`
}

const changePath = "units/%s/change.json"

func ChangeDocument(unit string) string { return trace.UnitSubject(unit) + "-change" }

func ReadUnitChange(repository *trace.Repository, stream config.WorkstreamID, unit string) (string, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return "", err
	}
	change := ""
	for _, d := range docs {
		if d.ID != ChangeDocument(unit) {
			continue
		}
		var c UnitChange
		if err := json.Unmarshal([]byte(d.Content), &c); err != nil {
			return "", fmt.Errorf("%s: %w", d.Path, err)
		}
		change = c.Change
	}
	return change, nil
}

func (m *masons) changeRecord(ctx context.Context, stream config.WorkstreamID, unit string, w workspace.Worktree) (trace.Document, bool, error) {
	g, err := NewUnitWorkspaces(m.cfg, m.repository).Of(stream)
	if err != nil {
		return trace.Document{}, false, err
	}
	change, err := g.Change(ctx, w)
	if err != nil || change == "" {
		return trace.Document{}, false, err
	}
	if recorded, err := ReadUnitChange(m.repository, stream, unit); err != nil || recorded != "" {
		return trace.Document{}, false, err
	}
	data, err := json.MarshalIndent(UnitChange{Unit: unit, Branch: w.Branch, Change: change}, "", "  ")
	if err != nil {
		return trace.Document{}, false, err
	}
	h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: ChangeDocument(unit), Revision: 1, Project: m.repository.Project(), Workstream: stream, Unit: unit, At: m.s.Now(), Actor: masonActor, Cause: trace.UnitSubject(unit) + "-" + UnitReady}
	return trace.Document{Header: h, Path: fmt.Sprintf(changePath, unit), Content: string(data) + "\n"}, true, nil
}

func (u unitWorkspaces) Find(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, bool, error) {
	g, err := u.Of(stream)
	if err != nil {
		return workspace.Worktree{}, "", false, err
	}
	w, found, err := g.Workspace(ctx, UnitName(stream, unit))
	if err != nil || !found {
		return workspace.Worktree{}, "", false, err
	}
	base, err := g.MergeBase(ctx, featureBranch(stream), w.Branch)
	if err != nil {
		return workspace.Worktree{}, "", false, err
	}
	return w, base, true, nil
}

func (u unitWorkspaces) Behind(ctx context.Context, stream config.WorkstreamID, unit string) (bool, error) {
	g, err := u.Of(stream)
	if err != nil {
		return false, err
	}
	w, found, err := g.Workspace(ctx, UnitName(stream, unit))
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

func (u unitWorkspaces) Snapshot(ctx context.Context, stream config.WorkstreamID, unit string) (workspace.Worktree, string, string, error) {
	w, base, found, err := u.Find(ctx, stream, unit)
	if err != nil {
		return workspace.Worktree{}, "", "", err
	}
	if !found {
		return workspace.Worktree{}, "", "", fmt.Errorf("unit %s of workstream %s has no workspace", unit, stream)
	}
	g, err := u.Of(stream)
	if err != nil {
		return workspace.Worktree{}, "", "", err
	}
	candidate, err := g.Snapshot(ctx, w, featureBranch(stream))
	return w, base, candidate, err
}

func (u unitWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	if err := ctx.Err(); err != nil {
		return coreadapter.WorkspaceLease{}, err
	}
	scope := req.Scope
	if scope.Role != masonRole || scope.Workstream == "" || scope.Unit == "" {
		return coreadapter.WorkspaceLease{}, errors.New("a unit workspace is lent to a mason turn of a unit alone")
	}
	stream := config.WorkstreamID(scope.Workstream)
	w, _, found, err := u.Find(ctx, stream, scope.Unit)
	if err != nil {
		return coreadapter.WorkspaceLease{}, err
	}
	if !found {
		return coreadapter.WorkspaceLease{}, fmt.Errorf("unit %s of workstream %s has no workspace", scope.Unit, stream)
	}
	return coreadapter.WorkspaceLease{Workspace: coreadapter.Workspace{ID: UnitName(stream, scope.Unit), Directory: w.Path, Access: req.Access}, Lease: noLease{}}, nil
}

func (u unitWorkspaces) Paths(w workspace.Worktree) ([]string, error) {
	return ViewPaths(w.Path)
}

func ViewPaths(dir string) ([]string, error) {
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

func (u unitWorkspaces) selection(ctx context.Context, scope coreadapter.Scope, execution coreadapter.ExecutionSettings) (isolation.Selection, error) {
	stream := config.WorkstreamID(scope.Workstream)
	w, _, found, err := u.Find(ctx, stream, scope.Unit)
	if err != nil {
		return isolation.Selection{}, err
	}
	if !found {
		return isolation.Selection{}, fmt.Errorf("unit %s of workstream %s has no workspace", scope.Unit, stream)
	}
	revision, err := u.record(ctx, scope, w)
	if err != nil {
		return isolation.Selection{}, err
	}
	paths, err := u.Paths(w)
	return isolation.Selection{Paths: paths, Commit: u.fetchable(ctx, stream, revision), Execution: execution, Narrow: &UnitMasonGrant}, err
}

func (u unitWorkspaces) fetchable(ctx context.Context, stream config.WorkstreamID, commit string) string {
	if commit == "" {
		return ""
	}
	g, err := u.Of(stream)
	if err != nil {
		return ""
	}
	if known, err := g.Known(ctx, commit); err != nil || !known {
		return ""
	}
	return commit
}

const recordedName = "recorded"

func TurnViews(cfg *config.Config, project config.ProjectID, stream config.WorkstreamID, agent, turn string) string {
	return filepath.Join(cfg.Root.String(), "views", string(project), string(stream), agent, turn)
}

func (u unitWorkspaces) record(ctx context.Context, scope coreadapter.Scope, w workspace.Worktree) (string, error) {
	stream := config.WorkstreamID(scope.Workstream)
	dir := TurnViews(u.Cfg, u.Repository.Project(), stream, scope.Thread, scope.Turn)
	path := filepath.Join(dir, recordedName)
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		if err != nil {
			return "", err
		}
		recorded, err := os.ReadFile(path)
		return string(recorded), err
	}
	g, err := u.Of(stream)
	if err != nil {
		return "", err
	}
	revision, err := g.Record(ctx, w)
	if err != nil || revision == "" {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path+".tmp", []byte(revision), 0600); err != nil {
		return "", err
	}
	return revision, os.Rename(path+".tmp", path)
}

func (u unitWorkspaces) Changed(ctx context.Context, stream config.WorkstreamID, unit, turn string) ([]string, bool, error) {
	w, _, found, err := u.Find(ctx, stream, unit)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, fmt.Errorf("unit %s of workstream %s has no workspace", unit, stream)
	}
	g, err := u.Of(stream)
	if err != nil {
		return nil, false, err
	}
	revision, err := os.ReadFile(filepath.Join(TurnViews(u.Cfg, u.Repository.Project(), stream, masonAgent(unit), turn), recordedName))
	if errors.Is(err, fs.ErrNotExist) {
		_, err := g.Record(ctx, w)
		return nil, false, err
	}
	if err != nil {
		return nil, false, err
	}
	paths, err := g.Changed(ctx, w, string(revision))
	return paths, err == nil, err
}

func kept(ended string, paths []string, recorded bool) string {
	switch {
	case !recorded:
		return "Your workspace includes the files left by " + ended + "."
	case len(paths) == 0:
		return "Your workspace includes the files left by " + ended + ", which changed none of them."
	default:
		return "Your workspace includes the files left by " + ended + ", which changed " + strings.Join(paths, ", ") + "."
	}
}

func (u unitWorkspaces) capture(ctx context.Context, scope coreadapter.Scope, view *isolation.FileView, _ coreadapter.SessionResult) error {
	stream := config.WorkstreamID(scope.Workstream)
	w, _, found, err := u.Find(ctx, stream, scope.Unit)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("unit %s of workstream %s has no workspace", scope.Unit, stream)
	}
	g, err := u.Of(stream)
	if err != nil {
		return err
	}
	return captureFiles(ctx, g, view.Workspace().Directory, w.Path)
}

type retriedTurns struct {
	Turns      coreadapter.Turns
	units      unitWorkspaces
	repository *trace.Repository
}

var _ coreadapter.Turns = (*retriedTurns)(nil)

var _ coreadapter.ResumeChecker = (*retriedTurns)(nil)

func (t *retriedTurns) Run(ctx context.Context, prepared coreadapter.PreparedTurn) (coreadapter.SessionResult, error) {
	scope := prepared.Scope
	if scope.Role == masonRole && scope.Thread != driftMasonAgent && scope.Unit != "" {
		stream := config.WorkstreamID(scope.Workstream)
		th, err := t.repository.Thread(stream, scope.Thread)
		if err != nil {
			return coreadapter.SessionResult{}, err
		}
		i := slices.IndexFunc(th.Turns, func(q trace.QueuedTurn) bool { return q.Request.TurnID == scope.Turn })
		if i >= 0 && slices.ContainsFunc(th.Turns[i].Attempts, func(a trace.TurnAttempt) bool { return a.Result != nil && a.Result.TimedOut }) {
			paths, recorded, err := t.units.Changed(ctx, stream, scope.Unit, scope.Turn)
			if err != nil {
				return coreadapter.SessionResult{}, err
			}
			if recorded {
				prepared.Prompt += "\n\nAn earlier attempt at this turn timed out. " + kept("the earlier attempts", paths, true) + " Continue from those files."
			}
		}
	}
	return t.Turns.Run(ctx, prepared)
}

func (t *retriedTurns) CheckResume(ctx context.Context, previous, next coreadapter.Profile, session coreadapter.BackendSession) error {
	if checker, ok := t.Turns.(coreadapter.ResumeChecker); ok {
		return checker.CheckResume(ctx, previous, next, session)
	}
	return coreadapter.ErrResumeUnavailable
}

func Mirror(src, dst string) error {
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

func copied(name string, info fs.FileInfo) bool {
	return !isolation.VCSMetadata(filepath.Base(name)) && (info.IsDir() || info.Mode().IsRegular())
}

var UnitMasonGrant = coreadapter.Capabilities{Tools: []string{"file_read", "file_write", questions.AskTool, "factory_context", "project_context", "record_discovery", DoneTool}, WriteFiles: true, Execute: true}
