package isolation

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// errOutsideSkills refuses every absolute path that is not inside one of the
// turn's own skill directories.
var errOutsideSkills = errors.New("path is outside the turn's view and its skill directories")

// SkillReader reads files inside the skill directories of one turn, at the
// absolute paths where the engine mounts them. The directories come from the
// engine's preparation of the turn's own skill references, never from a path
// the model supplies, and every one must lie inside a directory the engine
// mounts read-only. It reads only: it holds no write or shell access and
// copies nothing.
//
// A path is readable only when it lies inside a prepared skill's skills
// directory, either where the generated plugin holds it or where its link
// resolves. A symlink that leaves that directory, version control metadata and
// special files are refused.
type SkillReader struct {
	engine coreadapter.SkillPreparing
	refs   []string

	mu      sync.Mutex
	plugins []string
}

// NewSkillReader returns the reader of a turn given refs, or nil when the turn
// has no skills or the engine prepares none.
func NewSkillReader(engine coreadapter.Engine, refs []string) *SkillReader {
	source, ok := engine.(coreadapter.SkillPreparing)
	if !ok || len(refs) == 0 {
		return nil
	}
	return &SkillReader{engine: source, refs: append([]string(nil), refs...)}
}

// prepared returns the plugin directories of the turn's references. The first
// success is kept for the rest of the turn.
func (s *SkillReader) prepared(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plugins != nil {
		return s.plugins, nil
	}
	plugins, err := s.engine.PrepareSkills(ctx, s.refs)
	if err != nil {
		return nil, err
	}
	if len(plugins) != len(s.refs) {
		return nil, errors.New("skill preparation returned a different number of directories than references")
	}
	s.plugins = plugins
	return plugins, nil
}

// skillMount is a skill directory as a path the model may use, and the target
// directory it reads.
type skillMount struct{ path, root string }

// skillMounts lists where the prepared skills' directories are readable: each
// plugin's skills directory, or each entry of it, under the path the plugin
// holds it at and under the path it resolves to.
func skillMounts(plugin string) []skillMount {
	skills := filepath.Join(plugin, "skills")
	info, err := os.Lstat(skills)
	if err != nil {
		return nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(skills)
		if err != nil {
			return nil
		}
		return []skillMount{{skills, target}, {target, target}}
	}
	if !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(skills)
	if err != nil {
		return nil
	}
	var mounts []skillMount
	for _, entry := range entries {
		held := filepath.Join(skills, entry.Name())
		target, err := filepath.EvalSymlinks(held)
		if err != nil {
			continue
		}
		mounts = append(mounts, skillMount{held, target}, skillMount{target, target})
	}
	return mounts
}

// Read returns a file inside one of the turn's skill directories. For a
// directory it returns the entry names as FileView.Read does. name must be an
// absolute path.
func (s *SkillReader) Read(ctx context.Context, name string) ([]byte, error) {
	if !filepath.IsAbs(name) || strings.ContainsRune(name, 0) {
		return nil, errOutsideSkills
	}
	name = filepath.Clean(name)
	plugins, err := s.prepared(ctx)
	if err != nil {
		return nil, err
	}
	var mounted []string
	for _, dir := range s.engine.SkillDirs() {
		if target, err := filepath.EvalSymlinks(dir); err == nil {
			mounted = append(mounted, target)
		}
	}
	for _, plugin := range plugins {
		held := filepath.Join(plugin, "skills")
		if name == held {
			if info, err := os.Lstat(held); err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir() {
				return listSkillEntries(held, mounted)
			}
		}
		for _, mount := range skillMounts(plugin) {
			rel, err := filepath.Rel(mount.path, name)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			if !insideAny(mounted, mount.root) {
				continue
			}
			return readSkillFile(mount.root, filepath.ToSlash(rel))
		}
	}
	return nil, errOutsideSkills
}

func insideAny(roots []string, dir string) bool {
	for _, root := range roots {
		if within(root, dir) {
			return true
		}
	}
	return false
}

// listSkillEntries names the entries of a plugin's own skills directory whose
// targets lie inside a mounted directory.
func listSkillEntries(dir string, mounted []string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		target, err := filepath.EvalSymlinks(filepath.Join(dir, entry.Name()))
		if err != nil || !insideAny(mounted, target) {
			continue
		}
		if info, err := os.Stat(target); err == nil && info.IsDir() {
			names = append(names, entry.Name()+"/")
		}
	}
	return []byte(strings.Join(names, "\n")), nil
}

// readSkillFile reads rel under root. os.Root refuses a symlink that leaves
// root.
func readSkillFile(root, rel string) ([]byte, error) {
	if rel != "." {
		for _, part := range strings.Split(rel, "/") {
			if VCSMetadata(part) {
				return nil, errors.New("VCS metadata is not exposed")
			}
		}
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	info, err := dir.Stat(rel)
	if err != nil {
		return nil, err
	}
	switch {
	case info.Mode().IsRegular():
		return dir.ReadFile(rel)
	case info.IsDir():
		entries, err := fs.ReadDir(dir.FS(), rel)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, entry := range entries {
			if VCSMetadata(entry.Name()) {
				continue
			}
			child, err := dir.Stat(path.Join(rel, entry.Name()))
			switch {
			case err != nil:
			case child.IsDir():
				names = append(names, entry.Name()+"/")
			case child.Mode().IsRegular():
				names = append(names, entry.Name())
			}
		}
		return []byte(strings.Join(names, "\n")), nil
	}
	return nil, errors.New("special files are not exposed")
}
