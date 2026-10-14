package link

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/mux"
	"github.com/nasnet-community/mtproto-remote-plugin/reality"
	"github.com/nasnet-community/mtproto-remote-plugin/realityserver"
	"github.com/nasnet-community/mtproto-remote-plugin/tgnet"
)

type rig struct {
	agent  *Agent
	hub    *Hub
	allow  *tgnet.Allowlist
	dcAddr string
	token  []byte
	events chan Event

	addr string // the agent's listener
	pub  []byte
	sid  [8]byte
	dest string // the cover site

	mu   sync.Mutex
	raws []net.Conn // every raw conn the hub dialed, so tests can cut them

	cancel context.CancelFunc
}

type rigOpts struct {
	quietDC    bool // the fake DC reads with a tiny buffer instead of echoing (soak runs)
	hubToken   []byte
	agentMax   int
	authWait   time.Duration
	skipWarmup bool
	agentNow   func() time.Time
	hubCreds   func(c Creds) Creds   // what the hub dials with, from the agent's own
	noAllow    bool                  // the hub has no allowlist at all
	agentToks  func([]byte) [][]byte // the agent's tokens, from the hub's
	maxPending int
}

func newRig(t *testing.T, o rigOpts) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	dest := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	dest.EnableHTTP2 = true
	dest.StartTLS()
	t.Cleanup(dest.Close)

	// the fake datacentre: echoes, and prefixes nothing
	dc, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dc.Close() })
	go func() {
		for {
			c, err := dc.Accept()
			if err != nil {
				return
			}
			if o.quietDC {
				go func() {
					var b [1]byte
					for {
						if _, err := c.Read(b[:]); err != nil {
							c.Close()
							return
						}
					}
				}()
				continue
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	dcPort := dc.Addr().(*net.TCPAddr).Port

	priv, pub, _ := reality.GenerateKey()
	sid := [8]byte{4, 2}
	token := make([]byte, 32)
	rand.Read(token)
	hubToken := token
	if o.hubToken != nil {
		hubToken = o.hubToken
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	toks := [][]byte{token}
	if o.agentToks != nil {
		toks = o.agentToks(token)
	}
	agent := NewAgent(AgentConfig{
		AgentID:  "agent-1",
		Tokens:   toks,
		Listener: ln,
		Reality: realityserver.ServerConfig{
			Dest: dest.Listener.Addr().String(), ServerNames: []string{"example.com"},
			PrivateKey: priv, ShortIDs: [][8]byte{sid}, MaxTimeDiff: time.Minute,
		},
		Handlers: map[string]ctrl.Handler{
			ctrl.MStatus: func(context.Context, json.RawMessage, io.Reader) (any, error) {
				return ctrl.Status{AgentVersion: "test-1"}, nil
			},
			"boom": func(context.Context, json.RawMessage, io.Reader) (any, error) {
				return nil, &ctrl.Error{Code: "nope", Msg: "refused"}
			},
			"sum": func(_ context.Context, _ json.RawMessage, raw io.Reader) (any, error) {
				n, err := io.Copy(io.Discard, raw)
				return map[string]int64{"bytes": n}, err
			},
		},
		MaxStreams:  o.agentMax,
		MaxPending:  o.maxPending,
		AuthTimeout: o.authWait,
		Now:         o.agentNow,
	})
	go agent.Serve(ctx)

	allow := tgnet.NewWithPorts([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, uint16(dcPort))
	r := &rig{agent: agent, allow: allow, dcAddr: dc.Addr().String(), token: token, events: make(chan Event, 256), cancel: cancel,
		addr: ln.Addr().String(), pub: pub, sid: sid, dest: dest.Listener.Addr().String()}
	creds := Creds{Token: hubToken, Reality: reality.ClientConfig{ServerName: "example.com", PublicKey: pub, ShortID: sid}}
	if o.hubCreds != nil {
		creds = o.hubCreds(creds)
	}
	hubAllow := allow
	if o.noAllow {
		hubAllow = nil
	}
	var d net.Dialer
	dial := func(ctx context.Context, n, a string) (net.Conn, error) {
		c, err := d.DialContext(ctx, n, a)
		if err == nil {
			r.mu.Lock()
			r.raws = append(r.raws, c)
			r.mu.Unlock()
		}
		return c, err
	}
	r.hub = NewHub(HubConfig{
		AgentID: "agent-1", Token: creds.Token, Addr: ln.Addr().String(),
		Reality: creds.Reality,
		Conns:   []ConnSpec{{Name: "wan1", Dial: dial}, {Name: "wan2", Dial: dial}},
		Allow:   hubAllow,
		DialDC:  d.DialContext,
		OnEvent: func(e Event) {
			select {
			case r.events <- e:
			default:
			}
		},
	})
	if creds.Next != nil {
		r.hub.SetCreds(creds)
	}
	if !o.skipWarmup {
		// REALITY samples the borrowed site for ~5 s after start
		for i := 0; !agent.Warm() && i < 300; i++ {
			time.Sleep(50 * time.Millisecond)
		}
	}
	go r.hub.Run(ctx)
	return r
}

func (r *rig) waitConns(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if r.hub.Status().Up >= n && r.agent.Conns() >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d connections: hub=%+v agent conns=%d", n, r.hub.Status(), r.agent.Conns())
}

func echoOnce(t *testing.T, st *mux.Stream, msg string) {
	t.Helper()
	if _, err := st.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	st.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != msg {
		t.Fatalf("echo = %q err=%v, want %q", buf, err, msg)
	}
}

func TestLink(t *testing.T) {
	r := newRig(t, rigOpts{agentMax: 6})
	r.waitConns(t, 2)

	t.Run("a stream reaches the datacentre and back, first bytes included", func(t *testing.T) {
		st, err := r.agent.OpenData(r.dcAddr, []byte("hello"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		buf := make([]byte, 5)
		st.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "hello" {
			t.Fatalf("first bytes: %q %v", buf, err)
		}
		echoOnce(t, st, "second message")
	})

	t.Run("half-close travels end to end", func(t *testing.T) {
		st, _ := r.agent.OpenData(r.dcAddr, nil)
		st.Write([]byte("last words"))
		st.CloseWrite() // FIN -> hub -> CloseWrite on the DC conn -> echo ends -> FIN back
		got, err := io.ReadAll(st)
		if err != nil || string(got) != "last words" {
			t.Fatalf("got %q err %v", got, err)
		}
		st.Close()
	})

	t.Run("large transfer arrives intact", func(t *testing.T) {
		data := make([]byte, 3<<20)
		rand.Read(data)
		st, _ := r.agent.OpenData(r.dcAddr, nil)
		go func() { st.Write(data); st.CloseWrite() }()
		got, err := io.ReadAll(st)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("got %d bytes err %v", len(got), err)
		}
		st.Close()
	})

	t.Run("destinations outside the allowlist are refused and counted", func(t *testing.T) {
		before := r.allow.ViolationsTotal()
		for _, dst := range []string{"10.1.2.3:443", "127.0.0.1:22", "not-an-address"} {
			st, err := r.agent.OpenData(dst, nil)
			if err != nil {
				t.Fatal(err)
			}
			st.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := st.Read(make([]byte, 1)); !errors.Is(err, mux.ErrStreamReset) {
				t.Errorf("%s: read = %v, want a reset", dst, err)
			}
		}
		if got := r.allow.ViolationsTotal() - before; got != 3 {
			t.Errorf("violations = %d, want 3", got)
		}
		seen := 0
		for len(r.events) > 0 {
			if (<-r.events).Kind == EventViolation {
				seen++
			}
		}
		if seen < 3 {
			t.Errorf("only %d violation events", seen)
		}
	})

	t.Run("an unreachable datacentre resets the stream", func(t *testing.T) {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		dead := l.Addr().String()
		l.Close()
		a := tgnet.NewWithPorts([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, uint16(l.Addr().(*net.TCPAddr).Port))
		r.hub.cfg.Allow = a // the hub is idle here, so swapping is safe in this test
		defer func() { r.hub.cfg.Allow = r.allow }()
		st, _ := r.agent.OpenData(dead, nil)
		st.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := st.Read(make([]byte, 1)); !errors.Is(err, mux.ErrStreamReset) {
			t.Fatalf("read = %v, want a reset", err)
		}
		if r.hub.Status().DialFailures == 0 {
			t.Error("dial failure not counted")
		}
	})

	t.Run("control calls round-trip, errors keep their code, raw bodies stream", func(t *testing.T) {
		var s ctrl.Status
		if err := r.hub.Call(context.Background(), ctrl.MStatus, nil, &s); err != nil || s.AgentVersion != "test-1" {
			t.Fatalf("status: %+v %v", s, err)
		}
		err := r.hub.Call(context.Background(), "boom", nil, nil)
		var ce *ctrl.Error
		if !errors.As(err, &ce) || ce.Code != "nope" {
			t.Fatalf("boom: %v", err)
		}
		if err := r.hub.Call(context.Background(), "missing", nil, nil); err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("unknown method: %v", err)
		}
		var sum map[string]int64
		blob := bytes.Repeat([]byte("x"), 2<<20)
		if err := r.hub.CallRaw(context.Background(), "sum", nil, bytes.NewReader(blob), &sum); err != nil || sum["bytes"] != int64(len(blob)) {
			t.Fatalf("raw body: %v %v", sum, err)
		}
	})

	t.Run("the agent refuses streams past its cap", func(t *testing.T) {
		var held []*mux.Stream
		for i := 0; ; i++ {
			st, err := r.agent.OpenData(r.dcAddr, nil)
			if errors.Is(err, ErrBusy) {
				break
			}
			if err != nil || i > 20 {
				t.Fatalf("open %d: %v", i, err)
			}
			held = append(held, st)
		}
		if len(held) == 0 || len(held) > 6 {
			t.Fatalf("held %d streams with a cap of 6", len(held))
		}
		for _, st := range held {
			st.Close()
		}
		waitUntil(t, func() bool { return r.agent.Streams() == 0 }, "streams to drain")
	})

	t.Run("a cut connection is redialled and traffic keeps flowing", func(t *testing.T) {
		r.mu.Lock()
		victim := r.raws[0]
		r.mu.Unlock()
		victim.Close()
		waitUntil(t, func() bool { return r.hub.Status().Up == 1 && r.agent.Conns() == 1 }, "both ends to notice the cut")
		// the other connection serves meanwhile
		st, err := r.agent.OpenData(r.dcAddr, nil)
		if err != nil {
			t.Fatalf("no stream during the outage: %v", err)
		}
		echoOnce(t, st, "survivor")
		st.Close()
		r.waitConns(t, 2)
	})
}

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWrongTokenNeverComesUp(t *testing.T) {
	bad := make([]byte, 32)
	rand.Read(bad)
	r := newRig(t, rigOpts{hubToken: bad, authWait: time.Second})
	time.Sleep(3 * time.Second)
	if r.hub.Status().Up != 0 || r.agent.Conns() != 0 {
		t.Fatalf("a hub with the wrong token got in: hub=%+v conns=%d", r.hub.Status(), r.agent.Conns())
	}
	if _, err := r.agent.OpenData(r.dcAddr, nil); !errors.Is(err, ErrNoLink) {
		t.Fatalf("OpenData with no authed hub: %v", err)
	}
	for _, c := range r.hub.Status().Conns {
		if !strings.Contains(c.LastErr, "token") {
			t.Errorf("conn %s: last error %q does not name the token", c.Name, c.LastErr)
		}
	}
}

func TestPipeHalfCloseAndAbort(t *testing.T) {
	// TCP <-> TCP through Pipe: EOF on one side becomes FIN on the other
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { b, _ := io.ReadAll(c); c.Write(bytes.ToUpper(b)); c.Close() }()
		}
	}()
	a1, a2 := tcpPair(t)
	b, _ := net.Dial("tcp", l.Addr().String())
	go Pipe(a2, b)
	a1.Write([]byte("shout"))
	a1.(*net.TCPConn).CloseWrite()
	got, err := io.ReadAll(a1)
	if err != nil || string(got) != "SHOUT" {
		t.Fatalf("got %q err %v", got, err)
	}

	// an error on one side resets the other instead of closing it cleanly
	x1, x2 := tcpPair(t)
	y1, y2 := tcpPair(t)
	done := make(chan struct{})
	go func() { Pipe(x2, y1); close(done) }()
	x1.Write([]byte("partial"))
	buf := make([]byte, 7)
	y2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(y2, buf); err != nil {
		t.Fatal(err)
	}
	x1.(*net.TCPConn).SetLinger(0)
	x1.Close() // RST towards Pipe
	y2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := y2.Read(buf); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("the far side saw %v, want a reset", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Pipe did not return after the abort")
	}
}

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); ch <- c }()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return a, <-ch
}

// Data already queued when we start waiting must wake us at once. Go's poller is
// edge-triggered, so a naive "return false and wait" sleeps through it.
func TestWaitReadableSeesAlreadyQueuedData(t *testing.T) {
	a, b := tcpPair(t)
	defer a.Close()
	defer b.Close()
	a.Write([]byte("queued before anyone waits"))
	time.Sleep(50 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- waitReadable(b) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitReadable slept through queued data")
	}
	// and it still parks when there is nothing to read
	b.Read(make([]byte, 64))
	parked := make(chan struct{})
	go func() { waitReadable(b); close(parked) }()
	select {
	case <-parked:
		t.Fatal("waitReadable returned with nothing to read")
	case <-time.After(200 * time.Millisecond):
	}
	a.Write([]byte("x"))
	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("waitReadable missed new data")
	}
	// EOF counts as readable
	a.Close()
	b.Read(make([]byte, 1))
	if err := waitReadable(b); err != nil {
		t.Fatal(err)
	}
}

// Rotating keys or changing the disguise domain swaps credentials on a live hub:
// connections made afterwards use them.
func TestSetCredsLetsAHubWithStaleCredentialsIn(t *testing.T) {
	bad := make([]byte, 32)
	rand.Read(bad)
	r := newRig(t, rigOpts{hubToken: bad, authWait: time.Second})
	time.Sleep(1500 * time.Millisecond)
	if r.hub.Status().Up != 0 {
		t.Fatal("came up with the wrong token")
	}
	cur := r.hub.Creds()
	cur.Token = r.token
	r.hub.SetCreds(cur)
	r.waitConns(t, 2)
	st, err := r.agent.OpenData(r.dcAddr, []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	st.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "hi" {
		t.Fatalf("%q %v", buf, err)
	}
}

// Close drops connections that are already up, not just the listener, so a
// stopped agent really does go away from the hub's point of view.
func TestAgentCloseDropsEstablishedLinks(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.waitConns(t, 2)
	r.agent.Close()
	waitUntil(t, func() bool { return r.hub.Status().Up == 0 }, "the hub to notice the agent is gone")
	if r.agent.Conns() != 0 {
		t.Fatalf("agent still counts %d connections", r.agent.Conns())
	}
}

// A skewed agent looks like a dead one once REALITY gives up on it, so the hub
// measures the offset on every hello while it still can.
func TestHubMeasuresTheAgentsClockSkew(t *testing.T) {
	r := newRig(t, rigOpts{agentNow: func() time.Time { return time.Now().Add(90 * time.Second) }})
	r.waitConns(t, 2)
	for _, c := range r.hub.Status().Conns {
		if d := c.Skew - 90*time.Second; d < -2*time.Second || d > 2*time.Second {
			t.Errorf("%s: skew = %v, want about 90s", c.Name, c.Skew)
		}
	}

	ok := newRig(t, rigOpts{})
	ok.waitConns(t, 2)
	for _, c := range ok.hub.Status().Conns {
		if c.Skew < -2*time.Second || c.Skew > 2*time.Second {
			t.Errorf("%s: skew = %v with matching clocks", c.Name, c.Skew)
		}
	}
}
