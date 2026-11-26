package fixturekit

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SkillNames are the names of the skills a lab configuration loads and of the
// other skills a prompt could refer to by name. No text an agent receives may
// contain one: a prompt is complete without any skill, and a reference to a
// skill would make it depend on one.
var SkillNames = []string{
	"handoff-reports", "design-sketch", "hill-climb-evaluator", "behavioral-testing", "simple-code", "no-deltas",
	"lint-discipline", "unslop", "typescript-best-practices", "debugging", "pr-review", "no-comments", "pr-writing",
	"osmia-design-language", "frontend-design", "web-design-guidelines", "kpenfound/skills",
}

// OwnershipSentence is Anthropic's sentence that tells an agent it owns the
// code it works on. Osmia's masons are never given it.
const OwnershipSentence = "you own this code"

// LoggedTurn is one turn an engine ran: the request its backend session was
// sent and what the turn's tool session offered.
type LoggedTurn struct {
	Name, ResumeID, SystemPrompt, Prompt string
	Profile, Model                       string
	Skills, GrantedTools, Tools          []string
	VCS                                  bool
	// ToolDescriptions is every tool the turn was offered, with its
	// description and input schema, as the agent reads them.
	ToolDescriptions map[string]string
}

// LoggedText is a text an agent receives and where it came from.
type LoggedText struct{ Source, Text string }

// PromptLog records every turn an engine runs.
type PromptLog struct {
	mu    sync.Mutex
	turns []LoggedTurn
}

// NewPromptLog records every turn engine runs from now on.
func NewPromptLog(engine *DemoEngine) *PromptLog {
	l := &PromptLog{}
	engine.Mu.Lock()
	previous := engine.Observe
	engine.Observe = func(ctx context.Context, req agent.Request, tools *mcp.ClientSession) {
		if previous != nil {
			previous(ctx, req, tools)
		}
		l.record(ctx, req, tools)
	}
	engine.Mu.Unlock()
	return l
}

func (l *PromptLog) record(ctx context.Context, req agent.Request, tools *mcp.ClientSession) {
	turn := LoggedTurn{Name: req.Name, ResumeID: req.ResumeID, SystemPrompt: req.SystemPrompt, Prompt: req.Prompt,
		Profile: req.Profile.Name, Model: req.Profile.Model, Skills: slices.Clone(req.Profile.Skills), ToolDescriptions: map[string]string{}}
	if req.Grants != nil {
		turn.GrantedTools, turn.VCS = slices.Clone(req.Grants.Tools), req.Grants.VCS
	}
	if tools != nil {
		if listed, err := tools.ListTools(ctx, nil); err == nil {
			for _, tool := range listed.Tools {
				data, _ := json.Marshal(tool)
				turn.Tools = append(turn.Tools, tool.Name)
				turn.ToolDescriptions[tool.Name] = string(data)
			}
			slices.Sort(turn.Tools)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.turns = append(l.turns, turn)
}

// Turns are the turns run so far, oldest first.
func (l *PromptLog) Turns() []LoggedTurn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.turns)
}

// Turn is the first turn run under name.
func (l *PromptLog) Turn(name string) (LoggedTurn, bool) {
	for _, turn := range l.Turns() {
		if turn.Name == name {
			return turn, true
		}
	}
	return LoggedTurn{}, false
}

// Named are the turns whose name has prefix, oldest first.
func (l *PromptLog) Named(prefix string) []LoggedTurn {
	return slices.DeleteFunc(l.Turns(), func(turn LoggedTurn) bool { return !strings.HasPrefix(turn.Name, prefix) })
}

// Texts are the system prompt, prompt and tool descriptions of every turn.
func (l *PromptLog) Texts() []LoggedText {
	var out []LoggedText
	for _, turn := range l.Turns() {
		out = append(out, LoggedText{turn.Name + " system prompt", turn.SystemPrompt}, LoggedText{turn.Name + " prompt", turn.Prompt})
		for _, name := range turn.Tools {
			out = append(out, LoggedText{turn.Name + " tool " + name, turn.ToolDescriptions[name]})
		}
	}
	return out
}

var toolResultPath = regexp.MustCompile(`^tools/tool-`)

// ToolResults are the results of every tool the agents of stream called, as
// the trace recorded them when each call ended.
func ToolResults(t *testing.T, repo *trace.Repository, stream config.WorkstreamID) []LoggedText {
	t.Helper()
	docs, err := trace.Read[trace.Document](repo, stream)
	Must(t, err)
	latest := map[string]trace.Document{}
	for _, d := range docs {
		if toolResultPath.MatchString(d.Path) && d.Revision >= latest[d.ID].Revision {
			latest[d.ID] = d
		}
	}
	var out []LoggedText
	for id, d := range latest {
		var call trace.ToolCall
		if json.Unmarshal([]byte(d.Content), &call) != nil || call.Output == nil {
			continue
		}
		out = append(out, LoggedText{"tool result " + call.Name + " " + id, call.Output.Content})
	}
	slices.SortFunc(out, func(a, b LoggedText) int { return strings.Compare(a.Source, b.Source) })
	return out
}

// NamedSkill is the first skill name text contains, if any.
func NamedSkill(text string) (string, bool) {
	lower := strings.ToLower(text)
	for _, name := range SkillNames {
		if strings.Contains(lower, name) {
			return name, true
		}
	}
	return "", false
}

// AssertNamesNoSkill fails for every text that names a skill.
func AssertNamesNoSkill(t *testing.T, texts []LoggedText) {
	t.Helper()
	for _, text := range texts {
		if name, ok := NamedSkill(text.Text); ok {
			t.Errorf("%s names the skill %q:\n%s", text.Source, name, text.Text)
		}
	}
}

// AssertMasonsAreNotToldTheyOwnTheCode fails when a turn of a mason carries
// the ownership sentence in its system prompt or its prompt. A mason's turn
// is one whose name starts with a mason agent's name.
func AssertMasonsAreNotToldTheyOwnTheCode(t *testing.T, turns []LoggedTurn, masonPrefix string) {
	t.Helper()
	seen := 0
	for _, turn := range turns {
		if !strings.HasPrefix(turn.Name, masonPrefix) {
			continue
		}
		seen++
		if strings.Contains(strings.ToLower(turn.SystemPrompt+"\n"+turn.Prompt), OwnershipSentence) {
			t.Errorf("mason turn %s is told %q", turn.Name, OwnershipSentence)
		}
	}
	if seen == 0 {
		t.Errorf("no mason turn started with %q", masonPrefix)
	}
}
