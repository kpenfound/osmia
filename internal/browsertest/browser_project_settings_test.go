package browsertest

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/projectstore"
	"github.com/kpenfound/osmia/internal/service"
)

type ProjectConfigEdit = service.ProjectConfigEdit
type ProjectConfigEditValues = service.ProjectConfigEditValues

const ownerTables = "\n[capacity]\n# Keep this low.\ngit_operations = 3\n"

type settingsFixture struct {
	t       *testing.T
	s       *service.Service
	c       *service.Client
	p       *page
	project config.ProjectID
	root    config.Root
	before  string
}

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
	root, err := config.ResolveRoot(opts.Config.Root, "")
	must(t, err)
	store := projectstore.Open(root)
	base, err := store.Current(project)
	must(t, err)
	text := "# Owner notes.\n" + string(base.Data) + rootKeys + ownerTables
	owned, err := store.Replace(project, snapshot.Pin, []byte(text))
	must(t, err)
	_, err = c.SaveProjectConfig(ctx, project, ProjectConfigEdit{Pin: owned.Pin})
	must(t, err)
	f := &settingsFixture{t: t, s: s, c: c, project: project, root: root}
	f.before = f.disk()
	return f
}

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
	f.p.AwaitText("#config-file", "Stored settings of project "+string(f.project))
	f.p.Await("settings read", `document.querySelector('#project-config').dataset.pin !== ''`)
}

func settingInput(name string) string { return `#project-config [name="` + name + `"]` }

func settingMarker(name, part string) string {
	return `#project-config [data-setting="` + name + `"] [data-marker=` + part + `]`
}

func settingsResult() string { return "#project-config .result" }

func (f *settingsFixture) disk() string {
	f.t.Helper()
	revision, err := projectstore.Open(f.root).Current(f.project)
	must(f.t, err)
	return string(revision.Data)
}

func (f *settingsFixture) hand(from, to string) string {
	f.t.Helper()
	store := projectstore.Open(f.root)
	current, err := store.Current(f.project)
	must(f.t, err)
	edited := strings.Replace(string(current.Data), from, to, 1)
	if edited == string(current.Data) {
		f.t.Fatalf("%q is not in the stored settings:\n%s", from, current.Data)
	}
	_, err = store.Replace(f.project, current.Pin, []byte(edited))
	must(f.t, err)
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
		t.Fatalf("line %q of the stored settings was dropped or reordered:\nbefore:\n%s\nafter:\n%s", line, before, after)
	}
}

func TestBrowserProjectManagementSavesProjectSettingsToTheStore(t *testing.T) {
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
	f.p.Await("every setting of the stored settings, in order", `[...document.querySelectorAll('#config-fields [data-setting]')].map(n => n.dataset.setting).join(',') === `+quote(strings.Join(names, ",")))
	f.p.Await("omitted settings marked as defaults and no others", `[...document.querySelectorAll('#config-fields [data-setting]')].filter(n => n.querySelector('[data-marker=default]').textContent !== '').map(n => n.dataset.setting).join(',') === `+quote(strings.Join(defaults, ",")))
	f.p.Await("no setting pending", `[...document.querySelectorAll('#config-fields [data-marker=pending]')].every(n => n.textContent === '')`)
	for _, setting := range read.Settings {
		if text, ok := setting.Value.(string); ok {
			f.p.Await("the stored value of "+setting.Name, `document.querySelector(`+quote(settingInput(setting.Name))+`).value === `+quote(text))
		}
	}
	f.p.Await("the saved capacity from the stored settings", `document.querySelector(`+quote(settingInput("capacity.git_operations"))+`).value === '3'`)
	f.p.Await("a default shown with its value", `document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === `+quote(config.DefaultChecksTimeout))
	f.p.AwaitText(settingMarker("capacity.per_workstream", "default"), "inherited")
	f.p.Await("settings fit at phone width", `document.documentElement.scrollWidth <= innerWidth && document.querySelector('#config-fields').getBoundingClientRect().right <= innerWidth`)

	f.p.SetValue(settingInput("checks_timeout"), "30m")
	f.p.SetValue(settingInput("capacity.git_operations"), "5")
	f.p.SetValue(settingInput("hearsay_scope"), "work:main:shop")
	f.p.SetValue(settingInput("hearsay_entities"), "cart = code:shop:cart")
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "ok")

	disk := f.disk()
	for _, want := range []string{`checks_timeout = "30m"`, "git_operations = 5", `hearsay_scope = "work:main:shop"`, `cart = "code:shop:cart"`} {
		if !strings.Contains(disk, want) {
			t.Fatalf("%s is not in the stored settings:\n%s", want, disk)
		}
	}
	keepsOtherBytes(t, f.before, disk, "git_operations = 3")
	for _, kept := range []string{"# Owner notes.", "[capacity]", "# Keep this low."} {
		if !strings.Contains(disk, kept) {
			t.Fatalf("%q was lost from the stored settings:\n%s", kept, disk)
		}
	}
	p := f.effective()
	if p.ChecksTimeout != "30m" || p.Capacity.GitOperations != 5 || p.HearsayScope != "work:main:shop" || p.HearsayEntities["cart"] != "code:shop:cart" {
		t.Fatalf("in effect: %+v", p)
	}
	saved, err := f.c.ProjectConfig(ctx, f.project)
	must(t, err)
	f.p.Await("the save sent only the changed settings, pinned to the revision it read", `(() => {
      const w = managementWrites.at(-1);
      return w.method === 'PUT' && w.body.pin === `+quote(read.Pin)+` && Object.keys(w.body.set).sort().join(',') === 'capacity.git_operations,checks_timeout,hearsay_entities,hearsay_scope' && w.body.clear.length === 0;
    })()`)
	f.p.Await("the form holds the saved revision, without a pending value", `document.querySelector('#project-config').dataset.pin === `+quote(saved.Pin)+` && document.querySelector('#project-config .result').textContent.includes('Saved project settings as revision') && document.querySelector(`+quote(settingMarker("checks_timeout", "default"))+`).textContent === '' && document.querySelector(`+quote(settingMarker("checks_timeout", "pending"))+`).textContent === ''`)
}

func TestBrowserProjectManagementShowsAnUnappliedRevisionAndKeepsItOnSave(t *testing.T) {
	t.Parallel()
	f := newSettingsFixture(t, "")
	f.hand("landing = \"commit-per-unit\"\n", "landing = \"commit-per-unit\"\nchecks_timeout = \"20m\"\n")
	if p := f.effective(); p.ChecksTimeout != config.DefaultChecksTimeout {
		t.Fatalf("the unapplied revision was applied: %+v", p)
	}
	f.open(1280, 800)

	f.p.Await("the stored value", `document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === '20m'`)
	f.p.AwaitText(settingMarker("checks_timeout", "pending"), "Not yet applied. In effect: "+config.DefaultChecksTimeout)
	f.p.Await("only that setting pending and set in the stored settings", `[...document.querySelectorAll('#config-fields [data-marker=pending]')].filter(n => n.textContent !== '').length === 1 && document.querySelector(`+quote(settingMarker("checks_timeout", "default"))+`).textContent === ''`)

	f.p.SetValue(settingInput("capacity.git_operations"), "2")
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "ok")
	disk := f.disk()
	if !strings.Contains(disk, `checks_timeout = "20m"`) || !strings.Contains(disk, "git_operations = 2") {
		t.Fatalf("the earlier revision's edit or the saved value is not in the stored settings:\n%s", disk)
	}
	p := f.effective()
	if p.ChecksTimeout != "20m" || p.Capacity.GitOperations != 2 {
		t.Fatalf("both changes are in effect: %+v", p)
	}
	f.p.Await("nothing pending after the save", `[...document.querySelectorAll('#config-fields [data-marker=pending]')].every(n => n.textContent === '')`)
}

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
		t.Fatalf("a refused save changed the stored settings:\n%s", f.disk())
	}
	if p := f.effective(); p.ChecksTimeout != config.DefaultChecksTimeout {
		t.Fatalf("a refused save changed the loaded configuration: %+v", p)
	}

	f.p.SetValue(settingInput("checks_timeout"), "30m")
	f.p.SetValue(settingInput("landing"), "sideways")
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "error")
	f.p.AwaitText(settingsResult(), "landing")
	f.p.Await("both fields of the draft kept", `document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === '30m' && document.querySelector(`+quote(settingInput("landing"))+`).value === 'sideways'`)
	if f.disk() != f.before {
		t.Fatalf("a refused save changed the stored settings:\n%s", f.disk())
	}

	f.p.SetValue(settingInput("landing"), "squash")
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "ok")
	if p := f.effective(); p.ChecksTimeout != "30m" || p.Landing != "squash" {
		t.Fatalf("the corrected draft in effect: %+v", p)
	}
}

func TestBrowserProjectManagementRefusesAStaleSaveAndLoadsTheLatestValues(t *testing.T) {
	t.Parallel()
	f := newSettingsFixture(t, "")
	f.open(1280, 800)
	f.p.Await("the stored capacity", `document.querySelector(`+quote(settingInput("capacity.git_operations"))+`).value === '3'`)

	f.p.SetValue(settingInput("capacity.git_operations"), "6")
	edited := f.hand("landing = \"commit-per-unit\"\n", "landing = \"commit-per-unit\"\nchecks_timeout = \"20m\"\n")
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "error")
	f.p.AwaitText(settingsResult(), "changed since they were read")
	f.p.Await("the stale notice and the draft", `!document.querySelector('#config-changed').hidden && document.querySelector(`+quote(settingInput("capacity.git_operations"))+`).value === '6'`)
	if f.disk() != edited {
		t.Fatalf("a stale save wrote to the stored settings:\n%s", f.disk())
	}

	f.p.Click("#project-config [data-action=read-config]")
	managementOutcome(f.p, settingsResult(), "ok")
	f.p.Await("the latest stored values, with the other revision not yet applied", `document.querySelector('#config-changed').hidden &&
      document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === '20m' &&
      document.querySelector(`+quote(settingInput("capacity.git_operations"))+`).value === '3' &&
      document.querySelector(`+quote(settingMarker("checks_timeout", "pending"))+`).textContent.includes('In effect: `+config.DefaultChecksTimeout+`')`)
	if p := f.effective(); p.ChecksTimeout != config.DefaultChecksTimeout {
		t.Fatalf("loading the latest values reloaded the service: %+v", p)
	}
}

func TestBrowserProjectManagementClearsASettingToItsDefault(t *testing.T) {
	t.Parallel()
	f := newSettingsFixture(t, "checks_timeout = \"30m\" # the owner's choice\n")
	if p := f.effective(); p.ChecksTimeout != "30m" || p.Fork == "" {
		t.Fatalf("before: %+v", p)
	}
	f.open(1280, 800)
	f.p.Await("the stored timeout, not a default", `document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === '30m' && document.querySelector(`+quote(settingMarker("checks_timeout", "default"))+`).textContent === ''`)

	f.p.SetValue(settingInput("checks_timeout"), "")
	f.p.SetValue(settingInput("fork"), "")
	f.p.Click("#project-config [type=submit]")
	managementOutcome(f.p, settingsResult(), "ok")
	disk := f.disk()
	if strings.Contains(disk, "checks_timeout") || strings.Contains(disk, "fork =") {
		t.Fatalf("cleared keys are still in the stored settings:\n%s", disk)
	}
	for _, kept := range []string{"# Owner notes.", "[capacity]", "# Keep this low.", "git_operations = 3", "upstream = "} {
		if !strings.Contains(disk, kept) {
			t.Fatalf("%q was lost from the stored settings:\n%s", kept, disk)
		}
	}
	if p := f.effective(); p.ChecksTimeout != config.DefaultChecksTimeout || p.Fork != "" {
		t.Fatalf("defaults in effect: %+v", p)
	}
	f.p.Await("the defaults shown as in effect", `document.querySelector(`+quote(settingInput("checks_timeout"))+`).value === `+quote(config.DefaultChecksTimeout)+` && document.querySelector(`+quote(settingMarker("checks_timeout", "default"))+`).textContent !== '' && document.querySelector(`+quote(settingInput("fork"))+`).value === '' && document.querySelector(`+quote(settingMarker("fork", "default"))+`).textContent !== ''`)
}

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
