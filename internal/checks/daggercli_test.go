//go:build daggercli

package checks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
)

// realCLIWorkspace writes a Dagger workspace of Go packages using the project's
// pinned modules. broken always fails. TestLast fails when its test pattern
// names TestNeighbour in another package, and passes when selected alone.
// TestIdle is never selected beside TestLast, forcing that run to carry a
// test-name pattern rather than selecting every test in the package.
func realCLIWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	lock, err := os.ReadFile("../../dagger.lock")
	must(t, err)
	files := map[string]string{
		"dagger.lock":                        string(lock),
		"dagger.toml":                        "[modules.go]\nsource = \"dagger.io/go\"\n\n[modules.dang-sdk]\nsource = \"dagger.io/sdk/dang\"\n\n[sdks.dang]\nmodule = \"dang-sdk\"\n\n[modules.fixture]\nsource = \".dagger/fixture\"\n",
		".dagger/fixture/dagger-module.toml": "name = \"fixture\"\nengineVersion = \"v1.0.0-beta.15\"\n\n[runtime]\nsource = \"dang\"\n",
		".dagger/fixture/main.dang":          "type Fixture {\n  pub standalone: Void @check { null }\n}\n",
		"go.mod":                             "module fixture\n\ngo 1.26\n",
		"a/a.go":                             "package a\n",
		"a/a_test.go":                        "package a\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) {}\n\nfunc TestTwo(t *testing.T) {}\n",
		"b/b.go":                             "package b\n",
		"b/b_test.go":                        "package b\n\nimport \"testing\"\n\nfunc TestThree(t *testing.T) {}\n\nfunc TestFour(t *testing.T) {}\n",
		"broken/broken.go":                   "package broken\n",
		"broken/broken_test.go":              "package broken\n\nimport \"testing\"\n\nfunc TestBroken(t *testing.T) { t.Fatal(\"wrong answer\") }\n",
		"order/order.go":                     "package order\n",
		"order/order_test.go":                "package order\n\nimport (\"flag\"; \"strings\"; \"testing\")\n\nfunc TestLast(t *testing.T) {\n\tif strings.Contains(flag.Lookup(\"test.run\").Value.String(), \"TestNeighbour\") {\n\t\tt.Fatal(\"fails beside TestNeighbour\")\n\t}\n}\n\nfunc TestIdle(t *testing.T) {}\n",
		"neighbour/neighbour.go":             "package neighbour\n",
		"neighbour/neighbour_test.go":        "package neighbour\n\nimport \"testing\"\n\nfunc TestNeighbour(t *testing.T) {}\n",
		"lone/lone.go":                       "package lone\n",
		"lone/lone_test.go":                  "package lone\n\nimport \"testing\"\n\nfunc TestLone(t *testing.T) {}\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		must(t, os.MkdirAll(filepath.Dir(path), 0700))
		must(t, os.WriteFile(path, []byte(content), 0600))
	}
	return dir
}

// installNestedDagger puts a dagger on PATH that reaches the session this test
// runs in. The runner passes the CLI only its own environment, and a nested
// CLI looks for its workspace from the container's root without -W.
func installNestedDagger(t *testing.T) {
	t.Helper()
	cli, err := exec.LookPath("dagger")
	must(t, err)
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	for _, name := range []string{"DAGGER_SESSION_PORT", "DAGGER_SESSION_TOKEN", "_DAGGER_NESTED_CLIENT_ID"} {
		fmt.Fprintf(&script, "export %s='%s'\n", name, os.Getenv(name))
	}
	fmt.Fprintf(&script, "if [ \"$1\" = version ]; then exec '%s' \"$@\"; fi\n", cli)
	fmt.Fprintf(&script, "exec '%s' -W \"$(pwd)\" \"$@\"\n", cli)
	bin := t.TempDir()
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script.String()), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// serial: installs a dagger executable through PATH.
func TestRealDaggerCLI(t *testing.T) {
	installNestedDagger(t)
	const (
		testOne       = "dag+check://?check=test&go-module=.&go-package=a&go-test=TestOne"
		packageB      = "dag+check://?check=test&go-module=.&go-package=b"
		packageA      = "dag+check://?check=test&go-module=.&go-package=a"
		broken        = "dag+check://?check=test&go-module=.&go-package=broken"
		noneMatch     = "dag+check://?check=test&go-module=.&go-package=does/not/exist"
		beside        = "dag+check://?check=test&go-module=.&go-package=neighbour&go-package=order&go-test=TestLast&go-test=TestNeighbour"
		last          = "dag://?check=test&go-module=.&go-package=order&go-test=TestLast"
		neighbourTest = "dag://?check=test&go-module=.&go-package=neighbour&go-test=TestNeighbour"
		lone          = "dag+check://?check=test&go-module=.&go-package=lone"
		loneTest      = "dag://?check=test&go-module=.&go-package=lone&go-test=TestLone"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	dir := realCLIWorkspace(t)
	checks := DaggerChecks{}
	check := func(t *testing.T, links ...string) (CheckResult, string, []string, error) {
		t.Helper()
		result, err := checks.Check(ctx, dir, Identity{}, links)
		must(t, err)
		status, failed, reason := ClassifyChecks(result, nil)
		return result, status, failed, reason
	}

	t.Run("package addresses with different filters are refused", func(t *testing.T) {
		version, err := exec.CommandContext(ctx, "dagger", "version").CombinedOutput()
		if err != nil {
			t.Fatalf("dagger version: %v: %s", err, version)
		}
		var output bytes.Buffer
		started := time.Now()
		exit, _, err := checks.dagger(ctx, dir, Identity{}, false, &output, checkArgs([]string{packageA, packageB})...)
		must(t, err)
		t.Logf("CLI %s; two package-only links %q; exit %d in %s\n%s", version, []string{packageA, packageB}, exit, time.Since(started), output.String())
		if exit == 0 || !strings.Contains(output.String(), "addresses with different filters") || strings.Contains(output.String(), "CHECKS") {
			t.Fatalf("exit %d: %s", exit, output.String())
		}
	})
	t.Run("lists every test as a link", func(t *testing.T) {
		links, err := checks.List(ctx, dir, Identity{})
		must(t, err)
		if !slices.Contains(links, testOne) || !slices.Contains(links, broken+"&go-test=TestBroken") {
			t.Fatalf("listed %q", links)
		}
	})
	t.Run("attributes expanded links to top-level checks", func(t *testing.T) {
		roots, err := checks.ListTopLevel(ctx, dir, Identity{})
		must(t, err)
		links, err := checks.List(ctx, dir, Identity{})
		must(t, err)
		wantRoots := []string{
			"dag+check://?check=dang-sdk/generate/stale",
			"dag+check://?check=fixture/standalone",
			"dag+check://?check=go/modules/packages/generate/stale",
			"dag+check://?check=go/modules/packages/tests/test",
		}
		if !slices.Equal(roots, wantRoots) {
			t.Fatalf("top-level listing %q, want %q", roots, wantRoots)
		}
		// Scoped listings preserve Dagger's membership across its full-path
		// top-level names and short check names in expanded items.
		var stale, tests []string
		for _, pkg := range []string{"a", "b", "broken", "lone", "neighbour", "order"} {
			stale = append(stale, "dag+check://?check=stale&go-module=.&go-package="+pkg)
		}
		for _, item := range []struct {
			pkg, test string
		}{
			{"a", "TestOne"}, {"a", "TestTwo"},
			{"b", "TestFour"}, {"b", "TestThree"},
			{"broken", "TestBroken"}, {"lone", "TestLone"},
			{"neighbour", "TestNeighbour"},
			{"order", "TestIdle"}, {"order", "TestLast"},
		} {
			tests = append(tests, "dag+check://?check=test&go-module=.&go-package="+item.pkg+"&go-test="+item.test)
		}
		standalone := wantRoots[:2]
		wantLinks := slices.Concat(standalone, stale, tests)
		slices.Sort(wantLinks)
		if !slices.Equal(links, wantLinks) {
			t.Fatalf("expanded listing %q, want %q", links, wantLinks)
		}
		scoped := make(map[string][]string, len(roots))
		for _, root := range roots {
			started := time.Now()
			scoped[root], err = checks.ListScoped(ctx, dir, Identity{}, root)
			must(t, err)
			t.Logf("scoped --all listing %s took %s: %q", root, time.Since(started), scoped[root])
		}
		plan, err := checkselect.BreadthScoped(roots, links, scoped)
		must(t, err)
		if len(plan.Roots) != len(wantRoots) || len(plan.Uncovered) != 0 {
			t.Fatalf("attribution roots %+v, uncovered %q", plan.Roots, plan.Uncovered)
		}
		for i, root := range plan.Roots {
			want := tests
			if root.Link == wantRoots[2] {
				want = stale
			}
			if slices.Contains(standalone, root.Link) {
				want = []string{root.Link}
			}
			collection := !slices.Contains(standalone, root.Link)
			if !slices.Equal(scoped[root.Link], want) {
				t.Errorf("scoped listing for %s: %q, want %q", root.Link, scoped[root.Link], want)
			}
			if root.Link != wantRoots[i] || root.Collection != collection || !slices.Equal(root.Links, want) {
				t.Errorf("attribution for %s: %+v, want links %q", wantRoots[i], root, want)
			}
		}
		t.Logf("top-level listing %q; expanded listing %q; attribution roots %+v; uncovered %q", roots, links, plan.Roots, plan.Uncovered)
	})
	t.Run("offers narrow checks from the real listing", func(t *testing.T) {
		links, err := checks.List(ctx, dir, Identity{})
		must(t, err)
		candidates, ok := checkselect.Candidates(links, checkselect.MaxQuestions)
		if !ok || len(candidates) == 0 {
			t.Fatalf("no candidates from %q", links)
		}
		for _, broad := range []string{"dag+check://?check=test", "dag+check://?check=test&go-module=."} {
			if slices.Contains(candidates, broad) {
				t.Errorf("broad option %s in %q", broad, candidates)
			}
		}
		if !slices.Contains(candidates, testOne) && !slices.Contains(candidates, packageA) {
			t.Fatalf("missing workspace tests or packages from %q; listed %q", candidates, links)
		}
		t.Logf("real listing %q; candidates %q", links, candidates)
	})
	for _, picks := range []int{0, 2} {
		t.Run(fmt.Sprintf("runs mixed selection with fallback picks %d", picks), func(t *testing.T) {
			selection := api.CheckSelection{Mode: SelectionSelected, Links: []string{testOne, packageB}, Candidates: 2, Judgment: "recorded-selection", FallbackPicks: picks}
			started := time.Now()
			result, ran, err := RunSelection(ctx, checks, dir, Identity{}, selection)
			must(t, err)
			status, failed, reason := ClassifyChecks(result, nil)
			if !slices.Equal(ran.Links, selection.Links) || ran.Judgment != selection.Judgment || ran.FallbackPicks != picks {
				t.Fatalf("selection changed: %+v", ran)
			}
			t.Logf("mixed selection took %s; actual commands %q\n%s", time.Since(started), result.Command, result.Output)
			if status != ChecksPassed || len(failed) != 0 || reason != nil {
				t.Fatalf("classified %s, %q, %v:\n%s", status, failed, reason, result.Output)
			}
			var actual []string
			for _, line := range strings.Split(result.Output, "\n") {
				if m := reportCheck.FindStringSubmatch(line); m != nil {
					actual = append(actual, m[3])
				}
			}
			want := []string{
				strings.Replace(testOne, "dag+check", "dag", 1),
				strings.Replace(packageB, "dag+check", "dag", 1) + "&go-test=TestThree",
				strings.Replace(packageB, "dag+check", "dag", 1) + "&go-test=TestFour",
			}
			slices.Sort(actual)
			slices.Sort(want)
			if !slices.Equal(actual, want) {
				t.Fatalf("ran %q, want exactly %q:\n%s", actual, want, result.Output)
			}
		})
	}
	t.Run("runs links that differ in one filter as one view", func(t *testing.T) {
		if got := Command([]string{packageA, packageB}); slices.Contains(got, "&&") {
			t.Fatalf("command %q", got)
		}
		started := time.Now()
		result, status, failed, reason := check(t, packageA, packageB)
		t.Logf("grouped packages took %s; actual commands %q\n%s", time.Since(started), result.Command, result.Output)
		if status != ChecksPassed || len(failed) != 0 || reason != nil {
			t.Fatalf("classified %s, %q, %v:\n%s", status, failed, reason, result.Output)
		}
		for _, want := range []string{"go-test=TestOne", "go-test=TestTwo", "go-test=TestThree", "go-test=TestFour"} {
			if !strings.Contains(result.Output, want) {
				t.Fatalf("output lacks %q:\n%s", want, result.Output)
			}
		}
	})
	t.Run("measures repeated workspace loading", func(t *testing.T) {
		started := time.Now()
		check(t, packageA)
		check(t, packageB)
		separate := time.Since(started)
		started = time.Now()
		check(t, packageA, packageB)
		t.Logf("same cached package selection: two commands %s; one exact grouped command %s", separate, time.Since(started))
	})
	t.Run("a failed check names its link and stops later runs", func(t *testing.T) {
		result, status, failed, _ := check(t, broken+"&go-test=TestBroken", packageA)
		if status != ChecksFailed || !slices.Contains(failed, strings.Replace(broken, "dag+check", "dag", 1)+"&go-test=TestBroken") {
			t.Fatalf("classified %s, %q:\n%s", status, failed, result.Output)
		}
		if result.Refusal != "" || !strings.Contains(result.Output, "wrong answer") || strings.Contains(result.Output, "go-test=TestOne") {
			t.Fatalf("refusal %q:\n%s", result.Refusal, result.Output)
		}
	})
	t.Run("a check that passes alone is a flake and the rest of its view runs", func(t *testing.T) {
		result, status, failed, reason := check(t, beside)
		t.Logf("flake reports, commands %q:\n%s", result.Command, result.Output)
		if status != ChecksPassed || len(failed) != 0 || reason != nil || result.ExitCode != 0 || !slices.Contains(result.Flaky, last) {
			t.Fatalf("classified %s, %q, %v, exit %d, flaky %q:\n%s", status, failed, reason, result.ExitCode, result.Flaky, result.Output)
		}
		_, failures := parseCheckReport(result.Output)
		for _, link := range result.Flaky {
			if (link != last && link != neighbourTest) || !slices.ContainsFunc(failures, func(f checkFailure) bool { return f.link == link }) {
				t.Fatalf("flake not backed by an allowed failed check: %s\n%s", link, result.Output)
			}
		}
		for _, want := range []string{"fails beside TestNeighbour", "go-test=TestNeighbour", "--skip " + last} {
			if !strings.Contains(result.Output, want) {
				t.Fatalf("output lacks %q:\n%s", want, result.Output)
			}
		}
		if summary, _ := SummarizeCheckOutput(result.Output, result.ExitCode); !strings.Contains(summary, "[passed on rerun]") || !strings.Contains(summary, "fails beside TestNeighbour") {
			t.Fatalf("summary: %q", summary)
		}
		if !strings.Contains(result.Output, "$ dagger "+strings.Join(checkArgs([]string{last}), " ")+"\n") {
			t.Fatalf("no isolated rerun for TestLast:\n%s", result.Output)
		}
		if strings.Contains(result.Output, "go-test=TestIdle") {
			t.Fatalf("selected view widened to TestIdle:\n%s", result.Output)
		}
	})
	t.Run("a failed rerun of the same address replays the failure", func(t *testing.T) {
		exact := broken + "&go-test=TestBroken"
		result, status, failed, _ := check(t, exact)
		if status != ChecksFailed || len(failed) != 1 || len(result.Flaky) != 0 || strings.Count(result.Output, "$ dagger") != 1 {
			t.Fatalf("classified %s, %q, flaky %q:\n%s", status, failed, result.Flaky, result.Output)
		}
	})
	t.Run("skipping every check of a view is recognised as nothing left to run", func(t *testing.T) {
		var addressed, bare bytes.Buffer
		exit, _, err := checks.dagger(ctx, dir, Identity{}, false, &addressed, skipArgs([]string{lone}, []string{loneTest})...)
		must(t, err)
		if !reportNothingSelected.MatchString(refusal(addressed.String(), exit)) {
			t.Fatalf("exit %d, refusal %q:\n%s", exit, refusal(addressed.String(), exit), addressed.String())
		}
		links, err := checks.List(ctx, dir, Identity{})
		must(t, err)
		exit, _, err = checks.dagger(ctx, dir, Identity{}, false, &bare, skipArgs(nil, links)...)
		must(t, err)
		if !reportNothingSelected.MatchString(refusal(bare.String(), exit)) {
			t.Fatalf("exit %d, refusal %q:\n%s", exit, refusal(bare.String(), exit), bare.String())
		}
	})
	t.Run("a link that matches nothing is refused with Dagger's error", func(t *testing.T) {
		result, status, _, reason := check(t, noneMatch)
		if status != ChecksIncomplete || result.Refusal == "" || reason == nil || !strings.Contains(reason.Error(), result.Refusal) {
			t.Fatalf("classified %s, refusal %q, reason %v:\n%s", status, result.Refusal, reason, result.Output)
		}
	})
	t.Run("addresses with different filters are recognised as refused", func(t *testing.T) {
		var output bytes.Buffer
		exit, _, err := checks.dagger(ctx, dir, Identity{}, false, &output, checkArgs([]string{testOne, packageB})...)
		must(t, err)
		if exit == 0 || !strings.Contains(output.String(), "addresses with different filters") || refusal(output.String(), exit) == "" {
			t.Fatalf("exit %d is not recognised as a refusal:\n%s", exit, output.String())
		}
	})
	t.Run("a refused selection runs every check", func(t *testing.T) {
		selection := api.CheckSelection{Mode: SelectionSelected, Links: []string{noneMatch}, Candidates: 1, Judgment: "judgment"}
		result, ran, err := RunSelection(ctx, checks, dir, Identity{}, selection)
		must(t, err)
		if ran.Mode != SelectionFull || ran.Reason != SelectionRefused || ran.Detail == "" || ran.Judgment != "judgment" {
			t.Fatalf("selection %+v", ran)
		}
		if status, _, _ := ClassifyChecks(result, nil); status != ChecksFailed || !strings.Contains(result.Output, "wrong answer") {
			t.Fatalf("classified %s:\n%s", status, result.Output)
		}
	})
}

func TestRealDaggerCLIServiceID(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dagger", "query")
	cmd.Stdin = strings.NewReader(`{container{withDefaultArgs(args:["sleep","60"]){asService{id}}}}`)
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("Service ID query: %v: %s", err, diagnostic.String())
	}
	var response struct {
		Container struct {
			WithDefaultArgs struct{ AsService struct{ ID string } }
		}
	}
	must(t, json.Unmarshal(output, &response))
	if !previewServiceID(response.Container.WithDefaultArgs.AsService.ID) {
		t.Fatalf("real CLI Service ID refused: %s", output)
	}
}

// viewCLI runs the real Dagger CLI from the root of the view at root, with the
// arguments given and nothing that names a workspace, and returns its output
// and exit status. A nested Dagger client takes its working directory from
// the container it runs in, never from its process, so the CLI runs in a
// container of its own whose working directory is the view root: the view is
// that container's directory /view, repository included, and the CLI is this
// test's own.
func viewCLI(ctx context.Context, t *testing.T, root string, args ...string) (string, int) {
	t.Helper()
	cli, err := exec.LookPath("dagger")
	must(t, err)
	if head, err := os.ReadFile(cli); err != nil || bytes.HasPrefix(head, []byte("#!")) {
		t.Fatalf("%s is a wrapper, not the Dagger CLI: %v", cli, err)
	}
	for _, arg := range args {
		if arg == "-W" || strings.HasPrefix(arg, "--workspace") || strings.HasPrefix(arg, "-W=") {
			t.Fatalf("dagger %v names a workspace", args)
		}
	}
	core := func(last ...string) (string, error) {
		argv := append([]string{"core", "container", "from", "--address=golang:1.26-bookworm",
			"with-file", "--path=/usr/local/bin/dagger", "--source=" + cli, "--permissions=493",
			"with-directory", "--path=/view", "--source=" + root,
			"with-workdir", "--path=/view",
			"with-exec", "--args=" + strings.Join(append([]string{"dagger"}, args...), ","), "--experimental-privileged-nesting", "--expect=ANY"}, last...)
		cmd := exec.CommandContext(ctx, cli, argv...)
		for _, kv := range os.Environ() {
			if name, _, _ := strings.Cut(kv, "="); !strings.HasPrefix(name, "GIT_") && name != "DAGGER_WORKSPACE" {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("%w: %s", err, stderr.String())
		}
		return stdout.String(), nil
	}
	status, err := core("exit-code")
	must(t, err)
	var exit int
	_, err = fmt.Sscanf(strings.TrimSpace(status), "%d", &exit)
	must(t, err)
	output, err := core("combined-output")
	must(t, err)
	return output, exit
}

// enclosingRepository reports a directory above dir, or dir itself, that holds
// Git metadata.
func enclosingRepository(dir string) string {
	for ; ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		if filepath.Dir(dir) == dir {
			return ""
		}
	}
}

// A mason's view, built by Osmia's view code, is a Dagger workspace of its own
// at the view root: plain dagger commands there find the project's modules with
// no workspace flag, no copy and no repository the test made. The same project
// in a view without the repository is not found, so the difference is the
// view's repository and not an enclosing one.
// serial: the nested session is the process's own.
func TestRealDaggerCLIViewRepository(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	project := realCLIWorkspace(t)
	toml, err := os.OpenFile(filepath.Join(project, "dagger.toml"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = toml.WriteString("\n[env.flake.modules.go.settings]\ngoflags = \"-count=2\"\n")
	must(t, errors.Join(err, toml.Close()))
	entries, err := os.ReadDir(project)
	must(t, err)
	var paths []string
	for _, entry := range entries {
		paths = append(paths, entry.Name())
	}
	storage := t.TempDir()
	if enclosing := enclosingRepository(storage); enclosing != "" {
		t.Fatalf("view storage %s is inside the Git repository at %s", storage, enclosing)
	}
	build := func(t *testing.T, settings coreadapter.ExecutionSettings) string {
		t.Helper()
		view, err := (isolation.Views{Directory: storage}).CreateFor(ctx, coreadapter.Workspace{Directory: project, Access: coreadapter.ReadWrite}, paths, settings)
		must(t, err)
		t.Cleanup(func() { view.Release(context.WithoutCancel(ctx)) })
		return view.Workspace().Directory
	}
	// What a mason's execution settings carry; only Dagger differs.
	mason := coreadapter.ExecutionSettings{Mode: agent.SandboxSbx, Dagger: &agent.Dagger{Engine: "tcp://127.0.0.1:1234", Version: "v0.20.5"}}
	// listsChecks reports the "MODULE CHECK" rows of a dagger check -l table.
	listsChecks := func(output string) (rows []string) {
		for _, line := range strings.Split(output, "\n") {
			if fields := strings.Fields(line); len(fields) >= 2 && !strings.HasPrefix(line, "#") && fields[0] != "MODULE" {
				rows = append(rows, fields[0]+"/"+fields[1])
			}
		}
		return rows
	}

	t.Run("a view without the repository does not find the project", func(t *testing.T) {
		root := build(t, coreadapter.ExecutionSettings{Mode: agent.SandboxSbx})
		if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
			t.Fatal("the control view holds .git")
		}
		if _, err := os.Lstat(filepath.Join(root, "dagger.toml")); err != nil {
			t.Fatalf("the control view lacks the project: %v", err)
		}
		output, exit := viewCLI(ctx, t, root, "check", "-l")
		t.Logf("dagger check -l without the repository: exit %d\n%s", exit, output)
		if rows := listsChecks(output); len(rows) != 0 {
			t.Fatalf("checks %q are listed without the view's repository:\n%s", rows, output)
		}
	})

	root := build(t, mason)
	if _, err := os.Lstat(filepath.Join(root, ".git", "HEAD")); err != nil {
		t.Fatalf("the mason view holds no repository: %v", err)
	}
	if enclosing := enclosingRepository(filepath.Dir(root)); enclosing != "" {
		t.Fatalf("the view is inside the Git repository at %s", enclosing)
	}
	t.Run("dagger check lists the project's checks from the view root", func(t *testing.T) {
		output, exit := viewCLI(ctx, t, root, "check", "-l")
		t.Logf("dagger check -l: exit %d\n%s", exit, output)
		if exit != 0 {
			t.Fatalf("dagger check -l exited %d", exit)
		}
		rows := listsChecks(output)
		for _, want := range []string{"go/test", "go/stale", "fixture/standalone", "dang-sdk/stale"} {
			if !slices.Contains(rows, want) {
				t.Fatalf("dagger check -l lists %q, lacking %s:\n%s", rows, want, output)
			}
		}
	})
	t.Run("dagger check accepts the Go check's flags", func(t *testing.T) {
		output, exit := viewCLI(ctx, t, root, "check", "--progress=report", "--go-package=a", "--go-test=TestOne", "--env=flake")
		t.Logf("dagger check --go-package --go-test --env: exit %d\n%s", exit, output)
		if exit != 0 || !strings.Contains(output, "go-test=TestOne") || strings.Contains(output, "TestTwo") {
			t.Fatalf("dagger check with the Go check's flags exited %d:\n%s", exit, output)
		}
	})
	t.Run("dagger generate runs", func(t *testing.T) {
		output, exit := viewCLI(ctx, t, root, "generate", "--progress=report")
		t.Logf("dagger generate: exit %d\n%s", exit, output)
		if exit != 0 {
			t.Fatalf("dagger generate exited %d", exit)
		}
	})
}
