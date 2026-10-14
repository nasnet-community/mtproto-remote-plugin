// Package link joins the two ends of the tunnel: the agent on the VPS accepts
// REALITY connections from the hub (the router always dials), opens a mux
// stream per Telegram connection, and the hub dials the datacentre from its
// own egress. The same mux carries the hub's control calls the other way.
package link

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// copyBuf is only held while bytes are actually moving. An idle stream parks in
// waitReadable with no buffer, which is what keeps thousands of them cheap.
var copyBufs = sync.Pool{New: func() any { b := make([]byte, 16<<10); return &b }}

type readWaiter interface{ WaitReadable() error }

// waitReadable returns once a Read on c would not block.
func waitReadable(c net.Conn) error {
	if w, ok := c.(readWaiter); ok {
		return w.WaitReadable()
	}
	return waitFD(c)
}

type closeWriter interface{ CloseWrite() error }
type resetter interface{ Reset() }

func abort(c net.Conn) {
	if r, ok := c.(resetter); ok {
		r.Reset()
		return
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetLinger(0)
	}
	c.Close()
}

func finish(c net.Conn) {
	if cw, ok := c.(closeWriter); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}

// copyOne moves src to dst until EOF or error, counting bytes.
func copyOne(dst, src net.Conn, n *atomic.Int64) error {
	for {
		if err := waitReadable(src); err != nil {
			return err
		}
		bp := copyBufs.Get().(*[]byte)
		nr, rerr := src.Read(*bp)
		if nr > 0 {
			nw, werr := dst.Write((*bp)[:nr])
			n.Add(int64(nw))
			if werr != nil {
				copyBufs.Put(bp)
				return werr
			}
		}
		copyBufs.Put(bp)
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return rerr
		}
	}
}

// Pipe relays a and b both ways with TCP-style half-close: an EOF on one side
// becomes a FIN on the other, and any error resets both. It returns the byte
// counts (a→b, b→a) once both directions are finished.
func Pipe(a, b net.Conn) (ab, ba int64) {
	var nab, nba atomic.Int64
	var wg sync.WaitGroup
	var failed atomic.Bool
	run := func(dst, src net.Conn, n *atomic.Int64) {
		defer wg.Done()
		if err := copyOne(dst, src, n); err != nil {
			if failed.CompareAndSwap(false, true) {
				abort(dst)
				abort(src)
			}
			return
		}
		finish(dst)
	}
	// the caller's own goroutine carries one direction: a goroutine parked in
	// wg.Wait would cost a stack per stream for nothing
	wg.Add(2)
	go run(b, a, &nab)
	run(a, b, &nba)
	wg.Wait()
	if !failed.Load() {
		a.Close()
		b.Close()
	}
	return nab.Load(), nba.Load()
}
