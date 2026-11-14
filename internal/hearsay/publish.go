package hearsay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/redact"
)

// MaxRequestBytes is the largest hooks request body Hearsay accepts.
const MaxRequestBytes = 4 << 20

// The request kinds of Hearsay's work source.
const (
	KindProject    = "project"
	KindWorkstream = "workstream"
	KindUpdate     = "update"
	KindDocument   = "document"
	KindDecision   = "decision"
)

// The tiers of a record. A pass posts lower tiers first, so a record is held
// before anything that names it: the project, then its workstreams, then
// documents, decisions and updates.
const (
	TierProject = iota
	TierWorkstream
	TierRecord
)

// Repo declares one repository on a project: the Hearsay source that holds
// it and the repository's project container.
type Repo struct {
	Source  string `json:"source"`
	Project string `json:"project"`
}

// Wire is one hooks request body, in the field spelling of Hearsay's work
// connector contract. Every field is optional here and omitted when empty;
// each record kind sets only the fields the contract accepts for it, because
// Hearsay refuses unknown and misplaced fields. Times are RFC 3339.
type Wire struct {
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	Workstream  string `json:"workstream,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	State       string `json:"state,omitempty"`
	Status      string `json:"status,omitempty"`
	Repos       []Repo `json:"repos,omitempty"`
	URL         string `json:"url,omitempty"`
	Author      string `json:"author,omitempty"`

	Document string `json:"document,omitempty"`
	Body     string `json:"body,omitempty"`

	Decision        string   `json:"decision,omitempty"`
	Context         string   `json:"context,omitempty"`
	Outcome         string   `json:"outcome,omitempty"`
	Rationale       string   `json:"rationale,omitempty"`
	RejectedOptions []string `json:"rejected_options,omitempty"`
	Decider         string   `json:"decider,omitempty"`
	Supersedes      string   `json:"supersedes,omitempty"`
	SpecDocument    string   `json:"spec_document,omitempty"`
	PullRequest     string   `json:"pull_request,omitempty"`
	Question        string   `json:"question,omitempty"`

	Update string `json:"update,omitempty"`
	Text   string `json:"text,omitempty"`

	Deleted   bool   `json:"deleted,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// Digest identifies the content of a request: everything it carries except
// its update time, which only orders revisions.
func (w Wire) Digest() string {
	w.UpdatedAt = ""
	data, _ := json.Marshal(w)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Record is one artifact the publisher keeps posted. Source is the hooks
// source id it is posted to and Key a stable name for it within that source.
// Versions are the revisions to post, oldest first; the last is current.
// After names records (by Ref) that must be held first: a record is not
// posted in a pass in which one of them failed.
type Record struct {
	Source   string   `json:"source"`
	Key      string   `json:"key"`
	Tier     int      `json:"tier"`
	After    []string `json:"after,omitempty"`
	Versions []Wire   `json:"versions"`
}

// Ref names a record across sources.
func (r Record) Ref() string { return Ref(r.Source, r.Key) }

// Ref is the name of a record by its source and key.
func Ref(source, key string) string { return source + "/" + key }

// Posted is what was last posted successfully for one record.
type Posted struct {
	Digest    string
	UpdatedAt time.Time
}

// Entry is a Posted for one record, to be kept by the caller.
type Entry struct {
	Source, Key string
	Posted
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,128}$`)

// ValidID reports whether s fits Hearsay's id grammar for projects,
// workstreams, documents, decisions and updates: 1 to 128 bytes of letters,
// digits, `-`, `_`, `.` and `/`.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// StatusError is a hooks response other than 202 or 204.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("hearsay hooks returned HTTP %d", e.Code)
	}
	return fmt.Sprintf("hearsay hooks returned HTTP %d: %s", e.Code, e.Body)
}

// Poster sends hooks requests as the publisher agent. Its token is the
// publisher's own and is used for nothing else.
type Poster struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// Post sends one request to the hooks endpoint of source. A nil error means
// Hearsay answered 202 (written or already held) or 204. A *StatusError is
// Hearsay's refusal; any other error means it was not reached.
func (p Poster) Post(ctx context.Context, source string, w Wire) error {
	if !ValidID(source) {
		return errors.New("invalid hooks source")
	}
	data, err := json.Marshal(w)
	if err != nil {
		return err
	}
	if len(data) > MaxRequestBytes {
		return &StatusError{Code: http.StatusRequestEntityTooLarge, Body: "request exceeds the 4 MiB limit"}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	target := strings.TrimRight(p.URL, "/") + "/hooks/" + url.PathEscape(source)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid hooks endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.Token)
	client := http.Client{}
	if p.HTTP != nil {
		client = *p.HTTP
	}
	// A redirected post must never carry the publisher's token elsewhere.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return errors.New("hearsay hooks are unreachable")
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	switch response.StatusCode {
	case http.StatusAccepted, http.StatusNoContent:
		return nil
	}
	return &StatusError{Code: response.StatusCode, Body: redact.Secrets(strings.TrimSpace(string(body)), p.Token)}
}

// Failure is a record whose post was refused.
type Failure struct {
	Record string
	Err    error
}

// Result is the outcome of one reconcile pass.
type Result struct {
	// Entries are the records posted this pass, with what to keep for each.
	// They are returned even when the pass stopped early.
	Entries []Entry
	// Posts counts accepted requests.
	Posts int
	// Failures are refusals and skipped records; Unreachable is set when the
	// pass stopped because the hooks endpoint could not be reached.
	Failures    []Failure
	Unreachable bool
}

// Reconcile posts what changed. For each record it posts the versions after
// the last one posted, oldest first: all of them for a record never posted,
// only the current one when the posted digest is no longer among them. A
// record whose current digest equals the posted one sends nothing. Records are
// posted in tier order, and one named by After is skipped for the pass when
// its parent was refused. Reaching nothing stops the pass: the remaining
// records are left unposted for a later one.
func Reconcile(ctx context.Context, poster Poster, records []Record, posted map[string]Posted) Result {
	var res Result
	ordered := slices.Clone(records)
	slices.SortStableFunc(ordered, func(a, b Record) int { return a.Tier - b.Tier })
	failed := map[string]bool{}
	for _, rec := range ordered {
		if ctx.Err() != nil {
			return res
		}
		if slices.ContainsFunc(rec.After, func(ref string) bool { return failed[ref] }) {
			failed[rec.Ref()] = true
			res.Failures = append(res.Failures, Failure{rec.Ref(), errors.New("its parent was not posted")})
			continue
		}
		prev := posted[rec.Ref()]
		if prev.Digest == "" {
			// A cleared record is posted afresh, with the times it first had.
			prev = Posted{}
		}
		for _, w := range pending(rec.Versions, prev.Digest) {
			w.UpdatedAt = updatedAt(w, prev.UpdatedAt)
			if err := poster.Post(ctx, rec.Source, w); err != nil {
				failed[rec.Ref()] = true
				res.Failures = append(res.Failures, Failure{rec.Ref(), err})
				var status *StatusError
				if !errors.As(err, &status) {
					res.Unreachable = ctx.Err() == nil
					return res
				}
				break
			}
			updated, _ := time.Parse(time.RFC3339Nano, w.UpdatedAt)
			prev = Posted{Digest: w.Digest(), UpdatedAt: updated}
			res.Posts++
			res.Entries = append(res.Entries, Entry{rec.Source, rec.Key, prev})
		}
	}
	return res
}

// Unposted narrows rec to the versions a pass would still post given what was
// last posted for it. It reports false when the record's current content is
// already posted, so nothing of it remains to keep.
func Unposted(rec Record, posted Posted) (Record, bool) {
	versions := pending(rec.Versions, posted.Digest)
	if len(versions) == 0 {
		return Record{}, false
	}
	rec.Versions = slices.Clone(versions)
	return rec, true
}

// Current is the digest of the record's current revision, or empty for a
// record with none.
func (r Record) Current() string {
	if len(r.Versions) == 0 {
		return ""
	}
	return r.Versions[len(r.Versions)-1].Digest()
}

// pending selects the versions to post: those after the last one whose
// digest equals posted, everything when nothing was posted, and only the
// current version when the posted digest is not among them. Consecutive
// versions with the same content count once.
func pending(versions []Wire, posted string) []Wire {
	var unique []Wire
	for _, w := range versions {
		if len(unique) == 0 || unique[len(unique)-1].Digest() != w.Digest() {
			unique = append(unique, w)
		}
	}
	if len(unique) == 0 {
		return nil
	}
	if posted == "" {
		return unique
	}
	for i := len(unique) - 1; i >= 0; i-- {
		if unique[i].Digest() == posted {
			return unique[i+1:]
		}
	}
	return unique[len(unique)-1:]
}

// updatedAt is the update time a post carries: its own, kept after the
// record's creation and strictly after the previous post's, so a newer
// revision is never mistaken for a stale one.
func updatedAt(w Wire, previous time.Time) string {
	at, err := time.Parse(time.RFC3339Nano, w.UpdatedAt)
	if created, cerr := time.Parse(time.RFC3339Nano, w.CreatedAt); cerr == nil && (err != nil || at.Before(created)) {
		at = created
	}
	if !previous.IsZero() && !at.After(previous) {
		at = previous.Add(time.Nanosecond)
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// Time formats t as a Hearsay timestamp.
func Time(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
