package link

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/mux"
	"github.com/nasnet-community/mtproto-remote-plugin/reality"
)

// DialFunc dials an address; hubs plug in their routing here (a marked dialer
// on the Linux hub, a plain one in the MikroTik container).
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// ConnSpec is one parallel connection to the agent. Naming it lets the Linux
// hub pin each to a different domestic WAN so one dying costs one connection.
type ConnSpec struct {
	Name string
	Dial DialFunc
}

// Allowlist is where the hub may dial for an agent (*tgnet.Allowlist; nil refuses
// all). An interface, so the agent, which shares this package, never links the address list.
type Allowlist interface {
	Permit(netip.AddrPort) bool
	Violation()
	ViolationsTotal() uint64
}

type HubConfig struct {
	AgentID    string
	Token      []byte
	Addr       string // the agent's hub port, host:port
	Reality    reality.ClientConfig
	Conns      []ConnSpec
	Allow      Allowlist
	DialDC     DialFunc // how Telegram is reached: the foreign exit
	MaxStreams int      // DATA streams relayed at once (default 5000)
	Mux        *mux.Config
	OnEvent    func(Event)

	HandshakeTimeout time.Duration // dial to hello; keep well above the ~5 s a restarted agent lags
	DCDialTimeout    time.Duration
}

type EventKind string

const (
	EventConnUp    EventKind = "conn_up"
	EventConnDown  EventKind = "conn_down"
	EventViolation EventKind = "violation"
	EventDialFail  EventKind = "dial_fail"
)

type Event struct {
	Kind   EventKind
	Conn   string
	Detail string
	Err    error
	At     time.Time
}

type ConnStatus struct {
	Name    string
	Up      bool
	Since   time.Time
	RTT     time.Duration
	Streams int
	LastErr string
	Skew    time.Duration // agent clock minus ours, from the last hello; 0 if the agent did not say
	// Alternate: it got in with Creds.Next or an expected cover site, not the current set
	Alternate bool
}

type Status struct {
	Conns        []ConnStatus
	Up           int // connections currently up
	Streams      int64
	Relayed      uint64
	DialFailures uint64
	Violations   uint64
	BytesToDC    uint64
	BytesFromDC  uint64
}

// Creds are what a new connection authenticates with. They can change while
// the hub runs (key rotation, a new disguise domain); connections already up
// keep working, new ones use the new values.
type Creds struct {
	Token   []byte
	Reality reality.ClientConfig
	// Next: a set the agent may already hold (a lost rotation reply); dials alternate until one gets in
	Next *Creds
}

// Hub is the router end: it dials the agent and serves what the agent opens.
type Hub struct {
	cfg   HubConfig
	conn  []*hubConn
	creds atomic.Pointer[Creds]
	sni   atomic.Pointer[string] // a cover site the agent may have moved to
	pick  atomic.Uint32          // the dial set to try next: the last one that got in

	streams  atomic.Int64
	relayed  atomic.Uint64
	dialFail atomic.Uint64
	toDC     atomic.Uint64
	fromDC   atomic.Uint64
}

type hubConn struct {
	spec ConnSpec

	mu      sync.Mutex
	sess    *mux.Session
	since   time.Time
	lastErr string
	skew    time.Duration
	alt     bool
}

func NewHub(cfg HubConfig) *Hub {
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 25 * time.Second
	}
	if cfg.DCDialTimeout == 0 {
		cfg.DCDialTimeout = 10 * time.Second
	}
	if cfg.MaxStreams == 0 {
		cfg.MaxStreams = 5000
	}
	h := &Hub{cfg: cfg}
	h.creds.Store(&Creds{Token: cfg.Token, Reality: cfg.Reality})
	for _, s := range cfg.Conns {
		h.conn = append(h.conn, &hubConn{spec: s})
	}
	return h
}

// SetCreds swaps the credentials used for connections dialled from now on.
func (h *Hub) SetCreds(c Creds) {
	h.creds.Store(&c)
	h.pick.Store(0)
}

// Creds returns the credentials new connections currently use.
func (h *Hub) Creds() Creds { return *h.creds.Load() }

// ExpectServerName: a cover site the agent may move to (its reply can be lost); dials try it too.
func (h *Hub) ExpectServerName(name string) {
	name = strings.ToLower(strings.TrimSpace(name))
	if old := h.sni.Swap(&name); old == nil || *old != name {
		h.pick.Store(0)
	}
}

// dialSets is every credential set worth a try, the current one first.
func (h *Hub) dialSets() []Creds {
	c := h.creds.Load()
	sets := []Creds{{Token: c.Token, Reality: c.Reality}}
	if c.Next != nil {
		sets = append(sets, Creds{Token: c.Next.Token, Reality: c.Next.Reality})
	}
	if name := h.sni.Load(); name != nil && *name != "" {
		for _, s := range slices.Clone(sets) {
			if !strings.EqualFold(s.Reality.ServerName, *name) {
				s.Reality.ServerName = *name
				sets = append(sets, s)
			}
		}
	}
	return sets
}

// Run keeps every connection up until ctx ends.
func (h *Hub) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, c := range h.conn {
		wg.Add(1)
		go func() { defer wg.Done(); h.keep(ctx, c) }()
	}
	wg.Wait()
	return ctx.Err()
}

func (h *Hub) emit(e Event) {
	if h.cfg.OnEvent != nil {
		e.At = time.Now()
		h.cfg.OnEvent(e)
	}
}

// keep dials, serves and redials with jittered backoff; a connection that held
// for a while starts the backoff over.
func (h *Hub) keep(ctx context.Context, c *hubConn) {
	delay := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := h.runConn(ctx, c)
		c.mu.Lock()
		c.sess = nil
		if err != nil {
			c.lastErr = err.Error()
		}
		c.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		h.emit(Event{Kind: EventConnDown, Conn: c.spec.Name, Err: err})
		if time.Since(started) > 30*time.Second || errors.Is(err, errRetired) {
			delay = time.Second
		}
		jitter := time.Duration(rand.Int64N(int64(delay) / 5))
		select {
		case <-time.After(delay + jitter):
		case <-ctx.Done():
			return
		}
		if delay *= 2; delay > time.Minute {
			delay = time.Minute
		}
	}
}

func (h *Hub) runConn(ctx context.Context, c *hubConn) error {
	dctx, cancel := context.WithTimeout(ctx, h.cfg.HandshakeTimeout)
	defer cancel()
	raw, err := c.spec.Dial(dctx, "tcp", h.cfg.Addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	sets := h.dialSets()
	i := int(h.pick.Load()) % len(sets)
	creds := sets[i]
	// a set the agent turns away hands the next attempt to the next set
	miss := func() { h.pick.CompareAndSwap(uint32(i), uint32((i+1)%len(sets))) }
	tc, err := reality.Client(dctx, raw, creds.Reality)
	if err != nil {
		raw.Close()
		miss()
		return fmt.Errorf("reality: %w", err)
	}
	mc := h.cfg.Mux
	if mc == nil {
		mc = &mux.Config{HandshakeTimeout: h.cfg.HandshakeTimeout}
	}
	sess, err := mux.Client(tc, mc)
	if err != nil {
		return fmt.Errorf("mux: %w", err)
	}
	retired := false
	defer func() {
		if !retired {
			sess.Close()
		}
	}()
	skew, err := h.handshake(dctx, sess, creds.Token)
	if err != nil {
		if errors.Is(err, errDenied) {
			miss()
		}
		return fmt.Errorf("hello: %w", err)
	}
	cancel()
	c.mu.Lock()
	c.sess, c.since, c.lastErr, c.skew, c.alt = sess, time.Now(), "", skew, i != 0
	c.mu.Unlock()
	h.emit(Event{Kind: EventConnUp, Conn: c.spec.Name})
	go func() {
		for {
			st, err := sess.Accept()
			if err != nil {
				return
			}
			go h.serveData(ctx, c, st)
		}
	}()
	select {
	case <-sess.Done():
		return sess.Err()
	case <-ctx.Done():
		return ctx.Err()
	case <-sess.PeerGoAway():
		// the agent wants a fresh connection (its stream ids are running out)
		retired = true
		go drain(ctx, sess)
		return errRetired
	}
}

var (
	errDenied  = errors.New("agent did not accept the link token")
	errRetired = errors.New("agent retired the connection")
)

// drain closes a retired connection once the streams on it are done.
func drain(ctx context.Context, sess *mux.Session) {
	defer sess.Close()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for sess.NumStreams() > 0 {
		select {
		case <-t.C:
		case <-sess.Done():
			return
		case <-ctx.Done():
			return
		}
	}
}

func (h *Hub) handshake(ctx context.Context, sess *mux.Session, token []byte) (time.Duration, error) {
	nH := nonce()
	var resp ctrl.Hello
	st, err := sess.Open([]byte("ctrl"), nil)
	if err != nil {
		return 0, err
	}
	sent := time.Now()
	if err := ctrl.Call(ctx, st, ctrl.MHello, ctrl.Hello{Version: ctrl.ProtocolVersion, Nonce: nH, TokenID: tokenID(token)}, &resp); err != nil {
		return 0, err
	}
	rtt := time.Since(sent)
	if resp.AgentID != h.cfg.AgentID || !macEqual(resp.MAC, mac(token, "agent", nH, resp.Nonce, h.cfg.AgentID)) {
		return 0, errDenied
	}
	var skew time.Duration
	if resp.Now != 0 {
		// the agent read its clock about halfway through the round trip
		skew = time.UnixMilli(resp.Now).Sub(sent.Add(rtt / 2))
	}
	st, err = sess.Open([]byte("ctrl"), nil)
	if err != nil {
		return 0, err
	}
	return skew, ctrl.Call(ctx, st, ctrl.MHelloConfirm, ctrl.HelloConfirm{MAC: mac(token, "hub", resp.Nonce, nH, h.cfg.AgentID)}, nil)
}

// serveData relays one agent-opened stream to a Telegram datacentre.
func (h *Hub) serveData(ctx context.Context, c *hubConn, st *mux.Stream) {
	dst := string(st.Meta())
	ap, err := netip.ParseAddrPort(dst)
	if err != nil || h.cfg.Allow == nil || !h.cfg.Allow.Permit(ap) {
		if h.cfg.Allow != nil {
			h.cfg.Allow.Violation()
		}
		h.emit(Event{Kind: EventViolation, Conn: c.spec.Name, Detail: dst})
		st.Reset()
		return
	}
	if h.streams.Add(1) > int64(h.cfg.MaxStreams) {
		h.streams.Add(-1)
		st.Reset()
		return
	}
	defer h.streams.Add(-1)
	dctx, cancel := context.WithTimeout(ctx, h.cfg.DCDialTimeout)
	dc, err := h.cfg.DialDC(dctx, "tcp", dst)
	cancel()
	if err != nil {
		h.dialFail.Add(1)
		h.emit(Event{Kind: EventDialFail, Conn: c.spec.Name, Detail: dst, Err: err})
		st.Reset()
		return
	}
	h.relayed.Add(1)
	up, down := Pipe(st, dc)
	h.toDC.Add(uint64(up))
	h.fromDC.Add(uint64(down))
}

// Call runs a control method on the agent over the least-loaded live connection.
func (h *Hub) Call(ctx context.Context, method string, req, resp any) error {
	st, err := h.ctlStream()
	if err != nil {
		return err
	}
	defer st.Close()
	return ctrl.Call(ctx, st, method, req, resp)
}

// CallRaw is Call with a raw body after the request (agent updates).
func (h *Hub) CallRaw(ctx context.Context, method string, req any, body interface{ Read([]byte) (int, error) }, resp any) error {
	st, err := h.ctlStream()
	if err != nil {
		return err
	}
	defer st.Close()
	return ctrl.CallRaw(ctx, st, method, req, body, resp)
}

func (h *Hub) ctlStream() (*mux.Stream, error) {
	var best *mux.Session
	bestN := 0
	for _, c := range h.conn {
		c.mu.Lock()
		s := c.sess
		c.mu.Unlock()
		if s == nil || s.Err() != nil || s.Draining() {
			continue
		}
		if n := s.NumStreams(); best == nil || n < bestN {
			best, bestN = s, n
		}
	}
	if best == nil {
		return nil, ErrNoLink
	}
	return best.Open([]byte("ctrl"), nil)
}

func (h *Hub) Status() Status {
	st := Status{
		Streams: h.streams.Load(), Relayed: h.relayed.Load(), DialFailures: h.dialFail.Load(),
		BytesToDC: h.toDC.Load(), BytesFromDC: h.fromDC.Load(),
	}
	if h.cfg.Allow != nil {
		st.Violations = h.cfg.Allow.ViolationsTotal()
	}
	for _, c := range h.conn {
		c.mu.Lock()
		cs := ConnStatus{Name: c.spec.Name, LastErr: c.lastErr, Since: c.since, Skew: c.skew}
		if c.sess != nil && c.sess.Err() == nil {
			cs.Up, cs.RTT, cs.Streams, cs.Alternate = true, c.sess.RTT(), c.sess.NumStreams(), c.alt
			st.Up++
		}
		c.mu.Unlock()
		st.Conns = append(st.Conns, cs)
	}
	return st
}

// WaitUp blocks until at least one connection is up.
func (h *Hub) WaitUp(ctx context.Context) error {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		if h.Status().Up > 0 {
			return nil
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
