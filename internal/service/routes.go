package service

import (
	"net/http"

	"github.com/kpenfound/osmia/internal/trace"
)

// Route describes one endpoint of the local API for its OpenAPI description.
// Path is an OpenAPI path template such as /v1/status/{workstream}. Request
// and Response hold zero values of the JSON body types; Request is nil for an
// endpoint that reads no body. Status is the success status, 200 when zero.
// Stream marks the text/event-stream endpoint, whose Response is one event.
type Route struct {
	Method   string
	Path     string
	Summary  string
	Request  any
	Response any
	Status   int
	Stream   bool
}

// Routes lists every endpoint Service.handle serves under Prefix.
var Routes = []Route{
	{Method: http.MethodGet, Path: Prefix + "/health", Summary: "Report API availability, backend readiness, API version and build identity", Response: HealthResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/activity", Summary: "List the backend operations the activity record holds", Response: ActivityResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/stop", Summary: "Stop the service; refused on the web and tailnet listeners", Response: StopResponse{}, Status: http.StatusAccepted},
	{Method: http.MethodGet, Path: Prefix + "/events", Summary: "Follow the event stream", Response: Event{}, Stream: true},

	{Method: http.MethodGet, Path: Prefix + "/config", Summary: "Show the loaded configuration, its disk drift and diagnostics", Response: ConfigResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/reload", Summary: "Reload the configuration files", Response: ReloadResponse{}},
	{Method: http.MethodPut, Path: Prefix + "/config/root", Summary: "Always refused with restart_required: the root changes only on restart", Response: ErrorResponse{}, Status: http.StatusConflict},
	{Method: http.MethodPut, Path: Prefix + "/config/listen", Summary: "Always refused with restart_required: listeners change only on restart", Response: ErrorResponse{}, Status: http.StatusConflict},

	{Method: http.MethodGet, Path: Prefix + "/runtime", Summary: "Show pauses, priorities, profiles and provider limits in effect", Response: RuntimeResponse{}},
	{Method: http.MethodPut, Path: Prefix + "/runtime/pause", Summary: "Pause the factory, a project, a workstream or a role", Request: PauseRequest{}, Response: MutationResponse{}},
	{Method: http.MethodDelete, Path: Prefix + "/runtime/pause", Summary: "Resume a paused target", Request: ClearPauseRequest{}, Response: MutationResponse{}},
	{Method: http.MethodPut, Path: Prefix + "/runtime/priority", Summary: "Set a project's workstream priority order", Request: PriorityRequest{}, Response: MutationResponse{}},
	{Method: http.MethodDelete, Path: Prefix + "/runtime/priority", Summary: "Clear a project's priority order", Request: ClearPriorityRequest{}, Response: MutationResponse{}},
	{Method: http.MethodPut, Path: Prefix + "/runtime/profile", Summary: "Override a role's profile", Request: ProfileRequest{}, Response: MutationResponse{}},
	{Method: http.MethodDelete, Path: Prefix + "/runtime/profile", Summary: "Restore a role's configured profile", Request: ClearProfileRequest{}, Response: MutationResponse{}},
	{Method: http.MethodDelete, Path: Prefix + "/runtime/provider-limit", Summary: "Clear a provider usage limit", Request: ClearProviderLimitRequest{}, Response: MutationResponse{}},

	{Method: http.MethodPost, Path: Prefix + "/projects", Summary: "Register a project", Request: ProjectAddRequest{}, Response: ProjectResponse{}},
	{Method: http.MethodDelete, Path: Prefix + "/projects", Summary: "Remove a project from active work", Request: ProjectRemoveRequest{}, Response: ProjectResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/projects/repos", Summary: "Add a repo to an existing project", Request: RepoAddRequest{}, Response: ProjectResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/projects/extract", Summary: "Start a knowledge-base extraction", Request: ProjectExtractRequest{}, Response: ExtractionResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/projects/rebase", Summary: "Ask for a drift rebase of a project's workstreams", Request: ProjectRebaseRequest{}, Response: ProjectRebaseResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/projects/memory/{project}", Summary: "Export a project's Hearsay setup", Response: MemorySetup{}},
	{Method: http.MethodGet, Path: Prefix + "/projects/charter/{project}", Summary: "Read a project's charter", Response: trace.Document{}},
	{Method: http.MethodPut, Path: Prefix + "/projects/charter/{project}", Summary: "Save a project's charter", Request: CharterEdit{}, Response: trace.Document{}},

	{Method: http.MethodPost, Path: Prefix + "/sessions", Summary: "Open a session", Request: SessionOpenRequest{}, Response: SessionResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/sessions", Summary: "List sessions, optionally filtered by project", Response: SessionsResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/sessions/{workstream}", Summary: "Show one session", Response: SessionResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/sessions/{workstream}/message", Summary: "Send a message to a session's agent; refused as busy while a turn is running", Request: SessionMessageRequest{}, Response: ConversationEntry{}},
	{Method: http.MethodPost, Path: Prefix + "/sessions/{workstream}/stop", Summary: "Stop a session's running turn, keeping its worktree changes", Response: SessionResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/sessions/{workstream}/changes", Summary: "Read a session's uncommitted worktree changes and its branch's commits since its base", Response: SessionChangesResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/sessions/{workstream}/commit", Summary: "Commit every current worktree change onto the session branch under the owner's message", Request: SessionCommitRequest{}, Response: SessionCommitResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/sessions/{workstream}/check", Summary: "Check the session head commit: smart under JEV, full otherwise; refused while a check is already running", Response: SessionCheckRun{}},
	{Method: http.MethodPost, Path: Prefix + "/sessions/{workstream}/push", Summary: "Push the session branch to the project's remote; refused when the remote branch has commits not on it", Response: SessionPushResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/sessions/{workstream}/end", Summary: "End a session, releasing its worktree and stored model-session state", Response: SessionResponse{}},

	{Method: http.MethodPost, Path: Prefix + "/design", Summary: "Open a design session in a project", Request: DesignOpenRequest{}, Response: DesignResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/design", Summary: "List design sessions, optionally filtered by project", Response: DesignsResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/design/{workstream}", Summary: "Show one design session", Response: DesignResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/design/{workstream}/conversation", Summary: "List a design session's conversation with its agent", Response: ConversationResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/design/{workstream}/message", Summary: "Send a message to a design session's agent; refused as busy while a turn is running", Request: DesignMessageRequest{}, Response: ConversationEntry{}},
	{Method: http.MethodPost, Path: Prefix + "/design/{workstream}/start", Summary: "Start a workgroup from a design session's drafted brief; the same thread continues as the workgroup's chief of staff", Response: DesignStartResponse{}},

	{Method: http.MethodPost, Path: Prefix + "/handin", Summary: "Hand in a workstream", Request: HandInRequest{}, Response: HandInResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/status", Summary: "List workstreams with capacity, usage and diagnostics", Response: StatusResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/status/{workstream}", Summary: "Show one workstream", Response: WorkstreamStatus{}},
	{Method: http.MethodPost, Path: Prefix + "/abandon/{workstream}", Summary: "Abandon an undelivered workstream", Request: AbandonRequest{}, Response: AbandonResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/rebase/{workstream}", Summary: "Ask for a drift rebase of one workstream", Response: WorkstreamRebaseResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/archive/{workstream}", Summary: "Permanently archive a terminal workstream, deleting its trace and retaining its title", Response: ArchiveResponse{}},
	{Method: http.MethodDelete, Path: Prefix + "/archive/{workstream}", Summary: "Refuse restoration of a permanently archived workstream", Response: ArchiveResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/base/{workstream}", Summary: "Show a workstream's base", Response: trace.WorkstreamBase{}},
	{Method: http.MethodPut, Path: Prefix + "/base/{workstream}", Summary: "Set a workstream's base before ratification", Request: BaseEdit{}, Response: trace.WorkstreamBase{}},
	{Method: http.MethodPost, Path: Prefix + "/base/{workstream}/upstream", Summary: "Move a workstream parked on its abandoned base onto upstream", Request: BaseUpstreamRequest{}, Response: BaseUpstreamResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/documents/{workstream}", Summary: "Read a workstream's handed document, spec and plan", Response: map[string]trace.Document{}},
	{Method: http.MethodPut, Path: Prefix + "/documents/{workstream}", Summary: "Save edits to a workstream's spec and plan", Request: DraftEdit{}, Response: map[string]trace.Document{}},

	{Method: http.MethodGet, Path: Prefix + "/conversation/{workstream}", Summary: "List a workstream's conversation with the chief of staff", Response: ConversationResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/conversation/{workstream}", Summary: "Send a message to a workstream's chief of staff", Request: SendRequest{}, Response: ConversationEntry{}},
	{Method: http.MethodGet, Path: Prefix + "/feed/{workstream}", Summary: "List a workstream's conversation, statuses, sessions and state changes in time order", Response: FeedResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/inbox", Summary: "List open owner decisions", Response: InboxResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/inbox/{number}", Summary: "Answer an inbox entry", Request: AnswerRequest{}, Response: AnswerResponse{}},

	{Method: http.MethodPost, Path: Prefix + "/shed/object/{workstream}", Summary: "Add an owner objection to the current debate round", Request: ShedObjectRequest{}, Response: ShedResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/shed/rule/{workstream}", Summary: "Sustain or dismiss an objection", Request: ShedRuleRequest{}, Response: ShedResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/shed/skip/{workstream}", Summary: "Skip debate and go to ratification", Response: ShedResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/shed/overrule/{workstream}", Summary: "Overrule an objection", Request: ShedOverruleRequest{}, Response: ShedResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/shed/more/{workstream}", Summary: "Ask for more debate rounds", Request: ShedMoreRequest{}, Response: ShedResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/shed/redraft/{workstream}", Summary: "Ask the architect for a redraft", Request: ShedRedraftRequest{}, Response: ShedResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/packet/{workstream}", Summary: "Show the ratification packet", Response: PacketResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/ratify/{workstream}", Summary: "Ratify the spec and plan", Request: RatifyRequest{}, Response: RatifyResponse{}},

	{Method: http.MethodPost, Path: Prefix + "/contested/{workstream}/{unit}", Summary: "Direct a contested unit", Request: ContestedRulingRequest{}, Response: ContestedRulingResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/move/{workstream}", Summary: "Move a workstream to another feature state", Request: WorkstreamMoveRequest{}, Response: WorkstreamMoveResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/move/{workstream}/{unit}", Summary: "Move a unit to another state", Request: UnitMoveRequest{}, Response: UnitMoveResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/amendment/{workstream}/{amendment}", Summary: "Show an amendment", Response: AmendmentResponse{}},
	{Method: http.MethodPost, Path: Prefix + "/amendment/{workstream}/{amendment}", Summary: "Decide an amendment", Request: AmendmentDecisionRequest{}, Response: AmendmentResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/charter", Summary: "List charter proposals", Response: CharterProposalsResponse{}},
	{Method: http.MethodGet, Path: Prefix + "/charter/{workstream}/{question}", Summary: "Show a charter proposal", Response: CharterProposalView{}},
	{Method: http.MethodPost, Path: Prefix + "/charter/{workstream}/{question}", Summary: "Ratify or decline a charter proposal", Request: CharterDecisionRequest{}, Response: CharterProposalView{}},

	{Method: http.MethodGet, Path: Prefix + "/delivery/{workstream}", Summary: "Show the final report and the drafted pull request title and body", Response: DeliveryPresentation{}},
	{Method: http.MethodPost, Path: Prefix + "/delivery/{workstream}", Summary: "Approve delivery", Request: DeliveryDecision{}, Response: DeliveryApproval{}},
	{Method: http.MethodPost, Path: Prefix + "/delivery/{workstream}/merged", Summary: "Record an assembled workstream delivered by the owner's own merge into upstream", Response: MergeResponse{}},

	{Method: http.MethodGet, Path: Prefix + "/trace/{workstream}", Summary: "Summarize a workstream's trace", Response: TraceSummary{}},
	{Method: http.MethodGet, Path: Prefix + "/trace/{workstream}/unit/{unit}", Summary: "Trace one unit", Response: UnitTrace{}},
	{Method: http.MethodGet, Path: Prefix + "/trace/{workstream}/criterion/{criterion}", Summary: "Trace one criterion", Response: CriterionTrace{}},
	{Method: http.MethodGet, Path: Prefix + "/trace/{workstream}/commit/{commit}", Summary: "Trace one commit", Response: CommitTrace{}},
}
