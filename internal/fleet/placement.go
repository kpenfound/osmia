package fleet

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
)

// Local is the ID of the built-in local client, the server's own machine. It
// is the ID the trace records for turns that ran there.
const Local = coreadapter.LocalClient

// The reasons a turn finds no client to start on.
const (
	// WaitClient: a client the turn can start on has no free slot. For a
	// resumed turn it is the client that holds its session.
	WaitClient = "client"
	// WaitNoClient: no connected, non-draining client can run the turn's main
	// profile.
	WaitNoClient = "no-client"
)

// The reasons a client takes no turn now.
const (
	IdleDraining  = "draining"
	IdleNoSlots   = "no-slots"
	IdleFull      = "full"
	IdleNoReport  = "no-report"
	IdleNoProfile = "no-profile"
)

const reservationTTL = 10 * time.Minute

const waitPoll = 250 * time.Millisecond

// Turn is what placement needs to know of a turn.
type Turn struct {
	Project    string
	Workstream string
	// Agent is the ID of the turn's thread in the trace.
	Agent string
	// ID is the turn's ID.
	ID   string
	Role string
	// Profile is the turn's main profile.
	Profile string
	// SessionClient is the client that holds the saved session the turn
	// resumes. It is empty for a turn that starts a new session or replays its
	// log.
	SessionClient string
}

// Choice is where a turn can start now.
type Choice struct {
	// Client is the client to start the turn on; empty when none can take it.
	Client string
	// Pinned is the client the turn is pinned to because it resumes that
	// client's session, or empty for a turn placed round robin.
	Pinned string
	// Reason is why Client is empty: WaitClient or WaitNoClient.
	Reason string
}

// Placed is where a started turn runs.
type Placed struct {
	// Client is the ID of the client running the turn: Local, or a tailnet
	// node's stable ID.
	Client string
	// Runs reports whether the client can run profile. The local client runs
	// every profile.
	Runs func(profile string) bool
	// Release gives the turn's client slot back. It is safe to call twice.
	Release func()
}

// Place is one client as placement sees it: the local client or a connected
// client.
type Place struct {
	ID   string
	Name string
	// Local is true for the built-in local client.
	Local bool
	// Slots is the declared slot count; meaningless when Unbounded.
	Slots int
	// Unbounded is true for a local client that has no slot setting, which adds
	// no cap beyond the role limits.
	Unbounded bool
	// Used is how many slots the turns placed on it take.
	Used int
	// Turns are the IDs of those turns.
	Turns    []string
	Draining bool
	// Idle is why the client takes no new turn now, or empty when it does.
	Idle string
}

// Placement chooses the client each turn runs on and counts the slots the
// turns take. New turns go round robin over the local client and the
// connected clients, one place each; a turn that resumes a client's session
// is pinned to that client. The role limits that cap every client together
// stay with the scheduler, which checks them first.
type Placement struct {
	// Registry holds the connected clients.
	Registry *Registry
	// LocalSlots reports the local client's slot count. It reports false when
	// no count is set, which adds no cap beyond the role limits. Nil is unset.
	LocalSlots func() (int, bool)

	mu      sync.Mutex
	started time.Time
	last    string
	picked  bool
	turns   map[string]*placedTurn
	owed    map[string]map[string]owedTurn
	changed chan struct{}
}

type owedTurn struct {
	client string
	at     time.Time
}

type placedTurn struct {
	turn   Turn
	client string
	begun  bool
	at     time.Time
}

func (p *Placement) now() time.Time { return p.Registry.now() }

func (p *Placement) localLimit() (int, bool) {
	if p.LocalSlots == nil {
		return 0, false
	}
	return p.LocalSlots()
}

func (p *Placement) settling() bool {
	return p.now().Before(p.started.Add(p.Registry.Grace()))
}

func (p *Placement) wake() {
	if p.changed != nil {
		close(p.changed)
		p.changed = nil
	}
}

func (p *Placement) prune() {
	now := p.now()
	if p.started.IsZero() {
		p.started = now
	}
	for id, t := range p.turns {
		if !t.begun && now.Sub(t.at) > reservationTTL {
			delete(p.turns, id)
		}
	}
	for project, turns := range p.owed {
		for id, o := range turns {
			if now.Sub(o.at) > reservationTTL {
				delete(turns, id)
			}
		}
		if len(turns) == 0 {
			delete(p.owed, project)
		}
	}
}

// Owe tells placement which resumed turns of project are owed the next free
// slot of the client they are pinned to, by turn. A new turn leaves each such
// slot to its turn: with a free slot per owed turn it takes none. Each call
// replaces what the project owed before, and owed turns the project stops
// naming, such as ones that started, are forgotten after a while.
func (p *Placement) Owe(project string, turns map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(turns) == 0 {
		delete(p.owed, project)
		return
	}
	if p.owed == nil {
		p.owed = map[string]map[string]owedTurn{}
	}
	at := p.now()
	next := make(map[string]owedTurn, len(turns))
	for turn, client := range turns {
		next[turn] = owedTurn{client, at}
	}
	p.owed[project] = next
}

func (p *Placement) owedOn(client, except string) int {
	n := 0
	for _, turns := range p.owed {
		for id, o := range turns {
			if o.client == client && id != except {
				n++
			}
		}
	}
	return n
}

func rank(id string) string {
	if id == Local {
		return ""
	}
	return "n" + id
}

func (p *Placement) used(client string, held *Client) []string {
	var out []string
	for id, t := range p.turns {
		if t.client == client {
			out = append(out, id)
		}
	}
	if held != nil {
		for _, h := range held.Held {
			if !slices.Contains(out, h.Turn) {
				out = append(out, h.Turn)
			}
		}
		for _, o := range held.Offered {
			if !slices.Contains(out, o) {
				out = append(out, o)
			}
		}
	}
	sort.Strings(out)
	return out
}

func connected(clients []Client, id string) (Client, bool) {
	for _, c := range clients {
		if c.Identity.ID == id {
			return c, true
		}
	}
	return Client{}, false
}

func runnable(c Client, profile string) bool {
	return profile == "" || c.CanRun(profile)
}

func (p *Placement) pinned(t Turn, clients []Client) (string, bool) {
	if t.SessionClient == "" {
		return "", false
	}
	if t.SessionClient == Local {
		if n, set := p.localLimit(); set && n <= 0 {
			return "", false
		}
		return Local, true
	}
	c, ok := connected(clients, t.SessionClient)
	if !ok {
		return t.SessionClient, p.settling()
	}
	if c.Draining {
		return "", false
	}
	return t.SessionClient, true
}

// Pinned returns the client a turn is pinned to: the one that holds the
// session it resumes, while that client is connected and not draining, the
// lease's grace window included, whatever its capability report says; the turn
// waits there for a runnable profile and a free slot. A resumed turn whose session client is gone,
// draining, or the local client set to no slots is not pinned and is placed
// round robin.
func (p *Placement) Pinned(t Turn) (string, bool) {
	clients := p.Registry.Clients()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune()
	return p.pinned(t, clients)
}

func (p *Placement) room(client string, clients []Client, hold int) bool {
	if client == Local {
		n, set := p.localLimit()
		if !set {
			return true
		}
		return len(p.used(Local, nil))+hold < n
	}
	c, ok := connected(clients, client)
	if !ok || c.Draining || c.Capabilities == nil {
		return false
	}
	return len(p.used(client, &c))+hold < c.Slots
}

func (p *Placement) eligible(id string, t Turn, clients []Client, hold map[string]int, needRoom bool) bool {
	if id == Local {
		if n, set := p.localLimit(); set && n <= 0 {
			return false
		}
		return !needRoom || p.room(Local, clients, hold[Local])
	}
	c, ok := connected(clients, id)
	if !ok || c.Draining || !runnable(c, t.Profile) {
		return false
	}
	return !needRoom || p.room(id, clients, hold[id])
}

func (p *Placement) ring(clients []Client) []string {
	ring := []string{Local}
	for _, c := range clients {
		ring = append(ring, c.Identity.ID)
	}
	sort.Slice(ring, func(i, j int) bool { return rank(ring[i]) < rank(ring[j]) })
	return ring
}

func (p *Placement) choose(t Turn) Choice {
	clients := p.Registry.Clients()
	if client, ok := p.pinned(t, clients); ok {
		if !p.startable(client, t, clients, 0) {
			return Choice{Pinned: client, Reason: WaitClient}
		}
		return Choice{Client: client, Pinned: client}
	}
	hold := map[string]int{}
	for _, id := range p.ring(clients) {
		hold[id] = p.owedOn(id, t.ID)
	}
	ring := p.ring(clients)
	start := 0
	if p.picked {
		start = len(ring)
		for i, id := range ring {
			if rank(p.last) < rank(id) {
				start = i
				break
			}
		}
	}
	for i := range ring {
		id := ring[(start+i)%len(ring)]
		if p.eligible(id, t, clients, hold, true) {
			return Choice{Client: id}
		}
	}
	for _, id := range ring {
		if p.eligible(id, t, clients, nil, false) {
			return Choice{Reason: WaitClient}
		}
	}
	return Choice{Reason: WaitNoClient}
}

func (p *Placement) startable(client string, t Turn, clients []Client, hold int) bool {
	if client == Local {
		return p.room(Local, clients, hold)
	}
	c, ok := connected(clients, client)
	if !ok || c.Draining || c.Capabilities == nil || !runnable(c, t.Profile) {
		return false
	}
	return p.room(client, clients, hold)
}

// Choose says where a turn can start now, and changes nothing. A turn pinned to
// a client waits for it; any other waits for a client that can run its main
// profile, has a free slot beyond those owed to resumed turns (see Owe) and is
// not draining.
func (p *Placement) Choose(t Turn) Choice {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune()
	return p.choose(t)
}

// Take chooses as Choose does and, when a client can take the turn, takes one
// of its slots for the turn until Release. A turn placed round robin moves the
// rotation past the client; a turn that no client can take changes nothing, so
// it leaves the rotation where it was.
func (p *Placement) Take(t Turn) Choice {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune()
	c := p.choose(t)
	if c.Client != "" {
		p.reserve(t, c.Client, c.Pinned == "", false)
	}
	return c
}

func (p *Placement) reserve(t Turn, client string, rotate, begun bool) {
	if p.turns == nil {
		p.turns = map[string]*placedTurn{}
	}
	p.turns[t.ID] = &placedTurn{turn: t, client: client, begun: begun, at: p.now()}
	if rotate {
		p.last, p.picked = client, true
	}
}

func (p *Placement) usable(client string, t Turn, clients []Client) bool {
	if client == Local {
		n, set := p.localLimit()
		return !set || n > 0
	}
	c, ok := connected(clients, client)
	return ok && !c.Draining && (c.Capabilities == nil || runnable(c, t.Profile))
}

// Room reports whether some client could take a new turn of profile now: the
// local client, or a connected client that can run it, has a free slot.
func (p *Placement) Room(profile string) bool {
	clients := p.Registry.Clients()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune()
	t := Turn{Profile: profile}
	for _, id := range p.ring(clients) {
		if p.eligible(id, t, clients, nil, true) {
			return true
		}
	}
	return false
}

// Begin places a turn that is about to run and returns where. A turn the
// scheduler reserved a slot for runs on that client; one that has none, such as
// a turn a restart interrupted, is placed as the scheduler would place it, and
// waits for a slot. A turn whose client went away or started draining since is
// placed again. It returns when ctx ends.
func (p *Placement) Begin(ctx context.Context, t Turn) (Placed, error) {
	for {
		clients := p.Registry.Clients()
		p.mu.Lock()
		p.prune()
		if r, ok := p.turns[t.ID]; ok {
			if p.usable(r.client, t, clients) {
				r.begun = true
				client := r.client
				p.mu.Unlock()
				return p.placed(t.ID, client), nil
			}
			delete(p.turns, t.ID)
		}
		if c := p.choose(t); c.Client != "" {
			p.reserve(t, c.Client, c.Pinned == "", true)
			p.mu.Unlock()
			return p.placed(t.ID, c.Client), nil
		}
		if p.changed == nil {
			p.changed = make(chan struct{})
		}
		changed := p.changed
		p.mu.Unlock()
		timer := time.NewTimer(waitPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Placed{}, ctx.Err()
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (p *Placement) placed(turn, client string) Placed {
	var once sync.Once
	out := Placed{Client: client, Release: func() { once.Do(func() { p.Release(turn) }) }}
	out.Runs = func(profile string) bool { return p.Runs(client, profile) }
	return out
}

// Release gives back the slot a turn took.
func (p *Placement) Release(turn string) {
	p.mu.Lock()
	delete(p.turns, turn)
	p.wake()
	p.mu.Unlock()
}

// Runs reports whether client can run profile. The local client runs every
// profile; another client runs those its report says it can, and none before
// it has reported.
func (p *Placement) Runs(client, profile string) bool {
	if client == Local {
		return true
	}
	c, ok := p.Registry.Client(client)
	return ok && c.CanRun(profile)
}

// Locate returns the client a turn was placed on. It reports false for a turn
// placed on the local client and for a turn that was never placed, which the
// service runs on its own machine.
func (p *Placement) Locate(turn string) (Identity, bool) {
	p.mu.Lock()
	t, ok := p.turns[turn]
	p.mu.Unlock()
	if !ok || t.client == Local {
		return Identity{}, false
	}
	if c, ok := p.Registry.Client(t.client); ok {
		return c.Identity, true
	}
	return Identity{ID: t.client}, true
}

// PlacedOn returns the ID of the client a turn was placed on, Local for the
// server's own machine. It reports false for a turn that holds no placement.
func (p *Placement) PlacedOn(turn string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.turns[turn]
	if !ok {
		return "", false
	}
	return t.client, true
}

// Places lists the local client and every connected client with the slots the
// turns placed on them take, and why one takes no new turn, ordered the way the
// rotation visits them.
func (p *Placement) Places() []Place {
	clients := p.Registry.Clients()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune()
	var out []Place
	for _, id := range p.ring(clients) {
		if id == Local {
			n, set := p.localLimit()
			turns := p.used(Local, nil)
			place := Place{ID: Local, Name: Local, Local: true, Slots: n, Unbounded: !set, Used: len(turns), Turns: turns}
			switch {
			case set && n <= 0:
				place.Idle = IdleNoSlots
			case set && len(turns) >= n:
				place.Idle = IdleFull
			}
			out = append(out, place)
			continue
		}
		c, _ := connected(clients, id)
		turns := p.used(id, &c)
		place := Place{ID: id, Name: c.Identity.Name, Slots: c.Slots, Used: len(turns), Turns: turns, Draining: c.Draining}
		switch {
		case c.Draining:
			place.Idle = IdleDraining
		case c.Capabilities == nil:
			place.Idle = IdleNoReport
		case !slices.ContainsFunc(c.Capabilities.Profiles, func(pc api.ProfileCapability) bool { return pc.Runnable }):
			place.Idle = IdleNoProfile
		case c.Slots <= 0:
			place.Idle = IdleNoSlots
		case len(turns) >= c.Slots:
			place.Idle = IdleFull
		}
		out = append(out, place)
	}
	return out
}
