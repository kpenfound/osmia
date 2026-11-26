package thread

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/trace"
)

type resumeFallbackKey struct{}

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

func source(t trace.Thread, before uint64, resumableRoles func(string) bool) (profile coreadapter.Profile, session coreadapter.BackendSession, boundary uint64, stopped, clean bool) {
	for _, q := range t.Turns {
		if q.Sequence >= before {
			break
		}
		if q.Sequence < t.Epoch {
			continue
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
		stopped = res.Stop != nil
		clean = !stopped && res.Failure == "" && !res.Result.IsError && !res.Result.Cancelled && !res.Result.TimedOut && res.Result.ExitCode == 0 && res.Result.Signal == 0
	}
	return profile, session, boundary, stopped, clean
}

// Resumes reports whether q, the next turn of t, would resume the thread's
// saved session, and the client that holds that session. A turn resumes when
// the thread's last session ended cleanly or at a stop, is valid and was
// started by the same backend as q's main profile. resumable is the Runner's
// ResumableRoles. Whether the backend still accepts the session is only known
// when the turn runs; a turn that cannot resume replays its log wherever it
// runs.
func Resumes(t trace.Thread, q trace.QueuedTurn, resumable func(string) bool) (client string, ok bool) {
	previous, session, _, stopped, clean := source(t, q.Sequence, resumable)
	if !(stopped || clean) || !coreadapter.ValidSession(session) || previous.Backend != q.Request.Profile.Backend {
		return "", false
	}
	return t.SessionClient()
}

func failed(result coreadapter.SessionResult, runErr error) bool {
	return runErr != nil || result.Cancelled || result.IsError || result.TimedOut || result.ExitCode != 0 || result.Signal != 0
}

func (r Runner) execute(ctx context.Context, t trace.Thread, q *trace.QueuedTurn, prepared coreadapter.PreparedTurn) (result coreadapter.SessionResult, runErr, persistErr error) {
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
	var placed Placed
	if r.Place != nil {
		var err error
		if placed, err = r.Place(run, t, *q); err != nil {
			result = coreadapter.SessionResult{SessionDirectory: q.Claim.SessionDirectory, StartedAt: r.Now(), Cancelled: errors.Is(err, context.Canceled), IsError: true, ErrorSubtype: "unplaced"}
			return result, err, nil
		}
		if placed.Release != nil {
			defer placed.Release()
		}
	}
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
	current := prepared.SystemPrompt
	baseline, baselined := "", false
	if coreadapter.ValidSession(session) {
		baseline, baselined = sessionBaseline(t, q.Sequence, session, r.LegacyBaseline)
	}
	attempts, tried := 1, map[string]bool{prepared.Profile.Name: true}
	forceReplay := false
	reason, retry := "no compatible completed session", ""
	if held, ok := t.SessionClient(); ok && placed.Client != "" && held != placed.Client && coreadapter.ValidSession(session) {
		forceReplay, reason = true, "the thread's session is on client "+held+" and the turn runs on "+placed.Client+"; replay the owned log"
	}
	skipped := map[string]bool{}
	skip := func(profile string) {
		if skipped[profile] {
			return
		}
		skipped[profile] = true
		event := trace.ClientEvent{Kind: trace.ClientSkipped, Turn: q.Request.TurnID, Thread: q.Request.ThreadID, Client: placed.Client, What: profile, At: r.Now().UTC()}
		if _, err := r.Store.RecordClientEvent(context.WithoutCancel(ctx), q.Request.Workstream, q.Request.Unit, len(skipped), event); err != nil {
			ghlog.Logger(ctx).Warn("cannot record a skipped fallback profile", "turn", q.Request.TurnID, "profile", profile, "error", err.Error())
		}
	}
	healthy := stoppedSource || cleanSource
	fallbackAllowed := r.ResumableRoles != nil && r.ResumableRoles(t.Identity.Role)
	for {
		prepared.Resume, prepared.History, prepared.SystemPrompt = nil, history, current
		path := "replay"
		if healthy && !forceReplay && coreadapter.ValidSession(session) && previous.Backend == prepared.Profile.Backend {
			if !baselined {
				reason = "the system prompt the saved session began with is not on record; replay"
			} else if checker, ok := r.Turns.(coreadapter.ResumeChecker); ok {
				err := checker.CheckResume(withResumeFallbackAllowed(ctx, fallbackAllowed), previous, prepared.Profile, session)
				switch {
				case err == nil:
					prepared.Resume, prepared.History, prepared.SystemPrompt, path, reason = &session, "", baseline, "resume", "compatible available session"
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
		sent := prepared.SystemPrompt
		a := trace.TurnAttempt{Number: len(q.Attempts) + 1, Profile: prepared.Profile, Path: path, Reason: why, SourceSession: session, SourceSequence: boundary, ReplayFrom: from, Omitted: omitted, SystemPrompt: &sent, At: r.Now()}
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
		var stop *Stop
		clean := runErr == nil && !result.Cancelled && !result.IsError && !result.TimedOut && result.ExitCode == 0 && result.Signal == 0
		if s := stopped(run); s != nil && !clean {
			stop, result.Cancelled, runErr = s, true, s
		}
		hadSession := result.Session.ID != ""
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
			fallback := r.fallback(prepared.Profile, limited, placed, skip)
			registry := limited && result.Limit.Kind == coreadapter.RegistryLimitKind
			if registry {
				fallback = coreadapter.Profile{}
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
				if registry {
					decision.Reason = "Docker Hub's pull rate limit"
				}
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
		fallback := r.fallback(prepared.Profile, limited, placed, skip)
		if fallback.Name == "" || fallback.Backend == "" {
			return result, runErr, fmt.Errorf("invalid fallback profile")
		}
		retry = fmt.Sprintf("fallback from profile %s to %s after %d infrastructure failures: %s", prepared.Profile.Name, fallback.Name, attempts, decision.Reason)
		prepared.Profile, forceReplay, reason = fallback, true, "a fallback profile replays the owned log"
		attempts, tried[fallback.Name] = 1, true
	}
	return result, runErr, nil
}

func (r Runner) fallback(profile coreadapter.Profile, limited bool, placed Placed, skip func(string)) coreadapter.Profile {
	fallback := r.Fallbacks[profile.Name]
	seen := map[string]bool{profile.Name: true}
	for fallback.Name != "" && !seen[fallback.Name] {
		seen[fallback.Name] = true
		switch {
		case limited && fallback.Backend == profile.Backend:
		case placed.Runs != nil && !placed.Runs(fallback.Name):
			skip(fallback.Name)
		default:
			return fallback
		}
		fallback = r.Fallbacks[fallback.Name]
	}
	return coreadapter.Profile{}
}
