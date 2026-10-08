package mux

import (
	"os"
	"sync"
	"time"
)

// deadline is a resettable timer exposed as a channel, the same shape as the
// one inside net.Pipe: wait() closes when the deadline passes.
type deadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel chan struct{}
}

func newDeadline() *deadline { return &deadline{cancel: make(chan struct{})} }

func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil && !d.timer.Stop() {
		<-d.cancel // timer fired: wait for the close to land
	}
	d.timer = nil
	closed := isClosed(d.cancel)
	if t.IsZero() {
		if closed {
			d.cancel = make(chan struct{})
		}
		return
	}
	if dur := time.Until(t); dur > 0 {
		if closed {
			d.cancel = make(chan struct{})
		}
		d.timer = time.AfterFunc(dur, func() { close(d.cancel) })
		return
	}
	if !closed {
		close(d.cancel)
	}
}

func (d *deadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// timeoutErr satisfies net.Error with Timeout() true and unwraps to
// os.ErrDeadlineExceeded, like a real conn.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "mux: i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
func (timeoutErr) Unwrap() error   { return os.ErrDeadlineExceeded }
