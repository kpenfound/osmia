package coreadapter

import (
	"context"
	"encoding/json"
)

// OperationBoundary names a local, service-owned effect. Delivery is not a local boundary.
type OperationBoundary string

const (
	RepositoryBoundary OperationBoundary = "repository"
	RunnerBoundary     OperationBoundary = "runner"
	ContainerBoundary  OperationBoundary = "container"
)

// Operation identifies immutable input across attempts. Input encodes the relevant
// WorkspaceRequest, PreparedTurn or SandboxRequest; it must contain no secret values.
type Operation struct {
	ID       string            `json:"id"`
	Boundary OperationBoundary `json:"boundary"`
	Action   string            `json:"action"`
	Input    json.RawMessage   `json:"input"`
}

type ObservationState string

const (
	EffectAbsent    ObservationState = "absent"
	EffectCompleted ObservationState = "completed"
	EffectUnknown   ObservationState = "unknown"
)

// OperationResult is terminal evidence, including unsuccessful domain outcomes.
// Infrastructure failures instead leave an operation pending for inspection.
type OperationResult struct {
	Outcome  string          `json:"outcome"`
	Evidence string          `json:"evidence"`
	Data     json.RawMessage `json:"data,omitempty"`
}
type Observation struct {
	State    ObservationState `json:"state"`
	Evidence string           `json:"evidence"`
	Result   *OperationResult `json:"result,omitempty"`
}

// Reconciler must inspect the external system by Operation.ID, not process memory.
// Absent proves that no previous attempt is running or can subsequently complete.
// Running, unreachable and unidentifiable effects are Unknown. Completed includes
// a durable terminal result. Apply must preserve ID in the external resource or
// session identity so inspection after a crash can recover it. Unsupported
// identity/inspection capabilities must fail closed before launching an effect.
// Implementations wrap the Workspaces, Turns and Sandboxes execution contracts;
// they never change workflow state or grant permission to execute.
type Reconciler interface {
	Inspect(context.Context, Operation) (Observation, error)
	Apply(context.Context, Operation) (OperationResult, error)
}
