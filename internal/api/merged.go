package api

import (
	"github.com/kpenfound/osmia/internal/seal"
)

// DeliveryMerge records a feature branch the owner merged into upstream
// outside the factory: the branch, its tip, and the upstream commit found
// holding that tip's changes.
type DeliveryMerge struct {
	Branch   string    `json:"branch"`
	Commit   string    `json:"commit"`
	Upstream seal.Base `json:"upstream"`
}
