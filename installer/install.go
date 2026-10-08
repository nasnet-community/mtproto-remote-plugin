package installer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/agent/state"
	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/layout"
	"github.com/nasnet-community/mtproto-remote-plugin/reality"
	"github.com/nasnet-community/mtproto-remote-plugin/release"
)

// Where things go on the VPS (shared with the agent).
const (
	AgentBin   = layout.AgentBin
	TelemtBin  = layout.TelemtBin
	ConfDir    = layout.ConfDir
	StatePath  = layout.StatePath
	DataDir    = layout.DataDir
	TelemtDir  = layout.TelemtDir
	TelemtRun  = layout.TelemtRun
	UnitPath   = layout.UnitPath
	UnitName   = layout.UnitName
	TelemtUser = layout.TelemtUser
)

// Bounds on single remote commands: a hung apt or systemctl must not hold a job.
const (
	cmdTimeout       = 2 * time.Minute
	pkgTimeout       = 10 * time.Minute
	aptUpdateTimeout = 5 * time.Minute
	uploadTimeout    = 15 * time.Minute
)

// within runs fn under a deadline of its own.
func within(ctx context.Context, d time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return fn(ctx)
}

// Defaults for the two ports the agent opens besides telemt's own.
const (
	DefaultHubPort     = 8443
	DefaultCapturePort = 9443
	MTProtoPort        = 443
)

// Facts is what we learn about the VPS before touching it.
type Facts struct {
	OS         string
	Arch       release.Arch
	IsRoot     bool
	HasSudo    bool
	HasSystemd bool
	HasNft     bool
	PkgMgr     string // apt-get | dnf | yum | ""
	Listening  map[int]bool
	Installed  bool // an agent from an earlier install is present, credentials and all
	// a telemt account or binary with no agent on the box is someone else's:
	// the account is never deleted, the binary never replaced
	OtherUser   bool
	OtherTelemt bool
}

// Inspect reads the facts Install needs.
func Inspect(ctx context.Context, c *Conn) (Facts, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*cmdTimeout)
	defer cancel()
	var p Facts
	arch, err := c.Run(ctx, "uname -m", nil)
	if err != nil {
		return p, err
	}
	if p.Arch, err = release.ParseArch(strings.TrimSpace(arch)); err != nil {
		return p, err
	}
	p.IsRoot = c.IsRoot()
	has := func(tool string) bool {
		_, err := c.Run(ctx, "command -v "+shq(tool), nil)
		return err == nil
	}
	p.HasSystemd, p.HasNft, p.HasSudo = has("systemctl"), has("nft"), has("sudo")
	switch {
	case has("apt-get"):
		p.PkgMgr = "apt-get"
	case has("dnf"):
		p.PkgMgr = "dnf"
	case has("yum"):
		p.PkgMgr = "yum"
	}
	if out, err := c.Run(ctx, ". /etc/os-release 2>/dev/null && echo \"$PRETTY_NAME\"", nil); err == nil {
		p.OS = strings.TrimSpace(out)
	}
	p.Listening = map[int]bool{}
	if out, err := c.Run(ctx, "ss -ltn 2>/dev/null", nil); err == nil {
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) < 4 {
				continue
			}
			addr := f[3]
			if i := strings.LastIndexByte(addr, ':'); i >= 0 {
				if n, err := strconv.Atoi(addr[i+1:]); err == nil {
					p.Listening[n] = true
				}
			}
		}
	}
	if _, err := c.Run(ctx, "test -e "+AgentBin, nil); err != nil {
		_, err = c.Run(ctx, "id -u "+TelemtUser, nil)
		p.OtherUser = err == nil
		_, err = c.Run(ctx, "test -e "+TelemtBin, nil)
		p.OtherTelemt = err == nil
	}
	if !p.IsRoot && !p.HasSudo {
		return p, errors.New("installer: this login is not root and the VPS has no sudo")
	}
	// an agent with credentials, not the binary a failed install left; the state is root's only
	out, err := c.Root(ctx, "if test -x "+AgentBin+" && test -e "+StatePath+"; then echo installed; fi", nil)
	if err != nil {
		return p, err
	}
	p.Installed = strings.TrimSpace(out) == "installed"
	return p, nil
}

// Creds are the link credentials the hub generates and keeps (the public half).
type Creds struct {
	Priv    []byte
	Pub     []byte
	ShortID [8]byte
	Token   []byte
}

func NewCreds() (Creds, error) {
	priv, pub, err := reality.GenerateKey()
	if err != nil {
		return Creds{}, err
	}
	c := Creds{Priv: priv, Pub: pub, Token: make([]byte, 32)}
	if _, err := rand.Read(c.ShortID[:]); err != nil {
		return Creds{}, err
	}
	if _, err := rand.Read(c.Token); err != nil {
		return Creds{}, err
	}
	return c, nil
}

// Bin is a file to push: the hub opens it from its cache and vouches for its hash.
type Bin struct {
	Open   func() (io.ReadCloser, error)
	Size   int64
	SHA256 string
}

type Request struct {
	Agent, Telemt Bin
	Disguise      string
	HubPort       int // default 8443
	CapturePort   int // default 9443
	AgentID       string
	Creds         Creds
	// KeepState reinstalls over an existing agent without touching its
	// credentials; the result carries what is on the box.
	KeepState bool
	// Repair: the box is a node the panel already has, so an agent there is
	// expected (KeepState implies it). Without it an agent on the box is refused.
	Repair bool
	// AllowUnverifiedDisguise installs even if the disguise check fails.
	AllowUnverifiedDisguise bool
	// CoverChecked: the panel already tested Disguise from this VPS (CheckCover),
	// so the agent doesn't test it again after the upload.
	CoverChecked bool
}

type Step struct {
	Name   string `json:"name"`
	State  string `json:"state"` // running | ok | failed
	Detail string `json:"detail,omitempty"`
}

type Progress func(Step)

type Result struct {
	AgentID     string
	Creds       Creds
	HubPort     int
	CapturePort int
	TelemtUID   uint32
	Disguise    ctrl.DisguiseResult
	Facts       Facts
	Started     bool   // the checks passed and the box has been changed
	Domain      string // the cover site the agent runs: the request's, or a kept state's own
}

type runner struct {
	c        *Conn
	progress Progress
}

func (r *runner) step(name string) func(detail string, err error) error {
	if r.progress != nil {
		r.progress(Step{Name: name, State: "running"})
	}
	return func(detail string, err error) error {
		if r.progress != nil {
			st := Step{Name: name, State: "ok", Detail: detail}
			if err != nil {
				st = Step{Name: name, State: "failed", Detail: err.Error()}
			}
			r.progress(st)
		}
		return err
	}
}

// Install sets the box up; a failure still returns what it found, so the caller knows what it may undo.
func Install(ctx context.Context, c *Conn, req Request, progress Progress) (*Result, error) {
	res := &Result{}
	if err := install(ctx, c, req, progress, res); err != nil {
		if res.Facts.Arch == "" {
			return nil, err
		}
		return res, err
	}
	return res, nil
}

func install(ctx context.Context, c *Conn, req Request, progress Progress, res *Result) error {
	r := &runner{c: c, progress: progress}
	if req.HubPort == 0 {
		req.HubPort = DefaultHubPort
	}
	if req.CapturePort == 0 {
		req.CapturePort = DefaultCapturePort
	}
	if req.AgentID == "" {
		req.AgentID = newID()
	}

	// 1. look before touching anything
	done := r.step("check")
	facts, err := Inspect(ctx, c)
	res.Facts = facts
	if err == nil && !facts.HasSystemd {
		err = errors.New("installer: this VPS does not run systemd")
	}
	repair := req.Repair || req.KeepState
	switch {
	case err != nil:
	case facts.Installed && !repair:
		// a fresh install would replace its credentials and cut its node off
		err = errors.New("installer: this host already runs mtproto-agent: repair its node instead, or remove the agent from the host first")
	case facts.OtherTelemt && !repair:
		err = fmt.Errorf("installer: %s already exists (another telemt install?): remove it first, the install would replace it", TelemtBin)
	default:
		for _, port := range []int{req.HubPort, req.CapturePort, MTProtoPort} {
			if facts.Listening[port] && !facts.Installed {
				err = fmt.Errorf("installer: port %d is already in use on the VPS", port)
				break
			}
		}
	}
	if err := done(string(facts.Arch)+" "+facts.OS, err); err != nil {
		return err
	}

	// 2. verify what we are about to push, before pushing anything
	done = r.step("verify")
	err = firstErr(checkBin("agent", req.Agent), checkBin("telemt", req.Telemt))
	if err := done("", err); err != nil {
		return err
	}
	res.Started = true

	// 3. nftables is the one thing we may need to install from the distro
	if !facts.HasNft {
		done = r.step("nftables")
		err = installNft(ctx, r, facts)
		if err := done("", err); err != nil {
			return err
		}
	}

	// 4. the unprivileged account telemt runs as; the capture rules match its uid
	done = r.step("account")
	uid, gid, err := ensureUser(ctx, c, facts)
	if err := done(fmt.Sprintf("uid %d", uid), err); err != nil {
		return err
	}

	// 5. binaries
	done = r.step("upload")
	err = firstErr(
		push(ctx, c, req.Agent, AgentBin),
		push(ctx, c, req.Telemt, TelemtBin),
	)
	if err := done("", err); err != nil {
		return err
	}

	done = r.step("directories")
	err = within(ctx, cmdTimeout, func(ctx context.Context) error {
		// telemt's uid has to pass through the data dir (an older 0700 one is fixed here)
		_, err := c.Root(ctx, strings.Join([]string{
			"install -d -m 0700 " + ConfDir,
			"install -d -m 0711 " + DataDir, "chmod 0711 " + DataDir,
			"install -d -m 0750 -o root -g " + TelemtUser + " " + TelemtDir,
			"install -d -m 0700 -o " + TelemtUser + " -g " + TelemtUser + " " + TelemtRun,
		}, " && "), nil)
		return err
	})
	if err := done("", err); err != nil {
		return err
	}

	// a kept state brings its own cover site: REALITY runs on that one, whatever the panel remembers
	var kept *state.File
	if req.KeepState {
		kept = readKept(ctx, c)
	}
	domain := strings.ToLower(req.Disguise)
	if kept != nil && kept.Disguise != "" {
		domain = kept.Disguise
	}

	// 6. the borrowed site has to work from here, or REALITY and FakeTLS look wrong
	var dres ctrl.DisguiseResult
	if req.CoverChecked && domain == strings.ToLower(req.Disguise) {
		dres.OK = true
	} else {
		done = r.step("disguise")
		dres, err = checkDisguise(ctx, c, domain)
		if err == nil && !dres.OK && !req.AllowUnverifiedDisguise {
			err = fmt.Errorf("installer: %s does not work as a cover site from this VPS: %s", domain, dres.Err)
		}
		if err := done(domain, err); err != nil {
			return err
		}
	}

	// 7. credentials
	done = r.step("credentials")
	creds, err := writeState(ctx, c, req, kept, uid, gid)
	if err := done("", err); err != nil {
		return err
	}

	// 8. service
	done = r.step("service")
	err = startService(ctx, c, req.HubPort)
	if err := done("", err); err != nil {
		return err
	}

	*res = Result{
		AgentID: req.AgentID, Creds: creds, HubPort: req.HubPort, CapturePort: req.CapturePort,
		TelemtUID: uid, Disguise: dres, Facts: facts, Started: true, Domain: domain,
	}
	return nil
}

func checkBin(name string, b Bin) error {
	if b.Open == nil || b.Size <= 0 || len(b.SHA256) != 64 {
		return fmt.Errorf("installer: no %s binary was provided", name)
	}
	rc, err := b.Open()
	if err != nil {
		return fmt.Errorf("installer: %s: %w", name, err)
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return err
	}
	if n != b.Size || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), b.SHA256) {
		return fmt.Errorf("installer: the %s binary does not match its pinned hash", name)
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func installNft(ctx context.Context, r *runner, p Facts) error {
	var cmd string
	switch p.PkgMgr {
	case "apt-get":
		// a fresh image often has no package lists; a refresh that fails is not fatal by itself
		_ = within(ctx, aptUpdateTimeout, func(ctx context.Context) error {
			_, err := r.c.Root(ctx, "DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=120 update", nil)
			return err
		})
		// wait a while for another apt to let go of the lock, not for ever
		cmd = "DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=300 install -y nftables"
	case "dnf", "yum":
		cmd = p.PkgMgr + " install -y nftables"
	default:
		return errors.New("installer: nftables is missing and this VPS has no supported package manager")
	}
	err := within(ctx, pkgTimeout, func(ctx context.Context) error {
		_, err := r.c.Root(ctx, cmd, nil)
		return err
	})
	if err != nil {
		return fmt.Errorf("installer: could not install nftables with %s (can the VPS reach its package lists?): %w", p.PkgMgr, err)
	}
	return nil
}

// ensureUser creates the telemt account if it does not exist and returns its ids.
// Someone else's account is marked so no removal deletes it; one we create is not.
func ensureUser(ctx context.Context, c *Conn, facts Facts) (uid, gid uint32, err error) {
	mark := ""
	if facts.OtherUser {
		mark = "install -d -m 0700 " + ConfDir + " && : > " + shq(layout.KeepUser) + " && "
	}
	script := mark + `{ id -u ` + TelemtUser + ` >/dev/null 2>&1 || { rm -f ` + shq(layout.KeepUser) + `; useradd --system --no-create-home --shell /usr/sbin/nologin ` + TelemtUser + `; }; } && echo "$(id -u ` + TelemtUser + `) $(id -g ` + TelemtUser + `)"`
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	out, err := c.Root(ctx, script, nil)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("installer: unexpected id output %q", out)
	}
	u, err1 := strconv.ParseUint(f[0], 10, 32)
	g, err2 := strconv.ParseUint(f[1], 10, 32)
	if err1 != nil || err2 != nil || u == 0 {
		return 0, 0, fmt.Errorf("installer: bad ids %q", out)
	}
	return uint32(u), uint32(g), nil
}

// push uploads b to a temp file on the VPS, checks its hash THERE, and only then
// moves it into place.
func push(ctx context.Context, c *Conn, b Bin, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	rc, err := b.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := dest + ".upload"
	if _, err := c.Root(ctx, "cat > "+shq(tmp)+" && chmod 0755 "+shq(tmp), rc); err != nil {
		return err
	}
	out, err := c.Root(ctx, "sha256sum "+shq(tmp), nil)
	if err != nil {
		c.Root(ctx, "rm -f "+shq(tmp), nil)
		return err
	}
	if f := strings.Fields(out); len(f) == 0 || !strings.EqualFold(f[0], b.SHA256) {
		c.Root(ctx, "rm -f "+shq(tmp), nil)
		return fmt.Errorf("installer: %s arrived corrupted (hash mismatch on the VPS)", dest)
	}
	_, err = c.Root(ctx, "mv -f "+shq(tmp)+" "+shq(dest), nil)
	return err
}

// checkDisguise runs the freshly pushed agent's own checker, so the answer
// reflects the VPS's network, not the hub's.
func checkDisguise(ctx context.Context, c *Conn, domain string) (ctrl.DisguiseResult, error) {
	var res ctrl.DisguiseResult
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	out, err := c.Root(ctx, AgentBin+" disguise-check "+shq(domain), nil)
	if err != nil {
		return res, err
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return res, fmt.Errorf("installer: unreadable cover site check: %w", err)
	}
	return res, nil
}

// readKept is the box's state when it has usable credentials, else nil.
func readKept(ctx context.Context, c *Conn) *state.File {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	out, err := c.Root(ctx, "cat "+StatePath, nil)
	if err != nil {
		return nil
	}
	var f state.File
	if json.Unmarshal([]byte(out), &f) != nil || len(f.RealityPriv) != 32 || len(f.Tokens) == 0 || len(f.ShortIDs) == 0 {
		return nil
	}
	return &f
}

// writeState keeps the box's credentials when kept, else writes the request's.
func writeState(ctx context.Context, c *Conn, req Request, kept *state.File, uid, gid uint32) (Creds, error) {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	if kept != nil {
		pub, err := reality.PublicFromPrivate(kept.RealityPriv)
		if err != nil {
			return Creds{}, err
		}
		var sid [8]byte
		copy(sid[:], kept.ShortIDs[0])
		return Creds{Priv: kept.RealityPriv, Pub: pub, ShortID: sid, Token: kept.Tokens[0]}, nil
	}
	cr := req.Creds
	if len(cr.Priv) != 32 || len(cr.Token) < 16 {
		return Creds{}, errors.New("installer: no credentials to install")
	}
	f := state.File{
		AgentID: req.AgentID, RealityPriv: cr.Priv, ShortIDs: [][]byte{cr.ShortID[:]},
		Tokens: [][]byte{cr.Token}, HubPort: req.HubPort, CapturePort: req.CapturePort,
		TelemtUID: uid, TelemtGID: gid, Disguise: strings.ToLower(req.Disguise),
	}
	b, err := json.Marshal(f)
	if err != nil {
		return Creds{}, err
	}
	// 0600 before the bytes land: the key is never readable by anyone else
	_, err = c.Root(ctx, "umask 077 && cat > "+shq(StatePath+".new")+" && mv -f "+shq(StatePath+".new")+" "+shq(StatePath), strings.NewReader(string(b)))
	return cr, err
}

const unit = `[Unit]
Description=MTProto agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=` + AgentBin + ` run
Restart=always
RestartSec=2
LimitNOFILE=65536
LimitCORE=0
Environment=GOMEMLIMIT=80MiB
Environment=GOTRACEBACK=none
StandardOutput=null
StandardError=null
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=full
ReadWritePaths=/usr/local/bin ` + ConfDir + ` ` + DataDir + `

[Install]
WantedBy=multi-user.target
`

func startService(ctx context.Context, c *Conn, hubPort int) error {
	ctx, cancel := context.WithTimeout(ctx, 2*cmdTimeout)
	defer cancel()
	if _, err := c.Root(ctx, "cat > "+shq(UnitPath)+" && chmod 0644 "+shq(UnitPath), strings.NewReader(unit)); err != nil {
		return err
	}
	if _, err := c.Root(ctx, "systemctl daemon-reload && systemctl enable "+UnitName+" && systemctl restart "+UnitName, nil); err != nil {
		return err
	}
	// the hub port listening is what proves the agent came up
	deadline := time.Now().Add(20 * time.Second)
	for {
		out, _ := c.Run(ctx, "systemctl is-active "+UnitName, nil)
		if strings.TrimSpace(out) == "active" {
			if ss, err := c.Run(ctx, "ss -ltn", nil); err == nil && strings.Contains(ss, ":"+strconv.Itoa(hubPort)+" ") {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("installer: the agent did not start listening on port %d", hubPort)
		}
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Uninstall stops the agent and removes everything Install put on the box.
func Uninstall(ctx context.Context, c *Conn, progress Progress) error {
	r := &runner{c: c, progress: progress}
	done := r.step("remove")
	script := strings.Join([]string{
		"keep=; [ -e " + layout.KeepUser + " ] && keep=1", // read before its directory goes
		"systemctl disable --now " + UnitName + " 2>/dev/null || true",
		"nft delete table ip mtproto_agent 2>/dev/null || true",
		"nft delete table ip6 mtproto_agent 2>/dev/null || true",
		"rm -f " + UnitPath + " " + AgentBin + " " + AgentBin + ".prev " + AgentBin + ".prev.tmp " + AgentBin + ".upload " +
			TelemtBin + " " + TelemtBin + ".prev " + TelemtBin + ".prev.tmp " + TelemtBin + ".upload",
		"rm -f " + staging(AgentBin) + " " + staging(TelemtBin), // an update cut short
		"rm -rf " + ConfDir + " " + DataDir,
		"systemctl daemon-reload 2>/dev/null || true",
		"[ -n \"$keep\" ] || userdel " + TelemtUser + " 2>/dev/null || true",
	}, "; ")
	err := within(ctx, 2*cmdTimeout, func(ctx context.Context) error {
		_, err := c.Root(ctx, script, nil)
		return err
	})
	return done("", err)
}

// staging matches the files an agent update writes before its swap.
func staging(bin string) string {
	i := strings.LastIndexByte(bin, '/')
	return bin[:i+1] + "." + bin[i+1:] + ".new*"
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6], b[8] = (b[6]&0x0f)|0x40, (b[8]&0x3f)|0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
