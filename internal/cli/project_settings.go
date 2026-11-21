package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
)

// Settings that hold a whole number or true or false. Every other setting of
// config.ProjectSettings holds text, except the hearsay_entities mapping, whose
// entries are named hearsay_entities.<local entity id>.
var (
	intProjectSettings  = []string{"capacity.per_workstream", "capacity.git_operations"}
	boolProjectSettings = []string{"hearsay_declare_repo", "flake_fixes"}
)

const entityPrefix = "hearsay_entities."

// errServiceRunning reports a root or project a running service holds.
var errServiceRunning = errors.New("a running service owns this root")

// projectSettingView is one setting of a stored revision.
type projectSettingView struct {
	Name    string `json:"name"`
	Value   any    `json:"value"`
	Default bool   `json:"default"`
}

// projectSettingsView is the stored settings of one project, as show prints
// them. A revision that does not load carries its error and its stored text
// instead of settings.
type projectSettingsView struct {
	Project  config.ProjectID     `json:"project"`
	Revision int                  `json:"revision"`
	Pin      string               `json:"pin"`
	Error    string               `json:"error,omitempty"`
	Settings []projectSettingView `json:"settings,omitempty"`
	Stored   string               `json:"stored,omitempty"`
}

// projectSettingsSaved is the result of a recorded edit.
type projectSettingsSaved struct {
	Project  config.ProjectID `json:"project"`
	Revision int              `json:"revision"`
	Previous int              `json:"previous"`
	Pin      string           `json:"pin"`
}

// runProjectSettings shows or edits the stored settings of project id. It is
// offline: it reads and writes the store directly, never contacts the service,
// and refuses while a service owns the root. It reads only the top-level file
// and the store, never a retired project file, and writes only a new revision
// of the project's settings, never the registry or the top-level file. verb is
// show, set or clear; args are the settings the last two name.
func runProjectSettings(root config.Root, id config.ProjectID, verb string, args []string, asJSON bool, stdout, stderr io.Writer) int {
	refuse := func(format string, a ...any) int {
		fmt.Fprintf(stderr, format+"\n", a...)
		return 1
	}
	path, err := root.ProjectConfig(id)
	if err != nil {
		return refuse("invalid root; choose an accessible directory with --root")
	}
	var edit config.ProjectEdit
	if verb != "show" {
		if edit, err = parseSettingsEdit(path, verb, args); err != nil {
			return refuse("invalid settings: %v; nothing was written", err)
		}
	}
	cfg, err := config.LoadTopLevel(root.Options(""))
	if err != nil {
		return refuse("%v", err)
	}
	store := projectstore.Open(cfg.Root)
	if code := requireRegistered(store, id, refuse); code != 0 {
		return code
	}
	if verb != "show" {
		// Validate the edit before taking the root's lock, which creates it:
		// a refused edit then leaves the root as it was.
		current, err := store.Current(id)
		if err != nil {
			return refuse("project %s: %v", id, err)
		}
		if _, _, err := cfg.EditProject(id, "", current.Data, edit); err != nil {
			return refuse("invalid settings: %v; nothing was written", err)
		}
	}
	release, err := holdService(cfg.Root, id, verb != "show")
	if err != nil {
		if errors.Is(err, errServiceRunning) {
			return refuse("%v; nothing was changed. While the service runs, change project settings on the Settings page or with PUT /v1/projects/config/%s. To use this command, stop the service with osmia stop first", err, id)
		}
		return refuse("cannot check that the service is stopped: %v; nothing was changed", err)
	}
	defer release()
	// The registry may have changed while the service was being checked for.
	if code := requireRegistered(store, id, refuse); code != 0 {
		return code
	}
	current, err := store.Current(id)
	if err != nil {
		return refuse("project %s: %v", id, err)
	}
	if verb == "show" {
		return showProjectSettings(cfg, store, id, asJSON, stdout, stderr)
	}
	revision, _, err := store.Save(cfg, "", id, current.Pin, edit)
	if err != nil {
		var field *config.FieldError
		switch {
		case errors.As(err, &field):
			return refuse("invalid settings: %v; nothing was written", err)
		case errors.Is(err, projectstore.ErrStale):
			return refuse("project %s: the settings changed while they were being edited; run the command again. Nothing was written", id)
		}
		return refuse("project %s: cannot record the settings: %v", id, err)
	}
	saved := projectSettingsSaved{Project: id, Revision: revision.Number, Previous: current.Number, Pin: revision.Pin}
	if asJSON {
		return output(stdout, stderr, saved)
	}
	fmt.Fprintf(stdout, "Project %s: recorded settings revision %d (it replaces revision %d). The service uses it when it starts.\n", id, saved.Revision, saved.Previous)
	return 0
}

// requireRegistered refuses a project the registry does not list. It reads the
// registry only.
func requireRegistered(store *projectstore.Store, id config.ProjectID, refuse func(string, ...any) int) int {
	imported, err := store.Imported()
	if err != nil {
		return refuse("cannot read the project registry: %v", err)
	}
	if !imported {
		return refuse("project %s is not in the registry: the registry is created when the service first starts and imports the projects; start it once, then use this command", id)
	}
	ids, err := store.Projects()
	if err != nil {
		return refuse("cannot read the project registry: %v", err)
	}
	if !slices.Contains(ids, id) {
		return refuse("project %s is not in the registry; this command changes the settings of a registered project only", id)
	}
	return 0
}

func showProjectSettings(cfg *config.Config, store *projectstore.Store, id config.ProjectID, asJSON bool, stdout, stderr io.Writer) int {
	file, revision, err := store.Load(cfg, "", id)
	view := projectSettingsView{Project: id, Revision: revision.Number, Pin: revision.Pin}
	if err != nil {
		view.Error, view.Stored = err.Error(), string(revision.Data)
	} else {
		for _, name := range config.ProjectSettings {
			view.Settings = append(view.Settings, projectSettingView{Name: name, Value: file.Project.SettingValue(name), Default: !file.Set(name)})
		}
	}
	if asJSON {
		return output(stdout, stderr, view)
	}
	if err != nil {
		fmt.Fprintf(stdout, "Project %s: stored settings revision %d do not load:\n  %s\n", id, view.Revision, view.Error)
		fmt.Fprintf(stdout, "Correct them with osmia project settings %s set FIELD=VALUE\nStored settings:\n%s", id, view.Stored)
		if !strings.HasSuffix(view.Stored, "\n") {
			fmt.Fprintln(stdout)
		}
		return 0
	}
	fmt.Fprintf(stdout, "Project %s: stored settings revision %d\n", id, view.Revision)
	for _, s := range view.Settings {
		value, _ := json.Marshal(s.Value)
		mark := ""
		if s.Default {
			mark = "  (default)"
		}
		fmt.Fprintf(stdout, "  %-24s %s%s\n", s.Name, value, mark)
	}
	return 0
}

// parseSettingsEdit turns the NAME=VALUE arguments of set, or the names of
// clear, into an edit. A name that is no setting of a project, a value of the
// wrong kind and a setting named twice are refused naming the setting; the
// edit itself is validated as a settings save validates it.
func parseSettingsEdit(path, verb string, args []string) (config.ProjectEdit, error) {
	edit := config.ProjectEdit{Strings: map[string]string{}, Ints: map[string]int{}, Bools: map[string]bool{}, Entities: map[string]string{}}
	refused := func(field, reason string) error { return &config.FieldError{Path: path, Field: field, Reason: reason} }
	seen := map[string]bool{}
	for _, arg := range args {
		name, value, has := strings.Cut(arg, "=")
		if verb == "set" && !has {
			return edit, refused(name, "needs a value: write "+name+"=VALUE")
		}
		if verb == "clear" && has {
			return edit, refused(name, "is cleared by name alone: write clear "+name)
		}
		if seen[name] {
			return edit, refused(name, "is changed more than once")
		}
		seen[name] = true
		entity, isEntity := strings.CutPrefix(name, entityPrefix)
		switch {
		case verb == "clear" && isEntity:
			edit.ClearEntities = append(edit.ClearEntities, entity)
		case verb == "clear":
			edit.Clear = append(edit.Clear, name)
		case isEntity:
			edit.Entities[entity] = value
		case !slices.Contains(config.ProjectSettings, name) || name == "hearsay_entities":
			return edit, refused(name, "is not a setting of a project; the settings are "+strings.Join(config.ProjectSettings, ", ")+", and hearsay_entities.<local id> for one mapping entry")
		case slices.Contains(intProjectSettings, name):
			n, err := strconv.Atoi(value)
			if err != nil {
				return edit, refused(name, "must be a whole number")
			}
			edit.Ints[name] = n
		case slices.Contains(boolProjectSettings, name):
			switch value {
			case "true", "false":
				edit.Bools[name] = value == "true"
			default:
				return edit, refused(name, "must be true or false")
			}
		default:
			edit.Strings[name] = value
		}
	}
	return edit, nil
}

// holdService takes the locks a running service holds, so that neither a
// running service nor a service starting meanwhile can write what this command
// reads and writes: the root's ownership lock and the project's trace lock. It
// returns errServiceRunning when either is held, and creates nothing unless
// create is set, which creates the root's ownership lock as the service does.
// The project's lock comes first, so a refusal for it creates no file.
func holdService(root config.Root, id config.ProjectID, create bool) (func(), error) {
	traces, err := root.ProjectTrace(id)
	if err != nil {
		return nil, err
	}
	var held []*os.File
	release := func() {
		for _, f := range held {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
		}
	}
	for _, l := range []struct {
		path   string
		create bool
	}{{filepath.Join(traces, "osmia.lock"), false}, {filepath.Join(root.String(), ".service.lock"), create}} {
		flags := os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
		if l.create {
			flags |= os.O_CREATE
		}
		f, err := os.OpenFile(l.path, flags, 0600)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			release()
			return nil, err
		}
		if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
			f.Close()
			release()
			return nil, fmt.Errorf("%s is not a regular file", l.path)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			release()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, errServiceRunning
			}
			return nil, err
		}
		held = append(held, f)
	}
	return release, nil
}
