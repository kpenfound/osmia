package api

import (
	"encoding/json"

	"github.com/kpenfound/osmia/internal/config"
)

// MarshalJSON preserves the distinction between an omitted list and an empty
// list, so clients can send an explicit empty list and receive a validation error.
func (r HandInRequest) MarshalJSON() ([]byte, error) {
	type request HandInRequest
	var projects *[]config.ProjectID
	if r.Projects != nil {
		projects = &r.Projects
	}
	return json.Marshal(struct {
		request
		Projects *[]config.ProjectID `json:"projects,omitempty"`
	}{request: request(r), Projects: projects})
}
