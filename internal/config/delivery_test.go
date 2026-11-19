package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const noForkProjectConfig = `version = 1
upstream = "upstream/repo"
clone = "~/clone"
`

func TestDeliveryDefaultsToPullRequest(t *testing.T) {
	for name, project := range map[string]string{"no fork": noForkProjectConfig, "fork": projectConfig} {
		c, err := Load(fixture(t, topConfig, project))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Project.Delivery != DeliveryPullRequest || c.Project.Lands() {
			t.Fatalf("%s: delivery %q", name, c.Project.Delivery)
		}
	}
}

func TestDeliveryAcceptsBothValues(t *testing.T) {
	for _, tc := range []struct {
		value string
		lands bool
	}{{"pull-request", false}, {"jev", true}} {
		c, err := Load(fixture(t, topConfig, noForkProjectConfig+"delivery = '"+tc.value+"'\n"))
		if err != nil {
			t.Fatalf("%s: %v", tc.value, err)
		}
		if c.Project.Delivery != tc.value || c.Project.Lands() != tc.lands {
			t.Fatalf("%s: %+v", tc.value, c.Project)
		}
	}
}

func TestDeliveryRejectsOtherValues(t *testing.T) {
	for _, value := range []string{"'merge'", "'Jev'", "'pull_request'", "''", "true", "1"} {
		c, err := Load(fixture(t, topConfig, noForkProjectConfig+"delivery = "+value+"\n"))
		var field *FieldError
		if c != nil || !errors.As(err, &field) || field.Field != "delivery" || !strings.Contains(field.Path, pid) {
			t.Fatalf("%s: %v %v", value, c, err)
		}
	}
}

func TestDeliveryJevIsRejectedOnAForkProject(t *testing.T) {
	c, err := Load(fixture(t, topConfig, projectConfig+"delivery = 'jev'\n"))
	var field *FieldError
	if c != nil || !errors.As(err, &field) || field.Field != "delivery" {
		t.Fatalf("fork project with jev: %v %v", c, err)
	}
	c, err = Load(fixture(t, topConfig, projectConfig+"delivery = 'pull-request'\n"))
	if err != nil || c.Project.Delivery != DeliveryPullRequest || c.Project.PushRepository() != "owner/repo" {
		t.Fatalf("fork project with pull-request: %v %v", c, err)
	}
	// A fork that names the upstream is not a fork.
	c, err = Load(fixture(t, topConfig, strings.Replace(projectConfig, `fork = "owner/repo"`, `fork = "UPSTREAM/repo"`, 1)+"delivery = 'jev'\n"))
	if err != nil || !c.Project.Lands() {
		t.Fatalf("fork equal to upstream: %v %v", c, err)
	}
}

func TestEditProjectRoundTripsAndValidatesDelivery(t *testing.T) {
	c, opts := projectEditFixture(t, noForkProjectConfig)
	pinned := []byte(noForkProjectConfig)
	str := func(name, value string) ProjectEdit { return ProjectEdit{Strings: map[string]string{name: value}} }

	edited, file, err := c.EditProject(pid, opts.Home, pinned, str("delivery", "jev"))
	if err != nil {
		t.Fatal(err)
	}
	if want := noForkProjectConfig + "delivery = \"jev\"\n"; string(edited) != want || !file.Project.Lands() || !file.Set("delivery") {
		t.Fatalf("edited %q: %+v", edited, file.Project)
	}
	write(t, filepath.Join(c.Root.String(), "projects", pid, "config.toml"), string(edited))
	fresh, err := Load(opts)
	if err != nil || fresh.Project.Delivery != DeliveryJev {
		t.Fatalf("reload: %v %v", fresh, err)
	}

	cleared, file, err := c.EditProject(pid, opts.Home, edited, ProjectEdit{Clear: []string{"delivery"}})
	if err != nil || string(cleared) != noForkProjectConfig || file.Project.Delivery != DeliveryPullRequest || file.Set("delivery") {
		t.Fatalf("clear: %q %+v %v", cleared, file.Project, err)
	}

	for name, tc := range map[string]struct {
		pinned []byte
		edit   ProjectEdit
	}{
		"other value":         {pinned, str("delivery", "merge")},
		"adding a fork":       {edited, str("fork", "owner/repo")},
		"jev beside the fork": {[]byte(projectConfig), str("delivery", "jev")},
	} {
		_, _, err := c.EditProject(pid, opts.Home, tc.pinned, tc.edit)
		var field *FieldError
		if !errors.As(err, &field) || field.Field != "delivery" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWriteProjectConfigKeepsAndValidatesDelivery(t *testing.T) {
	clone := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.toml")
	p := Project{Name: "x", Upstream: "up/repo", Clone: clone, BaseBranch: "main", Delivery: DeliveryJev}
	if err := WriteProjectConfig(path, p); err != nil || !strings.Contains(read(t, path), "delivery = \"jev\"\n") {
		t.Fatalf("jev: %v\n%s", err, read(t, path))
	}
	p.Delivery = ""
	if err := WriteProjectConfig(path, p); err != nil || !strings.Contains(read(t, path), "delivery = \"pull-request\"\n") {
		t.Fatalf("default: %v\n%s", err, read(t, path))
	}
	for name, bad := range map[string]Project{
		"other value": {Upstream: "up/repo", Clone: clone, BaseBranch: "main", Delivery: "merge"},
		"jev on fork": {Upstream: "up/repo", Fork: "me/repo", Clone: clone, BaseBranch: "main", Delivery: DeliveryJev},
	} {
		if err := WriteProjectConfig(filepath.Join(t.TempDir(), "config.toml"), bad); err == nil {
			t.Errorf("%s written", name)
		}
	}
}
