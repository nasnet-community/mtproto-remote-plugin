package daemon

import (
	"errors"
	"net"
	"sync"
	"time"
)

// ports is the link's listener while it moves: the old port keeps serving until
// the panel has reached the new one and says so.
type ports struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once

	mu   sync.Mutex
	lns  map[int]net.Listener
	addr net.Addr
}

func newPorts(port int, ln net.Listener) *ports {
	p := &ports{conns: make(chan net.Conn), done: make(chan struct{}), lns: map[int]net.Listener{}, addr: ln.Addr()}
	p.add(port, ln)
	return p
}

func (p *ports) add(port int, ln net.Listener) {
	p.mu.Lock()
	if old := p.lns[port]; old != nil {
		old.Close()
	}
	p.lns[port] = ln
	p.mu.Unlock()
	go p.accept(ln)
}

func (p *ports) has(port int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lns[port] != nil
}

// drop closes one port; connections it accepted carry on.
func (p *ports) drop(port int) {
	p.mu.Lock()
	ln := p.lns[port]
	delete(p.lns, port)
	p.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
}

func (p *ports) accept(ln net.Listener) {
	var backoff time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// out of fds and the like: wait it out, as the REALITY server does
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			select {
			case <-time.After(backoff):
				continue
			case <-p.done:
				return
			}
		}
		backoff = 0
		select {
		case p.conns <- c:
		case <-p.done:
			c.Close()
			return
		}
	}
}

func (p *ports) Accept() (net.Conn, error) {
	select {
	case c := <-p.conns:
		return c, nil
	case <-p.done:
		return nil, net.ErrClosed
	}
}

func (p *ports) Close() error {
	p.once.Do(func() {
		close(p.done)
		p.mu.Lock()
		for port, ln := range p.lns {
			ln.Close()
			delete(p.lns, port)
		}
		p.mu.Unlock()
	})
	return nil
}

func (p *ports) Addr() net.Addr { return p.addr }
