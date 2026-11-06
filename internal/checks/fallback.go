package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/checkselect"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// Remembered is what earlier check runs of a workstream showed needs the cloud
// engine: check links, and whether running every check does.
type Remembered struct {
	Links      []string `json:"links,omitempty"`
	EveryCheck bool     `json:"every_check,omitempty"`
}

// RememberedFrom derives the remembered set from the attributions of earlier
// check runs. Any cloud run counts, whatever its reason, so a link stays
// remembered while it keeps running there. Nil attributions, which belong to
// runs that predate the attribution or ran locally, add nothing.
func RememberedFrom(prior []*api.CheckAttribution) Remembered {
	var r Remembered
	for _, attribution := range prior {
		for _, run := range attribution.CloudRuns() {
			if run.EveryCheck {
				r.EveryCheck = true
			}
			for _, link := range run.Links {
				if !slices.Contains(r.Links, link) {
					r.Links = append(r.Links, link)
				}
			}
		}
	}
	return r
}

// checkRecordPaths matches the documents of unit and final-review check runs.
var checkRecordPaths = regexp.MustCompile(`^(units/[^/]+/checks-[0-9]+|final/checks-[0-9]+)\.json$`)

// RememberedIn derives the remembered set of a workstream from the engine
// attribution its unit and final-review check run records carry. A final
// review's runs are revisions of one document, and every revision counts.
// Records without an attribution add nothing. Nothing is stored for it.
func RememberedIn(repository *trace.Repository, stream config.WorkstreamID) (Remembered, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return Remembered{}, err
	}
	var prior []*api.CheckAttribution
	for _, d := range docs {
		if !checkRecordPaths.MatchString(d.Path) {
			continue
		}
		var record struct {
			Engines *api.CheckAttribution `json:"engines"`
		}
		if err := json.Unmarshal([]byte(d.Content), &record); err != nil {
			return Remembered{}, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		prior = append(prior, record.Engines)
	}
	return RememberedFrom(prior), nil
}

// Has reports whether link is remembered.
func (r Remembered) Has(link string) bool { return slices.Contains(r.Links, link) }

// DescribeAttribution says in a sentence what ran on the cloud engine, for a
// selection description or a prompt. It is empty when nothing did.
func DescribeAttribution(a *api.CheckAttribution) string {
	cloud := a.CloudRuns()
	if len(cloud) == 0 {
		return ""
	}
	var parts []string
	for _, run := range cloud {
		what := "every check"
		if !run.EveryCheck {
			what = CheckExcerpt(strings.Join(run.Links, ", "), 512)
		}
		why := map[string]string{
			api.CloudAfterTimeout:       "escalated after the local attempt reached its time budget",
			api.CloudAfterEngineFailure: "escalated after the local attempt lost its engine session or exited without reporting a failed check",
			api.CloudByMemory:           "routed there because an earlier run in this workstream needed cloud",
		}[run.Reason]
		if why == "" {
			why = "ran there"
		}
		if run.PassedLocally > 0 {
			why += fmt.Sprintf("; the %d checks that had passed locally were not rerun", run.PassedLocally)
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", what, why))
	}
	sentence := "The Dagger Cloud engine ran " + strings.Join(parts, "; ")
	if len(cloud) < len(a.Runs) {
		sentence += "; the other checks ran locally"
	}
	return sentence + "."
}

// DescribeSelectionRun is DescribeSelection followed, when any check ran on
// the cloud engine, by what ran there and why. It is DescribeSelection alone
// otherwise.
func DescribeSelectionRun(s api.CheckSelection, a *api.CheckAttribution) string {
	description := DescribeSelection(s)
	if sentence := DescribeAttribution(a); sentence != "" {
		description += "; " + strings.ToLower(sentence[:1]) + strings.TrimSuffix(sentence[1:], ".")
	}
	return description
}

// RunSelectionWithFallback is RunSelection with an optional cloud engine. It
// returns the combined result, the selection that produced it, and which
// engine ran each invocation. budget is checks_timeout.
//
// Without cloud it is RunSelection under a single budget. With cloud, the
// links remembered needs run there directly and the others run locally first.
// Local work that does not finish, because the budget ran out, the engine
// session was lost or Dagger exited without reporting a failed check, runs on
// cloud without the checks the local output reports as passed. All local work,
// reruns and the every-check run after refused links included, shares one
// budget; all cloud work shares another, which starts when cloud work does.
// A reported failure, cancellation of ctx, a preparation error and the refusal
// of selected links never lead to a cloud attempt. When the cloud attempt does
// not finish either, the error names it.
func RunSelectionWithFallback(ctx context.Context, local, cloud ReviewChecks, budget time.Duration, remembered Remembered, dir string, identity Identity, selection api.CheckSelection) (CheckResult, api.CheckSelection, *api.CheckAttribution, error) {
	o := &fallbackRun{ctx: ctx, local: local, cloud: cloud, budget: budget, remembered: remembered, dir: dir, identity: identity}
	defer o.cancel()
	if cloud == nil {
		bounded, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		result, ran, err := RunSelection(bounded, local, dir, identity, selection)
		attribution := &api.CheckAttribution{}
		for _, g := range o.plan(ran.Links, false) {
			attribution.Runs = append(attribution.Runs, g.run(api.CheckEngineLocal, "", 0))
		}
		return result, ran, attribution, err
	}
	groups := o.plan(selection.Links, true)
	refusal, err := o.execute(groups)
	if refusal != "" && err == nil {
		full := FullSelection(SelectionRefused, refusal)
		full.Candidates, full.Listed, full.Judgment = selection.Candidates, selection.Listed, selection.Judgment
		selection = full
		o.refused = fmt.Sprintf("\nDagger refused the selected links, so every check runs.\n$ %s\n", strings.Join(Command(nil), " "))
		o.commandFrom = len(o.segments)
		_, err = o.execute(o.plan(nil, true))
	}
	return o.result(), selection, &api.CheckAttribution{Runs: o.runs}, err
}

// group is the links one dagger check run selects, or every check.
type group struct {
	links      []string
	every      bool
	remembered bool
}

func (g group) run(engine, reason string, passedLocally int) api.CheckEngineRun {
	return api.CheckEngineRun{Engine: engine, Links: g.links, EveryCheck: g.every, Reason: reason, PassedLocally: passedLocally}
}

// segment is the result of one call to a runner.
type segment struct {
	engine string
	group  group
	result CheckResult
	// before is text that comes ahead of the segment's output.
	before string
}

// fallbackRun is the state of one RunSelectionWithFallback.
type fallbackRun struct {
	ctx          context.Context
	local, cloud ReviewChecks
	budget       time.Duration
	remembered   Remembered
	dir          string
	identity     Identity
	localCtx     context.Context
	cloudCtx     context.Context
	cancels      []context.CancelFunc
	segments     []segment
	commandFrom  int
	refused      string
	runs         []api.CheckEngineRun
	flaky        []string
}

func (o *fallbackRun) cancel() {
	for _, cancel := range o.cancels {
		cancel()
	}
}

// engineContext is the budget of engine, which starts at its first use.
func (o *fallbackRun) engineContext(engine string) context.Context {
	slot := &o.localCtx
	if engine == api.CheckEngineCloud {
		slot = &o.cloudCtx
	}
	if *slot == nil {
		ctx, cancel := context.WithTimeout(o.ctx, o.budget)
		*slot = ctx
		o.cancels = append(o.cancels, cancel)
	}
	return *slot
}

// plan splits links into the runs they need. With split, remembered links are
// grouped apart from the others.
func (o *fallbackRun) plan(links []string, split bool) []group {
	if len(links) == 0 {
		return []group{{every: true, remembered: split && o.remembered.EveryCheck}}
	}
	var fresh, known []string
	for _, link := range links {
		if split && o.remembered.Has(link) {
			known = append(known, link)
		} else {
			fresh = append(fresh, link)
		}
	}
	var groups []group
	for _, invocation := range checkselect.InvocationGroups(fresh) {
		groups = append(groups, group{links: invocation.Links})
	}
	for _, invocation := range checkselect.InvocationGroups(known) {
		groups = append(groups, group{links: invocation.Links, remembered: true})
	}
	return groups
}

// The kinds of attempt result.
const (
	attemptPassed = iota
	attemptFailed
	attemptRefused
	attemptStopped
	attemptUnfinished
)

// attempt runs g on engine and says how it ended. A stopped attempt, which
// was cancelled or could not be prepared, returns its error. An unfinished
// one returns the reason a cloud attempt would follow.
func (o *fallbackRun) attempt(engine string, g group, skip []string) (kind int, reason string, skipped bool, err error) {
	runner := o.local
	if engine == api.CheckEngineCloud {
		runner = o.cloud
	}
	ctx := o.engineContext(engine)
	var result CheckResult
	if skipper, ok := runner.(SkippingChecks); ok && len(skip) > 0 {
		result, err = skipper.CheckSkipping(ctx, o.dir, o.identity, g.links, skip)
		skipped = true
	} else {
		result, err = runner.Check(ctx, o.dir, o.identity, g.links)
	}
	o.segments = append(o.segments, segment{engine: engine, group: g, result: result, before: o.refused})
	o.refused = ""
	o.flaky = append(o.flaky, slices.DeleteFunc(slices.Clone(result.Flaky), func(l string) bool { return slices.Contains(o.flaky, l) })...)
	switch {
	case o.ctx.Err() != nil:
		if err == nil {
			err = o.ctx.Err()
		}
		return attemptStopped, "", skipped, err
	case err != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil:
		return attemptUnfinished, api.CloudAfterTimeout, skipped, err
	case err != nil:
		return attemptStopped, "", skipped, err
	case result.ExitCode == 0:
		return attemptPassed, "", skipped, nil
	case len(FailedChecks(result.Output)) > 0:
		return attemptFailed, "", skipped, nil
	case result.Refusal != "" && len(g.links) > 0:
		return attemptRefused, "", skipped, nil
	}
	return attemptUnfinished, api.CloudAfterEngineFailure, skipped, nil
}

// escalation is a run that goes to the cloud engine.
type escalation struct {
	index  int
	group  group
	reason string
	skip   []string
}

// execute runs groups, local work first and then cloud work, and returns the
// refusal when Dagger refused the links of a run, which ends the execution.
// A non-nil error ends it as incomplete.
func (o *fallbackRun) execute(groups []group) (refusal string, err error) {
	var jobs []escalation
	trigger := ""
	for i, g := range groups {
		switch {
		case g.remembered:
			jobs = append(jobs, escalation{index: i, group: g, reason: api.CloudByMemory})
			continue
		case trigger != "":
			jobs = append(jobs, escalation{index: i, group: g, reason: trigger})
			continue
		}
		kind, reason, _, err := o.attempt(api.CheckEngineLocal, g, nil)
		last := o.segments[len(o.segments)-1].result
		switch kind {
		case attemptPassed, attemptFailed:
			o.runs = append(o.runs, g.run(api.CheckEngineLocal, "", 0))
			if kind == attemptFailed {
				return "", nil
			}
		case attemptRefused:
			return last.Refusal, nil
		case attemptStopped:
			return "", err
		case attemptUnfinished:
			trigger = reason
			jobs = append(jobs, escalation{index: i, group: g, reason: reason, skip: PassedChecks(last.Output)})
		}
	}
	sort.SliceStable(jobs, func(a, b int) bool { return jobs[a].index < jobs[b].index })
	for _, job := range jobs {
		kind, _, skipped, err := o.attempt(api.CheckEngineCloud, job.group, job.skip)
		last := o.segments[len(o.segments)-1].result
		passedLocally := 0
		if skipped {
			passedLocally = len(job.skip)
		}
		switch kind {
		case attemptPassed, attemptFailed, attemptUnfinished:
			o.runs = append(o.runs, job.group.run(api.CheckEngineCloud, job.reason, passedLocally))
		}
		switch kind {
		case attemptFailed:
			return "", nil
		case attemptRefused:
			return last.Refusal, nil
		case attemptStopped:
			return "", err
		case attemptUnfinished:
			return "", o.cloudIncomplete(last, err)
		}
	}
	return "", nil
}

// cloudIncomplete is the error of a cloud attempt that did not finish.
func (o *fallbackRun) cloudIncomplete(result CheckResult, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("the cloud attempt did not finish within checks_timeout %s", o.budget)
	}
	_, _, reason := ClassifyChecks(result, err)
	return fmt.Errorf("the cloud attempt did not finish: %w", reason)
}

// result combines the segments into one result. Output holds every segment's
// report, headed by its engine once the cloud engine ran any.
func (o *fallbackRun) result() CheckResult {
	var combined CheckResult
	var output strings.Builder
	endLine := func() {
		if text := output.String(); text != "" && !strings.HasSuffix(text, "\n") {
			output.WriteString("\n")
		}
	}
	cloud := slices.ContainsFunc(o.segments, func(s segment) bool { return s.engine == api.CheckEngineCloud })
	var commands [][]string
	for i, s := range o.segments {
		if s.before != "" {
			output.WriteString(s.before)
		} else if i > 0 {
			endLine()
		}
		command := ResultCommand(s.result, s.group.links)
		switch {
		case cloud:
			endLine()
			fmt.Fprintf(&output, "== %s engine ==\n", s.engine)
		case len(o.segments) > 1 && !slices.Contains(command, "&&") && s.result.Command != nil:
			endLine()
			fmt.Fprintf(&output, "$ %s\n", strings.Join(command, " "))
		}
		output.WriteString(s.result.Output)
		if i >= o.commandFrom && s.result.Command != nil {
			commands = append(commands, s.result.Command)
		}
		combined.Truncated = combined.Truncated || s.result.Truncated
		if s.result.TraceURL != "" {
			combined.TraceURL = s.result.TraceURL
		}
		if s.result.CIIdentityFallback != "" {
			combined.CIIdentityFallback = s.result.CIIdentityFallback
		}
		combined.ExitCode, combined.Refusal = s.result.ExitCode, s.result.Refusal
	}
	for i, command := range commands {
		if i > 0 {
			combined.Command = append(combined.Command, "&&")
		}
		combined.Command = append(combined.Command, command...)
	}
	combined.Output = output.String()
	combined.Flaky = o.flaky
	return combined
}
