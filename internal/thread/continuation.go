package thread

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

func source(t trace.Thread, before uint64) (coreadapter.Profile, coreadapter.BackendSession, uint64, bool) {
	var profile coreadapter.Profile
	var session coreadapter.BackendSession
	var boundary uint64
	healthy := false
	for _, q := range t.Turns {
		if q.Sequence >= before {
			break
		}
		// A turn stopped before its session started leaves the source as it was.
		if !q.CompletedAt.IsZero() && q.Response != nil && q.Response.Stop != nil && !coreadapter.ValidSession(q.Response.Result.Session) {
			continue
		}
		boundary = q.Sequence
		healthy = q.Response != nil && !q.CompletedAt.IsZero()
		if !healthy {
			continue
		}
		res := q.Response
		profile = q.Request.Profile
		if len(q.Attempts) > 0 {
			profile = q.Attempts[len(q.Attempts)-1].Profile
		}
		session = res.Result.Session
		// A stopped session did not fail, so the next turn resumes it.
		healthy = res.Stop != nil || res.Failure == "" && !res.Result.IsError && !res.Result.Cancelled && !res.Result.TimedOut && res.Result.ExitCode == 0 && res.Result.Signal == 0
	}
	return profile, session, boundary, healthy
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
	previous, session, boundary, healthy := source(t, q.Sequence)
	history, from, omitted, err := Replay(t, q.Sequence, r.ReplayLimits)
	if err != nil {
		return result, nil, err
	}
	// attempts counts the attempts on the current profile; tried holds every
	// profile the turn has run on, so a fallback never returns to one.
	attempts, tried := 1, map[string]bool{prepared.Profile.Name: true}
	forceReplay := false
	reason, retry := "no compatible completed session", ""
	for {
		prepared.Resume, prepared.History = nil, history
		path := "replay"
		if healthy && !forceReplay && coreadapter.ValidSession(session) && previous.Backend == prepared.Profile.Backend {
			if checker, ok := r.Turns.(coreadapter.ResumeChecker); ok {
				err := checker.CheckResume(ctx, previous, prepared.Profile, session)
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
				runErr = fmt.Errorf("executor returned no valid session reference")
			}
		}
		captured := result
		a.Result = &captured
		var decision coreadapter.RetryDecision
		if stop == nil && failed(result, runErr) {
			fallback := r.Fallbacks[prepared.Profile.Name]
			if tried[fallback.Name] {
				fallback = coreadapter.Profile{}
			}
			decision, err = coreadapter.RetryAdapter{}.Decide(context.WithoutCancel(ctx), coreadapter.RetryRequest{Result: result, Err: runErr, Attempt: attempts, MaxRetries: r.MaxRetries, FallbackProfile: fallback.Name})
			if err != nil {
				return result, runErr, err
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
		if fallback.Name == "" || fallback.Backend == "" {
			return result, runErr, fmt.Errorf("invalid fallback profile")
		}
		retry = fmt.Sprintf("fallback from profile %s to %s after %d infrastructure failures: %s", prepared.Profile.Name, fallback.Name, attempts, decision.Reason)
		prepared.Profile, forceReplay, reason = fallback, true, "a fallback profile replays the owned log"
		attempts, tried[fallback.Name] = 1, true
	}
	return result, runErr, nil
}
