package pulls

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubCancellationDispositionRetriesAndPreservesMergedPulls(t *testing.T) {
	t.Parallel()
	for _, choice := range []string{"close", "draft"} {
		for _, merged := range []bool{false, true} {
			t.Run(choice+map[bool]string{true: "merged", false: "open"}[merged], func(t *testing.T) {
				state, draft, writes := "open", false, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer fixture" {
						t.Error("missing service credential")
					}
					switch r.Method {
					case http.MethodPatch:
						var body map[string]string
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["state"] != "closed" {
							t.Errorf("close body: %v %v", body, err)
						}
						writes++
						state = "closed"
					case http.MethodPost:
						var body struct {
							Query     string
							Variables map[string]string
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.URL.Path != "/graphql" || !strings.Contains(body.Query, "convertPullRequestToDraft") || body.Variables["id"] != "node" {
							t.Errorf("draft mutation: %+v %v", body, err)
						}
						writes++
						draft = true
						io.WriteString(w, `{"data":{"convertPullRequestToDraft":{}}}`)
						return
					case http.MethodGet:
					default:
						t.Errorf("unexpected method: %s", r.Method)
					}
					var mergedAt any
					if merged {
						mergedAt = "2026-01-01T00:00:00Z"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "node_id": "node", "state": state, "draft": draft, "merged_at": mergedAt})
				}))
				defer server.Close()
				g := GitHub{BaseURL: server.URL, Token: "fixture", HTTP: server.Client()}
				dispose := g.Close
				if choice == "draft" {
					dispose = g.Draft
				}
				for range 2 {
					pr, err := dispose(context.Background(), "owner/repo", 7)
					if err != nil || pr.Merged != merged {
						t.Fatalf("disposition: %+v %v", pr, err)
					}
				}
				want := 1
				if merged {
					want = 0
				}
				if writes != want {
					t.Fatalf("writes=%d want=%d", writes, want)
				}
			})
		}
	}
}

func TestGitHubDraftReportsGraphQLRejection(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"number":7,"node_id":"node","state":"open","draft":false}`)
			return
		}
		io.WriteString(w, `{"errors":[{"message":"Draft conversion refused"}]}`)
	}))
	defer server.Close()
	g := GitHub{BaseURL: server.URL, HTTP: server.Client()}
	if _, err := g.Draft(context.Background(), "owner/repo", 7); err == nil || !strings.Contains(err.Error(), "Draft conversion refused") {
		t.Fatalf("GraphQL rejection hidden: %v", err)
	}
}
