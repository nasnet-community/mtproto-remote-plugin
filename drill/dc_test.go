package drill

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeDC speaks the datacentre half of obfuscated2 and echoes whatever it
// decrypts, so a frame comes back in whatever transport telemt chose.
type fakeDC struct {
	ln net.Listener

	mu       sync.Mutex
	accepted []net.Addr // remote address of every connection
	tags     []uint32
	bad      int             // connections whose init did not decode
	seen     []*bytes.Buffer // what each decoded connection carried (capped), in accept order
	seenFrom []net.Addr
}

// seenCap bounds what is kept per connection: enough to find a test message.
const seenCap = 64 << 10

type capped struct{ b *bytes.Buffer }

func (c capped) Write(p []byte) (int, error) {
	if room := seenCap - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func newFakeDC(t *testing.T, addr string) *fakeDC {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDC{ln: ln}
	go d.serve()
	t.Cleanup(func() { ln.Close() })
	return d
}

func (d *fakeDC) serve() {
	for {
		c, err := d.ln.Accept()
		if err != nil {
			return
		}
		go d.handle(c)
	}
}

func (d *fakeDC) handle(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	s, err := o2Accept(c, nil)
	d.mu.Lock()
	d.accepted = append(d.accepted, c.RemoteAddr())
	if err != nil {
		d.bad++
	} else {
		d.tags = append(d.tags, s.Tag)
	}
	d.mu.Unlock()
	if err != nil {
		return
	}
	buf := &bytes.Buffer{}
	d.mu.Lock()
	d.seen, d.seenFrom = append(d.seen, buf), append(d.seenFrom, c.RemoteAddr())
	d.mu.Unlock()
	c.SetReadDeadline(time.Time{})
	io.Copy(s, io.TeeReader(s, lockedWriter{&d.mu, capped{buf}}))
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// carrying lists the connections whose stream contained msg: the test's own
// traffic, told apart from whatever telemt dials by itself meanwhile.
func (d *fakeDC) carrying(msg []byte) []net.Addr {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []net.Addr
	for i, b := range d.seen {
		if bytes.Contains(b.Bytes(), msg) {
			out = append(out, d.seenFrom[i])
		}
	}
	return out
}

func (d *fakeDC) snapshot() (accepted []net.Addr, bad int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]net.Addr(nil), d.accepted...), d.bad
}

func TestFakeDCEchoes(t *testing.T) {
	d := newFakeDC(t, "127.0.0.1:0")
	c, err := net.Dial("tcp", d.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	s, err := o2Dial(c, tagIntermediate, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := bytes.Repeat([]byte{7}, 40)
	if err := writeFrame(s, msg); err != nil {
		t.Fatal(err)
	}
	if got, err := readFrame(s, 1024); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("echo %x %v", got, err)
	}
	if acc, bad := d.snapshot(); len(acc) != 1 || bad != 0 {
		t.Fatalf("accepted %v bad %d", acc, bad)
	}
	if got := d.carrying(msg); len(got) != 1 || got[0].String() != c.LocalAddr().String() {
		t.Fatalf("carrying = %v, want this connection", got)
	}
}
