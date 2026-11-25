package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/fleet"
	"github.com/kpenfound/osmia/internal/ghlog"
	"github.com/kpenfound/osmia/internal/tailnet"
)

// fleetSweep is how often the service ends the leases that ran out.
const fleetSweep = 5 * time.Second

// resultLimit bounds a returned turn result, which carries the session files
// the server reads after the turn.
const resultLimit = 64 << 20

// resultTimeout bounds the upload of one result.
const resultTimeout = 5 * time.Minute

// Fleet is the registry of connected clients that placement and visibility
// read and that turn execution offers turns and sends requests through.
func (s *Service) Fleet() *fleet.Registry { return s.fleet }

// clientIdentity names the tailnet node a client request came from: the
// ClientIdentity option when set, else the node's WhoIs.
func (s *Service) clientIdentity(r *http.Request) (fleet.Identity, error) {
	if identify := s.options.ClientIdentity; identify != nil {
		return identify(r)
	}
	var node tailnet.Listener
	if s.tailnet != nil {
		node = s.tailnet.Current()
	}
	who, ok := node.(tailnet.Identifier)
	if !ok {
		return fleet.Identity{}, errors.New("no tailnet node is up to identify the caller")
	}
	n, err := who.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		return fleet.Identity{}, err
	}
	return fleet.Identity{ID: n.ID, Name: n.Name}, nil
}

// fleetRequest serves the client protocol, which only the tailnet listener
// answers.
func (s *Service) fleetRequest(w http.ResponseWriter, r *http.Request) bool {
	lease, rest, ok := api.ClientRouteParts(r.URL.Path)
	if !ok {
		return false
	}
	if tailnet.ArrivedOn(r) != tailnet.TailnetListener || r.Method != http.MethodPost {
		fail(w, NotFound)
		return true
	}
	ident, err := s.clientIdentity(r)
	if err != nil {
		failWith(w, &APIError{Code: Unavailable, Message: "the caller's tailnet node cannot be identified: " + err.Error()})
		return true
	}
	if lease == "connect" && rest == "" {
		var req api.ClientConnectRequest
		if decode(w, r, &req) {
			answerWith(w, 200)(s.fleet.Connect(r.Context(), ident, req))
		}
		return true
	}
	parts := strings.Split(rest, "/")
	switch {
	case len(parts) == 1 && parts[0] == "heartbeat":
		var req api.HeartbeatRequest
		if decode(w, r, &req) {
			answerWith(w, 200)(s.fleet.Heartbeat(lease, ident, req))
		}
	case len(parts) == 1 && parts[0] == "capabilities":
		var req api.CapabilityReport
		if decode(w, r, &req) {
			current, err := s.fleet.Report(lease, ident, req)
			answerWith(w, 200)(api.CapabilityResponse{Current: current}, err)
		}
	case len(parts) == 1 && parts[0] == "drain":
		var req api.DrainRequest
		if decode(w, r, &req) {
			err := s.fleet.Drain(lease, ident)
			answerWith(w, 200)(api.DrainResponse{Draining: true}, err)
		}
	case len(parts) == 1 && parts[0] == "leave":
		var req api.LeaveRequest
		if decode(w, r, &req) {
			err := s.fleet.Leave(lease, ident)
			answerWith(w, 200)(api.LeaveResponse{Ended: true}, err)
		}
	case len(parts) == 1 && parts[0] == "poll":
		var req api.PollRequest
		if decode(w, r, &req) {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.fleet.PollWindow() + s.options.WriteTimeout))
			messages, err := s.fleet.Poll(r.Context(), lease, ident, req)
			if messages == nil {
				messages = []api.Message{}
			}
			answerWith(w, 200)(api.PollResponse{Messages: messages}, err)
		}
	case len(parts) == 3 && parts[0] == "turns" && parts[2] == "claim":
		var req api.ClaimRequest
		if decode(w, r, &req) {
			answer, err := s.fleet.Claim(lease, ident, parts[1], req)
			answerWith(w, 200)(api.ClaimResponse{Answer: answer}, err)
		}
	case len(parts) == 3 && parts[0] == "turns" && parts[2] == "result":
		var req api.TurnResult
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(resultTimeout))
		if decodeLimit(w, r, &req, resultLimit) {
			answer, err := s.fleet.Result(r.Context(), lease, ident, parts[1], req)
			answerWith(w, 200)(api.TurnResultResponse{Answer: answer}, err)
		}
	case len(parts) == 3 && parts[0] == "requests" && parts[2] == "reply":
		var req api.ServerReply
		if decode(w, r, &req) {
			answered, err := s.fleet.Reply(lease, ident, parts[1], req)
			answerWith(w, 200)(api.ServerReplyResponse{Answered: answered}, err)
		}
	default:
		failWith(w, &APIError{Code: NotFound, Message: "route not found"})
	}
	return true
}

// answerWith answers with value, or with the failure err describes.
func answerWith(w http.ResponseWriter, status int) func(any, error) {
	return func(value any, err error) {
		if err == nil {
			respond(w, status, value)
			return
		}
		var failure *APIError
		if errors.As(err, &failure) {
			failWith(w, failure)
			return
		}
		failWith(w, &APIError{Code: Unavailable, Message: err.Error()})
	}
}

func decodeLimit(w http.ResponseWriter, r *http.Request, out any, limit int64) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			failWith(w, &APIError{Code: TooLarge, Message: "the request is too large"})
		} else {
			fail(w, Malformed)
		}
		return false
	}
	return decodeBody(w, data, out)
}

func (s *Service) startFleet(ctx context.Context) {
	go s.fleet.Run(ctx, fleetSweep)
	go func() {
		t := time.NewTicker(fleetSweep)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.recoverExpiredHolds(ctx)
			}
		}
	}()
}

func (s *Service) recoverExpiredHolds(ctx context.Context) {
	for _, e := range s.clients.Sweep() {
		cfg, p := s.runtimeOf(e.Project)
		if p == nil {
			continue
		}
		th, err := p.repository.Thread(e.Workstream, e.Agent)
		if err == nil {
			err = s.recoverThread(ctx, cfg, p.repository, e.Workstream, th)
		}
		if err != nil {
			ghlog.Logger(ctx).Warn("cannot recover a turn its client did not reattach", "project", string(e.Project), "turn", e.Turn, "error", err.Error())
		}
	}
}
