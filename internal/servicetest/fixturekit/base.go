package fixturekit

import (
	"context"

	"github.com/kpenfound/osmia/internal/workspace"
)

// BaseProvider supplies remote and ancestry responses without remote Git
// operations. Remotes and Fetches count calls made by the controller.
type BaseProvider struct {
	workspace.Provider
	Upstream, Tip string
	IsAncestor    bool
	// Remotes and Fetches count remote operations.
	Remotes, Fetches int
}

func (g *BaseProvider) Remote(context.Context, string) (string, error) {
	g.Remotes++
	return "upstream", nil
}
func (g *BaseProvider) Fetch(context.Context, string, string) (string, error) {
	g.Fetches++
	return g.Upstream, nil
}
func (g *BaseProvider) Branch(context.Context, string) (string, bool, error) {
	return g.Tip, g.Tip != "", nil
}
func (g *BaseProvider) Ancestor(context.Context, string, string) (bool, error) {
	return g.IsAncestor, nil
}
