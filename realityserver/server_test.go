package realityserver

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/reality"
	xr "github.com/xtls/reality"
)

type rig struct {
	srv    *Server
	dest   *httptest.Server
	addr   string
	pub    []byte
	sid    [8]byte
	served chan error
}

func newRig(t *testing.T) *rig { return newRigWith(t, 64, nil, nil) }

// newRigWith lets a test shrink the limits, or wrap the listener, before Serve starts.
func newRigWith(t *testing.T, maxHS int, tune func(*Server), wrap func(net.Listener) net.Listener) *rig {
	t.Helper()
	dest := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("hello from dest"))
	}))
	dest.EnableHTTP2 = true
	dest.StartTLS()
	t.Cleanup(dest.Close)

	priv, pub, err := reality.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sid := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := inner
	if wrap != nil {
		ln = wrap(inner)
	}
	srv := New(ln, ServerConfig{
		Dest:        dest.Listener.Addr().String(),
		ServerNames: []string{"example.com"}, // httptest's certificate is for example.com
		PrivateKey:  priv,
		ShortIDs:    [][8]byte{sid},
		MaxTimeDiff: time.Minute,
	}, maxHS)
	if tune != nil {
		tune(srv)
	}
	t.Cleanup(func() { srv.Close() })
	served := make(chan error, 1)
	go func() { served <- srv.Serve(func(c net.Conn) { io.Copy(c, c); c.Close() }) }()
	return &rig{srv: srv, dest: dest, addr: inner.Addr().String(), pub: pub, sid: sid, served: served}
}

// probe opens a real TLS session with the cover site through the server and leaves it idle.
func (r *rig) probe(t *testing.T) *tls.Conn {
	t.Helper()
	raw, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(r.dest.Certificate())
	c := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: "example.com"})
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := c.Handshake(); err != nil {
		raw.Close()
		t.Fatalf("probe handshake: %v", err)
	}
	c.SetDeadline(time.Time{})
	t.Cleanup(func() { c.Close() })
	return c
}

// get sends one HTTP/1.1 request over an open probe and reports whether a response came back.
func get(c net.Conn) error {
	c.SetDeadline(time.Now().Add(3 * time.Second))
	defer c.SetDeadline(time.Time{})
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// closedSoon reports whether the server hangs up on c within d (a timeout means it is still open).
func closedSoon(c net.Conn, d time.Duration) bool {
	c.SetReadDeadline(time.Now().Add(d))
	defer c.SetReadDeadline(time.Time{})
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	return err != nil && !(errors.As(err, &ne) && ne.Timeout())
}

func (r *rig) dial(t *testing.T, cfg reality.ClientConfig) error {
	raw, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u, err := reality.Client(ctx, raw, cfg)
	if err != nil {
		raw.Close()
		return err
	}
	defer u.Close()
	msg := []byte("ping over reality")
	if _, err := u.Write(msg); err != nil {
		return err
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(u, buf); err != nil {
		return err
	}
	if string(buf) != string(msg) {
		return errors.New("echo mismatch")
	}
	return nil
}

func TestAuthedRoundTrip(t *testing.T) {
	r := newRig(t)
	if err := r.dial(t, reality.ClientConfig{ServerName: "example.com", PublicKey: r.pub, ShortID: r.sid}); err != nil {
		t.Fatal(err)
	}
}

func TestWrongCredentialsNeverVerify(t *testing.T) {
	r := newRig(t)
	_, otherPub, _ := reality.GenerateKey()
	cases := map[string]reality.ClientConfig{
		"wrong shortid": {ServerName: "example.com", PublicKey: r.pub, ShortID: [8]byte{9}},
		"wrong key":     {ServerName: "example.com", PublicKey: otherPub, ShortID: r.sid},
		"wrong sni":     {ServerName: "other.test", PublicKey: r.pub, ShortID: r.sid},
	}
	for name, cfg := range cases {
		if err := r.dial(t, cfg); !errors.Is(err, reality.ErrNotVerified) {
			t.Errorf("%s: got %v, want ErrNotVerified", name, err)
		}
	}
}

// A stock TLS client (a probe) must see the borrowed site, byte for byte.
func TestProbeSeesTheRealSite(t *testing.T) {
	r := newRig(t)
	pool := x509.NewCertPool()
	pool.AddCert(r.dest.Certificate())
	hc := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com"},
			DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, n, r.addr)
			},
			ForceAttemptHTTP2: true,
		},
	}
	resp, err := hc.Get("https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello from dest" {
		t.Fatalf("probe got %q", body)
	}
}

// The first connection after start lags while REALITY samples the borrowed site;
// Warm tells callers when that is over.
func TestWarmFlagFlipsAndReconfigureKeepsOldConns(t *testing.T) {
	r := newRig(t)
	if r.srv.Warm() {
		t.Fatal("warm immediately: sampling should still be running")
	}
	raw, _ := net.Dial("tcp", r.addr)
	u, err := reality.Client(context.Background(), raw, reality.ClientConfig{ServerName: "example.com", PublicKey: r.pub, ShortID: r.sid})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	u.Write([]byte("a"))
	io.ReadFull(u, make([]byte, 1))
	t.Logf("first echo after %v", time.Since(t0).Round(time.Millisecond))
	deadline := time.Now().Add(10 * time.Second)
	for !r.srv.Warm() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !r.srv.Warm() {
		t.Fatal("never warmed")
	}
	priv2, pub2, _ := reality.GenerateKey()
	r.srv.Reconfigure(ServerConfig{Dest: r.dest.Listener.Addr().String(), ServerNames: []string{"example.com"}, PrivateKey: priv2, ShortIDs: [][8]byte{r.sid}, MaxTimeDiff: time.Minute})
	u.Write([]byte("b"))
	b := make([]byte, 1)
	if _, err := io.ReadFull(u, b); err != nil || b[0] != 'b' {
		t.Fatalf("established conn broke after Reconfigure: %v", err)
	}
	if err := r.dial(t, reality.ClientConfig{ServerName: "example.com", PublicKey: r.pub, ShortID: r.sid}); !errors.Is(err, reality.ErrNotVerified) {
		t.Fatalf("old key after swap: %v", err)
	}
	// the new key works once the new config's sampling is done
	if err := r.dial(t, reality.ClientConfig{ServerName: "example.com", PublicKey: pub2, ShortID: r.sid}); err != nil {
		t.Fatalf("new key rejected: %v", err)
	}
}

func TestHandshakeFloodIsCapped(t *testing.T) {
	dest := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dest.EnableHTTP2 = true
	dest.StartTLS()
	defer dest.Close()
	priv, _, _ := reality.GenerateKey()
	inner, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := New(inner, ServerConfig{Dest: dest.Listener.Addr().String(), ServerNames: []string{"example.com"}, PrivateKey: priv, ShortIDs: [][8]byte{{1}}}, 3)
	defer srv.Close()
	go srv.Serve(func(c net.Conn) { c.Close() })
	// silent connections hold a handshake slot until they time out; past the cap the server hangs up
	var conns []net.Conn
	for i := 0; i < 8; i++ {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	time.Sleep(300 * time.Millisecond)
	dropped := 0
	for _, c := range conns {
		c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); err == io.EOF {
			dropped++
		}
		c.Close()
	}
	if dropped < 4 {
		t.Fatalf("only %d of 8 over-cap connections were dropped, want at least 4", dropped)
	}
}

// Idle probes used to keep their handshake slots and lock the hub out.
func TestIdleProbesDoNotHoldHandshakeSlots(t *testing.T) {
	r := newRigWith(t, 2, nil, nil)
	for i := 0; i < 3; i++ {
		r.probe(t)
	}
	if err := r.dial(t, reality.ClientConfig{ServerName: "example.com", PublicKey: r.pub, ShortID: r.sid}); err != nil {
		t.Fatalf("the hub was locked out by idle probes: %v", err)
	}
}

func TestSilentConnectionLosesItsSlot(t *testing.T) {
	r := newRigWith(t, 1, func(s *Server) { s.helloTimeout = 300 * time.Millisecond }, nil)
	silent, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	if !closedSoon(silent, 3*time.Second) {
		t.Fatal("a connection that never sent a hello is still open")
	}
	if err := r.dial(t, reality.ClientConfig{ServerName: "example.com", PublicKey: r.pub, ShortID: r.sid}); err != nil {
		t.Fatalf("the slot was not given back: %v", err)
	}
}

func TestFallbackPoolDropsTheOldest(t *testing.T) {
	r := newRigWith(t, 8, func(s *Server) { s.maxFallback = 2 }, nil)
	p1, p2, p3 := r.probe(t), r.probe(t), r.probe(t)
	if !closedSoon(p1, 3*time.Second) {
		t.Fatal("the oldest cover-site connection outlived a full pool")
	}
	for i, p := range []net.Conn{p2, p3} {
		if err := get(p); err != nil {
			t.Fatalf("probe %d lost its session: %v", i+2, err)
		}
	}
}

func TestFallbackLifetimeIsCapped(t *testing.T) {
	r := newRigWith(t, 8, func(s *Server) { s.fallbackLife = 300 * time.Millisecond }, nil)
	p := r.probe(t)
	if err := get(p); err != nil {
		t.Fatalf("a fresh probe cannot reach the site: %v", err)
	}
	if !closedSoon(p, 3*time.Second) {
		t.Fatal("a cover-site connection outlived its lifetime")
	}
}

func TestOneSourceCannotTakeEverySlot(t *testing.T) {
	r := newRigWith(t, 64, func(s *Server) { s.perSource = 2 }, nil)
	var conns []net.Conn
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	if !closedSoon(conns[2], 2*time.Second) {
		t.Fatal("a third connection from one address was let in")
	}
	for i, c := range conns[:2] {
		if closedSoon(c, 200*time.Millisecond) {
			t.Fatalf("connection %d was dropped under the cap", i+1)
		}
	}
}

// nilAddrListener's first conn has a nil RemoteAddr, which panics inside the REALITY handshake.
type nilAddrListener struct {
	net.Listener
	once sync.Once
}

type nilAddrConn struct{ *net.TCPConn }

func (nilAddrConn) RemoteAddr() net.Addr { return nil }

func (l *nilAddrListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	var out net.Conn = c
	l.once.Do(func() { out = nilAddrConn{c.(*net.TCPConn)} })
	return out, nil
}

func TestPanicGivesBackTheSlotAndTheSocket(t *testing.T) {
	r := newRigWith(t, 1, nil, func(ln net.Listener) net.Listener { return &nilAddrListener{Listener: ln} })
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !closedSoon(c, 3*time.Second) {
		t.Fatal("the connection that panicked was left open")
	}
	if err := r.dial(t, reality.ClientConfig{ServerName: "example.com", PublicKey: r.pub, ShortID: r.sid}); err != nil {
		t.Fatalf("the panicked handshake kept its slot: %v", err)
	}
}

// Shutting down must not wait for probes to finish browsing the cover site.
func TestServeReturnsWithProbesConnected(t *testing.T) {
	r := newRigWith(t, 8, nil, nil)
	r.probe(t)
	r.srv.Close()
	select {
	case <-r.served:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve is still waiting for an idle probe")
	}
}

// fakeSources gives the first n accepted conns one of k made-up source addresses.
type fakeSources struct {
	net.Listener
	n, k int
	mu   sync.Mutex
	seen int
}

type fakeAddrConn struct {
	*net.TCPConn
	addr net.Addr
}

func (c fakeAddrConn) RemoteAddr() net.Addr { return c.addr }

func (l *fakeSources) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	i := l.seen
	l.seen++
	l.mu.Unlock()
	if i >= l.n {
		return c, nil
	}
	return fakeAddrConn{c.(*net.TCPConn), &net.TCPAddr{IP: net.IPv4(10, 0, 0, byte(1+i%l.k)), Port: 40000 + i}}, nil
}

func (s *Server) counts() (inflight, fallbacks, deciding int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight, s.fallbacks, s.deciding
}

// A handful of sources holding every handshake slot with silent connections
// must not lock the hub out: the hub's hello arrives within a round trip.
func TestSilentConnectionsFromAFewSourcesCannotLockTheHubOut(t *testing.T) {
	const silent = 128
	r := newRigWith(t, 128, nil, func(ln net.Listener) net.Listener { return &fakeSources{Listener: ln, n: silent, k: 4} })
	for i := 0; i < silent; i++ {
		c, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	deadline := time.Now().Add(3 * time.Second)
	for n, _, _ := r.srv.counts(); n < silent && time.Now().Before(deadline); n, _, _ = r.srv.counts() {
		time.Sleep(10 * time.Millisecond)
	}
	if n, _, _ := r.srv.counts(); n != silent {
		t.Fatalf("%d silent connections admitted, want %d", n, silent)
	}
	if err := r.dial(t, reality.ClientConfig{ServerName: "example.com", PublicKey: r.pub, ShortID: r.sid}); err != nil {
		t.Fatalf("the hub was locked out by silent connections: %v", err)
	}
}

// The hub's handshake can sit in REALITY for seconds while the cover site's
// record sizes are sampled; probes filling the cover-site pool meanwhile must
// not evict it.
func TestFallbackPressureCannotEvictAnAuthenticatedHandshake(t *testing.T) {
	r := newRigWith(t, 8, func(s *Server) { s.maxFallback = 2 }, nil)
	for i := 0; !r.srv.Warm() && i < 200; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	// a server name whose sampling never finishes until the test says so
	const sni = "slow.example.com"
	dest := r.dest.Listener.Addr().String()
	keys := []string{dest + " " + sni + " 0", dest + " " + sni + " 1", dest + " " + sni + " 2"}
	for _, k := range keys {
		xr.GlobalPostHandshakeRecordsLens.Store(k, false)
	}
	sc := *r.srv.sc.Load()
	sc.ServerNames = append([]string{sni}, sc.ServerNames...)
	r.srv.Reconfigure(sc)

	raw, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u, err := reality.Client(ctx, raw, reality.ClientConfig{ServerName: sni, PublicKey: r.pub, ShortID: r.sid})
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	defer u.Close()
	// the server is now waiting on the sampling; fill the cover-site pool past its cap
	for i := 0; i < 3; i++ {
		r.probe(t)
	}
	if _, fb, dc := r.srv.counts(); fb != 2 || dc != 1 {
		t.Fatalf("fallbacks=%d deciding=%d, want the probes pooled and the hub set apart", fb, dc)
	}
	for _, k := range keys {
		xr.GlobalPostHandshakeRecordsLens.Store(k, []int{})
	}
	u.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := u.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(u, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("the hub's handshake was evicted by cover-site traffic: %q %v", buf, err)
	}
}

// mute passes the first write (the ClientHello) and swallows the rest, so the
// server never sees the client's last flight.
type mute struct {
	net.Conn
	once sync.Once
}

func (m *mute) Write(b []byte) (int, error) {
	pass := false
	m.once.Do(func() { pass = true })
	if pass {
		return m.Conn.Write(b)
	}
	return len(b), nil
}

// Authenticated hellos that never finish are bounded: the oldest makes room,
// and each one only waits authTimeout.
func TestStalledAuthenticatedHandshakesAreBounded(t *testing.T) {
	r := newRigWith(t, 1, func(s *Server) { s.authTimeout = 2 * time.Second }, nil)
	for i := 0; !r.srv.Warm() && i < 200; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	stalled := func() net.Conn {
		t.Helper()
		raw, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { raw.Close() })
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := reality.Client(ctx, &mute{Conn: raw}, reality.ClientConfig{ServerName: "example.com", PublicKey: r.pub, ShortID: r.sid}); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	a := stalled()
	if _, _, dc := r.srv.counts(); dc != 1 {
		t.Fatalf("deciding=%d after one stalled handshake", dc)
	}
	b := stalled()
	if !closedSoon(a, time.Second) {
		t.Fatal("the oldest stalled handshake kept its place")
	}
	if !closedSoon(b, 4*time.Second) {
		t.Fatal("a stalled handshake outlived authTimeout")
	}
	deadline := time.Now().Add(2 * time.Second)
	for in, fb, dc := r.srv.counts(); in+fb+dc != 0; in, fb, dc = r.srv.counts() {
		if time.Now().After(deadline) {
			t.Fatalf("books not empty: inflight=%d fallbacks=%d deciding=%d", in, fb, dc)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// failNTimes fails Accept with a temporary error a few times, like EMFILE.
type failNTimes struct {
	net.Listener
	mu sync.Mutex
	n  int
}

type tempErr struct{}

func (tempErr) Error() string   { return "accept: too many open files" }
func (tempErr) Timeout() bool   { return false }
func (tempErr) Temporary() bool { return true }

func (l *failNTimes) Accept() (net.Conn, error) {
	l.mu.Lock()
	fail := l.n > 0
	l.n--
	l.mu.Unlock()
	if fail {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: tempErr{}}
	}
	return l.Listener.Accept()
}

// Running out of fds for a moment must not end Serve (the daemon would exit).
func TestTemporaryAcceptErrorsDoNotEndServe(t *testing.T) {
	r := newRigWith(t, 8, nil, func(ln net.Listener) net.Listener { return &failNTimes{Listener: ln, n: 5} })
	if err := r.dial(t, reality.ClientConfig{ServerName: "example.com", PublicKey: r.pub, ShortID: r.sid}); err != nil {
		t.Fatalf("dial after transient accept errors: %v", err)
	}
	select {
	case err := <-r.served:
		t.Fatalf("Serve returned: %v", err)
	default:
	}
}
