package api

import (
	"github.com/kpenfound/osmia/internal/config"
)

type DraftEdit struct {
	SpecRevision int    `json:"spec_revision"`
	PlanRevision int    `json:"plan_revision"`
	Spec         string `json:"spec"`
	Plan         string `json:"plan"`
}

type CharterEdit struct {
	Revision int    `json:"revision"`
	Content  string `json:"content"`
}

type BaseEdit struct {
	Base     config.WorkstreamID `json:"base"`
	Revision int                 `json:"revision"`
}
