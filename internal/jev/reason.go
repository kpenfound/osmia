package jev

import (
	"context"
	"errors"
	"time"

	"github.com/kpenfound/osmia/internal/systemone"
)

// Provider answers System One requests. The service's provider is a
// systemone.Client; tests use jevtest.
type Provider interface {
	Evaluate(context.Context, systemone.Request) (systemone.Response, error)
}

// Reason says why a judgment fell back instead of using Jev's answers.
type Reason string

const (
	// ReasonDisabled is a judgment asked while the boost is off. It is
	// neither sent nor recorded.
	ReasonDisabled Reason = "disabled"
	// ReasonUnconfigured is an enabled boost whose API key is not set.
	ReasonUnconfigured Reason = "unconfigured"
	// ReasonCoolingDown is a judgment not sent while the provider recovers
	// from rate limits or an outage.
	ReasonCoolingDown Reason = "cooling_down"
	ReasonTimeout     Reason = "timeout"
	ReasonRateLimited Reason = "rate_limited"
	// ReasonUnavailable is a network failure or a server error.
	ReasonUnavailable Reason = "unavailable"
	// ReasonRejected is a request refused, before sending or by the
	// provider: bad credentials, an invalid or oversized request.
	ReasonRejected Reason = "rejected"
	// ReasonUnusable is a response that does not answer the questions in
	// the shape they require.
	ReasonUnusable Reason = "unusable"
	// ReasonDeclined is a valid response the judgment's own acceptance,
	// such as a confidence threshold, did not accept.
	ReasonDeclined Reason = "declined"
	// ReasonInterrupted is a judgment whose earlier attempts were each
	// interrupted before a result was recorded.
	ReasonInterrupted Reason = "interrupted"
	// ReasonUnrecorded is a judgment whose trace record could not be
	// written or read; its answers are not used.
	ReasonUnrecorded Reason = "unrecorded"
	// ReasonCancelled is a judgment whose turn ended while it ran.
	ReasonCancelled Reason = "cancelled"
)

// transient reports whether another attempt may succeed shortly.
func (r Reason) transient() bool {
	return r == ReasonTimeout || r == ReasonUnavailable
}

// failure is a provider error in the judgment's terms.
type failure struct {
	reason     Reason
	retryAfter time.Duration
	detail     string
}

// failed translates a provider error. An error that is not a
// *systemone.Error is an unavailable provider.
func failed(err error) *failure {
	var e *systemone.Error
	if !errors.As(err, &e) {
		return &failure{reason: ReasonUnavailable, detail: err.Error()}
	}
	f := &failure{retryAfter: e.RetryAfter, detail: e.Message}
	switch e.Kind {
	case systemone.KindTimeout:
		f.reason = ReasonTimeout
	case systemone.KindRateLimited:
		f.reason = ReasonRateLimited
	case systemone.KindCancelled:
		f.reason = ReasonCancelled
	case systemone.KindMalformed:
		f.reason = ReasonUnusable
	case systemone.KindInvalidRequest, systemone.KindUnauthorized, systemone.KindRejected:
		f.reason = ReasonRejected
	default:
		f.reason = ReasonUnavailable
	}
	return f
}
