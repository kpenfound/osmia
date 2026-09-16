// Package trace stores service-owned records in a project's dedicated Git repository.
package trace

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

const Version = 1

// Header identifies one immutable revision. Cause identifies the triggering
// message, operation or record; Depth retains the causal nesting of agent turns.
type Header struct {
	Schema     string              `json:"schema"`
	Version    int                 `json:"version"`
	ID         string              `json:"id"`
	Revision   int                 `json:"revision"`
	Project    config.ProjectID    `json:"project"`
	Workstream config.WorkstreamID `json:"workstream,omitempty"`
	Unit       string              `json:"unit,omitempty"`
	At         time.Time           `json:"at"`
	Actor      Actor               `json:"actor"`
	Cause      string              `json:"cause"`
	Depth      int                 `json:"depth"`
}
type Actor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

func (h Header) header() Header { return h }

// Record is sealed to the supported versioned trace types.
type Record interface {
	header() Header
	traceRecord()
}

type Document struct {
	Header
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (Document) traceRecord() {}

type Transition struct {
	Header
	Subject string `json:"subject"`
	From    string `json:"from"`
	To      string `json:"to"`
	Reason  string `json:"reason"`
}

func (Transition) traceRecord() {}

type Question struct {
	Header
	AskedBy     Actor  `json:"asked_by"`
	Question    string `json:"question"`
	SentToOwner string `json:"sent_to_owner,omitempty"`
}

func (Question) traceRecord() {}

type Ruling struct {
	Header
	QuestionID       string   `json:"question_id"`
	QuestionRevision int      `json:"question_revision"`
	Decision         string   `json:"decision"`
	OwnerResponse    string   `json:"owner_response,omitempty"`
	ReturnedAnswer   string   `json:"returned_answer"`
	Changes          []string `json:"changes,omitempty"`
}

func (Ruling) traceRecord() {}

type Agent struct {
	Header
	Role     string                     `json:"role"`
	ThreadID string                     `json:"thread_id"`
	Session  coreadapter.BackendSession `json:"session"`
}

func (Agent) traceRecord() {}

type TurnRequest struct {
	Header
	AgentID      string                      `json:"agent_id"`
	ThreadID     string                      `json:"thread_id"`
	TurnID       string                      `json:"turn_id"`
	Profile      coreadapter.Profile         `json:"profile"`
	Resume       *coreadapter.BackendSession `json:"resume,omitempty"`
	SystemPrompt string                      `json:"system_prompt"`
	Prompt       string                      `json:"prompt"`
	History      string                      `json:"history,omitempty"`
}

func (TurnRequest) traceRecord() {}

type TurnResponse struct {
	Header
	AgentID         string                    `json:"agent_id"`
	ThreadID        string                    `json:"thread_id"`
	TurnID          string                    `json:"turn_id"`
	RequestID       string                    `json:"request_id"`
	RequestRevision int                       `json:"request_revision"`
	Result          coreadapter.SessionResult `json:"result"`
	Failure         string                    `json:"failure,omitempty"`
}

func (TurnResponse) traceRecord() {}

type Cost struct {
	Header
	Entry coreadapter.LedgerEntry `json:"entry"`
}

func (Cost) traceRecord() {}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func key(s string) bool     { return keyPattern.MatchString(s) }
func present(s string) bool { return strings.TrimSpace(s) != "" }
func validActor(a Actor) bool {
	return (a.Kind == "owner" || a.Kind == "service" || a.Kind == "agent") && key(a.ID)
}
func validUsage(u coreadapter.Usage) bool {
	return u.Turns >= 0 && u.CostUSD >= 0 && !math.IsNaN(u.CostUSD) && !math.IsInf(u.CostUSD, 0) && (u.CostKnown || u.CostUSD == 0)
}
func validSession(s coreadapter.BackendSession) bool { return present(s.Backend) && present(s.ID) }
func kind(r Record) string {
	switch r.(type) {
	case Document:
		return "document"
	case Transition:
		return "transition"
	case Question:
		return "question"
	case Ruling:
		return "ruling"
	case Agent:
		return "agent"
	case TurnRequest:
		return "turn-request"
	case TurnResponse:
		return "turn-response"
	case Cost:
		return "cost"
	default:
		return ""
	}
}
func validate(r Record) error {
	if r == nil || kind(r) == "" {
		return fmt.Errorf("unsupported record type")
	}
	h := r.header()
	if h.Schema != "osmia.trace."+kind(r) || h.Version != Version {
		return fmt.Errorf("unsupported schema/version %q/%d", h.Schema, h.Version)
	}
	if err := config.CheckProjectIDs(h.Project); err != nil {
		return err
	}
	if h.Workstream != "" {
		if err := config.CheckWorkstreamIDs(h.Workstream); err != nil {
			return err
		}
	}
	if h.Workstream == "" && kind(r) != "document" {
		return fmt.Errorf("workstream is required")
	}
	if !key(h.ID) || (h.Unit != "" && !key(h.Unit)) || h.Revision < 1 || h.At.IsZero() || !validActor(h.Actor) || !present(h.Cause) || h.Depth < 0 {
		return fmt.Errorf("invalid identity, revision, timestamp or provenance")
	}
	valid := false
	switch v := r.(type) {
	case Document:
		valid = documentPath(v.Path, h.Workstream != "") == nil
	case Transition:
		valid = key(v.Subject) && present(v.To) && present(v.Reason)
	case Question:
		valid = validActor(v.AskedBy) && present(v.Question)
	case Ruling:
		valid = key(v.QuestionID) && v.QuestionRevision > 0 && present(v.Decision) && present(v.ReturnedAnswer)
	case Agent:
		valid = key(v.Role) && key(v.ThreadID) && (v.Session == (coreadapter.BackendSession{}) || validSession(v.Session))
	case TurnRequest:
		valid = key(v.AgentID) && key(v.ThreadID) && key(v.TurnID) && key(v.Profile.Name) && present(v.Profile.Backend) && present(v.Profile.Model) && present(v.Prompt) && v.Profile.Timeout >= 0 && v.Profile.MaxTurns >= 0 && v.Profile.CostLimitUSD >= 0 && !math.IsNaN(v.Profile.CostLimitUSD) && !math.IsInf(v.Profile.CostLimitUSD, 0) && (v.Resume == nil || validSession(*v.Resume))
	case TurnResponse:
		valid = key(v.AgentID) && key(v.ThreadID) && key(v.TurnID) && key(v.RequestID) && v.RequestRevision > 0 && !v.Result.StartedAt.IsZero() && v.Result.Duration >= 0 && validUsage(v.Result.Usage) && (validSession(v.Result.Session) || (v.Result.Session == (coreadapter.BackendSession{}) && present(v.Failure)))
	case Cost:
		s := v.Entry.Scope
		valid = s.Project == string(h.Project) && s.Workstream == string(h.Workstream) && key(s.Thread) && key(s.Turn) && key(s.Role) && (s.Unit == "" || key(s.Unit)) && key(v.Entry.AttemptID) && !v.Entry.At.IsZero() && validUsage(v.Entry.Usage)
	}
	if !valid {
		return fmt.Errorf("invalid %s payload", kind(r))
	}
	return nil
}

func documentPath(p string, stream bool) error {
	if err := relative(p); err != nil {
		return err
	}
	parts := strings.Split(p, "/")
	if !stream && (p == "charter.md" || p == "kb/entities.json" || (len(parts) == 2 && (parts[0] == "kb" || parts[0] == "notes") && strings.HasSuffix(parts[1], ".md"))) {
		return nil
	}
	if stream && (p == "spec.md" || p == "plan.json" || (len(parts) == 2 && parts[0] == "handed")) {
		return nil
	}
	return fmt.Errorf("unsupported document path %q", p)
}
func relative(p string) error {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\\\x00\r\n:") {
		return fmt.Errorf("invalid relative path %q", p)
	}
	for _, s := range strings.Split(p, "/") {
		if s == "." || s == ".." || strings.HasPrefix(s, ".") || strings.TrimSpace(s) != s {
			return fmt.Errorf("invalid path component %q", s)
		}
	}
	return nil
}
func scopePath(h Header) string {
	if h.Workstream == "" {
		return ""
	}
	return "workstreams/" + string(h.Workstream) + "/"
}
func recordPath(r Record) string {
	prefix := scopePath(r.header())
	switch v := r.(type) {
	case Document:
		return prefix + "documents.jsonl"
	case Transition:
		return prefix + "events.jsonl"
	case Question:
		return prefix + "questions/" + v.ID + "/question.jsonl"
	case Ruling:
		return prefix + "questions/" + v.QuestionID + "/rulings.jsonl"
	case Agent:
		return prefix + "agents/" + v.ID + "/identity.jsonl"
	case TurnRequest:
		return prefix + "agents/" + v.AgentID + "/log.jsonl"
	case TurnResponse:
		return prefix + "agents/" + v.AgentID + "/log.jsonl"
	case Cost:
		return prefix + "ledger.jsonl"
	}
	return ""
}
