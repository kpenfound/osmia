package errorreport

import "time"

// Config holds the plain inputs a decision depends on. The caller passes the
// loaded configuration and build identity in.
type Config struct {
	// Repository is the target repository as owner/name.
	Repository string
	// Labels are recorded with a create intent as a lookup hint.
	Labels []string
	// MaxIssuesPerHour, MaxIssuesPerDay and MaxCommentsPerDay cap the writes.
	// A cap that is not positive allows no write of its kind.
	MaxIssuesPerHour  int
	MaxIssuesPerDay   int
	MaxCommentsPerDay int
	// CheckInterval is the least time between reads of one saved issue's
	// state. Zero allows a read at every recurrence.
	CheckInterval time.Duration
	// Version and Commit are the Osmia build identity.
	Version string
	Commit  string
}

// Kind is what a Decision tells the caller to do.
type Kind string

const (
	// Reconcile: an intent for the target repository is unresolved. Look
	// for the write it may have produced, adopt it or write again, before
	// any new write. The occurrence was not decided and is offered again.
	Reconcile Kind = "reconcile"
	// OpenIssue: create a new issue; its intent is recorded.
	OpenIssue Kind = "open_issue"
	// OpenLinkedIssue: create a new issue that links the closed issue
	// Decision.Issue; its intent is recorded.
	OpenLinkedIssue Kind = "open_linked_issue"
	// Comment: post the daily comment on Decision.Issue; its intent is
	// recorded.
	Comment Kind = "comment"
	// Count: only the saved count and last-seen time change.
	Count Kind = "count"
	// OverCap: a cap named by Decision.Cap is spent; the event is counted
	// as skipped.
	OverCap Kind = "over_cap"
	// CheckState: read whether Decision.Issue is open or closed, then
	// decide again with the result in Input.Check. Nothing is recorded yet.
	CheckState Kind = "check_state"
	// AwaitCheck: the issue's state cannot be read yet; the recurrence is
	// recorded as awaiting a check.
	AwaitCheck Kind = "await_check"
	// Idle: nothing to do until Decision.NotBefore, if set.
	Idle Kind = "idle"
)

// Caps named by an OverCap decision.
const (
	CapIssuesPerHour  = "issues_per_hour"
	CapIssuesPerDay   = "issues_per_day"
	CapCommentsPerDay = "comments_per_day"
)

// Check is a freshly read state of a saved issue.
type Check struct {
	Repository string
	Issue      int
	State      IssueState
	At         time.Time
}

// Input is what is decided. Report is a new occurrence; a nil Report asks for
// the next recurrence that was awaiting a check. Check is the issue state the
// caller read after a CheckState decision.
type Input struct {
	Report *Report
	Check  *Check
}

// Decision is the outcome of Decide.
type Decision struct {
	Kind        Kind
	Repository  string
	Fingerprint string
	// Issue is the issue to read for CheckState, to comment on for Comment,
	// or the closed issue replaced for OpenLinkedIssue.
	Issue int
	// Cap names the spent cap of an OverCap decision.
	Cap string
	// Intent is the unresolved intent to reconcile for Reconcile, and the
	// intent to write, already recorded by Apply, for OpenIssue,
	// OpenLinkedIssue and Comment.
	Intent *Intent
	// NotBefore is the earliest time Decide is worth calling again for an
	// Idle decision; zero when only a new event can change the outcome.
	NotBefore time.Time

	record *IssueRecord
	await  *Awaiting
}

const dayKey = "2006-01-02"

// Decide says what to do with an occurrence, or with the next recurrence
// awaiting a check, given the saved state, the configuration and the current
// time. It does not change st; Apply records the outcome. It never yields a
// reopen, never uses an issue saved under another repository and ignores
// intents of other repositories.
func Decide(st *State, cfg Config, in Input, now time.Time) Decision {
	now = now.UTC()
	repo := cfg.Repository
	if it := st.firstIntent(repo); it != nil {
		cp := it.clone()
		return Decision{Kind: Reconcile, Repository: repo, Fingerprint: it.Fingerprint, Intent: &cp}
	}
	event := in.Report != nil
	var rep Report
	if event {
		rep = *in.Report
	} else {
		aw, wait := st.pickAwaiting(cfg, in.Check, now)
		if aw == nil {
			return Decision{Kind: Idle, Repository: repo, NotBefore: wait}
		}
		rep = st.Record(repo, aw.Fingerprint).Report
	}
	fp := rep.Fingerprint
	d := Decision{Repository: repo, Fingerprint: fp}
	next := IssueRecord{}
	if rec := st.Record(repo, fp); rec != nil {
		next = *rec
	}

	checked := in.Check != nil && sameRepo(in.Check.Repository, repo) && next.Issue != 0 && in.Check.Issue == next.Issue &&
		(in.Check.State == IssueOpen || in.Check.State == IssueClosed)
	if next.Issue != 0 && !checked && next.State != IssueClosed && !checkAllowed(next, cfg, now) {
		next.occur(rep, event)
		d.Kind, d.Issue, d.record = AwaitCheck, next.Issue, &next
		if event {
			d.await = &Awaiting{Repository: repo, Fingerprint: fp, Issue: next.Issue, At: rep.Time.UTC()}
		}
		return d
	}
	if next.Issue != 0 && !checked && next.State != IssueClosed {
		return Decision{Kind: CheckState, Repository: repo, Fingerprint: fp, Issue: next.Issue}
	}
	next.occur(rep, event)
	if checked {
		next.State = in.Check.State
		next.CheckedAt = in.Check.At.UTC()
		if next.CheckedAt.IsZero() {
			next.CheckedAt = now
		}
	}
	if next.Issue != 0 && next.State == IssueClosed {
		next.roll()
	}
	day := now.Format(dayKey)
	if next.Issue == 0 {
		if spent := st.issueCap(cfg, now); spent != "" {
			next.SkippedCaps++
			d.Kind, d.Cap, d.record = OverCap, spent, &next
			return d
		}
		d.Kind, d.Issue = OpenIssue, next.Previous
		if next.Previous > 0 {
			d.Kind = OpenLinkedIssue
		}
		filing := st.filing(cfg, rep, next)
		filing.Previous = next.Previous
		d.Intent = &Intent{Kind: IntentCreate, Repository: repo, Fingerprint: fp, Day: day, RecordedAt: now,
			Labels: append([]string(nil), cfg.Labels...), Filing: filing}
		d.record = &next
		return d
	}
	next.Pending, next.PendingSince = 0, time.Time{}
	d.record, d.Issue = &next, next.Issue
	switch {
	case next.LastCommentDay == day:
		d.Kind = Count
	case cfg.MaxCommentsPerDay <= 0 || st.commentsOn(day) >= cfg.MaxCommentsPerDay:
		next.SkippedCaps++
		d.Kind, d.Cap = OverCap, CapCommentsPerDay
	default:
		d.Kind = Comment
		filing := st.filing(cfg, rep, next)
		filing.Day = day
		d.Intent = &Intent{Kind: IntentComment, Repository: repo, Fingerprint: fp, Issue: next.Issue, Day: day,
			RecordedAt: now, Filing: filing}
	}
	return d
}

// Apply records a decision made by Decide on the same state: counts, the
// write intent, the cap windows and the awaiting check. The caller saves the
// state before performing the write an intent describes.
func (st *State) Apply(d Decision) {
	switch d.Kind {
	case Reconcile, CheckState, Idle:
		return
	}
	if d.record != nil {
		st.setRecord(d.Repository, d.Fingerprint, *d.record)
	}
	switch d.Kind {
	case OpenIssue, OpenLinkedIssue:
		cutoff := d.Intent.RecordedAt.Add(-24 * time.Hour)
		kept := st.IssueTimes[:0]
		for _, t := range st.IssueTimes {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		st.IssueTimes = append(kept, d.Intent.RecordedAt)
		st.Intents = append(st.Intents, d.Intent.clone())
	case Comment:
		if st.CommentDay != d.Intent.Day {
			st.CommentDay, st.CommentsToday = d.Intent.Day, 0
		}
		st.CommentsToday++
		st.Intents = append(st.Intents, d.Intent.clone())
	case AwaitCheck:
		if d.await != nil {
			found := false
			for _, aw := range st.Awaiting {
				if sameRepo(aw.Repository, d.await.Repository) && aw.Fingerprint == d.await.Fingerprint && aw.Issue == d.await.Issue {
					found = true
				}
			}
			if !found {
				st.Awaiting = append(st.Awaiting, *d.await)
			}
		}
	}
	if d.Kind != AwaitCheck {
		kept := st.Awaiting[:0]
		for _, aw := range st.Awaiting {
			if !(sameRepo(aw.Repository, d.Repository) && aw.Fingerprint == d.Fingerprint) {
				kept = append(kept, aw)
			}
		}
		st.Awaiting = kept
	}
	st.pruneAwaiting()
}

func (r *IssueRecord) occur(rep Report, event bool) {
	if !event {
		return
	}
	r.Count++
	r.Pending++
	if r.Pending == 1 || r.PendingSince.IsZero() {
		r.PendingSince = rep.Time.UTC()
	}
	if r.FirstSeen.IsZero() {
		r.FirstSeen = rep.Time.UTC()
	}
	if rep.Time.After(r.LastSeen) {
		r.LastSeen = rep.Time.UTC()
	}
	r.Report = rep
}

func (r *IssueRecord) roll() {
	r.Previous = r.Issue
	r.Issue = 0
	r.State = ""
	r.CheckedAt = time.Time{}
	r.Count = max(1, r.Pending)
	if !r.PendingSince.IsZero() {
		r.FirstSeen = r.PendingSince
	}
	r.Pending, r.PendingSince = 0, time.Time{}
	r.LastCommentDay = ""
}

func checkAllowed(r IssueRecord, cfg Config, now time.Time) bool {
	return cfg.CheckInterval <= 0 || r.CheckedAt.IsZero() || !now.Before(r.CheckedAt.Add(cfg.CheckInterval))
}

func (st *State) pickAwaiting(cfg Config, check *Check, now time.Time) (*Awaiting, time.Time) {
	var wait time.Time
	for i := range st.Awaiting {
		aw := &st.Awaiting[i]
		if !sameRepo(aw.Repository, cfg.Repository) {
			continue
		}
		rec := st.Record(cfg.Repository, aw.Fingerprint)
		if rec == nil || rec.Issue != aw.Issue || aw.Issue == 0 {
			continue
		}
		if check != nil && sameRepo(check.Repository, cfg.Repository) && check.Issue == aw.Issue {
			return aw, time.Time{}
		}
		if checkAllowed(*rec, cfg, now) {
			return aw, time.Time{}
		}
		if at := rec.CheckedAt.Add(cfg.CheckInterval); wait.IsZero() || at.Before(wait) {
			wait = at
		}
	}
	return nil, wait
}

func (st *State) issueCap(cfg Config, now time.Time) string {
	hour, day := 0, 0
	for _, t := range st.IssueTimes {
		if t.After(now.Add(-time.Hour)) {
			hour++
		}
		if t.After(now.Add(-24 * time.Hour)) {
			day++
		}
	}
	switch {
	case cfg.MaxIssuesPerHour <= 0 || hour >= cfg.MaxIssuesPerHour:
		return CapIssuesPerHour
	case cfg.MaxIssuesPerDay <= 0 || day >= cfg.MaxIssuesPerDay:
		return CapIssuesPerDay
	}
	return ""
}

func (st *State) commentsOn(day string) int {
	if st.CommentDay != day {
		return 0
	}
	return st.CommentsToday
}

func (st *State) filing(cfg Config, rep Report, rec IssueRecord) Filing {
	return Filing{
		Report:         rep,
		Repository:     cfg.Repository,
		Version:        cfg.Version,
		Commit:         cfg.Commit,
		FirstSeen:      rec.FirstSeen,
		LastSeen:       rec.LastSeen,
		Count:          rec.Count,
		SkippedCaps:    rec.SkippedCaps,
		SkippedDropped: st.QueueDrops,
	}
}
