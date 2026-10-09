// Command smartcheck runs the Dagger checks Jev selects for the change
// between a Git working tree and a base branch, with the judgment the factory
// uses to choose a unit's checks. It reads no Osmia configuration or state.
package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/gitdiff"
	"github.com/kpenfound/osmia/internal/systemone"
)

const usage = `usage: smartcheck [flags] [-- dagger check flags]

Runs the Dagger checks Jev selects for the change between the working tree
and the merge base of --base, falling back to every check when Jev selects
none or does not answer. Arguments after the flags go to dagger check.

`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	h := harness{stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv, dagger: runDagger, evaluate: systemone.Client.Evaluate}
	code := h.run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// harness is the command with its environment, so tests replace Dagger and
// Jev.
type harness struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	// dagger runs the Dagger CLI in dir and returns its exit status.
	dagger   func(ctx context.Context, dir string, stdout, stderr io.Writer, args ...string) (int, error)
	evaluate func(systemone.Client, context.Context, systemone.Request) (systemone.Response, error)
}

type options struct {
	dir, base, url, model, keyEnv string
	threshold                     float64
	verbose, dryRun               bool
	checkArgs                     []string
}

func (h harness) parse(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("smartcheck", flag.ContinueOnError)
	fs.SetOutput(h.stderr)
	fs.Usage = func() {
		fmt.Fprint(h.stderr, usage)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.dir, "C", ".", "run in the Git working tree at `path`")
	fs.StringVar(&o.base, "base", "main", "compare against the merge base of this `revision`")
	fs.BoolVar(&o.verbose, "v", false, "show the changed files, Jev's answers and the selection")
	fs.BoolVar(&o.verbose, "verbose", false, "same as -v")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the selected check links instead of running them")
	fs.Float64Var(&o.threshold, "threshold", checkselect.Threshold, "select checks at or above this `probability`")
	fs.StringVar(&o.url, "url", config.DefaultJevURL, "base `URL` of the TypeSafe-compatible API")
	fs.StringVar(&o.model, "model", config.DefaultJevModel, "Jev `model` to ask")
	fs.StringVar(&o.keyEnv, "api-key-env", config.DefaultJevAPIKeyEnv, "environment `variable` holding the API key")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	o.checkArgs = fs.Args()
	if o.threshold < 0 || o.threshold > 1 {
		return o, fmt.Errorf("--threshold %v is not a probability", o.threshold)
	}
	return o, nil
}

func (h harness) run(ctx context.Context, args []string) int {
	o, err := h.parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(h.stderr, "smartcheck:", err)
		return 2
	}
	root, err := git(ctx, o.dir, "rev-parse", "--show-toplevel")
	if err != nil {
		fmt.Fprintln(h.stderr, "smartcheck:", err)
		return 1
	}
	o.dir = strings.TrimSpace(root)
	links, err := h.selection(ctx, o)
	if err != nil {
		fmt.Fprintln(h.stderr, "smartcheck:", err)
		return 1
	}
	if links == nil {
		return 0
	}
	if o.dryRun {
		if len(links) == 0 {
			fmt.Fprintln(h.stdout, "every check")
		}
		for _, l := range links {
			fmt.Fprintln(h.stdout, l)
		}
		return 0
	}
	exit, err := h.dagger(ctx, o.dir, h.stdout, h.stderr, slices.Concat([]string{"check", "--fail-fast"}, o.checkArgs, links)...)
	if err != nil {
		fmt.Fprintln(h.stderr, "smartcheck: dagger check:", err)
		return 1
	}
	return exit
}

// selection returns the check links to run in the working tree at o.dir:
// Jev's selection, an empty slice for every check, or nil when nothing
// changed.
func (h harness) selection(ctx context.Context, o options) ([]string, error) {
	key := h.getenv(o.keyEnv)
	if key == "" {
		return nil, fmt.Errorf("%s is not set; --api-key-env names the variable holding the Jev API key", o.keyEnv)
	}
	base, err := git(ctx, o.dir, "merge-base", o.base, "HEAD")
	if err != nil {
		return nil, err
	}
	base = strings.TrimSpace(base)
	diff, err := git(ctx, o.dir, "diff", "--no-ext-diff", "--binary", "--no-renames", base, "--")
	if err != nil {
		return nil, err
	}
	if diff == "" {
		fmt.Fprintf(h.stderr, "smartcheck: nothing changed since %s (%s); no check to run\n", o.base, short(base))
		return nil, nil
	}
	h.verbosef(o, "base: %s (merge base %s)\nchanged files:\n%s", o.base, short(base), gitdiff.ChangedFiles(diff))

	var listing bytes.Buffer
	exit, err := h.dagger(ctx, o.dir, &listing, &listing, "list", "checks", "--all", "--format=link")
	if err != nil {
		return nil, fmt.Errorf("dagger list checks: %w", err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("dagger list checks exited %d:\n%s", exit, listing.String())
	}
	links := checkselect.Links(listing.String())
	if len(links) == 0 {
		return h.fallback("the project lists no check links"), nil
	}
	candidates, ok := checkselect.Candidates(links, checkselect.MaxQuestions)
	if !ok {
		return h.fallback(fmt.Sprintf("%d check links do not narrow to %d", len(links), checkselect.MaxQuestions)), nil
	}
	state, ok := checkselect.State(diff)
	if !ok {
		return h.fallback("the changed files do not fit a judgment"), nil
	}
	h.verbosef(o, "checks: %d links, asking about %d\n", len(links), len(candidates))
	if d, _ := state["diff"].(string); d != diff {
		h.verbosef(o, "the diff is too large for the judgment; Jev judges from the changed files\n")
	}

	timeout, _ := time.ParseDuration(config.DefaultJevTimeout)
	asked, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	client := systemone.Client{BaseURL: o.url, Model: o.model, APIKey: key}
	response, err := h.evaluate(client, asked, checkselect.Request(state, candidates))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return h.fallback("Jev did not answer: " + err.Error()), nil
	}
	selected := checkselect.Selected(response, candidates, o.threshold)
	if o.verbose {
		h.explain(response, candidates, selected, o.threshold, time.Since(start))
	}
	if len(selected) == 0 {
		return h.fallback(fmt.Sprintf("no check reached probability %.2f", o.threshold)), nil
	}
	return selected, nil
}

// explain prints Jev's answer for every candidate, most likely first.
func (h harness) explain(r systemone.Response, candidates, selected []string, threshold float64, took time.Duration) {
	cost := ""
	if r.Usage.CostKnown {
		cost = fmt.Sprintf(", $%.4f", r.Usage.CostUSD)
	}
	fmt.Fprintf(h.stderr, "jev: %s answered in %s (%d input, %d output tokens%s)\n", r.Model, took.Round(time.Millisecond), r.Usage.InputTokens, r.Usage.OutputTokens, cost)
	order := make([]int, len(candidates))
	for i := range order {
		order[i] = i
	}
	noul := func(i int) float64 { return r.Answers[checkselect.Question(i)].Noul }
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(noul(b), noul(a)) })
	for _, i := range order {
		mark := " "
		if slices.Contains(selected, candidates[i]) {
			mark = "✓"
		}
		fmt.Fprintf(h.stderr, "  %s %.2f %s\n", mark, noul(i), candidates[i])
	}
	fmt.Fprintf(h.stderr, "selected %d of %d at probability %.2f or more\n", len(selected), len(candidates), threshold)
}

// fallback says why every check runs and selects them all.
func (h harness) fallback(reason string) []string {
	fmt.Fprintf(h.stderr, "smartcheck: running every check: %s\n", reason)
	return []string{}
}

func (h harness) verbosef(o options, format string, args ...any) {
	if o.verbose {
		fmt.Fprintf(h.stderr, format, args...)
	}
}

func short(commit string) string { return commit[:min(len(commit), 12)] }

// git runs git in dir and returns its standard output.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// runDagger runs the Dagger CLI in dir with the caller's environment. An
// interrupt reaches Dagger as one, so it can stop its checks.
func runDagger(ctx context.Context, dir string, stdout, stderr io.Writer, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, "dagger", args...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, os.Stdin, stdout, stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}
