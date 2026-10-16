package jev

import (
	corejev "github.com/kpenfound/agentcore/jev"
)

// Provider answers System One requests. The service's provider is a
// systemone.Client; tests use agentcore's jevtest.
type Provider = corejev.Provider

// Reason says why a judgment fell back instead of using Jev's answers.
type Reason = corejev.Reason

const (
	// ReasonDisabled is a judgment asked while the boost is off. It is
	// neither sent nor recorded.
	ReasonDisabled = corejev.ReasonDisabled
	// ReasonUnconfigured is an enabled boost whose API key is not set.
	ReasonUnconfigured = corejev.ReasonUnconfigured
	// ReasonCoolingDown is a judgment not sent while the provider recovers
	// from rate limits or an outage.
	ReasonCoolingDown = corejev.ReasonCoolingDown
	ReasonTimeout     = corejev.ReasonTimeout
	ReasonRateLimited = corejev.ReasonRateLimited
	// ReasonUnavailable is a network failure or a server error.
	ReasonUnavailable = corejev.ReasonUnavailable
	// ReasonRejected is a request refused, before sending or by the
	// provider: bad credentials, an invalid or oversized request.
	ReasonRejected = corejev.ReasonRejected
	// ReasonUnusable is a response that does not answer the questions in
	// the shape they require.
	ReasonUnusable = corejev.ReasonUnusable
	// ReasonCancelled is a judgment whose turn ended while it ran.
	ReasonCancelled = corejev.ReasonCancelled
)

const (
	// ReasonDeclined is a valid response the judgment's own acceptance,
	// such as a confidence threshold, did not accept.
	ReasonDeclined Reason = "declined"
	// ReasonInterrupted is a judgment whose earlier attempts were each
	// interrupted before a result was recorded.
	ReasonInterrupted Reason = "interrupted"
	// ReasonUnrecorded is a judgment whose trace record could not be
	// written or read; its answers are not used.
	ReasonUnrecorded Reason = "unrecorded"
)
