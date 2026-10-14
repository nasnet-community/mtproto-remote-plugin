package installer

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
)

// fakeHost is an SSH server that runs each command in a temporary root with
// stand-in tools (systemctl, useradd, sudo, ...). Absolute paths the installer
// uses are rewritten into that root, so the real flow runs end to end.
type fakeHost struct {
	t        *testing.T
	ln       net.Listener
	root     string
	bin      string
	fp       string
	signer   ssh.Signer
	user     string
	password string
	authKey  ssh.PublicKey

	env     map[string]string // FAKE_* knobs for the tools
	rewrite *strings.Replacer

	attempts atomic.Int32 // authentication attempts seen
	mu       sync.Mutex
	cmds     []string

	// forwards: names this box will connect to for us, and where they really go
	sites     map[string]string
	noForward bool     // like AllowTcpForwarding no
	forwarded []string // host:port the client asked the box to open
}

var fakeTools = map[string]string{
	"uname": `[ "$1" = "-m" ] && { echo "${FAKE_ARCH:-x86_64}"; exit 0; }; command -p uname "$@"; exit $?`,
	"id": `case "$*" in
 "-u") echo "${FAKE_UID:-0}";;
 "-u telemt"|"-g telemt") [ -e "$ROOT/users/telemt" ] && echo 998 || exit 1;;
 *) command -p id "$@"; exit $?;;
esac`,
	// like the real one: with NOPASSWD it never reads stdin, even with -S or -k
	"sudo": `nopw=; stdinpw=; k=
while [ $# -gt 0 ]; do case "$1" in -n) nopw=n; shift;; -S) stdinpw=S; shift;; -k) k=k; shift;; -p) shift 2;; *) break;; esac; done
echo "sudo -$nopw$stdinpw$k $*" >> "$ROOT/sudo.log"
if [ -n "$FAKE_SUDO_NOPASSWD" ]; then :
elif [ -n "$nopw" ]; then echo "sudo: a password is required" >&2; exit 1
elif [ -n "$stdinpw" ]; then read -r pw; [ "$pw" = "$FAKE_SUDO_PW" ] || { echo "sudo: incorrect password attempt" >&2; exit 1; }
else echo "sudo: a terminal is required to read the password" >&2; exit 1
fi
FAKE_UID=0 exec "$@"`,
	"systemctl": `echo "$*" >> "$ROOT/systemctl.log"
case "$1" in
 is-active) [ -e "$ROOT/active/$2" ] && echo active || { echo inactive; exit 3; };;
 enable|daemon-reload) ;;
 restart|start) [ -n "$FAKE_NO_START" ] && exit 0; mkdir -p "$ROOT/active"; touch "$ROOT/active/$2"; echo "LISTEN 0 128 0.0.0.0:8443 0.0.0.0:*" > "$ROOT/listening";;
 disable) rm -f "$ROOT/active/$3"; rm -f "$ROOT/listening";;
esac
exit 0`,
	"useradd": `mkdir -p "$ROOT/users"; touch "$ROOT/users/telemt"`,
	"userdel": `rm -f "$ROOT/users/telemt"`,
	"nft":     `echo "$*" >> "$ROOT/nft.log"; exit 0`,
	"ss":      `echo "State Recv-Q Send-Q Local Address:Port Peer Address:Port"; cat "$ROOT/listening" 2>/dev/null; exit 0`,
	"sha256sum": `[ -n "$FAKE_BAD_SHA" ] && { echo "0000000000000000000000000000000000000000000000000000000000000000  $1"; exit 0; }
command -p sha256sum "$@" 2>/dev/null && exit 0
exec shasum -a 256 "$@"`,
	"install": `args=""
while [ $# -gt 0 ]; do case "$1" in -o|-g) shift 2;; *) args="$args '$1'"; shift;; esac; done
eval command -p install $args; exit $?`,
	"apt-get": `echo "$*" >> "$ROOT/apt.log"
case " $* " in *" update "*) [ -n "$FAKE_APT_UPDATE_FAIL" ] && exit 100; exit 0;; esac
[ -n "$FAKE_APT_INSTALL_FAIL" ] && { echo "E: Unable to locate package nftables" >&2; exit 100; }
printf '#!/bin/sh\nexit 0\n' > "$BIN/nft"; chmod +x "$BIN/nft"`,
	"getent": `[ "$1" = ahosts ] || exit 1
case " $FAKE_NO_NAMES " in *" $2 "*) exit 2;; esac
printf '192.0.2.7       STREAM %s\n192.0.2.7       DGRAM\n2001:db8::7     STREAM\n' "$2"`,
}

func startFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "fakebin")
	os.MkdirAll(bin, 0o755)
	for name, body := range fakeTools {
		os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	}
	// the box sees only these: a real nft or systemctl on the test machine must not leak in
	for _, name := range []string{"cat", "chmod", "rm", "mv", "mkdir", "touch", "cp", "ln", "head", "tail", "tr", "sed", "grep", "dirname", "basename", "sleep", "cut", "wc", "sh", "env", "true", "false"} {
		if p, err := exec.LookPath(name); err == nil {
			os.Symlink(p, filepath.Join(bin, name))
		}
	}
	for _, d := range []string{"etc/systemd/system", "usr/local/bin", "var/lib", "tmp"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("PRETTY_NAME=\"Fake Linux 12\"\n"), 0o644)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	h := &fakeHost{t: t, root: root, bin: bin, signer: signer, fp: ssh.FingerprintSHA256(signer.PublicKey()),
		user: "root", password: "hunter2", env: map[string]string{"FAKE_UID": "0", "FAKE_SUDO_PW": "sudopw"}}

	var pairs []string
	for _, prefix := range []string{"/usr/local/", "/etc/", "/var/lib/", "/tmp/"} {
		pairs = append(pairs, prefix, root+prefix)
	}
	h.rewrite = strings.NewReplacer(pairs...)

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			h.attempts.Add(1)
			if c.User() == h.user && string(pw) == h.password {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			h.attempts.Add(1)
			if h.authKey != nil && c.User() == h.user && string(k.Marshal()) == string(h.authKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.ln = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go h.serve(c, cfg)
		}
	}()
	return h
}

func (h *fakeHost) target() Target {
	a := h.ln.Addr().(*net.TCPAddr)
	return Target{Host: "127.0.0.1", Port: a.Port, User: h.user, Password: h.password, HostKey: h.fp}
}

func (h *fakeHost) path(p string) string { return filepath.Join(h.root, p) }
func (h *fakeHost) read(p string) string {
	b, err := os.ReadFile(h.path(p))
	if err != nil {
		return ""
	}
	return string(b)
}
func (h *fakeHost) exists(p string) bool { _, err := os.Stat(h.path(p)); return err == nil }
func (h *fakeHost) commands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.cmds...)
}
func (h *fakeHost) removeTool(name string) { os.Remove(filepath.Join(h.bin, name)) }

func (h *fakeHost) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() == "direct-tcpip" {
			go h.forward(nch)
			continue
		}
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			return
		}
		go func() {
			for r := range creqs {
				if r.Type != "exec" {
					r.Reply(false, nil)
					continue
				}
				var p struct{ Cmd string }
				ssh.Unmarshal(r.Payload, &p)
				r.Reply(true, nil)
				h.run(ch, p.Cmd)
				return
			}
		}()
	}
}

func (h *fakeHost) run(ch ssh.Channel, cmd string) {
	h.mu.Lock()
	h.cmds = append(h.cmds, cmd)
	h.mu.Unlock()
	// one pass: on Linux the root itself lives under /tmp/ and must not be rewritten again
	cmd = h.rewrite.Replace(cmd)
	c := exec.Command("sh", "-c", cmd)
	c.Env = append(os.Environ(), "PATH="+h.bin, "ROOT="+h.root, "BIN="+h.bin)
	for k, v := range h.env {
		c.Env = append(c.Env, k+"="+v)
	}
	c.Stdin, c.Stdout, c.Stderr = ch, ch, ch.Stderr()
	status := uint32(0)
	if err := c.Run(); err != nil {
		status = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			status = uint32(ee.ExitCode())
		}
	}
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	ch.Close()
}

func (h *fakeHost) setForwarding(on bool) {
	h.mu.Lock()
	h.noForward = !on
	h.mu.Unlock()
}

func (h *fakeHost) forwards() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.forwarded...)
}

// site makes the box connect name:443 to addr.
func (h *fakeHost) site(name, addr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sites == nil {
		h.sites = map[string]string{}
	}
	h.sites[name] = addr
}

func (h *fakeHost) forward(nch ssh.NewChannel) {
	var p struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}
	ssh.Unmarshal(nch.ExtraData(), &p)
	h.mu.Lock()
	h.forwarded = append(h.forwarded, net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port))))
	to, known := h.sites[p.Host]
	off := h.noForward
	h.mu.Unlock()
	if off {
		nch.Reject(ssh.Prohibited, "administratively prohibited")
		return
	}
	if !known || p.Port != 443 {
		nch.Reject(ssh.ConnectionFailed, "Connection refused")
		return
	}
	up, err := net.Dial("tcp", to)
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		up.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { io.Copy(up, ch); up.Close() }()
	io.Copy(ch, up)
	ch.Close()
}
