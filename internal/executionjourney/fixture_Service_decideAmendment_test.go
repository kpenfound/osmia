package executionjourney

import (
	context "context"
)

func (s *Service) decideAmendment(ctx context.Context, raw string, id string, req AmendmentDecisionRequest) (AmendmentResponse, *APIError) {
	return s.intake().DecideAmendment(ctx, raw, id, req)
}
