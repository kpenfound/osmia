package fixturekit

import (
	"context"

	"github.com/kpenfound/osmia/internal/api"
)

// CheckFunc runs every check with its function, and lists no links.
type CheckFunc func(context.Context, string) (api.CheckResult, error)

func (f CheckFunc) List(context.Context, string, api.CheckIdentity) ([]string, error) {
	return nil, nil
}

func (f CheckFunc) Check(ctx context.Context, dir string, _ api.CheckIdentity, _ []string) (api.CheckResult, error) {
	return f(ctx, dir)
}

// PassingChecks is the check runner of service fixtures: every check passes.
var PassingChecks = CheckFunc(func(context.Context, string) (api.CheckResult, error) {
	return api.CheckResult{Output: "CHECKS  ✔ 1 passed\n✔ dag://go/packages/tests/test 1.0s OK\n"}, nil
})
