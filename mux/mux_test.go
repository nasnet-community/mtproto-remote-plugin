package mux

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

// tcpPair returns two ends of a real loopback TCP connection.
func tcpPair(t testing.TB) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := ln.Accept(); ch <- res{c, err} }()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	return a, r.c
}

func sessions(t testing.TB, cc, sc *Config) (*Session, *Session) {
	t.Helper()
	a, b := tcpPair(t)
	return sessionsOver(t, a, b, cc, sc)
}

func sessionsOver(t testing.TB, a, b net.Conn, cc, sc *Config) (*Session, *Session) {
	t.Helper()
	type res struct {
		s   *Session
		err error
	}
	ch := make(chan res, 1)
	go func() { s, err := Server(b, sc); ch <- res{s, err} }()
	c, err := Client(a, cc)
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { c.Close(); r.s.Close() })
	return c, r.s
}

func echoServer(t testing.TB, s *Session) {
	t.Helper()
	go func() {
		for {
			st, err := s.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(st, st)
				st.CloseWrite()
				st.Close()
			}()
		}
	}()
}

func goroutineCheck(t *testing.T) func() {
	base := runtime.NumGoroutine()
	return func() {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if runtime.NumGoroutine() <= base+2 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		buf := make([]byte, 1<<16)
		t.Errorf("goroutine leak: %d now vs %d at start\n%s", runtime.NumGoroutine(), base, buf[:runtime.Stack(buf, true)])
	}
}

func TestOpenCarriesMetaAndFirstBytes(t *testing.T) {
	c, s := sessions(t, nil, nil)
	go func() {
		st, err := s.Accept()
		if err != nil {
			return
		}
		got := make([]byte, 5)
		io.ReadFull(st, got)
		st.Write(append([]byte("meta="), st.Meta()...))
		st.Write(got)
		st.Close()
	}()
	st, err := c.Open([]byte("149.154.167.51:443"), []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5+18+5)
	if _, err := io.ReadFull(st, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "meta=149.154.167.51:443hello" {
		t.Fatalf("got %q", buf)
	}
}

func TestEitherSideCanOpen(t *testing.T) {
	c, s := sessions(t, nil, nil)
	echoServer(t, c) // the client side accepts too
	st, err := s.Open([]byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	st.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("got %q err %v", buf, err)
	}
	if st.ID()&1 != 0 {
		t.Fatalf("server-opened stream should have an even id, got %d", st.ID())
	}
}

func TestManyConcurrentStreamsEchoIntact(t *testing.T) {
	defer goroutineCheck(t)()
	c, s := sessions(t, nil, nil)
	echoServer(t, s)
	const n, size = 300, 150 << 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := make([]byte, size)
			rand.Read(data)
			want := sha256.Sum256(data)
			st, err := c.Open([]byte("e"), nil)
			if err != nil {
				errs <- err
				return
			}
			go func() { st.Write(data); st.CloseWrite() }()
			h := sha256.New()
			if _, err := io.Copy(h, st); err != nil {
				errs <- err
				return
			}
			var got [32]byte
			copy(got[:], h.Sum(nil))
			if got != want {
				errs <- errors.New("echo corrupted")
			}
			st.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return c.NumStreams() == 0 && s.NumStreams() == 0 }, "streams to drain")
	c.Close()
	s.Close()
}

func waitFor(t testing.TB, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A reader that never reads must stall the writer after one window, not buffer
// without bound.
func TestBackpressureStopsWriterAtWindow(t *testing.T) {
	c, s := sessions(t, nil, &Config{Window: 8 << 10})
	accepted := make(chan *Stream, 1)
	go func() { st, _ := s.Accept(); accepted <- st }()
	st, err := c.Open(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := <-accepted
	st.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
	n, err := st.Write(make([]byte, 1<<20))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("want write timeout, got %v after %d bytes", err, n)
	}
	if n != 8<<10 {
		t.Fatalf("writer got %d bytes through, want exactly the 8 KiB window", n)
	}
	// the reader draining unblocks the writer
	st.SetWriteDeadline(time.Time{})
	go func() { io.Copy(io.Discard, srv) }()
	if _, err := st.Write(make([]byte, 100<<10)); err != nil {
		t.Fatalf("write after drain: %v", err)
	}
}

func TestHalfCloseKeepsOtherDirectionOpen(t *testing.T) {
	c, s := sessions(t, nil, nil)
	go func() {
		st, _ := s.Accept()
		b, _ := io.ReadAll(st) // returns at the client's FIN
		st.Write(bytes.ToUpper(b))
		st.CloseWrite()
		st.Close()
	}()
	st, _ := c.Open(nil, nil)
	st.Write([]byte("shout"))
	st.CloseWrite()
	got, err := io.ReadAll(st)
	if err != nil || string(got) != "SHOUT" {
		t.Fatalf("got %q err %v", got, err)
	}
	st.Close()
	waitFor(t, func() bool { return c.NumStreams() == 0 && s.NumStreams() == 0 }, "both ends to forget the stream")
}

// Writing a reply and then closing must deliver the whole reply: Close sends
// FIN behind the data, it does not abort.
func TestWriteThenCloseDeliversEverything(t *testing.T) {
	c, s := sessions(t, nil, nil)
	payload := make([]byte, 300<<10)
	rand.Read(payload)
	go func() {
		st, _ := s.Accept()
		st.Write(payload)
		st.Close()
	}()
	st, _ := c.Open(nil, nil)
	got, err := io.ReadAll(st)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("got %d bytes (want %d) err %v", len(got), len(payload), err)
	}
}

// The FIN must queue behind data the stream already wrote.
func TestFINDoesNotOvertakeData(t *testing.T) {
	c, s := sessions(t, nil, nil)
	payload := make([]byte, 200<<10)
	rand.Read(payload)
	res := make(chan []byte, 1)
	go func() {
		st, _ := s.Accept()
		b, _ := io.ReadAll(st)
		res <- b
		st.Close()
	}()
	st, _ := c.Open(nil, nil)
	st.Write(payload)
	st.CloseWrite()
	select {
	case got := <-res:
		if !bytes.Equal(got, payload) {
			t.Fatalf("server saw %d bytes before FIN, want %d", len(got), len(payload))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the FIN")
	}
}

// Like a closed TCP socket: once one side has closed, the peer's next data is
// answered with a reset so it stops talking into the void.
func TestDataAfterCloseIsReset(t *testing.T) {
	c, s := sessions(t, nil, nil)
	accepted := make(chan *Stream, 1)
	go func() { st, _ := s.Accept(); accepted <- st }()
	st, _ := c.Open(nil, nil)
	srv := <-accepted
	srv.Close() // sends FIN; the client has not finished sending
	if _, err := st.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read after peer close: %v, want EOF", err)
	}
	waitFor(t, func() bool {
		_, err := st.Write([]byte("x"))
		return errors.Is(err, ErrStreamReset)
	}, "a reset after writing into a closed stream")
}

func TestUnreadDataMakesCloseReset(t *testing.T) {
	c, s := sessions(t, nil, nil)
	accepted := make(chan *Stream, 1)
	go func() { st, _ := s.Accept(); accepted <- st }()
	st, _ := c.Open(nil, nil)
	srv := <-accepted
	st.Write([]byte("nobody reads this"))
	waitFor(t, func() bool { srv.mu.Lock(); defer srv.mu.Unlock(); return srv.buffered > 0 }, "data to land")
	srv.Close()
	waitFor(t, func() bool {
		_, err := st.Read(make([]byte, 1))
		return errors.Is(err, ErrStreamReset)
	}, "reset after closing with unread data")
}

// A stream closed locally whose peer never answers must not live forever.
func TestLingerReapsHalfClosedStreams(t *testing.T) {
	c, s := sessions(t, &Config{KeepAlive: 50 * time.Millisecond, Linger: 150 * time.Millisecond}, nil)
	accepted := make(chan *Stream, 1)
	go func() { st, _ := s.Accept(); accepted <- st }()
	st, _ := c.Open(nil, nil)
	srv := <-accepted
	st.Close() // FIN; the server never closes its side
	waitFor(t, func() bool { return c.NumStreams() == 0 }, "client to reap the lingering stream")
	if _, err := srv.Read(make([]byte, 1)); err != io.EOF && !errors.Is(err, ErrStreamReset) {
		t.Fatalf("server read: %v", err)
	}
}

func TestGoAwayBlocksNewStreamsKeepsOldOnes(t *testing.T) {
	c, s := sessions(t, nil, nil)
	echoServer(t, s)
	st, _ := c.Open(nil, nil)
	// settle the stream first: GoAway refuses opens that are still in flight
	st.Write([]byte("hi"))
	io.ReadFull(st, make([]byte, 2))
	s.GoAway()
	waitFor(t, func() bool { _, err := c.Open(nil, nil); return errors.Is(err, ErrGoAway) }, "ErrGoAway on new opens")
	st.Write([]byte("still"))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "still" {
		t.Fatalf("old stream broke: %q %v", buf, err)
	}
}

func TestOpenRespectsPeerStreamLimit(t *testing.T) {
	c, s := sessions(t, nil, &Config{MaxStreams: 2})
	go func() {
		for {
			if _, err := s.Accept(); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 2; i++ {
		if _, err := c.Open(nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Open(nil, nil); !errors.Is(err, ErrTooManyOpen) {
		t.Fatalf("third open: %v, want ErrTooManyOpen", err)
	}
}

func TestSessionCloseFailsEveryStream(t *testing.T) {
	defer goroutineCheck(t)()
	c, s := sessions(t, nil, nil)
	echoServer(t, s)
	var sts []*Stream
	for i := 0; i < 20; i++ {
		st, _ := c.Open(nil, nil)
		sts = append(sts, st)
	}
	c.Close()
	for _, st := range sts {
		if _, err := st.Read(make([]byte, 1)); err == nil {
			t.Fatal("read succeeded on a closed session")
		}
	}
	if _, err := c.Open(nil, nil); err == nil {
		t.Fatal("open succeeded on a closed session")
	}
	s.Close()
}

func TestDeadPeerTimesOut(t *testing.T) {
	a, b := net.Pipe()
	// b answers the settings exchange, then goes silent
	go func() {
		h := make([]byte, headerLen+settingsLen)
		header{typ: typeSettings, length: settingsLen}.put(h)
		copy(h[headerLen:], settings{version: settingsV, window: 1 << 14, maxFrame: 1 << 14, maxStreams: 10}.marshal())
		go b.Write(h)
		io.ReadFull(b, make([]byte, len(h)))
		io.Copy(io.Discard, b)
	}()
	c, err := Client(a, &Config{Timeout: 300 * time.Millisecond, KeepAlive: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
		if !errors.Is(c.Err(), ErrTimeout) {
			t.Fatalf("err = %v, want ErrTimeout", c.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("silent peer never timed out")
	}
}

// rawPeer plays the other end by hand so tests can send frames a real session
// never would.
type rawPeer struct {
	conn net.Conn
}

func newRawPeer(t *testing.T, server bool, cfg *Config) (*Session, *rawPeer) {
	t.Helper()
	a, b := net.Pipe()
	h := make([]byte, headerLen+settingsLen)
	header{typ: typeSettings, length: settingsLen}.put(h)
	copy(h[headerLen:], settings{version: settingsV, window: 1 << 14, maxFrame: 1 << 14, maxStreams: 100}.marshal())
	go func() {
		b.Write(h)
		io.ReadFull(b, make([]byte, len(h)))
	}()
	var s *Session
	var err error
	if server {
		s, err = Server(a, cfg)
	} else {
		s, err = Client(a, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); b.Close() })
	go io.Copy(io.Discard, b) // swallow whatever the session sends
	return s, &rawPeer{conn: b}
}

func (p *rawPeer) send(t *testing.T, h header, payload []byte) {
	t.Helper()
	h.length = uint16(len(payload))
	buf := make([]byte, headerLen+len(payload))
	h.put(buf)
	copy(buf[headerLen:], payload)
	if _, err := p.conn.Write(buf); err != nil {
		t.Fatal(err)
	}
}

func TestPeerExceedingWindowResetsOnlyThatStream(t *testing.T) {
	s, p := newRawPeer(t, true, &Config{Window: 1024})
	// client (raw peer) opens odd stream 1, then sends more than the 1024 window
	p.send(t, header{typ: typeOpen, stream: 1}, []byte{0})
	st, err := s.Accept()
	if err != nil {
		t.Fatal(err)
	}
	p.send(t, header{typ: typeData, stream: 1}, make([]byte, 1025))
	_, err = st.Read(make([]byte, 1))
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("read: %v, want ErrProtocol", err)
	}
	if s.Err() != nil {
		t.Fatalf("session died too: %v", s.Err())
	}
}

func TestMalformedFramesKillTheSession(t *testing.T) {
	cases := map[string]struct {
		h       header
		payload []byte
	}{
		"unknown type":           {header{typ: 99}, nil},
		"credit with bad length": {header{typ: typeCredit, stream: 1}, []byte{1, 2}},
		"data on stream 0":       {header{typ: typeData}, []byte{1}},
		"ping with stream":       {header{typ: typePing, stream: 3}, make([]byte, 8)},
		"open without meta len":  {header{typ: typeOpen, stream: 1}, nil},
		"wrong parity open":      {header{typ: typeOpen, stream: 2}, []byte{0}},
		"oversize frame":         {header{typ: typeData, stream: 1}, make([]byte, 1<<14+1)},
		"second settings":        {header{typ: typeSettings}, settings{version: settingsV, window: 1, maxFrame: 512}.marshal()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, p := newRawPeer(t, true, nil)
			p.send(t, tc.h, tc.payload)
			select {
			case <-s.Done():
				if !errors.Is(s.Err(), ErrProtocol) {
					t.Fatalf("err = %v, want ErrProtocol", s.Err())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("session survived a malformed frame")
			}
		})
	}
}

func TestDeadlines(t *testing.T) {
	c, s := sessions(t, nil, nil)
	go func() { s.Accept() }()
	st, _ := c.Open(nil, nil)
	st.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := st.Read(make([]byte, 1))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("got %v, want a timeout", err)
	}
	// the stream still works after a timeout
	st.SetReadDeadline(time.Time{})
	if _, err := st.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
}

// delayConn adds a fixed one-way delay to everything written through it.
type delayConn struct {
	net.Conn
	d    time.Duration
	q    chan delayed
	done chan struct{}
	once sync.Once
}
type delayed struct {
	at time.Time
	b  []byte
}

func newDelayConn(c net.Conn, d time.Duration) *delayConn {
	dc := &delayConn{Conn: c, d: d, q: make(chan delayed, 4096), done: make(chan struct{})}
	go func() {
		for {
			select {
			case m := <-dc.q:
				time.Sleep(time.Until(m.at))
				if _, err := dc.Conn.Write(m.b); err != nil {
					return
				}
			case <-dc.done:
				return
			}
		}
	}()
	return dc
}

func (c *delayConn) Write(b []byte) (int, error) {
	select {
	case c.q <- delayed{time.Now().Add(c.d), append([]byte(nil), b...)}:
		return len(b), nil
	case <-c.done:
		return 0, net.ErrClosed
	}
}
func (c *delayConn) Close() error { c.once.Do(func() { close(c.done) }); return c.Conn.Close() }

// Over a 40 ms RTT a fixed 16 KiB window caps a stream near 400 KB/s. The
// auto-tuned window must lift that by an order of magnitude.
func TestAutoTuneRaisesThroughputOnLongRTT(t *testing.T) {
	a, b := tcpPair(t)
	da, db := newDelayConn(a, 20*time.Millisecond), newDelayConn(b, 20*time.Millisecond)
	c, s := sessionsOver(t, da, db, nil, nil)
	go func() {
		st, err := s.Accept()
		if err != nil {
			return
		}
		io.Copy(io.Discard, st)
	}()
	st, _ := c.Open(nil, nil)
	const total = 6 << 20
	start := time.Now()
	buf := make([]byte, 32<<10)
	for sent := 0; sent < total; sent += len(buf) {
		if _, err := st.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	st.CloseWrite()
	// the last window is still in flight: the clock stops when it has landed
	waitFor(t, func() bool { return s.Stats().BytesIn >= total }, "the last bytes to land")
	took := time.Since(start)
	rate := float64(total) / took.Seconds() / (1 << 20)
	t.Logf("6 MiB in %v = %.1f MiB/s, rtt=%v, growth steps=%d", took.Round(time.Millisecond), rate, c.RTT().Round(time.Millisecond), s.Stats().WindowGrowth)
	if s.Stats().WindowGrowth == 0 {
		t.Fatal("window never grew")
	}
	if rate < 2.0 {
		t.Fatalf("%.2f MiB/s: auto-tune did not lift the window-bound rate (~0.4 MiB/s)", rate)
	}
	if got := s.Stats().GrowUsed; got > 8<<20 {
		t.Fatalf("growth pool overrun: %d", got)
	}
}

func TestGrowBudgetCapsTotalGrowth(t *testing.T) {
	a, b := tcpPair(t)
	da, db := newDelayConn(a, 10*time.Millisecond), newDelayConn(b, 10*time.Millisecond)
	c, s := sessionsOver(t, da, db, nil, &Config{GrowBudget: 100 << 10})
	for i := 0; i < 4; i++ {
		go func() {
			st, err := s.Accept()
			if err != nil {
				return
			}
			io.Copy(io.Discard, st)
		}()
		st, _ := c.Open(nil, nil)
		go func() {
			buf := make([]byte, 32<<10)
			for j := 0; j < 100; j++ {
				if _, err := st.Write(buf); err != nil {
					return
				}
			}
		}()
	}
	time.Sleep(1500 * time.Millisecond)
	if got := s.Stats().GrowUsed; got > 100<<10 {
		t.Fatalf("grow pool %d exceeds the 100 KiB budget", got)
	}
}

// Idle streams must cost almost nothing: this is what makes 2.5k fit in 50 MB.
func TestIdleStreamFootprint(t *testing.T) {
	// a deep backlog: this measures memory, not how fast Accept drains a burst
	c, s := sessions(t, nil, &Config{AcceptBacklog: 4096})
	go func() {
		for {
			if _, err := s.Accept(); err != nil {
				return
			}
		}
	}()
	settleHeap()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const n = 2000
	for i := 0; i < n; i++ {
		if _, err := c.Open([]byte("149.154.167.51:443"), nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; s.NumStreams() != n; i++ {
		if i == 1000 {
			t.Fatalf("server has %d of %d streams (stats %+v)", s.NumStreams(), n, s.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	settleHeap()
	runtime.ReadMemStats(&after)
	per := (int64(after.HeapAlloc) - int64(before.HeapAlloc)) / n
	t.Logf("idle stream (both ends, mux state only): %d bytes", per)
	if per > 4096 {
		t.Fatalf("%d bytes per idle stream pair is too much", per)
	}
}

// settleHeap lets the garbage of earlier tests go before a heap reading.
func settleHeap() {
	for i := 0; i < 3; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
}

func BenchmarkSingleStreamThroughput(b *testing.B) {
	c, s := sessions(b, &Config{Window: 256 << 10}, &Config{Window: 256 << 10})
	go func() {
		st, err := s.Accept()
		if err != nil {
			return
		}
		io.Copy(io.Discard, st)
	}()
	st, _ := c.Open(nil, nil)
	buf := make([]byte, 64<<10)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.Write(buf); err != nil {
			b.Fatal(err)
		}
	}
}

func FuzzHeaderValidate(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0, 0, 1, 0, 5})
	f.Add([]byte{8, 0, 0, 0, 0, 0, 0, 11})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < headerLen {
			return
		}
		h := parseHeader(b)
		_ = h.validate(1 << 14) // must never panic
	})
}

// Whatever bytes a peer sends after a valid handshake, the session must end
// or survive cleanly: no panic, no hang, no leaked goroutines.
func FuzzSessionInput(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0, 0, 1, 0, 1, 0})
	f.Add([]byte{2, 1, 0, 0, 0, 1, 0, 0})
	f.Add([]byte{3, 0, 0, 0, 0, 1, 0, 4, 0, 0, 0, 9})
	f.Fuzz(func(t *testing.T, in []byte) {
		a, b := net.Pipe()
		h := make([]byte, headerLen+settingsLen)
		header{typ: typeSettings, length: settingsLen}.put(h)
		copy(h[headerLen:], settings{version: settingsV, window: 1 << 14, maxFrame: 1 << 14, maxStreams: 100}.marshal())
		go func() {
			b.Write(h)
			io.ReadFull(b, make([]byte, len(h)))
			b.Write(in)
			io.Copy(io.Discard, b)
		}()
		s, err := Server(a, &Config{Timeout: 300 * time.Millisecond, KeepAlive: time.Hour})
		if err != nil {
			return
		}
		defer s.Close()
		go func() {
			for {
				st, err := s.Accept()
				if err != nil {
					return
				}
				go func() { io.Copy(io.Discard, st); st.Close() }()
			}
		}()
		select {
		case <-s.Done():
		case <-time.After(1200 * time.Millisecond):
			// still alive after the input drained is fine; the idle timeout (300 ms)
			// must have fired by now though
			t.Fatal("session neither failed nor timed out")
		}
	})
}

// The reaper took st.mu under s.mu while a growing Read took s.mu under st.mu: a frozen session.
func TestReaperAndWindowGrowthCannotDeadlock(t *testing.T) {
	// no cleanup helper: closing a deadlocked session would hang the test too
	a, b := tcpPair(t)
	srv := make(chan *Session, 1)
	go func() { s, _ := Server(b, nil); srv <- s }()
	c, err := Client(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := <-srv
	go func() { s.Accept() }()
	st, err := c.Open(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	held, grew := make(chan struct{}), make(chan struct{})
	go func() {
		st.mu.Lock() // where Read sits in afterReadLocked
		close(held)
		time.Sleep(100 * time.Millisecond) // the reaper takes its locks meanwhile
		c.grow(1)
		st.mu.Unlock()
		close(grew)
	}()
	<-held
	reaped := make(chan struct{})
	go func() { c.reapLingering(); close(reaped) }()
	for _, ch := range []chan struct{}{grew, reaped} {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("the reaper and a growing reader wait on each other")
		}
	}
	c.Close()
	s.Close()
}

// 1-byte frames used to pin a pooled buffer each: the window bounded payload, not memory.
func TestTinyFramesCannotPinABufferEach(t *testing.T) {
	s, p := newRawPeer(t, true, nil)
	const streams, frames = 4, 16 << 10 // the whole default window, one byte at a time
	settleHeap()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	one := make([]byte, headerLen+1)
	var burst bytes.Buffer
	for i := 0; i < streams; i++ {
		id := uint32(2*i + 1)
		p.send(t, header{typ: typeOpen, stream: id}, []byte{0})
		header{typ: typeData, stream: id, length: 1}.put(one)
		burst.Reset()
		for j := 0; j < frames; j++ {
			burst.Write(one)
		}
		if _, err := p.conn.Write(burst.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return s.Stats().BytesIn == streams*frames }, "every byte to land")
	burst = bytes.Buffer{}
	settleHeap()
	runtime.ReadMemStats(&after)
	per := (int64(after.HeapAlloc) - int64(before.HeapAlloc)) / streams
	t.Logf("%d one-byte frames held: %d bytes per stream", frames, per)
	if per > 64<<10 {
		t.Fatalf("a %d-byte window holds %d bytes of heap", frames, per)
	}
	if s.Err() != nil {
		t.Fatalf("session died: %v", s.Err())
	}
}

// Packed small frames, the OPEN's bytes included, must read back intact and in order.
func TestPackedFramesReadBackIntact(t *testing.T) {
	s, p := newRawPeer(t, true, nil)
	want := make([]byte, 16<<10)
	rand.Read(want)
	first := 700
	p.send(t, header{typ: typeOpen, stream: 1}, append([]byte{3, 'a', 'b', 'c'}, want[:first]...))
	st, err := s.Accept()
	if err != nil {
		t.Fatal(err)
	}
	sizes := []int{1, 255, 256, 257, 2047, 2048, 2049, 1, 1, 3000, 16 << 10}
	for off, i := first, 0; off < len(want); i++ {
		n := min(sizes[i%len(sizes)], len(want)-off)
		p.send(t, header{typ: typeData, stream: 1}, want[off:off+n])
		off += n
	}
	p.send(t, header{typ: typeData, stream: 1, flags: flagFIN}, nil)
	got, err := io.ReadAll(st)
	if err != nil || !bytes.Equal(got, want) || string(st.Meta()) != "abc" {
		t.Fatalf("read %d of %d bytes, equal=%v, meta %q, err %v", len(got), len(want), bytes.Equal(got, want), st.Meta(), err)
	}
}

// On 32-bit routers a credit past 2^31 went negative and Write panicked slicing with it.
func TestHugeCreditIsAProtocolError(t *testing.T) {
	s, p := newRawPeer(t, true, nil)
	for i, credit := range []uint32{1 << 31, 1<<32 - 1, maxWindowSetting} {
		id := uint32(2*i + 1)
		p.send(t, header{typ: typeOpen, stream: id}, []byte{0})
		st, err := s.Accept()
		if err != nil {
			t.Fatal(err)
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], credit)
		p.send(t, header{typ: typeCredit, stream: id}, b[:])
		waitFor(t, func() bool {
			_, err := st.Write([]byte("x"))
			return errors.Is(err, ErrProtocol)
		}, fmt.Sprintf("a credit of %d to fail the stream", credit))
	}
	if s.Err() != nil {
		t.Fatalf("one bad stream took the session down: %v", s.Err())
	}
}

func TestHugeSettingsWindowIsRefused(t *testing.T) {
	for _, w := range []uint32{1 << 31, 1<<32 - 1, maxWindowSetting + 1} {
		a, b := net.Pipe()
		h := make([]byte, headerLen+settingsLen)
		header{typ: typeSettings, length: settingsLen}.put(h)
		copy(h[headerLen:], settings{version: settingsV, window: w, maxFrame: 1 << 14, maxStreams: 10}.marshal())
		go func() {
			b.Write(h)
			io.ReadFull(b, make([]byte, len(h)))
		}()
		s, err := Server(a, nil)
		if !errors.Is(err, ErrProtocol) {
			t.Errorf("window %d: err = %v, want ErrProtocol", w, err)
		}
		if s != nil {
			s.Close()
		}
		b.Close()
	}
}

// gateConn holds writes while shut, standing in for a stalled link.
type gateConn struct {
	net.Conn
	mu   sync.Mutex
	open chan struct{}
}

func newGateConn(c net.Conn) *gateConn {
	g := &gateConn{Conn: c, open: make(chan struct{})}
	close(g.open)
	return g
}

func (g *gateConn) shut() { g.mu.Lock(); g.open = make(chan struct{}); g.mu.Unlock() }

func (g *gateConn) reopen() {
	g.mu.Lock()
	if !isClosed(g.open) {
		close(g.open)
	}
	g.mu.Unlock()
}

func (g *gateConn) Write(b []byte) (int, error) {
	g.mu.Lock()
	ch := g.open
	g.mu.Unlock()
	<-ch
	return g.Conn.Write(b)
}

// A write that times out in the send queue never sent its frame, so it must
// give the credit back; otherwise the stream stalls for good.
func TestTimedOutWriteKeepsItsCredit(t *testing.T) {
	a, b := tcpPair(t)
	g := newGateConn(a)
	t.Cleanup(g.reopen)
	c, s := sessionsOver(t, g, b, nil, nil)
	go func() {
		for {
			st, err := s.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, st)
		}
	}()
	st, err := c.Open(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var others []*Stream
	for i := 0; i < 80; i++ {
		o, err := c.Open(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		others = append(others, o)
	}
	g.shut()
	// the others fill the send queue behind the stalled link
	for _, o := range others {
		go o.Write(make([]byte, 16<<10))
	}
	waitFor(t, func() bool { return len(c.loq) == cap(c.loq) }, "the send queue to fill")
	st.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
	if n, err := st.Write(make([]byte, 16<<10)); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("stalled write: n=%d err=%v, want a timeout with nothing sent", n, err)
	}
	g.reopen()
	st.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if n, err := st.Write(make([]byte, 16<<10)); err != nil {
		t.Fatalf("write after the stall: n=%d err=%v (credit lost to the timed-out write)", n, err)
	}
}

// Open must not hand back a live stream together with an error: callers that
// only check the error would leak it.
func TestOpenFailingMidFirstReturnsNoStream(t *testing.T) {
	c, s := sessions(t, nil, nil)
	go func() {
		for {
			st, err := s.Accept()
			if err != nil {
				return
			}
			st.Reset()
		}
	}()
	st, err := c.Open([]byte("m"), make([]byte, 64<<10))
	if err == nil {
		t.Fatal("open succeeded although the peer reset the stream mid-first")
	}
	if st != nil {
		t.Fatalf("Open returned a stream along with %v", err)
	}
}

// Running out of stream ids must drain the session, not wrap to an id the
// peer rejects (which kills every stream on it).
func TestStreamIDsRunningOutDrainsTheSession(t *testing.T) {
	c, s := sessions(t, nil, nil)
	echoServer(t, c)
	old, err := s.Open(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	echo := func(st *Stream, msg string) {
		t.Helper()
		st.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := st.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(st, buf); err != nil || string(buf) != msg {
			t.Fatalf("echo %q: %q %v", msg, buf, err)
		}
	}
	echo(old, "a")
	s.mu.Lock()
	s.nextID = 1<<32 - 4 // two even ids left
	s.mu.Unlock()
	for i := 0; i < 2; i++ {
		st, err := s.Open(nil, nil)
		if err != nil {
			t.Fatalf("open %d of the last two ids: %v", i+1, err)
		}
		echo(st, "b")
	}
	if st, err := s.Open(nil, nil); !errors.Is(err, ErrIDsExhausted) || !errors.Is(err, ErrGoAway) || st != nil {
		t.Fatalf("open past the last id: st=%v err=%v, want ErrIDsExhausted (an ErrGoAway)", st != nil, err)
	}
	// the peer was asked to move to a fresh connection before the ids ran out
	select {
	case <-c.PeerGoAway():
	case <-time.After(3 * time.Second):
		t.Fatal("the peer never got a GoAway")
	}
	if !c.Draining() || !s.Draining() {
		t.Fatalf("Draining: client %v, server %v", c.Draining(), s.Draining())
	}
	if _, err := c.Open(nil, nil); !errors.Is(err, ErrGoAway) {
		t.Fatalf("peer open on a draining session: %v", err)
	}
	if c.Err() != nil || s.Err() != nil {
		t.Fatalf("the session died: client %v, server %v", c.Err(), s.Err())
	}
	echo(old, "still")
}

// CloseWrite must not wait out the credit a concurrent Write is blocked on.
func TestCloseWriteDoesNotWaitForCredit(t *testing.T) {
	c, s := sessions(t, nil, nil)
	accepted := make(chan *Stream, 1)
	go func() { st, _ := s.Accept(); accepted <- st }()
	st, err := c.Open(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := <-accepted
	werr := make(chan error, 1)
	go func() { _, err := st.Write(make([]byte, 64<<10)); werr <- err }()
	waitFor(t, func() bool { st.mu.Lock(); defer st.mu.Unlock(); return st.sendCredit == 0 }, "the writer to run out of credit")
	done := make(chan error, 1)
	go func() { done <- st.CloseWrite() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CloseWrite is waiting for credit the peer will never send")
	}
	if err := <-werr; !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("blocked write: %v, want ErrClosedPipe", err)
	}
	srv.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(srv)
	if err != nil || len(got) != 16<<10 {
		t.Fatalf("peer read %d bytes, %v; want the 16 KiB window then EOF", len(got), err)
	}
}
