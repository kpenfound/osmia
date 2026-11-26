package service

import (
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/thread"
	"github.com/kpenfound/osmia/internal/trace"
)

func legacyBaseline(grants map[string]coreadapter.Capabilities) thread.LegacyBaseline {
	return func(role string, opening trace.QueuedTurn) (string, bool) {
		grant, ok := grants[role]
		prompt := opening.Request.SystemPrompt
		if !ok || prompt == "" || !grant.WriteFiles || grant.Web || (role != masonRole && role != sessionRole) {
			return "", false
		}
		if FactoryInstructions(coreadapter.Scope{Role: role, Turn: opening.Request.TurnID}) != "" || (opening.Request.Unit != "" && role == masonRole) {
			return "", false
		}
		return prompt, true
	}
}
