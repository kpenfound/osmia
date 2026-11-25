package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/kpenfound/osmia/internal/api"
)

const (
	callTimeout   = 30 * time.Second
	resultTimeout = 2 * time.Minute
)

// refusal is an answer the server gave with an error status.
type refusal struct {
	Status int
	Err    api.APIError
}

func (r *refusal) Error() string {
	return fmt.Sprintf("the server answered %d: %s", r.Status, r.Err.Error())
}

func refusedWith(err error, code api.Code) bool {
	var r *refusal
	return errors.As(err, &r) && r.Err.Code == code
}

// transient reports whether a failed call may succeed when repeated: the
// server could not be reached or said it could not answer now.
func transient(err error) bool {
	var r *refusal
	if errors.As(err, &r) {
		return r.Status >= 500 || r.Status == http.StatusTooManyRequests || r.Err.Code == api.Unavailable
	}
	return err != nil
}

func readRefusal(resp *http.Response) error {
	r := &refusal{Status: resp.StatusCode}
	var body api.ErrorResponse
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body) == nil && body.Error.Code != "" {
		r.Err = body.Error
	} else {
		r.Err = api.APIError{Code: api.Unavailable, Message: http.StatusText(resp.StatusCode)}
	}
	return r
}

// post sends in as JSON to path on the server and decodes the answer into out.
// A successful answer counts as contact with the server.
func (c *Client) post(ctx context.Context, path string, in, out any, timeout time.Duration) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return readRefusal(resp)
	}
	c.touch()
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func decodeJSON(r io.Reader, out any) error { return json.NewDecoder(r).Decode(out) }
