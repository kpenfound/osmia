// Package jev asks Jev, TypeSafe's System One model, the bounded judgments of
// Osmia's turns through a Provider, and owns what Osmia does with them: the
// global switch, fallbacks, cool-down, trace records and ledger costs. The
// API client itself is internal/systemone. Nothing here runs on the
// scheduling path.
package jev

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/systemone"
	"github.com/kpenfound/osmia/internal/trace"
)

// Role is the ledger role of Jev's costs.
const Role = "jev"

// maxAttempts bounds how many times one judgment is started. An attempt
// interrupted before its result was recorded leaves only its start; the
// judgment then starts again, until this many have been interrupted.
const maxAttempts = 2

// maxRequests bounds the requests of one attempt; only a timeout or an
// unavailable provider is retried.
const maxRequests = 2

// maxRecordedRequest bounds the request body kept in a judgment record; a
// larger one is kept as its digest and size.
const maxRecordedRequest = 64 << 10

var taskName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// Source identifies a trace record, at a revision, that a judgment's state
// was built from, so the record shows what the answers rest on. Kind is the
// record kind, such as "turn-response" or "ruling".
type Source struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int    `json:"revision,omitempty"`
}

// Judgment is one bounded judgment asked inside a turn.
type Judgment struct {
	// Scope is the turn the judgment runs in; its project, workstream,
	// thread, turn and role are required.
	Scope coreadapter.Scope
	// Cause is the trace record that asked for the judgment, normally the
	// turn request, and Depth its causal depth. The cause scopes reuse: a
	// retried attempt of the same turn request gets the decision recorded
	// for it, while another turn asks afresh.
	Cause string
	Depth int
	// Task names what is judged, in lowercase letters, digits and hyphens.
	// Version numbers its questions, the meaning given to their answers and
	// Accept's thresholds; bump it whenever any of them changes. Decisions
	// recorded under the old version stay as they were.
	Task    string
	Version int
	Sources []Source
	// Request is the state and questions sent. Include only what the
	// question needs; it is recorded in the trace.
	Request systemone.Request
	// Accept applies the task's own thresholds to a valid response and
	// returns why it is not accepted, or "" to accept it. It must decide
	// from the response alone, since its verdict is recorded and reused;
	// Response.Model names the version that answered, against which the
	// thresholds were tuned. A nil Accept accepts every valid response.
	Accept func(systemone.Response) string
}

// Outcome is whether a judgment's answers are used.
type Outcome string

const (
	Accepted Outcome = "accepted"
	Fallback Outcome = "fallback"
)

// Decision is a judgment's result. Use the answers only when Outcome is
// Accepted; on a fallback, take the path the workflow takes without Jev.
type Decision struct {
	// ID is the judgment's trace record, empty when nothing was recorded.
	ID      string
	Outcome Outcome
	// Reason and Detail say why a fallback fell back.
	Reason Reason
	Detail string
	// Response holds the answers whenever the provider gave valid ones,
	// including a declined fallback's, which are kept for evaluation and
	// must not drive the workflow.
	Response *systemone.Response
	// Recovered says the decision was read from the trace, as recorded
	// when the workflow first used it, rather than asked now.
	Recovered bool
}

// The states of a judgment record. A started revision with no later one is
// an interrupted attempt; accepted and fallback revisions are final.
const (
	StateStarted  = "started"
	StateAccepted = "accepted"
	StateFallback = "fallback"
)

// Record is the content of a judgment's trace document, one revision per
// state: a started revision before each attempt's request, and one final
// revision. A fallback that sends no request has only its final revision.
type Record struct {
	Scope   coreadapter.Scope `json:"scope"`
	Task    string            `json:"task"`
	Version int               `json:"version"`
	// Model is the configured model; ResolvedModel the versioned model
	// that answered.
	Model   string   `json:"model"`
	Sources []Source `json:"sources,omitempty"`
	// Digest is the SHA-256 of the request body and RequestBytes its size.
	// Request is the body itself when it is at most 64 KiB.
	Digest       string          `json:"digest"`
	RequestBytes int             `json:"request_bytes"`
	Request      json.RawMessage `json:"request,omitempty"`
	State        string          `json:"state"`
	// Attempt numbers the attempt this revision belongs to, from 1.
	Attempt       int                         `json:"attempt"`
	ResolvedModel string                      `json:"resolved_model,omitempty"`
	Answers       map[string]systemone.Answer `json:"answers,omitempty"`
	Reason        Reason                      `json:"reason,omitempty"`
	Detail        string                      `json:"detail,omitempty"`
	Usage         *systemone.Usage            `json:"usage,omitempty"`
	// Requests counts the attempt's requests, a retry included, and
	// DurationMS how long they took.
	Requests   int   `json:"requests,omitempty"`
	DurationMS int64 `json:"duration_ms,omitempty"`
}

// Path is where a judgment's record lives in its workstream's trace.
func Path(id string) string { return "judgments/" + id + ".json" }

// Judge asks Jev the judgments of every project's turns. One Judge serves the
// whole service, so an outage cools every caller down together. It must be
// called from turn execution, never from scheduling: a judgment waits on a
// network call. A nil Judge has the boost off.
type Judge struct {
	// Config returns the current settings.
	Config func() config.Jev
	// Provider returns the provider for settings and a key. Nil uses a
	// systemone.Client for the settings' URL and model.
	Provider func(settings config.Jev, key string) Provider
	// Getenv reads the API key. Nil uses os.Getenv.
	Getenv func(string) string
	Now    func() time.Time

	mu       sync.Mutex
	settings config.Jev
	cool     *coolDown
}

func (j *Judge) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func (j *Judge) key(settings config.Jev) string {
	if j.Getenv != nil {
		return j.Getenv(settings.APIKeyEnv)
	}
	return os.Getenv(settings.APIKeyEnv)
}

// coolDown returns the cool-down of the current settings. Changed settings
// reach a provider afresh.
func (j *Judge) coolDown(settings config.Jev) *coolDown {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cool == nil || j.settings != settings {
		j.settings, j.cool = settings, &coolDown{}
	}
	return j.cool
}

func (j *Judge) provider(settings config.Jev, key string) Provider {
	if j.Provider != nil {
		return j.Provider(settings, key)
	}
	return systemone.Client{BaseURL: settings.URL, Model: settings.Model, APIKey: key}
}

func (j Judgment) validate() error {
	s := j.Scope
	for _, k := range []string{s.Thread, s.Turn, s.Role} {
		if !traceKey.MatchString(k) {
			return errors.New("judgment scope needs a thread, turn and role")
		}
	}
	if s.Project == "" || s.Workstream == "" || s.Unit != "" && !traceKey.MatchString(s.Unit) {
		return errors.New("judgment scope needs a project and workstream")
	}
	if !taskName.MatchString(j.Task) || j.Version < 1 {
		return errors.New("judgment needs a task name and a positive version")
	}
	if j.Cause == "" || j.Depth < 0 {
		return errors.New("judgment needs a cause")
	}
	return j.Request.Validate()
}

var traceKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// Evaluate returns the judgment's decision; repo is the trace of the
// judgment's project. While the boost is off it returns a disabled fallback
// and neither sends nor records anything. Otherwise the
// judgment is identified by its cause, task, version and exact request, so
// the same judgment asked again, after a restart included, returns the
// decision recorded the first time instead of asking again, and changed
// inputs are a new judgment. Every failure is a fallback; Evaluate never
// returns accepted answers it could not record. It waits for at most two
// requests of the configured timeout, and returns when ctx ends, leaving the
// attempt interrupted.
func (j *Judge) Evaluate(ctx context.Context, repo *trace.Repository, judgment Judgment) Decision {
	if j == nil {
		return Decision{Outcome: Fallback, Reason: ReasonDisabled}
	}
	settings := j.Config()
	if !settings.Enabled {
		return Decision{Outcome: Fallback, Reason: ReasonDisabled}
	}
	if err := judgment.validate(); err != nil {
		return Decision{Outcome: Fallback, Reason: ReasonRejected, Detail: err.Error()}
	}
	body, err := judgment.Request.Body(settings.Model)
	if err != nil {
		return Decision{Outcome: Fallback, Reason: ReasonRejected, Detail: err.Error()}
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(body))
	identity, _ := json.Marshal([]any{judgment.Cause, judgment.Task, judgment.Version, digest})
	id := fmt.Sprintf("judgment_%x", sha256.Sum256(identity))
	stream := config.WorkstreamID(judgment.Scope.Workstream)

	latest, revision, err := load(repo, stream, id)
	if err != nil {
		return Decision{ID: id, Outcome: Fallback, Reason: ReasonUnrecorded, Detail: "judgment record cannot be read"}
	}
	if revision > 0 && latest.State != StateStarted {
		return latest.decision(id, true)
	}
	base := Record{Scope: judgment.Scope, Task: judgment.Task, Version: judgment.Version, Model: settings.Model, Sources: judgment.Sources, Digest: digest, RequestBytes: len(body)}
	if len(body) <= maxRecordedRequest {
		base.Request = body
	}
	w := writer{repo: repo, judgment: judgment, id: id, revision: revision, now: j.now}
	attempt := latest.Attempt + 1
	// Writes outlive the turn's cancellation, so an answer that arrived is
	// recorded with its cost.
	record := context.WithoutCancel(ctx)
	finish := func(r Record) Decision {
		r.Attempt = attempt
		if err := w.write(record, r); err != nil {
			return Decision{ID: id, Outcome: Fallback, Reason: ReasonUnrecorded, Detail: "judgment result cannot be recorded"}
		}
		return r.decision(id, false)
	}
	if revision > 0 && latest.Attempt >= maxAttempts {
		attempt = latest.Attempt
		r := base
		r.State, r.Reason, r.Detail = StateFallback, ReasonInterrupted, fmt.Sprintf("%d attempts were interrupted", latest.Attempt)
		return finish(r)
	}
	cool := j.coolDown(settings)
	key := j.key(settings)
	if key == "" {
		r := base
		r.State, r.Reason, r.Detail = StateFallback, ReasonUnconfigured, settings.APIKeyEnv+" is not set"
		return finish(r)
	}
	if cooling, until := cool.cooling(j.now()); cooling {
		r := base
		r.State, r.Reason, r.Detail = StateFallback, ReasonCoolingDown, "provider is cooling down until "+until.UTC().Format(time.RFC3339)
		return finish(r)
	}
	started := base
	started.State, started.Attempt = StateStarted, attempt
	if err := w.write(ctx, started); err != nil {
		return Decision{ID: id, Outcome: Fallback, Reason: ReasonUnrecorded, Detail: "judgment start cannot be recorded"}
	}

	provider := j.provider(settings, key)
	begin := j.now()
	var resp systemone.Response
	var last *failure
	requests := 0
	for requests < maxRequests {
		requests++
		call, cancel := context.WithTimeout(ctx, settings.RequestTimeout())
		resp, err = provider.Evaluate(call, judgment.Request)
		cancel()
		if err == nil {
			last = nil
			cool.observe(j.now(), nil)
			break
		}
		last = failed(err)
		if ctx.Err() != nil {
			// The turn ended; the start stays as an interrupted attempt.
			return Decision{ID: id, Outcome: Fallback, Reason: ReasonCancelled, Detail: "turn ended during the judgment"}
		}
		cool.observe(j.now(), last)
		if paused, _ := cool.cooling(j.now()); paused || !last.reason.transient() {
			break
		}
	}
	r := base
	r.Requests, r.DurationMS = requests, j.now().Sub(begin).Milliseconds()
	if last != nil {
		r.State, r.Reason, r.Detail = StateFallback, last.reason, last.detail
		return finish(r)
	}
	r.ResolvedModel, r.Answers, r.Usage = resp.Model, resp.Answers, &resp.Usage
	r.State = StateAccepted
	if judgment.Accept != nil {
		if reason := judgment.Accept(resp); reason != "" {
			r.State, r.Reason, r.Detail = StateFallback, ReasonDeclined, reason
		}
	}
	if err := w.cost(record, attempt, resp.Usage); err != nil {
		return Decision{ID: id, Outcome: Fallback, Reason: ReasonUnrecorded, Detail: "judgment cost cannot be recorded"}
	}
	return finish(r)
}

func (r Record) decision(id string, recovered bool) Decision {
	d := Decision{ID: id, Outcome: Fallback, Reason: r.Reason, Detail: r.Detail, Recovered: recovered}
	if r.State == StateAccepted {
		d.Outcome = Accepted
	}
	if r.ResolvedModel != "" {
		d.Response = &systemone.Response{Model: r.ResolvedModel, Answers: r.Answers}
		if r.Usage != nil {
			d.Response.Usage = *r.Usage
		}
	}
	return d
}

// load returns the latest revision of a judgment record, or revision 0.
func load(repo *trace.Repository, stream config.WorkstreamID, id string) (Record, int, error) {
	docs, err := trace.Read[trace.Document](repo, stream)
	if err != nil {
		return Record{}, 0, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == id && d.Revision > latest.Revision {
			latest = d
		}
	}
	if latest.Revision == 0 {
		return Record{}, 0, nil
	}
	var r Record
	if latest.Path != Path(id) || json.Unmarshal([]byte(latest.Content), &r) != nil || r.Attempt < 1 {
		return Record{}, 0, fmt.Errorf("judgment record %s is damaged", id)
	}
	return r, latest.Revision, nil
}

type writer struct {
	repo     *trace.Repository
	judgment Judgment
	id       string
	revision int
	now      func() time.Time
}

func (w *writer) header(schema, id string, revision int) trace.Header {
	s := w.judgment.Scope
	return trace.Header{Schema: schema, Version: trace.Version, ID: id, Revision: revision, Project: config.ProjectID(s.Project), Workstream: config.WorkstreamID(s.Workstream), Unit: s.Unit, At: w.now(), Actor: trace.Actor{Kind: "service", ID: Role}, Cause: w.judgment.Cause, Depth: w.judgment.Depth + 1}
}

// write records the next revision of the judgment.
func (w *writer) write(ctx context.Context, r Record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	doc := trace.Document{Header: w.header("osmia.trace.document", w.id, w.revision+1), Path: Path(w.id), Content: string(data)}
	if err := w.repo.RecordDocuments(ctx, []trace.Document{doc}); err != nil {
		return err
	}
	w.revision++
	return nil
}

// cost appends an attempt's usage to the ledger. It is recorded before the
// result, so every recorded answer has its cost.
func (w *writer) cost(ctx context.Context, attempt int, u systemone.Usage) error {
	scope := w.judgment.Scope
	scope.Role = Role
	usage := coreadapter.Usage{CostUSD: u.CostUSD, CostKnown: u.CostKnown}
	if !usage.CostKnown {
		usage.CostUSD = 0
	}
	id := trace.EventID(w.id, fmt.Sprintf("cost-%d", attempt))
	c := trace.Cost{Header: w.header("osmia.trace.cost", id, 1), Entry: coreadapter.LedgerEntry{Scope: scope, AttemptID: trace.EventID(w.id, fmt.Sprintf("attempt-%d", attempt)), At: w.now(), Usage: usage}}
	if err := w.repo.Append(ctx, c); err != nil && !errors.Is(err, trace.ErrConflict) {
		return err
	}
	return nil
}

// Mode is the boost's state as status reports it.
type Mode string

const (
	ModeDisabled Mode = "disabled"
	// ModeUnconfigured is an enabled boost without its API key; every
	// judgment falls back.
	ModeUnconfigured Mode = "unconfigured"
	ModeReady        Mode = "ready"
	// ModeDegraded is an enabled boost whose latest request failed or that
	// is cooling down; judgments fall back while it cools.
	ModeDegraded Mode = "degraded"
)

// Status reports the boost's mode, the configured model and the latest
// failure since the last success.
type Status struct {
	Mode         Mode       `json:"mode"`
	Model        string     `json:"model,omitempty"`
	Reason       Reason     `json:"reason,omitempty"`
	Detail       string     `json:"detail,omitempty"`
	CoolingUntil *time.Time `json:"cooling_until,omitempty"`
}

// Status reports the boost under the current settings.
func (j *Judge) Status() Status {
	if j == nil {
		return Status{Mode: ModeDisabled}
	}
	settings := j.Config()
	if !settings.Enabled {
		return Status{Mode: ModeDisabled}
	}
	out := Status{Mode: ModeReady, Model: settings.Model}
	if j.key(settings) == "" {
		out.Mode, out.Reason, out.Detail = ModeUnconfigured, ReasonUnconfigured, settings.APIKeyEnv+" is not set"
		return out
	}
	cool := j.coolDown(settings)
	if f := cool.lastFailure(); f != nil {
		out.Mode, out.Reason, out.Detail = ModeDegraded, f.reason, f.detail
	}
	if cooling, until := cool.cooling(j.now()); cooling {
		out.Mode, out.CoolingUntil = ModeDegraded, &until
	}
	return out
}
