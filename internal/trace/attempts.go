package trace

import (
	"context"
	"fmt"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// TurnAttempt records intent before launch and evidence before any safe retry.
// An absent Result means execution is ambiguous, including after a restart.
type TurnAttempt struct {
	Number         int                        `json:"number"`
	Profile        coreadapter.Profile        `json:"profile"`
	Path           string                     `json:"path"`
	Reason         string                     `json:"reason"`
	SourceSession  coreadapter.BackendSession `json:"source_session"`
	SourceSequence uint64                     `json:"source_sequence"`
	ReplayFrom     uint64                     `json:"replay_from"`
	Omitted        int                        `json:"omitted"`
	At             time.Time                  `json:"at"`
	Result         *coreadapter.SessionResult `json:"result,omitempty"`
	Failure        string                     `json:"failure,omitempty"`
	FailureClass   coreadapter.FailureKind    `json:"failure_class,omitempty"`
}

// RecordAttempt appends a new intent or captures the exact current attempt.
// It never grants permission to relaunch an existing intent.
func (r *Repository) RecordAttempt(ctx context.Context, stream config.WorkstreamID, agent, turn, token string, a TurnAttempt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	log, _, err := r.loadWorkflow(stream)
	if err != nil {
		return err
	}
	t, ok := log.Threads[agent]
	if !ok {
		return ErrClaim
	}
	for i, q := range t.Turns {
		if q.Request.TurnID != turn {
			continue
		}
		if q.Claim == nil || q.Claim.Token != token || q.Claim.ServiceSession != r.session || t.Active != turn || q.Response != nil {
			return ErrClaim
		}
		n := len(q.Attempts)
		switch {
		case a.Number == n+1 && a.Result == nil:
			if n > 0 && q.Attempts[n-1].Result == nil {
				return ErrClaim
			}
			q.Attempts = append(q.Attempts, a)
		case a.Number == n && n > 0:
			old := q.Attempts[n-1]
			if equalJSON(old, a) {
				return nil
			}
			if old.Result != nil || a.Result == nil {
				return ErrConflict
			}
			intent := a
			intent.Result, intent.Failure, intent.FailureClass = nil, "", ""
			if !equalJSON(old, intent) {
				return ErrConflict
			}
			q.Attempts[n-1] = a
		default:
			return ErrConflict
		}
		t.Turns[i] = q
		log.Threads[agent] = t
		return r.saveThread(ctx, stream, log)
	}
	return ErrClaim
}

func validateAttempts(q QueuedTurn) error {
	for i, a := range q.Attempts {
		if (i == 0 && a.Profile != q.Request.Profile) || (i > 0 && a.At.Before(q.Attempts[i-1].At)) {
			return fmt.Errorf("invalid attempt profile or chronology")
		}
		if q.Claim == nil || a.Number != i+1 || a.At.Before(q.Claim.At) || a.At.IsZero() ||
			!present(a.Profile.Name) || !present(a.Profile.Backend) ||
			(a.Path != "resume" && a.Path != "replay") || !present(a.Reason) ||
			a.SourceSequence >= q.Sequence || a.ReplayFrom > a.SourceSequence || a.Omitted < 0 ||
			(a.Path == "resume" && !coreadapter.ValidSession(a.SourceSession)) ||
			(i < len(q.Attempts)-1 && a.Result == nil) ||
			(a.Result == nil && (a.Failure != "" || a.FailureClass != "")) ||
			(a.FailureClass != "" && (a.Failure == "" || a.FailureClass != coreadapter.Infrastructure && a.FailureClass != coreadapter.Behavioural)) {
			return fmt.Errorf("invalid turn attempt")
		}
		if a.Result != nil && a.Result.Session.Backend != "" && a.Result.Session.Backend != a.Profile.Backend {
			return fmt.Errorf("attempt backend differs from profile")
		}
	}
	if q.Response != nil && len(q.Attempts) > 0 {
		last := q.Attempts[len(q.Attempts)-1]
		if last.Result == nil || !equalJSON(*last.Result, q.Response.Result) || last.Failure != q.Response.Failure || last.FailureClass != q.Response.FailureClass {
			return fmt.Errorf("response differs from final attempt")
		}
	}
	return nil
}
