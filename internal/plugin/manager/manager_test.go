package manager

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/hubnode"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/plugintest"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/store"
	"github.com/nasnet-community/mtproto-remote-plugin/link"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops/relaytest"
	"github.com/nasnet-community/mtproto-remote-plugin/tgnet"
)

var wait = plugintest.Wait

// flaky lets a test make store writes fail.
type flaky struct {
	*store.Store
	fail atomic.Bool
}

func (f *flaky) Mutate(id uint, fn func(*store.Server) error) error {
	if f.fail.Load() {
		return errors.New("disk full")
	}
	return f.Store.Mutate(id, fn)
}

func secret(c byte) string { return strings.Repeat(string([]byte{c}), 32) }

func recordFor(a *plugintest.Agent, id uint) *relayops.Record {
	res := a.Result()
	return &relayops.Record{
		NodeID: id, AgentID: res.AgentID, HubPort: res.HubPort, CapturePort: res.CapturePort, SSHPort: 22, SSHUser: "root",
		HostKey: "SHA256:abc", RealityPub: hex.EncodeToString(res.Creds.Pub), ShortID: hex.EncodeToString(res.Creds.ShortID[:]),
		Token: hex.EncodeToString(res.Creds.Token), Disguise: plugintest.Domains[0], MaxStreams: 2000, Arch: "amd64",
	}
}

func allow() *tgnet.Refresher {
	return &tgnet.Refresher{List: tgnet.New([]netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")})}
}

func newManager(t *testing.T, st Store, rf *tgnet.Refresher) *Manager {
	t.Helper()
	d := &net.Dialer{Timeout: 5 * time.Second}
	m := New(Config{
		Store: st, Allow: rf, Dial: d.DialContext, DialDC: d.DialContext, Conns: 1,
		StatusEvery: 100 * time.Millisecond, StatsEvery: 100 * time.Millisecond, FlushEvery: 100 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); m.Stop() })
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return m
}

type rig struct {
	agent *plugintest.Agent
	st    *flaky
	m     *Manager
	id    uint
}

func newRig(t *testing.T, users ...store.User) *rig {
	t.Helper()
	a := plugintest.Start(t)
	s, err := store.Open(t.TempDir(), store.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	st := &flaky{Store: s}
	id, err := st.Create(store.NewServer{Name: "relay", Host: "127.0.0.1", MaxStreams: 2000, Users: users})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRecord(recordFor(a, id)); err != nil {
		t.Fatal(err)
	}
	return &rig{agent: a, st: st, m: newManager(t, st, allow()), id: id}
}

func (r *rig) linkUp() bool {
	s, ok := r.m.Snapshot(r.id)
	return ok && s.Link.Up > 0
}

func (r *rig) user(name string) store.User {
	srv, _ := r.st.Get(r.id)
	for _, u := range srv.Users {
		if u.Name == name {
			return u
		}
	}
	return store.User{}
}

func userIn(s ctrl.State, name string) (ctrl.User, bool) {
	for _, u := range s.Users {
		if u.Name == name {
			return u, true
		}
	}
	return ctrl.User{}, false
}

func TestStoredServerComesUp(t *testing.T) {
	r := newRig(t, store.User{Name: "alice", Secret: secret('a'), Enabled: true, MaxIPs: 2})
	wait(t, func() bool { return r.agent.StateCount() > 0 }, "the first state")
	st := r.agent.LastState()
	if st.Port != 443 || st.Disguise != plugintest.Domains[0] || st.MaxStreams != 2000 {
		t.Fatalf("state = %+v", st)
	}
	if len(st.DCNets) != 1 || st.DCNets[0] != "203.0.113.0/24" {
		t.Fatalf("dc nets = %v", st.DCNets)
	}
	if u, ok := userIn(st, "alice"); !ok || u.Secret != secret('a') || !u.Enabled || u.MaxIPs != 2 {
		t.Fatalf("users = %+v", st.Users)
	}
	if st.Rev == "" || st.Rev != Rev(st) {
		t.Fatalf("rev %q is not the state's hash", st.Rev)
	}
	var set struct {
		Modes struct{ Classic, Secure, TLS bool }
	}
	b, _ := json.Marshal(st.Telemt)
	json.Unmarshal(b, &set)
	if !set.Modes.TLS || set.Modes.Classic || set.Modes.Secure {
		t.Fatalf("telemt settings = %s, want FakeTLS only", b)
	}

	wait(t, func() bool { v, ok := r.m.Links().View(r.id); return ok && v.Up && v.AgentVersion == "9.9.9" }, "the link view")
	wait(t, func() bool {
		srv, _ := r.st.Get(r.id)
		return srv.Record.AgentVersion == "9.9.9" && srv.Record.TelemtVersion == plugintest.OldTelemt && srv.Record.LastSeenAt != nil
	}, "the agent's versions to be stored")
	if evs := r.m.Events(r.id); len(evs) == 0 || evs[0].Kind != "link_up" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestUserChangesArePushed(t *testing.T) {
	r := newRig(t, store.User{Name: "alice", Secret: secret('a'), Enabled: true})
	wait(t, func() bool { return r.agent.StateCount() > 0 }, "the first state")
	first := r.agent.LastState().Rev

	r.st.Mutate(r.id, func(s *store.Server) error {
		s.Users = append(s.Users, store.User{Name: "bob", Secret: secret('b'), Enabled: true})
		return nil
	})
	r.m.UsersChanged(r.id)
	wait(t, func() bool { _, ok := userIn(r.agent.LastState(), "bob"); return ok }, "bob to reach the agent")
	added := r.agent.LastState().Rev
	if added == first {
		t.Fatal("a new user did not change the revision")
	}

	r.st.Mutate(r.id, func(s *store.Server) error { s.Users[1].Enabled = false; return nil })
	r.m.UsersChanged(r.id)
	wait(t, func() bool { u, ok := userIn(r.agent.LastState(), "bob"); return ok && !u.Enabled }, "bob to be disabled")
	if r.agent.LastState().Rev == added {
		t.Fatal("disabling did not change the revision")
	}

	r.st.Mutate(r.id, func(s *store.Server) error { s.Users = s.Users[:1]; return nil })
	r.m.UsersChanged(r.id)
	wait(t, func() bool { _, ok := userIn(r.agent.LastState(), "bob"); return !ok }, "bob to be removed")
	if got := r.agent.LastState().Rev; got != first {
		t.Fatalf("the same users gave revision %s, first was %s", got, first)
	}
}

func TestUsageAccumulatesAndSurvivesARestart(t *testing.T) {
	r := newRig(t, store.User{Name: "alice", Secret: secret('a'), Enabled: true})
	r.agent.Set(func(a *plugintest.Agent) {
		a.Users = []ctrl.UserStat{{Name: "alice", OctetsUp: 1000, OctetsDown: 4000}}
	})
	wait(t, func() bool { srv, _ := r.st.Get(r.id); return srv.Baseline["alice"].Up == 1000 }, "the baseline")
	if u := r.user("alice"); u.Up != 0 || u.Down != 0 {
		t.Fatalf("traffic from before we watched was counted: %+v", u)
	}
	r.agent.Set(func(a *plugintest.Agent) {
		a.Users = []ctrl.UserStat{{Name: "alice", OctetsUp: 1600, OctetsDown: 4500}}
	})
	wait(t, func() bool { return r.user("alice").Up == 600 }, "+600 up")
	if u := r.user("alice"); u.Down != 500 {
		t.Fatalf("down = %d", u.Down)
	}

	r.m.Stop()
	r.agent.Set(func(a *plugintest.Agent) {
		a.Users = []ctrl.UserStat{{Name: "alice", OctetsUp: 2000, OctetsDown: 5000}}
	})
	m2 := newManager(t, r.st, allow())
	wait(t, func() bool { return r.user("alice").Up == 1000 }, "the traffic made while the plugin was down")
	time.Sleep(400 * time.Millisecond)
	if u := r.user("alice"); u.Up != 1000 || u.Down != 1000 {
		t.Fatalf("after restart: %+v, want 1000/1000", u)
	}
	_ = m2
}

func TestAFailedWriteReportsTheDeltaAgain(t *testing.T) {
	r := newRig(t, store.User{Name: "carol", Secret: secret('c'), Enabled: true})
	r.agent.Set(func(a *plugintest.Agent) { a.Users = []ctrl.UserStat{{Name: "carol", OctetsUp: 10, OctetsDown: 10}} })
	wait(t, func() bool { srv, _ := r.st.Get(r.id); return srv.Baseline["carol"].Up == 10 }, "the baseline")
	r.st.fail.Store(true)
	r.agent.Set(func(a *plugintest.Agent) { a.Users = []ctrl.UserStat{{Name: "carol", OctetsUp: 110, OctetsDown: 10}} })
	time.Sleep(400 * time.Millisecond)
	r.st.fail.Store(false)
	wait(t, func() bool { return r.user("carol").Up == 100 }, "the delta once the disk recovers")
	time.Sleep(300 * time.Millisecond)
	if up := r.user("carol").Up; up != 100 {
		t.Fatalf("booked %d, want exactly 100", up)
	}
}

func TestAUserPastQuotaIsDisabled(t *testing.T) {
	r := newRig(t, store.User{Name: "dave", Secret: secret('d'), Enabled: true, QuotaBytes: 1000})
	r.agent.Set(func(a *plugintest.Agent) { a.Users = []ctrl.UserStat{{Name: "dave", OctetsUp: 0, OctetsDown: 0}} })
	wait(t, func() bool { u, ok := userIn(r.agent.LastState(), "dave"); return ok && u.Enabled }, "dave enabled")
	wait(t, func() bool { srv, _ := r.st.Get(r.id); _, ok := srv.Baseline["dave"]; return ok }, "dave's baseline")
	r.agent.Set(func(a *plugintest.Agent) { a.Users = []ctrl.UserStat{{Name: "dave", OctetsUp: 400, OctetsDown: 700}} })
	wait(t, func() bool { u, ok := userIn(r.agent.LastState(), "dave"); return ok && !u.Enabled }, "dave to be cut off")
	if u := r.user("dave"); !u.Enabled || !u.OverQuota() {
		t.Fatalf("stored = %+v: the flag stays, the quota is what stops him", u)
	}
	r.st.Mutate(r.id, func(s *store.Server) error { s.Users[0].Up, s.Users[0].Down = 0, 0; return nil })
	r.m.UsersChanged(r.id)
	wait(t, func() bool { u, _ := userIn(r.agent.LastState(), "dave"); return u.Enabled }, "a usage reset to let dave back in")
}

func TestReservations(t *testing.T) {
	s, _ := store.Open(t.TempDir(), store.Limits{})
	m := newManager(t, s, allow())
	rel1, err := m.Reserve(Install{Host: "203.0.113.1", MaxStreams: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reserve(Install{Host: "203.0.113.2", MaxStreams: 2500}); !errors.Is(err, store.ErrStreamBudget) {
		t.Fatalf("4000+2500 pending: %v", err)
	}
	if _, err := m.Reserve(Install{Host: "203.0.113.1", MaxStreams: 100}); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second install to the same host: %v", err)
	}
	rel2, err := m.Reserve(Install{Host: "203.0.113.2", MaxStreams: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reserve(Install{Host: "203.0.113.3", MaxStreams: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reserve(Install{Host: "203.0.113.4", MaxStreams: 100}); !errors.Is(err, store.ErrServerCap) {
		t.Fatalf("a fourth pending install: %v", err)
	}
	rel1()
	rel1() // twice is harmless
	if got := m.Allocated(); got != 2000 {
		t.Fatalf("allocated with two pending = %d", got)
	}
	rel2()

	// a host that already is a server here is repaired, not installed again
	id, _ := s.Create(store.NewServer{Name: "a", Host: "203.0.113.9", MaxStreams: 100})
	_ = id
	if _, err := m.Reserve(Install{Host: "203.0.113.9", MaxStreams: 100}); !errors.Is(err, ErrHostTaken) {
		t.Fatalf("installing over a stored server: %v", err)
	}
}

func service(m *Manager, st *store.Store, ssh *relaytest.SSH) *relayops.Service {
	return relayops.NewService(relayops.ServiceConfig{
		Store: st.Records(), Links: m.Links(), Bins: relaytest.Bins{}, SSH: ssh, Admin: m.Admin(),
		LinkWait: 20 * time.Second,
	})
}

func waitJob(t *testing.T, svc *relayops.Service, id string) relayops.JobView {
	t.Helper()
	var v relayops.JobView
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if v, _ = svc.Job(id); v.State != relayops.JobRunning {
			return v
		}
	}
	t.Fatalf("the job never finished: %+v", v)
	return v
}

func TestInstallRotateAndUninstallThroughRelayops(t *testing.T) {
	a := plugintest.Start(t)
	st, _ := store.Open(t.TempDir(), store.Limits{})
	m := newManager(t, st, allow())
	ssh := &relaytest.SSH{FP: "SHA256:abc", Arch_: "amd64", Result: a.Result()}
	svc := service(m, st, ssh)

	release, err := m.Reserve(Install{Host: "127.0.0.1", MaxStreams: 1500, FirstUser: "first"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	p := relaytest.GoodInstall()
	p.Login.Host, p.Disguise, p.MaxStreams = "127.0.0.1", plugintest.Domains[0], 1500
	jid, err := svc.StartInstall(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if v := waitJob(t, svc, jid); v.State != relayops.JobOK {
		t.Fatalf("install: %+v", v)
	}
	srvs := st.List()
	if len(srvs) != 1 {
		t.Fatalf("servers = %+v", srvs)
	}
	srv := srvs[0]
	if srv.Name != "Relay 1" || srv.Host != "127.0.0.1" || srv.Record.MaxStreams != 1500 || srv.CoverDomain() != plugintest.Domains[0] {
		t.Fatalf("server = %+v", srv)
	}
	if len(srv.Users) != 1 || srv.Users[0].Name != "first" || len(srv.Users[0].Secret) != 32 || !srv.Users[0].Enabled {
		t.Fatalf("first user = %+v", srv.Users)
	}
	wait(t, func() bool { _, ok := userIn(a.LastState(), "first"); return ok }, "the first user to reach the agent")
	if got := m.Allocated(); got != 1500 {
		t.Fatalf("the reservation was counted twice: %d", got)
	}

	before, _ := st.Get(srv.ID)
	jid, err = svc.StartRotate(context.Background(), srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v := waitJob(t, svc, jid); v.State != relayops.JobOK {
		t.Fatalf("rotate: %+v", v)
	}
	after, _ := st.Get(srv.ID)
	if after.Record.Token == before.Record.Token || after.Record.RealityPub == before.Record.RealityPub {
		t.Fatal("rotation did not store new credentials")
	}
	wait(t, func() bool { s, _ := m.Snapshot(srv.ID); return s.Link.Up > 0 }, "the link on the new credentials")

	jid, err = svc.StartUninstall(context.Background(), srv.ID, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if v := waitJob(t, svc, jid); v.State != relayops.JobOK {
		t.Fatalf("uninstall: %+v", v)
	}
	if len(st.List()) != 0 || m.IsRunning(srv.ID) {
		t.Fatal("the server outlived its uninstall")
	}
}

func TestInstallPastTheCapIsUndone(t *testing.T) {
	st, _ := store.Open(t.TempDir(), store.Limits{MaxServers: 1})
	st.Create(store.NewServer{Name: "a", Host: "203.0.113.9", MaxStreams: 100})
	m := newManager(t, st, allow())
	ssh := &relaytest.SSH{FP: "SHA256:abc", Arch_: "amd64"}
	svc := service(m, st, ssh)
	jid, err := svc.StartInstall(context.Background(), relaytest.GoodInstall())
	if err != nil {
		t.Fatal(err)
	}
	v := waitJob(t, svc, jid)
	if v.State != relayops.JobFailed || !strings.Contains(v.Error, "at most 1 server") {
		t.Fatalf("job = %+v", v)
	}
	if ssh.Uninstalls != 1 {
		t.Fatal("the half-done install was left on the VPS")
	}
}

func TestRemovingAServerStopsIt(t *testing.T) {
	r := newRig(t)
	wait(t, r.linkUp, "the link")
	r.m.Links().Remove(r.id)
	if r.m.IsRunning(r.id) {
		t.Fatal("still running")
	}
	if err := r.m.Call(context.Background(), r.id, ctrl.MStatus, nil, nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("call after remove: %v", err)
	}
	if err := r.m.Admin().DeleteNode(context.Background(), r.id); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.st.Get(r.id); ok {
		t.Fatal("DeleteNode left the server stored")
	}
}

func TestANewCoverSiteMovesTheSNIOnceApplied(t *testing.T) {
	r := newRig(t, store.User{Name: "alice", Secret: secret('a'), Enabled: true})
	wait(t, func() bool { return r.agent.StateCount() > 0 }, "the first state")
	r.st.Mutate(r.id, func(s *store.Server) error { s.Cover = plugintest.Domains[1]; return nil })
	if srv, _ := r.st.Get(r.id); srv.Record.Disguise != plugintest.Domains[0] {
		t.Fatal("the applied cover moved before the agent took it")
	}
	r.m.UsersChanged(r.id)
	wait(t, func() bool { return r.agent.LastState().Disguise == plugintest.Domains[1] }, "the new cover site")
	wait(t, func() bool { srv, _ := r.st.Get(r.id); return srv.Record.Disguise == plugintest.Domains[1] }, "the record to follow")
	if sni := r.m.entry(r.id).node.Hub().Creds().Reality.ServerName; sni != plugintest.Domains[1] {
		t.Fatalf("new connections would present %q", sni)
	}
}

func TestTelemtTroubleBecomesEvents(t *testing.T) {
	r := newRig(t, store.User{Name: "alice", Secret: secret('a'), Enabled: true})
	wait(t, func() bool { s, _ := r.m.Snapshot(r.id); return s.Agent != nil }, "a status report")
	r.agent.Set(func(a *plugintest.Agent) { a.Running, a.CrashLoop = false, true })
	has := func(kind string) bool {
		for _, e := range r.m.Events(r.id) {
			if e.Kind == kind {
				return true
			}
		}
		return false
	}
	wait(t, func() bool { return has("telemt_down") && has("telemt_crash_loop") }, "telemt events")
	time.Sleep(400 * time.Millisecond)
	n := 0
	for _, e := range r.m.Events(r.id) {
		if e.Kind == "telemt_crash_loop" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("a crash loop reported every poll fills the ring: %d entries", n)
	}
	r.agent.Set(func(a *plugintest.Agent) { a.Running, a.CrashLoop = true, false })
	wait(t, func() bool { return has("telemt_up") }, "telemt_up")
	if evs := r.m.Events(r.id); evs[0].Kind != "telemt_up" {
		t.Fatalf("events are not newest first: %+v", evs)
	}

	r.agent.Stop()
	wait(t, func() bool { return has("link_down") }, "link_down")
}

func TestARefreshedAllowlistIsPushed(t *testing.T) {
	r := newRig(t, store.User{Name: "alice", Secret: secret('a'), Enabled: true})
	wait(t, func() bool { return r.agent.StateCount() > 0 }, "the first state")
	if _, err := r.m.cfg.Allow.List.Merge([]netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}); err != nil {
		t.Fatal(err)
	}
	r.m.PrefixesChanged()
	wait(t, func() bool { return len(r.agent.LastState().DCNets) == 2 }, "the new prefixes")
}

func TestRevIgnoresUserOrder(t *testing.T) {
	m := New(Config{Allow: allow()})
	a := store.Server{Record: relayops.Record{Disguise: "x.example.com", MaxStreams: 10}, Users: []store.User{
		{Name: "b", Secret: secret('b'), Enabled: true}, {Name: "a", Secret: secret('a'), Enabled: true},
	}}
	b := a
	b.Users = []store.User{a.Users[1], a.Users[0]}
	if m.Desired(a).Rev != m.Desired(b).Rev {
		t.Fatal("equal user sets gave different revisions")
	}
	exp := a
	exp.Users = append([]store.User(nil), a.Users...)
	exp.Users[0].ExpiresAt = time.Now().Add(-time.Hour)
	st := m.Desired(exp)
	if u, _ := userIn(st, "b"); u.ExpiresAt.IsZero() {
		t.Fatal("an expired user's expiry was not passed on")
	}
}

// hooked runs a test's hook once, right after the next read of a server.
type hooked struct {
	*store.Store
	hook atomic.Pointer[func()]
}

func (h *hooked) Get(id uint) (store.Server, bool) {
	srv, ok := h.Store.Get(id)
	if f := h.hook.Swap(nil); f != nil {
		(*f)()
	}
	return srv, ok
}

func TestAUserChangeDuringARestartIsNotLost(t *testing.T) {
	a := plugintest.Start(t)
	s, _ := store.Open(t.TempDir(), store.Limits{})
	st := &hooked{Store: s}
	id, _ := st.Create(store.NewServer{Name: "relay", Host: "127.0.0.1", MaxStreams: 2000, Users: []store.User{{Name: "alice", Secret: secret('a'), Enabled: true}}})
	st.SaveRecord(recordFor(a, id))
	m := newManager(t, st, allow())
	wait(t, func() bool { _, ok := userIn(a.LastState(), "alice"); return ok }, "alice on the agent")

	// a user is added right after the restart read the store, before the new link is registered
	late := func() {
		st.Mutate(id, func(s *store.Server) error {
			s.Users = append(s.Users, store.User{Name: "late", Secret: secret('l'), Enabled: true})
			return nil
		})
		m.UsersChanged(id)
	}
	st.hook.Store(&late)
	if err := m.Ensure(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool { _, ok := userIn(a.LastState(), "late"); return ok }, "the user added during the restart")
}

func TestAStreamCapCountsPendingInstalls(t *testing.T) {
	r := newRig(t)
	release, err := r.m.Reserve(Install{Host: "203.0.113.7", MaxStreams: 3500})
	if err != nil {
		t.Fatal(err)
	}
	grow := func(n int) error {
		return r.m.Edit(r.id, func(s *store.Server) error { s.Record.MaxStreams = n; return nil })
	}
	if err := grow(3000); !errors.Is(err, store.ErrStreamBudget) || !strings.Contains(err.Error(), "only 2500 streams are left") {
		t.Fatalf("3000 beside 3500 pending: %v", err)
	}
	if err := grow(1000); err != nil {
		t.Fatalf("shrinking: %v", err)
	}
	release()
	if err := grow(6000); err != nil {
		t.Fatalf("the whole budget once the install is gone: %v", err)
	}
}

// cutDialer keeps its connections so a test can drop them all, as a blip would.
type cutDialer struct {
	mu   sync.Mutex
	raws []net.Conn
}

func (c *cutDialer) dial(ctx context.Context, n, a string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, n, a)
	if err == nil {
		c.mu.Lock()
		c.raws = append(c.raws, conn)
		c.mu.Unlock()
	}
	return conn, err
}

func (c *cutDialer) cut() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, conn := range c.raws {
		conn.Close()
	}
	c.raws = nil
}

func cutManager(t *testing.T, st Store, cd *cutDialer) *Manager {
	t.Helper()
	m := New(Config{Store: st, Allow: allow(), Dial: cd.dial, DialDC: cd.dial, Conns: 1, StatusEvery: 100 * time.Millisecond, StatsEvery: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); m.Stop() })
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return m
}

// rotations are the keys.rotate requests that are not the finishing one.
func rotations(method string, body json.RawMessage) (ctrl.KeysRotate, bool) {
	var k ctrl.KeysRotate
	if method != ctrl.MKeysRotate || json.Unmarshal(body, &k) != nil || k.Finish {
		return k, false
	}
	return k, true
}

func TestALostRotationReplyIsRecoveredOverTheLink(t *testing.T) {
	a := plugintest.Start(t)
	st, _ := store.Open(t.TempDir(), store.Limits{})
	id, _ := st.Create(store.NewServer{Name: "relay", Host: "127.0.0.1", MaxStreams: 2000})
	st.SaveRecord(recordFor(a, id))
	cd := &cutDialer{}
	m := cutManager(t, st, cd)
	var (
		mu   sync.Mutex
		seen [][]byte
		kept bool // the panel had the candidate on disk before the agent switched
	)
	a.Set(func(a *plugintest.Agent) {
		a.Gate = func(method string, body json.RawMessage) {
			k, ok := rotations(method, body)
			if !ok {
				return
			}
			srv, _ := st.Get(id)
			mu.Lock()
			seen = append(seen, k.Token)
			first := len(seen) == 1
			kept = kept || srv.Record.NextSeed != ""
			mu.Unlock()
			if first {
				cd.cut() // the switch still happens; its answer is lost with the connection
			}
		}
	})
	wait(t, func() bool { s, _ := m.Snapshot(id); return s.Link.Up > 0 }, "the link")
	before := a.Result().Creds.Token
	svc := relayops.NewService(relayops.ServiceConfig{Store: st.Records(), Links: m.Links(), Admin: m.Admin(), Bins: relaytest.Bins{}, LinkWait: 30 * time.Second})
	jid, err := svc.StartRotate(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if v := waitJob(t, svc, jid); v.State != relayops.JobOK {
		t.Fatalf("rotate: %+v", v)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || !bytes.Equal(seen[0], seen[1]) || !kept {
		t.Fatalf("the agent saw %d rotations (kept on disk first: %v); the repeat must be the same one", len(seen), kept)
	}
	after, _ := st.Get(id)
	if after.Record.Token != hex.EncodeToString(seen[0]) || after.Record.Token == hex.EncodeToString(before) || after.Record.NextSeed != "" {
		t.Fatalf("stored = %+v", after.Record)
	}
	wait(t, func() bool { s, _ := m.Snapshot(id); return s.Link.Up > 0 }, "the link on the new credentials")
}

func TestARotationLostAcrossARestartIsFinishedAfterIt(t *testing.T) {
	a := plugintest.Start(t)
	dir := t.TempDir()
	st, _ := store.Open(dir, store.Limits{})
	id, _ := st.Create(store.NewServer{Name: "relay", Host: "127.0.0.1", MaxStreams: 2000})
	st.SaveRecord(recordFor(a, id))
	reached, release := make(chan struct{}), make(chan struct{})
	var (
		mu   sync.Mutex
		seen [][]byte
	)
	a.Set(func(a *plugintest.Agent) {
		a.Gate = func(method string, body json.RawMessage) {
			k, ok := rotations(method, body)
			if !ok {
				return
			}
			mu.Lock()
			seen = append(seen, k.Token)
			first := len(seen) == 1
			mu.Unlock()
			if first {
				close(reached)
				<-release // the router restarts before this answer
			}
		}
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	m := cutManager(t, st, &cutDialer{})
	wait(t, func() bool { s, _ := m.Snapshot(id); return s.Link.Up > 0 }, "the link")
	original := a.Result().Creds.Token
	svc := relayops.NewService(relayops.ServiceConfig{Store: st.Records(), Links: m.Links(), Admin: m.Admin(), Bins: relaytest.Bins{}, LinkWait: time.Second})
	jid, err := svc.StartRotate(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the agent never got the rotation")
	}
	m.Stop()
	if v := waitJob(t, svc, jid); v.State != relayops.JobFailed {
		t.Fatalf("rotate = %+v", v)
	}
	close(release)
	wait(t, func() bool { return !bytes.Equal(a.Result().Creds.Token, original) }, "the agent to switch")

	// a fresh start from the file: the agent takes only the new keys now
	st2, err := store.Open(dir, store.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if srv, _ := st2.Get(id); srv.Record.NextSeed == "" {
		t.Fatal("the pending rotation was not kept")
	}
	m2 := cutManager(t, st2, &cutDialer{})
	wait(t, func() bool { s, _ := m2.Snapshot(id); return s.Link.Up > 0 }, "the link on the pending credentials")
	svc2 := relayops.NewService(relayops.ServiceConfig{Store: st2.Records(), Links: m2.Links(), Admin: m2.Admin(), Bins: relaytest.Bins{}, LinkWait: 30 * time.Second})
	if jid, err = svc2.StartRotate(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if v := waitJob(t, svc2, jid); v.State != relayops.JobOK {
		t.Fatalf("rotate after the restart = %+v", v)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || !bytes.Equal(seen[0], seen[1]) {
		t.Fatalf("the agent saw %d rotations; the restart must resend the same one", len(seen))
	}
	after, _ := st2.Get(id)
	if after.Record.Token != hex.EncodeToString(seen[0]) || after.Record.NextSeed != "" {
		t.Fatalf("stored = %+v", after.Record)
	}
	wait(t, func() bool { s, _ := m2.Snapshot(id); return s.Link.Up > 0 }, "the link on the new credentials")
}

func TestAnEventKeepsItsErrorApart(t *testing.T) {
	m := New(Config{Store: &flaky{}, Allow: allow()})
	m.onEvent(&entry{id: 1, up: map[string]bool{}}, hubnode.Event{Kind: hubnode.EventPushFailed, Detail: "r1", Err: errors.New("link: mux: closed")})
	if evs := m.Events(1); len(evs) != 1 || evs[0].Detail != "r1" || evs[0].Err != "link: mux: closed" {
		t.Fatalf("events = %+v", evs)
	}
}

type logged struct {
	mu   sync.Mutex
	msgs []string
}

func (l *logged) Enabled(context.Context, slog.Level) bool { return true }
func (l *logged) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, r.Message)
	return nil
}
func (l *logged) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logged) WithGroup(string) slog.Handler      { return l }

// a record with no host key, cover site or token can't be repaired, only removed
func TestAHalfRegisteredServerIsToldToBeRemoved(t *testing.T) {
	st, err := store.Open(t.TempDir(), store.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(store.NewServer{Name: "half", Host: "192.0.2.9", MaxStreams: 2000}); err != nil {
		t.Fatal(err)
	}
	l := &logged{}
	m := New(Config{Store: st, Allow: allow(), Log: slog.New(l)})
	m.Start(context.Background())
	defer m.Stop()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.msgs) != 1 || !strings.Contains(l.msgs[0], "remove it") || strings.Contains(l.msgs[0], "repair") {
		t.Fatalf("logged %q", l.msgs)
	}
}

// a rotation landing while a new cover site is applied must not put the old site back
func TestSetCredsKeepsACoverSiteAppliedMeanwhile(t *testing.T) {
	a := plugintest.Start(t)
	rec := recordFor(a, 1)
	st := &flaky{}
	st.fail.Store(true) // onApplied's write fails fast; only the entry matters here
	for trial := 0; trial < 30; trial++ {
		m := New(Config{Store: st, Allow: allow(), Log: slog.New(slog.DiscardHandler)})
		cr, _ := rec.LinkCreds()
		e := &entry{id: 1, rec: *rec, up: map[string]bool{}, node: hubnode.New(hubnode.Config{Link: link.HubConfig{Token: cr.Token, Reality: cr.Reality}})}
		m.entries[1] = e
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := m.SetCreds(context.Background(), rec); err != nil {
					t.Error(err)
					return
				}
			}
		}()
		last := ""
		for i := 1; i <= 50; i++ {
			last = fmt.Sprintf("c%d.example.com", i)
			m.onApplied(e, ctrl.State{Disguise: last})
		}
		close(stop)
		<-done
		e.mu.Lock()
		got := e.rec.Disguise
		e.mu.Unlock()
		if sni := e.node.Hub().Creds().Reality.ServerName; got != last || sni != last {
			t.Fatalf("trial %d: record says %s and the link dials %s, want %s", trial, got, sni, last)
		}
	}
}

// The box moved to another cover site behind the panel's back, so neither the record nor
// the wanted site gets in. A repair adopts the box's site so the link does; then the wanted
// one goes out like any cover change.
func TestARepairAdoptsTheCoverSiteTheBoxRuns(t *testing.T) {
	r := newRig(t, store.User{Name: "alice", Secret: secret('a'), Enabled: true})
	wait(t, r.linkUp, "the link")
	r.agent.OnlyAccept(plugintest.Domains[1])
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	r.agent.Set(func(a *plugintest.Agent) {
		a.Gate = func(method string, body json.RawMessage) {
			if method != ctrl.MStateApply {
				return
			}
			<-release
			var s ctrl.State
			json.Unmarshal(body, &s)
			r.agent.OnlyAccept(s.Disguise, plugintest.Domains[1]) // like the agent: the old name stays good a while
		}
	})
	res := r.agent.Result()
	res.Domain = plugintest.Domains[1]
	svc := service(r.m, r.st.Store, &relaytest.SSH{FP: "SHA256:abc", Arch_: "amd64", Result: res})
	jid, err := svc.StartRepair(context.Background(), r.id, relayops.Login{Password: "pw"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if v := waitJob(t, svc, jid); v.State != relayops.JobOK {
		t.Fatalf("repair: %+v", v)
	}
	sni := func() string { return r.m.entry(r.id).node.Hub().Creds().Reality.ServerName }
	if srv, _ := r.st.Get(r.id); srv.Record.Disguise != plugintest.Domains[1] || sni() != plugintest.Domains[1] {
		t.Fatalf("record %q, the link dials %q", srv.Record.Disguise, sni())
	}
	once.Do(func() { close(release) })
	wait(t, func() bool {
		srv, _ := r.st.Get(r.id)
		return srv.Record.Disguise == plugintest.Domains[0] && sni() == plugintest.Domains[0]
	}, "the wanted cover site to be applied")
	wait(t, r.linkUp, "the link on the wanted site")
}
