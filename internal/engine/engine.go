// Package engine probes Dagger engines and drives Osmia's managed container.
// Callers serialize lifecycle operations with work admission.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/redact"
)

const CacheVolumeName = "osmia-dagger-engine-cache"
const cachePath = "/var/lib/dagger"
const commandTimeout = 30 * time.Second

// Command runs an executable with exactly env and returns its standard output.
type Command func(ctx context.Context, path string, env []string, args ...string) (string, error)

// Options replaces host boundaries. Nil boundaries use the host's.
type Options struct {
	LookPath func(string) (string, error)
	Command  Command
	Getenv   func(string) string
	Timeout  time.Duration
}

// Controller uses a resolved, validated configuration. It never changes the
// configuration or manages containers other than ManagedDaggerEngineName.
type Controller struct {
	config  config.ResolvedDagger
	options Options
}

func New(resolved config.ResolvedDagger, o Options) *Controller {
	if resolved.Mode == "" {
		resolved.Mode = config.DaggerEngineAuto
	}
	if resolved.Mode == config.DaggerEngineManaged {
		resolved.Address = "docker-container://" + config.ManagedDaggerEngineName
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Command == nil {
		o.Command = command
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Timeout <= 0 {
		o.Timeout = commandTimeout
	}
	return &Controller{config: resolved, options: o}
}

// Status is a best-effort snapshot. Empty versions and nil CacheSizeBytes mean
// unknown; ReachabilityError explains a failed probe without exposing credentials.
type Status struct {
	Mode                      config.DaggerEngineMode `json:"mode"`
	Address                   string                  `json:"address,omitempty"`
	Reachable                 bool                    `json:"reachable"`
	Version                   string                  `json:"version,omitempty"`
	ExpectedVersion           string                  `json:"expected_version,omitempty"`
	CLIVersion                string                  `json:"cli_version,omitempty"`
	VersionMismatch           bool                    `json:"version_mismatch"`
	ConfiguredVersionMismatch bool                    `json:"configured_version_mismatch"`
	CacheSizeBytes            *int64                  `json:"cache_size_bytes,omitempty"`
	ReachabilityError         string                  `json:"reachability_error,omitempty"`
}

// NotApplicableError refuses a lifecycle action on an engine Osmia does not manage.
type NotApplicableError struct {
	Mode   config.DaggerEngineMode
	Action string
}

func (e *NotApplicableError) Error() string {
	return fmt.Sprintf("engine %s: not applicable to this mode (%s)", e.Action, e.Mode)
}

// Applicable checks whether a lifecycle action applies, without probing or
// modifying the engine. Admission gates call it before checking for busy work.
func (c *Controller) Applicable(action string) error {
	if c.config.Mode != config.DaggerEngineManaged {
		return &NotApplicableError{Mode: c.config.Mode, Action: action}
	}
	return nil
}

// ExpectedVersion resolves an omitted version to the installed CLI's release.
func (c *Controller) ExpectedVersion(ctx context.Context) (string, error) {
	if c.config.Version != "" {
		version := release(c.config.Version)
		if version == "" {
			return "", errors.New("cannot read configured engine release")
		}
		return version, nil
	}
	return c.cliVersion(ctx)
}

func (c *Controller) cliVersion(ctx context.Context) (string, error) {
	out, err := c.invoke(ctx, "dagger", false, "version")
	if err != nil {
		return "", err
	}
	version := release(out)
	if version == "" {
		return "", errors.New("cannot read dagger CLI release")
	}
	return version, nil
}

// Ensure creates a missing managed container or starts a stopped one. A running
// container is left untouched, including when its image differs from configuration.
// Other modes require no action.
func (c *Controller) Ensure(ctx context.Context) error {
	if c.config.Mode != config.DaggerEngineManaged {
		return nil
	}
	container, err := c.inspect(ctx)
	if err != nil {
		return err
	}
	if container != nil {
		if container.State.Running {
			return nil
		}
		_, err = c.invoke(ctx, "docker", false, "start", config.ManagedDaggerEngineName)
		return err
	}
	version, err := c.ExpectedVersion(ctx)
	if err != nil {
		return err
	}
	return c.create(ctx, version, CacheVolumeName)
}

// Restart recreates the managed container at the expected version, preserving
// its named cache volume. Callers must exclude active engine work throughout.
func (c *Controller) Restart(ctx context.Context) error {
	if err := c.Applicable("restart"); err != nil {
		return err
	}
	version, err := c.ExpectedVersion(ctx)
	if err != nil {
		return err
	}
	container, err := c.inspect(ctx)
	if err != nil {
		return err
	}
	volume := CacheVolumeName
	if container != nil {
		for _, mount := range container.Mounts {
			if mount.Destination == cachePath {
				if mount.Type != "volume" || mount.Name == "" {
					return errors.New("managed engine cache is not a named volume; cannot preserve it on restart")
				}
				volume = mount.Name
			}
		}
		if container.State.Running {
			if _, err := c.invoke(ctx, "docker", false, "stop", config.ManagedDaggerEngineName); err != nil {
				return err
			}
		}
		// No -v: cache volumes survive removal.
		if _, err := c.invoke(ctx, "docker", false, "rm", config.ManagedDaggerEngineName); err != nil {
			return err
		}
	}
	return c.create(ctx, version, volume)
}

// Prune releases all reclaimable cache entries on the managed engine. Callers
// must exclude active engine work throughout. It does not start a stopped engine.
func (c *Controller) Prune(ctx context.Context) error {
	if err := c.Applicable("prune"); err != nil {
		return err
	}
	container, err := c.inspect(ctx)
	if err != nil {
		return err
	}
	if container == nil || !container.State.Running {
		return errors.New("managed engine is not running")
	}
	_, err = c.invoke(ctx, "dagger", true, "core", "engine", "local-cache", "prune")
	return err
}

func (c *Controller) create(ctx context.Context, version, volume string) error {
	if version == "" {
		return errors.New("cannot determine managed engine version")
	}
	_, err := c.invoke(ctx, "docker", false, "run", "--detach", "--name", config.ManagedDaggerEngineName,
		"--privileged", "--restart", "always", "--volume", volume+":"+cachePath, "registry.dagger.io/engine:"+version)
	return err
}

// Status probes the configured engine in every mode without issuing lifecycle
// commands. Failure to reach an engine is represented in the snapshot.
func (c *Controller) Status(ctx context.Context) (Status, error) {
	s := Status{Mode: c.config.Mode, Address: c.config.Address, ExpectedVersion: release(c.config.Version)}
	s.CLIVersion, _ = c.cliVersion(ctx)
	if s.ExpectedVersion == "" {
		s.ExpectedVersion = s.CLIVersion
	}
	snapshot := func() (Status, error) {
		s.VersionMismatch = s.Version != "" && s.CLIVersion != "" && s.Version != s.CLIVersion
		s.ConfiguredVersionMismatch = s.Version != "" && s.ExpectedVersion != "" && s.Version != s.ExpectedVersion
		return s, nil
	}
	if c.config.Mode == config.DaggerEngineManaged {
		container, err := c.inspect(ctx)
		if err != nil {
			s.ReachabilityError = err.Error()
			return snapshot()
		}
		if container == nil {
			s.ReachabilityError = "managed engine container does not exist"
			return snapshot()
		}
		s.Version = imageRelease(container.Config.Image)
		if !container.State.Running {
			s.ReachabilityError = "managed engine container is stopped"
			return snapshot()
		}
	}
	out, err := c.invoke(ctx, "dagger", true, "core", "version")
	if err != nil {
		s.ReachabilityError = err.Error()
		return snapshot()
	}
	s.Reachable = true
	if version := release(out); version != "" {
		s.Version = version
	}
	if out, err := c.invoke(ctx, "dagger", true, "core", "engine", "local-cache", "entry-set", "disk-space-bytes"); err == nil {
		if size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil && size >= 0 {
			s.CacheSizeBytes = &size
		}
	}
	return snapshot()
}

type containerInfo struct {
	Config struct{ Image string }
	State  struct{ Running bool }
	Mounts []struct{ Type, Name, Destination string }
}

func (c *Controller) inspect(ctx context.Context) (*containerInfo, error) {
	out, err := c.invoke(ctx, "docker", false, "container", "inspect", config.ManagedDaggerEngineName)
	if err != nil {
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "no such container:") || strings.Contains(message, "no such object:") {
			return nil, nil
		}
		return nil, err
	}
	var containers []containerInfo
	if err := json.Unmarshal([]byte(out), &containers); err != nil {
		return nil, errors.New("cannot decode managed engine container inspection")
	}
	if len(containers) != 1 {
		return nil, errors.New("managed engine container inspection returned no unique container")
	}
	return &containers[0], nil
}

var releasePattern = regexp.MustCompile(`\bv?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?\b`)

func release(s string) string {
	found := releasePattern.FindString(s)
	if found == "" {
		return ""
	}
	return "v" + strings.TrimPrefix(found, "v")
}

func imageRelease(image string) string {
	if strings.Contains(image, "@") {
		return ""
	}
	_, tag, ok := strings.Cut(image[strings.LastIndex(image, "/")+1:], ":")
	if !ok {
		return ""
	}
	return release(tag)
}

func (c *Controller) invoke(ctx context.Context, name string, engine bool, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.options.Timeout)
	defer cancel()
	path, err := c.options.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s is not on PATH", name)
	}
	if engine && c.config.Mode == config.DaggerEngineCloud && c.options.Getenv(c.config.CloudTokenEnv) == "" {
		return "", errors.New("configured Cloud token is not set")
	}
	env := c.environment(engine)
	out, err := c.options.Command(ctx, path, env, args...)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), redact.Secrets(err.Error(), redact.KnownSecrets()...))
	}
	return out, nil
}

func (c *Controller) environment(engine bool) []string {
	var env []string
	for _, name := range []string{"PATH", "HOME", "USER", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "SSH_AUTH_SOCK"} {
		if value := c.options.Getenv(name); value != "" {
			env = append(env, name+"="+value)
		}
	}
	if !engine {
		return env
	}
	switch c.config.Mode {
	case config.DaggerEngineAuto:
		for _, name := range []string{"DAGGER_ENGINE", "DAGGER_CLOUD_ENGINE", "_EXPERIMENTAL_DAGGER_RUNNER_HOST", "DAGGER_X_RELEASE"} {
			if value := c.options.Getenv(name); value != "" {
				env = append(env, name+"="+value)
			}
		}
	case config.DaggerEngineCloud:
		env = append(env, "DAGGER_ENGINE=cloud", "DAGGER_CLOUD_ENGINE=1")
		token := c.options.Getenv(c.config.CloudTokenEnv)
		redact.Register(token)
		env = append(env, "DAGGER_CLOUD_TOKEN="+token)
	default:
		env = append(env, "DAGGER_ENGINE="+c.config.Address, "_EXPERIMENTAL_DAGGER_RUNNER_HOST="+c.config.Address)
	}
	return env
}

func command(ctx context.Context, path string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
	}
	return string(out), err
}
