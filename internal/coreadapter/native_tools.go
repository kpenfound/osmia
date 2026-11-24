package coreadapter

import "github.com/kpenfound/agentcore/agent"

const skillTool = "Skill"

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
