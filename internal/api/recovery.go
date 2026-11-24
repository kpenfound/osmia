package api

import (
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// The origins of a recovery move: a stall that neither Jev nor the chief of
// staff cleared, a move of the chief of staff's that belongs to the owner, and
// a move Jev judged would change what the owner approved.
const (
	RecoveryStall  = "stall"
	RecoveryOwner  = "owner"
	RecoveryIntent = "intent"
)

// RecoveryApprove is the answer of a recovery card.
const RecoveryApprove = "approve"

// RecoveryMove is the exact move a recovery card pins: the subject the move
// changes, the state and version it was read at, the target it enters and why.
// Occurrence names the router occurrence the card was opened for. A move whose
// subject has since changed state is stale: it is refused, never applied.
type RecoveryMove struct {
	Occurrence string `json:"occurrence" description:"The router occurrence the card was opened for: subject, version, trigger and cause."`
	Machine    string `json:"machine" description:"The state machine of the subject: unit, ws, final or dep."`
	Subject    string `json:"subject" description:"The workflow subject the move changes."`
	Unit       string `json:"unit,omitempty"`
	From       string `json:"from" description:"The state the subject was in when the card was opened."`
	Version    uint64 `json:"version" description:"The version of that state; the move applies only while the subject is still at it."`
	Target     string `json:"target" description:"The state the move enters."`
	Origin     string `json:"origin" enum:"stall,owner,intent"`
	Reason     string `json:"reason"`
}

// RecoveryRequest is the owner's approval of a recovery card: the pinned move
// the card shows, which is the card's answer body. A card whose subject has
// changed state since, or that was approved or closed, is refused as a
// conflict.
type RecoveryRequest struct {
	Occurrence string `json:"occurrence"`
	Subject    string `json:"subject"`
	Unit       string `json:"unit,omitempty"`
	From       string `json:"from"`
	Version    uint64 `json:"version"`
	Target     string `json:"target"`
}

// RecoveryResponse reports an approved recovery move: the router applied it as
// a pinned owner decision.
type RecoveryResponse struct {
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream"`
	Occurrence string              `json:"occurrence"`
	Subject    string              `json:"subject"`
	Unit       string              `json:"unit,omitempty"`
	From       string              `json:"from"`
	Target     string              `json:"target"`
	Detail     string              `json:"detail"`
	At         time.Time           `json:"at"`
}

// FeedRecovery is a recovery card in a workstream's feed: the pinned move and
// where the card stands. Occurrence identifies it while State progresses from
// open to approved or stale.
type FeedRecovery struct {
	RecoveryMove
	State string `json:"state" enum:"open,approved,stale"`
}
