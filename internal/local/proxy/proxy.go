// Package proxy runs telemt inside Local MTProto's container and keeps it on
// what the store says: its config, its process, its users' usage and its cover site.
package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/agent/cover"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/logring"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/supervise"
	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/local/store"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/users"
	"github.com/nasnet-community/mtproto-remote-plugin/telemt"
)

// Proc is telemt's process (supervise.Proc satisfies it).
type Proc interface {
	Run(ctx context.Context)
	Restart()
	SetEnabled(bool)
	Stop() error
	Status() supervise.Status
}

type Config struct {
	Store   *store.Store
	History *users.History
	Bin     string // telemt
	Dir     string // telemt's config, cover socket and working dir
	Port    int    // telemt listens here; the router forwards its own port to it
	// MaxConns is the connection limit when the owner has set none
	MaxConns int
	UID, GID uint32 // telemt's user; 0 runs it as the plugin's
	Log      *slog.Logger
	Now      func() time.Time

	StatsEvery time.Duration // default 15 s
	FlushEvery time.Duration // usage writes to flash, default 5 min
	CoverEvery time.Duration // the cover site re-check, default 6 h

	// seams; the zero values are the real thing
	NewProc func(supervise.Spec) Proc
	Scrape  func(ctx context.Context, port int) (string, error)
	Check   func(ctx context.Context, domain string) ctrl.DisguiseResult
	// CoverDial reaches the cover site for telemt's probes and certificate copy
	CoverDial func(ctx context.Context, addr string) (net.Conn, error)
	Version   func(ctx context.Context, bin string) string
}

// Event is one line of the proxy's history for the page.
type Event struct {
	At     time.Time
	Kind   string
	Detail string
}

type Runner struct {
	cfg   Config
	ring  *logring.Ring
	proc  Proc
	cover *cover.Forwarder
	book  *users.Book

	applyMu  sync.Mutex
	coreHash string

	mu        sync.Mutex
	held      string // why telemt is kept off; "" when it should run
	blocked   string // set from outside: the router can't take it
	applyErr  string
	version   string
	metrics   int
	last      map[string]users.Usage // telemt's counters at the last poll
	running   *bool                  // at the last look, for events
	crashSeen time.Time
	events    []Event               // oldest first
	checks    map[string]coverCheck // the last few cover site tests, by domain
}

// HeldNoUsers is why telemt waits while the proxy has no users: it won't start without one.
const HeldNoUsers = "it starts when there is a user"

func New(cfg Config) *Runner {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.StatsEvery == 0 {
		cfg.StatsEvery = 15 * time.Second
	}
	if cfg.FlushEvery == 0 {
		cfg.FlushEvery = 5 * time.Minute
	}
	if cfg.CoverEvery == 0 {
		cfg.CoverEvery = 6 * time.Hour
	}
	if cfg.NewProc == nil {
		cfg.NewProc = func(s supervise.Spec) Proc { return supervise.New(s) }
	}
	if cfg.Scrape == nil {
		cfg.Scrape = scrape
	}
	if cfg.Version == nil {
		cfg.Version = binaryVersion
	}
	r := &Runner{cfg: cfg, ring: logring.New(1000), cover: &cover.Forwarder{Dial: cfg.CoverDial}, book: users.NewBook(nil), checks: map[string]coverCheck{}}
	r.last = cfg.Store.Get().Baseline
	r.proc = cfg.NewProc(supervise.Spec{
		Bin: cfg.Bin, Args: []string{"run", r.configPath()}, Dir: r.runDir(),
		UID: cfg.UID, GID: cfg.GID, Out: r.ring,
	})
	return r
}

func (r *Runner) configPath() string { return filepath.Join(r.cfg.Dir, "config.toml") }
func (r *Runner) runDir() string     { return filepath.Join(r.cfg.Dir, "run") }
func (r *Runner) maskSock() string   { return filepath.Join(r.cfg.Dir, "mask.sock") }

func (r *Runner) logf(format string, a ...any) { fmt.Fprintf(r.ring, "plugin: "+format+"\n", a...) }

// Start brings telemt up and keeps it up until ctx ends; Stop then writes the usage.
func (r *Runner) Start(ctx context.Context) error {
	if err := r.prepare(); err != nil {
		return err
	}
	// the cover socket is up before telemt can need it; without it the site goes dark, not Telegram
	ln, err := cover.Listen(r.maskSock(), int(r.cfg.GID))
	if err != nil {
		return fmt.Errorf("proxy: the cover site socket: %w", err)
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go r.cover.Serve(ln)
	go r.proc.Run(ctx)
	go func() {
		v := r.cfg.Version(ctx, r.cfg.Bin)
		r.mu.Lock()
		r.version = v
		r.mu.Unlock()
	}()
	if err := r.Apply(); err != nil {
		r.cfg.Log.Warn("the proxy's settings could not be applied", "err", err)
	}
	go r.loop(ctx)
	return nil
}

// Stop writes what memory holds and stops telemt.
func (r *Runner) Stop() {
	if err := r.Flush(); err != nil {
		r.cfg.Log.Warn("could not store usage before stopping; it is counted again from the last baseline", "err", err)
	}
	if err := r.proc.Stop(); err != nil {
		r.cfg.Log.Warn("stopping telemt", "err", err)
	}
}

// prepare makes telemt's directories: the config is readable by its group only. A
// parent made here stays open to all, or telemt's user couldn't reach its own folder.
func (r *Runner) prepare() error {
	if err := os.MkdirAll(filepath.Dir(r.cfg.Dir), 0o755); err != nil {
		return err
	}
	for _, d := range []struct {
		path     string
		mode     os.FileMode
		uid, gid int
	}{
		{r.cfg.Dir, 0o750, 0, int(r.cfg.GID)},
		{r.runDir(), 0o700, int(r.cfg.UID), int(r.cfg.GID)},
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

func chown(path string, uid, gid int) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return os.Chown(path, uid, gid)
}

// --- settings ---

// Changed applies what the store holds now, after the page changed it.
func (r *Runner) Changed() {
	if err := r.Apply(); err != nil {
		r.cfg.Log.Warn("the proxy's settings could not be applied", "err", err)
	}
}

// SetBlocked keeps telemt off for a reason outside the store ("" lets it run).
func (r *Runner) SetBlocked(why string) {
	r.mu.Lock()
	same := r.blocked == why
	r.blocked = why
	r.mu.Unlock()
	if !same {
		r.Changed()
	}
}

// MaxConns is the connection limit in force.
func (r *Runner) MaxConns(p store.Proxy) int {
	if p.MaxConns > 0 {
		return p.MaxConns
	}
	return r.cfg.MaxConns
}

func (r *Runner) settings(p store.Proxy) (telemt.Settings, error) {
	raw, err := json.Marshal(users.TelemtSettings(p.Options))
	if err != nil {
		return telemt.Settings{}, err
	}
	set, err := telemt.ParseSettings(raw)
	if err != nil {
		return set, err
	}
	set.TLSDomain = strings.ToLower(p.Cover)
	set.MaxConnections = r.MaxConns(p)
	return set, nil
}

// Apply makes telemt match the store: its config, and whether it runs.
func (r *Runner) Apply() error {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	p := r.cfg.Store.Get()
	set, err := r.settings(p)
	if err != nil {
		return r.failed(err)
	}
	in := telemt.RenderInput{Port: r.cfg.Port, Settings: set, MaskSock: r.maskSock()}
	us := slices.Clone(p.Users)
	slices.SortFunc(us, func(a, b users.User) int { return strings.Compare(a.Name, b.Name) })
	for _, u := range us {
		in.Users = append(in.Users, telemt.User{
			Name: u.Name, Secret: u.Secret, MaxIPs: u.MaxIPs, Enabled: u.Enabled && !u.OverQuota(), ExpiresAt: u.ExpiresAt,
		})
	}
	r.mu.Lock()
	blocked := r.blocked
	r.mu.Unlock()
	held := ""
	switch {
	case blocked != "":
		held = blocked
	case p.Cover == "":
		held = "waiting for a cover site"
	case p.Stopped:
		held = ctrl.HeldStopped
	case len(p.Users) == 0:
		held = HeldNoUsers
	}
	core := ""
	if p.Cover != "" {
		out, err := telemt.Render(in)
		if err != nil {
			return r.failed(err)
		}
		r.cover.Set(cover.Target{Primary: set.TLSDomain, Domains: set.TLSDomains()})
		// written in place: telemt's file watcher follows the file, not a rename
		if err := writeConfig(r.configPath(), out.Config(), r.cfg.GID); err != nil {
			return r.failed(err)
		}
		core = hash(out.Core)
	}
	if held != "" {
		r.proc.SetEnabled(false)
		r.coreHash = ""
	} else {
		r.proc.SetEnabled(true)
		if r.coreHash != "" && core != r.coreHash {
			r.proc.Restart()
		}
		r.coreHash = core
	}
	r.mu.Lock()
	r.held, r.applyErr, r.metrics = held, "", in.MetricsPort()
	r.mu.Unlock()
	return nil
}

func (r *Runner) failed(err error) error {
	r.mu.Lock()
	first := r.applyErr == ""
	r.applyErr = err.Error()
	r.mu.Unlock()
	if first {
		r.event("push_failed", err.Error())
	}
	return err
}

func writeConfig(path, text string, gid uint32) error {
	if err := os.WriteFile(path, []byte(text), 0o640); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o640); err != nil {
		return err
	}
	return chown(path, 0, int(gid))
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// Restart restarts telemt; open connections drop and reconnect.
func (r *Runner) Restart() { r.proc.Restart() }

// --- the loop: usage, writes, cover checks, events ---

func (r *Runner) loop(ctx context.Context) {
	stats := time.NewTicker(r.cfg.StatsEvery)
	defer stats.Stop()
	flush := time.NewTicker(r.cfg.FlushEvery)
	defer flush.Stop()
	busy := make(chan struct{}, 1) // one cover check at a time
	for {
		r.watch()
		r.poll(ctx)
		// the site may take seconds to answer, so the loop does not wait for it
		if p := r.cfg.Store.Get(); p.Cover != "" && r.cfg.Check != nil && r.coverDue(p.Cover) {
			select {
			case busy <- struct{}{}:
				go func() {
					defer func() { <-busy }()
					r.CheckCover(ctx, p.Cover)
				}()
			default:
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			if err := r.Flush(); err != nil {
				r.cfg.Log.Warn("could not store usage; it is kept in memory and written again", "err", err)
			}
		case <-stats.C:
		}
	}
}

// watch turns telemt's process state into events.
func (r *Runner) watch() {
	st := r.proc.Status()
	r.mu.Lock()
	prev, held := r.running, r.held
	running := st.Running
	r.running = &running
	crash := st.CrashLoop && r.cfg.Now().Sub(r.crashSeen) >= 5*time.Minute
	if crash {
		r.crashSeen = r.cfg.Now()
	}
	r.mu.Unlock()
	switch {
	case running && (prev == nil || !*prev):
		r.event("telemt_up", "")
	case !running && prev != nil && *prev && held == "":
		r.event("telemt_down", st.LastExit)
	}
	if crash {
		r.event("telemt_crash_loop", st.LastExit)
	}
}

// poll reads telemt's counters into the book; a stopped telemt has nobody connected.
func (r *Runner) poll(ctx context.Context) {
	var stats []users.Stat
	if r.proc.Status().Running {
		r.mu.Lock()
		port := r.metrics
		r.mu.Unlock()
		text, err := r.cfg.Scrape(ctx, port)
		if err != nil {
			return // still starting: its ports open after its first checks
		}
		up, down := telemt.ParseMetric(text, telemt.MetricUp), telemt.ParseMetric(text, telemt.MetricDown)
		conns, devs := telemt.ParseMetric(text, telemt.MetricConnections), telemt.ParseMetric(text, telemt.MetricDevices)
		names := map[string]bool{}
		for _, m := range []map[string]uint64{up, down, conns, devs} {
			for n := range m {
				names[n] = true
			}
		}
		for n := range names {
			stats = append(stats, users.Stat{Name: n, Up: up[n], Down: down[n], Connections: int(conns[n]), Devices: int(devs[n])})
		}
	}
	r.mu.Lock()
	rep, next := users.Fold(r.last, stats, true)
	r.last = next
	r.mu.Unlock()
	pending := r.book.Add(rep, r.cfg.Now())
	// a user who just used up a quota is cut off now, not at the next write
	if users.CrossesQuota(r.cfg.Store.Get().Users, pending) {
		if err := r.Flush(); err != nil {
			r.cfg.Log.Warn("could not store usage; it is kept in memory and written again", "err", err)
			return
		}
		r.Changed()
	}
}

func scrape(ctx context.Context, port int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/metrics", nil)
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

type coverCheck struct {
	res ctrl.DisguiseResult
	at  time.Time
}

// coverDue says the site in use is due a check: a failing one is watched closely,
// so its recovery shows quickly.
func (r *Runner) coverDue(domain string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.checks[strings.ToLower(domain)]
	if !ok {
		return true
	}
	wait := r.cfg.CoverEvery
	if !c.res.OK {
		wait = min(10*time.Minute, wait)
	}
	return !r.cfg.Now().Before(c.at.Add(wait))
}

// CheckCover tests a site from the router. The last few tests are kept, so a site
// checked before it was chosen shows as checked.
func (r *Runner) CheckCover(ctx context.Context, domain string) ctrl.DisguiseResult {
	domain = strings.ToLower(strings.TrimSpace(domain))
	res := r.cfg.Check(ctx, domain)
	current := domain == strings.ToLower(r.cfg.Store.Get().Cover)
	r.mu.Lock()
	prev, was := r.checks[domain]
	if !was && len(r.checks) >= 8 {
		var oldest string
		for d, c := range r.checks {
			if oldest == "" || c.at.Before(r.checks[oldest].at) {
				oldest = d
			}
		}
		delete(r.checks, oldest)
	}
	r.checks[domain] = coverCheck{res: res, at: r.cfg.Now()}
	r.mu.Unlock()
	switch {
	case !current:
	case !res.OK && (!was || prev.res.OK):
		r.event("disguise_down", domain+": "+res.Err)
	case res.OK && was && !prev.res.OK:
		r.event("disguise_up", domain)
	}
	return res
}

const ringSize = 50

// Note adds an event from outside, like the router moving the port.
func (r *Runner) Note(kind, detail string) { r.event(kind, detail) }

func (r *Runner) event(kind, detail string) {
	r.mu.Lock()
	r.events = append(r.events, Event{At: r.cfg.Now(), Kind: kind, Detail: detail})
	if len(r.events) > ringSize {
		r.events = slices.Delete(r.events, 0, len(r.events)-ringSize)
	}
	r.mu.Unlock()
	lvl := slog.LevelWarn
	if kind == "telemt_up" || kind == "disguise_up" || kind == "port_changed" {
		lvl = slog.LevelInfo
	}
	r.cfg.Log.Log(context.Background(), lvl, "proxy event", "kind", kind, "detail", detail)
}

// Events is the proxy's recent history, newest first.
func (r *Runner) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Clone(r.events)
	slices.Reverse(out)
	return out
}

// --- what the page reads ---

// Status is the proxy as the page shows it.
type Status struct {
	Running   bool
	Restarts  int
	Uptime    time.Duration
	LastExit  string
	CrashLoop bool
	Held      string
	ApplyErr  string
	Version   string
	RSS       uint64 // telemt's memory now; 0 when it isn't running
	Cover     *ctrl.DisguiseResult
	CoverAt   time.Time
}

func (r *Runner) Status() Status {
	st := r.proc.Status()
	out := Status{Running: st.Running, Restarts: st.Restarts, LastExit: st.LastExit, CrashLoop: st.CrashLoop}
	if st.Running && !st.Since.IsZero() {
		out.Uptime = r.cfg.Now().Sub(st.Since)
		out.RSS = rss(st.PID)
	}
	cur := strings.ToLower(r.cfg.Store.Get().Cover)
	r.mu.Lock()
	out.Held, out.ApplyErr, out.Version = r.held, r.applyErr, r.version
	if c, ok := r.checks[cur]; ok {
		res := c.res
		out.Cover, out.CoverAt = &res, c.at
	}
	r.mu.Unlock()
	return out
}

// rss reads a process's resident memory from /proc (0 where there is none).
func rss(pid int) uint64 {
	if pid <= 0 {
		return 0
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			f := strings.Fields(v)
			if len(f) > 0 {
				kb, _ := strconv.ParseUint(f[0], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}

// Logs is telemt's and the plugin's latest lines, oldest first.
func (r *Runner) Logs(n int) []string { return r.ring.Tail(n) }

// LogWriter puts the plugin's own lines beside telemt's.
func (r *Runner) LogWriter() io.Writer { return r.ring }

// StatsStep is how often telemt's counters are read.
func (r *Runner) StatsStep() time.Duration { return r.cfg.StatsEvery }

// Usage is the proxy's and its users' numbers right now.
func (r *Runner) Usage(p store.Proxy) users.ServerUsage {
	return r.book.Usage(p.Users, r.cfg.Now(), true, 3*r.cfg.StatsEvery, r.cfg.History, ServerID)
}

// Traffic is the recent rates, oldest first.
func (r *Runner) Traffic() []users.Sample { return r.book.Samples() }

// ServerID is the proxy's key in the usage history, which keeps servers apart.
const ServerID = 1

// Flush writes what memory holds now.
func (r *Runner) Flush() error {
	return r.book.Flush(r.cfg.Now(), r.cfg.History, ServerID, saver{r.cfg.Store})
}

// Forget drops a deleted user's numbers.
func (r *Runner) Forget(name string) {
	r.book.Forget(name)
	if r.cfg.History != nil {
		r.cfg.History.Forget(ServerID, name)
	}
}

type saver struct{ st *store.Store }

func (s saver) Users() ([]users.User, bool) { return s.st.Get().Users, true }

func (s saver) Save(fn func(us []users.User, base map[string]users.Usage)) error {
	return s.st.Mutate(func(p *store.Proxy) error {
		if p.Baseline == nil {
			p.Baseline = map[string]users.Usage{}
		}
		fn(p.Users, p.Baseline)
		return nil
	})
}
