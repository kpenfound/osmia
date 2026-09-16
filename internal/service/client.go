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

// HandIn submits work to a project. The service answers every hand-in with an
// error: the charter gate's, or unsupported once the gate passes.
func (c *Client) HandIn(ctx context.Context, req HandInRequest) error {
	return c.Do(ctx, "POST", Prefix+"/handin", req, nil)
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
