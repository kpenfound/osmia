package browsertest

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/service"
)

type ProjectConfigEdit = service.ProjectConfigEdit
type ProjectConfigEditValues = service.ProjectConfigEditValues

// ownerTables is what an owner keeps below a project file's root keys: an
// ignored [preview] table and a commented capacity.
const ownerTables = "\n[preview]\ncall = [\"serve\"]\nport = 8080\n\n[capacity]\n# Keep this low.\ngit_operations = 3\n"

// settingsFixture is a running service with one project whose file an owner
// keeps by hand, and a page on its Project management view.
type settingsFixture struct {
	t       *testing.T
	s       *Service
	c       *Client
	p       *page
	project config.ProjectID
	path    string
	// before is the file as the page first read it.
	before string
}

// newSettingsFixture starts a real service on a temporary root and registers
// a project. Its file is commented and carries the owner's own root keys, then
// ownerTables; the service loads it before the page opens.
func newSettingsFixture(t *testing.T, rootKeys string) *settingsFixture {
	t.Helper()
	opts, clone := projectFixture(t)
	withWeb(t, opts, "127.0.0.1:0")
	s, c := start(t, opts)
	ctx := context.Background()
	added, err := c.AddProject(ctx, request(clone))
	must(t, err)
	project := added.Project.ID
	snapshot, err := c.ProjectConfig(ctx, project)
	must(t, err)
	base, err := os.ReadFile(snapshot.Path)
	must(t, err)
	text := "# Owner notes.\n" + string(base) + rootKeys + ownerTables
	must(t, os.WriteFile(snapshot.Path, []byte(text), 0600))
	_, err = c.Reload(ctx)
	must(t, err)
	return &settingsFixture{t: t, s: s, c: c, project: project, path: snapshot.Path, before: text}
}

// open shows the project's settings in Project management at the viewport's
// width, with the page's own config events and writes observed.
func (f *settingsFixture) open(width, height int64) {
	f.t.Helper()
	f.p = openBrowser(f.t)
	observeManagementConfig(f.p)
	viewport := chromedp.EmulateViewport(width, height)
	if width < 600 {
		viewport = chromedp.EmulateViewport(width, height, chromedp.EmulateMobile)
	}
	f.p.Run(viewport, chromedp.Navigate("http://"+f.s.WebAddr()+"/"))
	f.p.Await("project card", `document.querySelector('#project-pane [data-project-action=projects]') !== null`)
	recordManagementWrites(f.p)
	f.p.OpenView("projects")
	f.p.Choose("#project-edit [name=project]", string(f.project))
	f.p.Eval(`document.querySelector('#project-edit [name=project]').dispatchEvent(new Event('change', {bubbles: true}))`, nil)
	f.p.AwaitText("#config-file", f.path)
	f.p.Await("settings read", `document.querySelector('#project-config').dataset.pin !== ''`)
}

func settingInput(name string) string { return `#project-config [name="` + name + `"]` }

func settingMarker(name, part string) string {
	return `#project-config [data-setting="` + name + `"] [data-marker=` + part + `]`
}

func settingsResult() string { return "#project-config .result" }

func (f *settingsFixture) disk() string {
	f.t.Helper()
	data, err := os.ReadFile(f.path)
	must(f.t, err)
	return string(data)
}

// hand edits the file on disk the way an owner does, without reloading.
func (f *settingsFixture) hand(from, to string) string {
	f.t.Helper()
	text := f.disk()
	edited := strings.Replace(text, from, to, 1)
	if edited == text {
		f.t.Fatalf("%q is not in the file:\n%s", from, text)
	}
	must(f.t, os.WriteFile(f.path, []byte(edited), 0600))
	return edited
}

func (f *settingsFixture) effective() config.Project {
	f.t.Helper()
	cfg, err := f.c.Configuration(context.Background())
	must(f.t, err)
	for _, p := range cfg.Effective.Projects {
		if p.ID == f.project {
			return p
		}
	}
	f.t.Fatalf("project %s is not in effect: %+v", f.project, cfg.Effective.Projects)
	return config.Project{}
}

// drift is the state of the project's file against the loaded configuration.
func (f *settingsFixture) drift() string {
	f.t.Helper()
	cfg, err := f.c.Configuration(context.Background())
	must(f.t, err)
	for _, file := range cfg.Drift.Files {
		if file.Path == f.path {
			return file.State
		}
	}
	f.t.Fatalf("no drift entry for %s: %+v", f.path, cfg.Drift.Files)
	return ""
}

// keepsOtherBytes checks that after holds every line of before in order,
// except the lines dropped, so nothing the owner did not change was
// rewritten, reordered or lost.
func keepsOtherBytes(t *testing.T, before, after string, dropped ...string) {
	t.Helper()
	rest := strings.Split(after, "\n")
lines:
	for _, line := range strings.Split(before, "\n") {
		for _, drop := range dropped {
			if line == drop {
				continue lines
			}
		}
		for len(rest) > 0 {
			next := rest[0]
			rest = rest[1:]
			if next == line {
				continue lines
			}
		}
		t.Fatalf("line %q of the file was dropped or reordered:\nbefore:\n%s\nafter:\n%s", line, before, after)
	}
}

// Project management shows every setting of the project file with the file's
// values, marks the omitted ones as defaults, and saves a change to the file
// the service runs from: the file's other content stays as it was, the service
// uses the new values without a reload, and the drift view shows no drift.
func TestBrowserProjectManagementSavesProjectSettingsToTheFile(t *testing.T) {
	t.Parallel()
	f := newSettingsFixture(t, "")
	ctx := context.Background()
	read, err := f.c.ProjectConfig(ctx, f.project)
	must(t, err)
	f.open(390, 844)

	var names, defaults []string
	for _, setting := range read.Settings {
		names = append(names, setting.Name)
		if *setting.Default {
			defaults = append(defaults, setting.Name)
		}
	}
	f.p.Await("every setting of the file, in order", `[...document.querySelectorAll('#config-fields [data-setting]')].map(n => n.dataset.setting).join(',') === `+quote(strings.Join(names, ",")))
	f.p.Await("omitted settings marked as defaults and no others", `[...document.querySelectorAll('#config-fields [data-setting]')].filter(n => n.querySelector('[data-marker=default]').textContent !== '').map(n => n.dataset.setting).join(',') === `+quote(strings.Join(defaults, ",")))
	f.p.Await("no setting pending", `[...document.querySelectorAll('#config-fields [data-marker=pending]')].every(n => n.textContent === '')`)
	for _, setting := range read.Settings {
		if text, ok := setting.Value.(string); ok {
			f.p.Await("the file's value of "+setting.Name, `document.querySelector(`+quote(settingInput(setting.Name))+`).value === `+quote(text))
		}
	}
	f.p.Await("the saved capacity from the file", `document.querySelector(`+quote(settingInput("capacity.git_operations"))+`).value === '3'`)
	f.p.Await("a default shown with its value", `document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === `+quote(config.DefaultChecksTimeout))
	f.p.AwaitText(settingMarker("capacity.per_workstream", "default"), "inherited")
	f.p.Await("settings fit at phone width", `document.documentElement.scrollWidth <= innerWidth && document.querySelector('#config-fields').getBoundingClientRect().right <= innerWidth`)

	f.p.SetValue(settingInput("checks_timeout"), "30m")
	f.p.SetValue(settingInput("capacity.git_operations"), "5")
	f.p.SetValue(settingInput("hearsay_scope"), "shop")
	f.p.SetValue(settingInput("hearsay_entities"), "cart = cart-scope")
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "ok")

	disk := f.disk()
	for _, want := range []string{`checks_timeout = "30m"`, "git_operations = 5", `hearsay_scope = "shop"`, `cart = "cart-scope"`} {
		if !strings.Contains(disk, want) {
			t.Fatalf("%s is not in the file:\n%s", want, disk)
		}
	}
	keepsOtherBytes(t, f.before, disk, "git_operations = 3")
	for _, kept := range []string{"# Owner notes.", "[preview]", `call = ["serve"]`, "port = 8080", "# Keep this low."} {
		if !strings.Contains(disk, kept) {
			t.Fatalf("%q was lost from the file:\n%s", kept, disk)
		}
	}
	// No Reload was pressed: the service already runs the saved values.
	p := f.effective()
	if p.ChecksTimeout != "30m" || p.Capacity.GitOperations != 5 || p.HearsayScope != "shop" || p.HearsayEntities["cart"] != "cart-scope" {
		t.Fatalf("in effect: %+v", p)
	}
	if state := f.drift(); state != api.ConfigUnchanged {
		t.Fatalf("drift of the saved file: %s", state)
	}
	saved, err := f.c.ProjectConfig(ctx, f.project)
	must(t, err)
	f.p.Await("the save sent only the changed settings, pinned to the file it read", `(() => {
      const w = managementWrites.at(-1);
      return w.method === 'PUT' && w.body.pin === `+quote(read.Pin)+` && Object.keys(w.body.set).sort().join(',') === 'capacity.git_operations,checks_timeout,hearsay_entities,hearsay_scope' && w.body.clear.length === 0;
    })()`)
	f.p.Await("the form holds the saved file's version, without a pending value", `document.querySelector('#project-config').dataset.pin === `+quote(saved.Pin)+` && document.querySelector('#project-config .result').textContent.includes(`+quote(f.path)+`) && document.querySelector(`+quote(settingMarker("checks_timeout", "default"))+`).textContent === '' && document.querySelector(`+quote(settingMarker("checks_timeout", "pending"))+`).textContent === ''`)
}

// A hand edit the service has not applied shows as the file's value marked
// not yet applied beside the value in effect. A save of another setting keeps
// the edit in the file and puts it into effect, rather than reverting it.
func TestBrowserProjectManagementShowsAnUnappliedHandEditAndKeepsItOnSave(t *testing.T) {
	t.Parallel()
	f := newSettingsFixture(t, "")
	f.hand("landing = \"commit-per-unit\"\n", "landing = \"commit-per-unit\"\nchecks_timeout = \"20m\"\n")
	if p := f.effective(); p.ChecksTimeout != config.DefaultChecksTimeout {
		t.Fatalf("the hand edit was applied: %+v", p)
	}
	f.open(1280, 800)

	f.p.Await("the file's value", `document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === '20m'`)
	f.p.AwaitText(settingMarker("checks_timeout", "pending"), "Not yet applied. In effect: "+config.DefaultChecksTimeout)
	f.p.Await("only that setting pending and set in the file", `[...document.querySelectorAll('#config-fields [data-marker=pending]')].filter(n => n.textContent !== '').length === 1 && document.querySelector(`+quote(settingMarker("checks_timeout", "default"))+`).textContent === ''`)
	if f.drift() != api.ConfigChanged {
		t.Fatalf("drift of the hand-edited file: %s", f.drift())
	}

	f.p.SetValue(settingInput("capacity.git_operations"), "2")
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "ok")
	disk := f.disk()
	if !strings.Contains(disk, `checks_timeout = "20m"`) || !strings.Contains(disk, "git_operations = 2") {
		t.Fatalf("the hand edit or the saved value is not in the file:\n%s", disk)
	}
	p := f.effective()
	if p.ChecksTimeout != "20m" || p.Capacity.GitOperations != 2 {
		t.Fatalf("both changes are in effect: %+v", p)
	}
	if state := f.drift(); state != api.ConfigUnchanged {
		t.Fatalf("drift after the save: %s", state)
	}
	f.p.Await("nothing pending after the save", `[...document.querySelectorAll('#config-fields [data-marker=pending]')].every(n => n.textContent === '')`)
}

// An invalid value is refused with the field error on the page, the draft
// stays, and the file and the loaded configuration stay as they were. A
// check that spans files is refused the same way, and the kept draft saves
// once it is corrected.
func TestBrowserProjectManagementRefusesInvalidSettingsAndKeepsTheDraft(t *testing.T) {
	t.Parallel()
	f := newSettingsFixture(t, "")
	f.open(1280, 800)

	f.p.SetValue(settingInput("checks_timeout"), "soon")
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "error")
	f.p.AwaitText(settingMarker("checks_timeout", "default")+` ~ .field-error`, "checks_timeout")
	f.p.Await("the draft kept and the field marked", `document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === 'soon' && document.querySelector(`+quote(settingInput("checks_timeout"))+`).getAttribute('aria-invalid') === 'true'`)
	f.p.AwaitText(settingsResult(), "checks_timeout")
	if f.disk() != f.before {
		t.Fatalf("a refused save changed the file:\n%s", f.disk())
	}
	if p := f.effective(); p.ChecksTimeout != config.DefaultChecksTimeout {
		t.Fatalf("a refused save changed the loaded configuration: %+v", p)
	}

	f.p.SetValue(settingInput("checks_timeout"), "30m")
	f.p.SetValue(settingInput("classifier"), "no-such-profile")
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "error")
	f.p.AwaitText(settingsResult(), "classifier")
	f.p.Await("both fields of the draft kept", `document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === '30m' && document.querySelector(`+quote(settingInput("classifier"))+`).value === 'no-such-profile'`)
	if f.disk() != f.before {
		t.Fatalf("a refused save changed the file:\n%s", f.disk())
	}

	f.p.SetValue(settingInput("classifier"), "")
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "ok")
	if p := f.effective(); p.ChecksTimeout != "30m" || p.Classifier != "" {
		t.Fatalf("the corrected draft in effect: %+v", p)
	}
}

// A save pinned to a file that changed on disk after the page read it is
// refused as stale and writes nothing. The draft stays with a notice that
// the configuration changed elsewhere, until the owner reads the latest file
// values, which show the hand edit as not yet applied.
func TestBrowserProjectManagementRefusesAStaleSaveAndLoadsTheLatestValues(t *testing.T) {
	t.Parallel()
	f := newSettingsFixture(t, "")
	f.open(1280, 800)
	f.p.Await("the file's capacity", `document.querySelector(`+quote(settingInput("capacity.git_operations"))+`).value === '3'`)

	f.p.SetValue(settingInput("capacity.git_operations"), "6")
	edited := f.hand("landing = \"commit-per-unit\"\n", "landing = \"commit-per-unit\"\nchecks_timeout = \"20m\"\n")
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "error")
	f.p.AwaitText(settingsResult(), "changed since it was read")
	f.p.Await("the stale notice and the draft", `!document.querySelector('#config-changed').hidden && document.querySelector(`+quote(settingInput("capacity.git_operations"))+`).value === '6'`)
	if f.disk() != edited {
		t.Fatalf("a stale save wrote to the file:\n%s", f.disk())
	}

	f.p.Click("#project-config [data-action=read-config]")
	managementOutcome(f.p, settingsResult(), "ok")
	f.p.Await("the latest file values, with the hand edit not yet applied", `document.querySelector('#config-changed').hidden &&
      document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === '20m' &&
      document.querySelector(`+quote(settingInput("capacity.git_operations"))+`).value === '3' &&
      document.querySelector(`+quote(settingMarker("checks_timeout", "pending"))+`).textContent.includes('In effect: `+config.DefaultChecksTimeout+`')`)
	if p := f.effective(); p.ChecksTimeout != config.DefaultChecksTimeout {
		t.Fatalf("loading the latest values reloaded the service: %+v", p)
	}
	if f.drift() != api.ConfigChanged {
		t.Fatalf("drift of the hand-edited file: %s", f.drift())
	}
}

// Clearing a field on the page and saving removes its key from the file,
// and the default or inherited value then applies and shows as the default.
func TestBrowserProjectManagementClearsASettingToItsDefault(t *testing.T) {
	t.Parallel()
	f := newSettingsFixture(t, "checks_timeout = \"30m\" # the owner's choice\n")
	if p := f.effective(); p.ChecksTimeout != "30m" || p.Fork == "" {
		t.Fatalf("before: %+v", p)
	}
	f.open(1280, 800)
	f.p.Await("the file's timeout, not a default", `document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === '30m' && document.querySelector(`+quote(settingMarker("checks_timeout", "default"))+`).textContent === ''`)

	f.p.SetValue(settingInput("checks_timeout"), "")
	f.p.SetValue(settingInput("fork"), "")
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "ok")
	disk := f.disk()
	if strings.Contains(disk, "checks_timeout") || strings.Contains(disk, "fork =") {
		t.Fatalf("cleared keys are still in the file:\n%s", disk)
	}
	for _, kept := range []string{"# Owner notes.", "[preview]", "# Keep this low.", "git_operations = 3", "upstream = "} {
		if !strings.Contains(disk, kept) {
			t.Fatalf("%q was lost from the file:\n%s", kept, disk)
		}
	}
	if p := f.effective(); p.ChecksTimeout != config.DefaultChecksTimeout || p.Fork != "" {
		t.Fatalf("defaults in effect: %+v", p)
	}
	f.p.Await("the defaults shown as in effect", `document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === `+quote(config.DefaultChecksTimeout)+` && document.querySelector(`+quote(settingMarker("checks_timeout", "default"))+`).textContent !== '' && document.querySelector(`+quote(settingInput("fork"))+`).value === '' && document.querySelector(`+quote(settingMarker("fork", "default"))+`).textContent !== ''`)
}

// An untouched form follows another client's save live, and a later save from
// it is pinned to the version it followed. A form holding a draft keeps the
// draft and its pin, and says the configuration changed elsewhere.
func TestBrowserProjectManagementFollowsOtherClientsSaves(t *testing.T) {
	t.Parallel()
	f := newSettingsFixture(t, "")
	ctx := context.Background()
	f.open(1280, 800)
	f.p.Eval(`managementConfigEvents.length = 0`, nil)

	read, err := f.c.ProjectConfig(ctx, f.project)
	must(t, err)
	other, err := f.c.SaveProjectConfig(ctx, f.project, ProjectConfigEdit{Pin: read.Pin, Set: ProjectConfigEditValues{ChecksTimeout: textPtr("45m")}})
	must(t, err)
	// A configuration save announces a config event for the whole service,
	// without a project.
	f.p.Await("the other client's save announced", `managementConfigEvents.some(e => e.kind === 'config')`)
	f.p.Await("the untouched form follows the save and its pin", `document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === '45m' && document.querySelector('#project-config').dataset.pin === `+quote(other.Pin)+` && document.querySelector('#config-changed').hidden`)

	f.p.SetValue(settingInput("capacity.git_operations"), "2")
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "ok")
	f.p.Await("the save was pinned to the followed version", `managementWrites.at(-1).body.pin === `+quote(other.Pin))
	mine, err := f.c.ProjectConfig(ctx, f.project)
	must(t, err)

	const draft = "9"
	f.p.SetValue(settingInput("capacity.git_operations"), draft)
	later, err := f.c.SaveProjectConfig(ctx, f.project, ProjectConfigEdit{Pin: mine.Pin, Set: ProjectConfigEditValues{ChecksTimeout: textPtr("50m")}})
	must(t, err)
	f.p.Await("the draft and its pin kept, with the notice", `!document.querySelector('#config-changed').hidden &&
      document.querySelector(`+quote(settingInput("capacity.git_operations"))+`).value === `+quote(draft)+` &&
      document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === '45m' &&
      document.querySelector('#project-config').dataset.pin === `+quote(mine.Pin))
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "error")
	if got := f.effective(); got.ChecksTimeout != "50m" || got.Capacity.GitOperations != 2 {
		t.Fatalf("the stale draft overwrote the other client: %+v", got)
	}
	f.p.Click("#project-config [data-action=read-config]")
	managementOutcome(f.p, settingsResult(), "ok")
	f.p.Await("the latest values and version", `document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === '50m' && document.querySelector('#project-config').dataset.pin === `+quote(later.Pin)+` && document.querySelector('#config-changed').hidden`)
	f.p.Await("the same page", `window.sameDocument === true`)
}

func textPtr(s string) *string { return &s }
