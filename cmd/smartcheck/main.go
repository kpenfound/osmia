// Command smartcheck runs the Dagger checks Jev selects for the change
// between a Git working tree and a base branch, with the judgments the factory
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

	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/gitdiff"
)

const usage = `usage: smartcheck [flags] [-- dagger check flags]

Runs the Dagger checks Jev selects for the change between the working tree
and the merge base of --base. When no check reaches the threshold,
--fallback-picks can run the highest-scored checks; by default every check
runs. Breadth and depth failures keep the preceding stage's checks for ranking.
When ranking cannot answer, every check runs. Every run uses --parallel 4.
Arguments after the flags go to dagger check; caller --parallel and -j
arguments are refused.

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
	fallbackPicks                 int
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
	fs.IntVar(&o.fallbackPicks, "fallback-picks", 0, "run this many highest-scored checks when none reaches the threshold (0 runs every check)")
	fs.Float64Var(&o.threshold, "threshold", config.DefaultJevCheckThreshold, "select checks at or above this `probability`")
	fs.StringVar(&o.url, "url", config.DefaultJevURL, "base `URL` of the TypeSafe-compatible API")
	fs.StringVar(&o.model, "model", config.DefaultJevModel, "Jev `model` to ask")
	fs.StringVar(&o.keyEnv, "api-key-env", config.DefaultJevAPIKeyEnv, "environment `variable` holding the API key")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	o.checkArgs = fs.Args()
	if o.fallbackPicks < 0 {
		return o, fmt.Errorf("--fallback-picks must be a non-negative integer")
	}
	for _, arg := range o.checkArgs {
		if (strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(strings.SplitN(arg, "=", 2)[0], "j")) || arg == "--parallel" || strings.HasPrefix(arg, "--parallel=") || arg == "-parallel" || strings.HasPrefix(arg, "-parallel=") {
			return o, fmt.Errorf("--parallel is fixed at 4 by smartcheck")
		}
	}
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
	for _, addresses := range checkselect.Invocations(links) {
		exit, err := h.dagger(ctx, o.dir, h.stdout, h.stderr, slices.Concat([]string{"check", "--fail-fast", "--parallel", "4"}, o.checkArgs, addresses)...)
		if err != nil {
			fmt.Fprintln(h.stderr, "smartcheck: dagger check:", err)
			return 1
		}
		if exit != 0 {
			return exit
		}
	}
	return 0
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

	links, err := h.list(ctx, o.dir, true)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return h.fallback(err.Error()), nil
	}
	if len(links) == 0 {
		return h.fallback("the project lists no check links"), nil
	}
	state, ok := checkselect.State(diff)
	if !ok {
		return h.fallback("the changed files do not fit a judgment"), nil
	}
	if d, _ := state["diff"].(string); d != diff {
		h.verbosef(o, "the diff is too large for the judgment; Jev judges from the changed files\n")
	}

	client := systemone.Client{BaseURL: o.url, Model: o.model, APIKey: key}
	filtered, err := h.prefilter(ctx, o, client, state, links)
	if err != nil {
		return nil, err
	}
	candidates, ok := checkselect.Narrow(filtered, links, checkselect.MaxQuestions)
	if !ok {
		return h.fallback(fmt.Sprintf("%d check links do not narrow to %d", len(filtered), checkselect.MaxQuestions)), nil
	}
	if len(candidates) == 0 {
		return h.fallback("the project lists no candidate check links"), nil
	}
	h.verbosef(o, "checks: %d links, asking about %d\n", len(links), len(candidates))
	start := time.Now()
	response, err := h.judge(ctx, client, checkselect.Request(state, candidates))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return h.fallback("Jev did not answer: " + err.Error()), nil
	}
	if reason := checkselect.RankingAcceptance(response, candidates, o.threshold, o.fallbackPicks); reason == "unusable ranking answer" {
		return h.fallback("Jev did not answer: " + reason), nil
	}
	selected := checkselect.Selected(response, candidates, o.threshold)
	picks := false
	if len(selected) == 0 {
		selected = checkselect.FallbackPicks(response, candidates, o.fallbackPicks)
		picks = len(selected) > 0
	}
	if o.verbose {
		h.explain(response, candidates, selected, o.threshold, picks, time.Since(start))
	}
	if len(selected) == 0 {
		return h.fallback(fmt.Sprintf("no check reached probability %.2f", o.threshold)), nil
	}
	return selected, nil
}

func (h harness) list(ctx context.Context, dir string, all bool) ([]string, error) {
	args := []string{"list", "checks"}
	if all {
		args = append(args, "--all")
	}
	args = append(args, "--format=link")
	var listing bytes.Buffer
	exit, err := h.dagger(ctx, dir, &listing, &listing, args...)
	if err != nil {
		return nil, fmt.Errorf("dagger list checks: %w", err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("dagger list checks exited %d:\n%s", exit, listing.String())
	}
	return checkselect.Links(listing.String()), nil
}

// judge gives each stage its own full timeout, releasing it before the next.
func (h harness) judge(ctx context.Context, client systemone.Client, request systemone.Request) (systemone.Response, error) {
	timeout, _ := time.ParseDuration(config.DefaultJevTimeout)
	asked, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := h.evaluate(client, asked, request)
	if asked.Err() != nil {
		return systemone.Response{}, asked.Err()
	}
	return response, err
}

func (h harness) prefilter(ctx context.Context, o options, client systemone.Client, state map[string]any, links []string) ([]string, error) {
	roots, err := h.list(ctx, o.dir, false)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		h.verbosef(o, "breadth: listing failed; ranking the full candidate list: %s\n", err)
		return links, nil
	}
	breadth := checkselect.Breadth(roots, links)
	request := checkselect.BreadthRequest(state, breadth)
	var response systemone.Response
	if len(request.Questions) > 0 {
		response, err = h.judge(ctx, client, request)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	kept, ok := checkselect.ApplyBreadth(response, breadth, o.threshold)
	if err != nil || !ok {
		h.verbosef(o, "breadth: Jev did not answer; ranking the full candidate list\n")
		return links, nil
	}
	if o.verbose {
		h.explainBreadth(response, breadth, kept)
	}
	depth := checkselect.Depth(kept)
	if o.verbose {
		h.explainHierarchy(depth)
	}
	request = checkselect.DepthRequest(state, depth)
	response = systemone.Response{}
	if len(request.Questions) > 0 {
		response, err = h.judge(ctx, client, request)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	result, ok := checkselect.ApplyDepth(response, depth)
	if err != nil || !ok {
		h.verbosef(o, "depth: Jev did not answer; ranking kept collections at their finest level\n")
		result = checkselect.Finest(depth)
	}
	if o.verbose {
		for _, c := range depth.Collections {
			probability := " (no judgment needed)"
			if err != nil || !ok {
				probability = " (fallback)"
			} else if c.Question != "" {
				answer := response.Answers[c.Question]
				probability = fmt.Sprintf(" (probability %.2f)", answer.Probabilities[answer.Choice])
			}
			fmt.Fprintf(h.stderr, "depth: %s: chosen %s%s\n", c.Root, result.Levels[c.Root], probability)
		}
	}
	return result.Links, nil
}

func (h harness) explainBreadth(response systemone.Response, plan checkselect.BreadthPlan, kept checkselect.BreadthResult) {
	fmt.Fprintln(h.stderr, "breadth: top-level checks")
	for i, root := range plan.Roots {
		mark := " "
		if slices.Contains(kept.KeptRoots, root.Link) {
			mark = "✓"
		}
		fmt.Fprintf(h.stderr, "  %s %.2f %s\n", mark, response.Answers[checkselect.BreadthQuestion(i)].Noul, root.Link)
	}
}

func (h harness) explainHierarchy(plan checkselect.DepthPlan) {
	for _, c := range plan.Collections {
		fmt.Fprintf(h.stderr, "depth: %s hierarchy (coarsest first)\n", c.Root)
		if len(c.Hierarchy) == 0 {
			fmt.Fprintln(h.stderr, "  inconsistent filter order; using finest links")
		}
		for _, level := range c.Hierarchy {
			offered := "offered"
			if level.Offered != true {
				offered = fmt.Sprintf("not offered: %v", level.Offered)
			}
			fmt.Fprintf(h.stderr, "  %s: %d links, contains %s, %s\n    example: %s\n", level.Level, level.Links, level.Contains, offered, level.Example)
		}
	}
}

// explain prints Jev's answer for every candidate, most likely first.
func (h harness) explain(r systemone.Response, candidates, selected []string, threshold float64, picks bool, took time.Duration) {
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
	if picks {
		fmt.Fprintf(h.stderr, "no check reached probability %.2f; running the %d highest-scored checks as fallback picks out of %d candidates\n", threshold, len(selected), len(candidates))
		return
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
