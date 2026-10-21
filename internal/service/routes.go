package service

import (
	"net/http"

	"github.com/kpenfound/osmia/internal/api"
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

// Routes lists every endpoint Service.handle serves under api.Prefix.
var Routes = []Route{
	{Method: http.MethodGet, Path: api.Prefix + "/health", Summary: "Report API availability, backend readiness, API version and build identity", Response: api.HealthResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/activity", Summary: "List the backend operations the activity record holds", Response: api.ActivityResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/stop", Summary: "Stop the service; refused on the web and tailnet listeners", Response: api.StopResponse{}, Status: http.StatusAccepted},
	{Method: http.MethodGet, Path: api.Prefix + "/events", Summary: "Follow the event stream", Response: api.Event{}, Stream: true},

	{Method: http.MethodGet, Path: api.Prefix + "/config", Summary: "Show the loaded configuration, its disk drift and diagnostics", Response: api.ConfigResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/reload", Summary: "Reload the configuration files", Response: api.ReloadResponse{}},
	{Method: http.MethodPut, Path: api.Prefix + "/config/root", Summary: "Always refused with restart_required: the root changes only on restart", Response: api.ErrorResponse{}, Status: http.StatusConflict},
	{Method: http.MethodPut, Path: api.Prefix + "/config/listen", Summary: "Always refused with restart_required: listeners change only on restart", Response: api.ErrorResponse{}, Status: http.StatusConflict},

	{Method: http.MethodGet, Path: api.Prefix + "/runtime", Summary: "Show pauses, priorities, profiles and provider limits in effect", Response: api.RuntimeResponse{}},
	{Method: http.MethodPut, Path: api.Prefix + "/runtime/pause", Summary: "Pause the factory, a project, a workstream or a role", Request: api.PauseRequest{}, Response: api.MutationResponse{}},
	{Method: http.MethodDelete, Path: api.Prefix + "/runtime/pause", Summary: "Resume a paused target", Request: api.ClearPauseRequest{}, Response: api.MutationResponse{}},
	{Method: http.MethodPut, Path: api.Prefix + "/runtime/priority", Summary: "Set a project's workstream priority order", Request: api.PriorityRequest{}, Response: api.MutationResponse{}},
	{Method: http.MethodDelete, Path: api.Prefix + "/runtime/priority", Summary: "Clear a project's priority order", Request: api.ClearPriorityRequest{}, Response: api.MutationResponse{}},
	{Method: http.MethodPut, Path: api.Prefix + "/runtime/profile", Summary: "Override a role's profile", Request: api.ProfileRequest{}, Response: api.MutationResponse{}},
	{Method: http.MethodDelete, Path: api.Prefix + "/runtime/profile", Summary: "Restore a role's configured profile", Request: api.ClearProfileRequest{}, Response: api.MutationResponse{}},
	{Method: http.MethodDelete, Path: api.Prefix + "/runtime/provider-limit", Summary: "Clear a provider usage limit", Request: api.ClearProviderLimitRequest{}, Response: api.MutationResponse{}},

	{Method: http.MethodPost, Path: api.Prefix + "/projects", Summary: "Register a project", Request: api.ProjectAddRequest{}, Response: api.ProjectResponse{}},
	{Method: http.MethodDelete, Path: api.Prefix + "/projects", Summary: "Remove a project from active work", Request: api.ProjectRemoveRequest{}, Response: api.ProjectResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/projects/repos", Summary: "Add a repo to an existing project", Request: api.RepoAddRequest{}, Response: api.ProjectResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/projects/extract", Summary: "Start a knowledge-base extraction", Request: api.ProjectExtractRequest{}, Response: api.ExtractionResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/projects/rebase", Summary: "Ask for a drift rebase of a project's workstreams", Request: api.ProjectRebaseRequest{}, Response: api.ProjectRebaseResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/projects/memory/{project}", Summary: "Export a project's Hearsay setup", Response: api.MemorySetup{}},
	{Method: http.MethodGet, Path: api.Prefix + "/projects/charter/{project}", Summary: "Read a project's charter", Response: trace.Document{}},
	{Method: http.MethodPut, Path: api.Prefix + "/projects/charter/{project}", Summary: "Save a project's charter", Request: api.CharterEdit{}, Response: trace.Document{}},

	{Method: http.MethodPost, Path: api.Prefix + "/sessions", Summary: "Open a session", Request: api.SessionOpenRequest{}, Response: api.SessionResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/sessions", Summary: "List sessions, optionally filtered by project", Response: api.SessionsResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/sessions/{workstream}", Summary: "Show one session", Response: api.SessionResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/sessions/{workstream}/message", Summary: "Send a message to a session's agent; refused as busy while a turn is running", Request: api.SessionMessageRequest{}, Response: api.ConversationEntry{}},
	{Method: http.MethodPost, Path: api.Prefix + "/sessions/{workstream}/stop", Summary: "Stop a session's running turn, keeping its worktree changes", Response: api.SessionResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/sessions/{workstream}/changes", Summary: "Read a session's uncommitted worktree changes and its branch's commits since its base", Response: api.SessionChangesResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/sessions/{workstream}/commit", Summary: "Commit every current worktree change onto the session branch under the owner's message", Request: api.SessionCommitRequest{}, Response: api.SessionCommitResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/sessions/{workstream}/check", Summary: "Check the session head commit: smart under JEV, full otherwise; refused while a check is already running", Response: api.SessionCheckRun{}},
	{Method: http.MethodPost, Path: api.Prefix + "/sessions/{workstream}/push", Summary: "Push the session branch to the project's remote; refused when the remote branch has commits not on it", Response: api.SessionPushResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/sessions/{workstream}/end", Summary: "End a session, releasing its worktree and stored model-session state", Response: api.SessionResponse{}},

	{Method: http.MethodPost, Path: api.Prefix + "/sessions/{workstream}/destroy", Summary: "Permanently destroy an exploratory session and its local branch; refused while busy", Response: api.SessionDestroyResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/design/{workstream}/destroy", Summary: "Permanently destroy an open design session; refused while busy or already started", Response: api.SessionDestroyResponse{}},

	{Method: http.MethodPost, Path: api.Prefix + "/design", Summary: "Open a design session in a project", Request: api.DesignOpenRequest{}, Response: api.DesignResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/design", Summary: "List design sessions, optionally filtered by project", Response: api.DesignsResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/design/{workstream}", Summary: "Show one design session", Response: api.DesignResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/design/{workstream}/conversation", Summary: "List a design session's conversation with its agent", Response: api.ConversationResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/design/{workstream}/message", Summary: "Send a message to a design session's agent; refused as busy while a turn is running", Request: api.DesignMessageRequest{}, Response: api.ConversationEntry{}},
	{Method: http.MethodPost, Path: api.Prefix + "/design/{workstream}/start", Summary: "Start a workgroup from a design session's drafted brief; the same thread continues as the workgroup's chief of staff", Response: api.DesignStartResponse{}},

	{Method: http.MethodPost, Path: api.Prefix + "/handin", Summary: "Hand in a workstream", Request: api.HandInRequest{}, Response: api.HandInResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/status", Summary: "List workstreams with capacity, usage and diagnostics", Response: api.StatusResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/status/{workstream}", Summary: "Show one workstream", Response: api.WorkstreamStatus{}},
	{Method: http.MethodPost, Path: api.Prefix + "/abandon/{workstream}", Summary: "Abandon an undelivered workstream", Request: api.AbandonRequest{}, Response: api.AbandonResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/rebase/{workstream}", Summary: "Ask for a drift rebase of one workstream", Response: api.WorkstreamRebaseResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/archive/{workstream}", Summary: "Permanently archive a terminal workstream, deleting its trace and retaining its title", Response: api.ArchiveResponse{}},
	{Method: http.MethodDelete, Path: api.Prefix + "/archive/{workstream}", Summary: "Refuse restoration of a permanently archived workstream", Response: api.ArchiveResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/base/{workstream}", Summary: "Show a workstream's base", Response: trace.WorkstreamBase{}},
	{Method: http.MethodPut, Path: api.Prefix + "/base/{workstream}", Summary: "Set a workstream's base before ratification", Request: api.BaseEdit{}, Response: trace.WorkstreamBase{}},
	{Method: http.MethodPost, Path: api.Prefix + "/base/{workstream}/upstream", Summary: "Move a workstream parked on its abandoned base onto upstream", Request: api.BaseUpstreamRequest{}, Response: api.BaseUpstreamResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/documents/{workstream}", Summary: "Read a workstream's handed document, spec and plan", Response: map[string]trace.Document{}},
	{Method: http.MethodPut, Path: api.Prefix + "/documents/{workstream}", Summary: "Save edits to a workstream's spec and plan", Request: api.DraftEdit{}, Response: map[string]trace.Document{}},

	{Method: http.MethodGet, Path: api.Prefix + "/conversation/{workstream}", Summary: "List a workstream's conversation with the chief of staff", Response: api.ConversationResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/conversation/{workstream}", Summary: "Send a message to a workstream's chief of staff", Request: api.SendRequest{}, Response: api.ConversationEntry{}},
	{Method: http.MethodGet, Path: api.Prefix + "/feed/{workstream}", Summary: "List a workstream's conversation, statuses, sessions, all state changes, check runs and final reviews in time order", Response: api.FeedResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/inbox", Summary: "List open owner decisions", Response: api.InboxResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/inbox/{number}", Summary: "Answer an inbox entry", Request: api.AnswerRequest{}, Response: api.AnswerResponse{}},

	{Method: http.MethodPost, Path: api.Prefix + "/shed/object/{workstream}", Summary: "Add an owner objection to the current debate round", Request: api.ShedObjectRequest{}, Response: api.ShedResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/shed/rule/{workstream}", Summary: "Sustain or dismiss an objection", Request: api.ShedRuleRequest{}, Response: api.ShedResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/shed/skip/{workstream}", Summary: "Skip debate and go to ratification", Response: api.ShedResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/shed/overrule/{workstream}", Summary: "Overrule an objection", Request: api.ShedOverruleRequest{}, Response: api.ShedResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/shed/more/{workstream}", Summary: "Ask for more debate rounds", Request: api.ShedMoreRequest{}, Response: api.ShedResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/shed/redraft/{workstream}", Summary: "Ask the architect for a redraft", Request: api.ShedRedraftRequest{}, Response: api.ShedResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/packet/{workstream}", Summary: "Show the ratification packet", Response: api.PacketResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/ratify/{workstream}", Summary: "Ratify the spec and plan", Request: api.RatifyRequest{}, Response: api.RatifyResponse{}},

	{Method: http.MethodPost, Path: api.Prefix + "/contested/{workstream}/{unit}", Summary: "Direct a contested unit", Request: api.ContestedRulingRequest{}, Response: api.ContestedRulingResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/move/{workstream}", Summary: "Move a workstream to another feature state", Request: api.WorkstreamMoveRequest{}, Response: api.WorkstreamMoveResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/move/{workstream}/{unit}", Summary: "Move a unit to another state", Request: api.UnitMoveRequest{}, Response: api.UnitMoveResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/amendment/{workstream}/{amendment}", Summary: "Show an amendment", Response: api.AmendmentResponse{}},
	{Method: http.MethodPost, Path: api.Prefix + "/amendment/{workstream}/{amendment}", Summary: "Decide an amendment", Request: api.AmendmentDecisionRequest{}, Response: api.AmendmentResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/charter", Summary: "List charter proposals", Response: api.CharterProposalsResponse{}},
	{Method: http.MethodGet, Path: api.Prefix + "/charter/{workstream}/{question}", Summary: "Show a charter proposal", Response: api.CharterProposalView{}},
	{Method: http.MethodPost, Path: api.Prefix + "/charter/{workstream}/{question}", Summary: "Ratify or decline a charter proposal", Request: api.CharterDecisionRequest{}, Response: api.CharterProposalView{}},

	{Method: http.MethodGet, Path: api.Prefix + "/delivery/{workstream}", Summary: "Show the final report and the drafted pull request title and body", Response: api.DeliveryPresentation{}},
	{Method: http.MethodPost, Path: api.Prefix + "/delivery/{workstream}", Summary: "Approve delivery", Request: api.DeliveryDecision{}, Response: api.DeliveryApproval{}},
	{Method: http.MethodPost, Path: api.Prefix + "/delivery/{workstream}/merged", Summary: "Record an assembled workstream delivered by the owner's own merge into upstream", Response: api.MergeResponse{}},

	{Method: http.MethodGet, Path: api.Prefix + "/trace/{workstream}", Summary: "Summarize a workstream's trace", Response: api.TraceSummary{}},
	{Method: http.MethodGet, Path: api.Prefix + "/trace/{workstream}/unit/{unit}", Summary: "Trace one unit", Response: api.UnitTrace{}},
	{Method: http.MethodGet, Path: api.Prefix + "/trace/{workstream}/criterion/{criterion}", Summary: "Trace one criterion", Response: api.CriterionTrace{}},
	{Method: http.MethodGet, Path: api.Prefix + "/trace/{workstream}/commit/{commit}", Summary: "Trace one commit", Response: api.CommitTrace{}},
}
