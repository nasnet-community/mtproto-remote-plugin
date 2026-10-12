//go:build linux

package drill

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/agent/state"
	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/link"
	"github.com/nasnet-community/mtproto-remote-plugin/reality"
	"github.com/nasnet-community/mtproto-remote-plugin/tgnet"
)

// The soak runs the real agent binary in its own process for an hour, with
// thousands of streams through the capture path, and watches its memory:
//
//	sudo SOAK_FULL=1 go test -run TestSoakProcess -timeout 2h -v ./drill
//
// SOAK_DURATION (default 1h), SOAK_STREAMS (5000), SOAK_CHURN (how often a 10 %
// slice is closed and reopened, 1m) and SOAK_CSV (where the samples go) tune it.
const (
	soakDC       = "203.0.113.9:443" // made up; only the hub's echo listens there
	soakCover    = "cover.soak.test"
	soakHubPort  = 18443
	soakCapPort  = 19443
	soakSample   = 10 * time.Second
	soakHWMLimit = 256 << 20 // bytes; the 5,000-stream default stays under the agent's own limit
)

func TestSoakProcess(t *testing.T) {
	switch {
	case os.Getenv("MTPROTO_NETNS_INNER") == "1":
		soakInner(t)
	case os.Getenv("SOAK_FULL") != "1":
		t.Skip("set SOAK_FULL=1 (as root) to run the one-hour agent soak")
	default:
		dir := drillDir(t)
		// release flags: the race detector would multiply the very memory being measured
		build := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", filepath.Join(dir, "mtproto-agent"), "../cmd/mtproto-agent")
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("building the agent: %v\n%s", err, out)
		}
		runInner(t, dir, "TestSoakProcess", []string{"--net", "--mount"})
	}
}

func envDuration(t *testing.T, name string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		t.Fatalf("%s=%q is not a duration", name, v)
	}
	return d
}

type soakSampleRow struct {
	at         time.Duration
	phase      string
	rssKB      int
	hwmKB      int
	goroutines int
	active     int
	hubStreams int64
	cpuTicks   int64 // utime+stime so far, in USER_HZ ticks
}

// procCPUTicks is utime+stime from /proc/<pid>/stat (fields 14 and 15), read
// after the command name, which may hold spaces or parentheses.
func procCPUTicks(pid int) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return -1
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 13 {
		return -1
	}
	u, err1 := strconv.ParseInt(f[11], 10, 64)
	s, err2 := strconv.ParseInt(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return -1
	}
	return u + s
}

// procStatusKB reads one "Vm...:  1234 kB" line of /proc/<pid>/status.
func procStatusKB(pid int, key string) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return -1
	}
	for _, l := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(l, key+":"); ok {
			f := strings.Fields(rest)
			if len(f) > 0 {
				n, _ := strconv.Atoi(f[0])
				return n
			}
		}
	}
	return -1
}

// slopePer10Min is the least-squares RSS trend, in kB per ten minutes.
func slopePer10Min(rows []soakSampleRow) float64 {
	if len(rows) < 2 {
		return 0
	}
	var sx, sy, sxx, sxy float64
	for _, r := range rows {
		x, y := r.at.Minutes(), float64(r.rssKB)
		sx, sy, sxx, sxy = sx+x, sy+y, sxx+x*x, sxy+x*y
	}
	n := float64(len(rows))
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den * 10
}

// soakClient drives the uid-998 stand-in for telemt over its stdin/stdout.
type soakClient struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Scanner
}

func (c *soakClient) do(t *testing.T, cmd string) int {
	t.Helper()
	if _, err := fmt.Fprintln(c.in, cmd); err != nil {
		t.Fatalf("client %q: %v", cmd, err)
	}
	for c.out.Scan() {
		rest, ok := strings.CutPrefix(c.out.Text(), "SOAK ")
		if !ok {
			continue
		}
		if n, ok := strings.CutPrefix(rest, "ok "); ok {
			v, _ := strconv.Atoi(n)
			return v
		}
		t.Fatalf("client %q: %s", cmd, rest)
	}
	t.Fatalf("client %q: it went away: %v", cmd, c.out.Err())
	return 0
}

func soakInner(t *testing.T) {
	dir := os.Getenv("MTPROTO_DRILL_DIR")
	dur := envDuration(t, "SOAK_DURATION", time.Hour)
	churnEvery := envDuration(t, "SOAK_CHURN", time.Minute)
	n := 5000
	if v := os.Getenv("SOAK_STREAMS"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 10 {
			t.Fatalf("SOAK_STREAMS=%q", v)
		}
	}
	warmup := min(5*time.Minute, dur/3)
	csvPath := os.Getenv("SOAK_CSV")
	if csvPath == "" {
		csvPath = filepath.Join(os.TempDir(), fmt.Sprintf("mtproto-soak-%d.csv", time.Now().Unix()))
	}
	t.Logf("soak: %v, %d streams, churn every %v, warm-up %v, samples to %s", dur, n, churnEvery, warmup, csvPath)

	netnsBasics(t)
	// the cover name resolves here only: this mount namespace is private
	hosts := filepath.Join(dir, "hosts")
	os.WriteFile(hosts, []byte("127.0.0.1 localhost\n::1 localhost\n127.0.0.1 "+soakCover+"\n"), 0o644)
	sh(t, "mount", "-o", "bind", hosts, "/etc/hosts")
	dcAP := netip.MustParseAddrPort(soakDC)
	sh(t, "ip", "addr", "add", dcAP.Addr().String()+"/32", "dev", "lo")

	coverLn, err := net.Listen("tcp", "127.0.0.1:443")
	if err != nil {
		t.Fatal(err)
	}
	cover := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cover.Listener.Close()
	cover.Listener = coverLn
	cover.EnableHTTP2 = true
	cover.Config.ErrorLog = log.New(io.Discard, "", 0)
	cover.StartTLS()
	defer cover.Close()

	echo, err := net.Listen("tcp", soakDC)
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	// the agent, as its own process, against a scratch layout
	priv, pub, err := reality.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	token := make([]byte, 32)
	rand.Read(token)
	adir := filepath.Join(dir, "agent")
	os.MkdirAll(adir, 0o755)
	st := &state.File{AgentID: "soak", RealityPriv: priv, ShortIDs: [][]byte{{5, 5}}, Tokens: [][]byte{token},
		HubPort: soakHubPort, CapturePort: soakCapPort, TelemtUID: telemtUID, TelemtGID: telemtUID, Disguise: soakCover,
		// no users: telemt stays off and the stand-in plays its part
		Desired: &ctrl.State{Rev: "soak", Port: 18444, Disguise: soakCover, DCNets: []string{"203.0.113.0/24"}}}
	if err := state.Save(filepath.Join(adir, "state.json"), st); err != nil {
		t.Fatal(err)
	}
	var agentOut lockedBuffer
	agent := exec.Command(filepath.Join(dir, "mtproto-agent"), "run",
		"-state", filepath.Join(adir, "state.json"), "-data", adir, "-telemt", filepath.Join(adir, "telemt"),
		"-bin", filepath.Join(dir, "mtproto-agent"), "-trial", filepath.Join(adir, "trial.json"), "-unit", filepath.Join(adir, "unit"))
	// the same environment as the unit in installer/install.go: the budget is for the agent as shipped
	agent.Env = append(os.Environ(), "GOMEMLIMIT=256MiB", "GOTRACEBACK=none")
	agent.Stdout, agent.Stderr = &agentOut, &agentOut
	agent.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	agentExit := make(chan error, 1)
	go func() { agentExit <- agent.Wait() }()
	defer func() {
		agent.Process.Signal(syscall.SIGTERM)
		select {
		case <-agentExit:
		case <-time.After(10 * time.Second):
			agent.Process.Kill()
		}
		if t.Failed() {
			t.Logf("agent output:\n%s", agentOut.String())
		}
	}()
	pid := agent.Process.Pid

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dl net.Dialer
	hub := link.NewHub(link.HubConfig{
		AgentID: "soak", Token: token, Addr: fmt.Sprintf("127.0.0.1:%d", soakHubPort),
		Reality:    reality.ClientConfig{ServerName: soakCover, PublicKey: pub, ShortID: [8]byte{5, 5}},
		Conns:      []link.ConnSpec{{Name: "soak", Dial: dl.DialContext}},
		Allow:      tgnet.New([]netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}),
		DialDC:     dl.DialContext,
		MaxStreams: n + 500,
	})
	go hub.Run(ctx)
	wctx, wcancel := context.WithTimeout(ctx, 60*time.Second)
	if err := hub.WaitUp(wctx); err != nil {
		t.Fatalf("hub never reached the agent: %v", err)
	}
	wcancel()
	status := func() ctrl.Status {
		t.Helper()
		var s ctrl.Status
		cctx, ccancel := context.WithTimeout(ctx, 15*time.Second)
		defer ccancel()
		if err := hub.Call(cctx, ctrl.MStatus, nil, &s); err != nil {
			t.Fatalf("status: %v", err)
		}
		return s
	}
	if s := status(); !s.Capture.Rules || s.Goroutines == 0 {
		t.Fatalf("agent status before the soak: rules %v goroutines %d", s.Capture.Rules, s.Goroutines)
	}

	// the stand-in for telemt: the same binary as uid 998, holding the streams
	cl := exec.Command(filepath.Join(dir, "drill.test"), "-test.run", "^TestSoakProcessClient$", "-test.count=1", "-test.timeout=0")
	cl.Env = append(os.Environ(), "SOAK_CLIENT_DC="+soakDC, fmt.Sprintf("SOAK_CLIENT_N=%d", n))
	cl.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: telemtUID, Gid: telemtUID}, Pdeathsig: syscall.SIGKILL}
	cl.Stderr = os.Stderr
	in, _ := cl.StdinPipe()
	outPipe, _ := cl.StdoutPipe()
	if err := cl.Start(); err != nil {
		t.Fatal(err)
	}
	client := &soakClient{cmd: cl, in: in, out: bufio.NewScanner(outPipe)}
	defer func() { fmt.Fprintln(in, "quit"); in.Close(); cl.Wait() }()

	f, err := os.Create(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	// cpu_s assumes USER_HZ = 100, what Linux reports /proc times in
	w.Write([]string{"elapsed_s", "phase", "vmrss_kb", "vmhwm_kb", "goroutines", "capture_active", "hub_streams", "cpu_ticks", "cpu_s"})
	start := time.Now()
	var rows []soakSampleRow
	sample := func(phase string) soakSampleRow {
		s := status()
		r := soakSampleRow{time.Since(start), phase, procStatusKB(pid, "VmRSS"), procStatusKB(pid, "VmHWM"), s.Goroutines, s.Capture.Active, hub.Status().Streams, procCPUTicks(pid)}
		rows = append(rows, r)
		w.Write([]string{strconv.Itoa(int(r.at.Seconds())), phase, strconv.Itoa(r.rssKB), strconv.Itoa(r.hwmKB),
			strconv.Itoa(r.goroutines), strconv.Itoa(r.active), strconv.FormatInt(r.hubStreams, 10),
			strconv.FormatInt(r.cpuTicks, 10), strconv.FormatFloat(float64(r.cpuTicks)/100, 'f', 2, 64)})
		w.Flush()
		return r
	}
	settle := func(what string, want int, within time.Duration) bool {
		deadline := time.Now().Add(within)
		for {
			s := status()
			if s.Capture.Active == want && hub.Status().Streams == int64(want) {
				return true
			}
			if time.Now().After(deadline) {
				t.Errorf("%s: capture active %d, hub streams %d, want %d", what, s.Capture.Active, hub.Status().Streams, want)
				return false
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	base := sample("baseline")
	retries := client.do(t, fmt.Sprintf("open 0 %d", n))
	if !settle("opening", n, time.Minute) {
		t.FailNow()
	}
	opened := sample("opened")
	t.Logf("opened %d streams in %v (%d retried opens): RSS %d kB, goroutines %d -> %d",
		n, opened.at.Round(time.Second), retries, opened.rssKB, base.goroutines, opened.goroutines)

	slice := n / 10
	steady := time.Now()
	nextSample, nextChurn := steady.Add(soakSample), steady.Add(churnEvery)
	var ref *soakSampleRow
	waves, churns := 0, 0
	for time.Since(steady) < dur {
		k := waves % 10
		client.do(t, fmt.Sprintf("wave %d %d 4096", k*slice, (k+1)*slice))
		waves++
		if now := time.Now(); now.After(nextChurn) {
			c := churns % 10
			client.do(t, fmt.Sprintf("close %d %d", c*slice, (c+1)*slice))
			settle(fmt.Sprintf("churn %d closing", churns), n-slice, 30*time.Second)
			retries += client.do(t, fmt.Sprintf("open %d %d", c*slice, (c+1)*slice))
			settle(fmt.Sprintf("churn %d reopening", churns), n, 30*time.Second)
			churns++
			nextChurn = now.Add(churnEvery)
		}
		if now := time.Now(); now.After(nextSample) {
			r := sample("steady")
			if ref == nil && time.Since(steady) >= warmup {
				ref = &r
			}
			if len(rows)%6 == 0 {
				t.Logf("%v: RSS %d kB (peak %d), goroutines %d, streams %d", r.at.Round(time.Second), r.rssKB, r.hwmKB, r.goroutines, r.active)
			}
			nextSample = now.Add(soakSample)
		}
		time.Sleep(time.Second)
	}
	end := sample("end")

	client.do(t, fmt.Sprintf("close 0 %d", n))
	settle("closing everything", 0, time.Minute)
	near := base.goroutines + max(10, base.goroutines/20)
	var after soakSampleRow
	for deadline := time.Now().Add(time.Minute); ; {
		after = sample("closed")
		if after.goroutines <= near || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}

	// the gates
	peak := 0
	for _, r := range rows {
		peak = max(peak, r.hwmKB)
	}
	t.Logf("%d waves, %d churn waves, %d retried opens; peak VmHWM %d kB; goroutines %d idle, %d with %d streams, %d after closing; agent CPU %.1f s",
		waves, churns, retries, peak, base.goroutines, end.goroutines, n, after.goroutines, float64(after.cpuTicks-base.cpuTicks)/100)
	if peak*1024 > soakHWMLimit {
		t.Errorf("peak VmHWM %d kB is over the %d byte budget", peak, soakHWMLimit)
	}
	if ref == nil {
		t.Errorf("the run ended before the %v warm-up", warmup)
	} else {
		lo, hi := float64(ref.goroutines)*0.95, float64(ref.goroutines)*1.05
		for _, r := range rows {
			if r.phase == "steady" && r.at > ref.at && (float64(r.goroutines) < lo || float64(r.goroutines) > hi) {
				t.Errorf("goroutines %d at %v, outside 5%% of %d", r.goroutines, r.at.Round(time.Second), ref.goroutines)
			}
		}
	}
	window := min(30*time.Minute, dur-warmup)
	var tail []soakSampleRow
	for _, r := range rows {
		if r.phase == "steady" && r.at >= end.at-window {
			tail = append(tail, r)
		}
	}
	trend := slopePer10Min(tail)
	t.Logf("RSS trend over the last %v: %+.0f kB per 10 min", window.Round(time.Second), trend)
	if window >= 10*time.Minute && trend >= 1024 {
		t.Errorf("RSS grows %.0f kB per 10 min", trend)
	}
	if after.active != 0 || after.hubStreams != 0 {
		t.Errorf("after closing: capture active %d, hub streams %d", after.active, after.hubStreams)
	}
	if after.goroutines > near {
		t.Errorf("goroutines %d a minute after closing, baseline %d", after.goroutines, base.goroutines)
	}
	t.Logf("samples: %s", csvPath)
}

// TestSoakProcessClient is the soak's stand-in telemt; it only runs when the
// soak starts it as uid 998.
func TestSoakProcessClient(t *testing.T) {
	dst := os.Getenv("SOAK_CLIENT_DC")
	if dst == "" {
		t.Skip("helper for TestSoakProcess")
	}
	n, _ := strconv.Atoi(os.Getenv("SOAK_CLIENT_N"))
	conns := make([]net.Conn, n)
	each := func(i, j int, fn func(k int) error) error {
		var mu sync.Mutex
		var first error
		var wg sync.WaitGroup
		sem := make(chan struct{}, 64)
		for k := i; k < j; k++ {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if err := fn(k); err != nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return first
	}
	exchange := func(c net.Conn, p []byte) error {
		c.SetDeadline(time.Now().Add(15 * time.Second))
		if _, err := c.Write(p); err != nil {
			return err
		}
		got := make([]byte, len(p))
		if _, err := io.ReadFull(c, got); err != nil {
			return err
		}
		if !bytes.Equal(got, p) {
			return fmt.Errorf("echo mismatch")
		}
		c.SetDeadline(time.Time{})
		return nil
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var op string
		var i, j, size int
		fmt.Sscan(sc.Text(), &op, &i, &j, &size)
		var retried int
		var mu sync.Mutex
		var err error
		switch op {
		case "quit":
			return
		case "open":
			err = each(i, j, func(k int) error {
				hello := []byte(fmt.Sprintf("hello-%010d", k))
				var last error
				for try := 0; try < 50; try++ {
					c, err := net.DialTimeout("tcp", dst, 5*time.Second)
					if err == nil {
						if err = exchange(c, hello); err == nil {
							conns[k] = c
							if try > 0 {
								mu.Lock()
								retried++
								mu.Unlock()
							}
							return nil
						}
						c.Close()
					}
					last = err
					time.Sleep(100 * time.Millisecond)
				}
				return fmt.Errorf("stream %d never opened: %v", k, last)
			})
		case "wave":
			p := make([]byte, size)
			rand.Read(p)
			err = each(i, j, func(k int) error {
				if conns[k] == nil {
					return fmt.Errorf("stream %d is not open", k)
				}
				if err := exchange(conns[k], p); err != nil {
					return fmt.Errorf("stream %d: %v", k, err)
				}
				return nil
			})
		case "close":
			for k := i; k < j; k++ {
				if conns[k] != nil {
					conns[k].Close()
					conns[k] = nil
				}
			}
		default:
			err = fmt.Errorf("unknown command %q", sc.Text())
		}
		if err != nil {
			fmt.Println("SOAK err", err)
			continue
		}
		fmt.Println("SOAK ok", retried)
	}
}
