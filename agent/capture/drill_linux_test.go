//go:build linux

package capture

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The capture drill needs root, nft, ip and unshare, so it only runs when asked:
//
//	sudo MTPROTO_NETNS_DRILL=1 go test -count=1 -run TestCaptureDrill -v ./agent/capture
//
// The outer test copies this binary somewhere a plain uid can run it, then
// re-executes itself in a fresh network namespace. Inside, a stand-in "telemt"
// (this binary again, running as the unprivileged uid) dials made-up datacentre
// addresses and the kernel does the rest.
const (
	drillUID  = 998
	drillPort = 19443
	dcV4      = "203.0.113.9"   // inside the made-up v4 prefix
	dcV6      = "2001:db8:1::9" // inside the made-up v6 prefix
	offDC     = "198.51.100.77" // on the link, but not a datacentre
)

func TestCaptureDrill(t *testing.T) {
	switch {
	case os.Getenv("MTPROTO_NETNS_INNER") == "1":
		drillInner(t)
	case os.Getenv("MTPROTO_NETNS_DRILL") != "1":
		t.Skip("set MTPROTO_NETNS_DRILL=1 (as root) to run the netns capture drill")
	default:
		drillOuter(t)
	}
}

func drillOuter(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the drill needs root")
	}
	for _, tool := range []string{"unshare", "nft", "ip"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	dir, err := os.MkdirTemp("", "capture-drill")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	os.Chmod(dir, 0o755)
	self := filepath.Join(dir, "drill.test")
	src, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self, src, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", "--net", "--", self, "-test.run", "^TestCaptureDrill$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), "MTPROTO_NETNS_INNER=1")
	out, err := cmd.CombinedOutput()
	t.Logf("\n%s", out)
	if err != nil {
		t.Fatalf("drill failed: %v", err)
	}
}

// TestCaptureDrillClient is the stand-in telemt. It only does anything when the
// drill hands it an address.
func TestCaptureDrillClient(t *testing.T) {
	addr := os.Getenv("MTPROTO_DRILL_DIAL")
	if addr == "" {
		t.Skip("helper for TestCaptureDrill")
	}
	c, err := net.DialTimeout("tcp", addr, 1200*time.Millisecond)
	if err != nil {
		fmt.Println("RESULT", classify(err))
		return
	}
	defer c.Close()
	c.Write([]byte("hello"))
	c.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	b, _ := io.ReadAll(c)
	fmt.Println("RESULT connected", strings.TrimSpace(string(b)))
}

// TestCaptureDrillServer is a stand-in telemt listening for one client.
func TestCaptureDrillServer(t *testing.T) {
	addr := os.Getenv("MTPROTO_DRILL_LISTEN")
	if addr == "" {
		t.Skip("helper for TestCaptureDrill")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Println("RESULT", err)
		return
	}
	defer ln.Close()
	fmt.Println("READY")
	ln.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	if c, err := ln.Accept(); err == nil {
		c.Write([]byte("ok"))
		c.Close()
	}
}

func classify(err error) string {
	switch {
	case strings.Contains(err.Error(), "refused"):
		return "refused"
	case strings.Contains(err.Error(), "reset"):
		return "reset"
	case strings.Contains(err.Error(), "timeout"), strings.Contains(err.Error(), "i/o"):
		return "timeout"
	}
	return "error: " + err.Error()
}

type seen struct {
	dst   netip.AddrPort
	first string
}

func drillInner(t *testing.T) {
	sh := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	sh("ip", "link", "set", "lo", "up")
	sh("ip", "link", "add", "dummy0", "type", "dummy")
	sh("ip", "link", "set", "dummy0", "up")
	sh("ip", "addr", "add", "198.51.100.2/24", "dev", "dummy0")
	sh("ip", "route", "add", "default", "via", "198.51.100.1", "dev", "dummy0")
	sh("ip", "-6", "addr", "add", "2001:db8:ffff::2/64", "dev", "dummy0", "nodad")
	sh("ip", "-6", "route", "add", "default", "via", "2001:db8:ffff::1", "dev", "dummy0")

	var mu sync.Mutex
	var got []seen
	for _, host := range []string{"127.0.0.1", "::1"} {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(drillPort)))
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					dst, err := OriginalDst(c.(*net.TCPConn))
					if err != nil {
						t.Errorf("original destination: %v", err)
						return
					}
					buf := make([]byte, 16)
					c.SetReadDeadline(time.Now().Add(time.Second))
					n, _ := c.Read(buf)
					mu.Lock()
					got = append(got, seen{dst, string(buf[:n])})
					mu.Unlock()
					c.Write([]byte("ok"))
				}()
			}
		}()
	}
	took := func() []seen {
		mu.Lock()
		defer mu.Unlock()
		out := got
		got = nil
		return out
	}

	rules := Rules{
		UID: drillUID, Port: drillPort,
		V4: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")},
		V6: []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/48")},
	}
	script, err := rules.Script()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := Apply(ctx, script); err != nil {
		t.Fatal(err)
	}
	if !Present(ctx) || !Live(ctx, true, true) {
		t.Fatal("Present or Live is false right after Apply")
	}
	// what `systemctl restart nftables` does first
	if out, err := exec.Command("nft", "flush", "ruleset").CombinedOutput(); err != nil {
		t.Fatalf("flush: %v: %s", err, out)
	}
	if Live(ctx, true, false) || Live(ctx, false, true) {
		t.Fatal("Live is true after a flush")
	}
	if err := Apply(ctx, script); err != nil {
		t.Fatal(err)
	}
	if !Live(ctx, true, true) {
		t.Fatal("Live is false after putting the rules back")
	}
	// the script replaces the tables atomically, so a second run must be harmless
	if err := Apply(ctx, script); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	dial := func(uid uint32, addr string) string {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run", "^TestCaptureDrillClient$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), "MTPROTO_DRILL_DIAL="+addr)
		if uid != 0 {
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid}}
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("client for %s: %v: %s", addr, err, out)
		}
		for _, l := range strings.Split(string(out), "\n") {
			if rest, ok := strings.CutPrefix(l, "RESULT "); ok {
				return rest
			}
		}
		t.Fatalf("client for %s printed no result: %s", addr, out)
		return ""
	}

	t.Run("v4 datacentre on 443 is captured and the original is recoverable", func(t *testing.T) {
		if r := dial(drillUID, dcV4+":443"); r != "connected ok" {
			t.Fatalf("client saw %q", r)
		}
		s := took()
		if len(s) != 1 || s[0].dst != netip.MustParseAddrPort(dcV4+":443") || s[0].first != "hello" {
			t.Fatalf("listener saw %+v", s)
		}
	})
	t.Run("v6 datacentre on 443 is captured and the original is recoverable", func(t *testing.T) {
		if r := dial(drillUID, "["+dcV6+"]:443"); r != "connected ok" {
			t.Fatalf("client saw %q", r)
		}
		s := took()
		if len(s) != 1 || s[0].dst != netip.MustParseAddrPort("["+dcV6+"]:443") || s[0].first != "hello" {
			t.Fatalf("listener saw %+v", s)
		}
	})
	t.Run("another port on a datacentre is refused on the box, never captured", func(t *testing.T) {
		for _, addr := range []string{dcV4 + ":8888", "[" + dcV6 + "]:8888"} {
			if r := dial(drillUID, addr); r != "refused" && r != "reset" {
				t.Fatalf("%s: client saw %q, want an immediate refusal", addr, r)
			}
		}
		if s := took(); len(s) != 0 {
			t.Fatalf("listener saw %+v", s)
		}
	})
	t.Run("telemt reaches nothing that is not a datacentre, on any port", func(t *testing.T) {
		for _, addr := range []string{offDC + ":443", offDC + ":53", offDC + ":80", "[2001:db8:ffff::77]:443", "[2001:db8:ffff::77]:53"} {
			if r := dial(drillUID, addr); r != "refused" && r != "reset" {
				t.Fatalf("%s: telemt saw %q, want an immediate refusal", addr, r)
			}
		}
		if s := took(); len(s) != 0 {
			t.Fatalf("listener saw %+v", s)
		}
		// the agent itself still goes out the normal way
		if r := dial(0, offDC+":443"); r != "timeout" {
			t.Fatalf("root saw %q, want the normal (unanswered) path", r)
		}
	})
	t.Run("telemt still reaches loopback, and its v4 clients still get answers", func(t *testing.T) {
		lo, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(drillPort+2)))
		if err != nil {
			t.Fatal(err)
		}
		defer lo.Close()
		go func() {
			if c, err := lo.Accept(); err == nil {
				c.Write([]byte("ok"))
				c.Close()
			}
		}()
		if r := dial(drillUID, lo.Addr().String()); r != "connected ok" {
			t.Fatalf("telemt's loopback saw %q", r)
		}
		if r := v4Client(t); r != "connected ok" {
			t.Fatalf("a v4 client of telemt saw %q", r)
		}
	})
	t.Run("root, which is the agent itself, is never captured", func(t *testing.T) {
		if r := dial(0, dcV4+":443"); r != "timeout" {
			t.Fatalf("client saw %q", r)
		}
		if s := took(); len(s) != 0 {
			t.Fatalf("listener saw %+v", s)
		}
	})
	t.Run("with no v6 prefixes telemt gets no v6 at all, loopback aside", func(t *testing.T) {
		v4only, err := Rules{UID: drillUID, Port: drillPort, V4: rules.V4}.Script()
		if err != nil {
			t.Fatal(err)
		}
		if err := Apply(ctx, v4only); err != nil {
			t.Fatal(err)
		}
		if !Live(ctx, true, false) {
			t.Fatal("Live is false for the v4-only ruleset")
		}
		// telemt's own datacentre list has v6 addresses the panel did not send
		for _, addr := range []string{"[" + dcV6 + "]:443", "[2001:db8:ffff::77]:443", "[" + dcV6 + "]:8888"} {
			if r := dial(drillUID, addr); r != "refused" && r != "reset" {
				t.Fatalf("%s: telemt saw %q, want an immediate refusal", addr, r)
			}
		}
		if r := dial(0, "["+dcV6+"]:443"); r != "timeout" {
			t.Fatalf("root's v6 dial saw %q, want the normal (unanswered) path", r)
		}
		lo, err := net.Listen("tcp", net.JoinHostPort("::1", fmt.Sprint(drillPort+1)))
		if err != nil {
			t.Fatal(err)
		}
		defer lo.Close()
		go func() {
			if c, err := lo.Accept(); err == nil {
				c.Write([]byte("ok"))
				c.Close()
			}
		}()
		if r := dial(drillUID, lo.Addr().String()); r != "connected ok" {
			t.Fatalf("telemt's loopback v6 saw %q", r)
		}
		if r := dial(drillUID, dcV4+":443"); r != "connected ok" {
			t.Fatalf("v4 dial saw %q", r)
		}
		took()
		// what telemt starts is refused, not its answers to v6 clients
		if r := v6Client(t); r != "connected ok" {
			t.Fatalf("a v6 client of telemt saw %q", r)
		}
		// the refusal is part of the rules: without it they are not live
		if out, err := exec.Command("nft", "delete", "table", "ip6", Table).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if Live(ctx, true, false) {
			t.Fatal("Live is true with the v6 refusal gone")
		}
		if err := Apply(ctx, script); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("teardown removes the rules", func(t *testing.T) {
		if err := Apply(ctx, Teardown()); err != nil {
			t.Fatal(err)
		}
		if Present(ctx) {
			t.Fatal("tables survived the teardown")
		}
		if err := Apply(ctx, Teardown()); err != nil {
			t.Fatalf("teardown with nothing installed must be harmless: %v", err)
		}
		if r := dial(drillUID, dcV4+":443"); r != "timeout" {
			t.Fatalf("client saw %q after teardown", r)
		}
		if s := took(); len(s) != 0 {
			t.Fatalf("listener saw %+v after teardown", s)
		}
	})
}

// v6Client has a client in a namespace of its own, over a veth, talk to a
// listener running as telemt's uid; it returns what the client saw.
func v6Client(t *testing.T) string { t.Helper(); return peerClient(t, true) }
func v4Client(t *testing.T) string { t.Helper(); return peerClient(t, false) }

// peerClient connects to a stand-in telemt from another namespace, so the
// answers telemt sends leave through the rules like a real client's would.
func peerClient(t *testing.T, v6 bool) string {
	t.Helper()
	sh := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	peer := exec.Command("unshare", "--net", "--", "sleep", "30")
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { peer.Process.Kill(); peer.Wait() }()
	pid := fmt.Sprint(peer.Process.Pid)
	// unshare switches namespace after it starts: a veth sent too early lands in ours
	self, _ := os.Readlink("/proc/self/ns/net")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if ns, err := os.Readlink("/proc/" + pid + "/ns/net"); err == nil && ns != self {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the client's namespace never appeared")
		}
	}
	sh("ip", "link", "add", "vd0", "type", "veth", "peer", "name", "vd1", "netns", pid)
	defer exec.Command("ip", "link", "del", "vd0").Run()
	addr := "192.0.2.1:18080"
	if v6 {
		sh("ip", "-6", "addr", "add", "2001:db8:eeee::1/64", "dev", "vd0", "nodad")
		sh("nsenter", "-t", pid, "-n", "ip", "-6", "addr", "add", "2001:db8:eeee::2/64", "dev", "vd1", "nodad")
		addr = "[2001:db8:eeee::1]:18080"
	} else {
		sh("ip", "addr", "add", "192.0.2.1/24", "dev", "vd0")
		sh("nsenter", "-t", pid, "-n", "ip", "addr", "add", "192.0.2.2/24", "dev", "vd1")
	}
	sh("ip", "link", "set", "vd0", "up")
	sh("nsenter", "-t", pid, "-n", "ip", "link", "set", "lo", "up")
	sh("nsenter", "-t", pid, "-n", "ip", "link", "set", "vd1", "up")

	srv := exec.Command(os.Args[0], "-test.run", "^TestCaptureDrillServer$", "-test.v", "-test.count=1")
	srv.Env = append(os.Environ(), "MTPROTO_DRILL_LISTEN="+addr)
	srv.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: drillUID, Gid: drillUID}}
	out, err := srv.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Wait()
	buf := make([]byte, 4096)
	var seen string
	for !strings.Contains(seen, "READY") {
		n, err := out.Read(buf)
		if err != nil {
			t.Fatalf("the listener never came up: %q", seen)
		}
		seen += string(buf[:n])
	}
	go io.Copy(io.Discard, out)
	cl := exec.Command("nsenter", "-t", pid, "-n", os.Args[0], "-test.run", "^TestCaptureDrillClient$", "-test.v", "-test.count=1")
	cl.Env = append(os.Environ(), "MTPROTO_DRILL_DIAL="+addr)
	res, err := cl.CombinedOutput()
	if err != nil {
		t.Fatalf("client: %v: %s", err, res)
	}
	for _, l := range strings.Split(string(res), "\n") {
		if rest, ok := strings.CutPrefix(l, "RESULT "); ok {
			return rest
		}
	}
	t.Fatalf("the client printed no result: %s", res)
	return ""
}
