package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/isolation"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

// ExtractAction is the runner-boundary operation action that runs one
// knowledge-base extraction pass of the librarian.
const ExtractAction = "kb-extract"

const (
	librarianRole   = "librarian"
	librarianAgent  = "agent_librarian"
	librarianThread = "thread_librarian"
	// maxExtractionAttempts bounds the librarian turns one extraction may
	// start after service stops interrupt earlier ones.
	maxExtractionAttempts = 3
)

var librarianActor = trace.Actor{Kind: "agent", ID: librarianAgent}

// Librarian supplies the execution boundary of the librarian's turns: the
// core execution engine and the role-scoped MCP host. The service owns
// the librarian's view, tools, prompt and output capture.
type Librarian struct {
	Engine coreadapter.Engine
	Hosts  coreadapter.MCPHosts
}

// librarianWorkstream is the project's workstream holding the librarian
// thread and its extraction operations, derived from the project ID.
func librarianWorkstream(project config.ProjectID) config.WorkstreamID {
	sum := sha256.Sum256([]byte("librarian:" + string(project)))
	return config.WorkstreamID("w_" + hex.EncodeToString(sum[:16]))
}

type extractionInput struct {
	Extraction int `json:"extraction"`
}

func extractionIDs(n int) (transition, event string) {
	transition = fmt.Sprintf("extract-%d", n)
	return transition, trace.EventID(transition, "run")
}
func turnID(n, attempt int) string { return fmt.Sprintf("extract-%d-%d", n, attempt) }

// ensureLibrarianThread creates the librarian's workstream and thread in the
// trace when they are missing. Both are idempotent.
func ensureLibrarianThread(ctx context.Context, r *trace.Repository, at time.Time, actor trace.Actor) (config.WorkstreamID, error) {
	stream := librarianWorkstream(r.Project())
	streams, err := r.Workstreams()
	if err != nil {
		return stream, err
	}
	if !slices.Contains(streams, stream) {
		if err := r.CreateWorkstream(ctx, stream, at, actor); err != nil {
			return stream, err
		}
	}
	_, err = r.Thread(stream, librarianAgent)
	if errors.Is(err, os.ErrNotExist) {
		identity := trace.Agent{Header: trace.Header{Schema: "osmia.trace.agent", Version: trace.Version, ID: librarianAgent, Revision: 1, Project: r.Project(), Workstream: stream, At: at, Actor: actor, Cause: "librarian-thread"}, Role: librarianRole, ThreadID: librarianThread}
		err = r.CreateThread(ctx, identity)
	}
	return stream, err
}

// requestExtraction publishes extraction n as a durable operation. Extractions
// are numbered consecutively; n-1 must be the latest requested.
func requestExtraction(ctx context.Context, r *trace.Repository, n int, at time.Time, actor trace.Actor, cause, reason string) error {
	stream := librarianWorkstream(r.Project())
	transition, event := extractionIDs(n)
	input, err := json.Marshal(extractionInput{Extraction: n})
	if err != nil {
		return err
	}
	op := coreadapter.Operation{ID: trace.OperationID(r.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: ExtractAction, Input: input}
	from := ""
	if n > 1 {
		from = "requested"
	}
	tx := trace.Transaction{ExpectedVersion: uint64(n - 1), Transition: trace.Transition{
		Header:  trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: transition, Revision: 1, Project: r.Project(), Workstream: stream, At: at, Actor: actor, Cause: cause},
		Subject: "extraction", From: from, To: "requested", Reason: reason},
		Events: []trace.Event{{ID: event, Kind: "extraction", Body: fmt.Sprintf("Run knowledge-base extraction %d", n), Operation: &op}}}
	_, err = r.Transact(ctx, tx)
	return err
}

// ensureExtraction starts the first extraction of a newly registered project,
// creating the librarian's thread first. It uses the active project's open
// trace, or opens the trace when the project is not active yet.
func (s *Service) ensureExtraction(ctx context.Context, root config.Root, p config.Project) error {
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	var r *trace.Repository
	if active != nil && cfg.HasProject() && cfg.Project.ID == p.ID {
		r = active.repository
	} else {
		var err error
		r, err = trace.Open(root, p)
		if err != nil {
			if r != nil {
				r.Close()
			}
			return err
		}
		defer r.Close()
	}
	at := s.now()
	stream, err := ensureLibrarianThread(ctx, r, at, registrationActor)
	if err != nil {
		return err
	}
	ops, err := r.Operations(stream)
	if err != nil {
		return err
	}
	for _, o := range ops {
		if o.Operation.Action == ExtractAction {
			return nil
		}
	}
	return requestExtraction(ctx, r, 1, at, registrationActor, "project-add", "Knowledge-base extraction on project registration")
}

// extractionState derives the latest extraction's state from its operation
// record: running while a live claim has started the effect, pending while it
// is queued or waiting to retry (with the retry's reason), otherwise the
// result. A trace without a librarian workstream has no extraction.
func extractionState(r *trace.Repository) (*ExtractionState, error) {
	stream := librarianWorkstream(r.Project())
	streams, err := r.Workstreams()
	if err != nil {
		return nil, err
	}
	if !slices.Contains(streams, stream) {
		return nil, nil
	}
	ops, err := r.Operations(stream)
	if err != nil {
		return nil, err
	}
	var latest *trace.OperationRecord
	n := 0
	for i, o := range ops {
		if o.Operation.Action != ExtractAction {
			continue
		}
		var in extractionInput
		if err := json.Unmarshal(o.Operation.Input, &in); err != nil {
			return nil, err
		}
		if in.Extraction > n {
			latest, n = &ops[i], in.Extraction
		}
	}
	if latest == nil {
		return nil, nil
	}
	state := &ExtractionState{Extraction: n, State: "pending", At: latest.Transition.At}
	for _, a := range latest.History {
		state.At = a.At
		if a.Kind == "result" {
			// The result fixes the extraction's time; a later acknowledgement
			// must not move it.
			break
		}
		switch a.Kind {
		case "retry":
			state.Reason = a.Failure
		case "claim":
			state.Reason = ""
		}
	}
	switch {
	case latest.Result != nil:
		state.State, state.Reason = latest.Result.Outcome, ""
		if state.State == "failed" {
			state.Reason = latest.Result.Evidence
		}
	case latest.EffectStarted && latest.Claim != nil:
		state.State, state.Reason = "running", ""
	}
	return state, nil
}

// projectExtraction reports the active project's latest extraction.
func (s *Service) projectExtraction(id config.ProjectID) (*ExtractionState, error) {
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	if !cfg.HasProject() || cfg.Project.ID != id {
		return nil, errNoActiveProject
	}
	if active == nil {
		return nil, errNoTrace
	}
	return extractionState(active.repository)
}

// extractProject starts a new extraction pass of the active project. One that
// is still pending or running is not doubled.
func (s *Service) extractProject(ctx context.Context, req ProjectExtractRequest) (ExtractionResponse, *APIError) {
	if err := config.CheckProjectIDs(req.Project); err != nil {
		return ExtractionResponse{}, &APIError{Validation, "project must be a project ID: p_ followed by 32 lowercase hexadecimal digits"}
	}
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	s.mu.Lock()
	active, cfg := s.active, s.cfg
	s.mu.Unlock()
	if !cfg.HasProject() || cfg.Project.ID != req.Project {
		return ExtractionResponse{}, &APIError{NotFound, fmt.Sprintf("project %s is not an active project; check the project ID with osmia status", req.Project)}
	}
	if active == nil {
		return ExtractionResponse{}, &APIError{Internal, fmt.Sprintf("project %s is configured but has no trace repository; register it with osmia project add", req.Project)}
	}
	r := active.repository
	internal := &APIError{Internal, fmt.Sprintf("cannot record the extraction request in the trace of project %s; check the trace repository", req.Project)}
	at := s.now()
	if _, err := ensureLibrarianThread(ctx, r, at, registrationActor); err != nil {
		return ExtractionResponse{}, internal
	}
	state, err := extractionState(r)
	if err != nil {
		return ExtractionResponse{}, internal
	}
	n := 1
	if state != nil {
		if state.State == "pending" || state.State == "running" {
			return ExtractionResponse{}, &APIError{Conflict, fmt.Sprintf("extraction %d of project %s is still %s; wait for it to finish and check osmia status", state.Extraction, req.Project, state.State)}
		}
		n = state.Extraction + 1
	}
	if err := requestExtraction(ctx, r, n, at, registrationActor, "owner-request", "Knowledge-base extraction requested by the owner"); err != nil {
		return ExtractionResponse{}, internal
	}
	return ExtractionResponse{Project: projectView(cfg.Root, cfg.Project), Extraction: ExtractionState{Extraction: n, State: "pending", At: at}}, nil
}

// runnerAdapter routes runner-boundary operations: extraction passes to the
// service's extractor, architect drafts to its drafter, everything else to
// the bound thread reconciler.
type runnerAdapter struct {
	turns   coreadapter.Reconciler
	extract *extractor
	draft   *drafter
}

func (a runnerAdapter) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	if op.Action == ExtractAction {
		return a.extract.Inspect(ctx, op)
	}
	if op.Action == DraftAction {
		return a.draft.Inspect(ctx, op)
	}
	if a.turns == nil {
		return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "No reconciliation adapter configured"}, nil
	}
	return a.turns.Inspect(ctx, op)
}
func (a runnerAdapter) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	if op.Action == ExtractAction {
		return a.extract.Apply(ctx, op)
	}
	if op.Action == DraftAction {
		return a.draft.Apply(ctx, op)
	}
	if a.turns == nil {
		return coreadapter.OperationResult{}, errors.New("no runner adapter is configured")
	}
	return a.turns.Apply(ctx, op)
}

// extractor reconciles extraction operations. Each runs one librarian turn
// through the thread dispatcher, then validates the turn's output and records
// it as document revisions keyed by the operation ID.
type extractor struct {
	s          *Service
	repository *trace.Repository
}

var _ coreadapter.Reconciler = (*extractor)(nil)

func decodeExtraction(op coreadapter.Operation) (extractionInput, error) {
	var in extractionInput
	if op.Boundary != coreadapter.RunnerBoundary || op.Action != ExtractAction {
		return in, fmt.Errorf("unsupported runner operation %q", op.Action)
	}
	d := json.NewDecoder(bytes.NewReader(op.Input))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return in, fmt.Errorf("invalid extraction operation input: %w", err)
	}
	if in.Extraction < 1 {
		return in, errors.New("extraction operation requires a positive extraction number")
	}
	return in, nil
}

// recorded returns the document revisions an extraction operation recorded.
func (e *extractor) recorded(operation string) ([]trace.Document, error) {
	docs, err := trace.Read[trace.Document](e.repository, "")
	if err != nil {
		return nil, err
	}
	var out []trace.Document
	for _, d := range docs {
		if d.Cause == operation {
			out = append(out, d)
		}
	}
	return out, nil
}

func extractionResult(docs []trace.Document) coreadapter.OperationResult {
	subsystems, removed := []string{}, []string{}
	for _, d := range docs {
		if name, ok := kb.Subsystem(d.Path); ok {
			if d.Content == "" {
				removed = append(removed, name)
			} else {
				subsystems = append(subsystems, name)
			}
		}
	}
	sort.Strings(subsystems)
	sort.Strings(removed)
	data, _ := json.Marshal(struct {
		Subsystems []string `json:"subsystems"`
		Removed    []string `json:"removed"`
	}{subsystems, removed})
	return coreadapter.OperationResult{Outcome: "succeeded", Evidence: fmt.Sprintf("recorded %d subsystem files and the entity map", len(subsystems)), Data: data}
}
func failedExtraction(reason string) coreadapter.OperationResult {
	return coreadapter.OperationResult{Outcome: "failed", Evidence: reason}
}

// extractionTurns returns the turns of extraction n in attempt order.
func extractionTurns(t trace.Thread, n int) []trace.QueuedTurn {
	prefix := fmt.Sprintf("extract-%d-", n)
	var out []trace.QueuedTurn
	for _, q := range t.Turns {
		if strings.HasPrefix(q.Request.TurnID, prefix) {
			out = append(out, q)
		}
	}
	return out
}

// Inspect reads the recorded documents and the librarian thread. Recorded
// documents complete the operation; a turn this session is running is unknown;
// everything else is absent, and Apply decides between running, retrying and
// recording.
func (e *extractor) Inspect(_ context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := decodeExtraction(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	docs, err := e.recorded(op.ID)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if len(docs) > 0 {
		result := extractionResult(docs)
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "knowledge base recorded", Result: &result}, nil
	}
	t, err := e.repository.Thread(librarianWorkstream(e.repository.Project()), librarianAgent)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	turns := extractionTurns(t, in.Extraction)
	if len(turns) == 0 {
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "no librarian turn has run"}, nil
	}
	last := turns[len(turns)-1]
	switch {
	case last.Claim != nil && last.Response == nil && t.Status != "interrupted":
		return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "librarian turn " + last.Request.TurnID + " is running"}, nil
	case last.Claim != nil && last.Response == nil:
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "librarian turn " + last.Request.TurnID + " was interrupted by a service stop"}, nil
	case last.Claim == nil:
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "librarian turn " + last.Request.TurnID + " is queued and unclaimed"}, nil
	case last.CompletedAt.IsZero():
		return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "librarian turn " + last.Request.TurnID + " is captured and not completed"}, nil
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "librarian turn " + last.Request.TurnID + " is " + last.Status() + "; output not recorded"}, nil
}

// Apply drives the extraction to a terminal result: it abandons a turn a
// previous service stop interrupted, starts a new turn while attempts remain,
// runs the pending turn through the dispatcher, and records a completed turn's
// output. Invalid output and failed turns are terminal failures; storage
// errors leave the operation pending for another attempt.
func (e *extractor) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := decodeExtraction(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	docs, err := e.recorded(op.ID)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if len(docs) > 0 {
		return extractionResult(docs), nil
	}
	cfg := e.s.current()
	if !cfg.HasProject() || cfg.Project.ID != e.repository.Project() {
		return coreadapter.OperationResult{}, errors.New("the project is not active")
	}
	stream := librarianWorkstream(e.repository.Project())
	for {
		if err := ctx.Err(); err != nil {
			return coreadapter.OperationResult{}, err
		}
		t, err := e.repository.Thread(stream, librarianAgent)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		turns := extractionTurns(t, in.Extraction)
		var last *trace.QueuedTurn
		if len(turns) > 0 {
			last = &turns[len(turns)-1]
		}
		switch {
		case last != nil && last.Claim != nil && last.Response == nil:
			if t.Status != "interrupted" {
				return coreadapter.OperationResult{}, errors.New("librarian turn " + last.Request.TurnID + " is still running")
			}
			if err := e.repository.AbandonTurn(ctx, stream, librarianAgent, last.Request.TurnID, e.s.now()); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case last == nil || last.Status() == "interrupted":
			if len(turns) >= maxExtractionAttempts {
				return failedExtraction(fmt.Sprintf("the librarian turn was interrupted %d times by service stops; run osmia project extract to try again", len(turns))), nil
			}
			if e.s.options.Librarian == nil {
				return failedExtraction("this service has no agent runner for the librarian; run osmia project extract once one is configured"), nil
			}
			if err := e.enqueue(ctx, cfg, in.Extraction, len(turns)+1, op.ID); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case last.CompletedAt.IsZero():
			if _, err := e.dispatch(ctx, stream, last.Request.TurnID); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if err := os.RemoveAll(filepath.Join(e.turnDirectory(last.Request.TurnID), "workspace")); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case last.Status() == "idle":
			return e.record(ctx, op.ID, *last)
		default:
			reason := last.Response.Failure
			if reason == "" {
				reason = "the turn ended with status " + last.Status()
			}
			return failedExtraction("librarian turn " + last.Request.TurnID + " failed: " + reason), nil
		}
	}
}

// roleExecution resolves a role's effective profile: the runtime override
// when one is set, otherwise the configured role binding.
func (s *Service) roleExecution(cfg *config.Config, role string) (coreadapter.Profile, coreadapter.ExecutionSettings, error) {
	state, _ := s.store.Effective()
	name := state.Profiles[role]
	if name == "" {
		name = cfg.Roles[role].Profile
	}
	p, ok := cfg.Profiles[name]
	if !ok {
		return coreadapter.Profile{}, coreadapter.ExecutionSettings{}, fmt.Errorf("unknown profile %q", name)
	}
	timeout, err := time.ParseDuration(p.Timeout)
	if err != nil {
		return coreadapter.Profile{}, coreadapter.ExecutionSettings{}, err
	}
	r := cfg.Roles[role]
	return coreadapter.Profile{Name: name, Backend: p.Agent, Model: p.Model, Effort: p.Effort, Timeout: timeout, MaxTurns: p.MaxTurns}, coreadapter.ExecutionSettings{Mode: r.Sandbox, Image: r.Image}, nil
}

// enqueue accepts the librarian turn of one extraction attempt, fixing its
// profile and prompts.
func (e *extractor) enqueue(ctx context.Context, cfg *config.Config, n, attempt int, operation string) error {
	profile, _, err := e.s.roleExecution(cfg, librarianRole)
	if err != nil {
		return err
	}
	turn := turnID(n, attempt)
	at := e.s.now()
	req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: e.repository.Project(), Workstream: librarianWorkstream(e.repository.Project()), At: at, Actor: trace.Actor{Kind: "service", ID: "librarian-extraction"}, Cause: operation, Depth: 1},
		AgentID: librarianAgent, ThreadID: librarianThread, TurnID: turn, Profile: profile, SystemPrompt: librarianSystemPrompt(cfg.Project), Prompt: librarianPrompt(cfg.Project)}
	_, err = e.repository.EnqueueTurn(ctx, req)
	return err
}

// turnDirectory is the service-owned directory of one librarian turn: its
// staged workspace, its backend session and the captured output.
func (e *extractor) turnDirectory(turn string) string {
	return filepath.Join(e.s.current().Root.String(), "librarian", string(e.repository.Project()), turn)
}

// dispatch runs the turn through the thread dispatcher and runner.
func (e *extractor) dispatch(ctx context.Context, stream config.WorkstreamID, turn string) (coreadapter.OperationResult, error) {
	d := thread.Dispatcher{Runner: thread.Runner{Store: e.repository, Turns: e.turns(), Now: e.s.now}, Prepare: func(_ context.Context, in thread.TurnInput) (coreadapter.PreparedTurn, error) {
		directory := filepath.Join(e.turnDirectory(in.Turn), "session")
		return coreadapter.PreparedTurn{SessionDirectory: directory}, os.MkdirAll(directory, 0700)
	}}
	op, err := thread.TurnOperation(e.repository.Project(), "extraction-turn", thread.TurnInput{Workstream: stream, Agent: librarianAgent, Turn: turn})
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	return d.Apply(ctx, op)
}

// turns is the librarian's isolated turn path: a private view of the staged
// workspace, the file tools plus the role's notes, and no execute, network or
// VCS capability.
func (e *extractor) turns() *isolation.Turns {
	var engine coreadapter.Engine
	var hosts coreadapter.MCPHosts
	if l := e.s.options.Librarian; l != nil {
		engine, hosts = l.Engine, l.Hosts
	}
	return &isolation.Turns{
		Workspaces: stagedWorkspaces{},
		Views:      isolation.Views{Directory: filepath.Join(e.s.current().Root.String(), "views")},
		Select:     e.selectView,
		Grants:     map[string]coreadapter.Capabilities{librarianRole: {Tools: []string{"file_read", "file_write", "notes_read", "notes_write"}, WriteFiles: true}},
		Scoped: func(_ context.Context, scope coreadapter.Scope) ([]coreadapter.Tool, error) {
			return e.repository.NotesTools(librarianAgent, scope)
		},
		Hosts:   hosts,
		Engine:  engine,
		Capture: e.capture,
	}
}

// stagedWorkspaces lends a service-staged directory as a turn's workspace.
type stagedWorkspaces struct{}

type noLease struct{}

func (noLease) Release(context.Context) error { return nil }

func (stagedWorkspaces) Acquire(ctx context.Context, req coreadapter.WorkspaceRequest) (coreadapter.WorkspaceLease, error) {
	if err := ctx.Err(); err != nil {
		return coreadapter.WorkspaceLease{}, err
	}
	if req.SourceDirectory == "" {
		return coreadapter.WorkspaceLease{}, errors.New("staged workspace requires a source directory")
	}
	return coreadapter.WorkspaceLease{Workspace: coreadapter.Workspace{ID: filepath.Base(req.SourceDirectory), Directory: req.SourceDirectory, Access: req.Access}, Lease: noLease{}}, nil
}

// selectView stages the librarian's workspace for the claimed turn and selects
// all of it: repo/, kb/, seed/ and the empty output/.
func (e *extractor) selectView(ctx context.Context, scope coreadapter.Scope) (isolation.Selection, error) {
	cfg := e.s.current()
	if scope.Role != librarianRole || scope.Project != string(e.repository.Project()) || !cfg.HasProject() || cfg.Project.ID != e.repository.Project() {
		return isolation.Selection{}, errors.New("view selection denied")
	}
	_, settings, err := e.s.roleExecution(cfg, librarianRole)
	if err != nil {
		return isolation.Selection{}, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.Root.String(), "views"), 0700); err != nil {
		return isolation.Selection{}, err
	}
	workspace := filepath.Join(e.turnDirectory(scope.Turn), "workspace")
	if err := e.stage(ctx, cfg.Project.Clone, workspace); err != nil {
		return isolation.Selection{}, err
	}
	return isolation.Selection{Workspace: coreadapter.WorkspaceRequest{SourceDirectory: workspace, Directory: workspace}, Paths: []string{"repo", "kb", "seed", kb.OutputDirectory}, Execution: settings}, nil
}

// stage builds the workspace: the clone's tracked files under repo/, the
// current knowledge base under kb/, the deterministic entity seed of those
// tracked files under seed/ and an empty output/.
func (e *extractor) stage(ctx context.Context, clone, workspace string) error {
	if err := os.RemoveAll(workspace); err != nil {
		return err
	}
	for _, name := range []string{"repo", "kb", "seed", kb.OutputDirectory} {
		if err := os.MkdirAll(filepath.Join(workspace, name), 0700); err != nil {
			return err
		}
	}
	if err := copyTracked(ctx, clone, filepath.Join(workspace, "repo")); err != nil {
		return err
	}
	seed, err := kb.Seed(filepath.Join(workspace, "repo"))
	if err != nil {
		return err
	}
	current, err := kb.Load(e.repository)
	if err != nil {
		return err
	}
	entities, err := kb.Encode(current)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(workspace, "kb", "entities.json"), entities, 0600); err != nil {
		return err
	}
	docs, err := trace.Read[trace.Document](e.repository, "")
	if err != nil {
		return err
	}
	for _, d := range latestDocuments(docs) {
		if name, ok := kb.Subsystem(d.Path); ok && d.Content != "" {
			if err := os.WriteFile(filepath.Join(workspace, "kb", name+".md"), []byte(d.Content), 0600); err != nil {
				return err
			}
		}
	}
	data, err := kb.Encode(seed)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workspace, "seed", "entities.json"), data, 0600)
}

// latestDocuments returns the latest revision of each document, in ID order.
func latestDocuments(docs []trace.Document) []trace.Document {
	latest := map[string]trace.Document{}
	for _, d := range docs {
		latest[d.ID] = d
	}
	ids := make([]string, 0, len(latest))
	for id := range latest {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]trace.Document, 0, len(ids))
	for _, id := range ids {
		out = append(out, latest[id])
	}
	return out
}

// seedTracked seeds the entity map from the clone's tracked files alone, the
// same input the librarian's staged copy holds.
func seedTracked(ctx context.Context, clone string) (kb.Map, error) {
	dir, err := os.MkdirTemp("", "osmia-seed-")
	if err != nil {
		return kb.Map{}, err
	}
	defer os.RemoveAll(dir)
	if err := copyTracked(ctx, clone, dir); err != nil {
		return kb.Map{}, err
	}
	return kb.Seed(dir)
}

// copyTracked copies the clone's tracked regular files into dst. Git only
// lists the index; the clone is never written.
func copyTracked(ctx context.Context, clone, dst string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", clone, "ls-files", "-z")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("list tracked files of the clone: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	src, err := os.OpenRoot(clone)
	if err != nil {
		return err
	}
	defer src.Close()
	target, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer target.Close()
	for _, entry := range bytes.Split(out, []byte{0}) {
		name := string(entry)
		if name == "" || !fs.ValidPath(name) {
			continue
		}
		info, err := src.Lstat(name)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := src.ReadFile(name)
		if err != nil {
			return err
		}
		if err := target.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		if err := target.WriteFile(name, data, 0600|(info.Mode().Perm()&0100)); err != nil {
			return err
		}
	}
	return nil
}

// capture copies the view's output directory to the turn's directory before
// the view is released, whatever the turn's result.
func (e *extractor) capture(_ context.Context, scope coreadapter.Scope, view *isolation.FileView, _ coreadapter.SessionResult) error {
	directory := e.turnDirectory(scope.Turn)
	final := filepath.Join(directory, kb.OutputDirectory)
	tmp := final + ".tmp"
	if err := errors.Join(os.RemoveAll(tmp), os.RemoveAll(final)); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0700); err != nil {
		return err
	}
	src, err := os.OpenRoot(view.Workspace().Directory)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenRoot(tmp)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := src.Lstat(kb.OutputDirectory); err == nil {
		err = fs.WalkDir(src.FS(), kb.OutputDirectory, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel := strings.TrimPrefix(name, kb.OutputDirectory)
			rel = strings.TrimPrefix(rel, "/")
			info, err := src.Lstat(name)
			if err != nil {
				return err
			}
			switch {
			case info.IsDir():
				if rel == "" {
					return nil
				}
				return dst.Mkdir(rel, 0700)
			case info.Mode().IsRegular():
				data, err := src.ReadFile(name)
				if err != nil {
					return err
				}
				return dst.WriteFile(rel, data, 0600)
			}
			return fmt.Errorf("output/%s is not a regular file", rel)
		})
		if err != nil {
			return err
		}
	}
	return os.Rename(tmp, final)
}

// record validates the completed turn's output and records the complete
// knowledge base as one transaction of librarian-authored revisions: every
// produced subsystem, the removal of every subsystem no longer produced, and
// the refined entity map.
func (e *extractor) record(ctx context.Context, operation string, last trace.QueuedTurn) (coreadapter.OperationResult, error) {
	out, err := kb.ReadOutput(filepath.Join(e.turnDirectory(last.Request.TurnID), kb.OutputDirectory))
	if err != nil {
		return failedExtraction("invalid librarian output: " + err.Error()), nil
	}
	entities, err := kb.Encode(out.Entities)
	if err != nil {
		return failedExtraction("invalid librarian output: " + err.Error()), nil
	}
	docs, err := trace.Read[trace.Document](e.repository, "")
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	latest := map[string]trace.Document{}
	for _, d := range docs {
		latest[d.ID] = d
	}
	at := e.s.now()
	header := func(id string) trace.Header {
		return trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: latest[id].Revision + 1, Project: e.repository.Project(), At: at, Actor: librarianActor, Cause: operation, Depth: 1}
	}
	var records []trace.Document
	for _, name := range out.Subsystems() {
		records = append(records, trace.Document{Header: header(kb.ProseDocument(name)), Path: trace.ProsePath(name), Content: out.Prose[name]})
	}
	for _, d := range latestDocuments(docs) {
		if name, ok := kb.Subsystem(d.Path); ok && d.Content != "" && out.Prose[name] == "" {
			records = append(records, trace.Document{Header: header(d.ID), Path: d.Path})
		}
	}
	records = append(records, trace.Document{Header: header(trace.EntitiesDocument), Path: trace.EntitiesPath, Content: string(entities)})
	if err := e.repository.RecordDocuments(ctx, records); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return extractionResult(records), nil
}

func librarianSystemPrompt(p config.Project) string {
	return fmt.Sprintf("You are the librarian of the %s project (%s). You maintain its knowledge base: what a new contributor learns that the repository's own documents do not say. You work in a private copy of the repository with no version control; only what you write under output/kb is kept.", p.Name, p.Upstream)
}

func librarianPrompt(p config.Project) string {
	return fmt.Sprintf(`Extract the knowledge base of %s from the repository in your view.

Your view holds:
- repo/: the tracked files of %s from the owner's clone. It is a private copy; changes to it are discarded.
- kb/: the current knowledge base. kb/entities.json is the current entity map, and each kb/<subsystem>.md is the prose of the previous pass, if there was one.
- seed/entities.json: the entity map derived from CODEOWNERS and the repository's directory structure.

Write the complete knowledge base under output/kb/. Only files there are recorded, and a subsystem you do not write is removed from the knowledge base.

1. output/kb/<subsystem>.md, one file per subsystem: how the subsystem is built, how to run the tests that matter, what breaks when you touch what, and the decisions behind it. Keep each file short. A subsystem name is lowercase letters, digits and hyphens matching ^[a-z0-9][a-z0-9-]*$, at most %d characters. Write the files directly in output/kb/, never in a subdirectory.
2. output/kb/entities.json, the refined entity map. Start from seed/entities.json, keep every id that kb/entities.json has, add the aliases people use for each part of the code, correct owners and path patterns, and add part_of edges. The schema is {"version": 1, "entities": [{"id": ..., "name": ..., "aliases": [...], "paths": [...], "owners": [...], "part_of": [...]}]}: an id is lowercase letters, digits, dots and hyphens; paths are repository-relative glob patterns with the primary path first; part_of lists ids.

The repository's CLAUDE.md, AGENTS.md and CONTRIBUTING.md are inputs, not content: read them and do not repeat what they say. The knowledge base says what they do not.`, p.Name, p.Upstream, kb.MaxSubsystemLength)
}
