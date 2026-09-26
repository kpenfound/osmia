package service

import (
	"errors"
	"slices"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// configDrift compares the top-level configuration file and the loaded
// project's file on disk with cfg, each on its own, and reads them without
// applying anything. A file that cannot be read or does not validate is
// invalid. reloaded is the error a reload of the files together fails with,
// which also marks the file it names invalid, or adds it when it is another
// project's.
func (s *Service) configDrift(cfg *config.Config, reloaded error) ConfigDrift {
	path, _ := cfg.Root.Config()
	files := []ConfigFile{{Path: path, State: ConfigUnchanged}}
	if disk, err := config.LoadTopLevel(s.options.Config); err != nil {
		files[0].State, files[0].Reason = ConfigInvalid, fileReason(err)
	} else if topLevelDigest(disk) != topLevelDigest(cfg) {
		files[0].State = ConfigChanged
	}
	if cfg.HasProject() {
		path, _ := cfg.Root.ProjectConfig(cfg.Project.ID)
		file := ConfigFile{Path: path, Project: cfg.Project.ID, State: ConfigUnchanged}
		if disk, err := cfg.WithProject(cfg.Project.ID, s.options.Config.Home); err != nil {
			file.State, file.Reason = ConfigInvalid, fileReason(err)
		} else if digest(&config.Config{Project: disk.Project}) != digest(&config.Config{Project: cfg.Project}) {
			file.State = ConfigChanged
		}
		files = append(files, file)
	}
	if reloaded != nil {
		i := 0
		var field *config.FieldError
		if errors.As(reloaded, &field) {
			i = slices.IndexFunc(files, func(f ConfigFile) bool { return f.Path == field.Path })
			if i < 0 {
				files, i = append(files, ConfigFile{Path: field.Path}), len(files)
			}
		}
		if files[i].State != ConfigInvalid {
			files[i].State, files[i].Reason = ConfigInvalid, fileReason(reloaded)
		}
	}
	differs := slices.ContainsFunc(files, func(f ConfigFile) bool { return f.State != ConfigUnchanged })
	return ConfigDrift{Differs: differs, Files: files}
}

// topLevelDigest is the digest of c's top-level settings: every setting but
// the project's own.
func topLevelDigest(c *config.Config) string {
	top := *c
	top.Project = config.Project{}
	if len(top.ActiveProjects) == 0 {
		top.ActiveProjects = nil
	}
	return digest(&top)
}

// fileReason says why a configuration file fails to load, without its path,
// which the file's entry names.
func fileReason(err error) string {
	var field *config.FieldError
	if !errors.As(err, &field) {
		return reloadError(err, time.Time{}).Message
	}
	if field.Field == "" {
		return field.Reason
	}
	return field.Field + ": " + field.Reason
}
