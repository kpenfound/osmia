package intake

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/kb"
	"github.com/kpenfound/osmia/internal/trace"
)

// Design session tool and document names. draft_brief and read_repo are the
// only tools a design session's agent may ever reach; every other tool a
// chief-of-staff turn can hold, including every write, version-control,
// credential or owner-gate tool, is narrowed away for it (enforcement.go).
const (
	DraftBriefToolName      = "draft_brief"
	DesignReadRepoToolName  = "read_repo"
	DesignDraftDocument     = "design-draft"
	DesignDraftPath         = "tools/design-brief.json"
	DesignOpenTransition    = "design-open"
	DesignStartTransition   = "design-start"
	DesignBriefPath         = "handed/design-brief.md"
	DesignTranscriptHeading = "\n\n## Transcript\n" +
		"The decisions, intent and tradeoffs, and rejected options above are the brief. Where this transcript differs from them, the brief wins.\n\n"
)

// designSessionSubject is the workflow subject that marks a workstream as a
// design session: openDesign sets it to designOpenValue in the same commit
// that creates the workstream, and startDesignWorkgroup moves it to
// designClosedValue in the same commit that hands its brief off. Using a
// dedicated subject, rather than inferring "design session" from an empty
// kind and an empty feature state, means isDesignWorkstream can never
// misread an ordinary workstream that simply has not recorded a feature
// state yet through some other path (for instance, a fixture, or a future
// caller) as a design session: nothing but openDesign ever sets this
// subject to designOpenValue.
const DesignSessionSubject = "design-session"

const DesignOpenValue = "open"

const DesignClosedValue = "started"

type DesignBriefRecord struct {
	Content string `json:"content"`
}

// isDesignWorkstream reports whether stream is a design session: a
// workstream openDesign created and startDesignWorkgroup has not yet handed
// off. A design session carries no feature state until the owner starts a
// workgroup from its brief, which is exactly the window every factory pass
// (architect, shed, masons, reviewers, base refresh, drift, final review,
// publication, archive cleanup, workgroup resolution) already leaves alone
// for a workstream with none: none of them acts before a workstream reaches
// a feature state they recognise, so a design session needs no pass-by-pass
// exclusion of its own.
func IsDesignWorkstream(repository *trace.Repository, stream config.WorkstreamID) (bool, error) {
	state, err := repository.Workflow(stream, DesignSessionSubject)
	if err != nil {
		return false, err
	}
	return state.Value == DesignOpenValue, nil
}

func RenderDesignView(project config.ProjectID, w trace.WorkstreamStatus) DesignView {
	return DesignView{Workstream: w.Workstream, Project: project, CreatedAt: w.CreatedAt}
}

// openDesign opens a design session in a project: a new, ordinary workstream
// with a chief-of-staff thread and no feature state, wrapped in its own
// one-workstream workgroup like every other workstream. It takes no spec, no
// plan and no units, and the owner's conversation with it never reaches the
// tools or the context an ordinary chief of staff has.
func (s *Controller) OpenDesign(ctx context.Context, req DesignOpenRequest) (DesignView, *APIError) {
	project, api := s.ProjectFor(req.Project)
	if api != nil {
		return DesignView{}, api
	}
	repository, err := s.Repository(project)
	if err != nil {
		return DesignView{}, &APIError{Code: Internal, Message: fmt.Sprintf("project %s has no trace repository", project)}
	}
	streams, err := repository.Workstreams()
	if err != nil {
		return DesignView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the workstreams of project %s; check the trace repository", project)}
	}
	stream, err := config.NewWorkstreamID(streams...)
	if err != nil {
		return DesignView{}, &APIError{Code: Internal, Message: "cannot generate a design session identity"}
	}
	now := s.Now()
	if err := repository.CreateWorkstream(ctx, stream, now, OwnerActor); err != nil {
		return DesignView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot open design session %s; check the trace repository", stream)}
	}
	if _, err := repository.EnsureChiefOfStaff(ctx, stream, now, OwnerActor); err != nil {
		return DesignView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot prepare the thread of design session %s; check the trace repository", stream)}
	}
	if _, err := trace.EnsureWorkgroups(ctx, repository, []config.WorkstreamID{stream}, now, OwnerActor, DesignOpenTransition); err != nil {
		return DesignView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the workgroup of design session %s; check the trace repository", stream)}
	}
	openHeader := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: DesignOpenTransition, Revision: 1,
		Project: project, Workstream: stream, At: now, Actor: OwnerActor, Cause: DesignOpenTransition}
	if _, err := repository.Transact(ctx, trace.Transaction{Transition: trace.Transition{Header: openHeader, Subject: DesignSessionSubject, To: DesignOpenValue, Reason: "the owner opened a design session"}}); err != nil {
		return DesignView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the design session marker of %s; check the trace repository", stream)}
	}
	if err := s.RefreshRuntime(); err != nil {
		return DesignView{}, &APIError{Code: Internal, Message: "cannot resolve the runtime workstreams of the new design session"}
	}
	return s.DesignRead(string(stream))
}

// designList reports every design session of every active project, or only
// those of the named project when it is not empty, newest first.
func (s *Controller) DesignList(rawProject string) (DesignsResponse, *APIError) {
	var filter config.ProjectID
	if rawProject != "" {
		id, err := config.ParseProjectID(rawProject)
		if err != nil {
			return DesignsResponse{}, &APIError{Code: Validation, Message: "project must be a project ID: p_ followed by 32 lowercase hexadecimal digits"}
		}
		filter = id
	}
	cfg, projects := s.Runtimes()
	out := []DesignView{}
	for _, active := range projects {
		if !cfg.Active(active.ID) || (filter != "" && active.ID != filter) {
			continue
		}
		list, err := active.Repository.Statuses()
		if err != nil {
			return DesignsResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the workstream status of project %s; check the trace repository", active.ID)}
		}
		for _, w := range list {
			design, err := IsDesignWorkstream(active.Repository, w.Workstream)
			if err != nil {
				return DesignsResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the kind of workstream %s; check the trace repository", w.Workstream)}
			}
			if design {
				out = append(out, RenderDesignView(active.ID, w))
			}
		}
	}
	slices.SortFunc(out, func(a, b DesignView) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(string(a.Workstream), string(b.Workstream))
	})
	return DesignsResponse{Designs: out}, nil
}

// designRead reports one design session, by workstream ID, across every
// active project. A workstream that exists but is not a design session, or
// was one and has since started a workgroup, is refused with not_found, the
// same as an unknown one.
func (s *Controller) DesignRead(raw string) (DesignView, *APIError) {
	id, err := config.ParseWorkstreamID(raw)
	if err != nil {
		return DesignView{}, &APIError{Code: Validation, Message: "workstream must be a workstream ID: w_ followed by 32 lowercase hexadecimal digits"}
	}
	notFound := &APIError{Code: NotFound, Message: fmt.Sprintf("design session %s is not in an active project; list design sessions with GET %s/design", id, Prefix)}
	cfg, projects := s.Runtimes()
	for _, active := range projects {
		if !cfg.Active(active.ID) {
			continue
		}
		list, err := active.Repository.Statuses()
		if err != nil {
			return DesignView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the workstream status of project %s; check the trace repository", active.ID)}
		}
		for _, w := range list {
			if w.Workstream != id {
				continue
			}
			design, err := IsDesignWorkstream(active.Repository, id)
			if err != nil {
				return DesignView{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the kind of workstream %s; check the trace repository", id)}
			}
			if !design {
				return DesignView{}, notFound
			}
			return RenderDesignView(active.ID, w), nil
		}
	}
	return DesignView{}, notFound
}

// designTrace resolves raw to a design session of an active project and
// returns the project's open trace.
func (s *Controller) DesignTrace(raw string) (config.ProjectID, config.WorkstreamID, *trace.Repository, *APIError) {
	id, err := config.ParseWorkstreamID(raw)
	if err != nil {
		return "", "", nil, &APIError{Code: Validation, Message: "workstream must be a workstream ID: w_ followed by 32 lowercase hexadecimal digits"}
	}
	notFound := &APIError{Code: NotFound, Message: fmt.Sprintf("design session %s is not in an active project; list design sessions with GET %s/design", id, Prefix)}
	cfg, projects := s.Runtimes()
	for _, active := range projects {
		if !cfg.Active(active.ID) {
			continue
		}
		streams, err := active.Repository.Workstreams()
		if err != nil {
			return "", "", nil, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the workstreams of project %s; check the trace repository", active.ID)}
		}
		if !slices.Contains(streams, id) {
			continue
		}
		design, err := IsDesignWorkstream(active.Repository, id)
		if err != nil {
			return "", "", nil, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the kind of workstream %s; check the trace repository", id)}
		}
		if !design {
			return "", "", nil, notFound
		}
		return active.ID, id, active.Repository, nil
	}
	return "", "", nil, notFound
}

// designLockKey identifies one design session's busy lock: a repository and
// the one of its workstreams that is the session, so every design session
// of every project has its own lock.
type DesignLockKey struct {
	Repository *trace.Repository
	Stream     config.WorkstreamID
}

// designLocks is the Go guard against two concurrent calls to designMessage
// or startDesignWorkgroup against the same design session both finding it
// idle: at most one turn ever runs, and starting a workgroup never races a
// turn in flight.
var DesignLocks sync.Map

func DesignLock(repository *trace.Repository, stream config.WorkstreamID) *sync.Mutex {
	v, _ := DesignLocks.LoadOrStore(DesignLockKey{repository, stream}, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// designRolePrompt opens the system prompt of every design session turn. It
// states the agent's read-only, gate-free standing and the readiness pass it
// runs before handoff.
const DesignRolePrompt = "You are the design agent for a project the owner is scoping before any work starts. " +
	"You have the project's charter and the knowledge base of every repo in the project below, and read-only access to each " +
	"repo's committed source code through read_repo. You hold no write tool, no version control tool, no credential and no " +
	"owner gate: you cannot write a file, commit, push, open a pull request, rule on anything or start work yourself.\n\n" +
	"Run a readiness pass before handoff. Ask the owner only about choices that are expensive to undo (contracts, data " +
	"models, migrations, public APIs), genuine forks with no clear signal in the code or the charter, or points that are in " +
	"tension with the owner's stated intent. Give every such question a recommended answer. Infer everything else from the " +
	"code, the project's conventions and the owner's stated intent, and show it as a collapsed, overridable assumption the " +
	"owner can skim and override, for example inside a <details><summary>Assumptions</summary>...</details> block, rather " +
	"than asking about it.\n\n" +
	"Once the readiness pass is answered, call " + DraftBriefToolName + " with the brief: decisions, intent and tradeoffs, " +
	"and the options you explicitly rejected, as one piece of markdown. Call it again whenever the brief changes; only its " +
	"latest call is used. The owner starts the workgroup once satisfied; you never start it yourself, and nothing you say " +
	"or record causes any work to begin."

// designSystemPrompt renders stream's turn prompt: the role prompt, the
// project's own charter and, for every repo of the project, its knowledge
// base, using the service's already open repository handles.
func (s *Controller) DesignSystemPrompt(ctx context.Context, cfg *config.Config, repository *trace.Repository, now time.Time) (string, error) {
	var b strings.Builder
	b.WriteString(DesignRolePrompt)
	fmt.Fprintf(&b, "\n\n## Project\nRepos in this project: %s\n", DesignRepoList([]config.Project{cfg.Project}))
	charterDoc, err := repository.Charter(ctx)
	if err != nil {
		return "", fmt.Errorf("charter: %w", err)
	}
	b.WriteString("\n## Charter\n")
	b.WriteString(charterDoc.Content)
	text, err := DesignRepoKnowledge(repository)
	if err != nil {
		return "", fmt.Errorf("project %s knowledge base: %w", cfg.Project.ID, err)
	}
	fmt.Fprintf(&b, "\n## Repo %s (%s) knowledge base\n%s", cfg.Project.Name, cfg.Project.ID, text)
	return b.String(), nil
}

func DesignRepoList(members []config.Project) string {
	names := make([]string, len(members))
	for i, m := range members {
		names[i] = fmt.Sprintf("%s (%s)", m.Name, m.ID)
	}
	return strings.Join(names, ", ")
}

// designRepoKnowledge renders one repo's entity map and knowledge-base prose.
func DesignRepoKnowledge(repo *trace.Repository) (string, error) {
	m, _, err := kb.LoadRevision(repo)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if len(m.Entities) == 0 {
		b.WriteString("(no entity map yet)\n")
	} else {
		for _, e := range m.Entities {
			fmt.Fprintf(&b, "- %s (%s): %s\n", e.ID, e.Name, e.Primary())
		}
	}
	names, err := repo.Subsystems()
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		b.WriteString("(no knowledge-base prose yet)\n")
	}
	for _, name := range names {
		prose, err := repo.Prose(name)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "\n### %s\n%s\n", name, prose)
	}
	return b.String(), nil
}

// designMessage records text as the next owner message on a design
// session's thread and starts exactly one agent turn. It is refused,
// recording nothing, when the message is empty, the workstream is no longer
// a design session, or a turn is already running.
func (s *Controller) DesignMessage(ctx context.Context, raw string, req DesignMessageRequest) (ConversationEntry, *APIError) {
	project, stream, repository, api := s.DesignTrace(raw)
	if api != nil {
		return ConversationEntry{}, api
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return ConversationEntry{}, &APIError{Code: Validation, Message: "text must not be empty"}
	}
	cfg := s.Current().For(project)
	_, profile, err := s.ChiefOverride(cfg)
	if err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: "the chief-of-staff role has no usable profile; check osmia profiles"}
	}
	mu := DesignLock(repository, stream)
	if !mu.TryLock() {
		return ConversationEntry{}, &APIError{Code: Conflict, Message: "a turn is running for this design session; try again once it finishes"}
	}
	defer mu.Unlock()
	if _, _, _, api := s.DesignTrace(raw); api != nil {
		return ConversationEntry{}, api
	}
	documents, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: "cannot read the design session's documents"}
	}
	for _, document := range documents {
		if document.ID == HandedDocument {
			return ConversationEntry{}, &APIError{Code: Conflict, Message: "a workgroup start is pending; retry starting the workgroup before sending more messages"}
		}
	}
	at := s.Now()
	th, err := repository.EnsureChiefOfStaff(ctx, stream, at, OwnerActor)
	if err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the message for design session %s; check the trace repository", stream)}
	}
	if UnfinishedSessionTurn(th) {
		return ConversationEntry{}, &APIError{Code: Conflict, Message: "a turn is running for this design session; try again once it finishes"}
	}
	prompt, err := s.DesignSystemPrompt(ctx, cfg, repository, at)
	if err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot assemble the context of design session %s; check the charter and the knowledge base of every repo in the project", stream)}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: "cannot generate a message ID"}
	}
	id := hex.EncodeToString(nonce[:])
	q, err := repository.EnqueueTurn(ctx, trace.TurnRequest{
		Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + id, Revision: 1,
			Project: project, Workstream: stream, At: at, Actor: OwnerActor, Cause: "owner-message"},
		AgentID: trace.ChiefOfStaff, ThreadID: trace.ChiefOfStaff, TurnID: "message_" + id,
		Profile: profile, SystemPrompt: prompt, Prompt: text,
	})
	if err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the message for design session %s; check the trace repository", stream)}
	}
	return Conversation(q)[0], nil
}

// draftBriefTool records the latest brief a design session's agent has
// drafted: decisions, intent and tradeoffs, and rejected options, as one
// piece of markdown. A later call replaces the one startDesignWorkgroup
// reads; nothing before the latest call is used.
func DraftBriefTool(repository *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	return coreadapter.Tool{Name: DraftBriefToolName, Effect: coreadapter.ToolMemory,
		Description: "Record the brief for this design session: decisions, intent and tradeoffs, and the options you explicitly rejected, as one piece of markdown text. Call it again whenever the brief changes; only the latest call is used when the owner starts a workgroup from it.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"content":{"type":"string"}},"required":["content"],"additionalProperties":false}`),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			if err := repository.ActiveTurn(scope); err != nil {
				return nil, err
			}
			var in struct {
				Content string `json:"content"`
			}
			d := json.NewDecoder(bytes.NewReader(raw))
			d.DisallowUnknownFields()
			if err := d.Decode(&in); err != nil {
				return nil, fmt.Errorf("tool input: %w", err)
			}
			content := strings.TrimSpace(in.Content)
			if content == "" {
				return RefuseReport("content is required")
			}
			lower := strings.ToLower(content)
			var missing []string
			for _, want := range []string{"decision", "intent", "tradeoff", "reject"} {
				if !strings.Contains(lower, want) {
					missing = append(missing, want)
				}
			}
			if len(missing) > 0 {
				return RefuseReport("the brief must cover decisions, intent and tradeoffs, and rejected options; missing: %s", strings.Join(missing, ", "))
			}
			stream := config.WorkstreamID(scope.Workstream)
			docs, err := trace.Read[trace.Document](repository, stream)
			if err != nil {
				return nil, err
			}
			k := 1
			for _, d := range docs {
				if d.ID == DesignDraftDocument && d.Revision >= k {
					k = d.Revision + 1
				}
			}
			at := now()
			h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: DesignDraftDocument, Revision: k,
				Project: repository.Project(), Workstream: stream, At: at, Actor: ChiefActor, Cause: DraftBriefToolName}
			data, err := json.Marshal(DesignBriefRecord{Content: content})
			if err != nil {
				return nil, err
			}
			doc := trace.Document{Header: h, Path: DesignDraftPath, Content: string(data)}
			if err := repository.RecordDocuments(ctx, []trace.Document{doc}); err != nil {
				return nil, err
			}
			return json.Marshal(struct {
				Recorded bool   `json:"recorded"`
				Revision int    `json:"revision"`
				Next     string `json:"next"`
			}{true, k, "Recorded. The owner can now start a workgroup from this brief, or you can call " + DraftBriefToolName + " again to revise it."})
		}}
}

// designReadRepoTool reads a file, or lists a directory, from the committed
// head of one repo of the design session's project: the session's own repo
// by default, or another repo of the project named by its project ID. It
// reads only; it never writes, fetches credentials or touches version
// control beyond a local, read-only export of the head commit.
func DesignReadRepoTool(cfg *config.Config, r *trace.Repository, controls *Controls, scope coreadapter.Scope) coreadapter.Tool {
	return coreadapter.Tool{Name: DesignReadRepoToolName, Effect: coreadapter.ToolRead,
		Description: "Read a file, or list a directory, from the head of one repo of this project, read-only. repo defaults to this project's own repo; name another repo of the project by its project ID to read it instead. path defaults to the root; start is a one-based line, lines defaults to 200 (maximum 1000).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"repo":{"type":"string"},"path":{"type":"string"},"start":{"type":"integer"},"lines":{"type":"integer"}},"additionalProperties":false}`),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			if err := r.ActiveTurn(scope); err != nil {
				return nil, err
			}
			var in struct {
				Repo  string `json:"repo"`
				Path  string `json:"path"`
				Start int    `json:"start"`
				Lines int    `json:"lines"`
			}
			d := json.NewDecoder(bytes.NewReader(raw))
			d.DisallowUnknownFields()
			if err := d.Decode(&in); err != nil {
				return nil, fmt.Errorf("tool input: %w", err)
			}
			if in.Path == "" {
				in.Path = "."
			}
			if in.Start == 0 {
				in.Start = 1
			}
			if in.Lines == 0 {
				in.Lines = 200
			}
			if !fs.ValidPath(in.Path) || in.Start < 1 || in.Lines < 1 || in.Lines > 1000 {
				return nil, errors.New("invalid code range")
			}
			// Project registration replaces the service configuration without
			// replacing existing runners. Resolve membership and clones per read.
			current := cfg
			if service := controls.Load(); service != nil {
				current = service.About(r)
			}
			target := current.Project
			if in.Repo != "" && config.ProjectID(in.Repo) != current.Project.ID {
				return nil, fmt.Errorf("repo %q is not this project's repository", in.Repo)
			}
			cmd := exec.CommandContext(ctx, "git", "-C", target.Clone, "rev-parse", "--verify", "HEAD^{commit}")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
			out, err := cmd.Output()
			if err != nil {
				return nil, err
			}
			commit := strings.TrimSpace(string(out))
			dir, err := os.MkdirTemp("", "osmia-design-read-")
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(dir)
			if err := CopyCommit(ctx, target.Clone, commit, dir); err != nil {
				return nil, err
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				return nil, err
			}
			defer root.Close()
			stat, err := root.Stat(in.Path)
			if err != nil {
				return nil, err
			}
			if stat.IsDir() {
				entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(in.Path)))
				if err != nil {
					return nil, err
				}
				names := []string{}
				for _, e := range entries {
					names = append(names, e.Name())
				}
				slices.Sort(names)
				start := min(len(names), in.Start-1)
				end := min(len(names), start+in.Lines)
				return json.Marshal(map[string]any{"repo": target.ID, "commit": commit, "path": in.Path, "files": names[start:end], "total": len(names)})
			}
			data, err := root.ReadFile(in.Path)
			if err != nil {
				return nil, err
			}
			lines := strings.Split(string(data), "\n")
			start := min(len(lines), in.Start-1)
			end := min(len(lines), start+in.Lines)
			content := strings.Join(lines[start:end], "\n")
			if len(content) > 65536 {
				return nil, errors.New("code excerpt exceeds 65536 bytes; request fewer lines")
			}
			return json.Marshal(map[string]any{"repo": target.ID, "commit": commit, "path": in.Path, "start": in.Start, "content": content})
		}}
}

// stageDesignWorkspace prepares a design session turn's workspace: it has no
// files of its own, since the charter and every repo's knowledge base are
// already in the turn's system prompt and its source code is read through
// read_repo.
func StageDesignWorkspace(workspace string) ([]string, error) {
	if err := os.RemoveAll(workspace); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workspace, 0700); err != nil {
		return nil, err
	}
	readme := "This design session has no files of its own. The project's charter and every repo's knowledge base are in " +
		"your system prompt; read source code read-only with " + DesignReadRepoToolName + ".\n"
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte(readme), 0600); err != nil {
		return nil, err
	}
	return []string{"README.md"}, nil
}

// designTranscript renders a design session's conversation, oldest first,
// for attaching to its brief as a reference the brief itself always wins
// over.
func DesignTranscript(repository *trace.Repository, stream config.WorkstreamID) (string, error) {
	th, err := repository.ChiefOfStaffThread(stream)
	if errors.Is(err, os.ErrNotExist) {
		return "(no conversation recorded)\n", nil
	}
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, q := range th.Turns {
		if q.Request.Actor == OwnerActor {
			fmt.Fprintf(&b, "Owner: %s\n\n", q.Request.Prompt)
		}
		if q.Response != nil && q.Response.Result.FinalResponse != "" {
			fmt.Fprintf(&b, "Agent: %s\n\n", q.Response.Result.FinalResponse)
		}
	}
	if b.Len() == 0 {
		return "(no conversation recorded)\n", nil
	}
	return b.String(), nil
}

// startDesignWorkgroup hands a design session's latest drafted brief to the
// architect flow: it records the brief, with the conversation transcript
// attached for reference, as the workstream's handed input, exactly as an
// ordinary hand-in does, so the same chief-of-staff thread that held the
// design conversation continues as the workgroup's. It is refused, recording
// nothing, when the workstream is no longer a design session, a turn is
// running, or the agent has not drafted a brief yet.
func (s *Controller) StartDesignWorkgroup(ctx context.Context, raw string) (DesignStartResponse, *APIError) {
	project, stream, repository, api := s.DesignTrace(raw)
	if api != nil {
		return DesignStartResponse{}, api
	}
	mu := DesignLock(repository, stream)
	if !mu.TryLock() {
		return DesignStartResponse{}, &APIError{Code: Conflict, Message: "a turn is running for this design session; try again once it finishes"}
	}
	defer mu.Unlock()
	if _, _, _, api := s.DesignTrace(raw); api != nil {
		return DesignStartResponse{}, api
	}
	th, err := repository.ChiefOfStaffThread(stream)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the thread of design session %s; check the trace repository", stream)}
	}
	if UnfinishedSessionTurn(th) {
		return DesignStartResponse{}, &APIError{Code: Conflict, Message: "a turn is running for this design session; try again once it finishes"}
	}
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the documents of design session %s; check the trace repository", stream)}
	}
	var draft trace.Document
	for _, d := range docs {
		if d.ID == DesignDraftDocument && d.Revision >= draft.Revision {
			draft = d
		}
	}
	if draft.Revision == 0 {
		return DesignStartResponse{}, &APIError{Code: Validation, Message: "the design agent has not drafted a brief yet; keep talking to it until it records one"}
	}
	transcript, err := DesignTranscript(repository, stream)
	if err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the conversation of design session %s; check the trace repository", stream)}
	}
	var brief DesignBriefRecord
	if err := json.Unmarshal([]byte(draft.Content), &brief); err != nil || strings.TrimSpace(brief.Content) == "" {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: "cannot read the drafted brief; check the trace repository"}
	}
	content := strings.TrimRight(brief.Content, "\n") + "\n" + DesignTranscriptHeading + transcript
	now := s.Now()
	source := "design:" + string(stream)
	handed := trace.Document{Header: trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: HandedDocument, Revision: 1,
		Project: project, Workstream: stream, At: now, Actor: OwnerActor, Cause: DesignStartTransition},
		Path: DesignBriefPath, Content: content, Source: source}
	alreadyHanded := false
	for _, document := range docs {
		if document.ID == HandedDocument {
			if document.Source != source || document.Path != DesignBriefPath {
				return DesignStartResponse{}, &APIError{Code: Conflict, Message: "the design session already has a different handed input"}
			}
			handed, alreadyHanded = document, true
		}
	}
	if _, found, err := DesignSelectionOf(repository, stream); err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
	} else if !alreadyHanded && !found {
		if api := s.RecordOwnerProjects(ctx, repository, stream, []config.ProjectID{project}, ""); api != nil {
			return DesignStartResponse{}, api
		}
	}
	if !alreadyHanded {
		if err := repository.Append(ctx, handed); err != nil {
			return DesignStartResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot hand off design session %s; check the trace repository", stream)}
		}
	}
	if err := s.Step("design-brief-handed"); err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if err := RecordFactoryPolicy(ctx, repository, stream, now, false); err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record factory authority for %s; check the trace repository", stream)}
	}
	designState, err := repository.Workflow(stream, DesignSessionSubject)
	if err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot read the design session marker of %s; check the trace repository", stream)}
	}
	header := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: DesignStartTransition, Revision: 1,
		Project: project, Workstream: stream, At: now, Actor: OwnerActor, Cause: DesignStartTransition}
	closeHeader := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: DesignStartTransition + "-design-session", Revision: 1,
		Project: project, Workstream: stream, At: now, Actor: OwnerActor, Cause: DesignStartTransition}
	closeTx := trace.Transaction{ExpectedVersion: designState.Version,
		Transition: trace.Transition{Header: closeHeader, Subject: DesignSessionSubject, From: DesignOpenValue, To: DesignClosedValue,
			Reason: "the owner started a workgroup from the design session's brief"}}
	// The design session's own marker closes in the same commit as the
	// feature state's own first transition: a design session's feature state
	// is always "" until this moment, so MoveFeatureStateWith's own From
	// check (rather than SetFeatureState's unconditional write) also refuses
	// a stray second attempt to hand this same workstream off through some
	// other path between the read above and this commit.
	state, err := repository.MoveFeatureStateWith(ctx, header, "", HandedState, "the owner started a workgroup from the design session's brief", closeTx)
	if err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot record the handed state of %s; check the trace repository", stream)}
	}
	if err := s.RefreshRuntime(); err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: "cannot resolve the runtime workstreams after starting the workgroup"}
	}
	return DesignStartResponse{Project: project, Workstream: stream, State: state.Value, Handed: handed.Path, Source: source}, nil
}
