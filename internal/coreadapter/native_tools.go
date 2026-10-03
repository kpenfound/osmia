package coreadapter

import "github.com/kpenfound/busybees/core/agent"

// skillTool is Claude's tool that loads a skill from the turn's plugins.
const skillTool = "Skill"

// nativeTools maps service capabilities to each backend's tool vocabulary.
// Delegation, arbitrary MCP discovery and provider-native web tools are absent.
// Shell grants permit execution in the disposable view under the selected
// sandbox's network policy; delivery credentials and VCS remain withheld. A
// Claude turn given skills also holds the Skill tool.
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
	return tools
}
