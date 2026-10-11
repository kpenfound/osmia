package thread

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

// resumeFallbackKey is the context key carrying fallbackAllowed into a
// CheckResume call, the only way an implementation reached through
// coreadapter.Turns can learn which role's turn is asking: the interface
// itself carries no scope. A CheckResume that already has its own way to
// verify a saved session, such as a fixture's fake engine or a real
// backend's own API, has no reason to consult it; it exists only for an
// implementation that would otherwise have no verification at all and
// wants to confine a heuristic trust fallback to the role thread.Runner's
// ResumableRoles grants it to.
type resumeFallbackKey struct{}

// withResumeFallbackAllowed attaches whether a CheckResume implementation
// may use a role-blind heuristic fallback for the turn ctx belongs to.
func withResumeFallbackAllowed(ctx context.Context, allowed bool) context.Context {
	return context.WithValue(ctx, resumeFallbackKey{}, allowed)
}

// ResumeFallbackAllowed reports whether ctx, as passed to a CheckResume
// call, authorizes a role-blind heuristic fallback. Every other caller gets
// false, including a direct call outside the runner's own CheckResume
// invocation.
func ResumeFallbackAllowed(ctx context.Context) bool {
	allowed, _ := ctx.Value(resumeFallbackKey{}).(bool)
	return allowed
}

// source finds the most recent completed turn whose session the next turn
// could continue from. It reports two distinct kinds of continuation
// separately: stopped reports a turn that was interrupted by an explicit
// Stop, whose exact in-flight session every role has always resumed so no
// work is lost (existing behaviour, unconditional on role); clean reports an
// ordinarily completed turn's session, which every role may try to resume,
// subject to whatever the configured Turns answers for CheckResume. Either
// kind returns healthy so a caller that does not need the distinction can
// still use it.
//
// A turn stopped by an explicit Stop before its session started leaves the
// source as it was: the turn before it, not this one, is still what the next
// turn may continue from (base behaviour, unconditional on role). A turn a
// restart abandoned unanswered, with no session and no explicit Stop, gets
// the same treatment, but only for a role resumableRoles grants: that is the
// one place session turns look past an interrupted turn to the session's
// last recorded model session, confined to the session role so every other
// role's source selection is exactly what it was before that role existed.
func source(t trace.Thread, before uint64, resumableRoles func(string) bool) (profile coreadapter.Profile, session coreadapter.BackendSession, boundary uint64, stopped, clean bool) {
	for _, q := range t.Turns {
		if q.Sequence >= before {
			break
		}
		cancelledBeforeSession := !q.CompletedAt.IsZero() && q.Response != nil && q.Response.Result.Cancelled && !coreadapter.ValidSession(q.Response.Result.Session)
		if cancelledBeforeSession && (q.Response.Stop != nil || resumableRoles != nil && resumableRoles(t.Identity.Role)) {
			continue
		}
		boundary = q.Sequence
		responded := q.Response != nil && !q.CompletedAt.IsZero()
		if !responded {
			stopped, clean = false, false
			continue
		}
		res := q.Response
		profile = q.Request.Profile
		if len(q.Attempts) > 0 {
			profile = q.Attempts[len(q.Attempts)-1].Profile
		}
		session = res.Result.Session
		// A stopped session did not fail, so the next turn resumes it.
		stopped = res.Stop != nil
		clean = !stopped && res.Failure == "" && !res.Result.IsError && !res.Result.Cancelled && !res.Result.TimedOut && res.Result.ExitCode == 0 && res.Result.Signal == 0
	}
	return profile, session, boundary, stopped, clean
}

// failed reports whether an attempt that was not stopped ended in failure.
func failed(result coreadapter.SessionResult, runErr error) bool {
	return runErr != nil || result.Cancelled || result.IsError || result.TimedOut || result.ExitCode != 0 || result.Signal != 0
}

// execute runs the turn's attempts. An infrastructure failure is retried on
// the same profile up to MaxRetries times, then on each fallback in turn with
// the same bound; a behavioural failure ends the turn. The agent session runs under the context
// Stoppable attached to ctx; a turn stopped before or during its session
// returns its Stop as runErr, and an attempt a stop ended records no failure.
func (r Runner) execute(ctx context.Context, t trace.Thread, q *trace.QueuedTurn, prepared coreadapter.PreparedTurn) (result coreadapter.SessionResult, runErr, persistErr error) {
	// Leases span every attempt of the turn and are released exactly once.
	cleanup, workspace, retain := prepared.Cleanup, prepared.WorkspaceLease, prepared.RetainWorkspace
	prepared.Cleanup, prepared.WorkspaceLease = nil, nil
	defer func() {
		for i := len(cleanup) - 1; i >= 0; i-- {
			if cleanup[i] != nil {
				persistErr = errors.Join(persistErr, cleanup[i].Release(context.WithoutCancel(ctx)))
			}
		}
		if workspace != nil && !retain && workspace.Lease != nil {
			persistErr = errors.Join(persistErr, workspace.Lease.Release(context.WithoutCancel(ctx)))
		}
	}()
	run := execution(ctx)
	if stop := stopped(run); stop != nil {
		result = coreadapter.SessionResult{SessionDirectory: q.Claim.SessionDirectory, StartedAt: r.Now(), Cancelled: true, ErrorSubtype: "stopped"}
		return result, stop, nil
	}
	// Keep workspace identity validation in the adapter without transferring release.
	if workspace != nil {
		copy := *workspace
		copy.Lease = nil
		prepared.WorkspaceLease = &copy
	}
	previous, session, boundary, stoppedSource, cleanSource := source(t, q.Sequence, r.ResumableRoles)
	history, from, omitted, err := Replay(t, q.Sequence, r.ReplayLimits)
	if err != nil {
		return result, nil, err
	}
	// attempts counts the attempts on the current profile; tried holds every
	// profile the turn has run on, so a fallback never returns to one.
	attempts, tried := 1, map[string]bool{prepared.Profile.Name: true}
	forceReplay := false
	reason, retry := "no compatible completed session", ""
	// Every role may try to resume a stopped or clean source: this is base
	// behaviour, unconditional on role, deciding only whether CheckResume is
	// asked at all. What CheckResume itself may answer is a separate matter:
	// fallbackAllowed below confines a role-blind heuristic fallback (used
	// when the configured Turns cannot verify resumability itself) to the
	// one role resumableRoles grants, so a role CheckResume already answers
	// for directly, the same way on base, keeps that exact answer.
	healthy := stoppedSource || cleanSource
	fallbackAllowed := r.ResumableRoles != nil && r.ResumableRoles(t.Identity.Role)
	for {
		prepared.Resume, prepared.History = nil, history
		path := "replay"
		if healthy && !forceReplay && coreadapter.ValidSession(session) && previous.Backend == prepared.Profile.Backend {
			if checker, ok := r.Turns.(coreadapter.ResumeChecker); ok {
				err := checker.CheckResume(withResumeFallbackAllowed(ctx, fallbackAllowed), previous, prepared.Profile, session)
				switch {
				case err == nil:
					prepared.Resume, prepared.History, path, reason = &session, "", "resume", "compatible available session"
				case errors.Is(err, coreadapter.ErrResumeUnavailable), errors.Is(err, coreadapter.ErrUnsupported):
					reason = err.Error()
				default:
					return result, nil, err
				}
			}
		}
		why := reason
		if retry != "" {
			why = retry + "; " + reason
		}
		a := trace.TurnAttempt{Number: len(q.Attempts) + 1, Profile: prepared.Profile, Path: path, Reason: why, SourceSession: session, SourceSequence: boundary, ReplayFrom: from, Omitted: omitted, At: r.Now()}
		if path == "resume" {
			a.ReplayFrom, a.Omitted = 0, 0
		}
		if err := r.Store.RecordAttempt(ctx, q.Request.Workstream, q.Request.AgentID, q.Request.TurnID, q.Claim.Token, a); err != nil {
			return result, nil, err
		}
		q.Attempts = append(q.Attempts, a)
		result, runErr = r.Turns.Run(run, prepared)
		if result.StartedAt.IsZero() {
			result.StartedAt = a.At
		}
		result.SessionDirectory = q.Claim.SessionDirectory
		if errors.Is(runErr, context.Canceled) {
			result.Cancelled = true
		}
		// A session that ended cleanly keeps its result even when a stop came
		// after it; one that ended otherwise was ended by the stop.
		var stop *Stop
		clean := runErr == nil && !result.Cancelled && !result.IsError && !result.TimedOut && result.ExitCode == 0 && result.Signal == 0
		if s := stopped(run); s != nil && !clean {
			stop, result.Cancelled, runErr = s, true, s
		}
		hadSession := result.Session.ID != ""
		// Empty or malformed session references are unavailable, never resume hints.
		if !coreadapter.ValidSession(result.Session) {
			result.Session = coreadapter.BackendSession{}
			if runErr == nil {
				if failed(result, nil) {
					runErr = fmt.Errorf("agent startup failed: %s", coreadapter.FailureReason(result))
				} else {
					runErr = fmt.Errorf("executor returned no valid session reference")
				}
			}
		}
		captured := result
		a.Result = &captured
		var decision coreadapter.RetryDecision
		limited := result.Limit != nil && result.Limit.Blocking()
		if stop == nil && (failed(result, runErr) || limited) {
			fallback := r.Fallbacks[prepared.Profile.Name]
			if limited {
				for fallback.Name != "" && fallback.Backend == prepared.Profile.Backend {
					fallback = r.Fallbacks[fallback.Name]
				}
			}
			if tried[fallback.Name] {
				fallback = coreadapter.Profile{}
			}
			decision, err = coreadapter.RetryAdapter{}.Decide(context.WithoutCancel(ctx), coreadapter.RetryRequest{Result: result, Err: runErr, Attempt: attempts, MaxRetries: r.MaxRetries, FallbackProfile: fallback.Name})
			if err != nil {
				return result, runErr, err
			}
			if limited {
				decision.Kind, decision.Retry, decision.FallbackProfile = coreadapter.Infrastructure, fallback.Name != "", fallback.Name
				decision.Reason = "provider usage limit"
			}
			a.FailureClass, a.Failure = decision.Kind, "the session failed: "+decision.Reason
			if runErr != nil {
				a.Failure = runErr.Error()
			}
		}
		q.Attempts[len(q.Attempts)-1] = a
		if err := r.Store.RecordAttempt(context.WithoutCancel(ctx), q.Request.Workstream, q.Request.AgentID, q.Request.TurnID, q.Claim.Token, a); err != nil {
			return result, runErr, err
		}
		if result.Limit != nil && result.Limit.Blocking() && r.OnProviderLimit != nil {
			if err := r.OnProviderLimit(prepared.Profile, *result.Limit); err != nil {
				return result, runErr, err
			}
		}
		if run.Err() != nil {
			break
		}
		// Explicit no-work errors must not be accompanied by evidence of execution.
		unstarted := result.FinalResponse == "" && result.Outcome == nil && result.Usage.Turns == 0 && result.Usage.CostUSD == 0 && !hadSession
		if unstarted && path == "resume" && errors.Is(runErr, coreadapter.ErrResumeUnavailable) {
			forceReplay, reason, retry = true, "resume rejected before work; replay", ""
			continue
		}
		if !decision.Retry {
			break
		}
		if decision.FallbackProfile == "" {
			attempts++
			retry = fmt.Sprintf("retry %d of %d on profile %s after an infrastructure failure: %s", attempts-1, r.MaxRetries, prepared.Profile.Name, decision.Reason)
			continue
		}
		fallback := r.Fallbacks[prepared.Profile.Name]
		if limited {
			for fallback.Name != "" && fallback.Backend == prepared.Profile.Backend {
				fallback = r.Fallbacks[fallback.Name]
			}
		}
		if fallback.Name == "" || fallback.Backend == "" {
			return result, runErr, fmt.Errorf("invalid fallback profile")
		}
		retry = fmt.Sprintf("fallback from profile %s to %s after %d infrastructure failures: %s", prepared.Profile.Name, fallback.Name, attempts, decision.Reason)
		prepared.Profile, forceReplay, reason = fallback, true, "a fallback profile replays the owned log"
		attempts, tried[fallback.Name] = 1, true
	}
	return result, runErr, nil
}
