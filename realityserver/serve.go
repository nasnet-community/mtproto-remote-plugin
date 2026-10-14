package realityserver

import (
	"container/list"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	xr "github.com/xtls/reality"
)

// Limits for connections that are not (yet) the hub. Tests shorten them.
const (
	helloTimeout = 3 * time.Second  // the hub's hello lands within a round trip of connecting
	authTimeout  = 10 * time.Second // a whole hello's wait for REALITY's verdict and the client's last flight
	fallbackLife = 10 * time.Minute // well past any site's idle timeout, so probes see the site's own
	maxFallback  = 256
	perSource    = 32
)

// Server accepts REALITY connections on a TCP listener. The config can be
// swapped live (a new disguise domain) without dropping established conns.
type Server struct {
	ln    net.Listener
	cfg   atomic.Pointer[xr.Config]
	sc    atomic.Pointer[ServerConfig]
	maxHS int
	wg    sync.WaitGroup

	helloTimeout time.Duration
	authTimeout  time.Duration
	fallbackLife time.Duration
	maxFallback  int
	perSource    int

	mu        sync.Mutex
	live      map[*conn]struct{} // accepted, not yet handed to the caller
	inflight  int                // of those, still waiting for their first record
	waiting   *list.List         // the inflight ones, oldest first
	deciding  int                // past it with a whole ClientHello, REALITY not done with them
	fallbacks int                // handed to the cover site: mostly probes
	sources   map[netip.Addr]int
}

// New wraps an already-bound TCP listener. maxHandshakes bounds the
// handshakes in flight: each one dials Dest, so an unbounded flood would turn
// the agent into a dialer against the borrowed site.
func New(ln net.Listener, c ServerConfig, maxHandshakes int) *Server {
	s := &Server{
		ln: ln, maxHS: maxHandshakes,
		helloTimeout: helloTimeout, authTimeout: authTimeout, fallbackLife: fallbackLife, maxFallback: maxFallback, perSource: perSource,
		live: map[*conn]struct{}{}, waiting: list.New(), sources: map[netip.Addr]int{},
	}
	s.Reconfigure(c)
	return s
}

// Reconfigure installs c for every connection accepted from now on.
func (s *Server) Reconfigure(c ServerConfig) {
	cfg := c.build()
	dial := cfg.DialContext
	cfg.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		t, err := dial(ctx, network, addr)
		if c, ok := ctx.Value(connKey{}).(*conn); ok && err == nil {
			return &target{Conn: t, c: c}, nil
		}
		return t, err
	}
	go xr.DetectPostHandshakeRecordsLens(cfg)
	s.cfg.Store(cfg)
	s.sc.Store(&c)
}

// Warm reports whether Dest's post-handshake record sizes are sampled for every
// SNI/ALPN combination. Until then the first connection's first bytes lag ~5 s.
func (s *Server) Warm() bool {
	c := s.sc.Load()
	for _, sni := range c.ServerNames {
		for alpn := 0; alpn < 3; alpn++ {
			v, ok := xr.GlobalPostHandshakeRecordsLens.Load(c.Dest + " " + sni + " " + strconv.Itoa(alpn))
			if !ok {
				return false
			}
			if _, pending := v.(bool); pending {
				return false
			}
		}
	}
	return true
}

type connKey struct{}

// Serve calls handle for every authenticated connection until the listener closes.
func (s *Server) Serve(handle func(net.Conn)) error {
	var backoff time.Duration
	for {
		raw, err := s.ln.Accept()
		if err != nil {
			// out of fds and the like: wait it out rather than take the agent down
			var ne net.Error
			if errors.As(err, &ne) && ne.Temporary() && !errors.Is(err, net.ErrClosed) {
				backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
				time.Sleep(backoff)
				continue
			}
			s.dropAll()
			s.wg.Wait()
			return err
		}
		backoff = 0
		c, evicted := s.admit(raw)
		if evicted != nil {
			evicted.Conn.Close()
		}
		if c == nil {
			raw.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				if recover() != nil {
					s.release(c)
					c.Conn.Close()
				}
			}()
			// the cover-site dial carries c, so REALITY's verdict on it is visible
			rc, err := xr.Server(context.WithValue(context.Background(), connKey{}, c), c, s.cfg.Load())
			s.release(c)
			if err != nil {
				return // probe or bad auth: already forwarded to Dest and closed
			}
			handle(rc)
		}()
	}
}

func (s *Server) Close() error { return s.ln.Close() }

// admit books a fresh connection, or returns nil when its source is over the
// cap. With every slot taken the oldest silent one makes room (evicted, for the
// caller to close): refusing the newest would refuse the hub as well.
func (s *Server) admit(raw net.Conn) (c, evicted *conn) {
	c = &conn{Conn: raw, srv: s, src: sourceOf(raw.RemoteAddr())}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.src.IsValid() && s.sources[c.src] >= s.perSource {
		return nil, nil
	}
	if s.inflight >= s.maxHS {
		front := s.waiting.Front()
		if front == nil {
			return nil, nil
		}
		evicted = front.Value.(*conn)
		s.releaseLocked(evicted)
	}
	s.inflight++
	c.elem = s.waiting.PushBack(c)
	if c.src.IsValid() {
		s.sources[c.src]++
	}
	s.live[c] = struct{}{}
	_ = raw.SetReadDeadline(time.Now().Add(s.helloTimeout))
	return c, evicted
}

// helloDone frees the handshake slot once REALITY has its first record. A
// whole ClientHello waits for REALITY's verdict out of reach of cover-site
// pressure (the hub's sits there for seconds while Dest is sampled); anything
// else goes straight to the cover-site pool.
func (s *Server) helloDone(c *conn, whole bool) {
	var evict *conn
	s.mu.Lock()
	if c.state != stateHandshake {
		s.mu.Unlock()
		return
	}
	s.unbookLocked(c)
	if whole {
		evict = s.decideLocked(c)
	} else {
		evict = s.poolLocked(c)
	}
	s.mu.Unlock()
	if evict != nil {
		evict.Conn.Close()
	}
}

// fellBack moves c to the cover-site pool once REALITY hands it to Dest.
func (s *Server) fellBack(c *conn) {
	s.mu.Lock()
	if c.state != stateDeciding {
		s.mu.Unlock()
		return
	}
	s.unbookLocked(c)
	evict := s.poolLocked(c)
	s.mu.Unlock()
	if evict != nil {
		evict.Conn.Close()
	}
}

// decideLocked parks c until REALITY rules on it. Probes leave at once, so
// only authenticated handshakes (the hub, or a replay of its hello) pile up.
func (s *Server) decideLocked(c *conn) (evict *conn) {
	if s.deciding >= s.maxHS {
		if evict = s.oldestLocked(stateDeciding); evict != nil {
			s.releaseLocked(evict)
		}
	}
	c.state, c.since = stateDeciding, time.Now()
	s.deciding++
	_ = c.Conn.SetReadDeadline(time.Now().Add(s.authTimeout))
	return evict
}

// poolLocked puts c in the cover-site pool; a full pool drops its oldest rather
// than refuse, since this one may well be the hub.
func (s *Server) poolLocked(c *conn) (evict *conn) {
	_ = c.Conn.SetReadDeadline(time.Time{})
	if s.fallbacks >= s.maxFallback {
		if evict = s.oldestLocked(stateFallback); evict != nil {
			s.releaseLocked(evict)
		}
	}
	c.state, c.since = stateFallback, time.Now()
	s.fallbacks++
	c.timer = time.AfterFunc(s.fallbackLife, func() { s.release(c); c.Conn.Close() })
	return evict
}

func (s *Server) oldestLocked(state int) *conn {
	var o *conn
	for x := range s.live {
		if x.state == state && (o == nil || x.since.Before(o.since)) {
			o = x
		}
	}
	return o
}

// unbookLocked takes c out of the count its state is in; state itself is left to the caller.
func (s *Server) unbookLocked(c *conn) bool {
	switch c.state {
	case stateHandshake:
		s.inflight--
		s.waiting.Remove(c.elem)
	case stateDeciding:
		s.deciding--
	case stateFallback:
		s.fallbacks--
		c.timer.Stop()
	default:
		return false
	}
	return true
}

// release takes c off the books; it is safe to call more than once.
func (s *Server) release(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseLocked(c)
}

func (s *Server) releaseLocked(c *conn) {
	if !s.unbookLocked(c) {
		return
	}
	if c.state != stateFallback {
		_ = c.Conn.SetReadDeadline(time.Time{})
	}
	c.state = stateDone
	c.decided.Store(true)
	delete(s.live, c)
	if c.src.IsValid() {
		if s.sources[c.src]--; s.sources[c.src] <= 0 {
			delete(s.sources, c.src)
		}
	}
}

// dropAll hangs up on everything not yet handed out, so shutdown does not wait for probes.
func (s *Server) dropAll() {
	s.mu.Lock()
	all := make([]*conn, 0, len(s.live))
	for c := range s.live {
		all = append(all, c)
	}
	s.mu.Unlock()
	for _, c := range all {
		s.release(c)
		c.Conn.Close()
	}
}

// sourceOf keys the per-source cap; a v6 host usually owns its whole /64.
func sourceOf(a net.Addr) netip.Addr {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return netip.Addr{}
	}
	ip := ta.AddrPort().Addr().Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.Addr()
	}
	return ip
}

const (
	stateHandshake = iota
	stateDeciding
	stateFallback
	stateDone
)

// conn watches the first TLS record go by so the slot it holds can be freed.
type conn struct {
	net.Conn
	srv *Server
	src netip.Addr

	decided atomic.Bool // the first record is in (or the conn is gone)
	hdr     [9]byte     // record header, then the handshake message header
	seen    int

	// guarded by srv.mu
	state int
	since time.Time
	timer *time.Timer
	elem  *list.Element // in srv.waiting while stateHandshake
}

func (c *conn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && !c.decided.Load() {
		c.watch(b[:n])
	}
	return n, err
}

func (c *conn) watch(b []byte) {
	if c.seen < len(c.hdr) {
		copy(c.hdr[c.seen:], b)
	}
	c.seen += len(b)
	if c.seen < 5 {
		return
	}
	rec := int(binary.BigEndian.Uint16(c.hdr[3:5]))
	// anything but a handshake record goes straight to the cover site
	if c.hdr[0] != 22 {
		c.decided.Store(true)
		c.srv.helloDone(c, false)
		return
	}
	if c.seen < 5+rec {
		return
	}
	// the hub's ClientHello fits one record; one that spills over is not the hub
	whole := rec >= 4 && c.hdr[5] == 1 && 4+(int(c.hdr[6])<<16|int(c.hdr[7])<<8|int(c.hdr[8])) <= rec
	c.decided.Store(true)
	c.srv.helloDone(c, whole)
}

// CloseWrite is what REALITY uses to pass the cover site's FIN on to a probe.
func (c *conn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

// target is REALITY's connection to the cover site. REALITY copies the client
// into it only once it has decided the client is not ours.
type target struct {
	net.Conn
	c *conn
}

func (t *target) ReadFrom(r io.Reader) (int64, error) {
	t.c.srv.fellBack(t.c)
	return io.Copy(t.Conn, r)
}

func (t *target) CloseWrite() error {
	if cw, ok := t.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}
