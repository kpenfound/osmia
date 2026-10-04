// Package beekeeper builds and opens the Beekeeper's shadow project: the
// internal project the service creates and owns for the Beekeeper alone,
// hidden from the frontend. The shadow project is never written to, read
// from or listed in the owner's project configuration, and it runs no shed,
// architect, mason, reviewer or delivery workflow.
package beekeeper

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// Actor records every change the service makes to the shadow project on the
// Beekeeper's behalf.
var Actor = trace.Actor{Kind: "service", ID: "osmia"}

// Project builds the shadow project's configuration from the Beekeeper
// section of the service-level configuration. It has no target repository:
// nothing is cloned for it and nothing is ever delivered from it.
func Project(b config.Beekeeper) config.Project {
	return config.Project{
		ID:            config.ShadowProjectID,
		Version:       1,
		Name:          b.Name,
		BaseBranch:    "main",
		Landing:       "commit-per-unit",
		ChecksTimeout: config.DefaultChecksTimeout,
		Capacity:      config.ProjectCapacity{PerWorkstream: 1},
	}
}

// Open creates the shadow project under the service's state directory if it
// does not exist, or reopens it if it does, the way every project's trace is
// opened, and ensures its one reserved workstream. Callers must Close the
// returned repository.
func Open(ctx context.Context, root config.Root, b config.Beekeeper, now time.Time) (*trace.Repository, error) {
	p := Project(b)
	directory, err := root.ProjectTrace(p.ID)
	if err != nil {
		return nil, err
	}
	_, manifestErr := os.Lstat(filepath.Join(directory, "project.json"))
	_, gitErr := os.Lstat(filepath.Join(directory, ".git"))
	if !os.IsNotExist(manifestErr) || !os.IsNotExist(gitErr) {
		return trace.Open(root, p)
	}
	repository, err := trace.Create(ctx, root, p, now, Actor)
	if err != nil {
		return nil, err
	}
	if err := repository.CreateWorkstream(ctx, config.BeekeeperWorkstreamID, now, Actor); err != nil {
		repository.Close()
		return nil, err
	}
	return repository, nil
}
