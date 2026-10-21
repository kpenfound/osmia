package api

import (
	"encoding/json"
)

// MemorySetup contains reviewable Hearsay configuration fragments, not applied
// configuration. JSON contents are valid YAML for Hearsay's directory loader.
type MemorySetup struct {
	Files    map[string]json.RawMessage `json:"files"`
	Entities map[string]string          `json:"hearsay_entities"`
	Scope    string                     `json:"hearsay_scope"`
	Anchors  []MemoryAnchor             `json:"anchors"`
}

type MemoryAnchor struct {
	Source   string `json:"source"`
	Artifact string `json:"artifact"`
	Scope    string `json:"scope"`
}
