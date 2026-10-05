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
	"slices"
	"sort"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
)

// RefreshAction updates local knowledge from landings and code-based answers.
const RefreshAction = "kb-refresh"

// refreshInput names a refresh: of a code-based answer, by its question and
// inspection, or of a workstream's landings. Commit is the commit the
// librarian reads the repository at: the inspected commit, or the latest of
// the landings. A refresh lists its landings in Landings, or, as one recorded
// before landings were batched, names its one landing by Unit, Landing and
// Report.
type refreshInput struct {
	Question   string              `json:"question,omitempty"`
	Inspection string              `json:"inspection,omitempty"`
	Workstream config.WorkstreamID `json:"workstream"`
	Unit       string              `json:"unit"`
	Landing    int                 `json:"landing"`
	Commit     string              `json:"commit"`
	Report     int                 `json:"report"`
	Landings   []refreshLanding    `json:"landings,omitempty"`
}

// refreshLanding is one unit landing a refresh folds in: the unit, the
// revisions of its landing.json and of the report whose learnings it folds,
// and the commit it landed as.
type refreshLanding struct {
	Unit    string `json:"unit"`
	Landing int    `json:"landing"`
	Commit  string `json:"commit"`
	Report  int    `json:"report"`
}

// landings returns the landings the refresh folds in, oldest first.
func (in refreshInput) landings() []refreshLanding {
	if len(in.Landings) != 0 || in.Unit == "" {
		return in.Landings
	}
	return []refreshLanding{{Unit: in.Unit, Landing: in.Landing, Commit: in.Commit, Report: in.Report}}
}

// KnowledgeSource attributes a subsystem revision to its immutable evidence.
type KnowledgeSource struct {
	Question   string              `json:"question,omitempty"`
	Inspection string              `json:"inspection,omitempty"`
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

func refreshResult(docs []trace.Document) coreadapter.OperationResult {
	result := extractionResult(docs)
	count := 0
	for _, d := range docs {
		if _, ok := kb.Subsystem(d.Path); ok {
			count++
		}
	}
	result.Evidence = fmt.Sprintf("recorded %d subsystem revisions and the source ledger", count)
	return result
}

func refreshPending(repo *trace.Repository) (bool, error) {
	streams, err := repo.Workstreams()
	if err != nil {
		return false, err
	}
	if !slices.Contains(streams, librarianWorkstream(repo.Project())) {
		return false, nil
	}
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

// Pass serializes refreshes from recorded landings and code-based answers.
// Each request derives its identity from the immutable source records. A
// workstream's landings are folded in together once it is quiet: no unit of
// it is started and unmerged, as once it is assembled, or it is delivered or
// abandoned. Until then they wait, so one refresh carries what several units
// learned.
func (r *refresher) Pass(ctx context.Context) error {
	stream := librarianWorkstream(r.repository.Project())
	streams, err := r.repository.Workstreams()
	if err != nil {
		return err
	}
	if !slices.Contains(streams, stream) {
		return nil
	}
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
			for _, l := range in.landings() {
				known[l.Commit] = true
			}
			known[in.key()] = true
			if op.Result == nil {
				return nil
			}
		}
		if op.Operation.Action == ExtractAction && op.Result == nil {
			return nil
		}
	}
	for _, workstream := range streams {
		if workstream == stream {
			continue
		}
		landings, err := r.unfolded(workstream, known)
		if err != nil || len(landings) == 0 {
			if err != nil {
				return err
			}
			continue
		}
		if quiet, err := quietWorkstream(r.repository, workstream); err != nil || !quiet {
			if err != nil {
				return err
			}
			continue
		}
		in := refreshInput{Workstream: workstream, Commit: landings[len(landings)-1].Commit, Landings: landings}
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		var units []string
		for _, l := range landings {
			units = append(units, fmt.Sprintf("%s landed as %s", l.Unit, l.Commit))
		}
		id := in.key()
		event := trace.EventID(id, "run")
		op := coreadapter.Operation{ID: trace.OperationID(r.repository.Project(), stream, event), Boundary: coreadapter.RunnerBoundary, Action: RefreshAction, Input: data}
		at := r.s.now()
		tx := trace.Transaction{Transition: trace.Transition{Header: trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: r.repository.Project(), Workstream: stream, At: at, Actor: foremanActor, Cause: landingDocument(landings[len(landings)-1].Unit)}, Subject: id, From: "", To: "requested", Reason: fmt.Sprintf("refresh knowledge from workstream %s's units %s", workstream, strings.Join(units, ", "))}, Events: []trace.Event{{ID: event, Kind: RefreshAction, Body: "Refresh local knowledge base", Operation: &op}}}
		_, err = r.repository.Transact(ctx, tx)
		return err
	}
	return r.requestKnowledgeGap(ctx, streams, known)
}

// unfolded returns the workstream's landings no refresh has folded in, in
// the order they were recorded, each with the mason report of its candidate.
func (r *refresher) unfolded(workstream config.WorkstreamID, known map[string]bool) ([]refreshLanding, error) {
	docs, err := trace.Read[trace.Document](r.repository, workstream)
	if err != nil {
		return nil, err
	}
	var out []refreshLanding
	for _, doc := range docs {
		if !strings.HasSuffix(doc.Path, "/landing.json") {
			continue
		}
		var landing UnitLanding
		if err := json.Unmarshal([]byte(doc.Content), &landing); err != nil {
			return nil, err
		}
		if len(landing.Commit) < 16 {
			return nil, fmt.Errorf("landing %s has no commit", doc.Path)
		}
		if known[landing.Commit] {
			continue
		}
		report := 0
		for _, candidate := range docs {
			var value UnitReport
			if candidate.ID == reportDocument(landing.Unit) && json.Unmarshal([]byte(candidate.Content), &value) == nil && value.Candidate == landing.Candidate {
				report = candidate.Revision
			}
		}
		if report == 0 {
			return nil, fmt.Errorf("landing %s has no matching mason report", doc.Path)
		}
		out = append(out, refreshLanding{Unit: landing.Unit, Landing: doc.Revision, Commit: landing.Commit, Report: report})
	}
	return out, nil
}

// quietWorkstream reports whether no unit of the workstream is started and
// unmerged, or the workstream is delivered or abandoned.
func quietWorkstream(repository *trace.Repository, workstream config.WorkstreamID) (bool, error) {
	states, err := repository.WorkflowStates(workstream)
	if err != nil {
		return false, err
	}
	if feature := states[trace.FeatureSubject].Value; feature == DeliveredState || feature == AbandonedState {
		return true, nil
	}
	started := []string{UnitImplementing, UnitWaiting, UnitChecking, UnitReviewing, UnitApproved, UnitContested}
	for subject, state := range states {
		if strings.HasPrefix(subject, "unit-") && slices.Contains(started, state.Value) {
			return false, nil
		}
	}
	return true, nil
}

// sources returns the landings and mason reports of the refresh's
// landings, in its order, and fails unless each names its unit's merged
// landing and the report of the candidate that landed.
func (r *refresher) sources(in refreshInput) ([]UnitLanding, []UnitReport, error) {
	docs, err := trace.Read[trace.Document](r.repository, in.Workstream)
	if err != nil {
		return nil, nil, err
	}
	var landings []UnitLanding
	var reports []UnitReport
	for _, l := range in.landings() {
		var landing UnitLanding
		var report UnitReport
		lf, rf := false, false
		for _, d := range docs {
			if d.ID == landingDocument(l.Unit) && d.Revision == l.Landing {
				if err := json.Unmarshal([]byte(d.Content), &landing); err != nil {
					return nil, nil, err
				}
				lf = true
			}
			if d.ID == reportDocument(l.Unit) && d.Revision == l.Report {
				if err := json.Unmarshal([]byte(d.Content), &report); err != nil {
					return nil, nil, err
				}
				rf = true
			}
		}
		if !lf || !rf || landing.Commit != l.Commit || landing.Unit != l.Unit || report.Unit != l.Unit || report.Candidate != landing.Candidate {
			return nil, nil, fmt.Errorf("refresh source of unit %s does not match its unit and landing", l.Unit)
		}
		state, err := r.repository.Workflow(in.Workstream, trace.UnitSubject(l.Unit))
		if err != nil {
			return nil, nil, err
		}
		if state.Value != UnitMerged {
			return nil, nil, fmt.Errorf("refresh source unit %s has not merged", l.Unit)
		}
		landings, reports = append(landings, landing), append(reports, report)
	}
	return landings, reports, nil
}

func (r *refresher) decode(op coreadapter.Operation) (refreshInput, error) {
	var in refreshInput
	if op.Action != RefreshAction || op.Boundary != coreadapter.RunnerBoundary {
		return in, errors.New("invalid refresh operation")
	}
	if err := json.Unmarshal(op.Input, &in); err != nil {
		return in, err
	}
	if in.Workstream == "" || len(in.Commit) < 16 || (in.Inspection != "" && in.Question == "") {
		return in, errors.New("incomplete refresh operation")
	}
	if in.Inspection == "" {
		landings := in.landings()
		if len(landings) == 0 || landings[len(landings)-1].Commit != in.Commit {
			return in, errors.New("incomplete refresh operation")
		}
		for _, l := range landings {
			if l.Unit == "" || l.Landing < 1 || l.Report < 1 || len(l.Commit) < 16 {
				return in, errors.New("incomplete refresh operation")
			}
		}
	}
	return in, nil
}

func (r *refresher) Inspect(ctx context.Context, op coreadapter.Operation) (coreadapter.Observation, error) {
	in, err := r.decode(op)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if err := r.checkSource(in); err != nil {
		return coreadapter.Observation{}, err
	}
	docs, err := r.recorded(op.ID)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	if len(docs) > 0 {
		result := refreshResult(docs)
		return coreadapter.Observation{State: coreadapter.EffectCompleted, Evidence: "knowledge base recorded", Result: &result}, nil
	}
	t, err := r.repository.Thread(librarianWorkstream(r.repository.Project()), librarianAgent)
	if err != nil {
		return coreadapter.Observation{}, err
	}
	for _, turn := range t.Turns {
		if strings.HasPrefix(turn.Request.TurnID, in.key()+"-") && turn.Claim != nil && turn.Response == nil && t.Status != "interrupted" {
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
	var landings []UnitLanding
	if err := r.checkSource(in); err != nil {
		return coreadapter.OperationResult{}, err
	}
	if in.Inspection == "" {
		if landings, _, err = r.sources(in); err != nil {
			return coreadapter.OperationResult{}, err
		}
	}
	docs, err := r.recorded(op.ID)
	if err != nil {
		return coreadapter.OperationResult{}, err
	}
	if len(docs) > 0 {
		return refreshResult(docs), nil
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
			if strings.HasPrefix(turn.Request.TurnID, in.key()+"-") {
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
			cfg := r.s.about(r.repository)
			profile, _, err := r.s.roleExecution(cfg, librarianRole)
			if err != nil {
				return coreadapter.OperationResult{}, err
			}
			turn := fmt.Sprintf("%s-%d", in.key(), len(turns)+1)
			var units, operations []string
			for _, landing := range landings {
				units, operations = append(units, landing.Unit), append(operations, landing.Operation)
			}
			prompt := fmt.Sprintf("Fold the reported learnings of units %s into the current knowledge base. Read source/landings.json and source/reports.json for exact provenance and learnings, one entry per unit in the same order. Read repo/, kb/ and seed/ as needed; repo/ is at the latest of the landings. Write the complete resulting KB under output/kb/ with entities.json and one nonempty <subsystem>.md per subsystem. Preserve useful current content and stable entity IDs. Only output/kb/ is accepted. Do not repeat AGENTS.md, CLAUDE.md or CONTRIBUTING.md. Source landings: %s; commit: %s.", strings.Join(units, ", "), strings.Join(operations, ", "), in.Commit)
			if in.Inspection != "" {
				prompt = fmt.Sprintf("A chief-of-staff answer required inspecting code. Fill this knowledge gap using source/ruling.json and source/inspection.json, with repo/ fixed at commit %s. Read context.md, kb/ and seed/. Write the complete resulting KB under output/kb/. Preserve stable entity IDs and useful content; do not treat the chief's answer as an owner ruling. Only output/kb/ is accepted.", in.Commit)
			}
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
			return r.recordRefresh(ctx, op.ID, in, *last)
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
		if !strings.HasPrefix(turn, in.key()+"-") {
			continue
		}
		r := &refresher{extractor: &extractor{repository: repo}}
		sources := map[string]any{}
		if in.Inspection != "" {
			ruling, inspection, err := r.codeSource(in)
			if err != nil {
				return err
			}
			sources["ruling.json"], sources["inspection.json"] = ruling, inspection
		} else {
			landings, reports, err := r.sources(in)
			if err != nil {
				return err
			}
			sources["landings.json"], sources["reports.json"] = landings, reports
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
		for name, value := range sources {
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

func (r *refresher) recordRefresh(ctx context.Context, operation string, in refreshInput, last trace.QueuedTurn) (coreadapter.OperationResult, error) {
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
		if in.Inspection != "" {
			sources = append(sources, KnowledgeSource{Question: in.Question, Inspection: in.Inspection, Subsystem: name, Revision: latest[id].Revision + 1, Workstream: in.Workstream, Commit: in.Commit, Operation: operation})
		}
		for _, l := range in.landings() {
			sources = append(sources, KnowledgeSource{Subsystem: name, Revision: latest[id].Revision + 1, Workstream: in.Workstream, Unit: l.Unit, Landing: l.Landing, Commit: l.Commit, Report: l.Report, Operation: operation})
		}
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
	sort.SliceStable(sources, func(i, j int) bool {
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
	return refreshResult(records), nil
}
