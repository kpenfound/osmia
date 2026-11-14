package api

import (
	"encoding/json"
)

// MemorySetup contains reviewable Hearsay configuration fragments, not applied
// configuration. JSON contents are valid YAML for Hearsay's directory loader.
// The sources, scope and authority fragments are the same for every project
// that posts through the same work and owner sources; the principals fragment
// grants the project's entity to the owner and the agents.
type MemorySetup struct {
	Files map[string]json.RawMessage `json:"files"`
	// WorkSource and OwnerSource are the paired `work` sources, from
	// [hearsay.publish] or the documented defaults.
	WorkSource  string `json:"work_source"`
	OwnerSource string `json:"owner_source"`
	// Publisher is the agent principal that posts to both sources as an
	// attributor, and Principal is the owner it names as author.
	Publisher string `json:"publisher"`
	Principal string `json:"principal"`
	// Scope is the entity id the project reads by default.
	Scope string `json:"hearsay_scope"`
}
