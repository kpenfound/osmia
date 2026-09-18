package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// handInFixture starts a service with an active project whose charter has a
// rule, and returns its root and project ID.
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
	var added service.ProjectResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "project", "add", "dagger", "--upstream", "dagger/dagger", "--fork", "owner/dagger", "--clone", clone, "--json")), &added))
	must(t, os.WriteFile(added.Project.Charter, []byte("1. Keep changes small.\n"), 0600))
	return root, added.Project.ID, filepath.Dir(clone)
}

func handedWorkstream(t *testing.T, root string, project config.ProjectID, stream config.WorkstreamID) (trace.Document, string) {
	t.Helper()
	path := filepath.Join(root, "projects", string(project), "workstreams", string(stream), "documents.jsonl")
	data, err := os.ReadFile(path)
	must(t, err)
	var doc trace.Document
	must(t, json.Unmarshal(bytes.TrimSpace(data), &doc))
	handed, err := os.ReadFile(filepath.Join(filepath.Dir(path), doc.Path))
	must(t, err)
	return doc, string(handed)
}

// handedCount counts the project's workstreams other than the librarian's.
func handedCount(t *testing.T, root string, project config.ProjectID) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "projects", string(project), "workstreams"))
	must(t, err)
	sum := sha256.Sum256([]byte("librarian:" + string(project)))
	librarian := "w_" + hex.EncodeToString(sum[:16])
	n := 0
	for _, e := range entries {
		if e.Name() != librarian {
			n++
		}
	}
	return n
}

func TestHandInCommand(t *testing.T) {
	root, project, home := handInFixture(t)
	design := filepath.Join(home, "design.md")
	must(t, os.WriteFile(design, []byte("# Design\n"), 0600))

	// A relative path resolves against the client's working directory.
	wd, err := os.Getwd()
	must(t, err)
	relative, err := filepath.Rel(wd, design)
	must(t, err)
	var result service.HandInResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "handin", string(project), relative, "--json")), &result))
	if result.Project != project || result.State != "handed" || result.Source != "file:"+design || config.CheckWorkstreamIDs(result.Workstream) != nil {
		t.Fatalf("file hand-in %+v", result)
	}
	if doc, handed := handedWorkstream(t, root, project, result.Workstream); doc.Source != "file:"+design || handed != "# Design\n" || filepath.Base(result.Handed) != "design.md" {
		t.Fatalf("file copy %+v %q", doc, handed)
	}

	text := successful(t, root, "handin", string(project), "https://github.com/owner/repo/issues/3")
	lines := strings.Split(text, "\n")
	stream, ok := strings.CutPrefix(lines[0], "Workstream ")
	stream, ok2 := strings.CutSuffix(stream, " handed in to project "+string(project))
	if !ok || !ok2 || len(lines) != 5 || lines[1] != "State: handed" || lines[3] != "Source: https://github.com/owner/repo/issues/3" || lines[4] != "" {
		t.Fatalf("URL hand-in output:\n%s", text)
	}
	id := config.WorkstreamID(stream)
	want := "Handed: " + filepath.Join(root, "projects", string(project), "workstreams", stream, "handed", "issue-3.md")
	if lines[2] != want {
		t.Fatalf("URL hand-in output:\n%s\nwant %s", text, want)
	}
	if doc, handed := handedWorkstream(t, root, project, id); doc.Source != "https://github.com/owner/repo/issues/3" || handed != "# Title\n\nBody\n" {
		t.Fatalf("issue copy %+v %q", doc, handed)
	}

	stdin := "line one\r\n\ttabbed é\n"
	code, out, diag := invokeInput(t, root, stdin, "handin", string(project), "-", "--json")
	if code != 0 || diag != "" {
		t.Fatalf("stdin hand-in: %d %s %s", code, out, diag)
	}
	must(t, json.Unmarshal([]byte(out), &result))
	if doc, handed := handedWorkstream(t, root, project, result.Workstream); result.Source != "stdin" || doc.Source != "stdin" || handed != stdin || doc.Path != "handed/stdin" {
		t.Fatalf("stdin copy %+v %+v %q", result, doc, handed)
	}

	// Each command is its own hand-in.
	second := successful(t, root, "handin", string(project), design)
	if strings.Contains(second, string(result.Workstream)) || strings.Split(second, "\n")[0] == lines[0] {
		t.Fatalf("second hand-in reused a workstream:\n%s", second)
	}
	if n := handedCount(t, root, project); n != 4 {
		t.Fatalf("%d workstreams", n)
	}

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
	if n := handedCount(t, root, project); n != 4 {
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

// TestHandInSkipDebate hands in with --skip-debate: the request carries the
// skip, the output says so and the trace records the owner's skip of debate.
func TestHandInSkipDebate(t *testing.T) {
	root, project, home := handInFixture(t)
	design := filepath.Join(home, "design.md")
	must(t, os.WriteFile(design, []byte("# Design\n"), 0600))

	text := successful(t, root, "handin", string(project), design, "--skip-debate")
	lines := strings.Split(text, "\n")
	stream, ok := strings.CutPrefix(lines[0], "Workstream ")
	stream, ok2 := strings.CutSuffix(stream, " handed in to project "+string(project))
	if !ok || !ok2 || len(lines) != 6 || lines[1] != "State: handed" || lines[3] != "Source: file:"+design ||
		lines[4] != "Debate: skipped; ratify the spec and the plan once they are drafted" || lines[5] != "" {
		t.Fatalf("hand-in output:\n%s", text)
	}
	skips := func(stream string) []trace.Transition {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, "projects", string(project), "workstreams", stream, "events.jsonl"))
		must(t, err)
		var out []trace.Transition
		for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
			var tr trace.Transition
			must(t, json.Unmarshal([]byte(line), &tr))
			if tr.ID == "shed-owner-skip" {
				out = append(out, tr)
			}
		}
		return out
	}
	if got := skips(stream); len(got) != 1 || got[0].Subject != "shed-owner" || got[0].To != "skipped" || got[0].Cause != "handin" ||
		got[0].Reason != "the owner skipped debate at hand-in; the workstream still needs the owner's ratification of the spec and the plan" {
		t.Fatalf("skip transitions %+v", got)
	}

	var result service.HandInResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "handin", string(project), design, "--skip-debate", "--json")), &result))
	if !result.SkipDebate || result.State != "handed" || len(skips(string(result.Workstream))) != 1 {
		t.Fatalf("JSON hand-in %+v", result)
	}
	// Without the flag nothing is skipped and the output is unchanged.
	var debated service.HandInResponse
	must(t, json.Unmarshal([]byte(successful(t, root, "handin", string(project), design, "--json")), &debated))
	if debated.SkipDebate || debated.Workstream == result.Workstream || len(skips(string(debated.Workstream))) != 0 {
		t.Fatalf("hand-in without the flag %+v", debated)
	}

	for _, args := range [][]string{{"handin", string(project), design, "--skip-debate=true"}, {"status", "--skip-debate"}, {"shed", "skip", "w_0123456789abcdef0123456789abcdef", "--skip-debate"}} {
		code, out, diag := invokeInput(t, root, "", args...)
		if code != 2 || out != "" || diag != "invalid arguments; use osmia --help\n" {
			t.Errorf("%v: %d %q %q", args, code, out, diag)
		}
	}
}
