package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

type Client struct {
	http      *http.Client
	transport *http.Transport
}

// NewClient never reads state files and never dials TCP or follows redirects.
func NewClient(socket string) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Client{http: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, transport: transport}
}
func (c *Client) Close() { c.transport.CloseIdleConnections() }

// Do exchanges shared API types. Transport failures use the unavailable code;
// callers can still inspect context cancellation via their context.
func (c *Client) Do(ctx context.Context, method, path string, input, output any) error {
	if !strings.HasPrefix(path, Prefix+"/") {
		return fmt.Errorf("expected a versioned API path")
	}
	var body bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://osmia"+path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return &APIError{Unavailable, "cannot reach Osmia Unix socket"}
	}
	defer resp.Body.Close()
	d := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var out ErrorResponse
		if err := d.Decode(&out); err != nil || out.Error.Code == "" {
			return &APIError{Internal, "invalid API error response"}
		}
		return &out.Error
	}
	if output == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return d.Decode(output)
}
func (c *Client) Health(ctx context.Context) (HealthResponse, error) {
	var v HealthResponse
	err := c.Do(ctx, "GET", Prefix+"/health", nil, &v)
	return v, err
}
func (c *Client) Configuration(ctx context.Context) (ConfigResponse, error) {
	var v ConfigResponse
	err := c.Do(ctx, "GET", Prefix+"/config", nil, &v)
	return v, err
}
func (c *Client) Runtime(ctx context.Context) (RuntimeResponse, error) {
	var v RuntimeResponse
	err := c.Do(ctx, "GET", Prefix+"/runtime", nil, &v)
	return v, err
}
func (c *Client) AddProject(ctx context.Context, req ProjectAddRequest) (ProjectResponse, error) {
	var v ProjectResponse
	err := c.Do(ctx, "POST", Prefix+"/projects", req, &v)
	return v, err
}
func (c *Client) RemoveProject(ctx context.Context, id config.ProjectID) (ProjectResponse, error) {
	var v ProjectResponse
	err := c.Do(ctx, "DELETE", Prefix+"/projects", ProjectRemoveRequest{Project: id}, &v)
	return v, err
}

// ExtractProject starts a new knowledge-base extraction of the active project.
func (c *Client) ExtractProject(ctx context.Context, id config.ProjectID) (ExtractionResponse, error) {
	var v ExtractionResponse
	err := c.Do(ctx, "POST", Prefix+"/projects/extract", ProjectExtractRequest{Project: id}, &v)
	return v, err
}

// HandIn hands work to a project and returns the workstream it created.
func (c *Client) HandIn(ctx context.Context, req HandInRequest) (HandInResponse, error) {
	var v HandInResponse
	err := c.Do(ctx, "POST", Prefix+"/handin", req, &v)
	return v, err
}

// Abandon abandons a workstream with the owner's reason.
func (c *Client) Abandon(ctx context.Context, id config.WorkstreamID, reason string) (AbandonResponse, error) {
	var v AbandonResponse
	err := c.Do(ctx, "POST", Prefix+"/abandon/"+url.PathEscape(string(id)), AbandonRequest{Reason: reason}, &v)
	return v, err
}

// ShedObject adds the owner's own objection to a workstream's current round.
func (c *Client) ShedObject(ctx context.Context, id config.WorkstreamID, argument string) (ShedResponse, error) {
	return c.shed(ctx, "object", id, ShedObjectRequest{Argument: argument})
}

// ShedRule records the owner's ruling on one objection that stands.
func (c *Client) ShedRule(ctx context.Context, id config.WorkstreamID, objection, disposition, note string) (ShedResponse, error) {
	return c.shed(ctx, "rule", id, ShedRuleRequest{Objection: objection, Disposition: disposition, Note: note})
}

// ShedSkip records that the owner skips debate on a workstream.
func (c *Client) ShedSkip(ctx context.Context, id config.WorkstreamID) (ShedResponse, error) {
	return c.shed(ctx, "skip", id, struct{}{})
}

// ShedOverrule records that the owner overruled one objection that stands.
func (c *Client) ShedOverrule(ctx context.Context, id config.WorkstreamID, objection, reason string) (ShedResponse, error) {
	return c.shed(ctx, "overrule", id, ShedOverruleRequest{Objection: objection, Reason: reason})
}

// ShedMore asks for further rounds of debate after it concluded.
func (c *Client) ShedMore(ctx context.Context, id config.WorkstreamID, rounds int) (ShedResponse, error) {
	return c.shed(ctx, "more", id, ShedMoreRequest{Rounds: rounds})
}

// ShedRedraft asks the architect for a redraft of the spec and the plan.
func (c *Client) ShedRedraft(ctx context.Context, id config.WorkstreamID, note string) (ShedResponse, error) {
	return c.shed(ctx, "redraft", id, ShedRedraftRequest{Note: note})
}

// Packet reads a workstream's ratification packet.
func (c *Client) Packet(ctx context.Context, id config.WorkstreamID) (PacketResponse, error) {
	var v PacketResponse
	err := c.Do(ctx, "GET", Prefix+"/packet/"+url.PathEscape(string(id)), nil, &v)
	return v, err
}

// Ratify ratifies the given revisions of a workstream's spec and plan.
func (c *Client) Ratify(ctx context.Context, id config.WorkstreamID, spec, plan int) (RatifyResponse, error) {
	var v RatifyResponse
	err := c.Do(ctx, "POST", Prefix+"/ratify/"+url.PathEscape(string(id)), RatifyRequest{Spec: spec, Plan: plan}, &v)
	return v, err
}

func (c *Client) shed(ctx context.Context, action string, id config.WorkstreamID, input any) (ShedResponse, error) {
	var v ShedResponse
	err := c.Do(ctx, "POST", Prefix+"/shed/"+action+"/"+url.PathEscape(string(id)), input, &v)
	return v, err
}

// Statuses lists every workstream's status in the active project.
func (c *Client) Statuses(ctx context.Context) (StatusResponse, error) {
	var v StatusResponse
	err := c.Do(ctx, "GET", Prefix+"/status", nil, &v)
	return v, err
}

// Status reads one workstream's status.
func (c *Client) Status(ctx context.Context, id config.WorkstreamID) (WorkstreamStatus, error) {
	var v WorkstreamStatus
	err := c.Do(ctx, "GET", Prefix+"/status/"+url.PathEscape(string(id)), nil, &v)
	return v, err
}

// Send sends an owner message to a workstream's chief of staff. It returns
// once the message is durable.
func (c *Client) Send(ctx context.Context, id config.WorkstreamID, text string) (ConversationEntry, error) {
	var v ConversationEntry
	err := c.Do(ctx, "POST", Prefix+"/conversation/"+url.PathEscape(string(id)), SendRequest{Text: text}, &v)
	return v, err
}

// Conversation lists a workstream's conversation with its chief of staff.
func (c *Client) Conversation(ctx context.Context, id config.WorkstreamID) (ConversationResponse, error) {
	var v ConversationResponse
	err := c.Do(ctx, "GET", Prefix+"/conversation/"+url.PathEscape(string(id)), nil, &v)
	return v, err
}

// Inbox lists the escalations waiting for the owner's ruling.
func (c *Client) Inbox(ctx context.Context) (InboxResponse, error) {
	var v InboxResponse
	err := c.Do(ctx, "GET", Prefix+"/inbox", nil, &v)
	return v, err
}

// Answer records the owner's ruling on an inbox entry.
func (c *Client) Answer(ctx context.Context, number int, text string) (AnswerResponse, error) {
	var v AnswerResponse
	err := c.Do(ctx, "POST", Prefix+"/inbox/"+strconv.Itoa(number), AnswerRequest{Text: text}, &v)
	return v, err
}
