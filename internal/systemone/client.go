package systemone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is TypeSafe's own API.
const DefaultBaseURL = "https://api.typesafe.ai"

// ErrorKind says why a request produced no usable response.
type ErrorKind string

const (
	// KindInvalidRequest is a request refused before sending: it breaks the
	// API's limits, or the client has no API key.
	KindInvalidRequest ErrorKind = "invalid_request"
	// KindUnauthorized is an API key the endpoint refused.
	KindUnauthorized ErrorKind = "unauthorized"
	// KindRejected is any other request the endpoint refused, such as an
	// invalid or oversized one, or a redirect.
	KindRejected    ErrorKind = "rejected"
	KindRateLimited ErrorKind = "rate_limited"
	// KindUnavailable is a network failure or a server error.
	KindUnavailable ErrorKind = "unavailable"
	KindTimeout     ErrorKind = "timeout"
	KindCancelled   ErrorKind = "cancelled"
	// KindMalformed is a response that does not answer the questions in the
	// shape they require.
	KindMalformed ErrorKind = "malformed_response"
)

// Error is a failed request. Message never contains the API key. RetryAfter
// is the endpoint's requested delay, if any.
type Error struct {
	Kind       ErrorKind
	StatusCode int
	RetryAfter time.Duration
	Message    string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Kind)
	}
	return string(e.Kind) + ": " + e.Message
}

// maxResponseBytes bounds a response body.
const maxResponseBytes = 1 << 20

// Client calls a TypeSafe-compatible API at BaseURL, an empty BaseURL being
// DefaultBaseURL. Requests go to <BaseURL>/v1/systemone with APIKey as the
// bearer credential, which never follows a redirect. A nil HTTPClient uses
// http.DefaultClient's transport.
type Client struct {
	BaseURL, Model, APIKey string
	HTTPClient             *http.Client
}

// Evaluate asks the model r's questions and validates the answers. Every
// error is an *Error. The context bounds the request; Evaluate does not
// retry.
func (c Client) Evaluate(ctx context.Context, r Request) (Response, error) {
	if c.APIKey == "" {
		return Response{}, &Error{Kind: KindInvalidRequest, Message: "API key is not set"}
	}
	if err := r.Validate(); err != nil {
		return Response{}, &Error{Kind: KindInvalidRequest, Message: err.Error()}
	}
	body, err := r.Body(c.Model)
	if err != nil {
		return Response{}, &Error{Kind: KindInvalidRequest, Message: err.Error()}
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(base, "/")+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return Response{}, &Error{Kind: KindInvalidRequest, Message: "invalid base URL"}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return Response{}, &Error{Kind: KindTimeout, Message: "request deadline passed"}
			}
			return Response{}, &Error{Kind: KindCancelled, Message: "request cancelled"}
		}
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return Response{}, &Error{Kind: KindTimeout, Message: "request timed out"}
		}
		return Response{}, &Error{Kind: KindUnavailable, Message: "request failed"}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, &Error{Kind: KindTimeout, StatusCode: resp.StatusCode, Message: "response deadline passed"}
		}
		return Response{}, &Error{Kind: KindUnavailable, StatusCode: resp.StatusCode, Message: "response could not be read"}
	}
	if resp.StatusCode != http.StatusOK {
		e := &Error{StatusCode: resp.StatusCode, Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529:
			e.Kind = KindRateLimited
			e.RetryAfter = retryAfter(resp.Header.Get("Retry-After"), time.Now())
		case resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout:
			e.Kind = KindUnavailable
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			e.Kind = KindUnauthorized
		default:
			e.Kind = KindRejected
		}
		return Response{}, e
	}
	if len(data) > maxResponseBytes {
		return Response{}, &Error{Kind: KindMalformed, StatusCode: resp.StatusCode, Message: "response is too large"}
	}
	out, err := decode(data, r)
	if err != nil {
		return Response{}, &Error{Kind: KindMalformed, StatusCode: resp.StatusCode, Message: err.Error()}
	}
	return out, nil
}

// retryAfter reads a Retry-After header in seconds or as an HTTP date.
func retryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
