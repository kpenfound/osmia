package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
)

// RefreshAction folds a landed unit's reported learnings into the local KB.
const RefreshAction = "kb-refresh"

type refreshInput struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Unit       string              `json:"unit"`
	Landing    int                 `json:"landing"`
	Commit     string              `json:"commit"`
	Report     int                 `json:"report"`
}

// KnowledgeSource attributes one accepted subsystem revision to its unit and landing.
type KnowledgeSource struct {
	Subsystem  string              `json:"subsystem"`
	Revision   int                 `json:"revision"`
	Workstream config.WorkstreamID `json:"workstream"`
	Unit       string              `json:"unit"`
	Landing    int                 `json:"landing"`
	Commit     string              `json:"commit"`
	Report     int                 `json:"report"`
	Operation  string              `json:"operation"`
}

type refresher struct{ *extractor }

func refreshKey(commit string) string { return "refresh-" + commit[:16] }

func refreshPending(repo *trace.Repository) (bool, error) {
	ops, err := repo.Operations(librarianWorkstream(repo.Project()))
	if err != nil {
		return false, err
	}
	for _, op := range ops {
		if op.Operation.Action == RefreshAction && op.Result == nil {
			return true, nil
		}
	}
	return false, nil
}

// Pass asks for one refresh at a time, after a recorded landing. The source
// document is immutable and the request ID is derived from its landed commit.
func (r *refresher) Pass(ctx context.Context) error {
	stream := librarianWorkstream(r.repository.Project())
	ops, err := r.repository.Operations(stream)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, op := range ops {
		if op.Operation.Action == RefreshAction {
			var in refreshInput
			if err := json.Unmarshal(op.Operation.Input, &in); err != nil {
				return err
			}
			if len(in.Commit) < 16 {
				return errors.New("recorded refresh has no landing commit")
			}
			known[in.Commit] = true
			if op.Result == nil {
				return nil
			}
		}
		if op.Operation.Action == ExtractAction && op.Result == nil {
			return nil
		}
	}
	streams, err := r.repository.Workstreams()
	if err != nil {
		return err
	}
	for _, workstream := range streams {
		if workstream == stream {
			continue
		}
		docs, err := trace.Read[trace.Document](r.repository, workstream)
		if err != nil {
			return err
		}
		for _, doc := range docs {
			if !strings.HasSuffix(doc.Path, "/landing.json") {
				continue
			}
			var landing UnitLanding
			if err := json.Unmarshal([]byte(doc.Content), &landing); err != nil {
				return err
			}
			if len(landing.Commit) < 16 {
				return fmt.Errorf("landing %s has no commit", doc.Path)
			}
			if known[landing.Commit] {
				continue
			}
			var report trace.Document
			for _, candidate := range docs {
				if candidate.ID != reportDocument(landing.Unit) {
					continue
				}
				var value UnitReport
				if json.Unmarshal([]byte(candidate.Content), &value) == nil && value.Candidate == landing.Candidate {
					report = candidate
				}
			}
			if report.Revision == 0 {
				return fmt.Errorf("landing %s has no matching mason report", doc.Path)
			}
			in := refreshInput{Workstream: workstream, Unit: landing.Unit, Landing: doc.Revision, Commit: landing.Commit, Report: report.Revision}
			data, err := json.Marshal(in)
			if err != nil {
				return err
			}
			id := refreshKey(in.Commit)
			event := trace.EventID(id, "run")
			op := coreadapter.Operation{ID: trace.OperationID(r.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: RefreshAction, Input: data}
			at := r.s.now()
			tx := trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: r.repository.Project(), Workstream: stream, At: at, Actor: foremanActor, Cause: landing.Operation}, Subject: id, From: "", To: "requested", Reason: fmt.Sprintf("refresh knowledge from unit %s landed as %s", in.Unit, in.Commit)}, Events: []trace.Event{{ID: event, Kind: RefreshAction, Body: "Refresh local knowledge base", Operation: &op}}}
			_, err = r.repository.Transact(ctx, tx)
			return err
		}
	}
	return nil
}

func (r *refresher) source(in refreshInput) (UnitLanding, UnitReport, error) {
	docs, err := trace.Read[trace.Document](r.repository, in.Workstream)
	if err != nil {
		return UnitLanding{}, UnitReport{}, err
	}
	var landing UnitLanding
	var report UnitReport
	lf, rf := false, false
	for _, d := range docs {
		if d.ID == landingDocument(in.Unit) && d.Revision == in.Landing {
			if err := json.Unmarshal([]byte(d.Content), &landing); err != nil {
				return landing, report, err
			}
			lf = true
		}
		if d.ID == reportDocument(in.Unit) && d.Revision == in.Report {
			if err := json.Unmarshal([]byte(d.Content), &report); err != nil {
				return landing, report, err
			}
			rf = true
		}
	}
	if !lf || !rf || landing.Commit != in.Commit || landing.Unit != in.Unit || report.Unit != in.Unit || report.Candidate != landing.Candidate {
		return landing, report, errors.New("refresh source does not match its unit and landing")
	}
	state, err := r.repository.Workflow(in.Workstream, trace.UnitSubject(in.Unit))
	if err != nil {
		return landing, report, err
	}
	if state.Value != UnitMerged {
		return landing, report, errors.New("refresh source unit has not merged")
	}
	return landing, report, nil
}

func (r *refresher) decode(op coreadapter.Operation) (refreshInput, error) {
	var in refreshInput
	if op.Action != RefreshAction || op.Boundary != coreadapter.RunnerBoundary {
		return in, errors.New("invalid refresh operation")
	}
	if err := json.Unmarshal(op.Input, &in); err != nil {
		return in, err
	}
	if in.Workstream == "" || in.Unit == "" || in.Landing < 1 || in.Report < 1 || len(in.Commit) < 16 {
		return in, errors.New("incomplete refresh operation")
	}
	return in, nil
}

func (r *refresher) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := r.decode(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if _, _, err := r.source(in); err != nil {
		return coreadapter.Observation{}, err
	}
	docs, err := r.recorded(op.ID)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if len(docs) > 0 {
		result := extractionResult(docs)
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "knowledge base recorded", Result: &result}, nil
	}
	t, err := r.repository.Thread(librarianWorkstream(r.repository.Project()), librarianAgent)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	for _, turn := range t.Turns {
		if strings.HasPrefix(turn.Request.TurnID, refreshKey(in.Commit)+"-") && turn.Claim != nil && turn.Response == nil && t.Status != "interrupted" {
			return coreadapter.Observation{State: coreadapter.EffectUnknown, Evidence: "librarian turn is running"}, nil
		}
	}
	return coreadapter.Observation{State: coreadapter.EffectAbsent, Evidence: "refresh awaits librarian output"}, nil
}

func (r *refresher) Apply(ctx context.Context, op coreadapter.Operation) (coreadapter.OperationResult, error) {
	in, err := r.decode(op)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	landing, report, err := r.source(in)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	docs, err := r.recorded(op.ID)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if len(docs) > 0 {
		return extractionResult(docs), nil
	}
	stream := librarianWorkstream(r.repository.Project())
	for {
		if err := ctx.Err(); err != nil {
			return coreadapter.OperationResult{}, err
		}
		t, err := r.repository.Thread(stream, librarianAgent)
		if err != nil {
			return coreadapter.OperationResult{}, err
		}
		var turns []trace.QueuedTurn
		for _, turn := range t.Turns {
			if strings.HasPrefix(turn.Request.TurnID, refreshKey(in.Commit)+"-") {
				turns = append(turns, turn)
			}
		}
		var last *trace.QueuedTurn
		if len(turns) > 0 {
			last = &turns[len(turns)-1]
		}
		switch {
		case last != nil && last.Claim != nil && last.Response == nil:
			if t.Status != "interrupted" {
				return coreadapter.OperationResult{}, errors.New("librarian turn is still running")
			}
			if err := r.repository.AbandonTurn(ctx, stream, librarianAgent, last.Request.TurnID, r.s.now()); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case last == nil || last.Status() == "interrupted":
			if len(turns) >= maxExtractionAttempts {
				return failedExtraction("librarian refresh was interrupted three times"), nil
			}
			if r.s.options.Librarian == nil {
				return failedExtraction("this service has no librarian runner"), nil
			}
			cfg := r.s.current()
			profile, _, err := r.s.roleExecution(cfg, librarianRole)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			turn := fmt.Sprintf("%s-%d", refreshKey(in.Commit), len(turns)+1)
			prompt := fmt.Sprintf("Fold the reported learnings of unit %s into the current knowledge base. Read source/landing.json and source/report.json for exact provenance and learnings. Read repo/, kb/ and seed/ as needed. Write the complete resulting KB under output/kb/ with entities.json and one nonempty <subsystem>.md per subsystem. Preserve useful current content and stable entity IDs. Only output/kb/ is accepted. Do not repeat AGENTS.md, CLAUDE.md or CONTRIBUTING.md. Source landing: %s; commit: %s.", in.Unit, landing.Operation, in.Commit)
			req := trace.TurnRequest{Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turn, Revision: 1, Project: r.repository.Project(), Workstream: stream, At: r.s.now(), Actor: librarianActor, Cause: op.ID, Depth: 1}, AgentID: librarianAgent, ThreadID: librarianThread, TurnID: turn, Profile: profile, SystemPrompt: librarianSystemPrompt(cfg.Project), Prompt: prompt}
			if _, err := r.repository.EnqueueTurn(ctx, req); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case last.CompletedAt.IsZero():
			if _, err := r.dispatch(ctx, stream, last.Request.TurnID); err != nil {
				return coreadapter.OperationResult{}, err
			}
			if err := os.RemoveAll(filepath.Join(r.turnDirectory(last.Request.TurnID), "workspace")); err != nil {
				return coreadapter.OperationResult{}, err
			}
		case last.Status() == "idle":
			return r.recordRefresh(ctx, op.ID, in, report, *last)
		default:
			reason := last.Response.Failure
			if reason == "" {
				reason = "the turn ended with status " + last.Status()
			}
			return failedExtraction("librarian refresh failed: " + reason), nil
		}
	}
}

// stageRefreshSource supplies immutable local records to a refresh turn.
func stageRefreshSource(ctx context.Context, repo *trace.Repository, clone, turn, workspace string) error {
	ops, err := repo.Operations(librarianWorkstream(repo.Project()))
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.Operation.Action != RefreshAction {
			continue
		}
		var in refreshInput
		if err := json.Unmarshal(op.Operation.Input, &in); err != nil {
			return err
		}
		if !strings.HasPrefix(turn, refreshKey(in.Commit)+"-") {
			continue
		}
		r := &refresher{extractor: &extractor{repository: repo}}
		landing, report, err := r.source(in)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(workspace, "repo")); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(workspace, "repo"), 0700); err != nil {
			return err
		}
		if err := copyCommit(ctx, clone, in.Commit, filepath.Join(workspace, "repo")); err != nil {
			return err
		}
		seed, err := kb.Seed(filepath.Join(workspace, "repo"))
		if err != nil {
			return err
		}
		data, err := kb.Encode(seed)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(workspace, "seed", "entities.json"), data, 0600); err != nil {
			return err
		}
		dir := filepath.Join(workspace, "source")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		for name, value := range map[string]any{"landing.json": landing, "report.json": report} {
			data, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, name), append(data, '\n'), 0600); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("refresh turn has no source operation")
}

// copyCommit stages regular tracked files from the exact landed commit. Git
// object IDs, rather than the mutable feature worktree, fix the turn's source.
func copyCommit(ctx context.Context, clone, commit, dst string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", clone, "ls-tree", "-r", "-z", "--full-tree", commit)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("list landing commit %s: %w", commit, err)
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, entry := range bytes.Split(out, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		meta, name, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok || !fs.ValidPath(string(name)) {
			return errors.New("invalid landing tree entry")
		}
		fields := bytes.Fields(meta)
		if len(fields) != 3 {
			return errors.New("invalid landing tree metadata")
		}
		if string(fields[1]) != "blob" || string(fields[0]) == "120000" {
			continue
		}
		cmd := exec.CommandContext(ctx, "git", "-C", clone, "cat-file", "blob", string(fields[2]))
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
		data, err := cmd.Output()
		if err != nil {
			return err
		}
		if err := root.MkdirAll(path.Dir(string(name)), 0700); err != nil {
			return err
		}
		if err := root.WriteFile(string(name), data, 0600); err != nil {
			return err
		}
	}
	return nil
}

func (r *refresher) recordRefresh(ctx context.Context, operation string, in refreshInput, _ UnitReport, last trace.QueuedTurn) (coreadapter.OperationResult, error) {
	out, err := kb.ReadOutput(filepath.Join(r.turnDirectory(last.Request.TurnID), kb.OutputDirectory))
	if err != nil {
		return failedExtraction("invalid librarian output: " + err.Error()), nil
	}
	entities, err := kb.Encode(out.Entities)
	if err != nil {
		return failedExtraction("invalid librarian output: " + err.Error()), nil
	}
	docs, err := trace.Read[trace.Document](r.repository, "")
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	latest := map[string]trace.Document{}
	for _, doc := range docs {
		latest[doc.ID] = doc
	}
	var sources []KnowledgeSource
	if old := latest["kb-sources"]; old.Revision > 0 {
		if err := json.Unmarshal([]byte(old.Content), &sources); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	at := r.s.now()
	header := func(id string) trace.Header {
		return trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: id, Revision: latest[id].Revision + 1, Project: r.repository.Project(), At: at, Actor: librarianActor, Cause: operation, Depth: 1}
	}
	var records []trace.Document
	for _, name := range out.Subsystems() {
		id := kb.ProseDocument(name)
		if latest[id].Content == out.Prose[name] {
			continue
		}
		sources = append(sources, KnowledgeSource{Subsystem: name, Revision: latest[id].Revision + 1, Workstream: in.Workstream, Unit: in.Unit, Landing: in.Landing, Commit: in.Commit, Report: in.Report, Operation: operation})
		records = append(records, trace.Document{Header: header(id), Path: trace.ProsePath(name), Content: out.Prose[name]})
	}
	for _, d := range latestDocuments(docs) {
		if name, ok := kb.Subsystem(d.Path); ok && d.Content != "" && out.Prose[name] == "" {
			records = append(records, trace.Document{Header: header(d.ID), Path: d.Path})
		}
	}
	if latest[trace.EntitiesDocument].Content != string(entities) {
		records = append(records, trace.Document{Header: header(trace.EntitiesDocument), Path: trace.EntitiesPath, Content: string(entities)})
	}
	// The source ledger is also the durable marker for a no-op refresh.
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Operation == sources[j].Operation {
			return sources[i].Subsystem < sources[j].Subsystem
		}
		return sources[i].Operation < sources[j].Operation
	})
	data, err := json.MarshalIndent(sources, "", "  ")
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	records = append(records, trace.Document{Header: header("kb-sources"), Path: "kb/sources.json", Content: string(data) + "\n"})
	if err := r.repository.RecordDocuments(ctx, records); err != nil {
		return coreadapter.OperationResult{}, err
	}
	return extractionResult(records), nil
}
