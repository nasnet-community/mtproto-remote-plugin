// Package manager keeps one link per stored server, pushes its users and books
// their usage; relayops drives it.
package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/hubnode"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/store"
	"github.com/nasnet-community/mtproto-remote-plugin/link"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops"
	"github.com/nasnet-community/mtproto-remote-plugin/telemt"
	"github.com/nasnet-community/mtproto-remote-plugin/tgnet"
)

var (
	ErrNotRunning = errors.New("the server's link is not running")
	ErrBusy       = errors.New("an install to this host is already running")
	ErrHostTaken  = errors.New("this host is already a server here: repair it instead")
)

// Store is the part of the plugin's store the manager uses.
type Store interface {
	List() []store.Server
	Get(id uint) (store.Server, bool)
	Create(n store.NewServer) (uint, error)
	Delete(id uint) error
	Mutate(id uint, fn func(*store.Server) error) error
	Limits() store.Limits
}

type Config struct {
	Store  Store
	Allow  *tgnet.Refresher
	Dial   link.DialFunc // to the agents
	DialDC link.DialFunc // to the datacentres
	Conns  int           // link connections per server (default 2)

	StatusEvery time.Duration // hubnode defaults when zero
	StatsEvery  time.Duration
	// FlushEvery is how often counted usage is written to the router's flash (default 5 min)
	FlushEvery time.Duration
	History    *store.History // finished days; nil keeps none
	Log        *slog.Logger
	Now        func() time.Time
}

// Event is one line of a server's history for the UI.
type Event struct {
	At     time.Time
	Kind   string
	Detail string
	Err    string // raw, so the API can word it
}

// Install is a pending install's claim on the limits until its job ends.
type Install struct {
	Host       string
	MaxStreams int
	FirstUser  string
}

type Manager struct {
	cfg Config

	startMu sync.Mutex // one (re)start at a time

	mu      sync.Mutex
	ctx     context.Context
	entries map[uint]*entry
	pending map[string]Install
	rings   map[uint]*ring
	wg      sync.WaitGroup
}

type entry struct {
	id     uint
	node   *hubnode.Node
	cancel context.CancelFunc
	done   chan struct{}

	mu   sync.Mutex
	rec  relayops.Record
	up   map[string]bool // link connections currently up
	seen time.Time       // when versions/last-seen were last stored

	push sync.Mutex // one read-and-set of the users at a time, so an older read never lands last

	usage   *usage
	flushMu sync.Mutex // one usage write at a time
}

func New(cfg Config) *Manager {
	if cfg.Conns == 0 {
		cfg.Conns = 2
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.FlushEvery == 0 {
		cfg.FlushEvery = 5 * time.Minute
	}
	if cfg.Dial == nil || cfg.DialDC == nil {
		d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		if cfg.Dial == nil {
			cfg.Dial = d.DialContext
		}
		if cfg.DialDC == nil {
			cfg.DialDC = d.DialContext
		}
	}
	return &Manager{cfg: cfg, entries: map[uint]*entry{}, pending: map[string]Install{}, rings: map[uint]*ring{}}
}

// Start brings up every registered server; links live until ctx ends.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	for _, srv := range m.cfg.Store.List() {
		if !srv.Registered() {
			// an install that died between register and save; with no host key or token a repair can't help
			m.cfg.Log.Warn("server has no link credentials: remove it and add it again", "server", srv.ID)
			continue
		}
		if err := m.start(srv.ID); err != nil {
			m.cfg.Log.Warn("could not start a server's link", "server", srv.ID, "err", err)
		}
	}
	go m.flushLoop(ctx.Done())
	return nil
}

// Stop ends every link, waits for them and writes what they counted.
func (m *Manager) Stop() {
	m.mu.Lock()
	es := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		e.cancel()
		es = append(es, e)
	}
	m.entries = map[uint]*entry{}
	m.mu.Unlock()
	m.wg.Wait()
	for _, e := range es {
		if err := m.flush(e); err != nil {
			m.cfg.Log.Warn("could not store usage before stopping; it is counted again from the last baseline", "server", e.id, "err", err)
		}
	}
}

func (m *Manager) entry(id uint) *entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entries[id]
}

func (m *Manager) baseCtx() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

// start (re)starts one server's link from what is stored.
func (m *Manager) start(id uint) error {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	// the chart outlives a settings change that restarts the link
	var carried []Sample
	if old := m.entry(id); old != nil {
		carried = m.Traffic(id)
	}
	m.stop(id)
	// read only after the old node is gone, so its last usage write is in the baseline
	srv, ok := m.cfg.Store.Get(id)
	if !ok {
		return relayops.ErrNotFound
	}
	cr, err := srv.Record.LinkCreds()
	if err != nil {
		return err
	}
	conns := make([]link.ConnSpec, m.cfg.Conns)
	for i := range conns {
		conns[i] = link.ConnSpec{Name: "link-" + strconv.Itoa(i+1), Dial: m.cfg.Dial}
	}
	ctx, cancel := context.WithCancel(m.baseCtx())
	e := &entry{id: id, cancel: cancel, done: make(chan struct{}), rec: srv.Record, up: map[string]bool{}, usage: newUsage()}
	e.usage.samples = carried
	e.node = hubnode.New(hubnode.Config{
		Link: link.HubConfig{
			AgentID: srv.Record.AgentID, Token: cr.Token, Reality: cr.Reality,
			Addr:  net.JoinHostPort(srv.Host, strconv.Itoa(srv.Record.HubPort)),
			Conns: conns, Allow: m.cfg.Allow.List, DialDC: m.cfg.DialDC,
			MaxStreams: srv.Record.MaxStreams,
		},
		Baseline:    srv.Baseline,
		StatusEvery: m.cfg.StatusEvery,
		StatsEvery:  m.cfg.StatsEvery,
		OnUsage: func(r hubnode.UsageReport) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			m.onUsage(e, r)
			return nil
		},
		OnApplied: func(s ctrl.State) { m.onApplied(e, s) },
		OnEvent:   func(ev hubnode.Event) { m.onEvent(e, ev) },
		OnStatus:  func(s hubnode.Snapshot) { m.onStatus(e, s) },
	})
	e.node.SetCreds(cr) // the config has no room for a pending rotation's set

	m.mu.Lock()
	m.entries[id] = e
	m.mu.Unlock()
	// the users are read once the entry is visible, so a UsersChanged in between is not lost
	m.pushUsers(e)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(e.done)
		_ = e.node.Run(ctx)
	}()
	return nil
}

// stop ends a server's link and waits for its loop to finish.
func (m *Manager) stop(id uint) {
	m.mu.Lock()
	e := m.entries[id]
	delete(m.entries, id)
	m.mu.Unlock()
	if e != nil {
		e.cancel()
		<-e.done
		// before a restart reads the baseline back
		if err := m.flush(e); err != nil {
			m.cfg.Log.Warn("could not store usage; it is counted again from the last baseline", "server", id, "err", err)
		}
	}
}

// Ensure (re)starts a server's link, after an install, repair or settings change.
func (m *Manager) Ensure(_ context.Context, id uint) error { return m.start(id) }

// Remove stops a server's link and forgets its history.
func (m *Manager) Remove(id uint) {
	m.stop(id)
	m.mu.Lock()
	delete(m.rings, id)
	m.mu.Unlock()
	if m.cfg.History != nil {
		m.cfg.History.Forget(id, "")
	}
}

func (m *Manager) IsRunning(id uint) bool { return m.entry(id) != nil }

// Snapshot is a server's live state.
func (m *Manager) Snapshot(id uint) (hubnode.Snapshot, bool) {
	e := m.entry(id)
	if e == nil {
		return hubnode.Snapshot{}, false
	}
	return e.node.Snapshot(), true
}

func (m *Manager) Call(ctx context.Context, id uint, method string, req, resp any) error {
	e := m.entry(id)
	if e == nil {
		return ErrNotRunning
	}
	return e.node.Call(ctx, method, req, resp)
}

func (m *Manager) CallRaw(ctx context.Context, id uint, method string, req any, body io.Reader, resp any) error {
	e := m.entry(id)
	if e == nil {
		return ErrNotRunning
	}
	return e.node.Hub().CallRaw(ctx, method, req, body, resp)
}

// SetCreds follows a key rotation or repair; relayops has already stored them.
func (m *Manager) SetCreds(_ context.Context, r *relayops.Record) error {
	e := m.entry(r.NodeID)
	if e == nil {
		return ErrNotRunning
	}
	// one hold, so a cover site applied meanwhile is neither undone nor dialled with the old name
	e.mu.Lock()
	defer e.mu.Unlock()
	next := e.rec
	next.RealityPub, next.ShortID, next.Token, next.NextSeed = r.RealityPub, r.ShortID, r.Token, r.NextSeed
	cr, err := next.LinkCreds()
	if err != nil {
		return err
	}
	e.rec = next
	e.node.SetCreds(cr)
	return nil
}

// AdoptCover takes the cover site a repair found on the box; SaveRecord keeps the stored one.
func (m *Manager) AdoptCover(_ context.Context, id uint, domain string) error {
	m.startMu.Lock() // a start reading the record now would dial the old name
	defer m.startMu.Unlock()
	err := m.cfg.Store.Mutate(id, func(srv *store.Server) error { srv.Record.Disguise = domain; return nil })
	if errors.Is(err, store.ErrNotFound) {
		return relayops.ErrNotFound
	}
	if err != nil {
		return err
	}
	if e := m.entry(id); e != nil {
		e.mu.Lock()
		e.rec.Disguise = domain
		if cr, err := e.rec.LinkCreds(); err == nil {
			e.node.SetCreds(cr)
		}
		e.mu.Unlock()
	}
	return nil
}

// UsersChanged rebuilds a server's desired state; the link pushes it.
func (m *Manager) UsersChanged(id uint) {
	if e := m.entry(id); e != nil {
		m.pushUsers(e)
	}
}

// pushUsers hands the node what is stored now.
func (m *Manager) pushUsers(e *entry) {
	e.push.Lock()
	defer e.push.Unlock()
	if srv, ok := m.cfg.Store.Get(e.id); ok {
		e.node.SetDesired(m.Desired(srv))
	}
}

// PrefixesChanged re-pushes every server after an allowlist refresh.
func (m *Manager) PrefixesChanged() {
	m.mu.Lock()
	ids := make([]uint, 0, len(m.entries))
	for id := range m.entries {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.UsersChanged(id)
	}
}

// --- desired state ---

// telemtSettings is FakeTLS only, like the old plugin: user links are ee-links. An
// option left at its default adds no key, so the revision of an untouched server stays the same.
func telemtSettings(o store.ProxyOptions) map[string]any {
	set := map[string]any{"modes": telemt.Modes{TLS: true}, "log_level": "normal"}
	if o.LogLevel != "" {
		set["log_level"] = o.LogLevel
	}
	if len(o.ExtraCoverNames) > 0 {
		set["tls_domains_extra"] = o.ExtraCoverNames
	}
	if o.DeviceWindowSecs > 0 {
		set["unique_ip_window_secs"] = o.DeviceWindowSecs
	}
	return set
}

// Desired is the state a server's agent should run.
func (m *Manager) Desired(srv store.Server) ctrl.State {
	st := ctrl.State{
		Port: 443, Disguise: srv.CoverDomain(), Telemt: telemtSettings(srv.ProxyOptions),
		MaxStreams: srv.Record.MaxStreams, Users: []ctrl.User{}, Stopped: srv.ProxyStopped,
	}
	for _, u := range srv.Users {
		st.Users = append(st.Users, ctrl.User{
			Name: u.Name, Secret: u.Secret, Enabled: u.Enabled && !u.OverQuota(),
			MaxIPs: u.MaxIPs, ExpiresAt: u.ExpiresAt,
		})
	}
	slices.SortFunc(st.Users, func(a, b ctrl.User) int { return strings.Compare(a.Name, b.Name) })
	for _, p := range m.cfg.Allow.List.Prefixes() {
		st.DCNets = append(st.DCNets, p.String())
	}
	slices.Sort(st.DCNets)
	st.Rev = Rev(st)
	return st
}

// Rev is a content hash, so the agent's reported revision shows drift.
func Rev(s ctrl.State) string {
	s.Rev = ""
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// --- callbacks from the link ---

// onApplied follows a pushed state: a new cover site changes the link's SNI.
func (m *Manager) onApplied(e *entry, s ctrl.State) {
	e.mu.Lock()
	if s.Disguise == "" || s.Disguise == e.rec.Disguise {
		e.mu.Unlock()
		return
	}
	e.rec.Disguise = s.Disguise
	if cr, err := e.rec.LinkCreds(); err == nil {
		e.node.SetCreds(cr) // under the lock, in step with SetCreds
	}
	e.mu.Unlock()
	err := m.cfg.Store.Mutate(e.id, func(srv *store.Server) error { srv.Record.Disguise = s.Disguise; return nil })
	if err != nil {
		m.cfg.Log.Warn("could not store the applied cover site", "server", e.id, "err", err)
	}
}

func (m *Manager) onStatus(e *entry, s hubnode.Snapshot) {
	if s.Agent == nil {
		return
	}
	now := m.cfg.Now()
	e.mu.Lock()
	changed := e.rec.AgentVersion != s.Agent.AgentVersion || e.rec.TelemtVersion != s.Agent.TelemtVersion
	if !changed && now.Sub(e.seen) < 5*time.Minute {
		e.mu.Unlock()
		return
	}
	e.rec.AgentVersion, e.rec.TelemtVersion, e.seen = s.Agent.AgentVersion, s.Agent.TelemtVersion, now
	av, tv := e.rec.AgentVersion, e.rec.TelemtVersion
	e.mu.Unlock()
	seen := now.UTC()
	err := m.cfg.Store.Mutate(e.id, func(srv *store.Server) error {
		srv.Record.AgentVersion, srv.Record.TelemtVersion, srv.Record.LastSeenAt = av, tv, &seen
		return nil
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		m.cfg.Log.Warn("could not store the agent's versions", "server", e.id, "err", err)
	}
}

// quiet is how long repeats of a noisy event stay out of the history.
var quiet = map[hubnode.EventKind]time.Duration{
	hubnode.EventCrashLoop:   5 * time.Minute,
	hubnode.EventViolation:   time.Minute,
	hubnode.EventDialFailure: time.Minute,
	hubnode.EventPushFailed:  time.Minute,
}

func (m *Manager) onEvent(e *entry, ev hubnode.Event) {
	detail := ev.Detail
	switch ev.Kind {
	case hubnode.EventLinkUp:
		// per-connection events; the history wants the server's link as a whole
		e.mu.Lock()
		was := len(e.up) > 0
		e.up[ev.Detail] = true
		e.mu.Unlock()
		if was {
			return
		}
		detail = ""
	case hubnode.EventLinkDown:
		e.mu.Lock()
		if !e.up[ev.Detail] {
			e.mu.Unlock()
			return // a dial that failed, not a link that went down
		}
		delete(e.up, ev.Detail)
		still := len(e.up) > 0
		e.mu.Unlock()
		if still {
			return
		}
		detail = ""
	case hubnode.EventTelemtDown, hubnode.EventTelemtUp, hubnode.EventCrashLoop,
		hubnode.EventViolation, hubnode.EventDialFailure, hubnode.EventPushFailed:
	default:
		return
	}
	var why string
	if ev.Err != nil {
		why = ev.Err.Error()
	}
	at := ev.At
	if at.IsZero() {
		at = m.cfg.Now()
	}
	m.mu.Lock()
	r := m.rings[e.id]
	if r == nil {
		r = &ring{last: map[string]time.Time{}}
		m.rings[e.id] = r
	}
	kept := r.add(Event{At: at, Kind: string(ev.Kind), Detail: detail, Err: why}, quiet[ev.Kind])
	m.mu.Unlock()
	if !kept {
		return
	}
	lvl := slog.LevelWarn
	if ev.Kind == hubnode.EventLinkUp || ev.Kind == hubnode.EventTelemtUp {
		lvl = slog.LevelInfo
	}
	m.cfg.Log.Log(context.Background(), lvl, "server event", "server", e.id, "kind", ev.Kind, "detail", detail, "err", why)
}

// Events is a server's recent history, newest first.
func (m *Manager) Events(id uint) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rings[id]; r != nil {
		return r.list()
	}
	return nil
}

const ringSize = 50

type ring struct {
	ev   []Event // oldest first
	last map[string]time.Time
}

func (r *ring) add(e Event, quiet time.Duration) bool {
	if t, ok := r.last[e.Kind]; ok && quiet > 0 && e.At.Sub(t) < quiet {
		return false
	}
	r.last[e.Kind] = e.At
	r.ev = append(r.ev, e)
	if len(r.ev) > ringSize {
		r.ev = slices.Delete(r.ev, 0, len(r.ev)-ringSize)
	}
	return true
}

func (r *ring) list() []Event {
	out := slices.Clone(r.ev)
	slices.Reverse(out)
	return out
}

// --- installs ---

// Reserve holds an install's place within the limits; release gives it back.
func (m *Manager) Reserve(in Install) (func(), error) {
	lim := m.cfg.Store.Limits()
	// lock order: m.mu, then the store's own
	m.mu.Lock()
	defer m.mu.Unlock()
	stored := m.cfg.Store.List()
	if _, ok := m.pending[in.Host]; ok {
		return nil, ErrBusy
	}
	for _, s := range stored {
		if s.Host == in.Host {
			return nil, ErrHostTaken
		}
	}
	if len(stored)+len(m.pending) >= lim.MaxServers {
		if lim.MaxServers == 1 {
			return nil, fmt.Errorf("%w: this router runs at most 1 server", store.ErrServerCap)
		}
		return nil, fmt.Errorf("%w: this router runs at most %d servers", store.ErrServerCap, lim.MaxServers)
	}
	if used := m.allocatedLocked(stored); used+in.MaxStreams > lim.StreamBudget {
		return nil, fmt.Errorf("%w: %d of %d streams are already allocated", store.ErrStreamBudget, used, lim.StreamBudget)
	}
	m.pending[in.Host] = in
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if p, ok := m.pending[in.Host]; ok && p == in {
				delete(m.pending, in.Host)
			}
			m.mu.Unlock()
		})
	}, nil
}

// Allocated is max_streams summed over stored servers and pending installs.
func (m *Manager) Allocated() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.allocatedLocked(m.cfg.Store.List())
}

// Edit changes a stored server; a bigger stream cap has to fit beside pending installs too.
func (m *Manager) Edit(id uint, fn func(*store.Server) error) error {
	lim := m.cfg.Store.Limits()
	// lock order: m.mu, then the store's own
	m.mu.Lock()
	defer m.mu.Unlock()
	used := m.allocatedLocked(m.cfg.Store.List())
	return m.cfg.Store.Mutate(id, func(s *store.Server) error {
		before := s.Record.MaxStreams
		if err := fn(s); err != nil {
			return err
		}
		if left := lim.StreamBudget - (used - before); s.Record.MaxStreams > before && s.Record.MaxStreams > left {
			return fmt.Errorf("%w: only %d streams are left on this router", store.ErrStreamBudget, max(left, 0))
		}
		return nil
	})
}

func (m *Manager) allocatedLocked(stored []store.Server) int {
	n := 0
	for _, s := range stored {
		n += s.Record.MaxStreams
	}
	for _, p := range m.pending {
		n += p.MaxStreams
	}
	return n
}

// Pending is how many installs hold a reservation.
func (m *Manager) Pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}

func (m *Manager) createNode(name, host string) (uint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, ok := m.pending[host]
	delete(m.pending, host)
	n := store.NewServer{Name: name, Host: host, MaxStreams: store.DefaultMaxStreams}
	if ok {
		n.MaxStreams = in.MaxStreams
		if in.FirstUser != "" {
			sec, err := telemt.GenerateSecret()
			if err != nil {
				return 0, err
			}
			n.Users = []store.User{{Name: in.FirstUser, Secret: sec, Enabled: true, CreatedAt: m.cfg.Now().UTC()}}
		}
	}
	return m.cfg.Store.Create(n)
}

// --- relayops adapters ---

// Links is the manager as relayops drives it.
func (m *Manager) Links() relayops.Links { return linksAdapter{m} }

type linksAdapter struct{ m *Manager }

var _ relayops.CoverAdopter = linksAdapter{} // relayops asserts it at run time; a typo would skip it

func (a linksAdapter) Ensure(ctx context.Context, id uint) error { return a.m.Ensure(ctx, id) }
func (a linksAdapter) IsRunning(id uint) bool                    { return a.m.IsRunning(id) }
func (a linksAdapter) Remove(id uint)                            { a.m.Remove(id) }
func (a linksAdapter) Redial(ctx context.Context, id uint) error { return a.m.Ensure(ctx, id) }
func (a linksAdapter) SetCreds(ctx context.Context, r *relayops.Record) error {
	return a.m.SetCreds(ctx, r)
}
func (a linksAdapter) AdoptCover(ctx context.Context, id uint, domain string) error {
	return a.m.AdoptCover(ctx, id, domain)
}
func (a linksAdapter) Call(ctx context.Context, id uint, method string, req, resp any) error {
	return a.m.Call(ctx, id, method, req, resp)
}
func (a linksAdapter) CallRaw(ctx context.Context, id uint, method string, req any, body io.Reader, resp any) error {
	return a.m.CallRaw(ctx, id, method, req, body, resp)
}
func (a linksAdapter) View(id uint) (relayops.LinkView, bool) {
	s, ok := a.m.Snapshot(id)
	if !ok {
		return relayops.LinkView{}, false
	}
	v := relayops.LinkView{Up: s.Link.Up > 0}
	if s.Agent != nil {
		v.AgentVersion, v.AgentBuild, v.TelemtVersion = s.Agent.AgentVersion, s.Agent.AgentBuild, s.Agent.TelemtVersion
	}
	return v, true
}

// Admin is the plugin's server list as relayops sees it.
func (m *Manager) Admin() relayops.NodeAdmin { return adminAdapter{m} }

type adminAdapter struct{ m *Manager }

func (a adminAdapter) CreateNode(_ context.Context, name, host string) (uint, error) {
	return a.m.createNode(name, host)
}

func (a adminAdapter) DeleteNode(_ context.Context, id uint) error {
	a.m.Remove(id)
	return a.m.cfg.Store.Delete(id)
}

func (a adminAdapter) SetHost(_ context.Context, id uint, host string) error {
	return a.m.cfg.Store.Mutate(id, func(s *store.Server) error {
		s.Host = host
		return nil
	})
}

func (a adminAdapter) Host(_ context.Context, id uint) (string, error) {
	srv, ok := a.m.cfg.Store.Get(id)
	if !ok {
		return "", relayops.ErrNotFound
	}
	return srv.Host, nil
}
