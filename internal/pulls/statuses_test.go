package pulls

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/osmia/internal/ghlog"
)

// statusFake stands in for the GitHub combined-status API, a remote system
// whose real paging and rate limits it does not reproduce.
func statusFake(t *testing.T, statuses []string, perPage int, requests *[]string) GitHub {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.Method != http.MethodGet {
			t.Errorf("request %s with authorization %q", r.Method, r.Header.Get("Authorization"))
		}
		*requests = append(*requests, r.URL.RequestURI())
		if r.URL.Path != "/repos/acme/widgets/commits/abc123/status" {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		var chunk []string
		for i := (page - 1) * perPage; i < page*perPage && i < len(statuses); i++ {
			chunk = append(chunk, statuses[i])
		}
		fmt.Fprintf(w, `{"state":"failure","sha":"abc123","total_count":%d,"statuses":[%s]}`, len(statuses), strings.Join(chunk, ","))
	}))
	t.Cleanup(server.Close)
	return GitHub{BaseURL: server.URL, Token: "secret", HTTP: server.Client()}
}

func status(id int, context, state, description, target string) string {
	return fmt.Sprintf(`{"id":%d,"context":%q,"state":%q,"description":%q,"target_url":%q,"created_at":"2026-05-01T10:00:%02dZ","updated_at":"2026-05-01T10:01:%02dZ","creator":{"login":"x"}}`, id, context, state, description, target, id%60, id%60)
}

func TestCommitStatusesReturnTheLatestStatusOfEachContext(t *testing.T) {
	t.Parallel()
	var requests []string
	g := statusFake(t, []string{
		status(11, "load", "success", "Loaded", "https://dagger.cloud/l"),
		status(12, "Run Dagger Checks", "failure", "1 failed", "https://dagger.cloud/checks/x.json"),
		status(13, "test/a", "failure", "Failed in 2m33s. Run `dagger trace abc`", ""),
	}, 100, &requests)
	got, err := g.CommitStatuses(context.Background(), "acme/widgets", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got.Repository != "acme/widgets" || got.Commit != "abc123" || len(got.Statuses) != 3 {
		t.Fatalf("read %+v", got)
	}
	run, ok := got.Status("Run Dagger Checks")
	want := CommitStatus{ID: 12, Context: "Run Dagger Checks", State: "failure", Description: "1 failed", TargetURL: "https://dagger.cloud/checks/x.json",
		CreatedAt: time.Date(2026, 5, 1, 10, 0, 12, 0, time.UTC), UpdatedAt: time.Date(2026, 5, 1, 10, 1, 12, 0, time.UTC)}
	if !ok || run != want {
		t.Fatalf("run status %+v, want %+v", run, want)
	}
	if _, ok := got.Status("missing"); ok {
		t.Fatal("found a status that is not there")
	}
	if len(requests) != 1 || !strings.Contains(requests[0], "per_page=100") {
		t.Fatalf("requests %v", requests)
	}
}

func TestCommitStatusesFollowEveryPage(t *testing.T) {
	t.Parallel()
	var statuses []string
	for i := 1; i <= 230; i++ {
		statuses = append(statuses, status(i, fmt.Sprintf("check/%03d", i), "success", "", ""))
	}
	var requests []string
	g := statusFake(t, statuses, statusPageSize, &requests)
	got, err := g.CommitStatuses(context.Background(), "acme/widgets", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Statuses) != 230 || len(requests) != 3 {
		t.Fatalf("%d statuses from %d requests: %v", len(got.Statuses), len(requests), requests)
	}
	if last, _ := got.Status("check/230"); last.ID != 230 {
		t.Fatalf("last status %+v", last)
	}
}

func TestCommitStatusesKeepTheLaterPostingOfAContext(t *testing.T) {
	t.Parallel()
	var requests []string
	older := `{"id":5,"context":"Run Dagger Checks","state":"pending","created_at":"2026-05-01T10:00:00Z","updated_at":"2026-05-01T10:00:00Z"}`
	newer := `{"id":9,"context":"Run Dagger Checks","state":"success","created_at":"2026-05-01T10:05:00Z","updated_at":"2026-05-01T10:05:00Z"}`
	g := statusFake(t, []string{newer, older}, 100, &requests)
	got, err := g.CommitStatuses(context.Background(), "acme/widgets", "abc123")
	if err != nil || len(got.Statuses) != 1 || got.Statuses[0].ID != 9 {
		t.Fatalf("read %+v: %v", got, err)
	}
}

func TestCommitStatusesFailWithoutLeakingTheToken(t *testing.T) {
	t.Parallel()
	var requests []string
	g := statusFake(t, nil, 100, &requests)
	if _, err := g.CommitStatuses(context.Background(), "acme/widgets", "other"); err == nil || !strings.Contains(err.Error(), "GitHub answered 404") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("an unknown commit: %v", err)
	}
}

func TestCommitStatusesAreLoggedAsReads(t *testing.T) {
	t.Parallel()
	var requests []string
	g := statusFake(t, []string{status(1, "load", "success", "", "")}, 100, &requests)
	var buf bytes.Buffer
	ctx := ghlog.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	if _, err := g.CommitStatuses(ctx, "acme/widgets", "abc123"); err != nil {
		t.Fatal(err)
	}
	records := decodeGHRecords(t, &buf)
	if len(records) != 1 || records[0].Action != "commits.status" || records[0].Access != ghlog.Read || records[0].Method != http.MethodGet || records[0].Status != http.StatusOK || records[0].Repository != "acme/widgets" {
		t.Fatalf("records %+v", records)
	}
}
