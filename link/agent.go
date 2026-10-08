package link

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/mux"
	"github.com/nasnet-community/mtproto-remote-plugin/realityserver"
)

var (
	ErrNoLink = errors.New("link: no authenticated connection")
	ErrBusy   = errors.New("link: stream cap reached")
)

type AgentConfig struct {
	AgentID       string
	Tokens        [][]byte // any of these authenticates the hub (two during a rotation)
	Listener      net.Listener
	Reality       realityserver.ServerConfig
	Handlers      map[string]ctrl.Handler
	Mux           *mux.Config
	MaxHandshakes int // unauthenticated handshakes in flight (default 128)
	MaxStreams    int // DATA streams open at once (default 5000)
	MaxPending    int // connections past REALITY that have not said hello yet (default 16)
	AuthTimeout   time.Duration
	Info          func() ctrl.Hello // versions/caps for the hello reply
	Now           func() time.Time  // the clock the hello reply reports (default time.Now)
}

// Agent is the VPS end of the link.
type Agent struct {
	cfg        AgentConfig
	tokens     atomic.Pointer[[][]byte]
	srv        atomic.Pointer[realityserver.Server]
	maxStreams atomic.Int64

	mu       sync.Mutex
	sessions map[*agentSession]struct{}
	pending  int                        // sessions waiting for their hello
	reality  realityserver.ServerConfig // the latest, for a Serve still to come
	closed   bool

	Accepted atomic.Uint64 // authenticated connections, lifetime
	AuthFail atomic.Uint64
}

type agentSession struct {
	sess    *mux.Session
	authed  atomic.Bool
	since   time.Time
	waiting bool // guarded by Agent.mu: counted in pending

	mu    sync.Mutex
	nH    []byte
	nA    []byte
	token []byte
}

// preHello is how many control streams a session may hold before its hello.
const preHello = 2

func NewAgent(cfg AgentConfig) *Agent {
	if cfg.MaxHandshakes == 0 {
		cfg.MaxHandshakes = 128
	}
	if cfg.MaxPending == 0 {
		cfg.MaxPending = 16
	}
	if cfg.AuthTimeout == 0 {
		cfg.AuthTimeout = 20 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	a := &Agent{cfg: cfg, sessions: map[*agentSession]struct{}{}, reality: cfg.Reality}
	toks := cfg.Tokens
	a.tokens.Store(&toks)
	a.SetMaxStreams(cfg.MaxStreams)
	return a
}

// SetTokens swaps the accepted link tokens (key rotation).
func (a *Agent) SetTokens(t [][]byte) { a.tokens.Store(&t) }

// SetMaxStreams changes how many DATA streams may be open at once (0: 5000).
func (a *Agent) SetMaxStreams(n int) {
	if n <= 0 {
		n = 5000
	}
	a.maxStreams.Store(int64(n))
}

// MaxStreams is the cap OpenData enforces.
func (a *Agent) MaxStreams() int { return int(a.maxStreams.Load()) }

// Reconfigure applies a new REALITY config (new key or disguise domain) to every
// connection accepted from now on; established ones carry on.
func (a *Agent) Reconfigure(c realityserver.ServerConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reality = c // before Serve, Serve picks it up
	if s := a.srv.Load(); s != nil {
		s.Reconfigure(c)
	}
}

func (a *Agent) Warm() bool {
	s := a.srv.Load()
	return s != nil && s.Warm()
}

// Close stops accepting hub connections and drops the ones that are up. Streams
// in flight are reset, which telemt sees as the datacentre going away.
func (a *Agent) Close() {
	a.mu.Lock()
	a.closed = true
	srv := a.srv.Load() // under mu: a Serve racing this either sees closed or is seen here
	sessions := make([]*agentSession, 0, len(a.sessions))
	for s := range a.sessions {
		sessions = append(sessions, s)
	}
	a.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
	for _, s := range sessions {
		s.sess.Close()
	}
}

// Serve accepts hub connections until ctx ends.
func (a *Agent) Serve(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return net.ErrClosed
	}
	srv := realityserver.New(a.cfg.Listener, a.reality, a.cfg.MaxHandshakes)
	a.srv.Store(srv)
	a.mu.Unlock()
	go func() { <-ctx.Done(); a.Close() }()
	return srv.Serve(func(c net.Conn) { go a.handleConn(ctx, c) })
}

func (a *Agent) handleConn(ctx context.Context, c net.Conn) {
	mc := a.cfg.Mux
	sess, err := mux.Server(c, mc)
	if err != nil {
		c.Close()
		return
	}
	as := &agentSession{sess: sess, since: time.Now(), waiting: true}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		sess.Close()
		return
	}
	// only so many may wait for their hello; the oldest makes room, as the newest may be the hub
	var evict *agentSession
	if a.pending >= a.cfg.MaxPending {
		for s := range a.sessions {
			if s.waiting && (evict == nil || s.since.Before(evict.since)) {
				evict = s
			}
		}
		if evict != nil {
			a.settleLocked(evict)
		}
	}
	a.pending++
	a.sessions[as] = struct{}{}
	a.mu.Unlock()
	if evict != nil {
		a.AuthFail.Add(1)
		evict.sess.Close()
	}
	defer func() {
		a.mu.Lock()
		delete(a.sessions, as)
		a.settleLocked(as)
		a.mu.Unlock()
		sess.Close()
	}()
	// a connection that never proves the token is dropped without a word
	t := time.AfterFunc(a.cfg.AuthTimeout, func() {
		if !as.authed.Load() {
			a.AuthFail.Add(1)
			sess.Close()
		}
	})
	defer t.Stop()
	slots := make(chan struct{}, preHello)
	for {
		st, err := sess.Accept()
		if err != nil {
			return
		}
		if as.authed.Load() {
			go a.serveCtl(ctx, as, st)
			continue
		}
		// before the hello: control streams only, a couple at a time
		if string(st.Meta()) != "ctrl" {
			st.Reset()
			continue
		}
		select {
		case slots <- struct{}{}:
			go func() { defer func() { <-slots }(); a.serveCtl(ctx, as, st) }()
		default:
			st.Reset()
		}
	}
}

// settleLocked takes as off the pending count, once.
func (a *Agent) settleLocked(as *agentSession) {
	if as.waiting {
		as.waiting = false
		a.pending--
	}
}

func (a *Agent) serveCtl(ctx context.Context, as *agentSession, st *mux.Stream) {
	handlers := map[string]ctrl.Handler{
		ctrl.MHello:        func(_ context.Context, b json.RawMessage, _ io.Reader) (any, error) { return a.hello(as, b) },
		ctrl.MHelloConfirm: func(_ context.Context, b json.RawMessage, _ io.Reader) (any, error) { return nil, a.confirm(as, b) },
	}
	for name, h := range a.cfg.Handlers {
		h := h
		handlers[name] = func(ctx context.Context, b json.RawMessage, raw io.Reader) (any, error) {
			if !as.authed.Load() {
				return nil, &ctrl.Error{Code: "unauthenticated", Msg: "say hello first"}
			}
			return h(ctx, b, raw)
		}
	}
	ctrl.Serve(ctx, st, handlers)
}

func (a *Agent) hello(as *agentSession, body json.RawMessage) (any, error) {
	var req ctrl.Hello
	if err := json.Unmarshal(body, &req); err != nil || req.Version != ctrl.ProtocolVersion || len(req.Nonce) < 8 {
		return nil, &ctrl.Error{Code: "bad_hello", Msg: "bad hello"}
	}
	nA := nonce()
	as.mu.Lock()
	as.nH, as.nA = req.Nonce, nA
	as.mu.Unlock()
	out := ctrl.Hello{Version: ctrl.ProtocolVersion, AgentID: a.cfg.AgentID, Nonce: nA}
	if a.cfg.Info != nil {
		info := a.cfg.Info()
		out.AgentVersion, out.TelemtVersion, out.Caps = info.AgentVersion, info.TelemtVersion, info.Caps
	}
	out.MaxStreams = a.MaxStreams()
	out.Now = a.cfg.Now().UnixMilli()
	// prove with the token the hub says it holds; a hub that does not say gets the newest
	toks := *a.tokens.Load()
	if len(toks) == 0 {
		return nil, &ctrl.Error{Code: "no_token", Msg: "no token"}
	}
	tok := toks[0]
	for _, t := range toks {
		if len(req.TokenID) > 0 && hmac.Equal(tokenID(t), req.TokenID) {
			tok = t
			break
		}
	}
	out.MAC = mac(tok, "agent", req.Nonce, nA, a.cfg.AgentID)
	return out, nil
}

func (a *Agent) confirm(as *agentSession, body json.RawMessage) error {
	var req ctrl.HelloConfirm
	if err := json.Unmarshal(body, &req); err != nil {
		return &ctrl.Error{Code: "bad_hello", Msg: "bad confirm"}
	}
	as.mu.Lock()
	nH, nA := as.nH, as.nA
	as.mu.Unlock()
	if nH == nil {
		return &ctrl.Error{Code: "bad_hello", Msg: "confirm before hello"}
	}
	for _, tok := range *a.tokens.Load() {
		if macEqual(req.MAC, mac(tok, "hub", nA, nH, a.cfg.AgentID)) {
			if !a.authenticate(as) {
				return &ctrl.Error{Code: "denied", Msg: "denied"} // evicted while proving itself
			}
			a.Accepted.Add(1)
			return nil
		}
	}
	a.AuthFail.Add(1)
	go as.sess.Close()
	return &ctrl.Error{Code: "denied", Msg: "denied"}
}

// authenticate marks as authenticated unless it was already evicted for waiting too long.
func (a *Agent) authenticate(as *agentSession) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if as.authed.Load() {
		return true
	}
	if !as.waiting {
		return false
	}
	a.settleLocked(as)
	as.authed.Store(true)
	return true
}

// Streams counts streams on authenticated connections only: no hello, no share of the cap.
func (a *Agent) Streams() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for s := range a.sessions {
		if s.authed.Load() {
			n += s.sess.NumStreams()
		}
	}
	return n
}

// Conns is how many authenticated hub connections are up.
func (a *Agent) Conns() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for s := range a.sessions {
		if s.authed.Load() {
			n++
		}
	}
	return n
}

// OpenData opens a stream to the hub for dst ("ip:port") carrying first as its
// opening bytes. It picks the least-loaded authenticated connection, one that
// is being retired only when there is nothing else.
func (a *Agent) OpenData(dst string, first []byte) (*mux.Stream, error) {
	if a.Streams() >= a.MaxStreams() {
		return nil, ErrBusy
	}
	type cand struct {
		s     *mux.Session
		drain bool
		n     int
	}
	a.mu.Lock()
	var cands []cand
	for s := range a.sessions {
		if s.authed.Load() && s.sess.Err() == nil {
			cands = append(cands, cand{s.sess, s.sess.Draining(), s.sess.NumStreams()})
		}
	}
	a.mu.Unlock()
	slices.SortFunc(cands, func(x, y cand) int {
		if x.drain != y.drain {
			if x.drain {
				return 1
			}
			return -1
		}
		return x.n - y.n
	})
	err := ErrNoLink
	for _, c := range cands {
		st, e := c.s.Open([]byte(dst), first)
		if e == nil {
			return st, nil
		}
		err = e
		// a connection going away (or gone) is no reason to fail: try the next
		if !errors.Is(e, mux.ErrGoAway) && c.s.Err() == nil {
			return nil, e
		}
	}
	return nil, err
}
