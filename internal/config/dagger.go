package config

import "github.com/kpenfound/agentcore/agent"

// ManagedDaggerEngineName is the container shared by service checks and masons.
const ManagedDaggerEngineName = "osmia-dagger-engine"

type DaggerEngineMode string

const (
	DaggerEngineAuto    DaggerEngineMode = "auto"
	DaggerEngineManaged DaggerEngineMode = "managed"
	DaggerEngineCloud   DaggerEngineMode = "cloud"
	DaggerEngineAddress DaggerEngineMode = "address"
)

// DaggerConfig selects the service's engine and names its optional Cloud
// credential. Loading configuration never reads the credential's value.
type DaggerConfig struct {
	Version       string `toml:"version" json:"version,omitempty"`
	Engine        string `toml:"engine" json:"engine"`
	CloudTokenEnv string `toml:"cloud_token_env" json:"cloud_token_env,omitempty"`
}

// ResolvedDagger describes a validated service engine. Version is the expected
// release; empty leaves the host CLI release to be resolved at runtime.
// Address is set only for managed and explicit-address engines.
type ResolvedDagger struct {
	Mode          DaggerEngineMode
	Address       string
	Version       string
	CloudTokenEnv string
}

// Resolve returns the engine selection without probing engines or reading secrets.
// The zero value selects auto, as does an omitted [dagger] section.
func (d DaggerConfig) Resolve() ResolvedDagger {
	r := ResolvedDagger{Version: d.Version, CloudTokenEnv: d.CloudTokenEnv}
	switch d.Engine {
	case "", string(DaggerEngineAuto):
		r.Mode = DaggerEngineAuto
	case string(DaggerEngineManaged):
		r.Mode = DaggerEngineManaged
		r.Address = "docker-container://" + ManagedDaggerEngineName
	case string(DaggerEngineCloud):
		r.Mode = DaggerEngineCloud
	default:
		r.Mode = DaggerEngineAddress
		r.Address = d.Engine
	}
	return r
}

func (d DaggerConfig) validate(path string, versionSet bool) error {
	if versionSet {
		if err := agent.CheckDaggerVersion(d.Version); err != nil {
			return fieldError(path, "dagger.version", "must be a release such as v0.20.5")
		}
	}
	switch d.Engine {
	case string(DaggerEngineAuto), string(DaggerEngineManaged), string(DaggerEngineCloud):
	default:
		if err := agent.CheckDaggerEngine(d.Engine); err != nil {
			return fieldError(path, "dagger.engine", "must be auto, managed, cloud, unix://<absolute socket path>, tcp://<host>:<port> or docker-container://<container name>")
		}
	}
	if d.CloudTokenEnv != "" && !environmentName.MatchString(d.CloudTokenEnv) {
		return fieldError(path, "dagger.cloud_token_env", "must name an environment variable")
	}
	if d.Engine == string(DaggerEngineCloud) && d.CloudTokenEnv == "" {
		return fieldError(path, "dagger.cloud_token_env", "is required for the cloud engine")
	}
	return nil
}
