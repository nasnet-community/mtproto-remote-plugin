package installer_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/installer"
	"github.com/nasnet-community/mtproto-remote-plugin/link"
	"github.com/nasnet-community/mtproto-remote-plugin/reality"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops"
	"github.com/nasnet-community/mtproto-remote-plugin/release"
	"github.com/nasnet-community/mtproto-remote-plugin/tgnet"
)

// TestLiveInstall installs on a real box and takes it away again:
//
//	MTPROTO_LIVE_SSH=root@127.0.0.1:2222 MTPROTO_LIVE_KEY=~/.ssh/id_ed25519 \
//	  go test -count=1 -run TestLiveInstall -v ./installer
//
// MTPROTO_LIVE_ARCH overrides the box's own arch, MTPROTO_LIVE_DISGUISE names a
// cover site (without one the check result is ignored), MTPROTO_LIVE_HUB is the
// hub port as forwarded to this machine (default: the box's own), and
// MTPROTO_LIVE_DCNETS the prefixes to capture (default a documentation range, so
// nothing real is). TELEMT_CACHE is where the pinned telemt is kept. It changes
// the box, so it never runs unless asked.
func TestLiveInstall(t *testing.T) {
	spec, keyPath := os.Getenv("MTPROTO_LIVE_SSH"), os.Getenv("MTPROTO_LIVE_KEY")
	if spec == "" || keyPath == "" {
		t.Skip("set MTPROTO_LIVE_SSH=user@host:port and MTPROTO_LIVE_KEY=/path to install on a real box")
	}
	target := liveTarget(t, spec, keyPath)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	fp, err := installer.Probe(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("host key %s", fp)
	target.HostKey = fp
	c, err := installer.Connect(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	facts, err := installer.Inspect(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	arch := facts.Arch
	if v := os.Getenv("MTPROTO_LIVE_ARCH"); v != "" {
		if arch, err = release.ParseArch(v); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%s, %s, systemd %v, nft %v", facts.OS, arch, facts.HasSystemd, facts.HasNft)

	agent := buildAgent(t, arch)
	telemtBin := pinnedTelemt(t, ctx, arch)
	disguise, unverified := os.Getenv("MTPROTO_LIVE_DISGUISE"), false
	if disguise == "" {
		disguise, unverified = "example.com", true
	}
	creds, err := installer.NewCreds()
	if err != nil {
		t.Fatal(err)
	}
	progress := func(s installer.Step) { t.Logf("%-12s %-7s %s", s.Name, s.State, s.Detail) }

	uninstalled := false
	defer func() {
		if !uninstalled { // never leave a half-installed box behind
			installer.Uninstall(context.Background(), c, progress)
		}
	}()
	res, err := installer.Install(ctx, c, installer.Request{
		Agent: agent, Telemt: telemtBin, Disguise: disguise, Creds: creds,
		AllowUnverifiedDisguise: unverified,
	}, progress)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Logf("agent %s, hub port %d, capture port %d, telemt uid %d, cover site ok %v",
		res.AgentID, res.HubPort, res.CapturePort, res.TelemtUID, res.Disguise.OK)

	poll(t, "the service to be active", func() bool {
		out, _ := c.Root(ctx, "systemctl is-active "+installer.UnitName+" || true", nil)
		return strings.TrimSpace(out) == "active"
	})
	poll(t, "the hub port to listen", func() bool {
		out, _ := c.Root(ctx, "ss -ltn", nil)
		return strings.Contains(out, ":"+strconv.Itoa(res.HubPort)+" ")
	})
	hubAddr := os.Getenv("MTPROTO_LIVE_HUB")
	if hubAddr == "" {
		hubAddr = net.JoinHostPort(target.Host, strconv.Itoa(res.HubPort))
	}

	// a hub of our own: telemt only starts once a state is pushed
	var dcNets []string
	for _, p := range strings.Split(cmpOr(os.Getenv("MTPROTO_LIVE_DCNETS"), "203.0.113.0/24"), ",") {
		dcNets = append(dcNets, strings.TrimSpace(p))
	}
	var prefixes []netip.Prefix
	for _, p := range dcNets {
		prefixes = append(prefixes, netip.MustParsePrefix(p))
	}
	var d net.Dialer
	hub := link.NewHub(link.HubConfig{
		AgentID: res.AgentID, Token: res.Creds.Token, Addr: hubAddr,
		Reality: reality.ClientConfig{ServerName: strings.ToLower(disguise), PublicKey: res.Creds.Pub, ShortID: res.Creds.ShortID},
		Conns:   []link.ConnSpec{{Name: "live", Dial: d.DialContext}},
		Allow:   tgnet.New(prefixes), DialDC: d.DialContext,
	})
	hctx, stopHub := context.WithCancel(ctx)
	defer stopHub()
	go hub.Run(hctx)
	wctx, wcancel := context.WithTimeout(ctx, 2*time.Minute)
	if err := hub.WaitUp(wctx); err != nil {
		wcancel()
		t.Fatalf("the hub never reached the agent at %s: %v", hubAddr, err)
	}
	wcancel()
	status := func() ctrl.Status {
		var st ctrl.Status
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if err := hub.Call(cctx, ctrl.MStatus, nil, &st); err != nil {
			t.Fatalf("status: %v", err)
		}
		return st
	}
	if st := status(); st.Telemt.Running {
		t.Fatal("telemt runs before any state was pushed")
	}
	secret := make([]byte, 16)
	rand.Read(secret)
	var ack ctrl.StateAck
	if err := hub.Call(ctx, ctrl.MStateApply, ctrl.State{
		Rev: "live-1", Port: installer.MTProtoPort, Disguise: strings.ToLower(disguise),
		Users:  []ctrl.User{{Name: "live", Secret: hex.EncodeToString(secret), Enabled: true}},
		DCNets: dcNets,
	}, &ack); err != nil || ack.Rev != "live-1" {
		t.Fatalf("state.apply: %+v %v", ack, err)
	}
	poll2 := func(what string, within time.Duration, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(within); !ok(); time.Sleep(2 * time.Second) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	poll2("telemt to run behind the capture", 3*time.Minute, func() bool {
		st := status()
		return st.Telemt.Running && st.Capture.Rules && st.StateRev == "live-1"
	})
	t.Logf("telemt %s running, rules in place", status().TelemtVersion)
	out, err := c.Root(ctx, "nft list table ip mtproto_agent >/dev/null && echo tables; stat -c '%a %U:%G %n' "+
		installer.DataDir+" "+installer.TelemtDir+" "+installer.TelemtRun+" "+installer.TelemtDir+"/config.toml; "+
		"pgrep -u "+installer.TelemtUser+" -x telemt >/dev/null && echo telemt-as-its-user", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tables", "711 root:root " + installer.DataDir, "750 root:telemt " + installer.TelemtDir,
		"700 telemt:telemt " + installer.TelemtRun, "640 root:telemt " + installer.TelemtDir + "/config.toml", "telemt-as-its-user"} {
		if !strings.Contains(out, want) {
			t.Errorf("on the box: want %q in\n%s", want, out)
		}
	}

	// the agent's own removal, over the link
	if err := hub.Call(ctx, ctrl.MUninstall, nil, nil); err != nil {
		t.Fatalf("uninstall over the link: %v", err)
	}
	// gone, or at least stopped and disabled with its remover done: "not active" alone is a restart loop's gap
	unitGone := func() bool {
		out, _ := c.Root(ctx, "systemctl show -p LoadState -p ActiveState -p UnitFileState "+installer.UnitName+"; "+
			"systemctl is-active --quiet "+installer.UnitName+"-remove && echo remover-running; true", nil)
		return !strings.Contains(out, "remover-running") && (strings.Contains(out, "LoadState=not-found") ||
			strings.Contains(out, "ActiveState=inactive") && strings.Contains(out, "UnitFileState=disabled"))
	}
	poll2("the agent's unit to be gone", 2*time.Minute, unitGone)
	time.Sleep(5 * time.Second) // past RestartSec: a unit that comes back is not gone
	if !unitGone() {
		t.Error("the agent's unit came back after its own removal")
	}
	stopHub()
	check := []string{}
	for _, p := range []string{installer.ConfDir, installer.DataDir, installer.UnitPath, installer.AgentBin, installer.TelemtBin} {
		check = append(check, fmt.Sprintf("test -e %s && echo 'left: %s'", p, p))
	}
	check = append(check,
		"id "+installer.TelemtUser+" >/dev/null 2>&1 && echo 'left: the "+installer.TelemtUser+" user'",
		"pgrep -u "+installer.TelemtUser+" >/dev/null 2>&1 && echo 'left: processes of "+installer.TelemtUser+"'",
		"nft list table ip mtproto_agent >/dev/null 2>&1 && echo 'left: nft ip table'",
		"nft list table ip6 mtproto_agent >/dev/null 2>&1 && echo 'left: nft ip6 table'",
		"systemctl is-active --quiet "+installer.UnitName+" && echo 'left: the service is running'",
		"true")
	leftovers := func() string {
		out, err := c.Root(ctx, strings.Join(check, "; "), nil)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	if left := leftovers(); left != "" {
		t.Errorf("the agent's own removal left things behind:\n%s", left)
	}

	// and the sweep a hub runs over SSH when it has a login
	uninstalled = true
	if err := installer.Uninstall(ctx, c, progress); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if left := leftovers(); left != "" {
		t.Fatalf("uninstall left things behind:\n%s", left)
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func liveTarget(t *testing.T, spec, keyPath string) installer.Target {
	t.Helper()
	user, hostport, ok := strings.Cut(spec, "@")
	if !ok {
		t.Fatalf("MTPROTO_LIVE_SSH=%q is not user@host:port", spec)
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		t.Fatalf("MTPROTO_LIVE_SSH=%q: %v", spec, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("MTPROTO_LIVE_SSH=%q: bad port", spec)
	}
	if rest, ok := strings.CutPrefix(keyPath, "~/"); ok {
		home, _ := os.UserHomeDir()
		keyPath = filepath.Join(home, rest)
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return installer.Target{Host: host, Port: p, User: user, Key: key, Timeout: 30 * time.Second}
}

// buildAgent builds the agent from this tree with the release flags.
func buildAgent(t *testing.T, arch release.Arch) installer.Bin {
	t.Helper()
	out := filepath.Join(t.TempDir(), "mtproto-agent")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags", "-s -w -X main.version=0.0.0-live",
		"-o", out, "../cmd/mtproto-agent")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+string(arch))
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the agent: %v\n%s", err, b)
	}
	return fileBin(t, out)
}

func fileBin(t *testing.T, path string) installer.Bin {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		t.Fatal(err)
	}
	return installer.Bin{Open: func() (io.ReadCloser, error) { return os.Open(path) }, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}
}

// pinnedTelemt is the release telemt, fetched and checked the way a hub does it.
func pinnedTelemt(t *testing.T, ctx context.Context, arch release.Arch) installer.Bin {
	t.Helper()
	dir := os.Getenv("TELEMT_CACHE")
	if dir == "" {
		c, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		dir = filepath.Join(c, "mtproto-drill")
	}
	b := relayops.NewBinaries(dir, func() *http.Client { return &http.Client{Timeout: 3 * time.Minute} })
	bin, err := b.Telemt(ctx, arch)
	if err != nil {
		t.Fatalf("the pinned telemt: %v", err)
	}
	return bin
}

func poll(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(45 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Second)
	}
}
