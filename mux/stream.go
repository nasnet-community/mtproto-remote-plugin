package mux

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"
)

type chunk struct {
	pb  *pbuf
	off int
	n   int // bytes left in this chunk
}

// Stream is one multiplexed byte stream. It implements net.Conn, so it plugs
// into io.Copy and friends; CloseWrite gives TCP-style half-close.
type Stream struct {
	s    *Session
	id   uint32
	meta []byte

	mu sync.Mutex
	// receive side
	chunks     []chunk
	buffered   int       // bytes queued, unread
	recvCredit int       // bytes the peer may still send
	window     int       // current target window
	pending    int       // consumed since the last CREDIT
	lastGrant  time.Time // for auto-tune
	remoteFIN  bool
	// send side
	sendCredit int
	localFIN   bool
	// terminal
	dead     error // reset / session death: everything fails with this
	closed   bool  // the app called Close
	closedAt time.Time

	readReady  chan struct{}
	writeReady chan struct{}
	rd, wd     *deadline
	wmu        sync.Mutex // one Write at a time keeps frames in order
}

func (s *Session) newStream(id uint32, meta []byte, sendCredit, window int) *Stream {
	return &Stream{
		s: s, id: id, meta: meta,
		sendCredit: sendCredit,
		window:     window,
		recvCredit: window,
		lastGrant:  time.Now(),
		readReady:  make(chan struct{}, 1),
		writeReady: make(chan struct{}, 1),
		rd:         newDeadline(),
		wd:         newDeadline(),
	}
}

func (st *Stream) ID() uint32           { return st.id }
func (st *Stream) Meta() []byte         { return st.meta }
func (st *Stream) LocalAddr() net.Addr  { return st.s.conn.LocalAddr() }
func (st *Stream) RemoteAddr() net.Addr { return st.s.conn.RemoteAddr() }

func kick(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// --- receive ---

func (st *Stream) onData(pb *pbuf, n int, fin bool) {
	st.mu.Lock()
	if st.dead != nil {
		st.mu.Unlock()
		putBuf(pb)
		return
	}
	if st.closed {
		// like a closed TCP socket: data is answered with a reset, the peer's FIN
		// completes the close
		putBuf(pb)
		if n > 0 {
			st.dead = io.ErrClosedPipe
			win := st.window
			st.mu.Unlock()
			st.s.sendCtl(header{typ: typeReset, stream: st.id}, []byte{ResetCancel})
			st.s.remove(st, win)
			return
		}
		if fin {
			st.remoteFIN = true
		}
		finished, win := st.finishedLocked(), st.window
		st.mu.Unlock()
		if finished {
			st.s.remove(st, win)
		}
		return
	}
	if n > st.recvCredit {
		// the peer ignored our window: reset only this stream
		st.mu.Unlock()
		putBuf(pb)
		st.s.sendCtl(header{typ: typeReset, stream: st.id}, []byte{ResetProtocol})
		st.abort(ErrProtocol)
		return
	}
	st.recvCredit -= n
	if n > 0 {
		st.queueLocked(pb, 0, n)
		st.buffered += n
	} else {
		putBuf(pb)
	}
	if fin {
		st.remoteFIN = true
	}
	finished := st.finishedLocked()
	st.mu.Unlock()
	st.s.bytesIn.Add(uint64(n))
	kick(st.readReady)
	if finished {
		st.s.remove(st, st.curWindow())
	}
}

// queueLocked owns pb; topping up the last chunk stops tiny frames pinning a buffer each.
func (st *Stream) queueLocked(pb *pbuf, off, n int) {
	if k := len(st.chunks); k > 0 {
		t := &st.chunks[k-1]
		m := copy(t.pb.b[t.off+t.n:], pb.b[off:off+n])
		t.n += m
		off += m
		n -= m
	}
	if n == 0 {
		putBuf(pb)
		return
	}
	if off > 0 {
		copy(pb.b, pb.b[off:off+n]) // the room goes to the next small frame
	}
	st.chunks = append(st.chunks, chunk{pb: pb, n: n})
}

func (st *Stream) onCredit(n uint32) {
	st.mu.Lock()
	if int64(st.sendCredit)+int64(n) > maxWindowSetting {
		// no window is that large: the peer's accounting is broken
		st.mu.Unlock()
		st.s.sendCtl(header{typ: typeReset, stream: st.id}, []byte{ResetProtocol})
		st.abort(ErrProtocol)
		return
	}
	st.sendCredit += int(n)
	st.mu.Unlock()
	kick(st.writeReady)
}

func (st *Stream) curWindow() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.window
}

// finishedLocked: both directions are done, so the session can forget us.
func (st *Stream) finishedLocked() bool {
	return st.dead != nil || (st.localFIN && st.remoteFIN && (st.buffered == 0 || st.closed))
}

func (st *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		st.mu.Lock()
		switch {
		case st.dead != nil:
			err := st.dead
			st.mu.Unlock()
			return 0, err
		case st.closed:
			st.mu.Unlock()
			return 0, io.ErrClosedPipe
		case st.buffered > 0:
			n := st.copyOutLocked(p)
			grant, finished, win := st.afterReadLocked(n)
			st.mu.Unlock()
			if grant > 0 {
				var b [4]byte
				binary.BigEndian.PutUint32(b[:], uint32(grant))
				st.s.sendCtl(header{typ: typeCredit, stream: st.id}, b[:])
			}
			if finished {
				st.s.remove(st, win)
			}
			return n, nil
		case st.remoteFIN:
			st.mu.Unlock()
			return 0, io.EOF
		}
		st.mu.Unlock()
		select {
		case <-st.readReady:
		case <-st.rd.wait():
			return 0, timeoutErr{}
		case <-st.s.done:
			if err := st.s.Err(); err != nil {
				return 0, err
			}
		}
	}
}

// WaitReadable blocks until a Read would not block: data, EOF or an error is
// pending. It consumes nothing, so a relay loop can park here without holding a
// copy buffer.
func (st *Stream) WaitReadable() error {
	for {
		st.mu.Lock()
		ready := st.dead != nil || st.closed || st.buffered > 0 || st.remoteFIN
		st.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-st.readReady:
		case <-st.rd.wait():
			return timeoutErr{}
		case <-st.s.done:
			return nil
		}
	}
}

func (st *Stream) copyOutLocked(p []byte) int {
	n := 0
	for n < len(p) && len(st.chunks) > 0 {
		c := &st.chunks[0]
		k := copy(p[n:], c.pb.b[c.off:c.off+c.n])
		n += k
		c.off += k
		c.n -= k
		if c.n == 0 {
			putBuf(c.pb)
			st.chunks[0] = chunk{}
			st.chunks = st.chunks[1:]
			if len(st.chunks) == 0 {
				st.chunks = nil // release the backing array while idle
			}
		}
	}
	return n
}

// afterReadLocked does the credit bookkeeping for n freshly consumed bytes and
// returns how much CREDIT to send.
func (st *Stream) afterReadLocked(n int) (grant int, finished bool, window int) {
	st.buffered -= n
	st.pending += n
	if st.pending*2 >= st.window && !st.remoteFIN {
		extra := 0
		// a consumer that drains the whole window inside two round trips is
		// bandwidth-limited by the window: double it, within the pool
		if st.window < int(st.s.cfg.MaxWindow) && time.Since(st.lastGrant) < 2*st.s.RTT() {
			next := st.window * 2
			if next > int(st.s.cfg.MaxWindow) {
				next = int(st.s.cfg.MaxWindow)
			}
			if st.s.grow(uint32(next - st.window)) {
				extra = next - st.window
				st.window = next
			}
		}
		st.lastGrant = time.Now()
		grant = st.pending + extra
		st.pending = 0
		st.recvCredit += grant
	}
	return grant, st.finishedLocked(), st.window
}

// --- send ---

func (st *Stream) Write(p []byte) (int, error) {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	written := 0
	for len(p) > 0 {
		st.mu.Lock()
		switch {
		case st.dead != nil:
			err := st.dead
			st.mu.Unlock()
			return written, err
		case st.closed || st.localFIN:
			st.mu.Unlock()
			return written, io.ErrClosedPipe
		}
		n := len(p)
		if n > st.sendCredit {
			n = st.sendCredit
		}
		if n > st.s.maxOut {
			n = st.s.maxOut
		}
		if n <= 0 {
			st.mu.Unlock()
			select {
			case <-st.writeReady:
			case <-st.wd.wait():
				return written, timeoutErr{}
			case <-st.s.done:
				return written, st.s.Err()
			}
			continue
		}
		st.sendCredit -= n
		st.mu.Unlock()

		pb := getBuf(headerLen + n)
		header{typ: typeData, stream: st.id, length: uint16(n)}.put(pb.b)
		copy(pb.b[headerLen:], p[:n])
		if err := st.s.sendData(outFrame{pb, headerLen + n}, st.wd.wait()); err != nil {
			// the frame never went out, so neither did the credit
			st.mu.Lock()
			st.sendCredit += n
			st.mu.Unlock()
			kick(st.writeReady)
			return written, err
		}
		st.s.bytesOut.Add(uint64(n))
		written += n
		p = p[n:]
	}
	return written, nil
}

// CloseWrite sends FIN: no more data from us, but we keep reading.
func (st *Stream) CloseWrite() error {
	st.mu.Lock()
	if st.dead != nil || st.closed || st.localFIN {
		st.mu.Unlock()
		return nil
	}
	st.localFIN = true
	finished := st.finishedLocked()
	win := st.window
	st.mu.Unlock()
	kick(st.writeReady) // a Write blocked on credit holds wmu; make it let go
	err := st.sendFIN()
	if finished {
		st.s.remove(st, win)
	}
	return err
}

// sendFIN queues FIN behind every DATA frame this stream already queued: it
// goes through the same FIFO, and wmu waits out a Write that is mid-flight.
func (st *Stream) sendFIN() error {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	pb := getBuf(headerLen)
	header{typ: typeData, flags: flagFIN, stream: st.id}.put(pb.b)
	return st.s.sendData(outFrame{pb, headerLen}, nil)
}

// sendRST goes through the same FIFO as OPEN so it can never overtake the open.
func (st *Stream) sendRST(reason byte) {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	pb := getBuf(headerLen + 1)
	header{typ: typeReset, stream: st.id, length: 1}.put(pb.b)
	pb.b[headerLen] = reason
	_ = st.s.sendData(outFrame{pb, headerLen + 1}, nil)
}

// Close ends the stream the way closing a TCP socket does. With nothing unread
// it sends FIN and waits (up to Config.Linger) for the peer's FIN; with unread
// data it resets. Data that arrives after Close is answered with a reset.
func (st *Stream) Close() error {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return nil
	}
	st.closed = true
	st.closedAt = time.Now()
	if st.dead != nil {
		st.mu.Unlock()
		kick(st.readReady)
		kick(st.writeReady)
		return nil
	}
	unread := st.buffered > 0
	st.freeLocked()
	needFIN := !st.localFIN
	st.localFIN = true
	if unread {
		st.dead = io.ErrClosedPipe
	}
	finished, win := st.finishedLocked(), st.window
	st.mu.Unlock()
	kick(st.readReady)
	kick(st.writeReady) // a Write blocked on credit must notice and let go of wmu
	switch {
	case unread:
		st.sendRST(ResetCancel)
	case needFIN:
		_ = st.sendFIN()
	}
	if finished {
		st.s.remove(st, win)
	}
	return nil
}

// Reset aborts the stream: the peer's reads and writes fail with ErrStreamReset.
func (st *Stream) Reset() {
	st.mu.Lock()
	if st.dead != nil {
		st.mu.Unlock()
		return
	}
	st.dead = io.ErrClosedPipe
	win := st.window
	st.freeLocked()
	st.mu.Unlock()
	kick(st.readReady)
	kick(st.writeReady)
	st.sendRST(ResetCancel)
	st.s.remove(st, win)
}

func (st *Stream) lingerExpired(now time.Time, linger time.Duration) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.closed && st.dead == nil && now.Sub(st.closedAt) > linger
}

// abort fails the stream locally (peer reset or session death).
func (st *Stream) abort(err error) {
	st.mu.Lock()
	if st.dead == nil {
		st.dead = err
	}
	win := st.window
	st.freeLocked()
	st.mu.Unlock()
	st.s.remove(st, win)
	kick(st.readReady)
	kick(st.writeReady)
}

func (st *Stream) freeLocked() {
	for _, c := range st.chunks {
		putBuf(c.pb)
	}
	st.chunks = nil
	st.buffered = 0
}

func (st *Stream) SetDeadline(t time.Time) error {
	st.rd.set(t)
	st.wd.set(t)
	return nil
}
func (st *Stream) SetReadDeadline(t time.Time) error  { st.rd.set(t); return nil }
func (st *Stream) SetWriteDeadline(t time.Time) error { st.wd.set(t); return nil }

var _ net.Conn = (*Stream)(nil)
