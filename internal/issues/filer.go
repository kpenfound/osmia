package issues

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/redact"
)

// Issue states GitHub reports.
const (
	StateOpen   = "open"
	StateClosed = "closed"
)

// Issue is an issue as the Filer reads it back.
type Issue struct {
	Number    int
	URL       string
	State     string // StateOpen or StateClosed
	Title     string
	Body      string
	Labels    []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Comment is an issue comment as the Filer reads it back.
type Comment struct {
	ID        int64
	URL       string
	Body      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewIssue is the text and labels of an issue to create.
type NewIssue struct {
	Title  string
	Body   string
	Labels []string
}

// Filer writes and reads issues and comments through the GitHub REST API for
// the error reporter. Every call names its repository (owner/name) and, where
// it concerns one issue, the issue number: nothing defaults either. Every call
// is recorded in the GitHub activity log and carries ghlog's reporter marker,
// so the reporter's own calls can be told apart from the service's other
// GitHub traffic.
//
// Filer does not retry. Its errors say whether the outcome of a call is
// uncertain: see Uncertain.
type Filer struct {
	BaseURL string // defaults to https://api.github.com
	Token   string // sent as a bearer token
	HTTP    *http.Client
	// PerPage is the page size of the list calls, at most 100. Zero means 100.
	PerPage int
}

// maxListPages bounds how many pages one list call reads.
const maxListPages = 100

// Error is a failed Filer call. It carries the HTTP status (0 when no response
// arrived) and a short reason, never the token or the request body.
type Error struct {
	// Op names the call, such as "create issue on owner/name".
	Op string
	// Status is the HTTP status, 0 when no response arrived.
	Status int
	// Reason is a short, single-line description of the failure.
	Reason string
	// uncertain marks a call whose effect on GitHub is unknown.
	uncertain bool
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s: GitHub answered %d: %s", e.Op, e.Status, e.Reason)
	}
	return fmt.Sprintf("%s: %s", e.Op, e.Reason)
}

// Uncertain reports whether a write may have been applied even though the call
// failed: no response arrived, GitHub answered 5xx or an unexpected status, or
// the answer to an accepted write could not be read. A 4xx answer is a
// definite rejection, as is a failure to send the request at all. A read has no
// effect to be unsure of, so its failure is always reported as a definite
// failure; callers use Status to tell a rejected read from a missing response.
func (e *Error) Uncertain() bool { return e.uncertain }

// Rejected reports whether GitHub answered with a 4xx status: the request was
// definitely not applied.
func (e *Error) Rejected() bool { return e.Status >= 400 && e.Status < 500 }

// IsUncertain reports whether err is a Filer error whose outcome is uncertain.
func IsUncertain(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Uncertain()
}

// IsRejected reports whether err is a Filer error for a 4xx answer.
func IsRejected(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Rejected()
}

// Create opens an issue in repository and returns it with its number and URL.
func (f Filer) Create(ctx context.Context, repository string, issue NewIssue) (Issue, error) {
	op := "create issue on " + repository
	if err := checkRepository(op, repository); err != nil {
		return Issue{}, err
	}
	labels := issue.Labels
	if labels == nil {
		labels = []string{}
	}
	in := map[string]any{"title": issue.Title, "body": issue.Body, "labels": labels}
	var created wireIssue
	path := "/repos/" + repository + "/issues"
	if err := f.do(ctx, call{action: "issues.create", access: ghlog.Write, op: op, repository: repository, method: http.MethodPost, path: path, want: http.StatusCreated, write: true}, in, &created); err != nil {
		return Issue{}, err
	}
	if created.Number < 1 {
		return Issue{}, &Error{Op: op, Status: http.StatusCreated, Reason: "invalid response", uncertain: true}
	}
	return created.issue(), nil
}

// Comment adds a comment to issue number of repository and returns it.
func (f Filer) Comment(ctx context.Context, repository string, number int, body string) (Comment, error) {
	op := fmt.Sprintf("comment on %s#%d", repository, number)
	if err := checkIssue(op, repository, number); err != nil {
		return Comment{}, err
	}
	var created wireComment
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", repository, number)
	if err := f.do(ctx, call{action: "issues.comment", access: ghlog.Write, op: op, repository: repository, method: http.MethodPost, path: path, want: http.StatusCreated, write: true}, map[string]any{"body": body}, &created); err != nil {
		return Comment{}, err
	}
	return created.comment(), nil
}

// State reads whether issue number of repository is open or closed, as
// StateOpen or StateClosed. A number that names a pull request is an error.
func (f Filer) State(ctx context.Context, repository string, number int) (string, error) {
	op := fmt.Sprintf("read state of %s#%d", repository, number)
	if err := checkIssue(op, repository, number); err != nil {
		return "", err
	}
	var issue wireIssue
	path := fmt.Sprintf("/repos/%s/issues/%d", repository, number)
	if err := f.do(ctx, call{action: "issues.state", access: ghlog.Read, op: op, repository: repository, method: http.MethodGet, path: path, want: http.StatusOK}, nil, &issue); err != nil {
		return "", err
	}
	if issue.PullRequest != nil {
		return "", &Error{Op: op, Status: http.StatusOK, Reason: "the number is a pull request, not an issue"}
	}
	switch issue.State {
	case StateOpen, StateClosed:
		return issue.State, nil
	}
	return "", &Error{Op: op, Status: http.StatusOK, Reason: "invalid response"}
}

// ListIssues lists every issue of repository, open and closed, updated at or
// after since, oldest page first. Pull requests are left out. With no labels
// it filters by nothing but time, so it returns issues whatever labels they
// carry; with labels, GitHub returns only issues carrying all of them. A zero
// since lists every issue.
func (f Filer) ListIssues(ctx context.Context, repository string, since time.Time, labels []string) ([]Issue, error) {
	op := "list issues of " + repository
	if err := checkRepository(op, repository); err != nil {
		return nil, err
	}
	query := url.Values{"state": {"all"}}
	if !since.IsZero() {
		query.Set("since", since.UTC().Format(time.RFC3339))
	}
	if len(labels) > 0 {
		query.Set("labels", strings.Join(labels, ","))
	}
	var out []Issue
	err := f.pages(ctx, call{action: "issues.list", access: ghlog.Read, op: op, repository: repository, method: http.MethodGet, path: "/repos/" + repository + "/issues", want: http.StatusOK}, query, func(data json.RawMessage) (int, error) {
		var page []wireIssue
		if err := json.Unmarshal(data, &page); err != nil {
			return 0, err
		}
		for _, w := range page {
			if w.PullRequest == nil {
				out = append(out, w.issue())
			}
		}
		return len(page), nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListComments lists the comments of issue number of repository created or
// updated at or after since. A zero since lists every comment.
func (f Filer) ListComments(ctx context.Context, repository string, number int, since time.Time) ([]Comment, error) {
	op := fmt.Sprintf("list comments of %s#%d", repository, number)
	if err := checkIssue(op, repository, number); err != nil {
		return nil, err
	}
	query := url.Values{}
	if !since.IsZero() {
		query.Set("since", since.UTC().Format(time.RFC3339))
	}
	var out []Comment
	err := f.pages(ctx, call{action: "issues.list_comments", access: ghlog.Read, op: op, repository: repository, method: http.MethodGet, path: fmt.Sprintf("/repos/%s/issues/%d/comments", repository, number), want: http.StatusOK}, query, func(data json.RawMessage) (int, error) {
		var page []wireComment
		if err := json.Unmarshal(data, &page); err != nil {
			return 0, err
		}
		for _, w := range page {
			out = append(out, w.comment())
		}
		return len(page), nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// wireIssue is an issue as GitHub's REST API sends it.
type wireIssue struct {
	Number      int             `json:"number"`
	HTMLURL     string          `json:"html_url"`
	State       string          `json:"state"`
	Title       string          `json:"title"`
	Body        *string         `json:"body"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	PullRequest json.RawMessage `json:"pull_request"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (w wireIssue) issue() Issue {
	out := Issue{Number: w.Number, URL: w.HTMLURL, State: w.State, Title: w.Title, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}
	if w.Body != nil {
		out.Body = *w.Body
	}
	for _, l := range w.Labels {
		out.Labels = append(out.Labels, l.Name)
	}
	return out
}

// wireComment is an issue comment as GitHub's REST API sends it.
type wireComment struct {
	ID        int64     `json:"id"`
	HTMLURL   string    `json:"html_url"`
	Body      *string   `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (w wireComment) comment() Comment {
	out := Comment{ID: w.ID, URL: w.HTMLURL, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}
	if w.Body != nil {
		out.Body = *w.Body
	}
	return out
}

// call describes one REST request for do: what to log and how to read the
// answer.
type call struct {
	action, access, op, repository, method, path string
	query                                        url.Values
	want                                         int
	// write marks a call whose effect on GitHub is unknown when it fails
	// without a definite rejection.
	write bool
}

// do performs one request, records one activity entry for it, and decodes the
// answer into out.
func (f Filer) do(ctx context.Context, c call, in, out any) error {
	data, err := f.send(ctx, c, in)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &Error{Op: c.op, Status: c.want, Reason: "invalid response", uncertain: c.write}
	}
	return nil
}

// pages reads a list endpoint page by page, passing each page's JSON to add,
// which returns the page's item count. It stops after a page shorter than the
// page size that announces no next page.
func (f Filer) pages(ctx context.Context, c call, query url.Values, add func(json.RawMessage) (int, error)) error {
	per := f.PerPage
	if per < 1 || per > 100 {
		per = 100
	}
	for page := 1; ; page++ {
		if page > maxListPages {
			return &Error{Op: c.op, Reason: "too many pages"}
		}
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("per_page", strconv.Itoa(per))
		q.Set("page", strconv.Itoa(page))
		c.query = q
		data, header, err := f.exchange(ctx, c, nil)
		if err != nil {
			return err
		}
		n, err := add(data)
		if err != nil {
			return &Error{Op: c.op, Status: c.want, Reason: "invalid response"}
		}
		if n < per && !hasNext(header) {
			return nil
		}
		if n == 0 {
			return nil
		}
	}
}

// hasNext reports whether a Link header announces a next page.
func hasNext(h http.Header) bool {
	for _, link := range h.Values("Link") {
		for _, part := range strings.Split(link, ",") {
			if strings.Contains(part, `rel="next"`) {
				return true
			}
		}
	}
	return false
}

// send is exchange without the headers.
func (f Filer) send(ctx context.Context, c call, in any) (json.RawMessage, error) {
	data, _, err := f.exchange(ctx, c, in)
	return data, err
}

// exchange performs one request and records its activity entry. The request is
// marked as the reporter's own through ctx. Errors never include the token or
// the request body.
func (f Filer) exchange(ctx context.Context, c call, in any) (json.RawMessage, http.Header, error) {
	ctx = ghlog.WithReporter(ctx)
	start := time.Now()
	data, header, status, err := f.request(ctx, c, in)
	ghlog.Log(ctx, start, ghlog.Entry{Action: c.action, Access: c.access, Repository: c.repository, Method: c.method, Path: c.path, Status: status, NoResponse: status == 0, Err: err})
	return data, header, err
}

func (f Filer) request(ctx context.Context, c call, in any) (json.RawMessage, http.Header, int, error) {
	base := f.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	var reader io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, nil, 0, &Error{Op: c.op, Reason: "the request could not be encoded"}
		}
		reader = bytes.NewReader(data)
	}
	target := strings.TrimSuffix(base, "/") + c.path
	if len(c.query) > 0 {
		target += "?" + c.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, c.method, target, reader)
	if err != nil {
		return nil, nil, 0, &Error{Op: c.op, Reason: "the request could not be built"}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if f.Token != "" {
		req.Header.Set("Authorization", "Bearer "+f.Token)
	}
	client := f.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, &Error{Op: c.op, Reason: "no response from GitHub", uncertain: c.write}
	}
	defer resp.Body.Close()
	if resp.StatusCode != c.want {
		return nil, nil, resp.StatusCode, f.statusError(c, resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil || len(data) > MaxResponse {
		return nil, nil, resp.StatusCode, &Error{Op: c.op, Status: resp.StatusCode, Reason: "unreadable or oversized response", uncertain: c.write}
	}
	return data, resp.Header, resp.StatusCode, nil
}

// maxReason bounds the GitHub message an Error keeps.
const maxReason = 200

// statusError describes an unexpected answer with its status and GitHub's
// short message. A write is uncertain unless the status is a 4xx rejection.
func (f Filer) statusError(c call, resp *http.Response) *Error {
	var body struct {
		Message string `json:"message"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(data, &body)
	reason := f.reasonText(body.Message)
	if reason == "" {
		reason = strings.ToLower(http.StatusText(resp.StatusCode))
	}
	if reason == "" {
		reason = "unexpected status"
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		reason += "; the rate limit is exhausted"
	}
	rejected := resp.StatusCode >= 400 && resp.StatusCode < 500
	return &Error{Op: c.op, Status: resp.StatusCode, Reason: reason, uncertain: c.write && !rejected}
}

// reasonText is one value from GitHub's answer on one line, bounded, with the
// token and every known secret removed.
func (f Filer) reasonText(s string) string {
	secrets := redact.KnownSecrets()
	if f.Token != "" {
		secrets = append(secrets, f.Token)
	}
	s = redact.Secrets(s, secrets...)
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
	if r := []rune(s); len(r) > maxReason {
		s = string(r[:maxReason]) + "…"
	}
	return s
}

// checkRepository rejects a repository that is not owner/name before any
// request is made.
func checkRepository(op, repository string) error {
	owner, repo, ok := strings.Cut(repository, "/")
	if !ok || !name.MatchString(owner) || !name.MatchString(repo) || owner == "." || owner == ".." || repo == "." || repo == ".." {
		return &Error{Op: op, Reason: "the repository must be owner/name"}
	}
	return nil
}

// checkIssue rejects a repository or issue number that is not usable before
// any request is made.
func checkIssue(op, repository string, number int) error {
	if err := checkRepository(op, repository); err != nil {
		return err
	}
	if number < 1 {
		return &Error{Op: op, Reason: "the issue number must be positive"}
	}
	return nil
}
