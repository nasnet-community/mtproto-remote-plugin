package link

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/mux"
	"github.com/nasnet-community/mtproto-remote-plugin/reality"
	"github.com/nasnet-community/mtproto-remote-plugin/realityserver"
)

func randomToken() []byte {
	b := make([]byte, 32)
	rand.Read(b)
	return b
}

// While a rotation settles the agent holds two tokens; a hub with either one
// gets in, whichever the agent lists first.
func TestAHubWithEitherOfTheAgentsTokensGetsIn(t *testing.T) {
	r := newRig(t, rigOpts{agentToks: func(hub []byte) [][]byte { return [][]byte{randomToken(), hub} }})
	r.waitConns(t, 2)
	r2 := newRig(t, rigOpts{agentToks: func(hub []byte) [][]byte { return [][]byte{hub, randomToken()} }})
	r2.waitConns(t, 2)
}

// A rotation whose reply was lost leaves the hub unsure which set the agent
// holds: it dials the candidate when the current set does not get in.
func TestTheHubFallsBackToItsCandidateCredentials(t *testing.T) {
	_, wrongPub, _ := reality.GenerateKey()
	r := newRig(t, rigOpts{authWait: time.Second, hubCreds: func(c Creds) Creds {
		real := c
		c.Token = randomToken()
		c.Reality.PublicKey = wrongPub
		c.Next = &real
		return c
	}})
	r.waitConns(t, 2)
	for _, c := range r.hub.Status().Conns {
		if !c.Alternate {
			t.Errorf("%s came up without the candidate set", c.Name)
		}
	}
	echo(t, r)

	// once the panel settles on the candidate, new connections need no fallback
	cur := r.hub.Creds()
	r.hub.SetCreds(*cur.Next)
	r.cut()
	r.waitConns(t, 2)
	waitUntil(t, func() bool {
		for _, c := range r.hub.Status().Conns {
			if c.Alternate || !c.Up {
				return false
			}
		}
		return true
	}, "connections on the settled set")
}

// A cover-site change whose reply was lost moves the agent's SNI without the
// hub hearing back: the name it expects is dialled too.
func TestAnExpectedCoverSiteIsTriedWhenTheCurrentOneFails(t *testing.T) {
	r := newRig(t, rigOpts{hubCreds: func(c Creds) Creds { c.Reality.ServerName = "old.example.com"; return c }})
	time.Sleep(1500 * time.Millisecond)
	if r.hub.Status().Up != 0 {
		t.Fatal("came up with a cover site the agent does not serve")
	}
	r.hub.ExpectServerName("Example.COM")
	r.waitConns(t, 2)
	echo(t, r)
}

func TestNoCandidateMeansNoFallback(t *testing.T) {
	_, wrongPub, _ := reality.GenerateKey()
	r := newRig(t, rigOpts{hubCreds: func(c Creds) Creds { c.Reality.PublicKey = wrongPub; return c }})
	r.hub.ExpectServerName("example.com") // the name it already has: nothing new to try
	time.Sleep(2 * time.Second)
	if r.hub.Status().Up != 0 {
		t.Fatal("a hub with the wrong key got in")
	}
}

// A hub without an allowlist refuses every stream instead of crashing.
func TestAHubWithoutAnAllowlistResetsStreams(t *testing.T) {
	r := newRig(t, rigOpts{noAllow: true})
	r.waitConns(t, 2)
	st, err := r.agent.OpenData(r.dcAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	st.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := st.Read(make([]byte, 1)); !errors.Is(err, mux.ErrStreamReset) {
		t.Fatalf("read = %v, want a reset", err)
	}
	if r.hub.Status().Up == 0 {
		t.Fatal("the hub went down")
	}
}

// rawSession gets past REALITY and the mux but never says hello.
func rawSession(t *testing.T, r *rig) *mux.Session {
	t.Helper()
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tc, err := reality.Client(ctx, c, reality.ClientConfig{ServerName: "example.com", PublicKey: r.pub, ShortID: r.sid})
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	sess, err := mux.Client(tc, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func ctlCall(st *mux.Stream, method string) error {
	st.SetDeadline(time.Now().Add(5 * time.Second))
	return ctrl.Call(context.Background(), st, method, nil, nil)
}

// Before the hello only the hello is served: one control stream at a time,
// nothing else.
func TestBeforeTheHelloOnlyTheHelloIsServed(t *testing.T) {
	r := newRig(t, rigOpts{})
	sess := rawSession(t, r)

	st, err := sess.Open([]byte("data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	st.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := st.Read(make([]byte, 1)); err == nil {
		t.Fatal("a non-control stream was served before the hello")
	}

	st, _ = sess.Open([]byte("ctrl"), nil)
	var ce *ctrl.Error
	if err := ctlCall(st, ctrl.MStatus); !errors.As(err, &ce) {
		t.Fatalf("status before the hello: %v", err)
	}

	// streams that sit there without a request: only a couple are kept
	var held []*mux.Stream
	for i := 0; i < 6; i++ {
		s, err := sess.Open([]byte("ctrl"), nil)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, s)
	}
	refused := 0
	for _, s := range held {
		s.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := s.Read(make([]byte, 1)); errors.Is(err, mux.ErrStreamReset) {
			refused++
		}
	}
	if refused < 4 {
		t.Fatalf("only %d of 6 idle pre-hello streams were refused", refused)
	}
}

// Connections that got through REALITY but never say hello are capped, and
// do not count against the agent's stream budget.
func TestSessionsThatNeverSayHelloAreCappedAndDoNotCount(t *testing.T) {
	r := newRig(t, rigOpts{agentMax: 1, maxPending: 2})
	r.waitConns(t, 2)
	var raws []*mux.Session
	for i := 0; i < 4; i++ {
		s := rawSession(t, r)
		s.Open([]byte("ctrl"), nil) // holds a stream on the agent
		raws = append(raws, s)
	}
	waitUntil(t, func() bool {
		closed := 0
		for _, s := range raws {
			if s.Err() != nil {
				closed++
			}
		}
		return closed >= 2
	}, "the sessions past the cap to be dropped")
	if n := r.agent.Streams(); n != 0 {
		t.Fatalf("Streams() = %d with only unauthenticated streams open", n)
	}
	st, err := r.agent.OpenData(r.dcAddr, []byte("x"))
	if err != nil {
		t.Fatalf("unauthenticated sessions used up the stream cap: %v", err)
	}
	st.Close()
}

// The pushed cap is what OpenData enforces.
func TestSetMaxStreamsMovesTheCap(t *testing.T) {
	r := newRig(t, rigOpts{agentMax: 1})
	r.waitConns(t, 2)
	r.agent.SetMaxStreams(3)
	if r.agent.MaxStreams() != 3 {
		t.Fatalf("MaxStreams() = %d", r.agent.MaxStreams())
	}
	var held []*mux.Stream
	for {
		st, err := r.agent.OpenData(r.dcAddr, nil)
		if errors.Is(err, ErrBusy) {
			break
		}
		if err != nil || len(held) > 10 {
			t.Fatalf("open: %v (%d held)", err, len(held))
		}
		held = append(held, st)
	}
	if len(held) != 3 {
		t.Fatalf("held %d streams with a cap of 3", len(held))
	}
	for _, st := range held {
		st.Close()
	}
}

func echo(t *testing.T, r *rig) {
	t.Helper()
	st, err := r.agent.OpenData(r.dcAddr, []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	buf := make([]byte, 2)
	st.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "hi" {
		t.Fatalf("%q %v", buf, err)
	}
}

// cut drops every connection the hub has dialled so far.
func (r *rig) cut() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.raws {
		c.Close()
	}
	r.raws = nil
}

// Sessions that got through REALITY but never say hello must not keep the hub
// out: the oldest makes room for a newcomer.
func TestSilentSessionsCannotKeepTheHubOut(t *testing.T) {
	r := newRig(t, rigOpts{maxPending: 2})
	r.waitConns(t, 2)
	raws := []*mux.Session{rawSession(t, r), rawSession(t, r)}
	r.cut()
	deadline := time.Now().Add(8 * time.Second)
	for r.hub.Status().Up < 2 || r.agent.Conns() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the hub could not get back in past silent sessions: hub up=%d agent conns=%d", r.hub.Status().Up, r.agent.Conns())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if raws[0].Err() == nil {
		t.Error("the oldest silent session did not make room for the hub")
	}
	echo(t, r)
}

// A connection the agent asks to retire (its stream ids running out) is
// replaced: the hub dials a fresh one, streams on the old one carry on, and new
// streams go to the fresh one.
func TestADrainingConnectionIsReplaced(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.waitConns(t, 2)
	old, err := r.agent.OpenData(r.dcAddr, []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	old.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.ReadFull(old, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	r.agent.mu.Lock()
	var retiring []*mux.Session
	for s := range r.agent.sessions {
		retiring = append(retiring, s.sess)
	}
	r.agent.mu.Unlock()
	for _, s := range retiring {
		s.GoAway()
	}
	fresh := func() int {
		r.agent.mu.Lock()
		defer r.agent.mu.Unlock()
		n := 0
		for s := range r.agent.sessions {
			if s.authed.Load() && !s.sess.Draining() {
				n++
			}
		}
		return n
	}
	waitUntil(t, func() bool { return fresh() == 2 && r.hub.Status().Up == 2 }, "the hub to dial fresh connections")
	st, err := r.agent.OpenData(r.dcAddr, []byte("b"))
	if err != nil {
		t.Fatalf("new stream with the old connections draining: %v", err)
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(st, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Write([]byte("still")); err != nil {
		t.Fatalf("the stream on the draining connection broke: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(old, buf); err != nil || string(buf) != "still" {
		t.Fatalf("draining connection: %q %v", buf, err)
	}
	old.Close()
	// once its streams are done the hub hangs up on the old connection
	waitUntil(t, func() bool { return r.agent.Conns() == 2 }, "the drained connections to close")
	for _, s := range retiring {
		if s.Err() == nil {
			t.Error("a drained connection is still up")
		}
	}
}

func agentRig(t *testing.T) (ln net.Listener, sc func() (realityserver.ServerConfig, []byte)) {
	t.Helper()
	d := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	d.EnableHTTP2 = true
	d.StartTLS()
	t.Cleanup(d.Close)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l, func() (realityserver.ServerConfig, []byte) {
		priv, pub, _ := reality.GenerateKey()
		return realityserver.ServerConfig{Dest: d.Listener.Addr().String(), ServerNames: []string{"example.com"},
			PrivateKey: priv, ShortIDs: [][8]byte{{4, 2}}, MaxTimeDiff: time.Minute}, pub
	}
}

// A Reconfigure that lands before Serve (a key pushed during start-up) must not be lost.
func TestReconfigureBeforeServeIsKept(t *testing.T) {
	ln, mk := agentRig(t)
	first, _ := mk()
	second, pub := mk()
	a := NewAgent(AgentConfig{AgentID: "agent-1", Tokens: [][]byte{randomToken()}, Listener: ln, Reality: first})
	a.Reconfigure(second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Serve(ctx)
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
	defer dcancel()
	if _, err := reality.Client(dctx, raw, reality.ClientConfig{ServerName: "example.com", PublicKey: pub, ShortID: [8]byte{4, 2}}); err != nil {
		t.Fatalf("the key set before Serve was dropped: %v", err)
	}
}

// A connection that finishes REALITY just after Close must not leave a session behind.
func TestASessionArrivingAfterCloseIsDropped(t *testing.T) {
	ln, mk := agentRig(t)
	sc, _ := mk()
	a := NewAgent(AgentConfig{AgentID: "agent-1", Tokens: [][]byte{randomToken()}, Listener: ln, Reality: sc})
	a.Close()
	if err := a.Serve(context.Background()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Serve after Close: %v, want net.ErrClosed", err)
	}
	p1, p2 := net.Pipe()
	go a.handleConn(context.Background(), p1)
	cs, err := mux.Client(p2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	select {
	case <-cs.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a session that arrived after Close is still up")
	}
}
