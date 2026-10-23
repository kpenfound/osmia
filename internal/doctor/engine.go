package doctor

import (
	"context"
	"fmt"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/engine"
)

func (r *run) engine(ctx context.Context, cfg *config.Config) {
	controller := engine.New(cfg.Dagger.Resolve(), engine.Options{LookPath: r.LookPath, Command: r.EngineCommand, Getenv: r.Getenv, Timeout: commandTimeout})
	status, err := controller.Status(ctx)
	if err != nil {
		r.add(GroupToolchain, "dagger engine", Warn, reason(err), "check the configured Dagger engine and its connectivity")
		return
	}
	reachability := "reachable"
	level := Pass
	if !status.Reachable {
		level = Warn
		reachability = "unreachable: " + status.ReachabilityError
	}
	r.add(GroupToolchain, "dagger engine", level, fmt.Sprintf("%s: %s", status.Mode, reachability), "check the configured Dagger engine and its connectivity; under managed, start the service to ensure its container")
	known := func(value string) string {
		if value == "" {
			return "unknown"
		}
		return value
	}
	detail := fmt.Sprintf("engine %s; expected %s; host CLI %s", known(status.Version), known(status.ExpectedVersion), known(status.CLIVersion))
	level = Pass
	remedy := ""
	if status.Version == "" || status.CLIVersion == "" {
		level = Warn
		remedy = "install dagger and check connectivity to the configured engine"
	}
	if status.VersionMismatch {
		level = Warn
		detail += "; engine and CLI releases differ"
		remedy = "align the host dagger CLI and engine releases when convenient"
	}
	if status.ConfiguredVersionMismatch {
		level = Warn
		detail += "; running and configured releases differ"
		remedy = "align the expected engine release; under managed, restart the engine when idle to apply it"
	}
	r.add(GroupToolchain, "dagger engine version", level, detail, remedy)
	cache := "unknown (engine or CLI does not expose cache size)"
	if status.CacheSizeBytes != nil {
		cache = fmt.Sprintf("%d bytes", *status.CacheSizeBytes)
	}
	r.add(GroupToolchain, "dagger engine cache", Pass, cache, "")
}
