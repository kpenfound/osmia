package api

import (
	"github.com/kpenfound/osmia/internal/config"
)

// OverlapAdvisory is a warning that another workstream of the project builds
// in the same code: Workstream is that workstream, Seal and OtherSeal the
// seals of this workstream and of it that were compared, Subsystems the
// top-level entities the shared code belongs to, Entities the most specific
// entities both sealed footprints cover and Paths the sealed path patterns of
// either that overlap the other's. Message says the same in plain language.
type OverlapAdvisory struct {
	Workstream config.WorkstreamID `json:"workstream"`
	Seal       int                 `json:"seal"`
	OtherSeal  int                 `json:"other_seal"`
	Subsystems []string            `json:"subsystems"`
	Entities   []string            `json:"entities"`
	Paths      []string            `json:"paths"`
	Message    string              `json:"message"`
}
