package telemetry

import (
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/osmia/internal/config"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// The attribute keys a span may carry. Anything else is dropped; a new key
// is added here, with its value kind, and nowhere else.
var (
	KeyRole          = Key("role")
	KeyBackend       = Key("backend")
	KeyModel         = Key("model")
	KeyProfile       = Key("profile")
	KeyProject       = Key("project", "id")
	KeyWorkstream    = Key("workstream", "id")
	KeyUnit          = Key("unit", "id")
	KeyRecord        = Key("record", "id")
	KeyClient        = Key("client", "id")
	KeyPool          = Key("pool", "id")
	KeyPoolRoles     = Key("pool", "roles")
	KeyPoolLimit     = Key("pool", "limit")
	KeyPoolUnbounded = Key("pool", "unbounded")
	KeyOutcome       = Key("outcome")
	KeyCause         = Key("cause")
	KeyState         = Key("state")
	KeyNextState     = Key("next_state")
	KeyWaitReason    = Key("wait_reason")
	KeyKind          = Key("kind")
	KeyPauseScope    = Key("pause", "scope")
	KeyPauseSource   = Key("pause", "source")
	KeyCovered       = Key("covered")
	KeySlotWaitMS    = Key("slot_wait_ms")
	KeyCostUSD       = Key("cost_usd")
	KeyCostKnown     = Key("cost_known")
	KeyDecidedBy     = Key("decided_by")
	KeyDefaultState  = Key("default_state")
	KeyDeviated      = Key("deviated")
	KeyTrigger       = Key("trigger")
	KeyConfidence    = Key("confidence")
	KeyJudgment      = Key("judgment")

	KeyVerifyRuns              = Key("verify", "runs")
	KeyVerifyPass              = Key("verify", "pass")
	KeyVerifyFail              = Key("verify", "fail")
	KeyVerifyInconclusive      = Key("verify", "inconclusive")
	KeyVerifyLimited           = Key("verify", "limited")
	KeyVerifyDeclaredRuns      = Key("verify", "declared_runs")
	KeyVerifyDeclaredFlows     = Key("verify", "declared_flows")
	KeyVerifyDeclaredUntagged  = Key("verify", "declared_untagged")
	KeyVerifyReviewerRuns      = Key("verify", "reviewer_runs")
	KeyVerifyReviewerFlows     = Key("verify", "reviewer_flows")
	KeyVerifyReviewerUntagged  = Key("verify", "reviewer_untagged")
	KeyVerifySkipped           = Key("verify", "skipped")
	KeyVerifyRefused           = Key("verify", "refused")
	KeyVerifyLeverTouched      = Key("verify", "lever_touched")
	KeyVerifyMapTouched        = Key("verify", "map_touched")
	KeyVerifyFlow              = Key("verify", "flow")
	KeyVerifyUnits             = Key("verify", "units")
	KeyVerifyShared            = Key("verify", "shared")
	KeyVerifyUnattributed      = Key("verify", "unattributed")
	KeyVerifyFailedFlows       = Key("verify", "failed_flows")
	KeyVerifyInconclusiveFlows = Key("verify", "inconclusive_flows")
	KeyVerifyUnattributedFails = Key("verify", "unattributed_failures")
	KeyVerifyFailingUnits      = Key("verify", "failing_units")
	KeyVerifyInconclusiveUnits = Key("verify", "inconclusive_units")
)

// The OpenTelemetry GenAI convention attributes a turn span may carry. They
// are the keys of the pinned semantic conventions version (see
// GenAIConventionsVersion), not Osmia's own.
var (
	KeyGenAIOperation = string(semconv.GenAIOperationNameKey)
	KeyGenAIProvider  = string(semconv.GenAIProviderNameKey)
	KeyGenAIModel     = string(semconv.GenAIRequestModelKey)
	KeyGenAIAgentName = string(semconv.GenAIAgentNameKey)
	KeyGenAIAgentID   = string(semconv.GenAIAgentIDKey)
)

type kind int

const (
	kindID kind = iota + 1
	kindToken
	kindIDs
	kindInt
	kindFloat
	kindBool
)

var allowed = map[string]kind{
	KeyRole: kindID, KeyBackend: kindID, KeyModel: kindID, KeyProfile: kindID,
	KeyProject: kindID, KeyWorkstream: kindID, KeyUnit: kindID, KeyRecord: kindID, KeyClient: kindID,
	KeyPool: kindID, KeyPoolRoles: kindIDs, KeyPoolLimit: kindInt, KeyPoolUnbounded: kindBool,
	KeyOutcome: kindToken, KeyCause: kindToken, KeyState: kindToken, KeyNextState: kindToken,
	KeyWaitReason: kindToken, KeyKind: kindToken, KeyPauseScope: kindToken, KeyPauseSource: kindToken,
	KeyCovered: kindInt, KeySlotWaitMS: kindInt, KeyCostUSD: kindFloat, KeyCostKnown: kindBool,
	KeyDecidedBy: kindToken, KeyDefaultState: kindToken, KeyDeviated: kindBool,
	KeyTrigger: kindToken, KeyConfidence: kindToken, KeyJudgment: kindID,
	KeyGenAIOperation: kindToken, KeyGenAIProvider: kindID, KeyGenAIModel: kindID,
	KeyGenAIAgentName: kindID, KeyGenAIAgentID: kindID,

	KeyVerifyRuns: kindInt, KeyVerifyPass: kindInt, KeyVerifyFail: kindInt, KeyVerifyInconclusive: kindInt, KeyVerifyLimited: kindInt,
	KeyVerifyDeclaredRuns: kindInt, KeyVerifyDeclaredFlows: kindInt, KeyVerifyDeclaredUntagged: kindInt,
	KeyVerifyReviewerRuns: kindInt, KeyVerifyReviewerFlows: kindInt, KeyVerifyReviewerUntagged: kindInt,
	KeyVerifySkipped: kindInt, KeyVerifyRefused: kindInt, KeyVerifyLeverTouched: kindBool, KeyVerifyMapTouched: kindBool,
	KeyVerifyFlow: kindID, KeyVerifyUnits: kindIDs, KeyVerifyShared: kindBool, KeyVerifyUnattributed: kindBool,
	KeyVerifyFailedFlows: kindInt, KeyVerifyInconclusiveFlows: kindInt, KeyVerifyUnattributedFails: kindInt,
	KeyVerifyFailingUnits: kindInt, KeyVerifyInconclusiveUnits: kindInt,
}

// AttributeKeys lists every attribute key a span may carry, sorted.
func AttributeKeys() []string {
	keys := make([]string, 0, len(allowed))
	for k := range allowed {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

var (
	identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+=-]{0,127}$`)
	token      = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
)

// Attr is one span attribute. Only the constructors below make a usable one:
// a key outside the allow-list, a value of the wrong kind or a value that is
// not identifier-shaped gives the zero Attr, which a span drops.
type Attr struct{ kv attribute.KeyValue }

// Valid reports whether a is an allow-listed attribute.
func (a Attr) Valid() bool { return a.kv.Key != "" }

// Key returns a's key, or "" for a dropped attribute.
func (a Attr) Key() string { return string(a.kv.Key) }

// Value returns a's value as the OpenTelemetry attribute value.
func (a Attr) Value() attribute.Value { return a.kv.Value }

// String is an allow-listed string attribute with an identifier-shaped value.
func String(key, value string) Attr {
	switch allowed[key] {
	case kindID:
		if identifier.MatchString(value) {
			return Attr{attribute.String(key, value)}
		}
	case kindToken:
		if token.MatchString(value) {
			return Attr{attribute.String(key, value)}
		}
	}
	return Attr{}
}

// Strings is an allow-listed list-of-strings attribute whose every value is
// identifier-shaped.
func Strings(key string, values []string) Attr {
	if allowed[key] != kindIDs || len(values) == 0 || len(values) > 64 {
		return Attr{}
	}
	for _, v := range values {
		if !identifier.MatchString(v) {
			return Attr{}
		}
	}
	return Attr{attribute.StringSlice(key, values)}
}

// Int is an allow-listed non-negative integer attribute.
func Int(key string, value int64) Attr {
	if allowed[key] != kindInt || value < 0 {
		return Attr{}
	}
	return Attr{attribute.Int64(key, value)}
}

// Float is an allow-listed non-negative finite number attribute.
func Float(key string, value float64) Attr {
	if allowed[key] != kindFloat || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return Attr{}
	}
	return Attr{attribute.Float64(key, value)}
}

// Bool is an allow-listed boolean attribute.
func Bool(key string, value bool) Attr {
	if allowed[key] != kindBool {
		return Attr{}
	}
	return Attr{attribute.Bool(key, value)}
}

// Role is the role whose work or capacity the span describes.
func Role(role string) Attr { return String(KeyRole, role) }

// Backend is the agent backend that ran a turn.
func Backend(backend string) Attr { return String(KeyBackend, backend) }

// Model is the model a turn ran with.
func Model(model string) Attr { return String(KeyModel, model) }

// Profile is the name of the profile a turn ran with.
func Profile(profile string) Attr { return String(KeyProfile, profile) }

// Project is the project a span belongs to.
func Project(id config.ProjectID) Attr { return String(KeyProject, string(id)) }

// Workstream is the workstream a span belongs to.
func Workstream(id config.WorkstreamID) Attr { return String(KeyWorkstream, string(id)) }

// Unit is the unit a span belongs to.
func Unit(id string) Attr { return String(KeyUnit, id) }

// Record is the ID of the trace record a span was derived from.
func Record(id string) Attr { return String(KeyRecord, id) }

// Client is the identity of the client a turn ran on or a slot belongs to: the
// tailnet node's stable ID, or local for the server's own machine.
func Client(id string) Attr { return String(KeyClient, id) }

// Pool is the identity of a capacity pool; a pool shared across projects has
// one identity.
func Pool(id string) Attr { return String(KeyPool, id) }

// PoolRoles lists the roles a capacity pool serves.
func PoolRoles(roles []string) Attr { return Strings(KeyPoolRoles, roles) }

// PoolLimit is the limit of a bounded pool. An unbounded pool has
// PoolUnbounded instead.
func PoolLimit(limit int) Attr { return Int(KeyPoolLimit, int64(limit)) }

// PoolUnbounded marks a pool without a limit.
func PoolUnbounded() Attr { return Bool(KeyPoolUnbounded, true) }

// Outcome is how the interval the span covers ended, as an enumerated word.
func Outcome(outcome string) Attr { return String(KeyOutcome, outcome) }

// Cause is what caused the transition that ended the interval, as an
// enumerated word.
func Cause(cause string) Attr { return String(KeyCause, cause) }

// State is the state a state span covers.
func State(state string) Attr { return String(KeyState, state) }

// NextState is the state a transition entered.
func NextState(state string) Attr { return String(KeyNextState, state) }

// DecidedBy says who chose the transition that ended the interval, as an
// enumerated word.
func DecidedBy(by string) Attr { return String(KeyDecidedBy, by) }

// DefaultState is the state the transition router's default would have
// entered.
func DefaultState(state string) Attr { return String(KeyDefaultState, state) }

// Deviated records whether a transition entered a state other than its
// default.
func Deviated(deviated bool) Attr { return Bool(KeyDeviated, deviated) }

// Trigger is what raised an exit to the transition router, as an enumerated
// word.
func Trigger(trigger string) Attr { return String(KeyTrigger, trigger) }

// Confidence is the band of Jev's confidence in the state a route chose, as
// an enumerated word.
func Confidence(band string) Attr { return String(KeyConfidence, band) }

// Judgment is the ID of the Jev judgment record behind a decision.
func Judgment(id string) Attr { return String(KeyJudgment, id) }

// WaitReason is why work waited for capacity, as an enumerated word.
func WaitReason(reason string) Attr { return String(KeyWaitReason, reason) }

// CheckKind names the kind of a check run.
func CheckKind(name string) Attr { return String(KeyKind, name) }

// PauseScope is the scope of a pause, as an enumerated word.
func PauseScope(scope string) Attr { return String(KeyPauseScope, scope) }

// PauseSource is who or what set a pause, as an enumerated word.
func PauseSource(source string) Attr { return String(KeyPauseSource, source) }

// Covered is the number of checks a check run covered.
func Covered(n int) Attr { return Int(KeyCovered, int64(n)) }

// SlotWait is how long a turn waited for its slot.
func SlotWait(d time.Duration) Attr { return Int(KeySlotWaitMS, d.Milliseconds()) }

// CostUSD is a turn's cost in US dollars, given only when the backend
// reported it; CostKnown(false) says it did not.
func CostUSD(usd float64) Attr { return Float(KeyCostUSD, usd) }

// CostKnown records whether the backend reported the turn's cost.
func CostKnown(known bool) Attr { return Bool(KeyCostKnown, known) }

// GenAIOperation is the gen_ai.operation.name of a turn span, such as
// "invoke_agent".
func GenAIOperation(operation string) Attr { return String(KeyGenAIOperation, operation) }

// GenAIProvider is the gen_ai.provider.name of a turn span.
func GenAIProvider(provider string) Attr {
	return String(KeyGenAIProvider, strings.ToLower(provider))
}

// GenAIModel is the gen_ai.request.model of a turn span.
func GenAIModel(model string) Attr { return String(KeyGenAIModel, model) }

// GenAIAgentName is the gen_ai.agent.name of a turn span.
func GenAIAgentName(name string) Attr { return String(KeyGenAIAgentName, name) }

// GenAIAgentID is the gen_ai.agent.id of a turn span.
func GenAIAgentID(id string) Attr { return String(KeyGenAIAgentID, id) }

// VerifyCount is one of the integer measures of verification, given by its
// key: a run count, a flow count or a unit count.
func VerifyCount(key string, n int) Attr { return Int(key, int64(n)) }

// VerifyFlow is the flow a final measure span covers.
func VerifyFlow(flow string) Attr { return String(KeyVerifyFlow, flow) }

// VerifyUnits lists the approved units a final failure or an inconclusive flow
// is attributed to.
func VerifyUnits(units []string) Attr { return Strings(KeyVerifyUnits, units) }

// VerifyFlag is one of the boolean markers of verification, given by its key:
// the lever or the map was touched, or a final failure is shared or
// unattributed.
func VerifyFlag(key string, set bool) Attr { return Bool(key, set) }
