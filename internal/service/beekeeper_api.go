package service

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/kpenfound/osmia/internal/service/beekeeper"
)

// defaultBeekeeperMessages and maxBeekeeperMessages bound the limit parameter
// of GET /v1/beekeeper: a limit of zero or less, or one left out, uses the
// default; a limit above the maximum is clamped to it.
const (
	defaultBeekeeperMessages = 50
	maxBeekeeperMessages     = 200
)

// BeekeeperMessagesResponse lists the Beekeeper chat's messages, oldest
// first, with whether older messages than the ones listed exist.
type BeekeeperMessagesResponse struct {
	Messages []beekeeper.Message `json:"messages"`
	HasOlder bool                `json:"has_older"`
}

// BeekeeperSendRequest is an owner message to the Beekeeper.
type BeekeeperSendRequest struct {
	Text string `json:"text"`
}

// beekeeperLimit parses the limit query parameter of GET /v1/beekeeper: an
// absent or non-positive value uses defaultBeekeeperMessages, and a value
// above maxBeekeeperMessages is clamped to it.
func beekeeperLimit(r *http.Request) (int, *APIError) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultBeekeeperMessages, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, &APIError{Validation, "limit must be a positive integer"}
	}
	if n > maxBeekeeperMessages {
		n = maxBeekeeperMessages
	}
	return n, nil
}

// beekeeperMessages reads the Beekeeper chat's recent messages, oldest
// first. It works whether or not any owner project is registered: the
// Beekeeper's thread lives in the shadow project, never in a registered
// project's trace.
func (s *Service) beekeeperMessages(r *http.Request) (BeekeeperMessagesResponse, *APIError) {
	limit, api := beekeeperLimit(r)
	if api != nil {
		return BeekeeperMessagesResponse{}, api
	}
	messages, hasOlder, err := beekeeper.Messages(s.Beekeeper(), limit)
	if err != nil {
		return BeekeeperMessagesResponse{}, &APIError{Internal, "cannot read the beekeeper chat; check the trace repository"}
	}
	return BeekeeperMessagesResponse{Messages: messages, HasOlder: hasOlder}, nil
}

// postBeekeeperMessage records text as the Beekeeper thread's next owner
// request and runs the Beekeeper turn through Service.PostBeekeeper. It
// works whether or not any owner project is registered. A busy Beekeeper
// refuses the message with a conflict and records nothing.
func (s *Service) postBeekeeperMessage(ctx context.Context, req BeekeeperSendRequest) (BeekeeperMessagesResponse, *APIError) {
	if strings.TrimSpace(req.Text) == "" {
		return BeekeeperMessagesResponse{}, &APIError{Validation, "text must not be empty"}
	}
	q, err := s.PostBeekeeper(ctx, req.Text)
	if err != nil {
		if errors.Is(err, beekeeper.ErrBusy) {
			return BeekeeperMessagesResponse{}, &APIError{Conflict, "the beekeeper is busy with a previous message; send it again once that turn finishes"}
		}
		return BeekeeperMessagesResponse{}, &APIError{Internal, "cannot record or run the beekeeper turn; check the trace repository"}
	}
	return BeekeeperMessagesResponse{Messages: beekeeper.EntriesOf(q)}, nil
}
