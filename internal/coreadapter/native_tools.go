package coreadapter

import "github.com/kpenfound/agentcore/agent"

// skillTool is Claude's tool that loads a skill from the turn's plugins.
const skillTool = "Skill"

// nativeTools maps service capabilities to each backend's tool vocabulary.
// Delegation and arbitrary MCP discovery are absent. The Web permission adds
// the backend's own web tools after all others (webTools); without it no web
// tool is listed, whatever else is granted. Shell grants permit execution in
// the disposable view under the selected sandbox's network policy; delivery
// credentials and VCS remain withheld. A Claude turn given skills also holds
// the Skill tool.
func nativeTools(backend string, c Capabilities, skills bool) []string {
	tools := []string{}
	switch backend {
	case "", agent.AgentClaude:
		tools = append(tools, "Read", "Glob", "Grep")
		if c.WriteFiles {
			tools = append(tools, "Write", "Edit")
			if c.Execute {
				tools = append(tools, "Bash")
			}
		}
		if skills {
			tools = append(tools, skillTool)
		}
	case agent.AgentCodex:
		if c.WriteFiles {
			tools = append(tools, "apply_patch")
			if c.Execute {
				tools = append(tools, "shell")
			}
		}
	case agent.AgentOpenCode:
		tools = append(tools, "read", "glob", "grep", "list")
		if c.WriteFiles {
			tools = append(tools, "edit")
			if c.Execute {
				tools = append(tools, "bash")
			}
		}
	}
	if c.Web {
		tools = append(tools, webTools(backend)...)
	}
	return tools
}

// webTools are the web search and page fetch tools of a backend itself: the
// tools the Web permission grants. The service runs no search or fetch of its
// own.
func webTools(backend string) []string {
	switch backend {
	case "", agent.AgentClaude:
		return []string{"WebSearch", "WebFetch"}
	case agent.AgentCodex:
		return []string{"web_search"}
	case agent.AgentOpenCode:
		return []string{"websearch", "webfetch"}
	}
	return nil
}
