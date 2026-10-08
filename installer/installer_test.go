package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/nasnet-community/mtproto-remote-plugin/agent/state"
)

const fakeAgent = `#!/bin/sh
case "$1" in
 version) echo "agent test";;
 disguise-check) echo '{"ok":true,"tls13":true,"x25519":true,"h2":true,"cert_ok":true,"latency_ms":3}';;
esac
exit 0
`

func bin(content string) Bin {
	h := sha256.Sum256([]byte(content))
	return Bin{
		Open:   func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(content)), nil },
		Size:   int64(len(content)),
		SHA256: hex.EncodeToString(h[:]),
	}
}

func request(t *testing.T) Request {
	t.Helper()
	creds, err := NewCreds()
	if err != nil {
		t.Fatal(err)
	}
	return Request{Agent: bin(fakeAgent), Telemt: bin("telemt-binary-bytes"), Disguise: "Mirror.Example.ir", AgentID: "agent-xyz", Creds: creds}
}

func connect(t *testing.T, h *fakeHost) *Conn {
	t.Helper()
	c, err := Connect(context.Background(), h.target())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestProbeReturnsFingerprintWithoutSendingCredentials(t *testing.T) {
	h := startFakeHost(t)
	tg := h.target()
	tg.HostKey = ""
	fp, err := Probe(context.Background(), tg)
	if err != nil || fp != h.fp {
		t.Fatalf("fp=%q err=%v want %q", fp, err, h.fp)
	}
	if h.attempts.Load() != 0 {
		t.Fatal("Probe authenticated: the password went to a host nobody had vouched for")
	}
}

func TestConnectRefusesAMovedHostKeyAndNeverLogsIn(t *testing.T) {
	h := startFakeHost(t)
	tg := h.target()
	tg.HostKey = "SHA256:somethingElse"
	_, err := Connect(context.Background(), tg)
	var hk *HostKeyError
	if !errors.As(err, &hk) || hk.Got != h.fp {
		t.Fatalf("err = %v", err)
	}
	if h.attempts.Load() != 0 {
		t.Fatal("credentials were sent to a server with the wrong key")
	}
	tg.HostKey = ""
	if _, err := Connect(context.Background(), tg); err == nil {
		t.Fatal("Connect without a pinned key succeeded")
	}
}

func TestConnectWithAKey(t *testing.T) {
	h := startFakeHost(t)
	pub, priv := genKey(t)
	h.authKey = pub
	tg := h.target()
	tg.Password, tg.Key = "", priv
	c, err := Connect(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestInspect(t *testing.T) {
	h := startFakeHost(t)
	f, err := Inspect(context.Background(), connect(t, h))
	if err != nil {
		t.Fatal(err)
	}
	if f.Arch != "amd64" || !f.IsRoot || !f.HasSystemd || !f.HasNft || f.OS != "Fake Linux 12" || f.Installed {
		t.Fatalf("%+v", f)
	}
}

func TestInstallAsRootEndToEnd(t *testing.T) {
	h := startFakeHost(t)
	var steps []Step
	req := request(t)
	res, err := Install(context.Background(), connect(t, h), req, func(s Step) { steps = append(steps, s) })
	if err != nil {
		t.Fatal(err)
	}

	if got := h.read("usr/local/bin/mtproto-agent"); got != fakeAgent {
		t.Errorf("agent binary: %q", got)
	}
	if got := h.read("usr/local/bin/telemt"); got != "telemt-binary-bytes" {
		t.Errorf("telemt binary: %q", got)
	}
	if h.exists("usr/local/bin/mtproto-agent.upload") {
		t.Error("temp upload left behind")
	}
	st, err := os.Stat(h.path("etc/mtproto-agent/state.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("state.json: %v mode=%v", err, st)
	}
	var f state.File
	if err := json.Unmarshal([]byte(h.read("etc/mtproto-agent/state.json")), &f); err != nil {
		t.Fatal(err)
	}
	if f.AgentID != "agent-xyz" || !bytes.Equal(f.RealityPriv, req.Creds.Priv) || !bytes.Equal(f.Tokens[0], req.Creds.Token) ||
		f.Disguise != "mirror.example.ir" || f.HubPort != 8443 || f.CapturePort != 9443 || f.TelemtUID != 998 {
		t.Fatalf("state = %+v", f)
	}
	unit := h.read("etc/systemd/system/mtproto-agent.service")
	for _, want := range []string{"ExecStart=/usr/local/bin/mtproto-agent run", "Restart=always", "StandardOutput=null", "GOMEMLIMIT=80MiB"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q", want)
		}
	}
	if !strings.Contains(h.read("systemctl.log"), "restart mtproto-agent") || !h.exists("active/mtproto-agent") {
		t.Errorf("service not started: %q", h.read("systemctl.log"))
	}
	if res.AgentID != "agent-xyz" || res.TelemtUID != 998 || !res.Disguise.OK || res.HubPort != 8443 {
		t.Errorf("result = %+v", res)
	}

	// progress reports every phase, each ending ok
	want := []string{"check", "verify", "account", "upload", "directories", "disguise", "credentials", "service"}
	var okSteps []string
	for _, s := range steps {
		if s.State == "failed" {
			t.Errorf("step %s failed: %s", s.Name, s.Detail)
		}
		if s.State == "ok" {
			okSteps = append(okSteps, s.Name)
		}
	}
	if strings.Join(okSteps, ",") != strings.Join(want, ",") {
		t.Errorf("steps: %v", okSteps)
	}
}

func TestInstallAsSudoUserNeverPutsThePasswordOnACommandLine(t *testing.T) {
	h := startFakeHost(t)
	h.user = "ubuntu"
	h.env["FAKE_UID"] = "1000"
	tg := h.target()
	c, err := Connect(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.IsRoot() {
		t.Fatal("a uid 1000 login reported root")
	}
	// the login password differs from the sudo one in the fake; use the sudo password
	c.t.Password = "sudopw"
	if _, err := Install(context.Background(), c, request(t), nil); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range h.commands() {
		if strings.Contains(cmd, "sudopw") || strings.Contains(cmd, "hunter2") {
			t.Fatalf("a password reached a command line: %q", cmd)
		}
	}
	if !h.exists("usr/local/bin/mtproto-agent") {
		t.Fatal("not installed")
	}
}

// With NOPASSWD sudo reads nothing, so a password written to its stdin would
// become the first line of the command's input: the upload, here.
func TestNopasswdSudoNeverSeesThePassword(t *testing.T) {
	h := startFakeHost(t)
	h.user = "ubuntu"
	h.env["FAKE_UID"] = "1000"
	h.env["FAKE_SUDO_NOPASSWD"] = "1"
	if _, err := Install(context.Background(), connect(t, h), request(t), nil); err != nil {
		t.Fatal(err)
	}
	if got := h.read("usr/local/bin/mtproto-agent"); got != fakeAgent {
		t.Fatalf("the agent arrived as %q", got)
	}
	filepath.WalkDir(h.root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), h.password) {
				t.Errorf("the login password reached %s", p)
			}
		}
		return nil
	})
	for _, l := range strings.Split(strings.TrimSpace(h.read("sudo.log")), "\n") {
		if !strings.HasPrefix(l, "sudo -n ") {
			t.Errorf("a password-less sudo was asked for a password: %q", l)
		}
	}
}

// Asking for the password every time (-k) is what makes it safe to send: a
// cached sudo credential would otherwise leave it unread on stdin.
func TestPasswordSudoAlwaysReadsThePassword(t *testing.T) {
	h := startFakeHost(t)
	h.user = "ubuntu"
	h.env["FAKE_UID"] = "1000"
	h.password = "sudopw"
	if _, err := Install(context.Background(), connect(t, h), request(t), nil); err != nil {
		t.Fatal(err)
	}
	if got := h.read("usr/local/bin/mtproto-agent"); got != fakeAgent {
		t.Fatalf("the agent arrived as %q", got)
	}
	var asked int
	for _, l := range strings.Split(strings.TrimSpace(h.read("sudo.log")), "\n") {
		switch {
		case strings.HasPrefix(l, "sudo -n true"): // the probe
		case strings.HasPrefix(l, "sudo -Sk "):
			asked++
		default:
			t.Errorf("unexpected sudo use: %q", l)
		}
	}
	if asked == 0 {
		t.Fatal("no command went through sudo")
	}
}

// A host that accepts TCP but never speaks SSH must not hold a job forever.
func TestSSHHandshakeHasADeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
			defer c.Close() // held open, silent
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	tg := Target{Host: "127.0.0.1", Port: port, User: "root", Password: "x", HostKey: "SHA256:x", Timeout: 300 * time.Millisecond}
	for name, f := range map[string]func(context.Context, Target) error{
		"probe":   func(ctx context.Context, t Target) error { _, err := Probe(ctx, t); return err },
		"connect": func(ctx context.Context, t Target) error { _, err := Connect(ctx, t); return err },
	} {
		start := time.Now()
		if err := f(context.Background(), tg); err == nil {
			t.Fatalf("%s: a silent host passed", name)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%s: took %v with a 300ms timeout", name, d)
		}
		// the caller's context ends it too
		long := tg
		long.Timeout = time.Minute
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		start = time.Now()
		err := f(ctx, long)
		cancel()
		if err == nil || time.Since(start) > 3*time.Second {
			t.Errorf("%s: err %v after %v with a cancelled context", name, err, time.Since(start))
		}
	}
}

// The hub decides how SSH leaves it (router mode pins a path), so every dial
// goes through Target.Dial when it is set.
func TestTargetDialIsUsedForEverySSHConnection(t *testing.T) {
	h := startFakeHost(t)
	var calls []string
	var d net.Dialer
	tg := h.target()
	tg.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		calls = append(calls, network+" "+addr)
		return d.DialContext(ctx, network, addr)
	}
	if _, err := Probe(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	c, err := Connect(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	want := "tcp " + h.ln.Addr().String()
	if len(calls) != 2 || calls[0] != want || calls[1] != want {
		t.Fatalf("dials = %q, want two of %q", calls, want)
	}
}

func TestWrongSudoPasswordFailsEarly(t *testing.T) {
	h := startFakeHost(t)
	h.user = "ubuntu"
	h.env["FAKE_UID"] = "1000"
	c, err := Connect(context.Background(), h.target()) // login works; sudo password in the fake is "sudopw"
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = Install(context.Background(), c, request(t), nil)
	if err == nil || !strings.Contains(err.Error(), "incorrect password") {
		t.Fatalf("err = %v", err)
	}
	if h.exists("usr/local/bin/mtproto-agent") {
		t.Fatal("something was installed despite no root")
	}
}

func TestInstallRefusesBeforeChangingAnything(t *testing.T) {
	cases := map[string]struct {
		setup func(*fakeHost)
		req   func(*Request)
		want  string
	}{
		"no systemd":       {func(h *fakeHost) { h.removeTool("systemctl") }, nil, "systemd"},
		"unsupported arch": {func(h *fakeHost) { h.env["FAKE_ARCH"] = "armv7l" }, nil, "not supported"},
		"port 443 taken": {func(h *fakeHost) {
			os.WriteFile(h.path("listening"), []byte("LISTEN 0 1 0.0.0.0:443 0.0.0.0:*\n"), 0o644)
		}, nil, "port 443"},
		"hub port taken": {func(h *fakeHost) {
			os.WriteFile(h.path("listening"), []byte("LISTEN 0 1 0.0.0.0:8443 0.0.0.0:*\n"), 0o644)
		}, nil, "port 8443"},
		// the agent would crash-loop on it while a brief hub-port listen passed for a start
		"capture port taken": {func(h *fakeHost) {
			os.WriteFile(h.path("listening"), []byte("LISTEN 0 1 127.0.0.1:9443 0.0.0.0:*\n"), 0o644)
		}, nil, "port 9443"},
		"agent hash is wrong": {nil, func(r *Request) { r.Agent.SHA256 = strings.Repeat("0", 64) }, "pinned hash"},
		"telemt size is off":  {nil, func(r *Request) { r.Telemt.Size++ }, "pinned hash"},
		"no agent given":      {nil, func(r *Request) { r.Agent = Bin{} }, "no agent binary"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := startFakeHost(t)
			if tc.setup != nil {
				tc.setup(h)
			}
			req := request(t)
			if tc.req != nil {
				tc.req(&req)
			}
			_, err := Install(context.Background(), connect(t, h), req, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if h.exists("usr/local/bin/mtproto-agent") || h.exists("etc/mtproto-agent/state.json") || h.exists("users/telemt") {
				t.Fatal("the box was changed before the refusal")
			}
		})
	}
}

func TestCorruptedUploadIsCaughtOnTheVPSAndCleanedUp(t *testing.T) {
	h := startFakeHost(t)
	h.env["FAKE_BAD_SHA"] = "1"
	_, err := Install(context.Background(), connect(t, h), request(t), nil)
	if err == nil || !strings.Contains(err.Error(), "corrupted") {
		t.Fatalf("err = %v", err)
	}
	if h.exists("usr/local/bin/mtproto-agent") || h.exists("usr/local/bin/mtproto-agent.upload") {
		t.Fatal("a binary that failed its hash was left or installed")
	}
}

// telemt runs as its own uid: it must get through the data dir to its config
// and into a working dir of its own. A 0700 data dir locked it out.
func TestDataDirLetsTelemtThrough(t *testing.T) {
	h := startFakeHost(t)
	os.MkdirAll(h.path("var/lib/mtproto-agent"), 0o700) // what an older install left
	if _, err := Install(context.Background(), connect(t, h), request(t), nil); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		"var/lib/mtproto-agent":            0o711,
		"var/lib/mtproto-agent/telemt":     0o750,
		"var/lib/mtproto-agent/telemt/run": 0o700,
		"etc/mtproto-agent":                0o700,
	} {
		st, err := os.Stat(h.path(path))
		if err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", path, err, st, want)
		}
	}
}

// A failed install says what it found, so the caller knows whether the box had
// an agent before and may be cleaned up.
func TestAFailedInstallReportsWhatWasThere(t *testing.T) {
	h := startFakeHost(t)
	h.env["FAKE_NO_START"] = "1"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := Install(ctx, connect(t, h), request(t), nil)
	if err == nil {
		t.Fatal("install passed with an agent that never started")
	}
	if res == nil || res.Facts.Installed || res.Facts.Arch != "amd64" || !res.Started {
		t.Fatalf("result = %+v", res)
	}
	// a refusal in the checks changed nothing, and says so
	h2 := startFakeHost(t)
	os.WriteFile(h2.path("listening"), []byte("LISTEN 0 1 0.0.0.0:443 0.0.0.0:*\n"), 0o644)
	if res, err := Install(context.Background(), connect(t, h2), request(t), nil); err == nil || res == nil || res.Started {
		t.Fatalf("port check: %+v %v", res, err)
	}
	// the box afterwards: an uninstall takes the half-install away completely
	if err := Uninstall(context.Background(), connect(t, h), nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"usr/local/bin/mtproto-agent", "usr/local/bin/telemt", "etc/mtproto-agent/state.json", "var/lib/mtproto-agent", "etc/systemd/system/mtproto-agent.service", "users/telemt", "active/mtproto-agent"} {
		if h.exists(p) {
			t.Errorf("%s survived", p)
		}
	}
	// and a second try checks the ports again: nothing says "installed" any more
	f, err := Inspect(context.Background(), connect(t, h))
	if err != nil || f.Installed {
		t.Fatalf("after the clean-up: %+v %v", f, err)
	}
}

// Error text reaches the UI as it is: it says cover site.
func TestTheCoverSiteIsCalledThat(t *testing.T) {
	bad := strings.Replace(fakeAgent, `"ok":true`, `"ok":false,"err":"no h2"`, 1)
	h := startFakeHost(t)
	req := request(t)
	req.Agent = bin(bad)
	_, err := Install(context.Background(), connect(t, h), req, nil)
	if err == nil || !strings.Contains(err.Error(), "cover site") || strings.Contains(err.Error(), "disguise") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnusableDisguiseStopsTheInstallUnlessAllowed(t *testing.T) {
	bad := strings.Replace(fakeAgent, `"ok":true`, `"ok":false,"err":"no h2"`, 1)
	h := startFakeHost(t)
	req := request(t)
	req.Agent = bin(bad)
	_, err := Install(context.Background(), connect(t, h), req, nil)
	if err == nil || !strings.Contains(err.Error(), "does not work as a cover site") || !strings.Contains(err.Error(), "no h2") {
		t.Fatalf("err = %v", err)
	}
	if h.exists("etc/systemd/system/mtproto-agent.service") || h.exists("etc/mtproto-agent/state.json") {
		t.Fatal("a service was set up despite the bad disguise")
	}
	req.AllowUnverifiedDisguise = true
	if _, err := Install(context.Background(), connect(t, h), req, nil); err != nil {
		t.Fatalf("with the override: %v", err)
	}
}

func TestMissingNftIsInstalledFromThePackageManager(t *testing.T) {
	h := startFakeHost(t)
	h.removeTool("nft")
	if _, err := Install(context.Background(), connect(t, h), request(t), nil); err != nil {
		t.Fatal(err)
	}
	// a fresh VPS often has no package lists yet
	if log := h.read("apt.log"); !strings.Contains(log, "install -y nftables") || !strings.Contains(log, " update") ||
		strings.Index(log, " update") > strings.Index(log, "install -y nftables") {
		t.Fatalf("apt log: %q", log)
	}
	// a failed update is not fatal; a failed install says what it was doing
	hu := startFakeHost(t)
	hu.removeTool("nft")
	hu.env["FAKE_APT_UPDATE_FAIL"] = "1"
	if _, err := Install(context.Background(), connect(t, hu), request(t), nil); err != nil {
		t.Fatalf("with apt-get update failing: %v", err)
	}
	hi := startFakeHost(t)
	hi.removeTool("nft")
	hi.env["FAKE_APT_UPDATE_FAIL"], hi.env["FAKE_APT_INSTALL_FAIL"] = "1", "1"
	if _, err := Install(context.Background(), connect(t, hi), request(t), nil); err == nil ||
		!strings.Contains(err.Error(), "could not install nftables") || !strings.Contains(err.Error(), "package lists") {
		t.Fatalf("with apt-get install failing: %v", err)
	}
	h2 := startFakeHost(t)
	h2.removeTool("nft")
	h2.removeTool("apt-get")
	if _, err := Install(context.Background(), connect(t, h2), request(t), nil); err == nil || !strings.Contains(err.Error(), "package manager") {
		t.Fatalf("no package manager: %v", err)
	}
}

func TestServiceThatNeverListensFails(t *testing.T) {
	h := startFakeHost(t)
	h.env["FAKE_NO_START"] = "1"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := Install(ctx, connect(t, h), request(t), nil)
	if err == nil {
		t.Fatal("install reported success for an agent that never started")
	}
}

func TestRepairKeepsTheCredentialsOnTheBox(t *testing.T) {
	h := startFakeHost(t)
	first := request(t)
	if _, err := Install(context.Background(), connect(t, h), first, nil); err != nil {
		t.Fatal(err)
	}
	before := h.read("etc/mtproto-agent/state.json")

	second := request(t) // fresh credentials the hub does NOT want installed
	second.KeepState = true
	res, err := Install(context.Background(), connect(t, h), second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.read("etc/mtproto-agent/state.json") != before {
		t.Fatal("repair rewrote the credentials")
	}
	if !bytes.Equal(res.Creds.Priv, first.Creds.Priv) || !bytes.Equal(res.Creds.Token, first.Creds.Token) || res.Creds.ShortID != first.Creds.ShortID {
		t.Fatal("the result does not describe what is on the box")
	}
	if !bytes.Equal(res.Creds.Pub, first.Creds.Pub) {
		t.Fatal("public key not re-derived from the stored private key")
	}
	// a reinstall over a running agent must not trip the port check
	if !res.Facts.Installed {
		t.Error("the earlier install was not noticed")
	}
}

func TestRepairWithNothingOnTheBoxWritesFreshState(t *testing.T) {
	h := startFakeHost(t)
	req := request(t)
	req.KeepState = true
	res, err := Install(context.Background(), connect(t, h), req, nil)
	if err != nil || !bytes.Equal(res.Creds.Priv, req.Creds.Priv) {
		t.Fatalf("err=%v", err)
	}
	if !h.exists("etc/mtproto-agent/state.json") {
		t.Fatal("no state written")
	}
}

func TestUninstallRemovesEverything(t *testing.T) {
	h := startFakeHost(t)
	if _, err := Install(context.Background(), connect(t, h), request(t), nil); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(context.Background(), connect(t, h), nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"usr/local/bin/mtproto-agent", "usr/local/bin/telemt", "etc/mtproto-agent", "var/lib/mtproto-agent", "etc/systemd/system/mtproto-agent.service", "users/telemt", "active/mtproto-agent"} {
		if h.exists(p) {
			t.Errorf("%s survived the uninstall", p)
		}
	}
	if !strings.Contains(h.read("nft.log"), "delete table ip mtproto_agent") {
		t.Errorf("capture rules not removed: %q", h.read("nft.log"))
	}
	// and it is safe to run twice
	if err := Uninstall(context.Background(), connect(t, h), nil); err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
}

func TestShq(t *testing.T) {
	for in, want := range map[string]string{
		"plain":  `'plain'`,
		"it's":   `'it'\''s'`,
		"a b;rm": `'a b;rm'`,
		"":       `''`,
		"$(x)":   `'$(x)'`,
	} {
		if got := shq(in); got != want {
			t.Errorf("shq(%q) = %s, want %s", in, got, want)
		}
	}
}

func genKey(t *testing.T) (ssh.PublicKey, []byte) {
	t.Helper()
	pubRaw, privRaw := mustEd25519(t)
	signer, _ := ssh.NewSignerFromKey(privRaw)
	_ = pubRaw
	der, err := x509.MarshalPKCS8PrivateKey(privRaw)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey(), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// A fresh install over a running agent would replace its credentials and cut
// its node off: it is refused, and a repair (which may reset them) is the way.
func TestAFreshInstallRefusesABoxThatRunsAnAgent(t *testing.T) {
	h := startFakeHost(t)
	if _, err := Install(context.Background(), connect(t, h), request(t), nil); err != nil {
		t.Fatal(err)
	}
	before := h.read("etc/mtproto-agent/state.json")
	res, err := Install(context.Background(), connect(t, h), request(t), nil)
	if err == nil || !strings.Contains(err.Error(), "already runs mtproto-agent") || !strings.Contains(err.Error(), "repair") {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.Started || !res.Facts.Installed {
		t.Fatalf("result = %+v", res)
	}
	if h.read("etc/mtproto-agent/state.json") != before {
		t.Fatal("the running agent's credentials were replaced")
	}
	req := request(t)
	req.Repair = true // a reset repair replaces them on purpose
	res, err = Install(context.Background(), connect(t, h), req, nil)
	if err != nil || h.read("etc/mtproto-agent/state.json") == before || !bytes.Equal(res.Creds.Priv, req.Creds.Priv) {
		t.Fatalf("reset repair: %v", err)
	}
}

// A telemt from an earlier direct deploy is someone else's: the install stops
// before replacing it.
func TestAnotherTelemtOnTheBoxIsNotReplaced(t *testing.T) {
	h := startFakeHost(t)
	os.WriteFile(h.path("usr/local/bin/telemt"), []byte("their telemt"), 0o755)
	res, err := Install(context.Background(), connect(t, h), request(t), nil)
	if err == nil || !strings.Contains(err.Error(), "/usr/local/bin/telemt") {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.Started {
		t.Fatalf("result = %+v", res)
	}
	if h.read("usr/local/bin/telemt") != "their telemt" || h.exists("usr/local/bin/mtproto-agent") || h.exists("users/telemt") {
		t.Fatal("the box was changed")
	}
}

// An account that was there before the install is never deleted: not by an
// uninstall, not by the clean-up after a failed install.
func TestATelemtAccountThatWasThereStays(t *testing.T) {
	theirs := func(h *fakeHost) {
		os.MkdirAll(h.path("users"), 0o755)
		os.WriteFile(h.path("users/telemt"), nil, 0o644)
	}
	h := startFakeHost(t)
	theirs(h)
	res, err := Install(context.Background(), connect(t, h), request(t), nil)
	if err != nil || !res.Facts.OtherUser {
		t.Fatalf("install: %+v %v", res, err)
	}
	keep := request(t)
	keep.KeepState = true
	if _, err := Install(context.Background(), connect(t, h), keep, nil); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(context.Background(), connect(t, h), nil); err != nil {
		t.Fatal(err)
	}
	if !h.exists("users/telemt") {
		t.Fatal("the account that was there before was deleted")
	}
	for _, p := range []string{"usr/local/bin/mtproto-agent", "usr/local/bin/telemt", "etc/mtproto-agent", "var/lib/mtproto-agent"} {
		if h.exists(p) {
			t.Errorf("%s survived", p)
		}
	}

	// a half-install's clean-up
	h2 := startFakeHost(t)
	theirs(h2)
	h2.env["FAKE_NO_START"] = "1"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if res, err := Install(ctx, connect(t, h2), request(t), nil); err == nil || !res.Started {
		t.Fatalf("install: %+v %v", res, err)
	}
	if err := Uninstall(context.Background(), connect(t, h2), nil); err != nil || !h2.exists("users/telemt") {
		t.Fatalf("the clean-up deleted the account that was there: %v", err)
	}

	// one the install made goes with it
	h3 := startFakeHost(t)
	if _, err := Install(context.Background(), connect(t, h3), request(t), nil); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(context.Background(), connect(t, h3), nil); err != nil || h3.exists("users/telemt") {
		t.Fatalf("the install's own account stayed: %v", err)
	}
}

// REALITY runs on the cover site the box has, which a lost ack can leave
// different from the panel's record: a repair keeps it, checks it and says which.
func TestRepairAdoptsTheCoverSiteTheBoxRuns(t *testing.T) {
	h := startFakeHost(t)
	if _, err := Install(context.Background(), connect(t, h), request(t), nil); err != nil {
		t.Fatal(err)
	}
	var f state.File
	json.Unmarshal([]byte(h.read("etc/mtproto-agent/state.json")), &f)
	f.Disguise = "moved.example.ir" // the agent took a push the panel never heard back about
	b, _ := json.Marshal(f)
	os.WriteFile(h.path("etc/mtproto-agent/state.json"), b, 0o600)

	req := request(t) // still names Mirror.Example.ir
	req.KeepState = true
	res, err := Install(context.Background(), connect(t, h), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Domain != "moved.example.ir" {
		t.Fatalf("result domain = %q", res.Domain)
	}
	checked := false
	for _, c := range h.commands() {
		if strings.Contains(c, "disguise-check") {
			checked = strings.Contains(c, "'moved.example.ir'")
		}
	}
	if !checked {
		t.Fatal("the repair checked a cover site the box does not run")
	}
	// a fresh state names the request's
	if res, err := Install(context.Background(), connect(t, startFakeHost(t)), request(t), nil); err != nil || res.Domain != "mirror.example.ir" {
		t.Fatalf("fresh: %v %q", err, res.Domain)
	}
}
