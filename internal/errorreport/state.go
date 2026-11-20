package errorreport

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IssueState is the last known state of a saved issue.
type IssueState string

const (
	IssueOpen   IssueState = "open"
	IssueClosed IssueState = "closed"
)

// IntentKind says which write an intent covers.
type IntentKind string

const (
	IntentCreate  IntentKind = "create"
	IntentComment IntentKind = "comment"
)

const stateVersion = 1

// State is everything the reporter keeps between runs. Issues and their
// counts are scoped by repository; an issue number is only ever meaningful
// together with its repository. It is not safe for concurrent use.
type State struct {
	Version int `json:"version"`
	// Repositories holds the saved issues by repository (see RepoKey).
	Repositories map[string]*RepoState `json:"repositories,omitempty"`
	// IssueTimes are the times issue writes were recorded in the last day.
	IssueTimes []time.Time `json:"issue_times,omitempty"`
	// CommentDay and CommentsToday are the UTC day of the comment window
	// and the comment writes recorded in it.
	CommentDay    string `json:"comment_day,omitempty"`
	CommentsToday int    `json:"comments_today,omitempty"`
	// QueueDrops counts events lost from a full queue that no filing has
	// reported yet.
	QueueDrops int `json:"queue_drops,omitempty"`
	// Intents are the writes recorded before they were attempted and not
	// yet resolved.
	Intents []Intent `json:"intents,omitempty"`
	// Awaiting are recurrences whose saved issue's state has not been read.
	Awaiting []Awaiting `json:"awaiting,omitempty"`
}

// RepoState holds the saved issues of one repository by fingerprint.
type RepoState struct {
	Issues map[string]*IssueRecord `json:"issues,omitempty"`
}

// IssueRecord is what is saved for one fingerprint in one repository.
type IssueRecord struct {
	// Issue is the saved issue's number in this repository; zero while no
	// issue has been opened for the current occurrences.
	Issue int `json:"issue,omitempty"`
	// State is the last known state of Issue, and CheckedAt when it, or an
	// attempt to read it, was last made.
	State     IssueState `json:"state,omitempty"`
	CheckedAt time.Time  `json:"checked_at,omitzero"`
	// Previous is the closed issue the current or next issue replaces.
	Previous int `json:"previous,omitempty"`
	// Count, FirstSeen and LastSeen describe the occurrences of the current
	// or next issue.
	Count     int       `json:"count,omitempty"`
	FirstSeen time.Time `json:"first_seen,omitzero"`
	LastSeen  time.Time `json:"last_seen,omitzero"`
	// Pending counts occurrences since Issue was last read as open, and
	// PendingSince is the first of them.
	Pending      int       `json:"pending,omitempty"`
	PendingSince time.Time `json:"pending_since,omitzero"`
	// LastCommentDay is the UTC day of the last comment on Issue.
	LastCommentDay string `json:"last_comment_day,omitempty"`
	// SkippedCaps counts events over a cap since the last filing.
	SkippedCaps int `json:"skipped_caps,omitempty"`
	// Report is the latest report for the fingerprint.
	Report Report `json:"report"`
}

// Intent records a write before it is attempted. It holds everything needed
// to repeat or to recognise the write.
type Intent struct {
	Kind        IntentKind `json:"kind"`
	Repository  string     `json:"repository"`
	Fingerprint string     `json:"fingerprint"`
	// Issue is the issue a comment is for.
	Issue int `json:"issue,omitempty"`
	// Day is the UTC day of the write.
	Day        string    `json:"day"`
	RecordedAt time.Time `json:"recorded_at"`
	Attempts   int       `json:"attempts"`
	// Labels are the labels sent with a create. They are a lookup hint
	// only: finding the write never depends on them.
	Labels []string `json:"labels,omitempty"`
	// Filing is the content of the write.
	Filing Filing `json:"filing"`
}

// Awaiting is a recurrence waiting for the state of its saved issue to be
// read.
type Awaiting struct {
	Repository  string    `json:"repository"`
	Fingerprint string    `json:"fingerprint"`
	Issue       int       `json:"issue"`
	At          time.Time `json:"at"`
}

// ClockSkew is the margin to subtract from an intent's time when looking for
// the issue or comment it may have produced.
const ClockSkew = 5 * time.Minute

// Since is the time from which a lookup for the intent's write must look: any
// issue or comment created after it may be the write.
func (in Intent) Since() time.Time { return in.RecordedAt.Add(-ClockSkew) }

// CreatedByIntent reports whether something created at createdAt can be the
// write the intent recorded.
func (in Intent) CreatedByIntent(createdAt time.Time) bool { return !createdAt.Before(in.Since()) }

// RepoKey is the form of a repository name that state is keyed by.
func RepoKey(repository string) string { return strings.ToLower(strings.TrimSpace(repository)) }

func sameRepo(a, b string) bool { return RepoKey(a) == RepoKey(b) }

// NewState returns an empty state.
func NewState() *State { return &State{Version: stateVersion} }

// LoadState reads the state saved at path. A missing file is an empty state.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewState(), nil
	}
	if err != nil {
		return nil, err
	}
	st := &State{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("read error report state %s: %w", path, err)
	}
	if st.Version != stateVersion {
		return nil, fmt.Errorf("error report state %s has unsupported version %d", path, st.Version)
	}
	return st, nil
}

// Save writes the state to path atomically: through a synced temporary file
// beside it, so a reader sees the old contents or the new ones.
func (st *State) Save(path string) error {
	st.Version = stateVersion
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileSynced(path, append(data, '\n'))
}

func writeFileSynced(path string, data []byte) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"-"+rand.Text()+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Record returns the saved record for fingerprint in repository, or nil. The
// record is looked up under that repository only.
func (st *State) Record(repository, fingerprint string) *IssueRecord {
	rs := st.Repositories[RepoKey(repository)]
	if rs == nil {
		return nil
	}
	return rs.Issues[fingerprint]
}

func (st *State) setRecord(repository, fingerprint string, rec IssueRecord) {
	if st.Repositories == nil {
		st.Repositories = map[string]*RepoState{}
	}
	rs := st.Repositories[RepoKey(repository)]
	if rs == nil {
		rs = &RepoState{Issues: map[string]*IssueRecord{}}
		st.Repositories[RepoKey(repository)] = rs
	}
	if rs.Issues == nil {
		rs.Issues = map[string]*IssueRecord{}
	}
	rs.Issues[fingerprint] = &rec
}

// IntentsFor returns the unresolved intents recorded for repository.
func (st *State) IntentsFor(repository string) []Intent {
	var out []Intent
	for _, in := range st.Intents {
		if sameRepo(in.Repository, repository) {
			out = append(out, in.clone())
		}
	}
	return out
}

func (st *State) firstIntent(repository string) *Intent {
	for i := range st.Intents {
		if sameRepo(st.Intents[i].Repository, repository) {
			return &st.Intents[i]
		}
	}
	return nil
}

func (in Intent) clone() Intent {
	in.Labels = append([]string(nil), in.Labels...)
	return in
}

func (in Intent) same(o Intent) bool {
	return in.Kind == o.Kind && sameRepo(in.Repository, o.Repository) && in.Fingerprint == o.Fingerprint && in.Day == o.Day
}

func (st *State) removeIntent(in Intent) bool {
	for i := range st.Intents {
		if st.Intents[i].same(in) {
			st.Intents = append(st.Intents[:i], st.Intents[i+1:]...)
			return true
		}
	}
	return false
}

// AwaitingFor returns the recurrences awaiting a state check in repository.
func (st *State) AwaitingFor(repository string) []Awaiting {
	var out []Awaiting
	for _, aw := range st.Awaiting {
		if sameRepo(aw.Repository, repository) {
			out = append(out, aw)
		}
	}
	return out
}

// NoteAttempt counts one more attempt of the intent's write or of reconciling
// it, and returns the new count. It returns zero if the intent is not saved.
func (st *State) NoteAttempt(in Intent) int {
	for i := range st.Intents {
		if st.Intents[i].same(in) {
			st.Intents[i].Attempts++
			return st.Intents[i].Attempts
		}
	}
	return 0
}

// CompleteCreate records that the intent's issue exists as issue number in the
// intent's repository, whether the reporter created it or adopted it after
// reconciling, and clears the intent. The reported skipped counts are cleared.
func (st *State) CompleteCreate(in Intent, issue int, at time.Time) error {
	if in.Kind != IntentCreate || issue <= 0 {
		return errors.New("not a create intent or no issue number")
	}
	if !st.removeIntent(in) {
		return errors.New("create intent is not saved")
	}
	rec := IssueRecord{}
	if cur := st.Record(in.Repository, in.Fingerprint); cur != nil {
		rec = *cur
	}
	rec.Issue = issue
	rec.State = IssueOpen
	rec.CheckedAt = at.UTC()
	rec.Pending, rec.PendingSince = 0, time.Time{}
	rec.LastCommentDay = ""
	rec.SkippedCaps = max(0, rec.SkippedCaps-in.Filing.SkippedCaps)
	st.QueueDrops = max(0, st.QueueDrops-in.Filing.SkippedDropped)
	st.setRecord(in.Repository, in.Fingerprint, rec)
	st.pruneAwaiting()
	return nil
}

// CompleteComment records that the intent's comment exists, whether the
// reporter posted it or adopted it after reconciling, and clears the intent.
func (st *State) CompleteComment(in Intent) error {
	if in.Kind != IntentComment {
		return errors.New("not a comment intent")
	}
	if !st.removeIntent(in) {
		return errors.New("comment intent is not saved")
	}
	if rec := st.Record(in.Repository, in.Fingerprint); rec != nil {
		next := *rec
		next.LastCommentDay = in.Day
		next.SkippedCaps = max(0, next.SkippedCaps-in.Filing.SkippedCaps)
		st.setRecord(in.Repository, in.Fingerprint, next)
	}
	st.QueueDrops = max(0, st.QueueDrops-in.Filing.SkippedDropped)
	return nil
}

// AbandonIntent clears an intent whose write was given up, so the next
// occurrence is decided afresh.
func (st *State) AbandonIntent(in Intent) { st.removeIntent(in) }

// NoteQueueDrops adds events dropped from a full queue, to be reported by the
// next filing.
func (st *State) NoteQueueDrops(n int) {
	if n > 0 {
		st.QueueDrops += n
	}
}

// NoteCheckAttempt records that the state of the fingerprint's saved issue was
// read, or that reading it failed, at time at, which spaces the next read.
func (st *State) NoteCheckAttempt(repository, fingerprint string, at time.Time) {
	if rec := st.Record(repository, fingerprint); rec != nil {
		next := *rec
		next.CheckedAt = at.UTC()
		st.setRecord(repository, fingerprint, next)
	}
}

// pruneAwaiting drops awaiting entries whose saved issue is gone or changed.
func (st *State) pruneAwaiting() {
	kept := st.Awaiting[:0]
	for _, aw := range st.Awaiting {
		if rec := st.Record(aw.Repository, aw.Fingerprint); rec != nil && rec.Issue == aw.Issue && aw.Issue != 0 {
			kept = append(kept, aw)
		}
	}
	st.Awaiting = kept
}
