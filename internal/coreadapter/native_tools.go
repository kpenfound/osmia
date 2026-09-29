package coreadapter

import "github.com/kpenfound/busybees/core/agent"

// nativeTools maps service capabilities to each backend's tool vocabulary.
// Delegation, arbitrary MCP discovery and provider-native web tools are absent.
// Shell grants permit execution in the disposable view under the selected
// sandbox's network policy; delivery credentials and VCS remain withheld.
func nativeTools(backend string, c Capabilities) []string {
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
