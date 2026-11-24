package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/delivery"
	"github.com/kpenfound/osmia/internal/execution"
	"github.com/kpenfound/osmia/internal/flakes"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/intake"
	"github.com/kpenfound/osmia/internal/trace"
)

const flakeFixToolName = "flake_fix"

var flakeActor = trace.Actor{Kind: "service", ID: "flake-recorder"}

const flakeRescan = 5 * time.Minute

type flakeRecorder struct {
	s          *Service
	cfg        *config.Config
	repository *trace.Repository

	mu         sync.Mutex
	done       map[string]bool
	scanned    bool
	generation uint64
	at         time.Time
	logged     map[string]string
}

type flakeRun struct {
	stream   config.WorkstreamID
	path     string
	source   flakes.Source
	flaky    []string
	known    []string
	evidence flakes.Evidence
}

type located struct {
	group string
	flakes.Occurrence
}

func (r flakeRun) key() string { return flakes.RunKey(r.stream, r.path) }

func (r flakeRun) describe() string {
	if r.source.FinalReview {
		return fmt.Sprintf("the final review of workstream %s", r.stream)
	}
	return fmt.Sprintf("unit %s of workstream %s", r.source.Unit, r.stream)
}

func (f *flakeRecorder) Pass(ctx context.Context) error {
	if f.cfg == nil || !f.cfg.Project.FlakeFixes {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.reconcile(ctx); err != nil && ctx.Err() == nil {
		f.report("", err)
	}
	return nil
}

func (f *flakeRecorder) report(run string, err error) {
	if f.logged == nil {
		f.logged = map[string]string{}
	}
	if f.logged[run] == err.Error() {
		return
	}
	f.logged[run] = err.Error()
	ghlog.Logger(context.Background()).Warn("flake recording will be tried again", "project", string(f.repository.Project()), "run", run, "error", err.Error())
}

func (f *flakeRecorder) reconcile(ctx context.Context) error {
	now := f.s.now()
	generation := f.repository.Generation()
	if f.scanned && generation == f.generation && now.Sub(f.at) < flakeRescan {
		return nil
	}
	finished := true
	if err := f.settle(ctx); err != nil {
		finished = false
		f.report("settle", err)
	}
	runs, err := f.collect()
	if err != nil {
		return err
	}
	if f.done == nil {
		f.done = map[string]bool{}
	}
	for _, run := range runs {
		if f.done[run.key()] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := f.process(ctx, run); err != nil {
			finished = false
			f.report(run.key(), err)
			continue
		}
		f.done[run.key()] = true
		delete(f.logged, run.key())
	}
	if err := f.announce(ctx); err != nil {
		finished = false
		f.report("announce", err)
	} else {
		delete(f.logged, "announce")
	}
	if finished {
		delete(f.logged, "settle")
	}
	f.scanned, f.generation, f.at = finished, generation, now
	return nil
}

func (f *flakeRecorder) collect() ([]flakeRun, error) {
	streams, err := intake.NonSessionWorkstreams(f.repository)
	if err != nil {
		return nil, err
	}
	var runs []flakeRun
	var known []string
	for _, stream := range streams {
		if session, err := intake.IsSession(f.repository, stream); err != nil {
			return nil, err
		} else if session {
			continue
		}
		units, err := execution.CheckRuns(f.repository, stream)
		if err != nil {
			return nil, err
		}
		for _, r := range units {
			known = append(known, r.KnownChecks()...)
			if len(r.Flaky) == 0 || r.Status == ChecksSuperseded {
				continue
			}
			runs = append(runs, flakeRun{stream: stream, path: execution.CheckPath(r.Unit, r.Run), source: flakes.Source{Workstream: stream, Unit: r.Unit}, flaky: r.Flaky, known: r.KnownChecks(), evidence: flakeEvidence(r.FlakeEvidence(), r.Output)})
		}
		finals, err := finalCheckRuns(f.repository, stream)
		if err != nil {
			return nil, err
		}
		for _, r := range finals {
			links := flakes.CheckLinks(slices.Concat(r.Failed, r.Flaky)...)
			if r.Selection != nil {
				links = flakes.CheckLinks(slices.Concat(r.Selection.Links, links)...)
			}
			if len(r.Flaky) == 0 {
				continue
			}
			runs = append(runs, flakeRun{stream: stream, path: delivery.FinalChecksPath(r.Review), source: flakes.Source{Workstream: stream, FinalReview: true}, flaky: r.Flaky, known: links, evidence: flakeEvidence(r.FlakeEvidence(), r.Output)})
		}
	}
	known = flakes.CheckLinks(known...)
	for i := range runs {
		runs[i].known = flakes.CheckLinks(slices.Concat(runs[i].known, known)...)
	}
	return runs, nil
}

func finalCheckRuns(repository *trace.Repository, stream config.WorkstreamID) ([]FinalCheckRun, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return nil, err
	}
	latest := map[string]trace.Document{}
	for _, d := range docs {
		if finalCheckPathPattern.MatchString(d.Path) && d.Revision > latest[d.Path].Revision {
			latest[d.Path] = d
		}
	}
	var out []FinalCheckRun
	for _, d := range latest {
		var run FinalCheckRun
		if err := json.Unmarshal([]byte(d.Content), &run); err != nil {
			return nil, fmt.Errorf("%s revision %d: %w", d.Path, d.Revision, err)
		}
		out = append(out, run)
	}
	slices.SortFunc(out, func(a, b FinalCheckRun) int { return a.Review - b.Review })
	return out, nil
}

func flakeEvidence(e api.CheckEvidence, output string) flakes.Evidence {
	out := flakes.Evidence{Engine: e.Engine, Flaky: slices.Clone(e.Flaky), CoRun: slices.Clone(e.CoRun), Parallelism: e.Parallelism, Output: output, OutputPath: e.OutputPath}
	for _, d := range e.Diagnostics {
		text := strings.TrimSpace(d.Text)
		if d.Check != "" {
			text = d.Check + ":\n" + text
		}
		out.Diagnostics = append(out.Diagnostics, text)
	}
	for _, t := range e.Traces {
		for _, ref := range []string{t.TraceID, t.URL} {
			if ref != "" {
				out.Traces = append(out.Traces, flakes.Trace{Check: t.Check, Role: t.Kind, Ref: ref})
			}
		}
	}
	for _, t := range e.Timings {
		if d, err := time.ParseDuration(t.Duration); err == nil {
			out.Timings = append(out.Timings, flakes.Timing{Check: t.Check, Duration: d})
		}
	}
	for _, u := range e.Unavailable {
		if !slices.Contains(out.Unavailable, u.Item) {
			out.Unavailable = append(out.Unavailable, u.Item)
		}
	}
	return out
}

func (f *flakeRecorder) process(ctx context.Context, run flakeRun) error {
	store := &flakes.Store{Repository: f.repository, Now: f.s.now}
	fix, err := store.IsFlakeFix(run.stream)
	if err != nil {
		return err
	}
	groups := flakes.GroupFlaky(run.known, run.flaky)
	for _, g := range groups {
		_, _, err := store.Record(ctx, flakes.OccurrenceInput{Group: g.Group, Unresolved: g.Unresolved, Run: run.key(), Source: run.source, Links: g.Links, Evidence: run.evidence, FromFlakeFix: fix})
		if err != nil {
			return err
		}
		if err := f.s.step("flake-occurrence-recorded"); err != nil {
			return err
		}
	}
	occurrences, err := f.occurrences(store, run, groups)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(occurrences, func(o located) bool { return o.FromFlakeFix }) {
		return nil
	}
	noticed := slices.ContainsFunc(occurrences, func(o located) bool { return o.Noticed != nil })
	if !noticed {
		for _, o := range occurrences {
			if o.Decision.State != flakes.Pending {
				continue
			}
			workstream, open, err := store.OpenWorkstream(o.group)
			if err != nil {
				return err
			}
			if open {
				if _, err := store.Join(ctx, o.group, run.key(), workstream); err != nil {
					return err
				}
			}
		}
		if occurrences, err = f.occurrences(store, run, groups); err != nil {
			return err
		}
	}
	for _, o := range occurrences {
		if o.Decision.State != flakes.Joined || o.Decision.Workstream == "" || slices.ContainsFunc(o.Surfaced, func(s flakes.Surfacing) bool { return s.Workstream == o.Decision.Workstream }) {
			continue
		}
		f.s.flakeFixMu.Lock()
		err := f.s.announceToTarget(ctx, f.repository, store, o.group, o.Occurrence)
		f.s.flakeFixMu.Unlock()
		if err != nil {
			return err
		}
	}
	if err := f.s.step("flake-before-notice"); err != nil {
		return err
	}
	if !noticed {
		if err := f.notice(ctx, run, occurrences); err != nil {
			return err
		}
		if err := f.s.step("flake-notice-sent"); err != nil {
			return err
		}
	}
	for _, o := range occurrences {
		if o.Noticed == nil {
			if err := store.MarkNoticed(ctx, o.group, run.key()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *flakeRecorder) occurrences(store *flakes.Store, run flakeRun, groups []flakes.GroupLinks) ([]located, error) {
	var out []located
	for _, g := range groups {
		group, found, err := store.Group(g.Group)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("%w: flake group %q is not recorded", flakes.ErrUnknownOccurrence, g.Group)
		}
		o, ok := group.Find(run.key())
		if !ok {
			return nil, fmt.Errorf("%w: run %s in flake group %q", flakes.ErrUnknownOccurrence, run.key(), g.Group)
		}
		out = append(out, located{group: g.Group, Occurrence: o})
	}
	return out, nil
}

func flakeDigest(parts ...string) string {
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

func flakeNoticeSubject(run string) string { return "flake-notice-" + flakeDigest("notice", run) }

func flakeEvidenceSubject(run string, workstream config.WorkstreamID) string {
	return "flake-evidence-" + flakeDigest("evidence", run, string(workstream))
}

func (f *flakeRecorder) post(ctx context.Context, stream config.WorkstreamID, unit, subject, cause, reason, body string) error {
	state, err := f.repository.Workflow(stream, subject)
	if err != nil {
		return err
	}
	if state.Value != "" {
		return nil
	}
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: subject, Revision: 1, Project: f.repository.Project(), Workstream: stream, Unit: unit, At: f.s.now(), Actor: flakeActor, Cause: cause}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: "sent", Reason: reason},
		Events:     []trace.Event{trace.Notice(subject, "flake", body)}}
	if _, err := f.repository.Transact(ctx, tx); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

func (f *flakeRecorder) surface(ctx context.Context, store *flakes.Store, run flakeRun, o located) error {
	workstream := o.Decision.Workstream
	group := o.group
	var b strings.Builder
	fmt.Fprintf(&b, "New evidence for flake group `%s`, which this workstream is fixing.\n\n", group)
	fmt.Fprintf(&b, "%s recorded these flaky checks in the group:\n\n", run.describe())
	for _, link := range o.Links {
		fmt.Fprintf(&b, "- `%s`\n", link)
	}
	fmt.Fprintf(&b, "\nRun record: `%s` in the trace of workstream `%s`. Occurrence `%s` joined this workstream. This opens no new workstream and asks for no decision; weigh it when judging the cause.", o.RunPath(), run.stream, o.ID)
	reason := fmt.Sprintf("flake occurrence %s of group %s joined this flake-fix workstream: %s recorded %s", o.ID, group, run.describe(), strings.Join(o.Links, ", "))
	if err := f.post(ctx, workstream, "", flakeEvidenceSubject(o.Run, workstream), flakeNoticeSubject(o.Run), reason, b.String()); err != nil {
		return err
	}
	if err := f.s.step("flake-surfaced-before-mark"); err != nil {
		return err
	}
	return store.MarkSurfaced(ctx, group, o.Run, workstream)
}

func (f *flakeRecorder) notice(ctx context.Context, run flakeRun, occurrences []located) error {
	var pending, decided []located
	for _, o := range occurrences {
		if o.Decision.State == flakes.Pending {
			pending = append(pending, o)
		} else {
			decided = append(decided, o)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s recorded flaky checks: they failed and then passed when rerun by their own link, so the run counted them as passes. The run record is `%s`. This is one notice for the whole run. The workstream carries on whatever you decide.\n", capitalize(run.describe()), run.path)
	if len(pending) > 0 {
		fmt.Fprintf(&b, "\nAwaiting your decision (%d):\n", len(pending))
		for _, o := range pending {
			fmt.Fprintf(&b, "\n- Occurrence `%s`, flake group `%s`: flaky %s. Run record `%s`.\n", o.ID, o.group, codeList(o.Links), o.RunPath())
			fmt.Fprintf(&b, "  To open a flake-fix workstream that finds and fixes the cause, call %s with action \"open\", occurrence \"%s\", workstream \"%s\" and, if useful, your notes. To decline, call %s with action \"decline\", occurrence \"%s\", workstream \"%s\" and a reason (for example, the flaky test was added by the unit itself). A decision on this occurrence is final.\n", flakeFixToolName, o.ID, run.stream, flakeFixToolName, o.ID, run.stream)
		}
	}
	if len(decided) > 0 {
		fmt.Fprintf(&b, "\nFor information (%d), already decided:\n", len(decided))
		for _, o := range decided {
			if o.Decision.State == flakes.Joined {
				fmt.Fprintf(&b, "\n- Occurrence `%s`, flake group `%s`: flaky %s. It joined the open flake-fix workstream `%s`, whose chief of staff has these links as new evidence. Nothing for you to decide.\n", o.ID, o.group, codeList(o.Links), o.Decision.Workstream)
			} else {
				fmt.Fprintf(&b, "\n- Occurrence `%s`, flake group `%s`: flaky %s. Its decision is %s. Nothing for you to decide.\n", o.ID, o.group, codeList(o.Links), o.Decision.State)
			}
		}
	}
	reason := fmt.Sprintf("%s recorded flaky checks in %s (%d awaiting a decision, %d decided)", capitalize(run.describe()), run.path, len(pending), len(decided))
	for _, o := range decided {
		if o.Decision.State == flakes.Joined {
			reason += fmt.Sprintf("; group %s joined flake-fix workstream %s", o.group, o.Decision.Workstream)
		}
	}
	return f.post(ctx, run.stream, run.source.Unit, flakeNoticeSubject(run.key()), flakeNoticeSubject(run.key()), reason, b.String())
}

func codeList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = "`" + item + "`"
	}
	return strings.Join(quoted, ", ")
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
