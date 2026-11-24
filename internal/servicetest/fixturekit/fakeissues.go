package fixturekit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// IssueClock is a settable clock for the fake GitHub and the service under
// test, so a test decides which UTC day an error occurs on.
type IssueClock struct {
	mu sync.Mutex
	at time.Time
}

// NewIssueClock starts a clock at at.
func NewIssueClock(at time.Time) *IssueClock { return &IssueClock{at: at.UTC()} }

// Now reads the clock.
func (c *IssueClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// Advance moves the clock forward by d.
func (c *IssueClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// FakeIssue is an issue the fake GitHub holds.
type FakeIssue struct {
	Number      int
	Title       string
	Body        string
	Labels      []string
	State       string
	PullRequest bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// FakeComment is an issue comment the fake GitHub holds.
type FakeComment struct {
	ID        int64
	Issue     int
	Body      string
	CreatedAt time.Time
}

// IssueRequest is one request the fake GitHub received.
type IssueRequest struct {
	Method string
	Path   string
	Query  string
	Body   string
	Auth   string
	At     time.Time
}

const (
	FaultCreate       = "create"
	FaultComment      = "comment"
	FaultListIssues   = "list_issues"
	FaultListComments = "list_comments"
	FaultState        = "state"
)

// FaultEffect says what a Fault does to a request.
type FaultEffect int

const (
	DropAfterApply FaultEffect = iota
	HoldAfterApply
	StatusAfterApply
	StatusBeforeApply
	HoldBeforeServe
)

// Fault scripts the fake GitHub's answer to matching requests. Times counts
// how many requests it affects: zero means one, a negative number means every
// request until Clear.
type Fault struct {
	Kind    string
	Repo    string
	Effect  FaultEffect
	Status  int
	Times   int
	Applied chan struct{}
	Release chan struct{}

	used int
}

// FakeIssueServer stands in for the GitHub REST API's issue endpoints, whose
// real behaviour it does not reproduce: it holds issues and comments per
// repository, numbers issues per repository, answers the reads and writes the
// error reporter makes, records every request, and can be scripted to lose a
// response, fail, report issues closed or change an issue's labels.
type FakeIssueServer struct {
	Server *httptest.Server
	Now    func() time.Time

	mu       sync.Mutex
	repos    map[string]*fakeRepo
	requests []IssueRequest
	faults   []*Fault
	nextID   int64
}

type fakeRepo struct {
	next     int
	issues   []*FakeIssue
	comments []*FakeComment
}

// NewFakeIssueServer starts a fake GitHub that accepts any bearer token.
func NewFakeIssueServer(t *testing.T) *FakeIssueServer {
	t.Helper()
	g := &FakeIssueServer{Now: time.Now, repos: map[string]*fakeRepo{}}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Server.Close)
	return g
}

// URL is the base URL of the fake.
func (g *FakeIssueServer) URL() string { return g.Server.URL }

// Client is an HTTP client for the fake.
func (g *FakeIssueServer) Client() *http.Client { return g.Server.Client() }

func (g *FakeIssueServer) repo(name string) *fakeRepo {
	key := strings.ToLower(name)
	r := g.repos[key]
	if r == nil {
		r = &fakeRepo{}
		g.repos[key] = r
	}
	return r
}

// Seed adds an unrelated open issue to repository and returns its number.
func (g *FakeIssueServer) Seed(repository, title, body string, labels ...string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.add(repository, title, body, labels).Number
}

func (g *FakeIssueServer) add(repository, title, body string, labels []string) *FakeIssue {
	r := g.repo(repository)
	r.next++
	now := g.Now().UTC()
	issue := &FakeIssue{Number: r.next, Title: title, Body: body, Labels: slices.Clone(labels), State: "open", CreatedAt: now, UpdatedAt: now}
	r.issues = append(r.issues, issue)
	return issue
}

// Close reports the issue closed.
func (g *FakeIssueServer) Close(repository string, number int) {
	g.change(repository, number, func(i *FakeIssue) { i.State = "closed" })
}

// SetLabels replaces the labels of the issue.
func (g *FakeIssueServer) SetLabels(repository string, number int, labels ...string) {
	g.change(repository, number, func(i *FakeIssue) { i.Labels = slices.Clone(labels) })
}

func (g *FakeIssueServer) change(repository string, number int, edit func(*FakeIssue)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, i := range g.repo(repository).issues {
		if i.Number == number {
			edit(i)
			i.UpdatedAt = g.Now().UTC()
		}
	}
}

// Issues returns a copy of the issues of repository, oldest first.
func (g *FakeIssueServer) Issues(repository string) []FakeIssue {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []FakeIssue
	for _, i := range g.repo(repository).issues {
		cp := *i
		cp.Labels = slices.Clone(i.Labels)
		out = append(out, cp)
	}
	return out
}

// Comments returns a copy of the comments of issue number of repository.
func (g *FakeIssueServer) Comments(repository string, number int) []FakeComment {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []FakeComment
	for _, c := range g.repo(repository).comments {
		if c.Issue == number {
			out = append(out, *c)
		}
	}
	return out
}

// Requests returns every request received so far.
func (g *FakeIssueServer) Requests() []IssueRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests)
}

// Count is the number of requests received with method whose path starts with
// prefix.
func (g *FakeIssueServer) Count(method, prefix string) int {
	n := 0
	for _, r := range g.Requests() {
		if r.Method == method && strings.HasPrefix(r.Path, prefix) {
			n++
		}
	}
	return n
}

// Inject scripts a fault and returns it.
func (g *FakeIssueServer) Inject(f Fault) *Fault {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.faults = append(g.faults, &f)
	return &f
}

// Clear removes every scripted fault.
func (g *FakeIssueServer) Clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.faults = nil
}

func (g *FakeIssueServer) fault(kind, repository string) *Fault {
	for _, f := range g.faults {
		if f.Kind != kind || (f.Repo != "" && !strings.EqualFold(f.Repo, repository)) {
			continue
		}
		limit := f.Times
		if limit == 0 {
			limit = 1
		}
		if limit > 0 && f.used >= limit {
			continue
		}
		f.used++
		return f
	}
	return nil
}

func (g *FakeIssueServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	g.requests = append(g.requests, IssueRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body), Auth: r.Header.Get("Authorization"), At: time.Now()})
	g.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		reply(w, http.StatusUnauthorized, map[string]string{"message": "Requires authentication"})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "repos" || parts[3] != "issues" {
		reply(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	repository := parts[1] + "/" + parts[2]
	switch {
	case len(parts) == 4 && r.Method == http.MethodPost:
		g.create(w, r, repository, body)
	case len(parts) == 4 && r.Method == http.MethodGet:
		g.list(w, r, repository)
	case len(parts) == 5 && r.Method == http.MethodGet:
		g.state(w, r, repository, parts[4])
	case len(parts) == 6 && parts[5] == "comments" && r.Method == http.MethodPost:
		g.comment(w, r, repository, parts[4], body)
	case len(parts) == 6 && parts[5] == "comments" && r.Method == http.MethodGet:
		g.listComments(w, r, repository, parts[4])
	default:
		reply(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func (g *FakeIssueServer) finish(w http.ResponseWriter, r *http.Request, f *Fault) bool {
	if f == nil {
		return false
	}
	switch f.Effect {
	case DropAfterApply:
		drop(w)
	case HoldAfterApply:
		if f.Applied != nil {
			f.Applied <- struct{}{}
		}
		select {
		case <-f.Release:
		case <-r.Context().Done():
		}
		drop(w)
	case StatusAfterApply:
		reply(w, f.Status, map[string]string{"message": "scripted failure"})
	default:
		return false
	}
	return true
}

func (g *FakeIssueServer) before(w http.ResponseWriter, r *http.Request, kind, repository string) (*Fault, bool) {
	g.mu.Lock()
	f := g.fault(kind, repository)
	g.mu.Unlock()
	if f != nil && f.Effect == HoldBeforeServe {
		if f.Applied != nil {
			f.Applied <- struct{}{}
		}
		select {
		case <-f.Release:
		case <-r.Context().Done():
			drop(w)
			return f, true
		}
		return nil, false
	}
	if f != nil && f.Effect == StatusBeforeApply {
		reply(w, f.Status, map[string]string{"message": "scripted failure"})
		return f, true
	}
	return f, false
}

func (g *FakeIssueServer) create(w http.ResponseWriter, r *http.Request, repository string, body []byte) {
	f, done := g.before(w, r, FaultCreate, repository)
	if done {
		return
	}
	var in struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if json.Unmarshal(body, &in) != nil {
		reply(w, http.StatusUnprocessableEntity, map[string]string{"message": "Problems parsing JSON"})
		return
	}
	g.mu.Lock()
	issue := g.add(repository, in.Title, in.Body, in.Labels)
	wire := wireIssue(*issue)
	g.mu.Unlock()
	if g.finish(w, r, f) {
		return
	}
	reply(w, http.StatusCreated, wire)
}

func (g *FakeIssueServer) comment(w http.ResponseWriter, r *http.Request, repository, number string, body []byte) {
	f, done := g.before(w, r, FaultComment, repository)
	if done {
		return
	}
	n, _ := strconv.Atoi(number)
	var in struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(body, &in) != nil {
		reply(w, http.StatusUnprocessableEntity, map[string]string{"message": "Problems parsing JSON"})
		return
	}
	g.mu.Lock()
	issue := g.find(repository, n)
	if issue == nil {
		g.mu.Unlock()
		reply(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	g.nextID++
	c := &FakeComment{ID: g.nextID, Issue: n, Body: in.Body, CreatedAt: g.Now().UTC()}
	g.repo(repository).comments = append(g.repo(repository).comments, c)
	issue.UpdatedAt = c.CreatedAt
	wire := wireComment(*c)
	g.mu.Unlock()
	if g.finish(w, r, f) {
		return
	}
	reply(w, http.StatusCreated, wire)
}

func (g *FakeIssueServer) find(repository string, number int) *FakeIssue {
	for _, i := range g.repo(repository).issues {
		if i.Number == number {
			return i
		}
	}
	return nil
}

func (g *FakeIssueServer) state(w http.ResponseWriter, r *http.Request, repository, number string) {
	if _, done := g.before(w, r, FaultState, repository); done {
		return
	}
	n, _ := strconv.Atoi(number)
	g.mu.Lock()
	issue := g.find(repository, n)
	var wire map[string]any
	if issue != nil {
		wire = wireIssue(*issue)
	}
	g.mu.Unlock()
	if wire == nil {
		reply(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	reply(w, http.StatusOK, wire)
}

func (g *FakeIssueServer) list(w http.ResponseWriter, r *http.Request, repository string) {
	if _, done := g.before(w, r, FaultListIssues, repository); done {
		return
	}
	q := r.URL.Query()
	since, _ := time.Parse(time.RFC3339, q.Get("since"))
	var labels []string
	if v := q.Get("labels"); v != "" {
		labels = strings.Split(v, ",")
	}
	g.mu.Lock()
	var out []map[string]any
	for _, i := range g.repo(repository).issues {
		if i.UpdatedAt.Before(since) {
			continue
		}
		carries := true
		for _, l := range labels {
			carries = carries && slices.Contains(i.Labels, l)
		}
		if carries {
			out = append(out, wireIssue(*i))
		}
	}
	g.mu.Unlock()
	reply(w, http.StatusOK, paged(out, q))
}

func (g *FakeIssueServer) listComments(w http.ResponseWriter, r *http.Request, repository, number string) {
	if _, done := g.before(w, r, FaultListComments, repository); done {
		return
	}
	n, _ := strconv.Atoi(number)
	q := r.URL.Query()
	since, _ := time.Parse(time.RFC3339, q.Get("since"))
	g.mu.Lock()
	if g.find(repository, n) == nil {
		g.mu.Unlock()
		reply(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	var out []map[string]any
	for _, c := range g.repo(repository).comments {
		if c.Issue == n && !c.CreatedAt.Before(since) {
			out = append(out, wireComment(*c))
		}
	}
	g.mu.Unlock()
	reply(w, http.StatusOK, paged(out, q))
}

func paged(items []map[string]any, q map[string][]string) []map[string]any {
	per, _ := strconv.Atoi(first(q["per_page"]))
	page, _ := strconv.Atoi(first(q["page"]))
	if per < 1 {
		per = 30
	}
	if page < 1 {
		page = 1
	}
	start := (page - 1) * per
	if start >= len(items) {
		return []map[string]any{}
	}
	return items[start:min(start+per, len(items))]
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func wireIssue(i FakeIssue) map[string]any {
	labels := []map[string]string{}
	for _, l := range i.Labels {
		labels = append(labels, map[string]string{"name": l})
	}
	out := map[string]any{
		"number": i.Number, "html_url": "https://github.com/fake/issues/" + strconv.Itoa(i.Number), "state": i.State,
		"title": i.Title, "body": i.Body, "labels": labels,
		"created_at": i.CreatedAt.Format(time.RFC3339), "updated_at": i.UpdatedAt.Format(time.RFC3339),
	}
	if i.PullRequest {
		out["pull_request"] = map[string]string{}
	}
	return out
}

func wireComment(c FakeComment) map[string]any {
	return map[string]any{"id": c.ID, "html_url": "https://github.com/fake/comments/" + strconv.FormatInt(c.ID, 10), "body": c.Body,
		"created_at": c.CreatedAt.Format(time.RFC3339), "updated_at": c.CreatedAt.Format(time.RFC3339)}
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func drop(w http.ResponseWriter) {
	if h, ok := w.(http.Hijacker); ok {
		if conn, _, err := h.Hijack(); err == nil {
			conn.Close()
		}
	}
}
