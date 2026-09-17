// Package issues fetches handed-in issues for the service. Credentials stay in
// the service process: callers hold them in a Client and no session receives
// them.
package issues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Ref names one issue.
type Ref struct {
	Owner, Repo string
	Number      int
}

// URL returns the canonical web address of the issue.
func (r Ref) URL() string {
	return fmt.Sprintf("https://github.com/%s/%s/issues/%d", r.Owner, r.Repo, r.Number)
}

var name = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// Parse accepts https://github.com/OWNER/REPO/issues/NUMBER.
func Parse(raw string) (Ref, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Ref{}, errors.New("issue URL must be https://github.com/OWNER/REPO/issues/NUMBER")
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 5 || parts[0] != "" || parts[3] != "issues" || !name.MatchString(parts[1]) || !name.MatchString(parts[2]) || parts[1] == "." || parts[1] == ".." || parts[2] == "." || parts[2] == ".." {
		return Ref{}, errors.New("issue URL must be https://github.com/OWNER/REPO/issues/NUMBER")
	}
	n, err := strconv.Atoi(parts[4])
	if err != nil || n < 1 || strconv.Itoa(n) != parts[4] {
		return Ref{}, errors.New("issue URL must end with a positive issue number")
	}
	return Ref{Owner: parts[1], Repo: parts[2], Number: n}, nil
}

// Client returns an issue's text as Markdown: its title as a heading, then its
// body.
type Client interface {
	Fetch(ctx context.Context, ref Ref) (string, error)
}

// Render is the Markdown text of an issue with the given title and body.
func Render(title, body string) string {
	text := "# " + title + "\n"
	if body != "" {
		text += "\n" + body
		if !strings.HasSuffix(body, "\n") {
			text += "\n"
		}
	}
	return text
}

// MaxResponse bounds the API response GitHub.Fetch reads.
const MaxResponse = 4 << 20

// GitHub reads issues through the GitHub REST API. Token, when set, is sent
// as a bearer token.
type GitHub struct {
	BaseURL string // defaults to https://api.github.com
	Token   string
	HTTP    *http.Client
}

func (g GitHub) Fetch(ctx context.Context, ref Ref) (string, error) {
	base := g.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/%s/issues/%d", strings.TrimSuffix(base, "/"), url.PathEscape(ref.Owner), url.PathEscape(ref.Repo), ref.Number), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: request failed", ref.URL())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: GitHub answered %d", ref.URL(), resp.StatusCode)
	}
	var issue struct {
		Title       *string         `json:"title"`
		Body        *string         `json:"body"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil || len(data) > MaxResponse {
		return "", fmt.Errorf("fetch %s: unreadable or oversized response", ref.URL())
	}
	if err := json.Unmarshal(data, &issue); err != nil || issue.Title == nil {
		return "", fmt.Errorf("fetch %s: invalid response", ref.URL())
	}
	if issue.PullRequest != nil {
		return "", fmt.Errorf("fetch %s: the number is a pull request, not an issue", ref.URL())
	}
	body := ""
	if issue.Body != nil {
		body = *issue.Body
	}
	return Render(*issue.Title, body), nil
}
