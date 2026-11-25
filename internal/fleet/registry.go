// Package fleet is the server side of the client protocol: the registry of
// connected clients and their leases, the profile handout, the capability
// reports and the channel the server uses to reach a client over the
// client's own outbound connection.
//
// A client connects out, is identified by its tailnet node, holds one lease
// and keeps it alive with heartbeats. The server never opens a connection to
// a client: it queues messages (turn offers, cancels and requests to carry
// out a view or skill operation) that the client collects by long poll, and
// the client replies with ordinary requests.
package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/api"
)

// The lease timings the server grants.
const (
	// DefaultGrace is how long a lease survives without hearing from its
	// client.
	DefaultGrace = 2 * time.Minute
	// DefaultHeartbeat is how often a client heartbeats.
	DefaultHeartbeat = 10 * time.Second
	// DefaultPoll is the longest a poll waits for a message.
	DefaultPoll = 20 * time.Second
	// lateIntervals is how many heartbeat intervals of silence make a lease
	// late.
	lateIntervals = 3
)

// The health of a lease.
const (
	// LeaseHealthy means the client was heard from recently.
	LeaseHealthy = "healthy"
	// LeaseLate means heartbeats are missing but the grace window has not
	// passed.
	LeaseLate = "late"
)

// Identity is the tailnet node a client connects from.
type Identity = api.ClientIdentity

// Errors the registry returns for calls it cannot carry out. Each is an
// *api.APIError when it comes from a client's request.
var (
	// ErrNoClient is returned for a client that holds no lease.
	ErrNoClient = errors.New("the client holds no lease")
	// ErrLeaseEnded is returned when the lease ended while a call waited.
	ErrLeaseEnded = errors.New("the client's lease ended")
	// ErrDraining is returned when a turn is offered to a draining client.
	ErrDraining = errors.New("the client is draining")
)

// RemoteError is a failure the client reports for a request the server made.
type RemoteError struct{ Message string }

func (e *RemoteError) Error() string { return e.Message }

// Claim is a client's answer to a turn offer.
type Claim struct {
	Accepted bool
	Reason   string
}

// Hooks connect the protocol to turn execution. Any may be nil.
type Hooks struct {
	// Progress receives the progress a heartbeat carries for a turn the client
	// holds.
	Progress func(Client, api.TurnProgress)
	// Reattach decides whether the server holds a turn for a client that
	// reports it still runs it and the registry does not know it, as after a
	// server restart. When it holds the turn it returns the bearer token the
	// turn's view upload takes from now on, which the client is told.
	Reattach func(context.Context, Identity, string) (token string, held bool)
	// Result takes a turn's returned result and answers it definitively.
	Result func(context.Context, Client, string, api.TurnResult) (api.ViewAnswer, error)
	// LeaseEnded receives a client whose lease ended and the turns it held.
	LeaseEnded func(Client, []string)
}

// Held is a turn a client runs.
type Held struct {
	Turn     string
	Since    time.Time
	Progress api.TurnProgress
}

// Client is a connected client as placement and visibility read it.
type Client struct {
	Identity    Identity
	Lease       string
	Slots       int
	Draining    bool
	Health      string
	ConnectedAt time.Time
	LastSeen    time.Time
	// LeaseEndsAt is when the lease ends unless the client is heard from.
	LeaseEndsAt time.Time
	// Capabilities is the client's latest report, nil before it sends one.
	Capabilities *api.CapabilityReport
	// Revision is the handout revision the server holds now.
	Revision string
	// Held are the turns the client runs, and Offered the turns offered and not
	// yet claimed; both take a slot.
	Held    []Held
	Offered []string
}

// Running is how many slots the client's turns and offers take.
func (c Client) Running() int { return len(c.Held) + len(c.Offered) }

// Free is how many slots the client has left.
func (c Client) Free() int { return max(c.Slots-c.Running(), 0) }

// CanRun reports whether the client's report says it can run profile.
func (c Client) CanRun(profile string) bool {
	if c.Capabilities == nil {
		return false
	}
	for _, p := range c.Capabilities.Profiles {
		if p.Profile == profile {
			return p.Runnable
		}
	}
	return false
}

// ReportCurrent reports whether the client's report checked the handout the
// server holds now.
func (c Client) ReportCurrent() bool {
	return c.Capabilities != nil && c.Capabilities.Revision == c.Revision
}

// Options configure a registry. The zero value takes the defaults and the
// wall clock.
type Options struct {
	Now       func() time.Time
	Grace     time.Duration
	Heartbeat time.Duration
	Poll      time.Duration
}

// Registry is the set of connected clients.
type Registry struct {
	now       func() time.Time
	grace     time.Duration
	heartbeat time.Duration
	poll      time.Duration

	mu      sync.Mutex
	hooks   Hooks
	handout api.ProfileHandout
	byNode  map[string]*lease
	byLease map[string]*lease
}

type lease struct {
	id          string
	identity    Identity
	slots       int
	draining    bool
	connectedAt time.Time
	lastSeen    time.Time
	generation  int
	caps        *api.CapabilityReport
	held        map[string]*Held
	offers      map[string]*offer
	calls       map[string]*call
	seq         int64
	queue       []api.Message
	wake        chan struct{}
	ended       chan struct{}
}

type offer struct{ done chan Claim }

type call struct{ done chan api.ServerReply }

// NewRegistry returns an empty registry.
func NewRegistry(o Options) *Registry {
	r := &Registry{now: o.Now, grace: o.Grace, heartbeat: o.Heartbeat, poll: o.Poll, byNode: map[string]*lease{}, byLease: map[string]*lease{}}
	if r.now == nil {
		r.now = time.Now
	}
	if r.grace <= 0 {
		r.grace = DefaultGrace
	}
	if r.heartbeat <= 0 {
		r.heartbeat = DefaultHeartbeat
	}
	if r.poll <= 0 {
		r.poll = DefaultPoll
	}
	return r
}

// SetHooks connects the registry to turn execution.
func (r *Registry) SetHooks(h Hooks) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks = h
}

func (r *Registry) hooksNow() Hooks {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hooks
}

// PollWindow is the longest a poll waits for a message.
func (r *Registry) PollWindow() time.Duration { return r.poll }

// Grace is how long a lease survives without its client.
func (r *Registry) Grace() time.Duration { return r.grace }

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newLease(ident Identity, slots int, now time.Time) *lease {
	return &lease{id: randomID(), identity: ident, slots: slots, connectedAt: now, lastSeen: now,
		held: map[string]*Held{}, offers: map[string]*offer{}, calls: map[string]*call{}, wake: make(chan struct{}), ended: make(chan struct{})}
}

func (l *lease) signal() {
	close(l.wake)
	l.wake = make(chan struct{})
}

func (l *lease) send(m api.Message) {
	l.seq++
	m.Seq = l.seq
	l.queue = append(l.queue, m)
	l.signal()
}

func (r *Registry) view(l *lease) Client {
	now := r.now()
	c := Client{Identity: l.identity, Lease: l.id, Slots: l.slots, Draining: l.draining, ConnectedAt: l.connectedAt, LastSeen: l.lastSeen,
		LeaseEndsAt: l.lastSeen.Add(r.grace), Revision: r.handout.Revision, Health: LeaseHealthy}
	if now.Sub(l.lastSeen) >= lateIntervals*r.heartbeat {
		c.Health = LeaseLate
	}
	if l.caps != nil {
		caps := *l.caps
		caps.Profiles = slices.Clone(l.caps.Profiles)
		c.Capabilities = &caps
	}
	for _, h := range l.held {
		c.Held = append(c.Held, *h)
	}
	sort.Slice(c.Held, func(i, j int) bool { return c.Held[i].Turn < c.Held[j].Turn })
	for turn := range l.offers {
		c.Offered = append(c.Offered, turn)
	}
	sort.Strings(c.Offered)
	return c
}

type endedLease struct {
	client Client
	turns  []string
}

func (r *Registry) endLocked(l *lease) endedLease {
	out := endedLease{client: r.view(l)}
	for turn := range l.held {
		out.turns = append(out.turns, turn)
	}
	sort.Strings(out.turns)
	delete(r.byNode, l.identity.ID)
	delete(r.byLease, l.id)
	close(l.ended)
	return out
}

func (r *Registry) fire(ended []endedLease) {
	hook := r.hooksNow().LeaseEnded
	if hook == nil {
		return
	}
	for _, e := range ended {
		hook(e.client, e.turns)
	}
}

// Expire ends every lease whose client has not been heard from within the
// grace window. Every registry call expires first, so it matters only for a
// lease nobody calls about; Run calls it periodically.
func (r *Registry) Expire() {
	r.mu.Lock()
	var ended []endedLease
	now := r.now()
	for _, l := range r.byLease {
		if now.Sub(l.lastSeen) >= r.grace {
			ended = append(ended, r.endLocked(l))
		}
	}
	r.mu.Unlock()
	sort.Slice(ended, func(i, j int) bool { return ended[i].client.Identity.ID < ended[j].client.Identity.ID })
	r.fire(ended)
}

// Run expires leases every interval until ctx ends.
func (r *Registry) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Expire()
		}
	}
}

// Clients lists the clients that hold a lease, ordered by identity.
func (r *Registry) Clients() []Client {
	r.Expire()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Client, 0, len(r.byLease))
	for _, l := range r.byLease {
		out = append(out, r.view(l))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity.ID < out[j].Identity.ID })
	return out
}

// Client returns the client of a node, if it holds a lease.
func (r *Registry) Client(id string) (Client, bool) {
	r.Expire()
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.byNode[id]
	if !ok {
		return Client{}, false
	}
	return r.view(l), true
}

// Handout returns the profile handout the server holds.
func (r *Registry) Handout() api.ProfileHandout {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handout
}

// SetHandout replaces the profile handout and sends it to every connected
// client. A client's earlier report then describes an older handout until it
// reports again.
func (r *Registry) SetHandout(h api.ProfileHandout) {
	r.Expire()
	r.mu.Lock()
	defer r.mu.Unlock()
	if h.Revision == r.handout.Revision {
		return
	}
	r.handout = h
	for _, l := range r.byLease {
		l.queue = slices.DeleteFunc(l.queue, func(m api.Message) bool { return m.Kind == api.MessageProfiles })
		l.send(api.Message{Kind: api.MessageProfiles, Profiles: &h})
	}
}

func refuse(code api.Code, format string, args ...any) *api.APIError {
	return &api.APIError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Connect grants a lease to the client of a node. It refuses a client of
// another protocol version, naming both, and a node whose earlier connection
// still holds its lease, unless the request names that lease: the same client
// reconnecting, which takes the connection over and reattaches its running
// turns by ID.
func (r *Registry) Connect(ctx context.Context, ident Identity, req api.ClientConnectRequest) (api.ClientConnectResponse, error) {
	if req.Version != api.ClientProtocolVersion {
		return api.ClientConnectResponse{}, refuse(api.VersionMismatch, "the server speaks client protocol version %d and this client speaks version %d; run the same osmia release on both", api.ClientProtocolVersion, req.Version)
	}
	if req.Slots < 0 {
		return api.ClientConnectResponse{}, refuse(api.Validation, "slots cannot be negative")
	}
	if ident.ID == "" {
		return api.ClientConnectResponse{}, refuse(api.Unavailable, "the caller's tailnet node cannot be identified")
	}
	r.Expire()
	r.mu.Lock()
	now := r.now()
	l, held := r.byNode[ident.ID]
	resumed := false
	switch {
	case held && req.Lease == l.id:
		resumed = true
		l.generation++
		l.identity = ident
		l.slots = req.Slots
		l.lastSeen = now
		l.signal()
	case held:
		remaining := l.lastSeen.Add(r.grace).Sub(now).Round(time.Second)
		r.mu.Unlock()
		return api.ClientConnectResponse{}, refuse(api.ClientConnected, "node %s already has a connection that holds a lease; it ends in %s unless that connection reconnects with its lease", ident.Name, remaining)
	default:
		l = newLease(ident, req.Slots, now)
		r.byNode[ident.ID] = l
		r.byLease[l.id] = l
	}
	if req.Draining {
		l.draining = true
	}
	unknown := []string{}
	for _, turn := range req.Running {
		if _, ok := l.held[turn]; !ok {
			unknown = append(unknown, turn)
		}
	}
	reattach := r.hooks.Reattach
	r.mu.Unlock()

	adopted := map[string]string{}
	if reattach != nil {
		for _, turn := range unknown {
			if token, held := reattach(ctx, ident, turn); held {
				adopted[turn] = token
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byLease[l.id] != l {
		return api.ClientConnectResponse{}, refuse(api.LeaseEnded, "the lease ended while it was granted; connect again")
	}
	resp := api.ClientConnectResponse{Version: api.ClientProtocolVersion, Lease: l.id, Client: ident, Resumed: resumed,
		HeartbeatSeconds: int(r.heartbeat / time.Second), GraceSeconds: int(r.grace / time.Second), PollSeconds: int(r.poll / time.Second), Profiles: r.handout}
	for _, turn := range req.Running {
		answer, token := api.TurnLost, ""
		_, held := l.held[turn]
		adoptedToken, adoptedTurn := adopted[turn]
		if held || adoptedTurn {
			answer, token = api.TurnReattached, adoptedToken
			if !held {
				l.held[turn] = &Held{Turn: turn, Since: now, Progress: api.TurnProgress{Turn: turn}}
			}
		}
		resp.Turns = append(resp.Turns, api.TurnReattach{Turn: turn, Answer: answer, Token: token})
	}
	return resp, nil
}

func (r *Registry) leaseFor(id string, caller Identity) (*lease, error) {
	l, ok := r.byLease[id]
	if !ok {
		return nil, refuse(api.LeaseEnded, "the lease ended or was never granted; connect again")
	}
	if l.identity.ID != caller.ID {
		return nil, refuse(api.Forbidden, "the lease belongs to another node")
	}
	return l, nil
}

// touch looks up the caller's lease and counts the request as contact.
func (r *Registry) touch(id string, caller Identity) (*lease, error) {
	r.Expire()
	r.mu.Lock()
	defer r.mu.Unlock()
	l, err := r.leaseFor(id, caller)
	if err != nil {
		return nil, err
	}
	l.lastSeen = r.now()
	return l, nil
}

// Heartbeat keeps a lease alive and takes each running turn's progress. Lost
// lists reported turns the server does not hold for the client.
func (r *Registry) Heartbeat(id string, caller Identity, hb api.HeartbeatRequest) (api.HeartbeatResponse, error) {
	l, err := r.touch(id, caller)
	if err != nil {
		return api.HeartbeatResponse{}, err
	}
	r.mu.Lock()
	resp := api.HeartbeatResponse{Draining: l.draining}
	var progressed []api.TurnProgress
	for _, p := range hb.Turns {
		h, ok := l.held[p.Turn]
		if !ok {
			resp.Lost = append(resp.Lost, p.Turn)
			continue
		}
		h.Progress = p
		progressed = append(progressed, p)
	}
	client := r.view(l)
	progress := r.hooks.Progress
	r.mu.Unlock()
	if progress != nil {
		for _, p := range progressed {
			progress(client, p)
		}
	}
	return resp, nil
}

// Drain marks the client draining: it takes no new turns and keeps its
// lease while its running turns finish.
func (r *Registry) Drain(id string, caller Identity) error {
	l, err := r.touch(id, caller)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	l.draining = true
	return nil
}

// Leave ends the client's lease now, as a client does when it exits.
func (r *Registry) Leave(id string, caller Identity) error {
	r.Expire()
	r.mu.Lock()
	l, err := r.leaseFor(id, caller)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	ended := r.endLocked(l)
	r.mu.Unlock()
	r.fire([]endedLease{ended})
	return nil
}

// Report stores a client's capability report and tells whether it checked
// the handout the server holds now.
func (r *Registry) Report(id string, caller Identity, report api.CapabilityReport) (bool, error) {
	l, err := r.touch(id, caller)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	report.Profiles = slices.Clone(report.Profiles)
	l.caps = &report
	return report.Revision == r.handout.Revision, nil
}

// Poll waits up to the requested time for messages the client has not
// acknowledged. It returns none when the wait ends first, when the client
// connected again on a newer connection, or when ctx ends. It returns an error
// when the lease ends.
func (r *Registry) Poll(ctx context.Context, id string, caller Identity, req api.PollRequest) ([]api.Message, error) {
	l, err := r.touch(id, caller)
	if err != nil {
		return nil, err
	}
	wait := time.Duration(req.WaitSeconds) * time.Second
	if wait <= 0 || wait > r.poll {
		wait = r.poll
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	r.mu.Lock()
	generation := l.generation
	for {
		l.queue = slices.DeleteFunc(l.queue, func(m api.Message) bool { return m.Seq <= req.Ack })
		if len(l.queue) > 0 {
			out := slices.Clone(l.queue)
			l.lastSeen = r.now()
			r.mu.Unlock()
			return out, nil
		}
		wake := l.wake
		r.mu.Unlock()
		select {
		case <-wake:
		case <-l.ended:
			return nil, refuse(api.LeaseEnded, "the lease ended; connect again")
		case <-timer.C:
			return nil, r.contact(l)
		case <-ctx.Done():
			return nil, nil
		}
		r.mu.Lock()
		if l.generation != generation {
			r.mu.Unlock()
			return nil, nil
		}
	}
}

func (r *Registry) contact(l *lease) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byLease[l.id] != l {
		return refuse(api.LeaseEnded, "the lease ended; connect again")
	}
	l.lastSeen = r.now()
	return nil
}

// Offer sends a turn to the client of a node and waits for its claim. It
// returns when the client claims or declines, when the lease ends, or when ctx
// ends, in which case the offer is withdrawn and a later claim is answered gone.
func (r *Registry) Offer(ctx context.Context, node string, o api.TurnOffer) (Claim, error) {
	r.Expire()
	r.mu.Lock()
	l, ok := r.byNode[node]
	if !ok {
		r.mu.Unlock()
		return Claim{}, ErrNoClient
	}
	if l.draining {
		r.mu.Unlock()
		return Claim{}, ErrDraining
	}
	pending := &offer{done: make(chan Claim, 1)}
	l.offers[o.Turn] = pending
	l.send(api.Message{Kind: api.MessageOffer, Offer: &o})
	r.mu.Unlock()
	select {
	case c := <-pending.done:
		return c, nil
	case <-l.ended:
		return Claim{}, ErrLeaseEnded
	case <-ctx.Done():
		r.mu.Lock()
		if l.offers[o.Turn] == pending {
			delete(l.offers, o.Turn)
		}
		r.mu.Unlock()
		select {
		case c := <-pending.done:
			if c.Accepted {
				r.Release(node, o.Turn)
			}
		default:
		}
		return Claim{}, ctx.Err()
	}
}

// Claim takes a client's answer to an offer.
func (r *Registry) Claim(id string, caller Identity, turn string, c api.ClaimRequest) (string, error) {
	if _, err := r.touch(id, caller); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	l, err := r.leaseFor(id, caller)
	if err != nil {
		return "", err
	}
	pending, ok := l.offers[turn]
	if !ok {
		return api.ClaimGone, nil
	}
	delete(l.offers, turn)
	if !c.Accept {
		pending.done <- Claim{Reason: c.Reason}
		return api.ClaimReleased, nil
	}
	l.held[turn] = &Held{Turn: turn, Since: r.now(), Progress: api.TurnProgress{Turn: turn}}
	pending.done <- Claim{Accepted: true}
	return api.ClaimTaken, nil
}

// Cancel asks the client of a node to cancel a turn it holds or was offered.
func (r *Registry) Cancel(node, turn, reason string) error {
	r.Expire()
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.byNode[node]
	if !ok {
		return ErrNoClient
	}
	delete(l.offers, turn)
	l.send(api.Message{Kind: api.MessageCancel, Cancel: &api.TurnCancel{Turn: turn, Reason: reason}})
	return nil
}

// Release forgets that the client of a node holds turn, which frees its slot.
func (r *Registry) Release(node, turn string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.byNode[node]; ok {
		delete(l.held, turn)
		delete(l.offers, turn)
	}
}

// Call asks the client of a node to carry out an operation on a turn's view or
// skills and returns the client's result. The request travels as a message the
// client collects on its own connection and the client replies on a request of
// its own. A failure the client reports is a *RemoteError.
func (r *Registry) Call(ctx context.Context, node, turn, op string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	r.Expire()
	r.mu.Lock()
	l, ok := r.byNode[node]
	if !ok {
		r.mu.Unlock()
		return nil, ErrNoClient
	}
	id := randomID()
	pending := &call{done: make(chan api.ServerReply, 1)}
	l.calls[id] = pending
	l.send(api.Message{Kind: api.MessageRequest, Request: &api.ServerRequest{ID: id, Turn: turn, Op: op, Params: raw}})
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(l.calls, id)
		r.mu.Unlock()
	}()
	select {
	case reply := <-pending.done:
		if reply.Error != "" {
			return nil, &RemoteError{Message: reply.Error}
		}
		return reply.Result, nil
	case <-l.ended:
		return nil, ErrLeaseEnded
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Reply takes a client's reply to a request. It tells whether the server still
// waited for it.
func (r *Registry) Reply(id string, caller Identity, request string, reply api.ServerReply) (bool, error) {
	if _, err := r.touch(id, caller); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	l, err := r.leaseFor(id, caller)
	if err != nil {
		return false, err
	}
	pending, ok := l.calls[request]
	if !ok {
		return false, nil
	}
	delete(l.calls, request)
	pending.done <- reply
	return true, nil
}

// Result hands a turn's returned result to the turn execution and returns its
// definitive answer. A turn the client holds is released once it is answered.
func (r *Registry) Result(ctx context.Context, id string, caller Identity, turn string, res api.TurnResult) (api.ViewAnswer, error) {
	l, err := r.touch(id, caller)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	client := r.view(l)
	result := r.hooks.Result
	r.mu.Unlock()
	if result == nil {
		return "", refuse(api.Unavailable, "the server takes no turn results yet")
	}
	answer, err := result(ctx, client, turn, res)
	if err != nil {
		return "", err
	}
	r.Release(caller.ID, turn)
	return answer, nil
}
