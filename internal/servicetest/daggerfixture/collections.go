package daggerfixture

import (
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"slices"
)

const (
	PathRoot = "dag+check://lint"
	PathA    = PathRoot + "?go-package=a"
	PathB    = PathRoot + "?go-package=b"
)

// CollectionOptions lists parents and leaves in both Dagger link shapes.
// Offered names the narrow options; none of the roots belongs in a question.
func CollectionOptions() (config Config, offered []string) {
	config.Leaves = append(slices.Clone(Leaves), PathA, PathB)
	config.Links = append(slices.Clone(config.Leaves),
		"dag+check://?check=test", "dag+check://?check=test&go-module=.",
		fixturekit.SmartPackageA, fixturekit.SmartPackageB, PathRoot)
	offered = append(slices.Clone(config.Leaves), fixturekit.SmartPackageA, fixturekit.SmartPackageB)
	slices.Sort(offered)
	return config, offered
}
