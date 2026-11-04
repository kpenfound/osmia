package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
)

func TestProjectPreviewSaves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := fixture(t)
	files := configFiles(t, opts)
	original := files.projectText + "# keep this comment\n\n[capacity] # custom spacing\nper_workstream=3\ngit_operations = 2\n"
	files.write(t, files.topText, original)
	s, c := start(t, opts)
	request := api.ProjectPreviewRequest{Call: []string{"serve", "--dev"}, Port: 8080}
	saved, err := c.SetProjectPreview(ctx, project, request)
	must(t, err)
	if saved.Preview == nil || saved.Preview.Path != "/" || saved.Preview.Timeout != "5m" {
		t.Fatalf("defaults: %+v", saved)
	}
	check := func(want *config.Preview) {
		t.Helper()
		cfg, err := c.Configuration(ctx)
		must(t, err)
		got, _ := json.Marshal(cfg.Effective.For(project).Project.Preview)
		expected, _ := json.Marshal(want)
		if string(got) != string(expected) {
			t.Fatalf("loaded %s want %s", got, expected)
		}
		current, _ := json.Marshal(s.current().For(project).Project.Preview)
		if string(current) != string(expected) {
			t.Fatalf("runtime %s", current)
		}
		for _, d := range cfg.Diagnostics {
			if d.Code == ReloadRequired {
				t.Fatal(d)
			}
		}
	}
	check(saved.Preview)
	first, err := os.ReadFile(files.project)
	must(t, err)
	if !strings.HasPrefix(string(first), original) {
		t.Fatalf("changed other bytes:\n%s", first)
	}
	info, err := os.Stat(files.project)
	must(t, err)
	_, err = c.SetProjectPreview(ctx, project, request)
	must(t, err)
	same, err := os.Stat(files.project)
	must(t, err)
	if !os.SameFile(info, same) {
		t.Fatal("idempotent set replaced the file")
	}
	request.Port = 8081
	saved, err = c.SetProjectPreview(ctx, project, request)
	must(t, err)
	check(saved.Preview)
	_, err = c.ClearProjectPreview(ctx, project)
	must(t, err)
	check(nil)
	cleared, err := os.ReadFile(files.project)
	must(t, err)
	if string(cleared) != original {
		t.Fatalf("clear changed other bytes:\n%s", cleared)
	}
	_, err = c.ClearProjectPreview(ctx, project)
	must(t, err)
	// Reload refreshes the byte baseline, including comments and formatting.
	hand := strings.Replace(original, "git_operations = 2", "git_operations  =  2", 1) + "# owner's hand edit\n"
	files.write(t, files.topText, hand)
	_, err = c.Reload(ctx)
	must(t, err)
	_, err = c.SetProjectPreview(ctx, project, request)
	must(t, err)
	updated, err := os.ReadFile(files.project)
	must(t, err)
	if !strings.HasPrefix(string(updated), hand) {
		t.Fatalf("lost hand edit:\n%s", updated)
	}
}

func TestProjectPreviewRefusesByteDrift(t *testing.T) {
	t.Parallel()
	for _, edit := range []string{"comment", "format", "value"} {
		for _, operation := range []string{"set", "same", "clear", "absent clear"} {
			t.Run(edit+"/"+operation, func(t *testing.T) {
				opts := fixture(t)
				files := configFiles(t, opts)
				_, c := start(t, opts)
				ctx := context.Background()
				req := api.ProjectPreviewRequest{Call: []string{"serve"}, Port: 80}
				if operation != "absent clear" {
					_, err := c.SetProjectPreview(ctx, project, req)
					must(t, err)
				}
				before, err := os.ReadFile(files.project)
				must(t, err)
				hand := string(before)
				switch edit {
				case "comment":
					hand = "# owner comment\n" + hand
				case "format":
					hand = strings.Replace(hand, "=", " = ", 1)
				case "value":
					hand = strings.Replace(hand, "version = 1", "version = 2", 1)
				}
				if hand == string(before) {
					t.Fatal("fixture edit changed nothing")
				}
				files.write(t, files.topText, hand)
				if operation == "set" {
					req.Port = 81
				}
				if operation == "set" || operation == "same" {
					_, err = c.SetProjectPreview(ctx, project, req)
				} else {
					_, err = c.ClearProjectPreview(ctx, project)
				}
				var failure *api.APIError
				if !errors.As(err, &failure) || failure.Code != Conflict || !strings.Contains(failure.Message, "drift") || !strings.Contains(failure.Message, "config.toml") {
					t.Fatalf("expected drift conflict: %v", err)
				}
				after, err := os.ReadFile(files.project)
				must(t, err)
				if string(after) != hand {
					t.Fatal("refusal overwrote hand edit")
				}
			})
		}
	}
}

func TestProjectPreviewAPIValidation(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	files := configFiles(t, opts)
	_, c := start(t, opts)
	before, err := os.ReadFile(files.project)
	must(t, err)
	for _, tc := range []struct{ body, field string }{
		{`{}`, "call"}, {`{"port":80}`, "call"}, {`{"call":[] ,"port":80}`, "call"},
		{`{"call":["serve"]}`, "port"}, {`{"call":["serve"],"port":0}`, "port"}, {`{"call":["serve"],"port":65536}`, "port"},
		{`{"call":["serve"],"port":80,"other":1}`, "other"},
		{`{"call":["serve"],"port":80,"nested":{"key":1}}`, "nested"},
		{`{"call":{"nested":1},"port":80}`, "call"},
		{`{"call":"serve","port":80}`, "call"}, {`{"call":[1],"port":80}`, "call"},
		{`{"call":["serve"],"port":"80"}`, "port"}, {`{"call":["serve"],"port":80,"path":1}`, "path"},
		{`{"call":["serve"],"port":80,"timeout":1}`, "timeout"},
		{`{"call":["serve"],"port":80,"path":"missing-slash"}`, "path"},
		{`{"call":["serve"],"port":80,"path":""}`, "path"},
		{`{"call":["serve"],"port":80,"timeout":"59s"}`, "timeout"},
		{`{"call":["serve"],"port":80,"timeout":"31m"}`, "timeout"},
		{`{"call":["serve"],"port":80,"timeout":null}`, "timeout"},
	} {
		var body map[string]any
		must(t, json.Unmarshal([]byte(tc.body), &body))
		var out api.ProjectPreviewResponse
		err := c.Do(context.Background(), http.MethodPut, Prefix+"/projects/preview/"+string(project), body, &out)
		var failure *api.APIError
		if !errors.As(err, &failure) || failure.Code != Validation || !strings.Contains(failure.Message, "preview."+tc.field) {
			t.Fatalf("%s: want %s validation: %v", tc.body, tc.field, err)
		}
		after, err := os.ReadFile(files.project)
		must(t, err)
		if string(after) != string(before) {
			t.Fatal("validation wrote the file")
		}
	}
}

func TestProjectPreviewAfterProjectSplit(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	files := configFiles(t, opts)
	files.write(t, files.topText, "group_id = \"\" # own charter\n"+files.projectText)
	_, c := start(t, opts)
	_, err := c.SetProjectPreview(context.Background(), project, api.ProjectPreviewRequest{Call: []string{"serve"}, Port: 80})
	must(t, err)
}

func TestProjectPreviewEditsExistingTable(t *testing.T) {
	t.Parallel()
	opts := fixture(t)
	files := configFiles(t, opts)
	before := "# owner note\n" + files.projectText + "\n"
	table := "['preview']\ncall = [\n  'serve',\n  '--dev',\n]\nport = 80\n"
	after := "\n# capacity stays exact\n[capacity]\nper_workstream  =  4\ngit_operations=2\n"
	files.write(t, files.topText, before+table+after)
	_, c := start(t, opts)
	_, err := c.SetProjectPreview(context.Background(), project, api.ProjectPreviewRequest{Call: []string{"web"}, Port: 81})
	must(t, err)
	data, err := os.ReadFile(files.project)
	must(t, err)
	if !strings.HasPrefix(string(data), before+after) {
		t.Fatalf("outside table changed:\n%s", data)
	}
	_, err = c.ClearProjectPreview(context.Background(), project)
	must(t, err)
	data, err = os.ReadFile(files.project)
	must(t, err)
	if string(data) != before+after {
		t.Fatalf("outside table changed:\n%s", data)
	}
}
