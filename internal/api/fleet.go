package api

import (
	"encoding/json"
	"strings"

	"github.com/kpenfound/osmia/internal/coreadapter"
)

// ClientProtocolVersion is the version of the client protocol. The server and
// `osmia client` both compile it in, and the server refuses a client that
// speaks another.
const ClientProtocolVersion = 1

// The client protocol codes beyond the common ones.
const (
	// VersionMismatch answers a connect from a client of another protocol
	// version. Its message names both versions.
	VersionMismatch Code = "client_version_mismatch"
	// ClientConnected answers a connect from a node whose earlier connection
	// still holds its lease.
	ClientConnected Code = "client_connected"
	// LeaseEnded answers a request on a lease that has ended or was never
	// granted; the client connects again.
	LeaseEnded Code = "lease_ended"
)

// ClientRoute is the path of a client protocol route below one lease, such
// as heartbeat or turns/{turn}/result.
func ClientRoute(lease, rest string) string {
	return Prefix + "/clients/" + lease + "/" + rest
}

// ClientConnectPath is the route a client connects on.
const ClientConnectPath = Prefix + "/clients/connect"

// ClientRouteParts splits a path under /v1/clients/ into its lease and the
// path below it. Either may be empty.
func ClientRouteParts(path string) (lease, rest string, ok bool) {
	tail, ok := strings.CutPrefix(path, Prefix+"/clients/")
	if !ok {
		return "", "", false
	}
	lease, rest, _ = strings.Cut(tail, "/")
	return lease, rest, true
}

// ClientIdentity names a client by its tailnet node: the node's stable ID,
// which the server places work by, and its name for display.
type ClientIdentity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ClientConnectRequest opens a client's connection. Lease and Running are
// set by a client that lost its connection and reconnects: Lease is the lease
// it held and Running the turns its sessions are still running, which it
// reattaches by ID.
type ClientConnectRequest struct {
	Version  int      `json:"version"`
	Slots    int      `json:"slots"`
	Draining bool     `json:"draining,omitempty"`
	Lease    string   `json:"lease,omitempty"`
	Running  []string `json:"running,omitempty"`
}

// The answers to a turn a reconnecting client still runs.
const (
	// TurnReattached means the server holds the turn for the client again.
	TurnReattached = "reattached"
	// TurnLost means the server no longer holds the turn for the client, so the
	// client cancels it and sends back its view and result.
	TurnLost = "lost"
)

// TurnReattach is the answer to one turn a client reported running when it
// connected. Token is set when the server restarted and issued the turn a new
// bearer token for its view upload: the one the client held died with the
// server's process.
type TurnReattach struct {
	Turn   string `json:"turn"`
	Answer string `json:"answer" enum:"reattached,lost"`
	Token  string `json:"token,omitempty"`
}

// ClientConnectResponse grants a lease. The client keeps it alive with
// heartbeats at most HeartbeatSeconds apart; the lease ends GraceSeconds after
// the server last heard from the client.
type ClientConnectResponse struct {
	Version          int            `json:"version"`
	Lease            string         `json:"lease"`
	Client           ClientIdentity `json:"client"`
	Resumed          bool           `json:"resumed,omitempty"`
	HeartbeatSeconds int            `json:"heartbeat_seconds"`
	GraceSeconds     int            `json:"grace_seconds"`
	PollSeconds      int            `json:"poll_seconds"`
	Profiles         ProfileHandout `json:"profiles"`
	Turns            []TurnReattach `json:"turns,omitempty"`
}

// ProfileSpec is one configured profile as a client needs it to check that it
// can run it. It holds no credential.
type ProfileSpec struct {
	Name     string `json:"name"`
	Backend  string `json:"backend"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"`
	Fallback string `json:"fallback,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
	MaxTurns int    `json:"max_turns,omitempty"`
}

// DaggerRequirement is the Dagger a mason's sandbox uses: the server's engine
// mode and the release it expects. The engine address and any token stay on
// the server; the client uses its own Dagger setting.
type DaggerRequirement struct {
	Mode    string `json:"mode"`
	Version string `json:"version,omitempty"`
}

// RoleRequirement is what turns of one role need from the client that runs
// them. Profile is the role's main profile and Chain that profile followed by
// its fallbacks in order; Backend and Model are the main profile's.
type RoleRequirement struct {
	Role          string             `json:"role"`
	Profile       string             `json:"profile"`
	EventsProfile string             `json:"events_profile,omitempty"`
	Chain         []string           `json:"chain"`
	Backend       string             `json:"backend"`
	Model         string             `json:"model"`
	Sandbox       string             `json:"sandbox"`
	Image         string             `json:"image,omitempty"`
	CPUs          int                `json:"cpus,omitempty"`
	Memory        string             `json:"memory,omitempty"`
	Domains       []string           `json:"domains"`
	Skills        []string           `json:"skills"`
	Dagger        *DaggerRequirement `json:"dagger,omitempty"`
}

// ProfileHandout is what the server sends a client on connect and again after
// every configuration reload. Revision changes whenever the content does.
type ProfileHandout struct {
	Revision string            `json:"revision"`
	Profiles []ProfileSpec     `json:"profiles"`
	Roles    []RoleRequirement `json:"roles"`
}

// ProfileProblem is one thing a client lacks to run a profile, with the fix.
type ProfileProblem struct {
	Check  string `json:"check"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// ProfileCapability is whether a client can run one profile and, when it
// cannot, what is missing.
type ProfileCapability struct {
	Profile  string           `json:"profile"`
	Runnable bool             `json:"runnable"`
	Problems []ProfileProblem `json:"problems,omitempty"`
}

// CapabilityReport is a client's account of the profiles it can and cannot
// run. Revision is the handout it checked.
type CapabilityReport struct {
	Revision string              `json:"revision"`
	Profiles []ProfileCapability `json:"profiles"`
}

// CapabilityResponse acknowledges a capability report. Current is false when
// the report checked an older handout than the one the server now holds.
type CapabilityResponse struct {
	Current bool `json:"current"`
}

// TurnProgress is what a heartbeat says of one running turn.
type TurnProgress struct {
	Turn     string   `json:"turn"`
	Phase    string   `json:"phase"`
	Progress *float64 `json:"progress,omitempty"`
}

// HeartbeatRequest keeps the lease alive and reports each running turn's
// progress.
type HeartbeatRequest struct {
	Turns []TurnProgress `json:"turns"`
}

// HeartbeatResponse answers a heartbeat. Lost lists reported turns the server
// does not hold for the client.
type HeartbeatResponse struct {
	Draining bool     `json:"draining"`
	Lost     []string `json:"lost,omitempty"`
}

// DrainRequest tells the server the client is draining: it takes no new turns
// and keeps the lease until its running turns are sent.
type DrainRequest struct{}

// DrainResponse acknowledges a drain.
type DrainResponse struct {
	Draining bool `json:"draining"`
}

// LeaveRequest ends the lease at once, which a client sends as it exits.
type LeaveRequest struct{}

// LeaveResponse acknowledges that the lease ended.
type LeaveResponse struct {
	Ended bool `json:"ended"`
}

// The kinds of message the server sends a client.
const (
	MessageProfiles = "profiles"
	MessageOffer    = "offer"
	MessageCancel   = "cancel"
	MessageRequest  = "request"
)

// The ways a turn's view reaches a client.
const (
	// ViewGit is built on the client from its cached clone at Commit, with Paths
	// exported.
	ViewGit = "git"
	// ViewArchive is downloaded as an archive from the turn's view route.
	ViewArchive = "archive"
)

// TurnView is how a client gets a turn's starting files.
type TurnView struct {
	Kind    string   `json:"kind" enum:"git,archive"`
	Project string   `json:"project,omitempty"`
	Commit  string   `json:"commit,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	// Overlay says the server holds files to place over the built view, which
	// the client downloads as an archive from the turn's view route.
	Overlay bool `json:"overlay,omitempty"`
}

// TurnOffer offers a turn to a client. The client answers with a claim.
//
// ToolEndpoint is the path, on the server the client connected to, of the
// turn's MCP relay, and ToolToken the turn's bearer token: the client's local
// listener forwards each tool call there with that token. The same token
// authorises the turn's view download and upload.
//
// The offer carries what the client needs to run the session besides its
// view and holds no secret: the profile as the server resolved it, the
// prompts, the tools and permissions the role is granted, the sandbox
// settings (with no Dagger Cloud token variable) and the public environment.
// A client uses its own provider login and its own Dagger setting.
type TurnOffer struct {
	Turn       string   `json:"turn"`
	Project    string   `json:"project,omitempty"`
	Workstream string   `json:"workstream,omitempty"`
	Unit       string   `json:"unit,omitempty"`
	Thread     string   `json:"thread,omitempty"`
	Role       string   `json:"role"`
	Profile    string   `json:"profile"`
	Fallbacks  []string `json:"fallbacks,omitempty"`
	// Settings is the main profile as the server resolved it for this turn.
	Settings        coreadapter.Profile           `json:"settings"`
	ResumeID        string                        `json:"resume_id,omitempty"`
	SystemPrompt    string                        `json:"system_prompt,omitempty"`
	Prompt          string                        `json:"prompt,omitempty"`
	History         string                        `json:"history,omitempty"`
	AllowedOutcomes []string                      `json:"allowed_outcomes,omitempty"`
	Capabilities    coreadapter.Capabilities      `json:"capabilities"`
	Execution       coreadapter.ExecutionSettings `json:"execution"`
	// RunsDagger is whether the turn's sandbox runs Dagger, which gives its
	// view the empty Git repository Dagger needs at its root.
	RunsDagger   bool              `json:"runs_dagger,omitempty"`
	Environment  map[string]string `json:"environment,omitempty"`
	ToolEndpoint string            `json:"tool_endpoint"`
	ToolToken    string            `json:"tool_token"`
	View         TurnView          `json:"view"`
	// ReturnView is whether the client uploads the finished view when the turn
	// ends: a turn that writes files does and the server records its commit
	// from it.
	ReturnView bool `json:"return_view,omitempty"`
}

// TurnCancel asks a client to cancel a turn it runs or was offered.
type TurnCancel struct {
	Turn   string `json:"turn"`
	Reason string `json:"reason,omitempty"`
}

// The operations the server asks a client to carry out on a running turn's
// view and skills.
const (
	OpViewRead   = "view_read"
	OpViewWrite  = "view_write"
	OpSkillRead  = "skill_read"
	OpViewExport = "view_export"
)

// ServerRequest is a request the server makes of a client on the client's own
// connection: the operation to carry out on a turn's view or skills. The
// client replies on the request's reply route.
type ServerRequest struct {
	ID     string          `json:"id"`
	Turn   string          `json:"turn"`
	Op     string          `json:"op"`
	Params json.RawMessage `json:"params,omitempty" type:"object"`
}

// ViewReadParams are the parameters of OpViewRead and OpSkillRead.
type ViewReadParams struct {
	Path string `json:"path"`
}

// ViewReadResult is the result of OpViewRead and OpSkillRead: a file's
// content, or for a directory its entry names, one per line with a trailing
// slash on each directory.
type ViewReadResult struct {
	Data []byte `json:"data"`
}

// ViewWriteParams are the parameters of OpViewWrite.
type ViewWriteParams struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

// ServerReply is a client's reply to a server request: the result, or why it
// could not be carried out.
type ServerReply struct {
	Result json.RawMessage `json:"result,omitempty" type:"object"`
	Error  string          `json:"error,omitempty"`
}

// ServerReplyResponse acknowledges a reply. Answered is false when the server
// no longer waits for it.
type ServerReplyResponse struct {
	Answered bool `json:"answered"`
}

// Message is one server-to-client message. Seq orders them; a client
// acknowledges the highest Seq it has handled on its next poll, and the server
// sends again what is not acknowledged.
type Message struct {
	Seq      int64           `json:"seq"`
	Kind     string          `json:"kind" enum:"profiles,offer,cancel,request"`
	Profiles *ProfileHandout `json:"profiles,omitempty"`
	Offer    *TurnOffer      `json:"offer,omitempty"`
	Cancel   *TurnCancel     `json:"cancel,omitempty"`
	Request  *ServerRequest  `json:"request,omitempty"`
}

// PollRequest waits up to WaitSeconds for messages, after acknowledging every
// message up to Ack.
type PollRequest struct {
	Ack         int64 `json:"ack"`
	WaitSeconds int   `json:"wait_seconds,omitempty"`
}

// PollResponse carries the messages the server holds for the client, none when
// the wait ended first.
type PollResponse struct {
	Messages []Message `json:"messages"`
}

// ClaimRequest answers a turn offer: the client takes the turn, or declines it
// with a reason.
type ClaimRequest struct {
	Accept bool   `json:"accept"`
	Reason string `json:"reason,omitempty"`
}

// The answers to a claim.
const (
	// ClaimTaken means the server holds the turn for the client.
	ClaimTaken = "taken"
	// ClaimGone means the offer was withdrawn first; the client must not run the
	// turn.
	ClaimGone = "gone"
	// ClaimReleased acknowledges a client's refusal of the offer.
	ClaimReleased = "released"
)

// ClaimResponse is the server's answer to a claim.
type ClaimResponse struct {
	Answer string `json:"answer" enum:"taken,gone,released"`
}

// TurnResult is what a client returns when a turn ends: the session's result
// with everything the server reads from the session directory afterwards.
type TurnResult struct {
	Result       coreadapter.SessionResult `json:"result"`
	SessionFiles []SessionFile             `json:"session_files,omitempty"`
}

// SessionFile is one file of a turn's session directory, which the server reads
// after the turn.
type SessionFile struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// TurnResultResponse is the definitive answer to a returned result: accepted,
// already recorded or superseded.
type TurnResultResponse struct {
	Answer ViewAnswer `json:"answer" enum:"accepted,already_recorded,superseded"`
}
