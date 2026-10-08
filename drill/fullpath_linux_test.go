//go:build linux

package drill

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
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
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/agent/capture"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/daemon"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/state"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/supervise"
	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/link"
	"github.com/nasnet-community/mtproto-remote-plugin/reality"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops"
	"github.com/nasnet-community/mtproto-remote-plugin/release"
	"github.com/nasnet-community/mtproto-remote-plugin/telemt"
	"github.com/nasnet-community/mtproto-remote-plugin/tgnet"
)

// The full-path drill runs the real telemt behind the real capture, link and
// hub, against fake datacentres on the addresses telemt itself dials:
//
//	sudo MTPROTO_NETNS_DRILL=1 TELEMT_BIN=/path/telemt go test -count=1 -run TestFullPathDrill -v ./drill
//
// Without TELEMT_BIN the pinned telemt is fetched into TELEMT_CACHE (default:
// the user cache dir) and checked against release.Telemt.
const (
	learnPort    = 18440
	telemtPort   = 18443
	otherPort    = 18444 // the telemt whose upstream sits on a captured address, port 1080
	otherMetrics = 14310
	capturePort  = 19443
	coverName    = "example.com"
)

func TestFullPathDrill(t *testing.T) {
	switch {
	case os.Getenv("MTPROTO_NETNS_INNER") == "1":
		fullPathInner(t)
	case os.Getenv("MTPROTO_NETNS_DRILL") != "1":
		t.Skip("set MTPROTO_NETNS_DRILL=1 (as root) to run the full-path drill")
	default:
		dir := drillDir(t)
		copyFile(t, telemtBinary(t), filepath.Join(dir, "telemt"), 0o755)
		runInner(t, dir, "TestFullPathDrill", []string{"--net"})
	}
}

// telemtBinary is TELEMT_BIN, or the pinned release fetched and hash-checked.
func telemtBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("TELEMT_BIN"); p != "" {
		return p
	}
	dir := os.Getenv("TELEMT_CACHE")
	if dir == "" {
		c, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		dir = filepath.Join(c, "mtproto-drill")
	}
	arch, err := release.ParseArch(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	b := relayops.NewBinaries(dir, func() *http.Client { return &http.Client{Timeout: 3 * time.Minute} })
	bin, err := b.Telemt(context.Background(), arch)
	if err != nil {
		t.Fatalf("no TELEMT_BIN and the pinned telemt could not be fetched: %v", err)
	}
	r, err := bin.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := filepath.Join(t.TempDir(), "telemt")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, r); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return out
}

// keptProc is the daemon's telemt with its life tied to the drill, not to the
// daemon, so the drill can stop the agent and watch what telemt does then.
type keptProc struct{ *supervise.Proc }

func (keptProc) Run(ctx context.Context) { <-ctx.Done() }

// dialRecorder is the hub's datacentre dial, remembering each local address so
// a fake datacentre's accept can be traced back to it and to where it went.
type dialRecorder struct {
	mu     sync.Mutex
	locals map[string]string // local address -> destination
	dsts   []string
}

func (r *dialRecorder) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, network, addr)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dsts = append(r.dsts, addr)
	if err == nil {
		r.locals[c.LocalAddr().String()] = addr
	}
	return c, err
}

// dstOf is where the hub's dial that the datacentre saw coming from a went, or "".
func (r *dialRecorder) dstOf(a net.Addr) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.locals[a.String()]
}

func (r *dialRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.dsts)
}

// dialsTo counts dials to addr from the n-th on: telemt's own background dials
// to other datacentres do not count.
func (r *dialRecorder) dialsTo(addr string, from int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, d := range r.dsts[from:] {
		if d == addr {
			n++
		}
	}
	return n
}

// violations remembers the destinations the hub refused.
type violations struct {
	mu   sync.Mutex
	dsts []string
}

func (v *violations) add(e link.Event) {
	if e.Kind == link.EventViolation {
		v.mu.Lock()
		v.dsts = append(v.dsts, e.Detail)
		v.mu.Unlock()
	}
}

func (v *violations) of(dst string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, d := range v.dsts {
		if d == dst {
			n++
		}
	}
	return n
}

func classicSettings(extra string) telemt.Settings {
	set, err := telemt.ParseSettings([]byte(`{"modes":{"classic":true}` + extra + `}`))
	if err != nil {
		panic(err)
	}
	return set
}

func runTelemt(t *testing.T, ctx context.Context, bin, dir, config string, out io.Writer) (*supervise.Proc, <-chan struct{}) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	p := supervise.New(supervise.Spec{Bin: bin, Args: []string{"run", path}, Dir: dir, UID: telemtUID, GID: telemtUID, Out: out})
	p.SetEnabled(true) // no daemon here to do it
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	return p, done
}

// learnDCs runs telemt with no capture in place and keeps every address its
// uid tries to reach on 443 before it starts listening. Nothing else is used.
func learnDCs(t *testing.T, bin, dir, secret string) []netip.Addr {
	t.Helper()
	out, err := telemt.Render(telemt.RenderInput{Port: learnPort, Settings: classicSettings(""),
		Users: []telemt.User{{Name: "drill", Secret: secret, Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	ctx, stop := context.WithCancel(context.Background())
	_, done := runTelemt(t, ctx, bin, filepath.Join(dir, "learn"), out.Config(), &logs)
	seen := map[netip.Addr]bool{}
	started := time.Now()
	for time.Since(started) < 90*time.Second && !listening(learnPort) {
		for _, s := range sockets() {
			if s.state == stSynSent && s.uid == telemtUID && s.remote.Port() == 443 {
				seen[s.remote.Addr()] = true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	<-done
	if len(seen) == 0 {
		t.Fatalf("telemt dialled nothing on 443 in %v:\n%s", time.Since(started), logs.String())
	}
	var addrs []netip.Addr
	for a := range seen {
		addrs = append(addrs, a)
	}
	slices.SortFunc(addrs, func(a, b netip.Addr) int { return a.Compare(b) })
	t.Logf("learned %d datacentre addresses in %v: %v", len(addrs), time.Since(started).Round(time.Second), addrs)
	return addrs
}

func hostPrefixes(addrs []netip.Addr) []netip.Prefix {
	var ps []netip.Prefix
	for _, a := range addrs {
		ps = append(ps, netip.PrefixFrom(a, a.BitLen()))
	}
	return ps
}

// clientRoundTrip is the stand-in Telegram client: classic obfuscated2 to
// telemt, one intermediate frame, and whatever comes back.
func clientRoundTrip(port int, secret []byte, msg []byte) ([]byte, time.Duration, error) {
	start := time.Now()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		return nil, time.Since(start), err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	s, err := o2Dial(c, tagIntermediate, 2, secret)
	if err != nil {
		return nil, time.Since(start), err
	}
	if err := writeFrame(s, msg); err != nil {
		return nil, time.Since(start), err
	}
	got, err := readFrame(s, 1<<20)
	return got, time.Since(start), err
}

func describe(err error) string {
	switch {
	case err == nil:
		return "no error"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "closed (EOF)"
	case strings.Contains(err.Error(), "reset"):
		return "reset"
	}
	return err.Error()
}

func fullPathInner(t *testing.T) {
	dir := os.Getenv("MTPROTO_DRILL_DIR")
	bin := filepath.Join(dir, "telemt")
	netnsBasics(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	secret := make([]byte, 16)
	rand.Read(secret)
	secretHex := hex.EncodeToString(secret)
	msg := make([]byte, 256)
	rand.Read(msg)

	// 1. learn the datacentres from telemt itself
	dcs := learnDCs(t, bin, dir, secretHex)
	pfx := hostPrefixes(dcs)

	// 2. a fake datacentre on every learned address
	for _, a := range dcs {
		if a.Is4() {
			sh(t, "ip", "addr", "add", a.String()+"/32", "dev", "lo")
		} else {
			sh(t, "ip", "-6", "addr", "add", a.String()+"/128", "dev", "lo", "nodad")
		}
	}
	dc := newFakeDC(t, ":443")
	func() {
		c, err := net.DialTimeout("tcp", netip.AddrPortFrom(dcs[0], 443).String(), 2*time.Second)
		if err != nil {
			t.Fatalf("root cannot reach the fake datacentre: %v", err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		s, _ := o2Dial(c, tagIntermediate, 2, nil)
		writeFrame(s, msg)
		if got, err := readFrame(s, 1<<20); err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("fake datacentre did not echo: %v", err)
		}
	}()

	// 3. the agent, with the real capture, the real telemt and a real REALITY pair
	cover := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cover.EnableHTTP2 = true
	cover.Config.ErrorLog = log.New(io.Discard, "", 0) // REALITY's probing is noise here
	cover.StartTLS()
	defer cover.Close()
	priv, pub, err := reality.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	token := make([]byte, 32)
	rand.Read(token)
	hubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adir := filepath.Join(dir, "agent")
	os.MkdirAll(adir, 0o755)
	st := &state.File{AgentID: "drill", RealityPriv: priv, ShortIDs: [][]byte{{4, 2}}, Tokens: [][]byte{token},
		CapturePort: capturePort, TelemtUID: telemtUID, TelemtGID: telemtUID, Disguise: coverName}
	var telemtLog lockedBuffer
	var proc *supervise.Proc
	var coverDials atomic.Int64
	exited := make(chan struct{}, 1)
	d, err := daemon.New(daemon.Config{
		StatePath: filepath.Join(adir, "state.json"), WorkDir: adir, TelemtBin: bin,
		AgentBin: filepath.Join(adir, "mtproto-agent"), TrialFile: filepath.Join(adir, "trial.json"),
		Version: "drill", Listen: hubLn,
		RealityDest: func(string) string { return cover.Listener.Addr().String() },
		// telemt's cover traffic comes to the agent, which dials the stand-in site
		CoverDial: func(ctx context.Context, addr string) (net.Conn, error) {
			coverDials.Add(1)
			var dl net.Dialer
			return dl.DialContext(ctx, "tcp", cover.Listener.Addr().String())
		},
		NewProc: func(s supervise.Spec) daemon.Proc {
			s.Out = io.MultiWriter(s.Out, &telemtLog)
			proc = supervise.New(s) // off until a pushed state turns it on
			go proc.Run(ctx)
			return keptProc{proc}
		},
		Exit: func() { exited <- struct{}{} },
	}, st)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			t.Logf("telemt output:\n%s", telemtLog.String())
		}
	}()
	agentCtx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan error, 1)
	go func() { agentDone <- d.Run(agentCtx) }()

	rec := &dialRecorder{locals: map[string]string{}}
	refused := &violations{}
	agentAddr := hubLn.Addr().String() // where the hub dials; the link can move
	startHub := func(allow *tgnet.Allowlist) (*link.Hub, func()) {
		var dl net.Dialer
		h := link.NewHub(link.HubConfig{
			AgentID: "drill", Token: token, Addr: agentAddr,
			Reality: reality.ClientConfig{ServerName: coverName, PublicKey: pub, ShortID: [8]byte{4, 2}},
			Conns:   []link.ConnSpec{{Name: "drill", Dial: dl.DialContext}},
			Allow:   allow, DialDC: rec.dial, OnEvent: refused.add,
		})
		hctx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { h.Run(hctx); close(done) }()
		wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
		defer wcancel()
		if err := h.WaitUp(wctx); err != nil {
			t.Fatalf("hub never connected: %v", err)
		}
		return h, func() { stop(); <-done }
	}
	hub, stopHub := startHub(tgnet.New(pfx))

	// nothing runs telemt before a state is in place: no crash loop, no direct dials
	time.Sleep(time.Second)
	if st := d.Status(ctx); proc.Status().Running || st.Telemt.Held == "" || proc.Status().Restarts != 0 {
		t.Fatalf("before the first push: running %v held %q restarts %d", proc.Status().Running, st.Telemt.Held, proc.Status().Restarts)
	}

	var nets []string
	for _, p := range pfx {
		nets = append(nets, p.String())
	}
	desired := ctrl.State{
		Rev: "drill-1", Port: telemtPort, Disguise: coverName,
		// classic for the clients here, tls so telemt fronts the cover site
		Telemt: map[string]any{"modes": map[string]any{"classic": true, "tls": true}},
		Users:  []ctrl.User{{Name: "drill", Secret: secretHex, Enabled: true}},
		DCNets: nets,
	}
	var ack ctrl.StateAck
	if err := hub.Call(ctx, ctrl.MStateApply, desired, &ack); err != nil || ack.Rev != "drill-1" {
		t.Fatalf("state.apply: %+v %v", ack, err)
	}
	if !capture.Present(ctx) {
		t.Fatal("no capture table after the state push")
	}

	// a second telemt whose upstream is a captured address on another port; it
	// starts now so its start-up overlaps the first one's
	other, err := telemt.Render(telemt.RenderInput{Port: otherPort, Settings: classicSettings(fmt.Sprintf(`,"metrics_port":%d`, otherMetrics)),
		Users: []telemt.User{{Name: "drill", Secret: secretHex, Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	upstream := netip.AddrPortFrom(dcs[0], 1080).String()
	otherCfg := strings.Replace(other.Config(), "type = \"direct\"\n", "type = \"socks5\"\naddress = \""+upstream+"\"\n", 1)
	if otherCfg == other.Config() {
		t.Fatal("could not swap the upstream in the rendered config")
	}
	var otherLog lockedBuffer
	otherCtx, stopOther := context.WithCancel(ctx)
	_, otherDone := runTelemt(t, otherCtx, bin, filepath.Join(dir, "other"), otherCfg, &otherLog)
	defer func() {
		stopOther()
		<-otherDone
		if t.Failed() {
			t.Logf("second telemt output:\n%s", otherLog.String())
		}
	}()

	started := time.Now()
	waitFor(t, "telemt to listen behind the capture", 150*time.Second, func() bool { return listening(telemtPort) })
	t.Logf("telemt listening %v after the state push", time.Since(started).Round(time.Second))
	waitFor(t, "the second telemt to listen", 150*time.Second, func() bool { return listening(otherPort) })
	// telemt's own start-up checks went through the chain too; let them finish
	waitFor(t, "telemt's start-up dials to drain", 60*time.Second, func() bool {
		return d.Status(ctx).Capture.Active == 0 && hub.Status().Streams == 0
	})
	t.Logf("start-up: hub relayed %d, fake datacentre saw %d", hub.Status().Relayed, len(must(dc.snapshot())))
	if v := d.Status(ctx).TelemtVersion; v != release.TelemtVersion {
		t.Errorf("status reports telemt %q, the binary is %s", v, release.TelemtVersion)
	}

	// the client's datacentre, found by what it carried, not by being the last dial
	var hitDst string

	t.Run("a client's message goes through telemt, the capture, the link and the hub, and comes back", func(t *testing.T) {
		relayed0, carried0 := hub.Status().Relayed, len(dc.carrying(msg))
		rejected0 := d.Status(ctx).Capture.Rejected
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", telemtPort), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(15 * time.Second))
		s, err := o2Dial(c, tagIntermediate, 2, secret)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeFrame(s, msg); err != nil {
			t.Fatal(err)
		}
		got, err := readFrame(s, 1<<20)
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("echo: %d bytes, %v", len(got), err)
		}
		// telemt may dial other datacentres meanwhile: at least ours is open
		if a := d.Status(ctx).Capture.Active; a < 1 {
			t.Errorf("agent capture active = %d while the client is connected", a)
		}
		if n := hub.Status().Streams; n < 1 {
			t.Errorf("hub streams = %d while the client is connected", n)
		}
		c.Close()
		if n := hub.Status().Relayed - relayed0; n < 1 {
			t.Errorf("hub relayed moved by %d", n)
		}
		if n := d.Status(ctx).Capture.Rejected - rejected0; n != 0 {
			t.Errorf("agent rejected %d", n)
		}
		carried := dc.carrying(msg)
		if len(carried)-carried0 != 1 {
			t.Fatalf("the message reached the fake datacentre on %d new connections, want 1", len(carried)-carried0)
		}
		if hitDst = rec.dstOf(carried[len(carried)-1]); hitDst == "" {
			t.Fatalf("the datacentre's accept from %v was not the hub's dial", carried[len(carried)-1])
		}
		if _, bad := dc.snapshot(); bad != 0 {
			t.Errorf("%d undecodable datacentre connections", bad)
		}
		t.Logf("dc 2 went to %s through the hub", hitDst)
	})

	t.Run("telemt copies the cover site's certificate through the agent", func(t *testing.T) {
		front := filepath.Join(adir, "telemt", "run", "tlsfront")
		waitFor(t, "telemt's certificate copy", 60*time.Second, func() bool {
			es, err := os.ReadDir(front)
			return err == nil && len(es) > 0
		})
		if coverDials.Load() < 1 {
			t.Error("the copy did not come through the agent's socket")
		}
	})

	t.Run("a stranger probing telemt gets the cover site's own certificate, by way of the agent", func(t *testing.T) {
		dials0 := coverDials.Load()
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", fmt.Sprintf("127.0.0.1:%d", telemtPort),
			&tls.Config{ServerName: coverName, InsecureSkipVerify: true})
		if err != nil {
			t.Fatalf("the probe's handshake: %v", err)
		}
		defer c.Close()
		got := c.ConnectionState().PeerCertificates
		if len(got) == 0 || !bytes.Equal(got[0].Raw, cover.Certificate().Raw) {
			t.Fatal("the probe was not handed the cover site's certificate")
		}
		if coverDials.Load() == dials0 {
			t.Error("the probe reached the site without the agent dialling it")
		}
	})

	t.Run("telemt does not look for its public address", func(t *testing.T) {
		out := telemtLog.String()
		if !strings.Contains(out, "STUN probe is disabled") {
			t.Errorf("no sign STUN is off in telemt's output")
		}
		if strings.Contains(out, "STUN-Quorum reached") || strings.Contains(out, "using HTTP public IPv4 fallback") {
			t.Errorf("telemt learned its address from outside")
		}
	})

	t.Run("telemt dialling a captured address on another port is refused at once", func(t *testing.T) {
		dials0, carried0 := rec.count(), len(dc.carrying(msg))
		before := d.Status(ctx).Capture
		got, took, err := clientRoundTrip(otherPort, secret, msg)
		if err == nil {
			t.Fatalf("the client got %d bytes back through a refused upstream", len(got))
		}
		t.Logf("client saw %s after %v", describe(err), took.Round(time.Millisecond))
		if took > 2*time.Second {
			t.Errorf("took %v to fail, want an immediate refusal", took)
		}
		after := d.Status(ctx).Capture
		if after.Rejected != before.Rejected || rec.dialsTo(upstream, dials0) != 0 || len(dc.carrying(msg)) != carried0 {
			t.Errorf("the dial reached the capture path or beyond: %+v -> %+v", before, after)
		}
	})

	t.Run("the link moves to another port, and the old one closes once the hub is in there", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		oldAddr := agentAddr
		if err := hub.Call(ctx, ctrl.MLinkPort, ctrl.LinkPort{Port: port}, nil); err != nil {
			t.Fatalf("open: %v", err)
		}
		// the panel's redial: a hub on the new port, then the old one goes
		agentAddr = fmt.Sprintf("127.0.0.1:%d", port)
		stopHub()
		hub, stopHub = startHub(tgnet.New(pfx))
		if err := hub.Call(ctx, ctrl.MLinkPort, ctrl.LinkPort{Finish: true}, nil); err != nil {
			t.Fatalf("finish: %v", err)
		}
		if c, err := net.DialTimeout("tcp", oldAddr, time.Second); err == nil {
			c.Close()
			t.Fatalf("%s still accepts after the move", oldAddr)
		}
		// a client's message still makes the whole trip
		carried0 := len(dc.carrying(msg))
		got, _, err := clientRoundTrip(telemtPort, secret, msg)
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("echo over the moved link: %d bytes, %v", len(got), err)
		}
		if len(dc.carrying(msg)) == carried0 {
			t.Error("the message did not reach the fake datacentre over the moved link")
		}
	})

	t.Run("a hub allowlist narrower than the agent's set resets the stream", func(t *testing.T) {
		hit, err := netip.ParseAddrPort(hitDst)
		if err != nil {
			t.Fatalf("no datacentre traced from the first subtest: %v", err)
		}
		var narrow []netip.Prefix
		for _, p := range pfx {
			if !p.Contains(hit.Addr()) {
				narrow = append(narrow, p)
			}
		}
		stopHub()
		hub, stopHub = startHub(tgnet.New(narrow))
		dials0, carried0, refused0 := rec.count(), len(dc.carrying(msg)), refused.of(hitDst)
		got, took, err := clientRoundTrip(telemtPort, secret, msg)
		if err == nil {
			t.Fatalf("the client got %d bytes back past the allowlist", len(got))
		}
		t.Logf("client saw %s after %v", describe(err), took.Round(time.Millisecond))
		// counted for that datacentre, whatever telemt dials elsewhere meanwhile
		if n := refused.of(hitDst) - refused0; n < 1 {
			t.Errorf("the hub refused %s %d times, want at least once", hitDst, n)
		}
		if n := rec.dialsTo(hitDst, dials0); n != 0 {
			t.Errorf("the hub dialled %s %d times past its allowlist", hitDst, n)
		}
		if len(dc.carrying(msg)) != carried0 {
			t.Error("the message reached the fake datacentre anyway")
		}
	})

	t.Run("with the agent stopped there is no direct path", func(t *testing.T) {
		stopHub()
		stopAgent()
		if err := <-agentDone; err != nil {
			t.Fatalf("agent run: %v", err)
		}
		if !capture.Present(ctx) {
			t.Fatal("stopping the agent removed the rules")
		}
		if !proc.Status().Running {
			t.Fatal("telemt is not running, so this would prove nothing")
		}
		acc0 := len(must(dc.snapshot()))
		// counted after the agent's rules: a packet from telemt that got past them
		// with an address that isn't loopback (a redirect has one) left directly
		sh(t, "nft", "add table inet drillwatch")
		sh(t, "nft", "add chain inet drillwatch out { type filter hook output priority 100; policy accept; }")
		sh(t, "nft", fmt.Sprintf("add rule inet drillwatch out meta skuid %d ip daddr != 127.0.0.0/8 counter", telemtUID))
		sh(t, "nft", fmt.Sprintf("add rule inet drillwatch out meta skuid %d ip6 daddr != ::1 counter", telemtUID))
		defer exec.Command("nft", "delete", "table", "inet", "drillwatch").Run()
		got, took, err := clientRoundTrip(telemtPort, secret, msg)
		time.Sleep(500 * time.Millisecond)
		if err == nil {
			t.Fatalf("the client got %d bytes back with the agent stopped", len(got))
		}
		t.Logf("client saw %s after %v", describe(err), took.Round(time.Millisecond))
		if took > 2*time.Second {
			t.Errorf("took %v to fail, want a fast refusal", took)
		}
		out, err := exec.Command("nft", "list", "table", "inet", "drillwatch").CombinedOutput()
		if err != nil {
			t.Fatalf("nft: %v: %s", err, out)
		}
		for _, m := range regexp.MustCompile(`packets (\d+)`).FindAllStringSubmatch(string(out), -1) {
			if m[1] != "0" {
				t.Errorf("%s packets from telemt left the box directly:\n%s", m[1], out)
			}
		}
		if len(must(dc.snapshot())) != acc0 {
			t.Error("something reached the fake datacentre")
		}
	})

	t.Run("the agent's teardown leaves no nft table behind", func(t *testing.T) {
		if _, err := d.Handlers()[ctrl.MUninstall](ctx, nil, nil); err != nil {
			t.Fatal(err)
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Fatal("uninstall never exited")
		}
		if capture.Present(ctx) {
			t.Fatal("a capture table survived the teardown")
		}
		waitFor(t, "telemt to stop", 10*time.Second, func() bool { return !proc.Status().Running })
	})
}

func must[T any](v T, _ int) T { return v }
