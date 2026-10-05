package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/service/beekeeper"
	"github.com/kpenfound/osmia/internal/trace"
)

// messageChiefOfStaffTool is the Beekeeper's tool that sends a message to
// the chief of staff of a named workstream in a named registered project.
const messageChiefOfStaffTool = "message_chief_of_staff"

// beekeeperActor is the provenance of a chief-of-staff turn the Beekeeper's
// message tool triggers: the Beekeeper, never the owner.
var beekeeperActor = trace.Actor{Kind: "beekeeper", ID: beekeeper.AgentID}

// chiefPromptFromBeekeeper opens the system prompt of a chief-of-staff turn
// the Beekeeper's message triggers; its framing makes clear the sender is
// the Beekeeper, the owner's one assistant for the whole factory, never the
// owner.
const chiefPromptFromBeekeeper = "You are the chief of staff for workstream %s. The prompt is a message from the Beekeeper, the owner's one assistant for the whole factory, not the owner. The project context below was assembled when the message was accepted."

// beekeeperMessageTurnID derives the deterministic turn identity of one
// Beekeeper message tool call to workstream of project, from the
// Beekeeper's own tool-call key, so delivering the same key for the same
// project and workstream twice, including by recovery after a restart,
// finds the request already recorded instead of appending a second one.
func beekeeperMessageTurnID(project config.ProjectID, workstream config.WorkstreamID, key string) string {
	sum := sha256.Sum256([]byte(string(project) + "\x00" + string(workstream) + "\x00" + key))
	return "bk_" + hex.EncodeToString(sum[:16])
}

// beekeeperTarget resolves rawProject and rawWorkstream to one workstream of
// one active, registered project: never the shadow project, never an
// unknown project or workstream, and never the project's own librarian
// workstream. It changes nothing.
func (s *Service) beekeeperTarget(rawProject, rawWorkstream string) (config.ProjectID, config.WorkstreamID, *trace.Repository, error) {
	project := config.ProjectID(rawProject)
	if err := config.CheckProjectIDs(project); err != nil {
		return "", "", nil, errors.New("project must be a project ID: p_ followed by 32 lowercase hexadecimal digits")
	}
	if project == config.ShadowProjectID {
		return "", "", nil, errors.New("the beekeeper's own shadow project has no chief of staff to message")
	}
	stream, err := config.ParseWorkstreamID(rawWorkstream)
	if err != nil {
		return "", "", nil, errors.New("workstream must be a workstream ID: w_ followed by 32 lowercase hexadecimal digits")
	}
	cfg, projects := s.runtimes()
	if !cfg.Active(project) {
		ids := cfg.ProjectIDs()
		if len(ids) == 0 {
			return "", "", nil, fmt.Errorf("project %s is not an active project; no project is registered", project)
		}
		return "", "", nil, fmt.Errorf("project %s is not an active project; the active projects are %s", project, projectList(ids))
	}
	for _, p := range projects {
		if p.id != project {
			continue
		}
		if stream == librarianWorkstream(project) {
			return "", "", nil, fmt.Errorf("workstream %s is not in project %s; list workstreams with osmia status", stream, project)
		}
		streams, err := p.repository.Workstreams()
		if err != nil {
			return "", "", nil, fmt.Errorf("cannot read the workstreams of project %s; check the trace repository", project)
		}
		if !slices.Contains(streams, stream) {
			return "", "", nil, fmt.Errorf("workstream %s is not in project %s; list workstreams with osmia status", stream, project)
		}
		return project, stream, p.repository, nil
	}
	return "", "", nil, fmt.Errorf("project %s has no open trace; check osmia status", project)
}

// messageChiefOfStaff records text as the next request of the target
// workstream's chief-of-staff thread, attributed to the Beekeeper and never
// the owner, and lets that project's own scheduler run the turn, the way an
// owner message does (service.send). Delivering the same key for the same
// project and workstream twice, including by recovery after a restart,
// finds the first delivery's queued turn and changes nothing further.
func (s *Service) messageChiefOfStaff(ctx context.Context, rawProject, rawWorkstream, text, key string) (trace.QueuedTurn, error) {
	if strings.TrimSpace(text) == "" {
		return trace.QueuedTurn{}, errors.New("text must not be empty")
	}
	if strings.TrimSpace(key) == "" {
		return trace.QueuedTurn{}, errors.New("key must not be empty")
	}
	project, stream, repository, err := s.beekeeperTarget(rawProject, rawWorkstream)
	if err != nil {
		return trace.QueuedTurn{}, err
	}
	cfg := s.current()
	name, profile, err := s.chiefOverride(cfg)
	if err != nil {
		return trace.QueuedTurn{}, fmt.Errorf("role %s has no usable profile %q; check osmia profiles", trace.ChiefOfStaff, name)
	}
	turnID := beekeeperMessageTurnID(project, stream, key)
	th, err := repository.EnsureChiefOfStaff(ctx, stream, s.now(), serviceActor)
	if err != nil {
		return trace.QueuedTurn{}, fmt.Errorf("cannot record the message for workstream %s; check the trace repository", stream)
	}
	for _, existing := range th.Turns {
		if existing.Request.TurnID == turnID {
			return existing, nil
		}
	}
	context, err := chiefContext(ctx, s.Context(), repository, project, stream)
	if err != nil {
		return trace.QueuedTurn{}, fmt.Errorf("cannot assemble the context of workstream %s; check the charter, the knowledge base and the trace repository", stream)
	}
	at := s.now()
	req := trace.TurnRequest{
		Header: trace.Header{Schema: "osmia.trace.turn-request", Version: trace.Version, ID: "request_" + turnID, Revision: 1,
			Project: project, Workstream: stream, At: at, Actor: beekeeperActor, Cause: "beekeeper-message"},
		AgentID:      trace.ChiefOfStaff,
		ThreadID:     trace.ChiefOfStaff,
		TurnID:       turnID,
		Profile:      profile,
		SystemPrompt: fmt.Sprintf(chiefPromptFromBeekeeper, stream) + "\n\n" + chiefDocumentsGuidance + "\n\n" + priorityGuidance + "\n\n" + pauseGuidance + "\n\n" + amendmentGuidance + "\n\n" + charterGuidance + "\n\n" + contestGuidance + "\n\n" + context,
		Prompt:       text,
	}
	q, err := repository.EnqueueTurn(ctx, req)
	if err != nil {
		if errors.Is(err, trace.ErrConflict) {
			if th, threadErr := repository.ChiefOfStaffThread(stream); threadErr == nil {
				for _, existing := range th.Turns {
					if existing.Request.TurnID == turnID {
						return existing, nil
					}
				}
			}
		}
		return trace.QueuedTurn{}, fmt.Errorf("cannot record the message for workstream %s; check the trace repository", stream)
	}
	return q, nil
}

// messageChiefOfStaff returns the Beekeeper's message-a-chief-of-staff
// tool. Only a claimed turn of the Beekeeper's own thread may call it; any
// other scope is refused and nothing changes.
func (c *runtimeControls) messageChiefOfStaff(scope coreadapter.Scope) coreadapter.Tool {
	return coreadapter.Tool{
		Name:   messageChiefOfStaffTool,
		Effect: coreadapter.ToolMemory,
		Description: "Send text to the chief of staff of workstream in project. The message is recorded in that chief of staff's thread attributed to the Beekeeper, never the owner, and runs a chief-of-staff turn. " +
			"project and workstream must name a registered project and one of its workstreams; the shadow project and an unknown project or workstream are refused. " +
			"key identifies this one call: reuse the exact same key, for the same project and workstream, only to retry a call you are not sure delivered; never reuse it for a different message, so a repeated delivery does not land twice.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"project":{"type":"string"},"workstream":{"type":"string"},"text":{"type":"string"},"key":{"type":"string"}},"required":["project","workstream","text","key"],"additionalProperties":false}`),
		Handle: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			if scope.Role != beekeeper.AgentID {
				return nil, errors.New(messageChiefOfStaffTool + " is granted to the beekeeper only")
			}
			var input struct {
				Project    string `json:"project"`
				Workstream string `json:"workstream"`
				Text       string `json:"text"`
				Key        string `json:"key"`
			}
			d := json.NewDecoder(bytes.NewReader(raw))
			d.DisallowUnknownFields()
			if err := d.Decode(&input); err != nil {
				return nil, fmt.Errorf("tool input: %w", err)
			}
			if err := d.Decode(new(any)); err != io.EOF {
				return nil, errors.New(messageChiefOfStaffTool + " input must be one object")
			}
			s := c.service.Load()
			if s == nil {
				return nil, errors.New("the service is not ready")
			}
			if err := s.Beekeeper().ActiveTurn(scope); err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			q, err := s.messageChiefOfStaff(ctx, input.Project, input.Workstream, input.Text, input.Key)
			if err != nil {
				return nil, err
			}
			return json.Marshal(struct {
				Delivered  bool   `json:"delivered"`
				Project    string `json:"project"`
				Workstream string `json:"workstream"`
				Turn       string `json:"turn"`
			}{true, input.Project, input.Workstream, q.Request.TurnID})
		},
	}
}
