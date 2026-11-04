//go:build daggercli

package checks

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/api"
)

// realCLIWorkspace writes a Dagger workspace of Go packages using the project's
// pinned modules. broken has a test that always fails. order has a test that
// fails only in a run with the test before it, a deterministic flake: it fails
// its package's run and passes alone.
func realCLIWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	lock, err := os.ReadFile("../../dagger.lock")
	must(t, err)
	files := map[string]string{
		"dagger.lock":           string(lock),
		"dagger.toml":           "[modules]\n\n[modules.go]\nsource = \"dagger.io/go\"\n",
		"go.mod":                "module fixture\n\ngo 1.26\n",
		"a/a.go":                "package a\n",
		"a/a_test.go":           "package a\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) {}\n\nfunc TestTwo(t *testing.T) {}\n",
		"b/b.go":                "package b\n",
		"b/b_test.go":           "package b\n\nimport \"testing\"\n\nfunc TestThree(t *testing.T) {}\n",
		"broken/broken.go":      "package broken\n",
		"broken/broken_test.go": "package broken\n\nimport \"testing\"\n\nfunc TestBroken(t *testing.T) { t.Fatal(\"wrong answer\") }\n",
		"order/order.go":        "package order\n",
		"order/order_test.go":   "package order\n\nimport \"testing\"\n\nvar first bool\n\nfunc TestFirst(t *testing.T) { first = true }\n\nfunc TestLast(t *testing.T) {\n\tif first {\n\t\tt.Fatal(\"fails after TestFirst\")\n\t}\n}\n\nfunc TestOther(t *testing.T) {}\n",
		"lone/lone.go":          "package lone\n",
		"lone/lone_test.go":     "package lone\n\nimport \"testing\"\n\nfunc TestLone(t *testing.T) {}\n",
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
	fmt.Fprintf(&script, "exec '%s' -W \"$(pwd)\" \"$@\"\n", cli)
	bin := t.TempDir()
	must(t, os.WriteFile(filepath.Join(bin, "dagger"), []byte(script.String()), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// serial: installs a dagger executable through PATH.
func TestRealDaggerCLI(t *testing.T) {
	installNestedDagger(t)
	const (
		testOne   = "dag+check://?check=test&go-module=.&go-package=a&go-test=TestOne"
		packageB  = "dag+check://?check=test&go-module=.&go-package=b"
		packageA  = "dag+check://?check=test&go-module=.&go-package=a"
		broken    = "dag+check://?check=test&go-module=.&go-package=broken"
		noneMatch = "dag+check://?check=test&go-module=.&go-package=does/not/exist"
		order     = "dag+check://?check=test&go-module=.&go-package=order"
		last      = "dag://?check=test&go-module=.&go-package=order&go-test=TestLast"
		lone      = "dag+check://?check=test&go-module=.&go-package=lone"
		loneTest  = "dag://?check=test&go-module=.&go-package=lone&go-test=TestLone"
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

	t.Run("lists every test as a link", func(t *testing.T) {
		links, err := checks.List(ctx, dir, Identity{})
		must(t, err)
		if !slices.Contains(links, testOne) || !slices.Contains(links, broken+"&go-test=TestBroken") {
			t.Fatalf("listed %q", links)
		}
	})
	t.Run("runs links with different filters as separate views", func(t *testing.T) {
		result, status, failed, reason := check(t, testOne, packageB)
		if status != ChecksPassed || len(failed) != 0 || reason != nil {
			t.Fatalf("classified %s, %q, %v:\n%s", status, failed, reason, result.Output)
		}
		for _, want := range []string{"go-test=TestOne", "go-test=TestThree"} {
			if !strings.Contains(result.Output, want) {
				t.Fatalf("output lacks %q:\n%s", want, result.Output)
			}
		}
		if strings.Contains(result.Output, "go-test=TestTwo") {
			t.Fatalf("a check no link selects ran:\n%s", result.Output)
		}
	})
	t.Run("runs links that differ in one filter as one view", func(t *testing.T) {
		if got := Command([]string{packageA, packageB}); slices.Contains(got, "&&") {
			t.Fatalf("command %q", got)
		}
		result, status, failed, reason := check(t, packageA, packageB)
		if status != ChecksPassed || len(failed) != 0 || reason != nil {
			t.Fatalf("classified %s, %q, %v:\n%s", status, failed, reason, result.Output)
		}
		for _, want := range []string{"go-test=TestOne", "go-test=TestTwo", "go-test=TestThree"} {
			if !strings.Contains(result.Output, want) {
				t.Fatalf("output lacks %q:\n%s", want, result.Output)
			}
		}
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
		result, status, failed, reason := check(t, order)
		if status != ChecksPassed || len(failed) != 0 || reason != nil || result.ExitCode != 0 || !slices.Equal(result.Flaky, []string{last}) {
			t.Fatalf("classified %s, %q, %v, exit %d, flaky %q:\n%s", status, failed, reason, result.ExitCode, result.Flaky, result.Output)
		}
		for _, want := range []string{"fails after TestFirst", "go-test=TestOther", "--skip " + last} {
			if !strings.Contains(result.Output, want) {
				t.Fatalf("output lacks %q:\n%s", want, result.Output)
			}
		}
		if summary, _ := SummarizeCheckOutput(result.Output, result.ExitCode); !strings.Contains(summary, "[passed on rerun]") || !strings.Contains(summary, "fails after TestFirst") {
			t.Fatalf("summary: %q", summary)
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
	t.Run("addresses with different filters pass or are recognised as refused", func(t *testing.T) {
		var output bytes.Buffer
		exit, _, err := checks.dagger(ctx, dir, Identity{}, false, &output, checkArgs([]string{testOne, packageB})...)
		must(t, err)
		if exit != 0 && refusal(output.String(), exit) == "" {
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
