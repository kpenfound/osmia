package intakefixture

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

type CharterClient interface {
	Charter(context.Context, config.ProjectID) (trace.Document, error)
	EditCharter(context.Context, config.ProjectID, api.CharterEdit) (trace.Document, error)
}

// WriteCharter records content as the owner's next charter revision through the API.
func WriteCharter(t *testing.T, c CharterClient, project config.ProjectID, content string) trace.Document {
	t.Helper()
	ctx := context.Background()
	current, err := c.Charter(ctx, project)
	Must(t, err)
	doc, err := c.EditCharter(ctx, project, api.CharterEdit{Revision: current.Revision, Content: content})
	Must(t, err)
	return doc
}

type MutationClient interface {
	Do(context.Context, string, string, any, any) error
}

func Mutation(t *testing.T, c MutationClient, method, kind string, input any) {
	t.Helper()
	var result api.MutationResponse
	Must(t, c.Do(context.Background(), method, api.Prefix+"/runtime/"+kind, input, &result))
	if !result.Applied {
		t.Fatal("not acknowledged")
	}
}
