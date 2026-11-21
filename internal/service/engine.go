package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/engine"
	"github.com/kpenfound/osmia/internal/redact"
)

// configuredChecks binds a client to the configuration of its operation. A
// running operation keeps this client when reload installs new stages.
func (s *Service) configuredChecks(cfg *config.Config) ReviewChecks {
	if s.options.reviewChecks != nil {
		return s.options.reviewChecks
	}
	if cfg == nil {
		return DaggerChecks{}
	}
	resolved := cfg.Dagger.Resolve()
	token := os.Getenv(resolved.CloudTokenEnv)
	redact.Register(token)
	selection := string(resolved.Mode)
	if resolved.Address != "" {
		selection = resolved.Address
	}
	return DaggerChecks{Engine: selection, CloudToken: token}
}

// configuredCloudChecks is the cloud engine runner a check run falls back to,
// or nil when cfg does not turn the fallback on. It carries the token the
// primary runner uses.
func (s *Service) configuredCloudChecks(cfg *config.Config) ReviewChecks {
	if cfg == nil {
		return nil
	}
	resolved := cfg.Dagger.Resolve()
	if !resolved.CheckFallbackCloud {
		return nil
	}
	if s.options.cloudChecks != nil {
		return s.options.cloudChecks
	}
	token := os.Getenv(resolved.CloudTokenEnv)
	redact.Register(token)
	return DaggerChecks{Engine: string(config.DaggerEngineCloud), CloudToken: token}
}

// configuredRerun is the runner that requests a rerun of a pull request's
// failed Cloud checks for cfg: the Dagger CLI with the token
// dagger.cloud_token_env names, which may be unset, in which case no rerun can
// be requested.
func (s *Service) configuredRerun(cfg *config.Config) CloudRerunner {
	if s.options.CloudRerun != nil {
		return s.options.CloudRerun
	}
	if cfg == nil {
		return nil
	}
	token := os.Getenv(cfg.Dagger.Resolve().CloudTokenEnv)
	redact.Register(token)
	return DaggerChecks{CloudToken: token}
}

// configuredTrace is the runner that fetches a Dagger Cloud trace's output
// for cfg: the Dagger CLI with the token dagger.cloud_token_env names, which
// may be unset, in which case every fetch reports that no token is configured.
func (s *Service) configuredTrace(cfg *config.Config) TraceRunner {
	return traceRunner(s, cfg)
}

// traceRunner is s's configured trace runner for cfg. A nil s, which a turn
// built before the service is attached has, runs the Dagger CLI with the
// token cfg names.
func traceRunner(s *Service, cfg *config.Config) TraceRunner {
	if s != nil && s.options.CloudTrace != nil {
		return s.options.CloudTrace
	}
	if cfg == nil {
		return DaggerChecks{}
	}
	token := os.Getenv(cfg.Dagger.Resolve().CloudTokenEnv)
	redact.Register(token)
	return DaggerChecks{CloudToken: token}
}

func (s *Service) ensureEngine(ctx context.Context, cfg *config.Config) error {
	// Read and register the token before any engine command can report an error.
	s.configuredChecks(cfg)
	if err := engine.New(cfg.Dagger.Resolve(), engine.Options{}).Ensure(ctx); err != nil {
		return fmt.Errorf("ensure Dagger engine: %w", err)
	}
	return nil
}

// engineHTTP composes lifecycle controls with the service's admission gate.
func (s *Service) engineHTTP(w http.ResponseWriter, r *http.Request) bool {
	action, ok := strings.CutPrefix(r.URL.Path, Prefix+"/engine/")
	if !ok {
		return false
	}
	if action == "status" && r.Method == http.MethodGet {
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(engineActionTimeout))
		status, err := engine.New(s.current().Dagger.Resolve(), engine.Options{}).Status(r.Context())
		if err != nil {
			failWith(w, engineAPIError(err))
		} else {
			respond(w, http.StatusOK, status)
		}
		return true
	}
	if (action != "restart" && action != "prune") || r.Method != http.MethodPost {
		return false
	}
	// Lifecycle commands may need several bounded probes and container commands.
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(engineActionTimeout))
	ctx, cancel := context.WithTimeout(r.Context(), engine.ActionTimeout)
	defer cancel()
	// Waiting for a configuration mutation counts against the action budget.
	retry := time.NewTicker(10 * time.Millisecond)
	defer retry.Stop()
	for !s.projectMu.TryLock() {
		select {
		case <-ctx.Done():
			failWith(w, engineAPIError(ctx.Err()))
			return true
		case <-retry.C:
		}
	}
	defer s.projectMu.Unlock()
	control := engine.New(s.current().Dagger.Resolve(), engine.Options{})
	var err error
	if action == "restart" {
		err = s.RestartEngine(ctx, control)
	} else {
		err = s.PruneEngine(ctx, control)
	}
	if err != nil {
		failWith(w, engineAPIError(err))
	} else {
		respond(w, http.StatusOK, MutationResponse{Applied: true})
	}
	return true
}

const engineActionTimeout = 5 * time.Minute

func engineAPIError(err error) *APIError {
	code := Internal
	var busy *EngineBusyError
	var mode *engine.NotApplicableError
	if errors.As(err, &busy) {
		code = Unavailable
	} else if errors.As(err, &mode) {
		code = Conflict
	}
	return &APIError{Code: code, Message: redact.Secrets(err.Error(), redact.KnownSecrets()...)}
}
