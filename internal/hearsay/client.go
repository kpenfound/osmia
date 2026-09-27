// Package hearsay supplies optional, authenticated external context. It does
// not own workflow state or grant authority to change local decisions.
package hearsay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
)

// Class maps Osmia roles to Hearsay's grant classes.
func Class(role string) string {
	switch role {
	case "chief_of_staff":
		return "orchestrator"
	case "architect", "committee", "mason", "reviewer":
		return "worker"
	default:
		return "observer"
	}
}

// Client resolves credentials only when calling the service. No credential is
// returned to agents, included in errors, or persisted in configuration.
type Client struct {
	Config    config.Hearsay
	HTTP      *http.Client
	LookupEnv func(string) string
}

func (c Client) Call(ctx context.Context, role, name string, input any) (json.RawMessage, error) {
	switch name {
	case "get_bundle", "resolve", "stance_history", "get_l1", "get_l0", "search", "assert", "watch":
	default:
		return nil, errors.New("unsupported memory call")
	}
	class := Class(role)
	if name == "assert" && class == "observer" || name == "watch" && class != "orchestrator" {
		return nil, errors.New("memory call is not granted to this role")
	}
	lookup := c.LookupEnv
	if lookup == nil {
		lookup = os.Getenv
	}
	agent := c.Config.Agents[class]
	human, token := lookup(c.Config.TokenEnv), lookup(agent.TokenEnv)
	if human == "" || token == "" || agent.ID == "" {
		return nil, errors.New("memory credentials are unavailable")
	}
	data, err := json.Marshal(input)
	if err != nil {
		return nil, errors.New("invalid memory arguments")
	}
	if len(data) > 1<<20 {
		return nil, errors.New("memory arguments exceed the size limit")
	}
	timeout := 3 * time.Second
	if name == "watch" {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Config.URL, "/")+"/v1/"+name, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("invalid memory endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+human)
	req.Header.Set("Hearsay-Principal", c.Config.Principal)
	req.Header.Set("Hearsay-Agent", agent.ID)
	req.Header.Set("Hearsay-Agent-Token", token)
	client := http.Client{}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	// A redirected call must never forward either credential to another endpoint.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("memory service is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("memory service returned HTTP %d", response.StatusCode)
	}
	out, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(out) > 1<<20 || !json.Valid(out) {
		return nil, errors.New("memory service returned an invalid or oversized response")
	}
	return json.RawMessage(out), nil
}
