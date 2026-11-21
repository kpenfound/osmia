package projectstore

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
)

// RetiredMark is the comment block an imported project file starts with. It
// is TOML comments only, so the file loads under the project-file rules as it
// did before the import.
const RetiredMark = "# Retired: Osmia reads project settings from its store; this file is no longer read.\n" +
	"# Edits here have no effect. An earlier Osmia version still starts on it.\n"

// source is one listed project's file, read and decoded as loading does.
type source struct {
	id   config.ProjectID
	data []byte
}

// readSource reads and validates project id's file as loading a project does.
// A file that fails is a *config.FieldError naming it and the field.
func readSource(cfg *config.Config, home string, id config.ProjectID) (source, error) {
	path, err := cfg.Root.ProjectConfig(id)
	if err != nil {
		return source{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return source{}, &config.FieldError{Path: path, Reason: "cannot be read", Err: err}
	}
	if _, err := cfg.DecodeProject(id, home, data); err != nil {
		return source{}, err
	}
	return source{id, data}, nil
}

// listed parses the top-level file's active_projects.
func listed(cfg *config.Config) ([]config.ProjectID, error) { return parseIDs(cfg.ActiveProjects) }

// unimported returns the sources the import still has to record: those of
// the listed projects that hold no stored revision. Any that fails to load
// fails the whole call.
func (s *Store) unimported(cfg *config.Config, home string, ids []config.ProjectID) ([]source, error) {
	var sources []source
	for _, id := range ids {
		numbers, err := s.numbers(id)
		if err != nil {
			return nil, err
		}
		if len(numbers) > 0 {
			continue
		}
		src, err := readSource(cfg, home, id)
		if err != nil {
			return nil, err
		}
		sources = append(sources, src)
	}
	return sources, nil
}

// Preflight checks that the import can bring in every listed project: each
// project in cfg's active_projects that the store does not hold yet must have
// a file that loads. It changes nothing, creates nothing and reads no
// project file once the import is complete. A file that does not load is
// returned as the loader's error, naming the file and the field.
func (s *Store) Preflight(cfg *config.Config, home string) error {
	r, err := s.readRegistry()
	if err != nil || r.Imported {
		return err
	}
	ids, err := listed(cfg)
	if err != nil {
		return err
	}
	_, err = s.unimported(cfg, home, ids)
	return err
}

// Import brings cfg's active_projects, in order, and every listed project's
// file into the store, once per root. The steps, each resumable:
//
//  1. every listed file that is not stored yet is checked to load; a file that
//     does not stops the import before anything is written;
//  2. each such file's bytes are recorded as that project's first revision;
//  3. each listed file is marked retired, which leaves it in place with its
//     settings;
//  4. the registry and the completion record are written as one file.
//
// A run interrupted anywhere resumes from what the store holds, without a
// second revision of any project. An import already complete does nothing,
// and reads no project file. Files of projects the list does not name are
// not read. home is the home directory the loader expands ~ against.
func (s *Store) Import(ctx context.Context, cfg *config.Config, home string) error {
	if r, err := s.readRegistry(); err != nil || r.Imported {
		return err
	}
	ids, err := listed(cfg)
	if err != nil {
		return err
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	// Another process may have finished the import while this one waited.
	r, err := s.readRegistry()
	if err != nil || r.Imported {
		return err
	}
	sources, err := s.unimported(cfg, home, ids)
	if err != nil {
		return err
	}
	if err := s.mark("import-checked"); err != nil {
		return err
	}
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.record(src.id, 1, src.data); err != nil {
			return err
		}
		if err := s.mark("settings-imported:" + string(src.id)); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := retire(cfg, id); err != nil {
			return err
		}
		if err := s.mark("file-retired:" + string(id)); err != nil {
			return err
		}
	}
	projects := make([]string, len(ids))
	for i, id := range ids {
		projects[i] = string(id)
	}
	if err := s.writeRegistry(registry{Version: 1, Imported: true, Projects: projects}); err != nil {
		return err
	}
	return s.mark("import-recorded")
}

// retire marks project id's file retired, keeping every byte it held after
// the mark. A file already marked is left as it is.
func retire(cfg *config.Config, id config.ProjectID) error {
	path, err := cfg.Root.ProjectConfig(id)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return &config.FieldError{Path: path, Reason: "cannot be read", Err: err}
	}
	if strings.HasPrefix(string(data), RetiredMark) {
		return nil
	}
	if err := config.ReplaceFile(path, append([]byte(RetiredMark), data...)); err != nil {
		return fmt.Errorf("retire %s: %w", path, err)
	}
	return nil
}
