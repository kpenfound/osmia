package api

import "github.com/kpenfound/osmia/internal/config"

// ProjectPreviewRequest sets a project's preview. Omitted path and timeout
// use the same defaults as the project file.
type ProjectPreviewRequest struct {
	Call    []string `json:"call" required:"true" nullable:"false" minItems:"1"`
	Port    int      `json:"port" required:"true" minimum:"1" maximum:"65535"`
	Path    *string  `json:"path,omitempty" nullable:"false" default:"/" pattern:"^/"`
	Timeout *string  `json:"timeout,omitempty" nullable:"false" default:"5m" description:"Go duration from 1m to 30m"`
}

type ProjectPreviewResponse struct {
	Project config.ProjectID `json:"project"`
	Preview *config.Preview  `json:"preview" nullable:"true"`
}
