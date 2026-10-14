//go:build linux

package drill

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
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
)

const (
	telemtUID = 998
	stSynSent = "02"
	stListen  = "0A"
)

// drillDir is a scratch directory the unprivileged uid can read, holding a copy
// of this test binary for the re-exec.
func drillDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("the drill needs root")
	}
	for _, tool := range []string{"unshare", "nft", "ip"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	dir, err := os.MkdirTemp("", "mtproto-drill")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	os.Chmod(dir, 0o755)
	copyFile(t, os.Args[0], filepath.Join(dir, "drill.test"), 0o755)
	return dir
}

func copyFile(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dst, mode)
}

// runInner re-executes test name in fresh namespaces and streams its output, so
// an hour-long run shows progress as it goes.
func runInner(t *testing.T, dir, name string, unshareArgs []string, env ...string) {
	t.Helper()
	ctx := context.Background()
	if dl, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, dl.Add(-10*time.Second))
		defer cancel()
	}
	args := append(append([]string{}, unshareArgs...), "--", filepath.Join(dir, "drill.test"),
		"-test.run", "^"+name+"$", "-test.v", "-test.count=1", "-test.timeout=0")
	cmd := exec.CommandContext(ctx, "unshare", args...)
	cmd.Env = append(append(os.Environ(), "MTPROTO_NETNS_INNER=1", "MTPROTO_DRILL_DIR="+dir), env...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s failed: %v", name, err)
	}
}

func sh(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// netnsBasics gives the namespace a loopback and a dead uplink, so a dial that
// is not captured has a route to hang on instead of failing at once.
func netnsBasics(t *testing.T) {
	t.Helper()
	sh(t, "ip", "link", "set", "lo", "up")
	sh(t, "ip", "link", "add", "dummy0", "type", "dummy")
	sh(t, "ip", "link", "set", "dummy0", "up")
	sh(t, "ip", "addr", "add", "198.51.100.2/24", "dev", "dummy0")
	sh(t, "ip", "route", "add", "default", "via", "198.51.100.1", "dev", "dummy0")
	sh(t, "ip", "-6", "addr", "add", "2001:db8:ffff::2/64", "dev", "dummy0", "nodad")
	sh(t, "ip", "-6", "route", "add", "default", "via", "2001:db8:ffff::1", "dev", "dummy0")
}

type sock struct {
	local, remote netip.AddrPort
	state         string
	uid           int
}

// sockets reads every TCP socket in the namespace from /proc/net/tcp{,6}.
func sockets() []sock {
	var out []sock
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		for _, line := range lines[1:] {
			fs := strings.Fields(line)
			if len(fs) < 8 {
				continue
			}
			l, err1 := procAddr(fs[1])
			r, err2 := procAddr(fs[2])
			uid, err3 := strconv.Atoi(fs[7])
			if err1 != nil || err2 != nil || err3 != nil {
				continue
			}
			out = append(out, sock{l, r, fs[3], uid})
		}
	}
	return out
}

// procAddr decodes "0100007F:01BB"; each 32-bit word is printed in host order.
func procAddr(s string) (netip.AddrPort, error) {
	h, p, ok := strings.Cut(s, ":")
	if !ok || len(h)%8 != 0 {
		return netip.AddrPort{}, fmt.Errorf("bad address %q", s)
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}
	raw := make([]byte, len(h)/2)
	for i := 0; i < len(h); i += 8 {
		v, err := strconv.ParseUint(h[i:i+8], 16, 32)
		if err != nil {
			return netip.AddrPort{}, err
		}
		binary.NativeEndian.PutUint32(raw[i/2:], uint32(v))
	}
	addr, ok := netip.AddrFromSlice(raw)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("bad address %q", s)
	}
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), nil
}

func listening(port int) bool {
	for _, s := range sockets() {
		if s.state == stListen && int(s.local.Port()) == port {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// lockedBuffer collects a child's output for the failure message.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
