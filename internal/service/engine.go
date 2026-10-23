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
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	control := engine.New(s.current().Dagger.Resolve(), engine.Options{})
	var err error
	if action == "restart" {
		err = s.RestartEngine(r.Context(), control)
	} else {
		err = s.PruneEngine(r.Context(), control)
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
