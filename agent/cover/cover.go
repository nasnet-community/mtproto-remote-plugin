// Package cover takes telemt's cover-site traffic (the probes it masks and the
// certificate it copies) on a Unix socket and dials the real site itself:
// telemt's own uid may reach nothing but Telegram.
package cover

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Target is where cover traffic goes.
type Target struct {
	Primary string   // the cover site
	Domains []string // every cover name; a ClientHello naming one is sent there
	Host    string   // the tunnel's mask host: when set, everything goes there
	Port    int      // 443 when zero
}

// addr is the site for a ClientHello's name; a stranger's name is never dialled.
func (t Target) addr(sni string) string {
	port := t.Port
	if port == 0 {
		port = 443
	}
	host := t.Host
	if host == "" {
		host = t.Primary
		for _, d := range t.Domains {
			if sni != "" && strings.EqualFold(d, sni) {
				host = strings.ToLower(d)
				break
			}
		}
	}
	if host == "" {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// Forwarder serves the socket. The zero value works once Set has named a site.
type Forwarder struct {
	Dial  func(ctx context.Context, addr string) (net.Conn, error) // net.Dialer by default
	Max   int                                                      // live forwards (256)
	Hello time.Duration                                            // to read a ClientHello (5 s)

	mu     sync.RWMutex
	target Target
	live   atomic.Int64
}

func (f *Forwarder) Set(t Target) {
	f.mu.Lock()
	f.target = t
	f.mu.Unlock()
}

// Live is how many connections are being forwarded now.
func (f *Forwarder) Live() int64 { return f.live.Load() }

// Listen opens the socket for telemt: a stale one is replaced, and only root
// and gid may connect (gid < 0 leaves the group alone).
func Listen(path string, gid int) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	if gid >= 0 && os.Geteuid() == 0 {
		if err := os.Chown(path, 0, gid); err != nil {
			ln.Close()
			return nil, err
		}
	}
	return ln, nil
}

func (f *Forwarder) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go f.handle(c)
	}
}

func (f *Forwarder) handle(c net.Conn) {
	defer c.Close()
	max := f.Max
	if max == 0 {
		max = 256
	}
	// probers decide how many of these there are: past the bound, refuse
	if f.live.Add(1) > int64(max) {
		f.live.Add(-1)
		return
	}
	defer f.live.Add(-1)
	wait := f.Hello
	if wait == 0 {
		wait = 5 * time.Second
	}
	sni, first := readHello(c, wait)
	f.mu.RLock()
	addr := f.target.addr(sni)
	f.mu.RUnlock()
	if addr == "" {
		return
	}
	dial := f.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		dial = func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	up, err := dial(ctx, addr)
	cancel()
	if err != nil {
		return
	}
	defer up.Close()
	if len(first) > 0 {
		if _, err := up.Write(first); err != nil {
			return
		}
	}
	// telemt bounds each relay itself; this only stops one that never ends
	deadline := time.Now().Add(10 * time.Minute)
	c.SetDeadline(deadline)
	up.SetDeadline(deadline)
	splice(c, up)
}

// recorder keeps what the TLS server reads, so it can be replayed to the site,
// and says nothing back: the site answers.
type recorder struct {
	net.Conn
	buf bytes.Buffer
}

func (r *recorder) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	r.buf.Write(p[:n])
	return n, err
}

func (r *recorder) Write(p []byte) (int, error) { return len(p), nil }

var errSeen = errors.New("cover: hello read")

// readHello reads up to the end of a ClientHello and returns its server name
// with every byte read; anything that isn't one comes back with no name.
func readHello(c net.Conn, wait time.Duration) (string, []byte) {
	rec := &recorder{Conn: c}
	var sni string
	c.SetReadDeadline(time.Now().Add(wait))
	_ = tls.Server(rec, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			sni = h.ServerName
			return nil, errSeen
		},
	}).Handshake()
	c.SetReadDeadline(time.Time{})
	return sni, rec.buf.Bytes()
}

// splice copies both ways; each side's end of input half-closes the other.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}
