// Package daemon is the agent's brain: it owns the state file, the capture
// rules, telemt's lifecycle and the link, and answers the hub's control calls.
package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/agent/capture"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/cover"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/disguise"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/logring"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/state"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/supervise"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/update"
	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/link"
	"github.com/nasnet-community/mtproto-remote-plugin/mux"
	"github.com/nasnet-community/mtproto-remote-plugin/realityserver"
	"github.com/nasnet-community/mtproto-remote-plugin/telemt"
)

// Proc is telemt's process, as the daemon needs it (supervise.Proc satisfies it).
type Proc interface {
	Run(ctx context.Context)
	Restart()
	SetEnabled(bool)
	Stop() error // SetEnabled(false) that returns once telemt has exited
	Status() supervise.Status
}

// Opener opens a tunnel stream (link.Agent satisfies it).
type Opener interface {
	OpenData(dst string, first []byte) (*mux.Stream, error)
}

type Config struct {
	StatePath string
	WorkDir   string // telemt's config lives in WorkDir/telemt, its working dir in WorkDir/telemt/run
	TelemtBin string
	AgentBin  string
	TrialFile string
	Version   string
	Build     string       // SHA-256 of the running binary ("" when it can't be read)
	Listen    net.Listener // the hub port
	// ListenPort opens another hub port while the link moves (all addresses by default)
	ListenPort func(port int) (net.Listener, error)

	// seams; the zero values are the real thing
	NewProc     func(supervise.Spec) Proc
	ApplyRules  func(ctx context.Context, script string) error
	RemoveRules func(ctx context.Context) error
	RulesLive   func(ctx context.Context, v4, v6 bool) bool
	OrigDst     func(*net.TCPConn) (netip.AddrPort, error)
	Scrape      func(ctx context.Context, port int) (string, error)
	// TelemtVersion asks a telemt binary its version ("" when it cannot say).
	TelemtVersion func(ctx context.Context, bin string) string
	Exit          func() // how the process leaves after an update or uninstall
	Teardown      func(ctx context.Context) error
	Check         disguise.Checker
	Now           func() time.Time
	// RealityDest is where REALITY proxies unauthenticated probes (domain:443).
	RealityDest func(domain string) string
	// DCPort is the datacentre port captured connections must target (443).
	DCPort uint16
	// HealthWindow is how long a new telemt gets to answer after an update.
	HealthWindow time.Duration
	// RuleCheckEvery is how often the capture rules are looked for (default 10 s).
	RuleCheckEvery time.Duration
	// CoverGrace is how long the old cover name stays accepted after a switch (default 15 min).
	CoverGrace time.Duration
	// MaskSock is the socket telemt's cover-site traffic comes in on (default <telemt dir>/mask.sock).
	MaskSock string
	// CoverDial reaches the cover site for telemt (a plain dial by default).
	CoverDial func(ctx context.Context, addr string) (net.Conn, error)
}

type Daemon struct {
	cfg  Config
	st   *state.File // changed under applyMu only
	ring *logring.Ring
	proc Proc
	// cover dials the cover site for telemt, which may reach nothing but Telegram
	cover *cover.Forwarder
	lk    atomic.Pointer[link.Agent]
	ports *ports // the hub ports the link answers on
	op    Opener // defaults to lk; tests substitute

	started time.Time
	boot    string
	trial   *update.Trial

	// applyMu: one change at a time (a push, a rotation, a rule repair, the uninstall)
	applyMu  sync.Mutex
	coreHash string
	dcNets   string // canonical form of the applied prefixes
	script   string // the capture ruleset in force, for the periodic check
	run      bool   // the applied state wants telemt running
	// the cover name before the last switch, still accepted until oldUntil
	oldName  string
	oldUntil time.Time

	// mu guards what status, the handlers and the capture path read
	mu       sync.RWMutex
	desired  *ctrl.State
	disguise string
	allowed  []netip.Prefix
	v4, v6   bool // which families the rules cover
	// metricsPort is where the running telemt's Prometheus endpoint listens:
	// whatever the last rendered config said, not a constant.
	metricsPort int
	telemtVer   string
	held        string // why telemt is kept stopped
	restored    int

	active       atomic.Int64
	rejected     atomic.Uint64
	updating     atomic.Bool
	uninstalling atomic.Bool

	checkMu sync.Mutex
	lastChk ctrl.DisguiseResult
	lastAt  time.Time
}

func New(cfg Config, st *state.File) (*Daemon, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RealityDest == nil {
		cfg.RealityDest = func(domain string) string { return net.JoinHostPort(domain, "443") }
	}
	if cfg.DCPort == 0 {
		cfg.DCPort = 443
	}
	if cfg.HealthWindow == 0 {
		cfg.HealthWindow = update.HealthWindow
	}
	if cfg.NewProc == nil {
		cfg.NewProc = func(s supervise.Spec) Proc { return supervise.New(s) }
	}
	if cfg.ApplyRules == nil {
		cfg.ApplyRules = capture.Apply
	}
	if cfg.RemoveRules == nil {
		cfg.RemoveRules = func(ctx context.Context) error { return capture.Apply(ctx, capture.Teardown()) }
	}
	if cfg.RulesLive == nil {
		cfg.RulesLive = capture.Live
	}
	if cfg.TelemtVersion == nil {
		cfg.TelemtVersion = binaryVersion
	}
	if cfg.RuleCheckEvery == 0 {
		cfg.RuleCheckEvery = 10 * time.Second
	}
	if cfg.CoverGrace == 0 {
		cfg.CoverGrace = 15 * time.Minute
	}
	if cfg.ListenPort == nil {
		cfg.ListenPort = func(port int) (net.Listener, error) { return net.Listen("tcp", ":"+strconv.Itoa(port)) }
	}
	if cfg.OrigDst == nil {
		cfg.OrigDst = capture.OriginalDst
	}
	if cfg.Scrape == nil {
		cfg.Scrape = scrape
	}
	if cfg.Exit == nil {
		cfg.Exit = func() { os.Exit(0) }
	}
	if cfg.Teardown == nil {
		cfg.Teardown = func(context.Context) error { return nil }
	}
	d := &Daemon{cfg: cfg, st: st, ring: logring.New(1000), started: cfg.Now(), boot: newBootID(), cover: &cover.Forwarder{Dial: cfg.CoverDial}}
	if st.Disguise == "" {
		return nil, errors.New("daemon: the state names no cover site")
	}
	d.disguise = st.Disguise
	d.held = "waiting for the panel's first state"
	if cfg.Listen != nil {
		d.ports = newPorts(st.HubPort, cfg.Listen)
	}
	d.proc = cfg.NewProc(supervise.Spec{
		Bin:  cfg.TelemtBin,
		Args: []string{"run", d.telemtConfigPath()},
		Dir:  d.telemtRunDir(),
		UID:  st.TelemtUID, GID: st.TelemtGID,
		Out: d.ring,
	})
	return d, nil
}

func (d *Daemon) telemtDir() string        { return filepath.Join(d.cfg.WorkDir, "telemt") }
func (d *Daemon) telemtRunDir() string     { return filepath.Join(d.telemtDir(), "run") }
func (d *Daemon) telemtConfigPath() string { return filepath.Join(d.telemtDir(), "config.toml") }

func (d *Daemon) maskSock() string {
	if d.cfg.MaskSock != "" {
		return d.cfg.MaskSock
	}
	return filepath.Join(d.telemtDir(), "mask.sock")
}
func (d *Daemon) Logs() *logring.Ring { return d.ring }

func newBootID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (d *Daemon) realityConfig(priv []byte, shortIDs [][]byte, domain string) (realityserver.ServerConfig, error) {
	var sids [][8]byte
	for _, s := range shortIDs {
		var a [8]byte
		if len(s) > 8 {
			return realityserver.ServerConfig{}, errors.New("daemon: short id longer than 8 bytes")
		}
		copy(a[:], s)
		sids = append(sids, a)
	}
	names := []string{domain}
	if d.oldName != "" && d.oldName != domain && d.cfg.Now().Before(d.oldUntil) {
		names = append(names, d.oldName)
	}
	return realityserver.ServerConfig{
		Dest: d.cfg.RealityDest(domain), ServerNames: names,
		PrivateKey: priv, ShortIDs: sids, MaxTimeDiff: 2 * time.Minute,
	}, nil
}

// dropOldName stops accepting the previous cover name once its grace is over.
func (d *Daemon) dropOldName() {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	if d.oldName == "" || d.cfg.Now().Before(d.oldUntil) {
		return // gone already, or a later switch moved the deadline
	}
	d.oldName = ""
	rc, err := d.realityConfig(d.st.RealityPriv, d.st.ShortIDs, d.st.Disguise)
	if lk := d.lk.Load(); lk != nil && err == nil {
		lk.Reconfigure(rc)
	}
}

func (d *Daemon) hold(why string) {
	d.mu.Lock()
	d.held = why
	d.mu.Unlock()
}

func (d *Daemon) logf(format string, a ...any) { fmt.Fprintf(d.ring, format+"\n", a...) }

// Run brings everything up and blocks until ctx ends.
func (d *Daemon) Run(ctx context.Context) error {
	d.applyMu.Lock()
	st := *d.st
	rc, err := d.realityConfig(st.RealityPriv, st.ShortIDs, st.Disguise)
	d.applyMu.Unlock()
	if err != nil {
		return err
	}
	if d.ports == nil {
		if d.cfg.Listen == nil {
			return errors.New("daemon: no hub port to listen on")
		}
		d.ports = newPorts(st.HubPort, d.cfg.Listen)
	}
	defer d.ports.Close()
	// a move the panel never finished keeps both ports, so it can get in on either
	if p := st.NextHubPort; p != 0 && p != st.HubPort {
		if ln, err := d.cfg.ListenPort(p); err != nil {
			d.logf("the new link port %d could not open: %v", p, err)
		} else {
			d.ports.add(p, ln)
		}
	}
	lk := link.NewAgent(link.AgentConfig{
		AgentID: st.AgentID, Tokens: st.Tokens, Listener: d.ports, Reality: rc,
		Handlers: d.Handlers(), MaxStreams: maxStreams(st.Desired),
		Info: func() ctrl.Hello {
			return ctrl.Hello{AgentVersion: d.cfg.Version, TelemtVersion: d.telemtVersion(), Caps: []string{"hub-egress"}}
		},
	})
	d.lk.Store(lk)
	d.op = lk

	// the capture listeners come up before any rule can send traffic at them
	var lns []net.Listener
	for _, host := range []string{"127.0.0.1", "::1"} {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(st.CapturePort)))
		if err != nil {
			if host == "127.0.0.1" {
				return fmt.Errorf("daemon: capture listener: %w", err)
			}
			continue // no IPv6 on this box
		}
		lns = append(lns, ln)
		go d.serveCapture(ctx, ln)
	}
	defer func() {
		for _, ln := range lns {
			ln.Close()
		}
	}()

	// the cover socket is up before telemt can need it; without it the site goes dark, not Telegram
	if err := prepareTelemt(d.cfg.WorkDir, st.TelemtUID, st.TelemtGID); err != nil {
		return err
	}
	if ln, err := cover.Listen(d.maskSock(), int(st.TelemtGID)); err != nil {
		d.logf("the cover site socket could not open: %v", err)
	} else {
		defer ln.Close()
		go d.cover.Serve(ln)
	}

	// the watchdog idles: only a state that applied (rules, then config) starts telemt
	go d.proc.Run(ctx)
	go d.setTelemtVersion(ctx)
	if st.Desired != nil {
		if _, err := d.ApplyState(ctx, *st.Desired, false); err != nil {
			d.hold("the saved state could not be applied: " + err.Error())
			d.logf("replaying the saved state failed: %v", err)
		}
	}
	go d.watchRules(ctx)
	d.watchTrial(ctx)

	errc := make(chan error, 1)
	go func() { errc <- lk.Serve(ctx) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
}

func (d *Daemon) telemtVersion() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.telemtVer
}

// setTelemtVersion asks the binary on disk, at start and after every telemt update.
func (d *Daemon) setTelemtVersion(ctx context.Context) {
	v := d.cfg.TelemtVersion(ctx, d.cfg.TelemtBin)
	d.mu.Lock()
	d.telemtVer = v
	d.mu.Unlock()
}

// binaryVersion runs `telemt --version` and keeps the first dotted number.
func binaryVersion(ctx context.Context, bin string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	for _, f := range strings.Fields(string(out)) {
		f = strings.TrimPrefix(f, "v")
		if f != "" && f[0] >= '0' && f[0] <= '9' && strings.Contains(f, ".") {
			return f
		}
	}
	return ""
}

func maxStreams(s *ctrl.State) int {
	if s == nil {
		return 0
	}
	return s.MaxStreams
}

// --- the state push ---

var (
	hexSecret = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hostRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

func validate(s ctrl.State) error {
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("port %d", s.Port)
	}
	if !hostRe.MatchString(strings.ToLower(s.Disguise)) {
		return fmt.Errorf("cover site %q is not a domain name", s.Disguise)
	}
	if s.MaxStreams < 0 || s.MaxStreams > 20000 {
		return fmt.Errorf("max streams %d", s.MaxStreams)
	}
	if len(s.Users) > 2000 {
		return fmt.Errorf("%d users", len(s.Users))
	}
	seen := map[string]bool{}
	for _, u := range s.Users {
		if u.Name == "" || len(u.Name) > 128 || strings.ContainsAny(u.Name, "\x00\r\n") {
			return fmt.Errorf("user name %q", u.Name)
		}
		if seen[u.Name] {
			return fmt.Errorf("user %q twice", u.Name)
		}
		seen[u.Name] = true
		if !hexSecret.MatchString(u.Secret) {
			return fmt.Errorf("user %q: the secret must be 32 lowercase hex characters", u.Name)
		}
	}
	if len(s.DCNets) == 0 || len(s.DCNets) > 512 {
		return fmt.Errorf("%d datacentre prefixes", len(s.DCNets))
	}
	return nil
}

func parseNets(in []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range in {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("prefix %q", s)
		}
		min := 12
		if p.Addr().Is6() && !p.Addr().Is4In6() {
			min = 29
		}
		if p.Bits() < min {
			return nil, fmt.Errorf("prefix %q is too broad", s)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// ApplyState makes the box match s. persist=false is the boot-time replay.
func (d *Daemon) ApplyState(ctx context.Context, s ctrl.State, persist bool) (ctrl.StateAck, error) {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	if d.uninstalling.Load() {
		return ctrl.StateAck{}, &ctrl.Error{Code: "uninstalling", Msg: "the agent is being removed"}
	}
	if err := validate(s); err != nil {
		return ctrl.StateAck{}, &ctrl.Error{Code: "bad_state", Msg: err.Error()}
	}
	nets, err := parseNets(s.DCNets)
	if err != nil {
		return ctrl.StateAck{}, &ctrl.Error{Code: "bad_state", Msg: err.Error()}
	}
	raw, _ := json.Marshal(s.Telemt)
	if s.Telemt == nil {
		raw = nil
	}
	set, err := telemt.ParseSettings(raw)
	if err != nil {
		return ctrl.StateAck{}, &ctrl.Error{Code: "bad_state", Msg: err.Error()}
	}
	set.TLSDomain = strings.ToLower(s.Disguise)
	in := telemt.RenderInput{Listen: s.Listen, Port: s.Port, Settings: set, MaskSock: d.maskSock()}
	for _, u := range s.Users {
		in.Users = append(in.Users, telemt.User{
			Name: u.Name, Secret: u.Secret, MaxIPs: u.MaxIPs, Enabled: u.Enabled,
			ExpiresAt: u.ExpiresAt, RateUpBps: u.RateUpBps, RateDownBps: u.RateDownBps,
		})
	}
	out, err := telemt.Render(in)
	if err != nil {
		return ctrl.StateAck{}, &ctrl.Error{Code: "bad_state", Msg: err.Error()}
	}
	d.cover.Set(cover.Target{Primary: set.TLSDomain, Domains: set.TLSDomains(), Host: strings.ToLower(strings.TrimSpace(set.MaskHost))})

	// 1. the redirect first: telemt must never run without it (a flushed one is put back too)
	canon := canonical(nets)
	v4, v6 := capture.Split(nets)
	gone := d.script != "" && !d.cfg.RulesLive(ctx, d.v4, d.v6)
	if canon != d.dcNets || gone {
		script, err := capture.Rules{UID: d.st.TelemtUID, Port: d.st.CapturePort, V4: v4, V6: v6}.Script()
		if err != nil {
			return ctrl.StateAck{}, &ctrl.Error{Code: "bad_state", Msg: err.Error()}
		}
		if gone {
			// fail closed, as the periodic check does: telemt stops before anything else
			d.logf("the capture rules are gone: telemt is stopped until they are back")
			if err := d.proc.Stop(); err != nil {
				d.logf("stopping telemt: %v", err)
			}
		}
		if err := d.cfg.ApplyRules(ctx, script); err != nil {
			if gone {
				d.hold("the capture rules are missing and could not be put back: " + err.Error())
			}
			return ctrl.StateAck{}, &ctrl.Error{Code: "rules", Msg: err.Error()}
		}
		d.dcNets, d.script = canon, script
		d.mu.Lock()
		d.allowed, d.v4, d.v6 = nets, len(v4) > 0, len(v6) > 0
		d.mu.Unlock()
	}

	// 2. telemt's directories and config; the access section reloads live, the core section needs a restart
	if err := prepareTelemt(d.cfg.WorkDir, d.st.TelemtUID, d.st.TelemtGID); err != nil {
		return ctrl.StateAck{}, err
	}
	if err := writeConfig(d.telemtConfigPath(), out.Config(), d.st.TelemtGID); err != nil {
		return ctrl.StateAck{}, err
	}
	core := hash(out.Core)
	ack := ctrl.StateAck{Rev: s.Rev}
	switch {
	case len(s.Users) == 0 || s.Stopped:
		d.proc.SetEnabled(false) // telemt refuses to start with no users
		d.coreHash, d.run = "", false
	default:
		d.proc.SetEnabled(true)
		if d.coreHash != "" && core != d.coreHash {
			d.proc.Restart()
			ack.Restarted = true
		}
		d.coreHash, d.run = core, true
	}

	// 3. a new disguise domain moves REALITY too; established links carry on
	newDomain := strings.ToLower(s.Disguise)
	lk := d.lk.Load()
	if newDomain != d.st.Disguise {
		// the hub moves its SNI only once it has our ack: the old name stays good a while
		d.oldName, d.oldUntil = d.st.Disguise, d.cfg.Now().Add(d.cfg.CoverGrace)
		time.AfterFunc(d.cfg.CoverGrace, d.dropOldName)
		rc, err := d.realityConfig(d.st.RealityPriv, d.st.ShortIDs, newDomain)
		if err != nil {
			return ack, err
		}
		d.st.Disguise = newDomain
		if lk != nil {
			lk.Reconfigure(rc)
		}
		d.checkMu.Lock()
		d.lastChk, d.lastAt = ctrl.DisguiseResult{}, time.Time{} // that check was of the old site
		d.checkMu.Unlock()
	}
	if lk != nil {
		lk.SetMaxStreams(s.MaxStreams)
	}

	if persist {
		d.st.Desired = &s
		if err := state.Save(d.cfg.StatePath, d.st); err != nil {
			return ack, err
		}
	}
	held := ""
	if s.Stopped {
		held = ctrl.HeldStopped
	}
	d.mu.Lock()
	d.desired, d.disguise, d.metricsPort, d.held = &s, newDomain, in.MetricsPort(), held
	d.mu.Unlock()
	return ack, nil
}

// watchRules puts the rules back if they vanish (`systemctl restart nftables` flushes them).
func (d *Daemon) watchRules(ctx context.Context) {
	t := time.NewTicker(d.cfg.RuleCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.checkRules(ctx)
		}
	}
}

func (d *Daemon) checkRules(ctx context.Context) {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	if d.script == "" || d.uninstalling.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d.mu.RLock()
	v4, v6 := d.v4, d.v6
	d.mu.RUnlock()
	if d.cfg.RulesLive(ctx, v4, v6) {
		return
	}
	// fail closed: telemt stops before anything else and stays stopped until they are back
	d.logf("the capture rules are gone: telemt is stopped until they are back")
	if err := d.proc.Stop(); err != nil {
		d.logf("stopping telemt: %v", err)
	}
	if err := d.cfg.ApplyRules(ctx, d.script); err != nil {
		d.hold("the capture rules are missing and could not be put back: " + err.Error())
		d.logf("putting the capture rules back failed: %v", err)
		return
	}
	d.mu.Lock()
	d.held, d.restored = "", d.restored+1
	if d.desired != nil && d.desired.Stopped {
		d.held = ctrl.HeldStopped
	}
	d.mu.Unlock()
	d.logf("the capture rules are back")
	if d.run {
		d.proc.SetEnabled(true)
	}
}

// prepareTelemt redoes telemt's dirs on every push, which heals older installs too.
func prepareTelemt(work string, uid, gid uint32) error {
	for _, d := range []struct {
		path     string
		mode     os.FileMode
		uid, gid int
	}{
		{work, 0o711, -1, -1},
		{filepath.Join(work, "telemt"), 0o750, 0, int(gid)},
		{filepath.Join(work, "telemt", "run"), 0o700, int(uid), int(gid)},
	} {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return err
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return err
		}
		if err := chown(d.path, d.uid, d.gid); err != nil {
			return err
		}
	}
	return nil
}

// writeConfig writes in place: telemt's file watcher follows the file, not a rename.
func writeConfig(path, text string, gid uint32) error {
	if err := os.WriteFile(path, []byte(text), 0o640); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o640); err != nil {
		return err
	}
	return chown(path, 0, int(gid))
}

// chown only matters, and only works, as root.
func chown(path string, uid, gid int) error {
	if uid < 0 || os.Geteuid() != 0 {
		return nil
	}
	return os.Chown(path, uid, gid)
}

func canonical(ps []netip.Prefix) string {
	ss := make([]string, len(ps))
	for i, p := range ps {
		ss[i] = p.String()
	}
	// order-insensitive
	for i := range ss {
		for j := i + 1; j < len(ss); j++ {
			if ss[j] < ss[i] {
				ss[i], ss[j] = ss[j], ss[i]
			}
		}
	}
	return strings.Join(ss, ",")
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// --- control handlers ---

func decode[T any](b json.RawMessage) (T, error) {
	var v T
	if len(b) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, &ctrl.Error{Code: "bad_request", Msg: err.Error()}
	}
	return v, nil
}

func (d *Daemon) Handlers() map[string]ctrl.Handler {
	return map[string]ctrl.Handler{
		ctrl.MStateApply: func(ctx context.Context, b json.RawMessage, _ io.Reader) (any, error) {
			s, err := decode[ctrl.State](b)
			if err != nil {
				return nil, err
			}
			return d.ApplyState(ctx, s, true)
		},
		ctrl.MStateGet: func(context.Context, json.RawMessage, io.Reader) (any, error) {
			d.mu.RLock()
			s := d.desired
			d.mu.RUnlock()
			if s == nil {
				return ctrl.State{}, nil
			}
			return s, nil
		},
		ctrl.MStatus: func(ctx context.Context, _ json.RawMessage, _ io.Reader) (any, error) { return d.Status(ctx), nil },
		ctrl.MStats:  func(ctx context.Context, _ json.RawMessage, _ io.Reader) (any, error) { return d.Stats(ctx) },
		ctrl.MLogsTail: func(_ context.Context, b json.RawMessage, _ io.Reader) (any, error) {
			r, err := decode[ctrl.LogsRequest](b)
			if err != nil {
				return nil, err
			}
			if r.Lines <= 0 || r.Lines > 1000 {
				r.Lines = 200
			}
			return ctrl.LogsResponse{Lines: d.ring.Tail(r.Lines)}, nil
		},
		ctrl.MServiceRestart: func(context.Context, json.RawMessage, io.Reader) (any, error) {
			d.proc.Restart()
			return nil, nil
		},
		ctrl.MDisguiseCheck: func(ctx context.Context, b json.RawMessage, _ io.Reader) (any, error) {
			r, err := decode[ctrl.DisguiseCheck](b)
			if err != nil {
				return nil, err
			}
			res := d.cfg.Check.Check(ctx, r.Domain)
			d.mu.RLock()
			current := d.disguise
			d.mu.RUnlock()
			if strings.EqualFold(r.Domain, current) {
				d.checkMu.Lock()
				d.lastChk, d.lastAt = res, d.cfg.Now()
				d.checkMu.Unlock()
			}
			return res, nil
		},
		ctrl.MKeysRotate: func(_ context.Context, b json.RawMessage, _ io.Reader) (any, error) {
			r, err := decode[ctrl.KeysRotate](b)
			if err != nil {
				return nil, err
			}
			return nil, d.rotate(r)
		},
		ctrl.MLinkPort: func(_ context.Context, b json.RawMessage, _ io.Reader) (any, error) {
			r, err := decode[ctrl.LinkPort](b)
			if err != nil {
				return nil, err
			}
			return nil, d.linkPort(r)
		},
		ctrl.MUpdate: func(ctx context.Context, b json.RawMessage, raw io.Reader) (any, error) {
			r, err := decode[ctrl.UpdateRequest](b)
			if err != nil {
				return nil, err
			}
			return d.update(ctx, r, raw)
		},
		ctrl.MUpdateCommit: func(context.Context, json.RawMessage, io.Reader) (any, error) {
			return nil, update.Commit(update.Paths{AgentBin: d.cfg.AgentBin, TelemtBin: d.cfg.TelemtBin, TrialFile: d.cfg.TrialFile})
		},
		ctrl.MUninstall: func(context.Context, json.RawMessage, io.Reader) (any, error) {
			if d.uninstalling.CompareAndSwap(false, true) {
				go d.uninstall()
			}
			return nil, nil
		},
	}
}

// uninstall: telemt gone first, then its rules, then its account (userdel refuses a busy user).
func (d *Daemon) uninstall() {
	time.Sleep(500 * time.Millisecond) // let the reply out first
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	if err := d.proc.Stop(); err != nil {
		d.logf("uninstall: %v", err)
	}
	_ = d.cfg.RemoveRules(context.Background())
	_ = d.cfg.Teardown(context.Background())
	d.cfg.Exit()
}

// rotate saves only what checks out, and a repeat (the hub lost the reply) changes nothing.
func (d *Daemon) rotate(r ctrl.KeysRotate) error {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	if d.uninstalling.Load() {
		return &ctrl.Error{Code: "uninstalling", Msg: "the agent is being removed"}
	}
	lk := d.lk.Load()
	next := *d.st
	if r.Finish {
		if len(next.Tokens) > 1 {
			next.Tokens = next.Tokens[:1]
		}
		if err := state.Save(d.cfg.StatePath, &next); err != nil {
			return err
		}
		*d.st = next
		if lk != nil {
			lk.SetTokens(next.Tokens)
		}
		return nil
	}
	if len(r.Token) < 16 || len(r.PrivateKey) != 32 || len(r.ShortIDs) == 0 {
		return &ctrl.Error{Code: "bad_request", Msg: "rotate needs a token, a 32-byte key and short ids"}
	}
	rc, err := d.realityConfig(r.PrivateKey, r.ShortIDs, d.st.Disguise)
	if err != nil {
		return &ctrl.Error{Code: "bad_request", Msg: err.Error()}
	}
	toks := [][]byte{r.Token}
	for _, t := range d.st.Tokens {
		if len(toks) < 2 && !bytes.Equal(t, r.Token) {
			toks = append(toks, t)
		}
	}
	next.Tokens, next.RealityPriv, next.ShortIDs = toks, r.PrivateKey, r.ShortIDs
	if err := state.Save(d.cfg.StatePath, &next); err != nil {
		return err
	}
	*d.st = next
	if lk != nil {
		lk.SetTokens(toks)
		lk.Reconfigure(rc)
	}
	return nil
}

// linkPort moves the link a step at a time; each step is safe to repeat.
func (d *Daemon) linkPort(r ctrl.LinkPort) error {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	if d.uninstalling.Load() {
		return &ctrl.Error{Code: "uninstalling", Msg: "the agent is being removed"}
	}
	if d.ports == nil {
		return &ctrl.Error{Code: "unavailable", Msg: "the agent is not serving the link"}
	}
	return d.linkPortLocked(r, *d.st)
}

func (d *Daemon) linkPortLocked(r ctrl.LinkPort, next state.File) error {
	switch {
	case r.Finish, r.Abort:
		moving := next.NextHubPort
		if moving == 0 {
			return nil // done already
		}
		closing := moving
		if r.Finish {
			closing, next.HubPort = next.HubPort, moving
		}
		next.NextHubPort = 0
		if err := state.Save(d.cfg.StatePath, &next); err != nil {
			return err
		}
		*d.st = next
		d.ports.drop(closing)
		if r.Finish {
			d.logf("the link moved to port %d", next.HubPort)
		}
		return nil
	}
	port := r.Port
	if port < 1 || port > 65535 {
		return &ctrl.Error{Code: "bad_request", Msg: "the link port must be between 1 and 65535"}
	}
	if port == next.HubPort {
		// a repeat after the move finished, or a move back before it did
		return d.linkPortLocked(ctrl.LinkPort{Abort: true}, next)
	}
	d.mu.RLock()
	proxy, metrics := 0, d.metricsPort
	if d.desired != nil {
		proxy = d.desired.Port
	}
	d.mu.RUnlock()
	switch port {
	case proxy:
		return &ctrl.Error{Code: "port_taken", Msg: fmt.Sprintf("port %d is the proxy's own port", port)}
	case next.CapturePort, metrics:
		return &ctrl.Error{Code: "port_taken", Msg: fmt.Sprintf("the agent uses port %d itself", port)}
	}
	if port == next.NextHubPort && d.ports.has(port) {
		return nil // a repeat
	}
	ln, err := d.cfg.ListenPort(port)
	if err != nil {
		return &ctrl.Error{Code: "port_busy", Msg: fmt.Sprintf("port %d can't be opened on the node: %v", port, err)}
	}
	// an earlier move the panel never finished gives way
	if p := next.NextHubPort; p != 0 && p != next.HubPort {
		d.ports.drop(p)
	}
	next.NextHubPort = port
	if err := state.Save(d.cfg.StatePath, &next); err != nil {
		ln.Close()
		return err
	}
	*d.st = next
	d.ports.add(port, ln)
	d.logf("the link also listens on port %d while it moves", port)
	return nil
}

func (d *Daemon) update(ctx context.Context, r ctrl.UpdateRequest, raw io.Reader) (any, error) {
	paths := update.Paths{AgentBin: d.cfg.AgentBin, TelemtBin: d.cfg.TelemtBin, TrialFile: d.cfg.TrialFile}
	dest := d.cfg.AgentBin
	smoke := []string{"version"}
	if r.Kind == "telemt" {
		dest, smoke = d.cfg.TelemtBin, []string{"--version"}
	} else if r.Kind != "agent" {
		return nil, &ctrl.Error{Code: "bad_request", Msg: "kind must be agent or telemt"}
	}
	if !d.updating.CompareAndSwap(false, true) {
		return nil, refuseUpdate(raw, "another update is in progress")
	}
	defer d.updating.Store(false)
	if _, err := os.Stat(d.cfg.TrialFile); err == nil {
		return nil, refuseUpdate(raw, "the last agent update is still on trial: commit it first")
	}
	staged, err := update.Receive(raw, r, dest)
	if err != nil {
		return nil, &ctrl.Error{Code: "update_rejected", Msg: err.Error()}
	}
	if err := update.Smoke(ctx, staged, smoke...); err != nil {
		os.Remove(staged)
		return nil, &ctrl.Error{Code: "update_rejected", Msg: err.Error()}
	}
	if r.Kind == "telemt" {
		d.applyMu.Lock()
		running := d.run || d.proc.Status().Running
		d.applyMu.Unlock()
		restart, healthy := func() error { d.proc.Restart(); return nil }, d.telemtAnswers(r.Ver)
		if !running {
			// stopped (no users): swap it in; it is proven when it next starts
			restart, healthy = func() error { return nil }, func(context.Context) error { return nil }
		}
		err := update.UpdateTelemt(ctx, paths, staged, restart, healthy, d.cfg.HealthWindow)
		d.setTelemtVersion(ctx)
		if err != nil {
			return nil, &ctrl.Error{Code: "update_failed", Msg: err.Error()}
		}
		return map[string]any{"restarted": running}, nil
	}
	if err := update.StartAgentTrial(paths, staged, r.Ver); err != nil {
		os.Remove(staged)
		return nil, &ctrl.Error{Code: "update_failed", Msg: err.Error()}
	}
	go func() {
		time.Sleep(500 * time.Millisecond) // the reply goes out, then the service manager restarts us
		d.cfg.Exit()
	}()
	return map[string]any{"restarting": true}, nil
}

// refuseUpdate reads the body away so the hub gets the refusal, not a broken stream.
func refuseUpdate(raw io.Reader, why string) error {
	if raw != nil {
		io.Copy(io.Discard, io.LimitReader(raw, update.MaxSize+1))
	}
	return &ctrl.Error{Code: "update_busy", Msg: why}
}

// telemtAnswers is the update's health check. telemt opens its ports only after
// its start-up fetches and datacentre checks, which can take a minute, so "the
// process is alive" proves nothing; the metrics port answering does, and the
// build_info series names the binary that is answering.
func (d *Daemon) telemtAnswers(want string) func(context.Context) error {
	want = strings.TrimPrefix(want, "v")
	return func(ctx context.Context) error {
		if !d.proc.Status().Running {
			return errors.New("not running")
		}
		d.mu.RLock()
		port := d.metricsPort
		d.mu.RUnlock()
		if port == 0 {
			port = telemt.DefaultMetricsPort
		}
		text, err := d.cfg.Scrape(ctx, port)
		if err != nil {
			return err
		}
		if want != "" && !strings.Contains(text, `telemt_build_info{version="`+want+`"}`) {
			return errors.New("the previous binary is still answering")
		}
		return nil
	}
}

// watchTrial enforces the trial window on a running trial agent.
func (d *Daemon) watchTrial(ctx context.Context) {
	if d.trial == nil {
		return
	}
	paths := update.Paths{AgentBin: d.cfg.AgentBin, TelemtBin: d.cfg.TelemtBin, TrialFile: d.cfg.TrialFile}
	wait := d.trial.Expiry().Sub(d.cfg.Now())
	go func() {
		select {
		case <-time.After(wait):
			if _, err := os.Stat(d.cfg.TrialFile); err == nil { // still uncommitted
				_ = update.RollBack(paths)
				d.cfg.Exit()
			}
		case <-ctx.Done():
		}
	}()
}

// SetTrial is called by main after update.Boot reports an open trial.
func (d *Daemon) SetTrial(t *update.Trial) { d.trial = t }

// --- status, stats ---

func (d *Daemon) Status(ctx context.Context) ctrl.Status {
	var s ctrl.Status
	d.mu.RLock()
	s.AgentVersion, s.AgentBuild, s.TelemtVersion = d.cfg.Version, d.cfg.Build, d.telemtVer
	if d.desired != nil {
		s.StateRev = d.desired.Rev
	}
	s.Disguise.Domain = d.disguise
	s.Telemt.Held, s.Capture.Restored = d.held, d.restored
	v4, v6 := d.v4, d.v6
	d.mu.RUnlock()
	s.BootID = d.boot
	s.UptimeSec = int64(d.cfg.Now().Sub(d.started).Seconds())
	s.RSSBytes = rss()
	s.Goroutines = runtime.NumGoroutine()
	lk := d.lk.Load()
	s.Warm = lk != nil && lk.Warm()
	ps := d.proc.Status()
	s.Telemt.Running, s.Telemt.Restarts, s.Telemt.LastExit, s.Telemt.CrashLoop = ps.Running, ps.Restarts, ps.LastExit, ps.CrashLoop
	if !ps.Since.IsZero() && ps.Running {
		s.Telemt.UptimeSec = int64(d.cfg.Now().Sub(ps.Since).Seconds())
	}
	s.Capture.Rules = (v4 || v6) && d.cfg.RulesLive(ctx, v4, v6)
	s.Capture.Active, s.Capture.Rejected = int(d.active.Load()), d.rejected.Load()
	d.checkMu.Lock()
	s.Disguise.OK, s.Disguise.CheckedAt, s.Disguise.Err = d.lastChk.OK, d.lastAt, d.lastChk.Err
	d.checkMu.Unlock()
	return s
}

func (d *Daemon) Stats(ctx context.Context) (ctrl.Stats, error) {
	st := ctrl.Stats{At: d.cfg.Now()}
	if !d.proc.Status().Running {
		return st, nil
	}
	d.mu.RLock()
	port := d.metricsPort
	d.mu.RUnlock()
	if port == 0 {
		port = telemt.DefaultMetricsPort
	}
	text, err := d.cfg.Scrape(ctx, port)
	if err != nil {
		return st, &ctrl.Error{Code: "metrics", Msg: err.Error()}
	}
	up, down := telemt.ParseMetric(text, telemt.MetricUp), telemt.ParseMetric(text, telemt.MetricDown)
	names := map[string]bool{}
	for n := range up {
		names[n] = true
	}
	for n := range down {
		names[n] = true
	}
	for n := range names {
		st.Users = append(st.Users, ctrl.UserStat{Name: n, OctetsUp: up[n], OctetsDown: down[n]})
	}
	return st, nil
}

func scrape(ctx context.Context, port int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/metrics", port), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metrics status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return string(b), err
}

// --- the capture path ---

// serveCapture takes each connection the nft rules redirected from telemt and
// sends it into the tunnel, opening with whatever telemt sent first.
func (d *Daemon) serveCapture(ctx context.Context, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go d.capture(c)
	}
}

func (d *Daemon) capture(c net.Conn) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		c.Close()
		return
	}
	dst, err := d.cfg.OrigDst(tc)
	if err != nil || !d.permitted(dst) {
		// not a datacentre we were told about: refuse, never forward
		d.rejected.Add(1)
		abortConn(c)
		return
	}
	// telemt speaks first on a datacentre connection, so its opening bytes are
	// usually already here; do not wait long for them
	var first []byte
	buf := make([]byte, 4096)
	c.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	if n, err := c.Read(buf); n > 0 {
		first = buf[:n]
	} else if err != nil && !isTimeout(err) {
		c.Close()
		return
	}
	c.SetReadDeadline(time.Time{})
	st, err := d.op.OpenData(dst.String(), first)
	if err != nil {
		abortConn(c) // tunnel down or full: telemt sees a reset and fails fast
		return
	}
	d.active.Add(1)
	defer d.active.Add(-1)
	link.Pipe(c, st)
}

func (d *Daemon) permitted(ap netip.AddrPort) bool {
	if ap.Port() != d.cfg.DCPort {
		return false
	}
	d.mu.RLock()
	nets := d.allowed
	d.mu.RUnlock()
	a := ap.Addr().Unmap()
	for _, p := range nets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func abortConn(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetLinger(0)
	}
	c.Close()
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
