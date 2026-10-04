package config

import (
	"slices"
	"strings"
)

// Beekeeper configures the Beekeeper's runtime session: the service's single
// assistant for the whole service, which runs in the shadow project rather
// than on any registered project's configuration. Defaults: Name
// "Beekeeper", Profile "default", Sandbox "none".
type Beekeeper struct {
	Name    string `toml:"name" json:"name"`
	Profile string `toml:"profile" json:"profile"`
	Sandbox string `toml:"sandbox" json:"sandbox"`
	// Image is the container image the sandbox uses; required when Sandbox
	// is "container" and refused otherwise, except for "sbx".
	Image string `toml:"image" json:"image,omitempty"`
}

// defaultBeekeeper is applied before decoding, so an absent [beekeeper]
// section, or any field it omits, keeps these documented defaults.
func defaultBeekeeper() Beekeeper {
	return Beekeeper{Name: "Beekeeper", Profile: "default", Sandbox: "none"}
}

func (b Beekeeper) validate(path string) error {
	if strings.TrimSpace(b.Name) == "" || strings.ContainsAny(b.Name, "\x00\r\n") {
		return fieldError(path, "beekeeper.name", "must be one non-empty line")
	}
	if strings.TrimSpace(b.Profile) == "" {
		return fieldError(path, "beekeeper.profile", "profile is required")
	}
	if !slices.Contains([]string{"none", "claude", "container", "sbx"}, b.Sandbox) {
		return fieldError(path, "beekeeper.sandbox", "expected none, claude, container or sbx")
	}
	if b.Sandbox == "container" && strings.TrimSpace(b.Image) == "" {
		return fieldError(path, "beekeeper.image", "container requires an explicit image")
	}
	if b.Sandbox != "container" && b.Sandbox != "sbx" && b.Image != "" {
		return fieldError(path, "beekeeper.image", "image requires container or sbx sandbox")
	}
	return nil
}
