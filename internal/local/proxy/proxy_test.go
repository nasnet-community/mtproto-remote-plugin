package proxy

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/agent/supervise"
	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/local/store"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/users"
)

// fakeProc is telemt's process: enabled means running.
type fakeProc struct {
	mu       sync.Mutex
	running  bool
	restarts int
}

func (f *fakeProc) Run(ctx context.Context) { <-ctx.Done() }
func (f *fakeProc) Restart()                { f.mu.Lock(); f.restarts++; f.mu.Unlock() }
func (f *fakeProc) SetEnabled(on bool)      { f.mu.Lock(); f.running = on; f.mu.Unlock() }
func (f *fakeProc) Stop() error             { f.SetEnabled(false); return nil }
func (f *fakeProc) Status() supervise.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return supervise.Status{Running: f.running, Restarts: f.restarts, PID: -1}
}

type rig struct {
	r       *Runner
	st      *store.Store
	proc    *fakeProc
	metrics func() string
	down    bool // the cover check fails
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	hist, _ := users.OpenHistory(dir)
	g := &rig{st: st, proc: &fakeProc{}, metrics: func() string { return "" }}
	g.r = New(Config{
		Store: st, History: hist, Bin: "telemt", Dir: dir + "/telemt", Port: 8443, MaxConns: 300,
		NewProc: func(supervise.Spec) Proc { return g.proc },
		Scrape:  func(context.Context, int) (string, error) { return g.metrics(), nil },
		Check: func(context.Context, string) ctrl.DisguiseResult {
			if g.down {
				return ctrl.DisguiseResult{Err: "cannot connect"}
			}
			return ctrl.DisguiseResult{OK: true, TLS13: true}
		},
	})
	if err := g.r.prepare(); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *rig) set(t *testing.T, fn func(p *store.Proxy)) {
	t.Helper()
	if err := g.st.Mutate(func(p *store.Proxy) error { fn(p); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.r.Apply(); err != nil {
		t.Fatal(err)
	}
}

func (g *rig) config(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(g.r.configPath())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func alice() users.User {
	return users.User{Name: "alice", Secret: strings.Repeat("a", 32), Enabled: true}
}

func TestTelemtWaitsForACoverSiteAndAUser(t *testing.T) {
	g := newRig(t)
	if err := g.r.Apply(); err != nil {
		t.Fatal(err)
	}
	if st := g.r.Status(); st.Held != "waiting for a cover site" || g.proc.Status().Running {
		t.Fatalf("before setup: %+v", st)
	}
	if _, err := os.Stat(g.r.configPath()); err == nil {
		t.Fatal("a config was written with no cover site")
	}
	g.set(t, func(p *store.Proxy) { p.Cover = "www.example.com" })
	if st := g.r.Status(); st.Held != HeldNoUsers {
		t.Fatalf("no users: %+v", st)
	}
	g.set(t, func(p *store.Proxy) { p.Users = append(p.Users, alice()) })
	if st := g.r.Status(); st.Held != "" || !g.proc.Status().Running {
		t.Fatalf("set up: %+v", st)
	}
	cfg := g.config(t)
	for _, want := range []string{`tls_domain = "www.example.com"`, "max_connections = 300", "port = 8443", `"alice" = "` + strings.Repeat("a", 32) + `"`, "mask_unix_sock"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %s:\n%s", want, cfg)
		}
	}
	g.set(t, func(p *store.Proxy) { p.Stopped = true })
	if st := g.r.Status(); st.Held != ctrl.HeldStopped || g.proc.Status().Running {
		t.Fatalf("stopped: %+v", st)
	}
	g.set(t, func(p *store.Proxy) { p.Stopped = false })
	g.r.SetBlocked("this router has no domestic link")
	if st := g.r.Status(); st.Held != "this router has no domestic link" || g.proc.Status().Running {
		t.Fatalf("blocked: %+v", st)
	}
	g.r.SetBlocked("")
	if !g.proc.Status().Running {
		t.Fatal("still off once unblocked")
	}
}

func TestOnlyACoreChangeRestartsTelemt(t *testing.T) {
	g := newRig(t)
	g.set(t, func(p *store.Proxy) { p.Cover, p.Users = "www.example.com", []users.User{alice()} })
	g.set(t, func(p *store.Proxy) {
		bob := alice()
		bob.Name, bob.Secret = "bob", strings.Repeat("b", 32)
		p.Users = append(p.Users, bob)
	})
	if n := g.proc.Status().Restarts; n != 0 {
		t.Fatalf("a new user restarted telemt %d times: its file watcher reloads users", n)
	}
	g.set(t, func(p *store.Proxy) { p.MaxConns = 500 })
	if n := g.proc.Status().Restarts; n != 1 {
		t.Fatalf("a new limit restarted it %d times", n)
	}
	if !strings.Contains(g.config(t), "max_connections = 500") {
		t.Fatal("the new limit isn't in the config")
	}
}

func metric(user string, up, down, conns int) string {
	return fmt.Sprintf("telemt_user_octets_from_client{user=%q} %d\ntelemt_user_octets_to_client{user=%q} %d\ntelemt_user_connections_current{user=%q} %d\ntelemt_user_unique_ips_current{user=%q} 1\n",
		user, up, user, down, user, conns, user)
}

func TestUsageIsCountedThenWritten(t *testing.T) {
	g := newRig(t)
	g.set(t, func(p *store.Proxy) { p.Cover, p.Users = "www.example.com", []users.User{alice()} })
	ctx := context.Background()
	g.metrics = func() string { return metric("alice", 0, 0, 0) }
	g.r.poll(ctx) // first sight: a baseline, not traffic
	g.metrics = func() string { return metric("alice", 300, 900, 2) }
	g.r.poll(ctx)
	u := g.r.Usage(g.st.Get())
	if a := u.Users["alice"]; a.Up != 300 || a.Down != 900 || a.Connections != 2 || !a.Live || u.UsersOnline != 1 {
		t.Fatalf("usage = %+v", u)
	}
	if got := g.st.Get().Users[0]; got.Up != 0 {
		t.Fatalf("written before its time: %+v", got)
	}
	if err := g.r.Flush(); err != nil {
		t.Fatal(err)
	}
	p := g.st.Get()
	if p.Users[0].Up != 300 || p.Baseline["alice"].Down != 900 {
		t.Fatalf("stored = %+v %+v", p.Users[0], p.Baseline)
	}
	// telemt restarted: its counters start again from zero
	g.metrics = func() string { return metric("alice", 50, 0, 0) }
	g.r.poll(ctx)
	if a := g.r.Usage(g.st.Get()).Users["alice"]; a.Up != 350 {
		t.Fatalf("after a restart = %+v", a)
	}
	// a stopped telemt has nobody connected
	g.set(t, func(p *store.Proxy) { p.Stopped = true })
	g.r.poll(ctx)
	if u := g.r.Usage(g.st.Get()); !u.Live || u.Connections != 0 {
		t.Fatalf("stopped = %+v", u)
	}
}

func TestAQuotaRunningOutCutsTheUserOffAtOnce(t *testing.T) {
	g := newRig(t)
	a := alice()
	a.QuotaBytes = 1000
	g.set(t, func(p *store.Proxy) { p.Cover, p.Users = "www.example.com", []users.User{a} })
	ctx := context.Background()
	g.metrics = func() string { return metric("alice", 0, 0, 0) }
	g.r.poll(ctx)
	g.metrics = func() string { return metric("alice", 400, 700, 1) }
	g.r.poll(ctx)
	if u := g.st.Get().Users[0]; u.Up+u.Down != 1100 {
		t.Fatalf("not written at once: %+v", u)
	}
	if !strings.Contains(g.config(t), `"alice" = false`) {
		t.Fatalf("alice is still enabled:\n%s", g.config(t))
	}
}

func TestTheCoverSitesCheckIsKeptAndItsFailureNoted(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	now := time.Now()
	g.r.cfg.Now = func() time.Time { return now }
	// checked before it is chosen: it shows as checked once it is
	if res := g.r.CheckCover(ctx, "WWW.Example.com"); !res.OK {
		t.Fatal("a good site failed")
	}
	g.set(t, func(p *store.Proxy) { p.Cover = "www.example.com" })
	if st := g.r.Status(); st.Cover == nil || !st.Cover.OK {
		t.Fatalf("status = %+v", st)
	}
	if g.r.coverDue("www.example.com") {
		t.Fatal("a working site is due again at once")
	}
	// the site stops working: noted once, and looked at again within ten minutes
	g.down = true
	g.r.CheckCover(ctx, "www.example.com")
	g.r.CheckCover(ctx, "www.example.com")
	if evs := g.r.Events(); len(evs) != 1 || evs[0].Kind != "disguise_down" {
		t.Fatalf("events = %+v", evs)
	}
	now = now.Add(11 * time.Minute)
	if !g.r.coverDue("www.example.com") {
		t.Fatal("a failing site waits more than ten minutes")
	}
	g.down = false
	g.r.CheckCover(ctx, "www.example.com")
	if evs := g.r.Events(); evs[0].Kind != "disguise_up" {
		t.Fatalf("events = %+v", evs)
	}
	g.set(t, func(p *store.Proxy) { p.Cover = "other.example.com" })
	if st := g.r.Status(); st.Cover != nil {
		t.Fatalf("another site's check shows: %+v", st.Cover)
	}
}
