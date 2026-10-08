// Package supervise keeps telemt running. The branch's heavy agent only restarts
// telemt on its next push; this one is a real watchdog: restart on exit with
// backoff, and report a crash loop so the hub can alert.
package supervise

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type Spec struct {
	Bin      string
	Args     []string
	Dir      string
	Env      []string
	UID, GID uint32 // run as this user (0 = inherit); capabilities are kept for the low port
	Out      io.Writer

	MinBackoff  time.Duration // default 1 s
	MaxBackoff  time.Duration // default 60 s
	StableAfter time.Duration // a run this long resets the backoff and the crash count (default 2 min)
	StopGrace   time.Duration // SIGTERM to SIGKILL (default 3 s)
}

type Status struct {
	Running   bool
	PID       int
	Since     time.Time // when the current run started
	Restarts  int       // runs after the first, lifetime
	LastExit  string
	CrashLoop bool // several quick exits in a row
}

// crashLoopAt quick exits in a row mark the loop.
const crashLoopAt = 5

type Proc struct {
	spec Spec

	mu      sync.Mutex
	cmd     *exec.Cmd
	exited  chan struct{} // closed when cmd has been reaped
	st      Status
	enabled bool
	quick   int // consecutive runs shorter than StableAfter
	restart chan struct{}
	wake    chan struct{}
}

var errDisabled = errors.New("supervise: disabled")

func New(spec Spec) *Proc {
	if spec.MinBackoff == 0 {
		spec.MinBackoff = time.Second
	}
	if spec.MaxBackoff == 0 {
		spec.MaxBackoff = time.Minute
	}
	if spec.StableAfter == 0 {
		spec.StableAfter = 2 * time.Minute
	}
	if spec.StopGrace == 0 {
		spec.StopGrace = 3 * time.Second
	}
	// off until the agent has the capture rules and a config in place
	return &Proc{spec: spec, restart: make(chan struct{}, 1), wake: make(chan struct{}, 1)}
}

func (p *Proc) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	// a run that has lasted long enough is healthy even though it has not ended
	if p.st.Running && time.Since(p.st.Since) >= p.spec.StableAfter {
		p.quick, p.st.CrashLoop = 0, false
	}
	return p.st
}

// SetEnabled(false) stops telemt and keeps it stopped (telemt refuses to start
// with no users); true lets the watchdog start it again.
func (p *Proc) SetEnabled(on bool) {
	p.mu.Lock()
	p.enabled = on
	cmd, exited := p.cmd, p.exited
	p.mu.Unlock()
	if !on && cmd != nil {
		p.terminate(cmd, exited)
	}
	p.poke()
}

// Stop disables telemt and returns once it has exited, so its rules can go right after.
func (p *Proc) Stop() error {
	p.mu.Lock()
	p.enabled = false
	cmd, exited := p.cmd, p.exited
	p.mu.Unlock()
	p.poke()
	if cmd == nil {
		return nil
	}
	p.terminate(cmd, exited)
	select {
	case <-exited:
		return nil
	case <-time.After(p.spec.StopGrace + 5*time.Second):
		return fmt.Errorf("supervise: telemt (pid %d) did not exit", cmd.Process.Pid)
	}
}

// Restart bounces telemt now; it is not counted as a crash.
func (p *Proc) Restart() {
	p.mu.Lock()
	cmd, exited := p.cmd, p.exited
	p.mu.Unlock()
	select {
	case p.restart <- struct{}{}:
	default:
	}
	if cmd != nil {
		p.terminate(cmd, exited)
	}
	p.poke()
}

func (p *Proc) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// terminate asks politely, then kills once the grace is up.
func (p *Proc) terminate(cmd *exec.Cmd, exited <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	go func() {
		select {
		case <-exited:
		case <-time.After(p.spec.StopGrace):
			_ = cmd.Process.Kill()
		}
	}()
}

// Run supervises until ctx ends, then stops telemt.
func (p *Proc) Run(ctx context.Context) {
	backoff := p.spec.MinBackoff
	for ctx.Err() == nil {
		p.mu.Lock()
		on := p.enabled
		p.mu.Unlock()
		if !on {
			select {
			case <-p.wake:
			case <-ctx.Done():
			}
			continue
		}
		started := time.Now()
		err := p.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errDisabled) {
			continue // disabled between the check above and the start
		}
		ran := time.Since(started)

		requested := false
		select {
		case <-p.restart:
			requested = true
		default:
		}
		p.mu.Lock()
		p.st.LastExit = exitText(err)
		if !requested && p.enabled {
			if ran >= p.spec.StableAfter {
				p.quick, backoff, p.st.CrashLoop = 0, p.spec.MinBackoff, false
			} else {
				p.quick++
				p.st.CrashLoop = p.quick >= crashLoopAt
			}
		}
		p.mu.Unlock()
		if requested || !p.isEnabled() {
			continue
		}
		select {
		case <-time.After(backoff):
		case <-p.restart: // a Restart during the wait skips it
		case <-p.wake:
		case <-ctx.Done():
			return
		}
		if backoff *= 2; backoff > p.spec.MaxBackoff {
			backoff = p.spec.MaxBackoff
		}
	}
}

func (p *Proc) isEnabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enabled
}

func (p *Proc) runOnce(ctx context.Context) error {
	cmd := exec.Command(p.spec.Bin, p.spec.Args...)
	cmd.Dir = p.spec.Dir
	cmd.Env = append(os.Environ(), p.spec.Env...)
	cmd.Stdout, cmd.Stderr = p.spec.Out, p.spec.Out
	setAttrs(cmd, p.spec.UID, p.spec.GID)
	// checked and started under the lock, so a Stop cannot slip in between
	p.mu.Lock()
	if !p.enabled {
		p.mu.Unlock()
		return errDisabled
	}
	if err := cmd.Start(); err != nil {
		p.mu.Unlock()
		return err
	}
	first := p.st.Since.IsZero()
	exited := make(chan struct{})
	p.cmd, p.exited = cmd, exited
	p.st.Running, p.st.PID, p.st.Since = true, cmd.Process.Pid, time.Now()
	if !first {
		p.st.Restarts++
	}
	p.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		p.terminate(cmd, exited)
		err = <-done
	}
	p.mu.Lock()
	p.cmd, p.exited = nil, nil
	p.st.Running, p.st.PID = false, 0
	p.mu.Unlock()
	close(exited)
	return err
}

func exitText(err error) string {
	if err == nil {
		return "exited"
	}
	return fmt.Sprint(err)
}
