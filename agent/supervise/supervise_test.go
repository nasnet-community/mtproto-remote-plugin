package supervise

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-telemt")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func run(t *testing.T, spec Spec) (*Proc, context.CancelFunc) {
	t.Helper()
	p, cancel := idle(t, spec)
	p.SetEnabled(true)
	return p, cancel
}

// idle runs the watchdog without enabling telemt, as the agent boots.
func idle(t *testing.T, spec Spec) (*Proc, context.CancelFunc) {
	t.Helper()
	p := New(spec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return p, cancel
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func TestStartsAndStopsWithContext(t *testing.T) {
	p, cancel := run(t, Spec{Bin: script(t, "exec sleep 600")})
	waitFor(t, func() bool { return p.Status().Running }, "start")
	pid := p.Status().PID
	cancel()
	waitFor(t, func() bool { return !alive(pid) }, "the child to be killed with the agent")
}

func TestRestartsACrashedChildWithBackoff(t *testing.T) {
	p, _ := run(t, Spec{Bin: script(t, "exit 3"), MinBackoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond})
	waitFor(t, func() bool { return p.Status().Restarts >= 3 }, "three restarts")
	if !strings.Contains(p.Status().LastExit, "exit status 3") {
		t.Errorf("last exit = %q", p.Status().LastExit)
	}
}

func TestCrashLoopIsFlaggedAndClearsOnceStable(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stable")
	// crashes until the marker exists, then runs forever
	bin := script(t, "[ -e "+marker+" ] && exec sleep 600; exit 1")
	p, _ := run(t, Spec{Bin: bin, MinBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond, StableAfter: 300 * time.Millisecond})
	waitFor(t, func() bool { return p.Status().CrashLoop }, "the crash loop flag")
	os.WriteFile(marker, nil, 0o644)
	waitFor(t, func() bool { s := p.Status(); return s.Running && !s.CrashLoop }, "recovery to clear the flag")
}

func TestRestartIsImmediateAndNotACrash(t *testing.T) {
	p, _ := run(t, Spec{Bin: script(t, "exec sleep 600"), MinBackoff: time.Hour})
	waitFor(t, func() bool { return p.Status().Running }, "start")
	first := p.Status().PID
	p.Restart()
	waitFor(t, func() bool { s := p.Status(); return s.Running && s.PID != first }, "a new pid")
	if p.Status().CrashLoop {
		t.Error("a requested restart counted as a crash")
	}
	if alive(first) {
		t.Error("old process still alive")
	}
}

func TestDisableKeepsItStoppedAndEnableBringsItBack(t *testing.T) {
	p, _ := run(t, Spec{Bin: script(t, "exec sleep 600"), MinBackoff: 5 * time.Millisecond})
	waitFor(t, func() bool { return p.Status().Running }, "start")
	pid := p.Status().PID
	p.SetEnabled(false)
	waitFor(t, func() bool { return !p.Status().Running && !alive(pid) }, "stop")
	time.Sleep(150 * time.Millisecond)
	if p.Status().Running {
		t.Fatal("watchdog restarted a disabled child")
	}
	p.SetEnabled(true)
	waitFor(t, func() bool { return p.Status().Running }, "start after enabling")
}

func TestOutputGoesToTheWriter(t *testing.T) {
	var mu sync.Mutex
	var b strings.Builder
	w := writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); b.Write(p); return len(p), nil })
	run(t, Spec{Bin: script(t, "echo hello from telemt; exec sleep 600"), Out: w})
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return strings.Contains(b.String(), "hello from telemt") }, "output")
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Nothing runs until the agent has put the capture rules and the config in
// place and says so: a fresh watchdog waits.
func TestStartsDisabledUntilEnabled(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	p, _ := idle(t, Spec{Bin: script(t, "touch "+marker+"; exec sleep 600"), MinBackoff: 5 * time.Millisecond})
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil || p.Status().Running {
		t.Fatal("telemt started before anyone enabled it")
	}
	p.SetEnabled(true)
	waitFor(t, func() bool { return p.Status().Running }, "start after enabling")
}

// Stop returns only once telemt is gone, killing one that ignores SIGTERM: the
// uninstall removes the rules and the user right after it.
func TestStopWaitsForTheExitAndKillsAfterTheGrace(t *testing.T) {
	// the child says when its trap is set; a SIGTERM before that would kill it at once
	ready := filepath.Join(t.TempDir(), "ready")
	p, _ := run(t, Spec{Bin: script(t, "trap '' TERM; : > '"+ready+"'; while :; do sleep 0.05; done"), StopGrace: 300 * time.Millisecond})
	waitFor(t, func() bool { _, err := os.Stat(ready); return err == nil && p.Status().Running }, "start")
	pid := p.Status().PID
	start := time.Now()
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	if alive(pid) || p.Status().Running {
		t.Fatal("Stop returned with telemt still running")
	}
	if took := time.Since(start); took < 250*time.Millisecond {
		t.Errorf("a SIGTERM-proof child went away in %v: it was not given its grace", took)
	}
	time.Sleep(150 * time.Millisecond)
	if p.Status().Running {
		t.Fatal("the watchdog restarted a stopped telemt")
	}
	// stopping what is not running is a no-op
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
}
