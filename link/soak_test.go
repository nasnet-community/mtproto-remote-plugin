package link

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func rssKB() int {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

type mem struct {
	heap, stack, sys uint64
	goroutines       int
	rss              int
}

func measure() mem {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return mem{m.HeapAlloc, m.StackInuse, m.Sys, runtime.NumGoroutine(), rssKB()}
}

// TestSoakIdleStreams drives the real chain, minus telemt itself: a fake telemt
// connection -> the capture-style accept -> agent.OpenData -> REALITY+mux -> hub
// -> the datacentre dial. SOAK=1 enables it; SOAK_STREAMS sets the count.
func TestSoakIdleStreams(t *testing.T) {
	if os.Getenv("SOAK") == "" {
		t.Skip("set SOAK=1 to run")
	}
	n := 2500
	if v, _ := strconv.Atoi(os.Getenv("SOAK_STREAMS")); v > 0 {
		n = v
	}
	r := newRig(t, rigOpts{agentMax: n + 100, quietDC: true})
	r.hub.cfg.MaxStreams = n + 100
	r.waitConns(t, 2)

	capLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer capLn.Close()
	go func() {
		for {
			c, err := capLn.Accept()
			if err != nil {
				return
			}
			go func() {
				st, err := r.agent.OpenData(r.dcAddr, nil)
				if err != nil {
					c.Close()
					return
				}
				Pipe(c, st)
			}()
		}
	}()

	// control: the same number of bare TCP pairs, to subtract the fake ends
	ctl := measure()
	var sinkConns []net.Conn
	sl, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() {
		for {
			c, err := sl.Accept()
			if err != nil {
				return
			}
			sinkConns = append(sinkConns, c)
			go func() { var b [1]byte; c.Read(b[:]) }()
		}
	}()
	var bare []net.Conn
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", sl.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		bare = append(bare, c)
	}
	time.Sleep(500 * time.Millisecond)
	fakes := measure()
	for _, c := range bare {
		c.Close()
	}
	for _, c := range sinkConns {
		c.Close()
	}
	sl.Close()
	time.Sleep(500 * time.Millisecond)

	base := measure()
	var clients []net.Conn
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", capLn.Addr().String())
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		clients = append(clients, c)
	}
	deadline := time.Now().Add(60 * time.Second)
	for r.hub.Status().Streams < int64(n) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	idle := measure()

	// 10% of the streams trade a small message every 100 ms for 5 s
	stop := make(chan struct{})
	for i := 0; i < n/10; i++ {
		c := clients[i]
		go func() {
			msg := make([]byte, 512)
			tk := time.NewTicker(100 * time.Millisecond)
			defer tk.Stop()
			for {
				select {
				case <-tk.C:
					c.Write(msg)
				case <-stop:
					return
				}
			}
		}()
	}
	time.Sleep(5 * time.Second)
	active := measure()
	close(stop)

	kb := func(b uint64) float64 { return float64(b) / 1024 }
	t.Logf("streams: hub=%d agent=%d", r.hub.Status().Streams, r.agent.Streams())
	t.Logf("%-28s %10s %10s %10s %8s", "", "heap KB", "stack KB", "sys KB", "gorout")
	for _, row := range []struct {
		name string
		m    mem
	}{{"baseline", base}, {"bare tcp pairs (control)", fakes}, {"idle streams", idle}, {"10% active", active}} {
		t.Logf("%-28s %10.0f %10.0f %10.0f %8d   rss=%d KB", row.name, kb(row.m.heap), kb(row.m.stack), kb(row.m.sys), row.m.goroutines, row.m.rss)
	}
	ctlHeap := float64(fakes.heap) - float64(ctl.heap)
	ctlStack := float64(fakes.stack) - float64(ctl.stack)
	dHeap := float64(idle.heap) - float64(base.heap) - ctlHeap
	dStack := float64(idle.stack) - float64(base.stack) - ctlStack
	t.Logf("per idle stream, both chain ends together (fake telemt+DC conns subtracted once): heap %.2f KB + stack %.2f KB = %.2f KB",
		dHeap/float64(n)/1024, dStack/float64(n)/1024, (dHeap+dStack)/float64(n)/1024)
	perStream := (dHeap + dStack) / float64(n) / 1024
	perGoroutines := float64(idle.goroutines-base.goroutines) / float64(n)
	t.Logf("goroutines per stream: %.2f", perGoroutines)
	// measured 15.4 KB and 5.0; the margin is for the Go runtime moving, not for regressions
	if perStream > 24 {
		t.Errorf("%.1f KB per idle stream, budget 24 KB", perStream)
	}
	if perGoroutines > 6 {
		t.Errorf("%.1f goroutines per idle stream, budget 6", perGoroutines)
	}
	fmt.Fprintln(os.Stderr)
	for _, c := range clients {
		c.Close()
	}
}
