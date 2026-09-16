// Package status checks and stores the chief of staff's workstream status.
package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/trace"
)

const (
	ToolName     = "set_status"
	maxGoal      = 200
	maxAttention = 400
	maxNote      = 1200
	maxNoteParts = 6
	maxAgent     = 200
	maxAgents    = 32
)

var (
	sentenceEnd = regexp.MustCompile(`[.!?]+(\s+|$)`)
	osmiaID     = regexp.MustCompile(`\b[a-z]{1,10}_[0-9a-f]{8,}\b`)
	uuid        = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	sessionID   = regexp.MustCompile(`\b(?:[A-Z2-7]{26}|sess?_[A-Za-z0-9]{6,})\b`)
	hex         = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	model       = regexp.MustCompile(`(?i)\b(?:(?:claude|gpt|gemini|llama|mistral|mixtral|codex|deepseek|qwen|grok|opus|sonnet|haiku)(?:[- ]?[0-9][a-z0-9.]*|-[a-z0-9.]+)*|o[1-9](?:-(?:mini|pro|preview))?)\b`)
	fileName    = regexp.MustCompile(`(?i)\b[\w-]+\.(?:go|md|json|jsonl|toml|ya?ml|txt|sh|py|rs|java|kt|rb|sql|proto|mod|sum|lock|html|css|xml|ini|cfg|conf|diff|patch|c|h|cc|cpp|hpp)\b`)
	token       = regexp.MustCompile(`[^\s"'()\[\]{}<>,;]+`)
)

// Prose words written with a slash; any other slashed token is a ref or path.
var slashWords = []string{"and/or", "either/or", "i/o", "input/output", "read/write", "yes/no", "on/off", "ci/cd", "pass/fail", "n/a", "w/o", "24/7"}

// Check returns why content cannot be the workstream status, or nil. Every
// message names the field and is written for the chief of staff to act on.
// Known identifiers are rejected where they appear as a whole token, but only
// those shaped like identifiers (containing a digit or underscore, or at least
// 16 characters), so ordinary words used as test IDs do not reject prose.
func Check(c trace.StatusContent, known []string) error {
	if strings.TrimSpace(c.Goal) == "" {
		return errors.New("goal is required: one sentence on what the workstream is working toward")
	}
	if strings.TrimSpace(c.Note) == "" {
		return errors.New("note is required: a few sentences on what changed and where things stand")
	}
	if c.Agents == nil {
		return errors.New("agents is required: one line per active agent, or an empty list")
	}
	if err := line("goal", c.Goal, maxGoal); err != nil {
		return err
	}
	if n := sentences(c.Goal); n > 1 {
		return fmt.Errorf("goal must be one sentence, not %d", n)
	}
	if err := line("attention", c.Attention, maxAttention); err != nil {
		return err
	}
	if len(c.Note) > maxNote {
		return fmt.Errorf("note must be at most %d characters", maxNote)
	}
	if n := sentences(c.Note); n > maxNoteParts {
		return fmt.Errorf("note must be a few sentences, at most %d, not %d", maxNoteParts, n)
	}
	if len(c.Agents) > maxAgents {
		return fmt.Errorf("agents must list at most %d lines", maxAgents)
	}
	for i, a := range c.Agents {
		field := fmt.Sprintf("agents[%d]", i)
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("%s is empty: describe the agent in a line or leave it out", field)
		}
		if err := line(field, a, maxAgent); err != nil {
			return err
		}
	}
	fields := [][2]string{{"goal", c.Goal}, {"attention", c.Attention}, {"note", c.Note}}
	for i, a := range c.Agents {
		fields = append(fields, [2]string{fmt.Sprintf("agents[%d]", i), a})
	}
	for _, f := range fields {
		if err := identifiers(f[0], f[1], known); err != nil {
			return err
		}
	}
	return nil
}

func line(field, s string, limit int) error {
	if strings.ContainsAny(s, "\r\n") {
		return fmt.Errorf("%s must be a single line", field)
	}
	if len(s) > limit {
		return fmt.Errorf("%s must be at most %d characters", field, limit)
	}
	return nil
}

func sentences(s string) int {
	n := 0
	for _, part := range sentenceEnd.Split(strings.TrimSpace(s), -1) {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	return n
}

// identifiers reports the first identifier in text, naming its class.
func identifiers(field, text string, known []string) error {
	found := func(class, value, instead string) error {
		return fmt.Errorf("%s contains %s (%q); %s", field, class, value, instead)
	}
	for _, id := range known {
		if identifierShaped(id) && wholeToken(text, id) {
			return found("an Osmia or backend identifier", id, "refer to the work or the agent in words")
		}
	}
	if m := osmiaID.FindString(text); m != "" {
		return found("an Osmia project, workstream or thread ID", m, "refer to the work in words")
	}
	if m := uuid.FindString(text); m != "" {
		return found("a session ID", m, "describe the agent's work instead")
	}
	if m := sessionID.FindString(text); m != "" {
		return found("a session ID", m, "describe the agent's work instead")
	}
	for _, m := range hex.FindAllString(text, -1) {
		if strings.ContainsAny(m, "0123456789") && strings.ContainsAny(m, "abcdef") {
			return found("a commit hash", m, "say what the change does")
		}
	}
	if m := model.FindString(text); m != "" {
		return found("a model name", m, "name the role, not the model behind it")
	}
	for _, t := range token.FindAllString(text, -1) {
		t = strings.TrimRight(t, ".:!?")
		switch {
		case strings.Contains(t, "://"):
			return found("a URL", t, "say what it points to")
		case strings.HasPrefix(t, "/") || strings.HasPrefix(t, "~/") || strings.HasPrefix(t, "./") || strings.HasPrefix(t, "../"):
			return found("a file path", t, "name the part of the product instead")
		case strings.Contains(strings.Trim(t, "/"), "/") && !slices.Contains(slashWords, strings.ToLower(t)):
			return found("a branch name or file path", t, "name the change or the part of the product instead")
		}
	}
	if m := fileName.FindString(text); m != "" {
		return found("a file name", m, "name the part of the product instead")
	}
	return nil
}

func identifierShaped(id string) bool {
	return strings.TrimSpace(id) != "" && (len(id) >= 16 || strings.ContainsAny(id, "_0123456789"))
}

// wholeToken reports whether s appears in text bounded by characters that
// cannot be part of an identifier.
func wholeToken(text, s string) bool {
	for from := 0; from <= len(text)-len(s); {
		i := strings.Index(text[from:], s)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(s)
		if (start == 0 || !idByte(text[start-1])) && (end == len(text) || !idByte(text[end])) {
			return true
		}
		from = start + 1
	}
	return false
}

func idByte(b byte) bool {
	return b == '_' || b == '-' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// Tool binds set_status to one claimed chief-of-staff turn. A status the
// check refuses is an ordinary result, {"stored":false,"reason":...}, so the
// chief of staff reads why; a stored one is {"stored":true,"revision":n}.
func Tool(repository *trace.Repository, agent string, scope coreadapter.Scope, now func() time.Time) (coreadapter.Tool, error) {
	if repository == nil || now == nil {
		return coreadapter.Tool{}, errors.New("status tool requires a trace and a clock")
	}
	if scope.Role != trace.StatusRole {
		return coreadapter.Tool{}, fmt.Errorf("%s is granted to the chief of staff only", ToolName)
	}
	tool := coreadapter.Tool{Name: ToolName, Effect: coreadapter.ToolMemory,
		Description: "Replace this workstream's status for the owner. goal: one sentence on what is being worked toward. attention: one concrete action the owner must take now, or empty. note: a few sentences on what changed and where things stand. agents: one line per active agent. Write in words: no commit hashes, branch names, file paths, session IDs, Osmia IDs or model names.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string"},"attention":{"type":"string"},"note":{"type":"string"},"agents":{"type":"array","items":{"type":"string"}}},"required":["goal","note","agents"],"additionalProperties":false}`)}
	tool.Handle = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var content trace.StatusContent
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&content); err != nil {
			return nil, fmt.Errorf("status input: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("status input must be one object")
		}
		revision, err := repository.SetStatus(ctx, agent, scope, content, now(), Check)
		var rejected *trace.StatusRejected
		if errors.As(err, &rejected) {
			return json.Marshal(struct {
				Stored bool   `json:"stored"`
				Reason string `json:"reason"`
			}{false, rejected.Reason})
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			Stored   bool `json:"stored"`
			Revision int  `json:"revision"`
		}{true, revision})
	}
	return tool, nil
}
