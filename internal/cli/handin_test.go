package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	wire "github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/issues"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/trace"
)

type issueText map[issues.Ref]string

func (m issueText) Fetch(_ context.Context, ref issues.Ref) (string, error) {
	if text, ok := m[ref]; ok {
		return text, nil
	}
	return "", errors.New("not found")
}

func invokeInput(t *testing.T, root string, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, diag bytes.Buffer
	code := Run(context.Background(), append([]string{"--root", root}, args...), strings.NewReader(stdin), &out, &diag)
	return code, out.String(), diag.String()
}

func handInFixture(t *testing.T) (string, config.ProjectID, string) {
	t.Helper()
	opts, clone := emptyFixture(t)
	opts.Issues = issueText{{Owner: "owner", Repo: "repo", Number: 3}: "# Title\n\nBody\n"}
	s, err := service.Start(context.Background(), opts)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	resolved, err := config.ResolveRoot(opts.Config.Root, "")
	must(t, err)
	root := resolved.String()
	var added wire.ProjectResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "project", "add", "dagger", "--upstream", "dagger/dagger", "--fork", "owner/dagger", "--clone", clone, "--json")), &added))
	setCharter(t, root, added.Project.ID, "1. Keep changes small.\n")
	return root, added.Project.ID, filepath.Dir(clone)
}

func handedWorkstream(t *testing.T, root string, project config.ProjectID, stream config.WorkstreamID) (trace.Document, string) {
	t.Helper()
	bodies := traceBodies(t, workstreamDatabase(t, root, project, string(stream)), "workstreams/"+string(stream)+"/documents.jsonl")
	if len(bodies) == 0 {
		t.Fatalf("workstream %s records no handed document", stream)
	}
	var doc trace.Document
	must(t, json.Unmarshal([]byte(bodies[0]), &doc))
	return doc, doc.Content
}

func handedCount(t *testing.T, root string, project config.ProjectID) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "workstreams", string(project)))
	if os.IsNotExist(err) {
		return 0
	}
	must(t, err)
	return len(entries)
}

func designFile(t *testing.T, home string) string {
	t.Helper()
	design := filepath.Join(home, "design.md")
	must(t, os.WriteFile(design, []byte("# Design\n"), 0600))
	return design
}

func TestHandInFileCopiesItIntoANewWorkstream(t *testing.T) {
	root, project, home := handInFixture(t)
	design := designFile(t, home)
	wd, err := os.Getwd()
	must(t, err)
	relative, err := filepath.Rel(wd, design)
	must(t, err)
	var result wire.HandInResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "handin", string(project), relative, "--json")), &result))
	if result.Project != project || result.State != "handed" || result.Source != "file:"+design || config.CheckWorkstreamIDs(result.Workstream) != nil {
		t.Fatalf("file hand-in %+v", result)
	}
	if doc, handed := handedWorkstream(t, root, project, result.Workstream); doc.Source != "file:"+design || handed != "# Design\n" || doc.Path != "handed/design.md" {
		t.Fatalf("file copy %+v %q", doc, handed)
	}
}

func TestHandInIssueURLCopiesTheIssueText(t *testing.T) {
	root, project, _ := handInFixture(t)
	text := successful(t, root, "handin", string(project), "https://github.com/owner/repo/issues/3")
	lines := strings.Split(text, "\n")
	stream, ok := strings.CutPrefix(lines[0], "Workstream ")
	stream, ok2 := strings.CutSuffix(stream, " handed in to project "+string(project))
	if !ok || !ok2 || len(lines) != 4 || lines[1] != "State: handed" || lines[2] != "Source: https://github.com/owner/repo/issues/3" || lines[3] != "" {
		t.Fatalf("URL hand-in output:\n%s", text)
	}
	id := config.WorkstreamID(stream)
	if doc, handed := handedWorkstream(t, root, project, id); doc.Source != "https://github.com/owner/repo/issues/3" || handed != "# Title\n\nBody\n" || doc.Path != "handed/issue-3.md" {
		t.Fatalf("issue copy %+v %q", doc, handed)
	}
}

func TestHandInStdinCopiesTheInputVerbatim(t *testing.T) {
	root, project, _ := handInFixture(t)
	stdin := "line one\r\n\ttabbed é\n"
	code, out, diag := invokeInput(t, root, stdin, "handin", string(project), "-", "--json")
	if code != 0 || diag != "" {
		t.Fatalf("stdin hand-in: %d %s %s", code, out, diag)
	}
	var result wire.HandInResponse
	must(t, json.Unmarshal([]byte(out), &result))
	if doc, handed := handedWorkstream(t, root, project, result.Workstream); result.Source != "stdin" || doc.Source != "stdin" || handed != stdin || doc.Path != "handed/stdin" {
		t.Fatalf("stdin copy %+v %+v %q", result, doc, handed)
	}
}

func TestEachHandInCreatesItsOwnWorkstream(t *testing.T) {
	root, project, home := handInFixture(t)
	design := designFile(t, home)
	first := successful(t, root, "handin", string(project), design)
	second := successful(t, root, "handin", string(project), design)
	if strings.Split(first, "\n")[0] == strings.Split(second, "\n")[0] {
		t.Fatalf("second hand-in reused a workstream:\n%s", second)
	}
	if n := handedCount(t, root, project); n != 2 {
		t.Fatalf("%d workstreams", n)
	}
}

func TestRefusedHandInsCreateNoWorkstream(t *testing.T) {
	root, project, home := handInFixture(t)
	for _, c := range []struct {
		stdin string
		args  []string
		code  int
		diag  string
	}{
		{"", []string{"-"}, 4, "validation: handed input is empty\n"},
		{"bad \xff byte", []string{"-"}, 4, "stdin is not UTF-8 text"},
		{strings.Repeat("x", service.MaxHandedBytes+1), []string{"-"}, 4, "larger than"},
		{"", []string{"http://github.com/owner/repo/issues/3"}, 4, "validation: issue URL must be https://github.com/OWNER/REPO/issues/NUMBER\n"},
		{"", []string{"https://github.com/owner/repo/issues/4"}, 5, "internal: cannot fetch https://github.com/owner/repo/issues/4; check the URL and the service's GitHub access\n"},
		{"", []string{home}, 4, "validation: " + home + " is not a regular file\n"},
	} {
		code, out, diag := invokeInput(t, root, c.stdin, append([]string{"handin", string(project)}, c.args...)...)
		if code != c.code || out != "" || !strings.Contains(diag, c.diag) {
			t.Errorf("%v: %d %q %q", c.args, code, out, diag)
		}
	}
	if n := handedCount(t, root, project); n != 0 {
		t.Fatalf("refused hand-ins created workstreams: %d", n)
	}
}

func TestHandInKeys(t *testing.T) {
	a, err := handInKey()
	must(t, err)
	b, err := handInKey()
	must(t, err)
	if a == b || len(a) != 36 || !strings.HasPrefix(a, "cli-") {
		t.Fatalf("keys %q %q", a, b)
	}
	stream := service.HandInWorkstream("p_0123456789abcdef0123456789abcdef", a)
	if config.CheckWorkstreamIDs(stream) != nil || stream == service.HandInWorkstream("p_0123456789abcdef0123456789abcdef", b) {
		t.Fatalf("workstream %q", stream)
	}
}

type skipDebateRoot struct {
	root    string
	project config.ProjectID
	design  string
}

func newSkipDebateRoot(t *testing.T) skipDebateRoot {
	t.Helper()
	root, project, home := handInFixture(t)
	design := filepath.Join(home, "design.md")
	must(t, os.WriteFile(design, []byte("# Design\n"), 0600))
	return skipDebateRoot{root: root, project: project, design: design}
}

func (r skipDebateRoot) skips(t *testing.T, stream string) []trace.Transition {
	t.Helper()
	var out []trace.Transition
	for _, line := range traceBodies(t, workstreamDatabase(t, r.root, r.project, stream), "workstreams/"+stream+"/events.jsonl") {
		var tr trace.Transition
		must(t, json.Unmarshal([]byte(line), &tr))
		if tr.ID == "shed-owner-skip" {
			out = append(out, tr)
		}
	}
	return out
}

func TestHandInSkipDebateReportsTheSkippedDebate(t *testing.T) {
	r := newSkipDebateRoot(t)
	text := successful(t, r.root, "handin", string(r.project), r.design, "--skip-debate")
	lines := strings.Split(text, "\n")
	if !strings.HasPrefix(lines[0], "Workstream ") || !strings.HasSuffix(lines[0], " handed in to project "+string(r.project)) || len(lines) != 5 || lines[1] != "State: handed" || lines[2] != "Source: file:"+r.design ||
		lines[3] != "Debate: skipped; ratify the spec and the plan once they are drafted" || lines[4] != "" {
		t.Fatalf("hand-in output:\n%s", text)
	}
}

func TestHandInSkipDebateRecordsTheOwnersSkip(t *testing.T) {
	r := newSkipDebateRoot(t)
	var result wire.HandInResponse
	must(t, json.Unmarshal([]byte(successful(t, r.root, "handin", string(r.project), r.design, "--skip-debate", "--json")), &result))
	if !result.SkipDebate || result.State != "handed" {
		t.Fatalf("JSON hand-in %+v", result)
	}
	if got := r.skips(t, string(result.Workstream)); len(got) != 1 || got[0].Subject != "shed-owner" || got[0].To != "skipped" || got[0].Cause != "handin" ||
		got[0].Reason != "the owner skipped debate at hand-in; the workstream still needs the owner's ratification of the spec and the plan" {
		t.Fatalf("skip transitions %+v", got)
	}
}

func TestHandInWithoutSkipDebateRecordsNoSkip(t *testing.T) {
	r := newSkipDebateRoot(t)
	var debated wire.HandInResponse
	must(t, json.Unmarshal([]byte(successful(t, r.root, "handin", string(r.project), r.design, "--json")), &debated))
	if debated.SkipDebate || len(r.skips(t, string(debated.Workstream))) != 0 {
		t.Fatalf("hand-in without the flag %+v", debated)
	}
}

func TestSkipDebateIsRefusedOutsideHandIn(t *testing.T) {
	r := newSkipDebateRoot(t)
	for _, args := range [][]string{{"handin", string(r.project), r.design, "--skip-debate=true"}, {"status", "--skip-debate"}, {"shed", "skip", "w_0123456789abcdef0123456789abcdef", "--skip-debate"}} {
		code, out, diag := invokeInput(t, r.root, "", args...)
		if code != 2 || out != "" || diag != "invalid arguments; use osmia --help\n" {
			t.Errorf("%v: %d %q %q", args, code, out, diag)
		}
	}
}

func TestHandInWithSendsOrderedProjects(t *testing.T) {
	t.Parallel()
	a := config.ProjectID("p_11111111111111111111111111111111")
	b := config.ProjectID("p_22222222222222222222222222222222")
	c := config.ProjectID("p_33333333333333333333333333333333")
	requests := make(chan wire.HandInRequest, 1)
	root, socket := ratifyServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/handin" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req wire.HandInRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		requests <- req
		json.NewEncoder(w).Encode(wire.HandInResponse{Project: a, Workstream: service.HandInWorkstream(a, req.Key), State: "handed", Source: "file:" + req.Path})
	}))
	code, out, diag := invoke(t, root, "handin", string(a), "brief.md", "--with", string(b), "--with", string(c), "--socket", socket)
	if code != 0 || diag != "" {
		t.Fatalf("handin with: %d %s %s", code, out, diag)
	}
	req := <-requests
	if !slices.Equal(req.Projects, []config.ProjectID{a, b, c}) || req.Project != a || filepath.Base(req.Path) != "brief.md" {
		t.Fatalf("handin projects: %+v", req)
	}
	for _, args := range [][]string{{"status", "--with", string(b)}, {"handin", string(a), "brief.md", "--with", "bad"}} {
		code, _, _ := invoke(t, root, append(args, "--socket", socket)...)
		if code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
