// Package hubnode is the hub's control loop for one agent, shared by the Linux hub and
// the MikroTik plugin: it keeps the agent on the desired state and turns usage into deltas.
package hubnode

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/link"
)

// Usage is traffic in bytes.
type Usage struct{ Up, Down uint64 }

// UsageReport is what OnUsage receives after each stats poll.
type UsageReport struct {
	Deltas     map[string]Usage // traffic since the previous report, per user
	Cumulative map[string]Usage // the agent's counters now: persist these as the next Baseline
}

type Config struct {
	Link link.HubConfig

	Tick        time.Duration // how often the loop looks (default 500 ms)
	StatusEvery time.Duration // default 10 s
	StatsEvery  time.Duration // default 30 s
	PushTimeout time.Duration // default 30 s
	CallTimeout time.Duration // default 15 s
	// DisguiseEvery is how often the cover site is re-checked from the VPS (default 6 h).
	DisguiseEvery time.Duration

	// Baseline is the cumulative counters stored from the last run. A user absent
	// from it is first seen at its current value (delta 0): the alternative is to
	// count traffic that was already counted before the hub restarted.
	Baseline map[string]Usage

	// OnUsage receives each report. If it returns an error the counters are not
	// advanced, so the same traffic is reported again on the next poll.
	OnUsage func(UsageReport) error
	// OnApplied is called after every successful state push, whether the
	// background loop or PushNow made it.
	OnApplied func(ctrl.State)
	OnEvent   func(Event)
	OnStatus  func(Snapshot)
}

type EventKind string

const (
	EventPushed       EventKind = "state_pushed"
	EventPushFailed   EventKind = "push_failed"
	EventDrift        EventKind = "drift"         // the agent reported a different revision
	EventAgentGone    EventKind = "status_failed" // the link is up but status calls fail
	EventTelemtDown   EventKind = "telemt_down"   // the agent says telemt is not running
	EventTelemtUp     EventKind = "telemt_up"
	EventCrashLoop    EventKind = "telemt_crash_loop"
	EventLinkUp       EventKind = "link_up"
	EventLinkDown     EventKind = "link_down"
	EventViolation    EventKind = "allowlist_violation"
	EventDialFailure  EventKind = "dc_dial_failed"
	EventDisguiseDown EventKind = "disguise_down" // the periodic check says the cover site stopped working
	EventDisguiseUp   EventKind = "disguise_up"
)

type Event struct {
	Kind   EventKind
	Detail string
	Err    error
	At     time.Time
}

// Snapshot is the node's state at a moment, for the UI and for alerts.
type Snapshot struct {
	Link        link.Status
	Agent       *ctrl.Status
	AgentAt     time.Time
	DesiredRev  string
	AppliedRev  string
	LastPush    time.Time
	LastPushErr string
	Disguise    *ctrl.DisguiseResult // the last periodic cover-site check
	DisguiseAt  time.Time
	// DisguiseDomain is the site Disguise is for: right after a switch it is still the old one.
	DisguiseDomain string
	// Healthy: the link is up, the agent answered recently, and it runs the state we want.
	Healthy bool
}

type Node struct {
	cfg Config
	hub *link.Hub

	mu        sync.Mutex
	desired   *ctrl.State
	applied   string
	needPush  bool
	lastPush  time.Time
	pushErr   string
	pushFails int
	nextPush  time.Time
	agent     *ctrl.Status
	agentAt   time.Time
	lastTelem *bool
	last      map[string]Usage
	statusAt  time.Time
	statsAt   time.Time
	cover     *ctrl.DisguiseResult
	coverAt   time.Time
	coverFor  string // the domain cover was checked for
	coverNext time.Time
	coverName string
	covering  bool

	kick chan struct{}
	now  func() time.Time
}

func New(cfg Config) *Node {
	if cfg.Tick == 0 {
		cfg.Tick = 500 * time.Millisecond
	}
	if cfg.StatusEvery == 0 {
		cfg.StatusEvery = 10 * time.Second
	}
	if cfg.StatsEvery == 0 {
		cfg.StatsEvery = 30 * time.Second
	}
	if cfg.PushTimeout == 0 {
		cfg.PushTimeout = 30 * time.Second
	}
	if cfg.CallTimeout == 0 {
		cfg.CallTimeout = 15 * time.Second
	}
	if cfg.DisguiseEvery == 0 {
		cfg.DisguiseEvery = 6 * time.Hour
	}
	n := &Node{cfg: cfg, kick: make(chan struct{}, 1), now: time.Now, last: map[string]Usage{}}
	for k, v := range cfg.Baseline {
		n.last[k] = v
	}
	user := cfg.Link.OnEvent
	cfg.Link.OnEvent = func(e link.Event) {
		n.onLinkEvent(e)
		if user != nil {
			user(e)
		}
	}
	n.hub = link.NewHub(cfg.Link)
	return n
}

// SetCreds swaps the link credentials for connections made from now on.
func (n *Node) SetCreds(c link.Creds) { n.hub.SetCreds(c) }

// Hub exposes the link for control calls the node does not wrap.
func (n *Node) Hub() *link.Hub { return n.hub }

func (n *Node) emit(k EventKind, detail string, err error) {
	if n.cfg.OnEvent != nil {
		n.cfg.OnEvent(Event{Kind: k, Detail: detail, Err: err, At: n.now()})
	}
}

func (n *Node) onLinkEvent(e link.Event) {
	switch e.Kind {
	case link.EventConnUp:
		// a connection came up: the agent may be a fresh process
		n.mu.Lock()
		n.needPush = true
		n.nextPush = time.Time{}
		n.mu.Unlock()
		n.poke()
		n.emit(EventLinkUp, e.Conn, nil)
	case link.EventConnDown:
		n.emit(EventLinkDown, e.Conn, e.Err)
	case link.EventViolation:
		n.emit(EventViolation, e.Detail, nil)
	case link.EventDialFail:
		n.emit(EventDialFailure, e.Detail, e.Err)
	}
}

func (n *Node) poke() {
	select {
	case n.kick <- struct{}{}:
	default:
	}
}

// SetDesired replaces the desired state; it is pushed as soon as the link allows.
func (n *Node) SetDesired(s ctrl.State) {
	n.mu.Lock()
	cp := s
	n.desired = &cp
	n.needPush = true
	n.nextPush = time.Time{}
	n.mu.Unlock()
	// the agent may switch cover site before its answer gets back: dial that name too
	n.hub.ExpectServerName(s.Disguise)
	n.poke()
}

// Run drives the link and the control loop until ctx ends.
func (n *Node) Run(ctx context.Context) error {
	go n.hub.Run(ctx)
	t := time.NewTicker(n.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-n.kick:
		}
		n.step(ctx)
	}
}

func (n *Node) step(ctx context.Context) {
	if n.hub.Status().Up == 0 {
		return
	}
	n.maybePush(ctx)
	now := n.now()
	n.mu.Lock()
	doStatus := now.Sub(n.statusAt) >= n.cfg.StatusEvery
	doStats := now.Sub(n.statsAt) >= n.cfg.StatsEvery
	if doStatus {
		n.statusAt = now
	}
	if doStats {
		n.statsAt = now
	}
	n.mu.Unlock()
	if doStatus {
		n.pollStatus(ctx)
	}
	if doStats {
		n.pollStats(ctx)
	}
	n.maybeCheckCover(ctx)
}

// maybeCheckCover re-checks the cover site the agent runs, after the status
// poll so the agent's own idea of its domain is known. The agent may take
// seconds to reach the site, so the loop does not wait for it.
func (n *Node) maybeCheckCover(ctx context.Context) {
	n.mu.Lock()
	domain := n.coverDomainLocked()
	// a new domain is checked at once rather than at the next interval
	if domain == "" || n.covering || (domain == n.coverName && n.now().Before(n.coverNext)) {
		n.mu.Unlock()
		return
	}
	n.coverNext, n.coverName, n.covering = n.now().Add(n.cfg.DisguiseEvery), domain, true
	n.mu.Unlock()
	go n.checkCover(ctx, domain)
}

// coverDomainLocked is the domain the agent reports, else the one it is being given.
func (n *Node) coverDomainLocked() string {
	if n.agent != nil && n.agent.Disguise.Domain != "" {
		return n.agent.Disguise.Domain
	}
	if n.desired != nil {
		return n.desired.Disguise
	}
	return ""
}

func (n *Node) checkCover(ctx context.Context, domain string) {
	defer func() {
		n.mu.Lock()
		n.covering = false
		n.mu.Unlock()
	}()
	cctx, cancel := context.WithTimeout(ctx, n.cfg.CallTimeout)
	defer cancel()
	var res ctrl.DisguiseResult
	if err := n.hub.Call(cctx, ctrl.MDisguiseCheck, ctrl.DisguiseCheck{Domain: domain}, &res); err != nil {
		// no answer says nothing about the site: ask again soon
		n.mu.Lock()
		n.coverNext = n.now().Add(min(time.Minute, n.cfg.DisguiseEvery))
		n.mu.Unlock()
		return
	}
	n.mu.Lock()
	if n.coverDomainLocked() != domain {
		n.mu.Unlock()
		return // the domain changed while we asked: nobody uses this site any more
	}
	prev := n.cover
	n.cover, n.coverAt, n.coverFor = &res, n.now(), domain
	if !res.OK {
		// a dead site is watched closely so its recovery shows quickly
		n.coverNext = n.now().Add(min(10*time.Minute, n.cfg.DisguiseEvery))
	}
	n.mu.Unlock()
	// the first answer counts as a transition, so an alert left firing across a restart clears
	switch {
	case !res.OK && (prev == nil || prev.OK):
		why := res.Err
		if why == "" {
			why = "the cover site check failed"
		}
		n.emit(EventDisguiseDown, domain, errors.New(why))
	case res.OK && (prev == nil || !prev.OK):
		n.emit(EventDisguiseUp, domain, nil)
	}
}

func (n *Node) maybePush(ctx context.Context) {
	n.mu.Lock()
	if n.desired == nil || !n.needPush || n.now().Before(n.nextPush) {
		n.mu.Unlock()
		return
	}
	s := *n.desired
	n.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, n.cfg.PushTimeout)
	defer cancel()
	var ack ctrl.StateAck
	err := n.hub.Call(cctx, ctrl.MStateApply, s, &ack)

	n.mu.Lock()
	if err != nil {
		n.pushFails++
		n.pushErr = err.Error()
		// 2 s, 4 s, ... capped at 32 s: a bad state must not hammer the agent
		d := time.Duration(1<<min(n.pushFails, 5)) * time.Second
		n.nextPush = n.now().Add(d)
		n.mu.Unlock()
		n.emit(EventPushFailed, s.Rev, err)
		return
	}
	// a state set while this one was in flight still has to go out
	n.needPush, n.pushFails, n.pushErr = n.desired.Rev != s.Rev, 0, ""
	n.applied, n.lastPush = s.Rev, n.now()
	n.mu.Unlock()
	n.emit(EventPushed, s.Rev, nil)
	if n.cfg.OnApplied != nil {
		n.cfg.OnApplied(s)
	}
}

func (n *Node) pollStatus(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, n.cfg.CallTimeout)
	defer cancel()
	var st ctrl.Status
	err := n.hub.Call(cctx, ctrl.MStatus, nil, &st)
	n.mu.Lock()
	if err != nil {
		n.mu.Unlock()
		n.emit(EventAgentGone, "", err)
		return
	}
	n.agent, n.agentAt = &st, n.now()
	drift := n.desired != nil && !n.needPush && st.StateRev != n.desired.Rev
	if drift {
		n.needPush = true
		n.nextPush = time.Time{}
	}
	prev := n.lastTelem
	running := st.Telemt.Running
	n.lastTelem = &running
	snap := n.snapshotLocked()
	n.mu.Unlock()

	if drift {
		n.emit(EventDrift, st.StateRev, nil)
		n.poke()
	}
	// telemt being stopped is expected with no users, or when the panel stopped it
	switch {
	case n.wantsTelemt() && !running && (prev == nil || *prev):
		n.emit(EventTelemtDown, st.Telemt.LastExit, nil)
	case running && prev != nil && !*prev:
		n.emit(EventTelemtUp, "", nil)
	}
	if st.Telemt.CrashLoop {
		n.emit(EventCrashLoop, st.Telemt.LastExit, nil)
	}
	if n.cfg.OnStatus != nil {
		n.cfg.OnStatus(snap)
	}
}

func (n *Node) wantsTelemt() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.desired != nil && len(n.desired.Users) > 0 && !n.desired.Stopped
}

func (n *Node) pollStats(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, n.cfg.CallTimeout)
	defer cancel()
	var st ctrl.Stats
	if err := n.hub.Call(cctx, ctrl.MStats, nil, &st); err != nil {
		return
	}
	rep, next := n.fold(st)
	if len(rep.Deltas) == 0 && len(rep.Cumulative) == 0 {
		return
	}
	if n.cfg.OnUsage != nil {
		if err := n.cfg.OnUsage(rep); err != nil {
			return // keep the old counters: the same traffic is reported next time
		}
	}
	n.mu.Lock()
	n.last = next
	n.mu.Unlock()
}

// fold turns counters into deltas (either going backwards means telemt restarted) and returns the next baseline.
func (n *Node) fold(st ctrl.Stats) (UsageReport, map[string]Usage) {
	n.mu.Lock()
	defer n.mu.Unlock()
	rep := UsageReport{Deltas: map[string]Usage{}, Cumulative: map[string]Usage{}}
	next := make(map[string]Usage, len(n.last)+len(st.Users))
	for k, v := range n.last {
		next[k] = v
	}
	for _, u := range st.Users {
		cur := Usage{Up: u.OctetsUp, Down: u.OctetsDown}
		rep.Cumulative[u.Name] = cur
		var d Usage
		// a user first seen without a stored baseline is recorded, not counted
		if prev, seen := n.last[u.Name]; seen {
			if cur.Up < prev.Up || cur.Down < prev.Down {
				d = cur
			} else {
				d = Usage{Up: cur.Up - prev.Up, Down: cur.Down - prev.Down}
			}
		}
		next[u.Name] = cur
		if d.Up > 0 || d.Down > 0 {
			rep.Deltas[u.Name] = d
		}
	}
	return rep, next
}

// Snapshot returns the current view.
func (n *Node) Snapshot() Snapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.snapshotLocked()
}

func (n *Node) snapshotLocked() Snapshot {
	s := Snapshot{
		Link: n.hub.Status(), Agent: n.agent, AgentAt: n.agentAt,
		AppliedRev: n.applied, LastPush: n.lastPush, LastPushErr: n.pushErr,
		Disguise: n.cover, DisguiseAt: n.coverAt, DisguiseDomain: n.coverFor,
	}
	if n.desired != nil {
		s.DesiredRev = n.desired.Rev
	}
	fresh := !n.agentAt.IsZero() && n.now().Sub(n.agentAt) < 3*n.cfg.StatusEvery
	s.Healthy = s.Link.Up > 0 && fresh && (n.desired == nil || n.applied == n.desired.Rev)
	return s
}

// Call is a pass-through for control calls the node does not wrap.
func (n *Node) Call(ctx context.Context, method string, req, resp any) error {
	return n.hub.Call(ctx, method, req, resp)
}

// ErrNoState is returned by PushNow when nothing has been set.
var ErrNoState = errors.New("hubnode: no desired state set")

// PushNow pushes synchronously and reports the agent's verdict, for the UI's
// "save" button. A failure is returned and also left for the retry loop.
func (n *Node) PushNow(ctx context.Context) (ctrl.StateAck, error) {
	n.mu.Lock()
	if n.desired == nil {
		n.mu.Unlock()
		return ctrl.StateAck{}, ErrNoState
	}
	s := *n.desired
	n.mu.Unlock()
	var ack ctrl.StateAck
	err := n.hub.Call(ctx, ctrl.MStateApply, s, &ack)
	n.mu.Lock()
	if err != nil {
		n.pushErr = err.Error()
		n.mu.Unlock()
		return ack, err
	}
	n.needPush, n.pushFails, n.pushErr, n.applied, n.lastPush = n.desired.Rev != s.Rev, 0, "", s.Rev, n.now()
	n.mu.Unlock()
	if n.cfg.OnApplied != nil {
		n.cfg.OnApplied(s)
	}
	return ack, nil
}
