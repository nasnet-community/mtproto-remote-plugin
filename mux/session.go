// Package mux carries thousands of mostly idle streams over one connection, with
// credit windows bounding receive memory; frame.go has the wire format.
package mux

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrSessionClosed = errors.New("mux: session closed")
	ErrGoAway        = errors.New("mux: peer is shutting down")
	ErrTimeout       = errors.New("mux: peer timed out")
	ErrStreamReset   = errors.New("mux: stream reset by peer")
	ErrTooManyOpen   = errors.New("mux: too many open streams")
	ErrMetaTooLong   = errors.New("mux: meta longer than 255 bytes")
	// ErrIDsExhausted: this session has no stream ids left; it matches ErrGoAway.
	ErrIDsExhausted error = idsExhausted{}
)

type idsExhausted struct{}

func (idsExhausted) Error() string     { return "mux: stream ids used up, open on a fresh connection" }
func (idsExhausted) Is(err error) bool { return err == ErrGoAway }

const (
	maxStreamID = 1<<32 - 1
	// with this few ids left, ask the peer for a fresh connection while they last
	idLowWater = 1 << 16
)

// Config tunes a session. Zero values take the defaults noted per field; the
// numbers are the soak-tested ones (see the plan's mux defaults).
type Config struct {
	MaxFrame         int           // largest DATA payload we send/accept (16 KiB)
	Window           uint32        // guaranteed receive window per stream (16 KiB)
	MaxWindow        uint32        // auto-tune ceiling per stream (256 KiB)
	GrowBudget       uint32        // sum of window growth over all streams (8 MiB)
	MaxStreams       int           // peer-opened streams alive at once (8192: one conn can carry a whole link)
	AcceptBacklog    int           // opened-but-not-accepted streams (1024)
	KeepAlive        time.Duration // ping interval (10 s)
	Timeout          time.Duration // silence that kills the session (30 s)
	HandshakeTimeout time.Duration // settings exchange (15 s)
	Linger           time.Duration // a closed stream waits this long for the peer's FIN (30 s)
}

func (c *Config) withDefaults() {
	if c.MaxFrame == 0 {
		c.MaxFrame = 16 << 10
	}
	if c.Window == 0 {
		c.Window = 16 << 10
	}
	if c.MaxWindow == 0 {
		c.MaxWindow = 256 << 10
	}
	if c.GrowBudget == 0 {
		c.GrowBudget = 8 << 20
	}
	if c.MaxStreams == 0 {
		c.MaxStreams = 8192
	}
	if c.AcceptBacklog == 0 {
		c.AcceptBacklog = 1024
	}
	if c.KeepAlive == 0 {
		c.KeepAlive = 10 * time.Second
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.HandshakeTimeout == 0 {
		c.HandshakeTimeout = 15 * time.Second
	}
	if c.Linger == 0 {
		c.Linger = 30 * time.Second
	}
	if c.MaxFrame > maxPayload {
		c.MaxFrame = maxPayload
	}
	c.Window = min(c.Window, maxWindowSetting)
	c.MaxWindow = min(c.MaxWindow, maxWindowSetting)
	if c.MaxWindow < c.Window {
		c.MaxWindow = c.Window
	}
}

// Stats is a point-in-time copy of the session counters.
type Stats struct {
	Streams      int // currently open
	Opened       uint64
	Accepted     uint64
	Resets       uint64
	BytesIn      uint64 // DATA payload bytes received
	BytesOut     uint64
	GrowUsed     uint32
	RTT          time.Duration
	Refused      uint64 // opens we refused (cap or full accept queue)
	WindowGrowth uint64 // auto-tune doublings
}

type outFrame struct {
	pb *pbuf
	n  int
}

// Session multiplexes streams over one connection.
type Session struct {
	conn   net.Conn
	cfg    Config
	client bool
	peer   settings
	maxOut int // largest DATA payload we may send

	// Lock order: a Stream's wmu, then its mu; s.mu is never held with a Stream's mu.
	mu         sync.Mutex
	streams    map[uint32]*Stream
	nextID     uint64 // wider than ids so running out is visible
	ours       int    // live streams we opened
	theirs     int    // live streams the peer opened
	goAwaySent bool
	goAwayRecv bool
	drain      bool          // we asked the peer to move to a fresh connection
	goAwayc    chan struct{} // closed when the peer's GoAway arrives
	growUsed   atomic.Uint32 // lock-free: Read grows the window holding st.mu

	qmu  sync.Mutex
	ctl  []outFrame // small frames; never blocks the read loop
	kick chan struct{}
	loq  chan outFrame // DATA frames; senders block here (backpressure)

	acceptq chan *Stream
	done    chan struct{}
	errOnce sync.Once
	err     error
	rtt     atomic.Int64

	opened, accepted, resets, refused, growths atomic.Uint64
	bytesIn, bytesOut                          atomic.Uint64
}

// Client returns a session that opens odd-numbered streams. The side that
// dialed the TCP connection conventionally uses it; Server is its mirror.
func Client(conn net.Conn, cfg *Config) (*Session, error) { return newSession(conn, cfg, true) }

// Server returns a session that opens even-numbered streams.
func Server(conn net.Conn, cfg *Config) (*Session, error) { return newSession(conn, cfg, false) }

func newSession(conn net.Conn, cfg *Config, client bool) (*Session, error) {
	var c Config
	if cfg != nil {
		c = *cfg
	}
	c.withDefaults()
	s := &Session{
		conn:    conn,
		cfg:     c,
		client:  client,
		streams: make(map[uint32]*Stream),
		kick:    make(chan struct{}, 1),
		loq:     make(chan outFrame, 64),
		acceptq: make(chan *Stream, c.AcceptBacklog),
		done:    make(chan struct{}),
		goAwayc: make(chan struct{}),
		nextID:  2,
	}
	if client {
		s.nextID = 1
	}

	br := bufio.NewReaderSize(conn, 32<<10)
	if err := s.exchangeSettings(br); err != nil {
		conn.Close()
		return nil, err
	}
	s.maxOut = c.MaxFrame
	if int(s.peer.maxFrame) < s.maxOut {
		s.maxOut = int(s.peer.maxFrame)
	}
	go s.readLoop(br)
	go s.writeLoop()
	go s.pingLoop()
	return s, nil
}

func (s *Session) exchangeSettings(br *bufio.Reader) error {
	mine := settings{
		version:    settingsV,
		window:     s.cfg.Window,
		maxFrame:   uint16(s.cfg.MaxFrame),
		maxStreams: uint32(s.cfg.MaxStreams),
	}.marshal()
	frame := make([]byte, headerLen+len(mine))
	header{typ: typeSettings, length: uint16(len(mine))}.put(frame)
	copy(frame[headerLen:], mine)

	_ = s.conn.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	defer s.conn.SetDeadline(time.Time{})

	// write in a goroutine: an unbuffered conn (net.Pipe) would deadlock if both
	// sides wrote before reading
	start := time.Now()
	werr := make(chan error, 1)
	go func() { _, err := s.conn.Write(frame); werr <- err }()

	var hb [headerLen]byte
	if _, err := io.ReadFull(br, hb[:]); err != nil {
		return fmt.Errorf("mux: reading peer settings: %w", err)
	}
	h := parseHeader(hb[:])
	if h.typ != typeSettings {
		return fmt.Errorf("%w: first frame is type %d, not settings", ErrProtocol, h.typ)
	}
	if err := h.validate(maxPayload); err != nil {
		return err
	}
	payload := make([]byte, h.length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return fmt.Errorf("mux: reading peer settings: %w", err)
	}
	peer, err := parseSettings(payload)
	if err != nil {
		return err
	}
	if err := <-werr; err != nil {
		return fmt.Errorf("mux: sending settings: %w", err)
	}
	s.peer = peer
	s.rtt.Store(int64(time.Since(start)))
	return nil
}

// Open starts a stream. meta travels in the OPEN frame (the link puts the
// destination there) and first, if any, rides along as the stream's first bytes
// so the far side can act before a round trip. first beyond what one frame and
// the peer's window allow is sent as ordinary DATA.
func (s *Session) Open(meta, first []byte) (*Stream, error) {
	if len(meta) > 255 {
		return nil, ErrMetaTooLong
	}
	s.mu.Lock()
	switch {
	case s.isDone():
		s.mu.Unlock()
		return nil, s.Err()
	case s.goAwaySent || s.goAwayRecv:
		s.mu.Unlock()
		return nil, ErrGoAway
	case int64(s.ours) >= int64(s.peer.maxStreams):
		s.mu.Unlock()
		return nil, ErrTooManyOpen
	case s.nextID > maxStreamID:
		s.mu.Unlock()
		return nil, ErrIDsExhausted
	}
	id := uint32(s.nextID)
	s.nextID += 2
	askNew := !s.drain && s.nextID+idLowWater > maxStreamID
	if askNew {
		s.drain = true
	}
	st := s.newStream(id, append([]byte(nil), meta...), int(s.peer.window), int(s.cfg.Window))
	s.streams[id] = st
	s.ours++
	s.mu.Unlock()
	s.opened.Add(1)
	if askNew {
		// the peer stops opening here and redials; our streams carry on meanwhile
		s.sendCtl(header{typ: typeGoAway}, nil)
	}

	n := len(first)
	if room := s.maxOut - 1 - len(meta); n > room {
		n = room
	}
	if n > int(s.peer.window) {
		n = int(s.peer.window)
	}
	pb := getBuf(headerLen + 1 + len(meta) + n)
	header{typ: typeOpen, stream: id, length: uint16(1 + len(meta) + n)}.put(pb.b)
	pb.b[headerLen] = byte(len(meta))
	copy(pb.b[headerLen+1:], meta)
	copy(pb.b[headerLen+1+len(meta):], first[:n])
	st.mu.Lock()
	st.sendCredit -= n
	st.mu.Unlock()
	if err := s.sendData(outFrame{pb, headerLen + 1 + len(meta) + n}, nil); err != nil {
		st.abort(err)
		return nil, err
	}
	s.bytesOut.Add(uint64(n))
	if rest := first[n:]; len(rest) > 0 {
		if _, err := st.Write(rest); err != nil {
			st.Reset()
			return nil, err
		}
	}
	return st, nil
}

// Accept waits for a stream opened by the peer.
func (s *Session) Accept() (*Stream, error) {
	select {
	case st := <-s.acceptq:
		return st, nil
	case <-s.done:
		// hand out anything already queued before reporting the close
		select {
		case st := <-s.acceptq:
			return st, nil
		default:
		}
		return nil, s.Err()
	}
}

// GoAway tells the peer to stop opening streams; existing ones carry on.
func (s *Session) GoAway() {
	s.mu.Lock()
	if s.goAwaySent {
		s.mu.Unlock()
		return
	}
	s.goAwaySent = true
	s.mu.Unlock()
	s.sendCtl(header{typ: typeGoAway}, nil)
}

// Close tears the session down and resets every stream.
func (s *Session) Close() error {
	s.fail(ErrSessionClosed)
	return nil
}

func (s *Session) Done() <-chan struct{} { return s.done }

// PeerGoAway is closed once the peer asks for no new streams here.
func (s *Session) PeerGoAway() <-chan struct{} { return s.goAwayc }

// Draining reports whether either side has asked to move new streams to a
// fresh connection; existing streams are unaffected.
func (s *Session) Draining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.goAwaySent || s.goAwayRecv || s.drain
}

// Err is the reason the session ended (nil while it is alive).
func (s *Session) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

func (s *Session) isDone() bool { return isClosed(s.done) }

func (s *Session) NumStreams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

func (s *Session) RTT() time.Duration { return time.Duration(s.rtt.Load()) }

func (s *Session) LocalAddr() net.Addr  { return s.conn.LocalAddr() }
func (s *Session) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }

func (s *Session) Stats() Stats {
	s.mu.Lock()
	n := len(s.streams)
	s.mu.Unlock()
	return Stats{
		Streams: n, Opened: s.opened.Load(), Accepted: s.accepted.Load(), Resets: s.resets.Load(),
		BytesIn: s.bytesIn.Load(), BytesOut: s.bytesOut.Load(), GrowUsed: s.growUsed.Load(), RTT: s.RTT(),
		Refused: s.refused.Load(), WindowGrowth: s.growths.Load(),
	}
}

func (s *Session) fail(err error) {
	s.errOnce.Do(func() {
		s.err = err
		close(s.done)
		s.conn.Close()
		s.mu.Lock()
		all := make([]*Stream, 0, len(s.streams))
		for _, st := range s.streams {
			all = append(all, st)
		}
		s.streams = map[uint32]*Stream{}
		s.mu.Unlock()
		for _, st := range all {
			st.abort(err)
		}
	})
}

// --- sending ---

const maxCtl = 8192 // peer-triggered replies we will queue before giving up

func (s *Session) sendCtl(h header, payload []byte) {
	pb := getBuf(headerLen + len(payload))
	h.length = uint16(len(payload))
	h.put(pb.b)
	copy(pb.b[headerLen:], payload)
	s.qmu.Lock()
	if len(s.ctl) >= maxCtl {
		s.qmu.Unlock()
		putBuf(pb)
		s.fail(fmt.Errorf("%w: control queue overflow", ErrProtocol))
		return
	}
	s.ctl = append(s.ctl, outFrame{pb, headerLen + len(payload)})
	s.qmu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// sendData queues a DATA/OPEN frame, blocking while the writer is behind. cancel
// (a stream deadline) can interrupt the wait.
func (s *Session) sendData(f outFrame, cancel <-chan struct{}) error {
	select {
	case s.loq <- f:
		return nil
	case <-s.done:
		putBuf(f.pb)
		return s.Err()
	case <-cancel:
		putBuf(f.pb)
		return timeoutErr{}
	}
}

func (s *Session) writeLoop() {
	buf := make([]byte, 0, 64<<10)
	for {
		var f outFrame
		haveData := false
		select {
		case f = <-s.loq:
			haveData = true
		case <-s.kick:
		case <-s.done:
			return
		}
		buf = buf[:0]
		s.qmu.Lock()
		for _, c := range s.ctl {
			buf = append(buf, c.pb.b[:c.n]...)
			putBuf(c.pb)
		}
		s.ctl = s.ctl[:0]
		s.qmu.Unlock()
		if haveData {
			buf = append(buf, f.pb.b[:f.n]...)
			putBuf(f.pb)
		}
		// batch whatever else is already waiting into the same write
		for len(buf)+headerLen+s.maxOut <= cap(buf) {
			select {
			case f = <-s.loq:
				buf = append(buf, f.pb.b[:f.n]...)
				putBuf(f.pb)
				continue
			default:
			}
			break
		}
		if len(buf) == 0 {
			continue
		}
		_ = s.conn.SetWriteDeadline(time.Now().Add(s.cfg.Timeout))
		if _, err := s.conn.Write(buf); err != nil {
			s.fail(err)
			return
		}
	}
}

func (s *Session) pingLoop() {
	t := time.NewTicker(s.cfg.KeepAlive)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			var p [8]byte
			binary.BigEndian.PutUint64(p[:], uint64(time.Now().UnixNano()))
			s.sendCtl(header{typ: typePing}, p[:])
			s.reapLingering()
		case <-s.done:
			return
		}
	}
}

// reapLingering resets streams the app closed whose peer never sent its FIN.
func (s *Session) reapLingering() {
	s.mu.Lock()
	all := make([]*Stream, 0, len(s.streams))
	for _, st := range s.streams {
		all = append(all, st)
	}
	s.mu.Unlock()
	now := time.Now()
	for _, st := range all {
		if st.lingerExpired(now, s.cfg.Linger) {
			st.Reset()
		}
	}
}

// --- receiving ---

func (s *Session) readLoop(br *bufio.Reader) {
	var hb [headerLen]byte
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(s.cfg.Timeout))
		if _, err := io.ReadFull(br, hb[:]); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				err = ErrTimeout
			}
			s.fail(err)
			return
		}
		h := parseHeader(hb[:])
		if err := h.validate(s.cfg.MaxFrame); err != nil {
			s.fail(err)
			return
		}
		var pb *pbuf
		if h.length > 0 {
			pb = getBuf(int(h.length))
			if _, err := io.ReadFull(br, pb.b[:h.length]); err != nil {
				putBuf(pb)
				s.fail(err)
				return
			}
		}
		if err := s.handle(h, pb); err != nil {
			s.fail(err)
			return
		}
	}
}

// handle owns pb. It must never block: control replies go through the
// unbounded-ish ctl queue, data lands in per-stream buffers.
func (s *Session) handle(h header, pb *pbuf) error {
	switch h.typ {
	case typeSettings:
		putBuf(pb)
		return fmt.Errorf("%w: settings sent twice", ErrProtocol)
	case typePing:
		s.sendCtl(header{typ: typePong}, pb.b[:8])
		putBuf(pb)
	case typePong:
		sent := int64(binary.BigEndian.Uint64(pb.b[:8]))
		putBuf(pb)
		if d := time.Now().UnixNano() - sent; d > 0 {
			old := s.rtt.Load()
			if old == 0 {
				s.rtt.Store(d)
			} else {
				s.rtt.Store((old*7 + d) / 8)
			}
		}
	case typeGoAway:
		s.mu.Lock()
		first := !s.goAwayRecv
		s.goAwayRecv = true
		s.mu.Unlock()
		if first {
			close(s.goAwayc)
		}
	case typeOpen:
		return s.handleOpen(h, pb)
	case typeData:
		st := s.lookup(h.stream)
		if st == nil {
			putBuf(pb)
			return nil // late frame for a stream we already closed
		}
		st.onData(pb, int(h.length), h.flags&flagFIN != 0)
	case typeCredit:
		n := binary.BigEndian.Uint32(pb.b[:4])
		putBuf(pb)
		if st := s.lookup(h.stream); st != nil {
			st.onCredit(n)
		}
	case typeReset:
		putBuf(pb)
		if st := s.lookup(h.stream); st != nil {
			s.resets.Add(1)
			st.abort(ErrStreamReset)
		}
	}
	return nil
}

func (s *Session) handleOpen(h header, pb *pbuf) error {
	mlen := int(pb.b[0])
	if 1+mlen > int(h.length) {
		putBuf(pb)
		return errBadFrame
	}
	// parity: the peer opens odd ids iff it is the client
	if (h.stream&1 == 1) != !s.client {
		putBuf(pb)
		return fmt.Errorf("%w: peer opened stream %d with the wrong parity", ErrProtocol, h.stream)
	}
	first := int(h.length) - 1 - mlen
	if first > int(s.cfg.Window) {
		putBuf(pb)
		return fmt.Errorf("%w: open carries %d bytes, window is %d", ErrProtocol, first, s.cfg.Window)
	}
	meta := append([]byte(nil), pb.b[1:1+mlen]...)

	s.mu.Lock()
	if _, dup := s.streams[h.stream]; dup {
		s.mu.Unlock()
		putBuf(pb)
		return fmt.Errorf("%w: stream %d opened twice", ErrProtocol, h.stream)
	}
	if s.theirs >= s.cfg.MaxStreams || s.goAwaySent {
		s.mu.Unlock()
		putBuf(pb)
		s.refused.Add(1)
		s.sendCtl(header{typ: typeReset, stream: h.stream}, []byte{ResetRefused})
		return nil
	}
	st := s.newStream(h.stream, meta, int(s.peer.window), int(s.cfg.Window))
	st.recvCredit = int(s.cfg.Window) - first
	s.streams[h.stream] = st
	s.theirs++
	s.mu.Unlock()

	if first > 0 {
		st.mu.Lock()
		st.queueLocked(pb, 1+mlen, first)
		st.buffered = first
		st.mu.Unlock()
	} else {
		putBuf(pb)
	}
	if h.flags&flagFIN != 0 {
		st.mu.Lock()
		st.remoteFIN = true
		st.mu.Unlock()
	}
	select {
	case s.acceptq <- st:
		s.accepted.Add(1)
	default:
		s.refused.Add(1)
		st.abort(ErrStreamReset)
		s.sendCtl(header{typ: typeReset, stream: h.stream}, []byte{ResetRefused})
	}
	return nil
}

func (s *Session) lookup(id uint32) *Stream {
	s.mu.Lock()
	st := s.streams[id]
	s.mu.Unlock()
	return st
}

// remove forgets a finished stream and returns the window growth it held.
func (s *Session) remove(st *Stream, window int) {
	s.mu.Lock()
	if cur, ok := s.streams[st.id]; ok && cur == st {
		delete(s.streams, st.id)
		if (st.id&1 == 1) == s.client {
			s.ours--
		} else {
			s.theirs--
		}
		if g := window - int(s.cfg.Window); g > 0 {
			s.growUsed.Add(^uint32(g - 1))
		}
	}
	s.mu.Unlock()
}

// grow asks for delta more bytes of window for one stream, within the pool.
func (s *Session) grow(delta uint32) bool {
	for {
		used := s.growUsed.Load()
		if uint64(used)+uint64(delta) > uint64(s.cfg.GrowBudget) {
			return false
		}
		if s.growUsed.CompareAndSwap(used, used+delta) {
			s.growths.Add(1)
			return true
		}
	}
}
