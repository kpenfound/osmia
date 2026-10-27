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

// Design session tool and document names. file_read, draft_brief and read_repo
// are the only tools a design session's agent may ever reach; every other tool a
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
	Content  string             `json:"content"`
	Projects []config.ProjectID `json:"projects,omitempty"`
}

// LatestDesignBrief resolves the legacy default without changing its revision.
func LatestDesignBrief(repository *trace.Repository, stream config.WorkstreamID) (DesignBriefRecord, int, error) {
	docs, err := trace.Read[trace.Document](repository, stream)
	if err != nil {
		return DesignBriefRecord{}, 0, err
	}
	var latest trace.Document
	for _, d := range docs {
		if d.ID == DesignDraftDocument && d.Revision > latest.Revision {
			latest = d
		}
	}
	brief := DesignBriefRecord{}
	if latest.Revision != 0 {
		if err := json.Unmarshal([]byte(latest.Content), &brief); err != nil {
			return brief, 0, err
		}
	}
	if brief.Projects == nil {
		brief.Projects = []config.ProjectID{repository.Project()}
	}
	return brief, latest.Revision, nil
}

func DesignBriefView(repository *trace.Repository, view DesignView) (DesignView, error) {
	brief, revision, err := LatestDesignBrief(repository, view.Workstream)
	view.BriefRevision, view.Brief, view.Projects = revision, brief.Content, brief.Projects
	return view, err
}

// IsDesignWorkstream reports whether stream is a design session: a
// workstream openDesign created and startDesignWorkgroup has not yet handed
// off. Factory discovery classifies it under the trace-deletion guard so an
// owner destroy cannot invalidate a later feature-workstream read.
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
				view, err := DesignBriefView(active.Repository, RenderDesignView(active.ID, w))
				if err != nil {
					return DesignsResponse{}, &APIError{Code: Internal, Message: err.Error()}
				}
				out = append(out, view)
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
			view, err := DesignBriefView(active.Repository, RenderDesignView(active.ID, w))
			if err != nil {
				return DesignView{}, &APIError{Code: Internal, Message: err.Error()}
			}
			return view, nil
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
	"You have a catalog of every active project below. Brief projects have their charter and knowledge base inline; " +
	"other projects have those documents in your view. Read any active project's committed source code through read_repo. You hold no write tool, no version control tool, no credential and no " +
	"owner gate: you cannot write a file, commit, push, open a pull request, rule on anything or start work yourself.\n\n" +
	"Run a readiness pass before handoff. Ask the owner only about choices that are expensive to undo (contracts, data " +
	"models, migrations, public APIs), genuine forks with no clear signal in the code or the charter, or points that are in " +
	"tension with the owner's stated intent. Give every such question a recommended answer. Infer everything else from the " +
	"code, the project's conventions and the owner's stated intent, and show it as a collapsed, overridable assumption the " +
	"owner can skim and override, for example inside a <details><summary>Assumptions</summary>...</details> block, rather " +
	"than asking about it.\n\n" +
	"An owner request to add or remove a project is confirmation of that list change. If you infer a project change, " +
	"raise it as a readiness question with a recommendation and apply it only after the owner agrees. " +
	"Use draft_brief projects to record the confirmed ordered project list; omitting projects keeps the current list.\n\n" +
	"Once the readiness pass is answered, call " + DraftBriefToolName + " with the brief: decisions, intent and tradeoffs, " +
	"and the options you explicitly rejected, as one piece of markdown. Call it again whenever the brief changes; only its " +
	"latest call is used. The owner starts the workgroup once satisfied; you never start it yourself, and nothing you say " +
	"or record causes any work to begin."

// DesignSystemPrompt includes the active catalog and only brief projects' full context.
func (s *Controller) DesignSystemPrompt(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID) (string, error) {
	brief, _, err := LatestDesignBrief(repository, stream)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(DesignRolePrompt)
	b.WriteString("\n\n## Active project catalog\nProject | Name | Upstream | In brief | Reference files\n--- | --- | --- | --- | ---\n")
	for _, project := range s.Current().Projects {
		references := "inline below"
		if !slices.Contains(brief.Projects, project.ID) {
			references = fmt.Sprintf("projects/%s/charter.md, projects/%s/kb.md", project.ID, project.ID)
		}
		fmt.Fprintf(&b, "%s | %s | %s | %t | %s\n", project.ID, project.Name, project.Upstream, slices.Contains(brief.Projects, project.ID), references)
	}
	for _, project := range brief.Projects {
		repo, err := s.Repository(project)
		if err != nil {
			return "", err
		}
		charterDoc, err := repo.Charter(ctx)
		if err != nil {
			return "", err
		}
		knowledge, err := DesignRepoKnowledge(repo)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "\n## Project %s charter\n%s\n## Project %s knowledge base\n%s", project, charterDoc.Content, project, knowledge)
	}
	return b.String(), nil
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
		if document.ID == HandedDocument || document.ID == DesignStartIntentDocument {
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
	prompt, err := s.DesignSystemPrompt(ctx, repository, stream)
	if err != nil {
		return ConversationEntry{}, &APIError{Code: Internal, Message: fmt.Sprintf("cannot assemble the context of design session %s; check the active projects' charters and knowledge bases", stream)}
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
func (s *Controller) DraftBriefTool(repository *trace.Repository, scope coreadapter.Scope, now func() time.Time) coreadapter.Tool {
	return coreadapter.Tool{Name: DraftBriefToolName, Effect: coreadapter.ToolMemory,
		Description: "Record the brief for this design session: decisions, intent and tradeoffs, and the options you explicitly rejected, as one piece of markdown text. Call it again whenever the brief changes; only the latest call is used when the owner starts a workgroup from it. projects is an optional ordered list of active projects; omitting it keeps the current list. Include list changes only when confirmed by the owner.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"content":{"type":"string"},"projects":{"type":"array","items":{"type":"string"}}},"required":["content"],"additionalProperties":false}`),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			if err := repository.ActiveTurn(scope); err != nil {
				return nil, err
			}
			var in struct {
				Content  string          `json:"content"`
				Projects json.RawMessage `json:"projects"`
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
			brief, revision, err := LatestDesignBrief(repository, stream)
			if err != nil {
				return nil, err
			}
			if in.Projects != nil {
				if err := json.Unmarshal(in.Projects, &brief.Projects); err != nil {
					return RefuseReport("projects must be an ordered project list")
				}
			}
			if api := s.ValidateProjects(ctx, brief.Projects); api != nil {
				return RefuseReport("%s", api.Message)
			}
			brief.Content = content
			k := revision + 1
			at := now()
			h := trace.Header{Schema: "osmia.trace.document", Version: trace.Version, ID: DesignDraftDocument, Revision: k,
				Project: repository.Project(), Workstream: stream, At: at, Actor: ChiefActor, Cause: DraftBriefToolName}
			data, err := json.Marshal(brief)
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
// head of any active project: the session's project by default, or another
// active project named by its project ID. It
// reads only; it never writes, fetches credentials or touches version
// control beyond a local, read-only export of the head commit.
func DesignReadRepoTool(cfg *config.Config, r *trace.Repository, controls *Controls, scope coreadapter.Scope) coreadapter.Tool {
	return coreadapter.Tool{Name: DesignReadRepoToolName, Effect: coreadapter.ToolRead,
		Description: "Read a file, or list a directory, from any active project, read-only. repo defaults to the session project; name any active project by its project ID to read it instead. path defaults to the root; start is a one-based line, lines defaults to 200 (maximum 1000).",
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
				current = service.Current().For(r.Project())
			}
			target := current.Project
			if in.Repo != "" {
				target = current.For(config.ProjectID(in.Repo)).Project
			}
			if !current.Active(target.ID) {
				return nil, fmt.Errorf("repo %q is not an active project", in.Repo)
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

// StageDesignWorkspace puts non-brief projects' reference documents in the read-only view.
func (s *Controller) StageDesignWorkspace(ctx context.Context, repository *trace.Repository, stream config.WorkstreamID, workspace string) ([]string, error) {
	if err := os.RemoveAll(workspace); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workspace, 0700); err != nil {
		return nil, err
	}
	brief, _, err := LatestDesignBrief(repository, stream)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, project := range s.Current().Projects {
		if slices.Contains(brief.Projects, project.ID) {
			continue
		}
		repo, err := s.Repository(project.ID)
		if err != nil {
			return nil, err
		}
		charterDoc, err := repo.Charter(ctx)
		if err != nil {
			return nil, err
		}
		knowledge, err := DesignRepoKnowledge(repo)
		if err != nil {
			return nil, err
		}
		for _, doc := range []struct{ name, content string }{{"charter.md", charterDoc.Content}, {"kb.md", knowledge}} {
			path := filepath.Join("projects", string(project.ID), doc.name)
			full := filepath.Join(workspace, path)
			if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(full, []byte(doc.content), 0600); err != nil {
				return nil, err
			}
			paths = append(paths, filepath.ToSlash(path))
		}
	}
	return paths, nil
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
func (s *Controller) StartDesignWorkgroup(ctx context.Context, raw string, requests ...DesignStartRequest) (DesignStartResponse, *APIError) {
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
		return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if UnfinishedSessionTurn(th) {
		return DesignStartResponse{}, &APIError{Code: Conflict, Message: "a turn is running for this design session; try again once it finishes"}
	}
	intent, found, err := DesignStartIntentOf(repository, stream)
	if err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	var revision *int
	if len(requests) > 0 {
		revision = requests[0].BriefRevision
	}
	if !found {
		// A handed input without an intent belongs to upgrade recovery. Never attach
		// today's draft project list to that immutable legacy input.
		docs, err := trace.Read[trace.Document](repository, stream)
		if err != nil {
			return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		for _, d := range docs {
			if d.ID == HandedDocument {
				if d.Source != "design:"+string(stream) || d.Path != DesignBriefPath {
					return DesignStartResponse{}, &APIError{Code: Conflict, Message: "the design session already has a different handed input"}
				}
				if err := s.ReconcileDesignStart(ctx, repository, stream); err != nil {
					return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
				}
				return DesignStartResponse{Project: project, Workstream: stream, State: HandedState, Handed: d.Path, Source: d.Source}, nil
			}
		}
		brief, latest, err := LatestDesignBrief(repository, stream)
		if err != nil {
			return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		if revision != nil && *revision != latest {
			return DesignStartResponse{}, &APIError{Code: Conflict, Message: "the brief has changed; read the latest brief before starting"}
		}
		if latest == 0 {
			return DesignStartResponse{}, &APIError{Code: Validation, Message: "the design agent has not drafted a brief yet; keep talking to it until it records one"}
		}
		if strings.TrimSpace(brief.Content) == "" {
			return DesignStartResponse{}, &APIError{Code: Internal, Message: "cannot read the drafted brief"}
		}
		// An owner selection recorded before handoff remains authoritative
		// when resuming an upgraded start that has no intent yet.
		if selected, recorded, err := DesignSelectionOf(repository, stream); err != nil {
			return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
		} else if recorded {
			brief.Projects = make([]config.ProjectID, 0, len(selected.Members))
			for _, member := range selected.Members {
				brief.Projects = append(brief.Projects, member.Repo)
			}
		}
		if api := s.ValidateProjects(ctx, brief.Projects); api != nil {
			return DesignStartResponse{}, api
		}
		transcript, err := DesignTranscript(repository, stream)
		if err != nil {
			return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		now := s.Now()
		handed := trace.Document{Header: OwnerHeader(HandedDocument, project, stream, DesignStartTransition, now), Path: DesignBriefPath,
			Content: strings.TrimRight(brief.Content, "\n") + "\n" + DesignTranscriptHeading + transcript, Source: "design:" + string(stream)}
		handed.Schema = "osmia.trace.document"
		intent = DesignStartIntent{BriefRevision: latest, Projects: brief.Projects, Handed: handed}
		data, err := json.Marshal(intent)
		if err != nil {
			return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		h := OwnerHeader(DesignStartIntentDocument, project, stream, DesignStartTransition, now)
		h.Schema = "osmia.trace.document"
		if err := repository.RecordDocuments(ctx, []trace.Document{{Header: h, Path: "tools/design-start.json", Content: string(data)}}); err != nil {
			return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
		if err := s.Step("design-start-intent-recorded"); err != nil {
			return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
		}
	} else if revision != nil && *revision != intent.BriefRevision {
		return DesignStartResponse{}, &APIError{Code: Conflict, Message: "a start is already pinned to another brief revision"}
	}
	if err := s.PreparePinnedDesignStart(ctx, repository, stream, intent); err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if err := s.ResumeHandedDesign(ctx, repository, stream, intent.Handed); err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	if err := s.RefreshRuntime(); err != nil {
		return DesignStartResponse{}, &APIError{Code: Internal, Message: err.Error()}
	}
	return DesignStartResponse{Project: project, Workstream: stream, State: HandedState, Handed: intent.Handed.Path, Source: intent.Handed.Source}, nil
}
