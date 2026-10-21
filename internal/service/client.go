package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

type Client struct {
	http                *http.Client
	transport           *http.Transport
	defaultTimeout      time.Duration
	addProjectTimeout   time.Duration
	sessionCheckTimeout time.Duration
}

const defaultClientTimeout = 15 * time.Second
const addProjectTimeout = 60 * time.Second

// sessionCheckTimeout is the client's response budget for a session check.
// A check runs inline on the server (session_controls.go, sessionCheck) and
// the caller waits for it to finish, so the budget must outlive any
// project's configured checks_timeout; config.MaxChecksTimeout bounds how
// large that can be, plus a margin for recording the result.
const sessionCheckTimeout = config.MaxChecksTimeout + time.Minute

// NewClient never reads state files and never dials TCP or follows redirects.
func NewClient(socket string) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Client{http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, transport: transport, defaultTimeout: defaultClientTimeout, addProjectTimeout: addProjectTimeout, sessionCheckTimeout: sessionCheckTimeout}
}
func (c *Client) Close() { c.transport.CloseIdleConnections() }

// Do exchanges shared API types with the default response budget. Transport
// failures use the unavailable code; callers can still inspect context
// cancellation via their context.
func (c *Client) Do(ctx context.Context, method, path string, input, output any) error {
	return c.do(ctx, method, path, input, output, c.defaultTimeout)
}

func (c *Client) do(ctx context.Context, method, path string, input, output any, timeout time.Duration) error {
	if !strings.HasPrefix(path, api.Prefix+"/") {
		return fmt.Errorf("expected a versioned API path")
	}
	var body bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			return err
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, method, "http://osmia"+path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return transportError(ctx, err, timeout)
	}
	defer resp.Body.Close()
	d := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errorResponse(ctx, d, timeout)
	}
	if output == nil {
		_, err = io.Copy(io.Discard, resp.Body)
	} else {
		err = d.Decode(output)
	}
	if err != nil {
		return transportError(ctx, err, timeout)
	}
	return nil
}

// errorResponse decodes the API error of a failed response.
func errorResponse(ctx context.Context, d *json.Decoder, timeout time.Duration) error {
	var out api.ErrorResponse
	if err := d.Decode(&out); err != nil {
		if isTimeout(ctx, err) {
			return transportError(ctx, err, timeout)
		}
		return &api.APIError{Code: api.Internal, Message: "invalid API error response"}
	}
	if out.Error.Code == "" {
		return &api.APIError{Code: api.Internal, Message: "invalid API error response"}
	}
	return &out.Error
}

// Events reads the service's event stream and passes each event to handle
// until ctx ends, the stream ends or handle returns an error. The first event
// of every stream is api.EventResync. The default response budget covers
// connecting and the response header; the stream itself has none. Events
// returns handle's error, ctx's error once ctx ends, and otherwise an
// unavailable error: the service closed the stream or cannot be reached. A
// caller that reconnects reads everything again on the new stream's resync.
func (c *Client) Events(ctx context.Context, handle func(api.Event) error) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(streamCtx, "GET", "http://osmia"+api.Prefix+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	budget := time.AfterFunc(c.defaultTimeout, cancel)
	resp, err := c.http.Do(req)
	late := !budget.Stop()
	if late && ctx.Err() == nil {
		if err == nil {
			resp.Body.Close()
		}
		return &api.APIError{Code: api.Unavailable, Message: fmt.Sprintf("no response within %s", c.defaultTimeout)}
	}
	if err != nil {
		return transportError(ctx, err, c.defaultTimeout)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errorResponse(ctx, json.NewDecoder(io.LimitReader(resp.Body, 4<<20)), c.defaultTimeout)
	}
	err = readEvents(bufio.NewReader(resp.Body), handle)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// readEvents parses server-sent events and passes each one to handle. Comments,
// fields other than event and data, and events without data are skipped.
func readEvents(r *bufio.Reader, handle func(api.Event) error) error {
	var kind string
	var data []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return &api.APIError{Code: api.Unavailable, Message: "the event stream ended"}
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if len(data) > 0 {
				var e api.Event
				if json.Unmarshal([]byte(strings.Join(data, "\n")), &e) != nil || (kind != "" && api.EventKind(kind) != e.Kind) {
					return &api.APIError{Code: api.Internal, Message: "invalid event in the event stream"}
				}
				if err := handle(e); err != nil {
					return err
				}
			}
			kind, data = "", nil
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			kind = value
		case "data":
			data = append(data, value)
		}
	}
}

func transportError(ctx context.Context, err error, timeout time.Duration) error {
	if isTimeout(ctx, err) {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &api.APIError{Code: api.Unavailable, Message: "no response before caller's context deadline"}
		}
		return &api.APIError{Code: api.Unavailable, Message: fmt.Sprintf("no response within %s", timeout)}
	}
	return &api.APIError{Code: api.Unavailable, Message: "cannot reach Osmia Unix socket"}
}

func isTimeout(ctx context.Context, err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout() || errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded)
}
func (c *Client) Health(ctx context.Context) (api.HealthResponse, error) {
	var v api.HealthResponse
	err := c.Do(ctx, "GET", api.Prefix+"/health", nil, &v)
	return v, err
}
func (c *Client) Activity(ctx context.Context) (api.ActivityResponse, error) {
	var v api.ActivityResponse
	err := c.Do(ctx, "GET", api.Prefix+"/activity", nil, &v)
	return v, err
}
func (c *Client) Configuration(ctx context.Context) (api.ConfigResponse, error) {
	var v api.ConfigResponse
	err := c.Do(ctx, "GET", api.Prefix+"/config", nil, &v)
	return v, err
}
func (c *Client) Reload(ctx context.Context) (api.ReloadResponse, error) {
	var v api.ReloadResponse
	err := c.Do(ctx, "POST", api.Prefix+"/reload", nil, &v)
	return v, err
}
func (c *Client) Runtime(ctx context.Context) (api.RuntimeResponse, error) {
	var v api.RuntimeResponse
	err := c.Do(ctx, "GET", api.Prefix+"/runtime", nil, &v)
	return v, err
}
func (c *Client) AddProject(ctx context.Context, req api.ProjectAddRequest) (api.ProjectResponse, error) {
	var v api.ProjectResponse
	err := c.do(ctx, "POST", api.Prefix+"/projects", req, &v, c.addProjectTimeout)
	return v, err
}
func (c *Client) RemoveProject(ctx context.Context, id config.ProjectID) (api.ProjectResponse, error) {
	var v api.ProjectResponse
	err := c.Do(ctx, "DELETE", api.Prefix+"/projects", api.ProjectRemoveRequest{Project: id}, &v)
	return v, err
}

// AddRepo adds a repo to an existing project, the same way AddProject adds a
// project's own default repo.
func (c *Client) AddRepo(ctx context.Context, req api.RepoAddRequest) (api.ProjectResponse, error) {
	var v api.ProjectResponse
	err := c.do(ctx, "POST", api.Prefix+"/projects/repos", req, &v, c.addProjectTimeout)
	return v, err
}

// ExtractProject starts a new knowledge-base extraction of the active project.
func (c *Client) ExtractProject(ctx context.Context, id config.ProjectID) (api.ExtractionResponse, error) {
	var v api.ExtractionResponse
	err := c.Do(ctx, "POST", api.Prefix+"/projects/extract", api.ProjectExtractRequest{Project: id}, &v)
	return v, err
}

// RebaseProject asks for a drift rebase of every eligible workstream of a
// project and reports the workstreams the request covers.
func (c *Client) RebaseProject(ctx context.Context, id config.ProjectID) (api.ProjectRebaseResponse, error) {
	var v api.ProjectRebaseResponse
	err := c.Do(ctx, "POST", api.Prefix+"/projects/rebase", api.ProjectRebaseRequest{Project: id}, &v)
	return v, err
}

// RebaseWorkstream asks for a drift rebase of an eligible workstream, including
// a delivered pull request, and reports the drift rebase that answers it.
func (c *Client) RebaseWorkstream(ctx context.Context, id config.WorkstreamID) (api.WorkstreamRebaseResponse, error) {
	var v api.WorkstreamRebaseResponse
	err := c.Do(ctx, "POST", api.Prefix+"/rebase/"+url.PathEscape(string(id)), nil, &v)
	return v, err
}

// HandIn hands work to a project and returns the workstream it created.
func (c *Client) HandIn(ctx context.Context, req api.HandInRequest) (api.HandInResponse, error) {
	var v api.HandInResponse
	err := c.Do(ctx, "POST", api.Prefix+"/handin", req, &v)
	return v, err
}

// OpenDesign opens a design session in a project.
func (c *Client) OpenDesign(ctx context.Context, project config.ProjectID) (api.DesignResponse, error) {
	var v api.DesignResponse
	err := c.Do(ctx, "POST", api.Prefix+"/design", api.DesignOpenRequest{Project: project}, &v)
	return v, err
}

// Designs lists design sessions, optionally filtered by project.
func (c *Client) Designs(ctx context.Context, project config.ProjectID) (api.DesignsResponse, error) {
	var v api.DesignsResponse
	path := api.Prefix + "/design"
	if project != "" {
		path += "?project=" + url.QueryEscape(string(project))
	}
	err := c.Do(ctx, "GET", path, nil, &v)
	return v, err
}

// Design reads one design session.
func (c *Client) Design(ctx context.Context, id config.WorkstreamID) (api.DesignResponse, error) {
	var v api.DesignResponse
	err := c.Do(ctx, "GET", api.Prefix+"/design/"+url.PathEscape(string(id)), nil, &v)
	return v, err
}

// DesignConversation lists a design session's conversation with its agent.
func (c *Client) DesignConversation(ctx context.Context, id config.WorkstreamID) (api.ConversationResponse, error) {
	var v api.ConversationResponse
	err := c.Do(ctx, "GET", api.Prefix+"/design/"+url.PathEscape(string(id))+"/conversation", nil, &v)
	return v, err
}

// DesignMessage sends an owner message to a design session's agent.
func (c *Client) DesignMessage(ctx context.Context, id config.WorkstreamID, text string) (api.ConversationEntry, error) {
	var v api.ConversationEntry
	err := c.Do(ctx, "POST", api.Prefix+"/design/"+url.PathEscape(string(id))+"/message", api.DesignMessageRequest{Text: text}, &v)
	return v, err
}

// DesignStart starts a workgroup from a design session's drafted brief.
func (c *Client) DesignStart(ctx context.Context, id config.WorkstreamID) (api.DesignStartResponse, error) {
	var v api.DesignStartResponse
	err := c.Do(ctx, "POST", api.Prefix+"/design/"+url.PathEscape(string(id))+"/start", nil, &v)
	return v, err
}

// Abandon abandons a workstream with the owner's reason.
func (c *Client) Abandon(ctx context.Context, id config.WorkstreamID, reason string) (api.AbandonResponse, error) {
	var v api.AbandonResponse
	err := c.Do(ctx, "POST", api.Prefix+"/abandon/"+url.PathEscape(string(id)), api.AbandonRequest{Reason: reason}, &v)
	return v, err
}

// BaseUpstream moves a workstream parked on its abandoned base onto upstream.
func (c *Client) BaseUpstream(ctx context.Context, id, base config.WorkstreamID) (api.BaseUpstreamResponse, error) {
	var v api.BaseUpstreamResponse
	err := c.Do(ctx, "POST", api.Prefix+"/base/"+url.PathEscape(string(id))+"/upstream", api.BaseUpstreamRequest{Base: base}, &v)
	return v, err
}

// Archive permanently archives a terminal workstream and schedules trace deletion.
func (c *Client) Archive(ctx context.Context, id config.WorkstreamID) (api.ArchiveResponse, error) {
	var v api.ArchiveResponse
	err := c.Do(ctx, "POST", api.Prefix+"/archive/"+url.PathEscape(string(id)), nil, &v)
	return v, err
}

// Unarchive returns a conflict for permanently archived workstreams.
func (c *Client) Unarchive(ctx context.Context, id config.WorkstreamID) (api.ArchiveResponse, error) {
	var v api.ArchiveResponse
	err := c.Do(ctx, "DELETE", api.Prefix+"/archive/"+url.PathEscape(string(id)), nil, &v)
	return v, err
}

// ShedObject adds the owner's own objection to a workstream's current round.
func (c *Client) ShedObject(ctx context.Context, id config.WorkstreamID, argument string) (api.ShedResponse, error) {
	return c.shed(ctx, "object", id, api.ShedObjectRequest{Argument: argument})
}

// ShedRule records the owner's ruling on one objection that stands.
func (c *Client) ShedRule(ctx context.Context, id config.WorkstreamID, objection, disposition, note string) (api.ShedResponse, error) {
	return c.shed(ctx, "rule", id, api.ShedRuleRequest{Objection: objection, Disposition: disposition, Note: note})
}

// ShedSkip records that the owner skips debate on a workstream.
func (c *Client) ShedSkip(ctx context.Context, id config.WorkstreamID) (api.ShedResponse, error) {
	return c.shed(ctx, "skip", id, struct{}{})
}

// ShedOverrule records that the owner overruled one objection that stands.
func (c *Client) ShedOverrule(ctx context.Context, id config.WorkstreamID, objection, reason string) (api.ShedResponse, error) {
	return c.shed(ctx, "overrule", id, api.ShedOverruleRequest{Objection: objection, Reason: reason})
}

// ShedMore asks for further rounds of debate after it concluded.
func (c *Client) ShedMore(ctx context.Context, id config.WorkstreamID, rounds int) (api.ShedResponse, error) {
	return c.shed(ctx, "more", id, api.ShedMoreRequest{Rounds: rounds})
}

// ShedRedraft asks the architect for a redraft of the spec and the plan.
func (c *Client) ShedRedraft(ctx context.Context, id config.WorkstreamID, note string) (api.ShedResponse, error) {
	return c.shed(ctx, "redraft", id, api.ShedRedraftRequest{Note: note})
}

// Packet reads a workstream's ratification packet.
func (c *Client) Packet(ctx context.Context, id config.WorkstreamID) (api.PacketResponse, error) {
	var v api.PacketResponse
	err := c.Do(ctx, "GET", api.Prefix+"/packet/"+url.PathEscape(string(id)), nil, &v)
	return v, err
}

// Amendment reads one amendment of a workstream: its state, latest packet
// and the owner's latest decision.
func (c *Client) Amendment(ctx context.Context, id config.WorkstreamID, amendment string) (api.AmendmentResponse, error) {
	var v api.AmendmentResponse
	err := c.Do(ctx, "GET", api.Prefix+"/amendment/"+url.PathEscape(string(id))+"/"+url.PathEscape(amendment), nil, &v)
	return v, err
}

// CharterProposals lists the charter proposals waiting for the owner's
// decision.
func (c *Client) CharterProposals(ctx context.Context) (api.CharterProposalsResponse, error) {
	var v api.CharterProposalsResponse
	err := c.Do(ctx, "GET", api.Prefix+"/charter", nil, &v)
	return v, err
}

// CharterProposal reads the charter proposal made from one question of a
// workstream.
func (c *Client) CharterProposal(ctx context.Context, id config.WorkstreamID, question string) (api.CharterProposalView, error) {
	var v api.CharterProposalView
	err := c.Do(ctx, "GET", api.Prefix+"/charter/"+url.PathEscape(string(id))+"/"+url.PathEscape(question), nil, &v)
	return v, err
}

// DecideCharter records the owner's decision on a charter proposal.
func (c *Client) DecideCharter(ctx context.Context, id config.WorkstreamID, question string, req api.CharterDecisionRequest) (api.CharterProposalView, error) {
	var v api.CharterProposalView
	err := c.Do(ctx, "POST", api.Prefix+"/charter/"+url.PathEscape(string(id))+"/"+url.PathEscape(question), req, &v)
	return v, err
}

// Charter reads the latest revision of a project's charter.
func (c *Client) Charter(ctx context.Context, project config.ProjectID) (trace.Document, error) {
	var v trace.Document
	err := c.Do(ctx, "GET", api.Prefix+"/projects/charter/"+url.PathEscape(string(project)), nil, &v)
	return v, err
}

// EditCharter records the owner's charter edit of the revision they read.
func (c *Client) EditCharter(ctx context.Context, project config.ProjectID, edit api.CharterEdit) (trace.Document, error) {
	var v trace.Document
	err := c.Do(ctx, "PUT", api.Prefix+"/projects/charter/"+url.PathEscape(string(project)), edit, &v)
	return v, err
}

// Documents reads the latest revisions of a workstream's handed document,
// spec and plan, by record ID.
func (c *Client) Documents(ctx context.Context, id config.WorkstreamID) (map[string]trace.Document, error) {
	var v map[string]trace.Document
	err := c.Do(ctx, "GET", api.Prefix+"/documents/"+url.PathEscape(string(id)), nil, &v)
	return v, err
}

// EditDraft records the owner's edit of a workstream's spec and plan, of
// the revisions they read, and returns the latest revisions by record ID.
func (c *Client) EditDraft(ctx context.Context, id config.WorkstreamID, edit api.DraftEdit) (map[string]trace.Document, error) {
	var v map[string]trace.Document
	err := c.Do(ctx, "PUT", api.Prefix+"/documents/"+url.PathEscape(string(id)), edit, &v)
	return v, err
}

// DecideAmendment records the owner's decision on a revision of an
// amendment's packet.
func (c *Client) DecideAmendment(ctx context.Context, id config.WorkstreamID, amendment string, req api.AmendmentDecisionRequest) (api.AmendmentResponse, error) {
	var v api.AmendmentResponse
	err := c.Do(ctx, "POST", api.Prefix+"/amendment/"+url.PathEscape(string(id))+"/"+url.PathEscape(amendment), req, &v)
	return v, err
}

// Ratify ratifies the displayed revisions. Cross-repo groups also require
// the project-spec revision served by Packet.
func (c *Client) Ratify(ctx context.Context, id config.WorkstreamID, spec, plan int, projectSpecRevision ...int) (api.RatifyResponse, error) {
	var v api.RatifyResponse
	req := api.RatifyRequest{Spec: spec, Plan: plan}
	if len(projectSpecRevision) > 0 {
		req.ProjectSpecRevision = projectSpecRevision[0]
	}
	err := c.Do(ctx, "POST", api.Prefix+"/ratify/"+url.PathEscape(string(id)), req, &v)
	return v, err
}

// Delivery reads the current final report and draft description.
func (c *Client) Delivery(ctx context.Context, id config.WorkstreamID) (api.DeliveryPresentation, error) {
	var v api.DeliveryPresentation
	err := c.Do(ctx, "GET", api.Prefix+"/delivery/"+url.PathEscape(string(id)), nil, &v)
	return v, err
}

// ApproveDelivery records the owner's exact description for a reviewed commit.
func (c *Client) ApproveDelivery(ctx context.Context, id config.WorkstreamID, decision api.DeliveryDecision) (api.DeliveryApproval, error) {
	var v api.DeliveryApproval
	err := c.Do(ctx, "POST", api.Prefix+"/delivery/"+url.PathEscape(string(id)), decision, &v)
	return v, err
}

// RecordMerge records an assembled workstream delivered by the owner's own
// merge of its feature branch into upstream.
func (c *Client) RecordMerge(ctx context.Context, id config.WorkstreamID) (api.MergeResponse, error) {
	var v api.MergeResponse
	err := c.Do(ctx, "POST", api.Prefix+"/delivery/"+url.PathEscape(string(id))+"/merged", nil, &v)
	return v, err
}

func (c *Client) shed(ctx context.Context, action string, id config.WorkstreamID, input any) (api.ShedResponse, error) {
	var v api.ShedResponse
	err := c.Do(ctx, "POST", api.Prefix+"/shed/"+action+"/"+url.PathEscape(string(id)), input, &v)
	return v, err
}

// Statuses lists every workstream's status in the active project.
func (c *Client) Statuses(ctx context.Context) (api.StatusResponse, error) {
	var v api.StatusResponse
	err := c.Do(ctx, "GET", api.Prefix+"/status", nil, &v)
	return v, err
}

// OpenSession opens a session.
func (c *Client) OpenSession(ctx context.Context, req api.SessionOpenRequest) (api.SessionResponse, error) {
	var v api.SessionResponse
	err := c.Do(ctx, "POST", api.Prefix+"/sessions", req, &v)
	return v, err
}

// Sessions lists sessions, optionally filtered by project.
func (c *Client) Sessions(ctx context.Context, project config.ProjectID) (api.SessionsResponse, error) {
	var v api.SessionsResponse
	path := api.Prefix + "/sessions"
	if project != "" {
		path += "?project=" + url.QueryEscape(string(project))
	}
	err := c.Do(ctx, "GET", path, nil, &v)
	return v, err
}

// Session reads one session.
func (c *Client) Session(ctx context.Context, id config.WorkstreamID) (api.SessionResponse, error) {
	var v api.SessionResponse
	err := c.Do(ctx, "GET", api.Prefix+"/sessions/"+url.PathEscape(string(id)), nil, &v)
	return v, err
}

// SessionMessage sends an owner message to a session's agent.
func (c *Client) SessionMessage(ctx context.Context, id config.WorkstreamID, text string) (api.ConversationEntry, error) {
	var v api.ConversationEntry
	err := c.Do(ctx, "POST", api.Prefix+"/sessions/"+url.PathEscape(string(id))+"/message", api.SessionMessageRequest{Text: text}, &v)
	return v, err
}

// SessionStop stops a session's running turn.
func (c *Client) SessionStop(ctx context.Context, id config.WorkstreamID) (api.SessionResponse, error) {
	var v api.SessionResponse
	err := c.Do(ctx, "POST", api.Prefix+"/sessions/"+url.PathEscape(string(id))+"/stop", nil, &v)
	return v, err
}

// SessionChanges reads a session's uncommitted and committed changes.
func (c *Client) SessionChanges(ctx context.Context, id config.WorkstreamID) (api.SessionChangesResponse, error) {
	var v api.SessionChangesResponse
	err := c.Do(ctx, "GET", api.Prefix+"/sessions/"+url.PathEscape(string(id))+"/changes", nil, &v)
	return v, err
}

// SessionCommit commits every current worktree change onto the session
// branch under message, deduplicated by requestKey.
func (c *Client) SessionCommit(ctx context.Context, id config.WorkstreamID, message, requestKey string) (api.SessionCommitResponse, error) {
	var v api.SessionCommitResponse
	err := c.Do(ctx, "POST", api.Prefix+"/sessions/"+url.PathEscape(string(id))+"/commit", api.SessionCommitRequest{Message: message, RequestKey: requestKey}, &v)
	return v, err
}

// SessionCheck checks a session's head commit. It runs inline on the
// server, so it uses the heavier sessionCheckTimeout budget rather than the
// default, the same way AddProject uses addProjectTimeout.
func (c *Client) SessionCheck(ctx context.Context, id config.WorkstreamID) (api.SessionCheckRun, error) {
	var v api.SessionCheckRun
	err := c.do(ctx, "POST", api.Prefix+"/sessions/"+url.PathEscape(string(id))+"/check", nil, &v, c.sessionCheckTimeout)
	return v, err
}

// SessionPush pushes the session branch to the project's remote.
func (c *Client) SessionPush(ctx context.Context, id config.WorkstreamID) (api.SessionPushResponse, error) {
	var v api.SessionPushResponse
	err := c.Do(ctx, "POST", api.Prefix+"/sessions/"+url.PathEscape(string(id))+"/push", nil, &v)
	return v, err
}

// SessionEnd ends a session.
func (c *Client) SessionEnd(ctx context.Context, id config.WorkstreamID) (api.SessionResponse, error) {
	var v api.SessionResponse
	err := c.Do(ctx, "POST", api.Prefix+"/sessions/"+url.PathEscape(string(id))+"/end", nil, &v)
	return v, err
}

// Status reads one workstream's status.
func (c *Client) Status(ctx context.Context, id config.WorkstreamID) (api.WorkstreamStatus, error) {
	var v api.WorkstreamStatus
	err := c.Do(ctx, "GET", api.Prefix+"/status/"+url.PathEscape(string(id)), nil, &v)
	return v, err
}

func (c *Client) Trace(ctx context.Context, id config.WorkstreamID) (api.TraceSummary, error) {
	var v api.TraceSummary
	err := c.Do(ctx, "GET", api.Prefix+"/trace/"+url.PathEscape(string(id)), nil, &v)
	return v, err
}

func (c *Client) TraceUnit(ctx context.Context, id config.WorkstreamID, unit string) (api.UnitTrace, error) {
	var v api.UnitTrace
	err := c.Do(ctx, "GET", api.Prefix+"/trace/"+url.PathEscape(string(id))+"/unit/"+url.PathEscape(unit), nil, &v)
	return v, err
}

func (c *Client) TraceCriterion(ctx context.Context, id config.WorkstreamID, criterion string) (api.CriterionTrace, error) {
	var v api.CriterionTrace
	err := c.Do(ctx, "GET", api.Prefix+"/trace/"+url.PathEscape(string(id))+"/criterion/"+url.PathEscape(criterion), nil, &v)
	return v, err
}

func (c *Client) TraceCommit(ctx context.Context, id config.WorkstreamID, commit string) (api.CommitTrace, error) {
	var v api.CommitTrace
	err := c.Do(ctx, "GET", api.Prefix+"/trace/"+url.PathEscape(string(id))+"/commit/"+url.PathEscape(commit), nil, &v)
	return v, err
}

// Send sends an owner message to a workstream's chief of staff. It returns
// once the message is durable.
func (c *Client) Send(ctx context.Context, id config.WorkstreamID, text string) (api.ConversationEntry, error) {
	var v api.ConversationEntry
	err := c.Do(ctx, "POST", api.Prefix+"/conversation/"+url.PathEscape(string(id)), api.SendRequest{Text: text}, &v)
	return v, err
}

// Conversation lists a workstream's conversation with its chief of staff.
func (c *Client) Conversation(ctx context.Context, id config.WorkstreamID) (api.ConversationResponse, error) {
	var v api.ConversationResponse
	err := c.Do(ctx, "GET", api.Prefix+"/conversation/"+url.PathEscape(string(id)), nil, &v)
	return v, err
}

// Inbox lists every owner decision waiting for the owner.
func (c *Client) Inbox(ctx context.Context) (api.InboxResponse, error) {
	var v api.InboxResponse
	err := c.Do(ctx, "GET", api.Prefix+"/inbox", nil, &v)
	return v, err
}

// Answer records the owner's ruling on inbox entry number of project, which
// may be empty while exactly one project is active.
func (c *Client) Answer(ctx context.Context, number int, text string, project config.ProjectID) (api.AnswerResponse, error) {
	var v api.AnswerResponse
	err := c.Do(ctx, "POST", api.Prefix+"/inbox/"+strconv.Itoa(number), api.AnswerRequest{Text: text, Project: project}, &v)
	return v, err
}

// MoveUnit moves a unit to another state as the owner.
func (c *Client) MoveUnit(ctx context.Context, stream config.WorkstreamID, unit, to, note string) (api.UnitMoveResponse, error) {
	var v api.UnitMoveResponse
	err := c.Do(ctx, "POST", api.Prefix+"/move/"+string(stream)+"/"+unit, api.UnitMoveRequest{To: to, Note: note}, &v)
	return v, err
}

// MoveWorkstream moves a workstream to another feature state with a note.
func (c *Client) MoveWorkstream(ctx context.Context, stream config.WorkstreamID, to, note string) (api.WorkstreamMoveResponse, error) {
	var v api.WorkstreamMoveResponse
	err := c.Do(ctx, "POST", api.Prefix+"/move/"+string(stream), api.WorkstreamMoveRequest{To: to, Note: note}, &v)
	return v, err
}

// RuleContested records the owner's direction for a contested unit.
func (c *Client) RuleContested(ctx context.Context, stream config.WorkstreamID, unit, decision, note string) (api.ContestedRulingResponse, error) {
	var v api.ContestedRulingResponse
	err := c.Do(ctx, "POST", api.Prefix+"/contested/"+string(stream)+"/"+unit, api.ContestedRulingRequest{Decision: decision, Note: note}, &v)
	return v, err
}

// DestroySession permanently removes an exploratory session and its local branch.
func (c *Client) DestroySession(ctx context.Context, id config.WorkstreamID) (SessionDestroyResponse, error) {
	var v SessionDestroyResponse
	err := c.Do(ctx, "POST", Prefix+"/sessions/"+url.PathEscape(string(id))+"/destroy", nil, &v)
	return v, err
}

// DestroyDesign permanently removes an open design session.
func (c *Client) DestroyDesign(ctx context.Context, id config.WorkstreamID) (SessionDestroyResponse, error) {
	var v SessionDestroyResponse
	err := c.Do(ctx, "POST", Prefix+"/design/"+url.PathEscape(string(id))+"/destroy", nil, &v)
	return v, err
}
