package service

import (
	"errors"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// configDrift compares the top-level configuration file on disk with cfg,
// reading it without applying anything. It is the only file a reload reads. A
// file that cannot be read or does not validate is invalid. reloaded is the
// error a reload fails with, which also marks the file invalid.
func (s *Service) configDrift(cfg *config.Config, reloaded error) ConfigDrift {
	path, _ := cfg.Root.Config()
	file := ConfigFile{Path: path, State: ConfigUnchanged}
	if disk, err := config.LoadTopLevel(s.options.Config); err != nil {
		file.State, file.Reason = ConfigInvalid, fileReason(err)
	} else if digest(disk.WithLoadedProjects(cfg.Projects)) != digest(cfg) {
		file.State = ConfigChanged
	}
	if reloaded != nil && file.State != ConfigInvalid {
		file.State, file.Reason = ConfigInvalid, fileReason(reloaded)
	}
	return ConfigDrift{Differs: file.State != ConfigUnchanged, Files: []ConfigFile{file}}
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
