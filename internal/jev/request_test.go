package jev_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/jev"
)

type requestTransport func(*http.Request) (*http.Response, error)

func (f requestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLoadedJevConfigurationReachesProvider(t *testing.T) {
	// Serial: replaces http.DefaultTransport and sets API key environment variables.
	for _, tc := range []struct {
		name, connection, destination, model, keyEnv string
	}{
		{
			name:        "TypeSafe defaults",
			destination: "https://api.typesafe.ai/v1/systemone",
			model:       "jev-latest",
			keyEnv:      "TYPESAFE_API_KEY",
		},
		{
			name: "explicit OpenRouter",
			connection: `url = "https://openrouter.ai/api"
model = "~typesafe/jev-latest"
api_key_env = "OPENROUTER_API_KEY"
`,
			destination: "https://openrouter.ai/api/v1/systemone",
			model:       "~typesafe/jev-latest",
			keyEnv:      "OPENROUTER_API_KEY",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Short roots keep the loaded Unix socket path portable.
			root, err := os.MkdirTemp("", "jev-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			text := "version = 1\n[profiles.default]\nagent = 'claude'\nmodel = 'test-model'\n[jev]\nenabled = true\n" + tc.connection
			if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			current, err := config.Load(config.Options{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if err := current.Jev.Settings().Validate(); err != nil {
				t.Fatal(err)
			}

			t.Setenv("TYPESAFE_API_KEY", "unused-typesafe-credential")
			t.Setenv("OPENROUTER_API_KEY", "unused-openrouter-credential")
			key := "synthetic-jev-credential"
			t.Setenv(tc.keyEnv, key)
			transport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = transport })
			requests := 0
			http.DefaultTransport = requestTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != http.MethodPost || r.URL.String() != tc.destination {
					t.Errorf("request = %s %s, want POST %s", r.Method, r.URL, tc.destination)
				}
				if r.Header.Get("Authorization") != "Bearer "+key {
					t.Error("bearer credential does not match the configured environment variable")
				}
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Model != tc.model {
					t.Errorf("request model = %q, want %q", body.Model, tc.model)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{
  "model": "jev-1.13.0",
  "answers": {"class": {"type": "choice", "choice": "claims_done", "probabilities": {"claims_done": 0.9, "gave_up": 0.1}, "confidence": 0.85}},
  "usage": {"input_tokens": 120, "output_tokens": 10}
}`))}, nil
			})

			f := newFixture(t)
			// The service feeds its current configuration to the same Judge;
			// nil Provider and Getenv use agentcore's client and the environment.
			j := &jev.Judge{Config: func() config.Jev { return current.Jev }}
			d := j.Evaluate(context.Background(), f.repo, judgment("Done."))
			if d.Outcome != jev.Accepted || d.Recovered || d.Response == nil || d.Response.Model != "jev-1.13.0" {
				t.Fatalf("decision = %+v", d)
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want 1", requests)
			}
		})
	}
}
