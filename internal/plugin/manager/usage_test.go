package manager

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/plugintest"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/store"
)

// clock is a settable Now for tests that cross midnight.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

// slowRig writes usage only when told to, like the router does every few minutes.
func slowRig(t *testing.T, hist *store.History, now func() time.Time, users ...store.User) *rig {
	t.Helper()
	r := newRig(t, users...)
	r.m.Stop() // the quick one newRig starts
	d := &net.Dialer{Timeout: 5 * time.Second}
	m := New(Config{
		Store: r.st, Allow: allow(), Dial: d.DialContext, DialDC: d.DialContext, Conns: 1,
		StatusEvery: 100 * time.Millisecond, StatsEvery: 100 * time.Millisecond, FlushEvery: time.Hour,
		History: hist, Now: now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); m.Stop() })
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.m = m
	return r
}

func TestUsageWaitsInMemoryUntilItIsWritten(t *testing.T) {
	r := slowRig(t, nil, nil, store.User{Name: "alice", Secret: secret('a'), Enabled: true})
	r.agent.Set(func(a *plugintest.Agent) {
		a.Live = true
		a.Users = []ctrl.UserStat{{Name: "alice", OctetsUp: 0, OctetsDown: 0, Devices: 1}}
	})
	srvNow := func() store.Server { s, _ := r.st.Get(r.id); return s }
	wait(t, func() bool { return !r.m.Usage(srvNow()).UpdatedAt.IsZero() }, "a usage poll")
	r.agent.Set(func(a *plugintest.Agent) {
		a.Users = []ctrl.UserStat{{Name: "alice", OctetsUp: 300, OctetsDown: 900, Connections: 2, Devices: 1}}
	})
	wait(t, func() bool { return r.m.Usage(srvNow()).Users["alice"].Up == 300 }, "alice's traffic in memory")
	got := r.m.Usage(srvNow())
	if a := got.Users["alice"]; a.Down != 900 || a.TodayUp != 300 || a.MonthDown != 900 || !a.Live || a.Connections != 2 || a.LastSeen.IsZero() {
		t.Fatalf("alice = %+v", a)
	}
	if got.UsersOnline != 1 || got.Connections != 2 || !got.Live {
		t.Fatalf("server = %+v", got)
	}
	if u := r.user("alice"); u.Up != 0 {
		t.Fatalf("written before its time: %+v", u)
	}
	if len(r.m.Traffic(r.id)) == 0 {
		t.Fatal("no rate samples")
	}

	if err := r.m.Flush(r.id); err != nil {
		t.Fatal(err)
	}
	u := r.user("alice")
	if u.Up != 300 || u.Down != 900 || u.Today.Up != 300 || u.LastSeenAt.IsZero() {
		t.Fatalf("stored = %+v", u)
	}
	if srv := srvNow(); srv.Baseline["alice"].Up != 300 {
		t.Fatalf("baseline = %+v", srv.Baseline)
	}
	// nothing counted twice: memory let go of what was written
	if a := r.m.Usage(srvNow()).Users["alice"]; a.Up != 300 || a.TodayUp != 300 {
		t.Fatalf("after the write = %+v", a)
	}

	// stopping writes what is left
	r.agent.Set(func(a *plugintest.Agent) { a.Users = []ctrl.UserStat{{Name: "alice", OctetsUp: 350, OctetsDown: 900}} })
	wait(t, func() bool { return r.m.Usage(srvNow()).Users["alice"].Up == 350 }, "more traffic in memory")
	r.m.Stop()
	if u := r.user("alice"); u.Up != 350 {
		t.Fatalf("stop did not write: %+v", u)
	}
}

func TestAQuotaRunningOutIsWrittenAtOnce(t *testing.T) {
	r := slowRig(t, nil, nil, store.User{Name: "dave", Secret: secret('d'), Enabled: true, QuotaBytes: 1000})
	r.agent.Set(func(a *plugintest.Agent) { a.Users = []ctrl.UserStat{{Name: "dave"}} })
	wait(t, func() bool { u, ok := userIn(r.agent.LastState(), "dave"); return ok && u.Enabled }, "dave enabled")
	srvNow := func() store.Server { s, _ := r.st.Get(r.id); return s }
	wait(t, func() bool { return !r.m.Usage(srvNow()).UpdatedAt.IsZero() }, "a usage poll")
	r.agent.Set(func(a *plugintest.Agent) { a.Users = []ctrl.UserStat{{Name: "dave", OctetsUp: 400, OctetsDown: 700}} })
	wait(t, func() bool { u, _ := userIn(r.agent.LastState(), "dave"); return !u.Enabled }, "dave cut off without waiting for the next write")
	if u := r.user("dave"); u.Up+u.Down != 1100 {
		t.Fatalf("stored = %+v", u)
	}
}

func TestAFinishedDayMovesToTheHistory(t *testing.T) {
	hist, err := store.OpenHistory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 10, 10, 23, 59, 0, 0, time.UTC)}
	r := slowRig(t, hist, c.now, store.User{Name: "erin", Secret: secret('e'), Enabled: true})
	srvNow := func() store.Server { s, _ := r.st.Get(r.id); return s }
	r.agent.Set(func(a *plugintest.Agent) { a.Users = []ctrl.UserStat{{Name: "erin"}} })
	wait(t, func() bool { return !r.m.Usage(srvNow()).UpdatedAt.IsZero() }, "a usage poll")
	r.agent.Set(func(a *plugintest.Agent) { a.Users = []ctrl.UserStat{{Name: "erin", OctetsUp: 100, OctetsDown: 200}} })
	wait(t, func() bool { return r.m.Usage(srvNow()).Users["erin"].TodayUp == 100 }, "the 10th's traffic")
	if err := r.m.Flush(r.id); err != nil {
		t.Fatal(err)
	}

	c.set(time.Date(2026, 10, 11, 0, 1, 0, 0, time.UTC))
	r.agent.Set(func(a *plugintest.Agent) { a.Users = []ctrl.UserStat{{Name: "erin", OctetsUp: 110, OctetsDown: 200}} })
	wait(t, func() bool { return r.m.Usage(srvNow()).Users["erin"].TodayUp == 10 }, "the 11th's traffic")
	if a := r.m.Usage(srvNow()).Users["erin"]; a.MonthUp != 110 || a.MonthDown != 200 {
		t.Fatalf("before the day rolls over = %+v", a)
	}
	if err := r.m.Flush(r.id); err != nil {
		t.Fatal(err)
	}
	if up, down := hist.Sum(r.id, "erin", "2026-10-10"); up != 100 || down != 200 {
		t.Fatalf("history = %d/%d", up, down)
	}
	if u := r.user("erin"); u.Today.Date != "2026-10-11" || u.Today.Up != 10 {
		t.Fatalf("today = %+v", u.Today)
	}
	if a := r.m.Usage(srvNow()).Users["erin"]; a.TodayUp != 10 || a.MonthUp != 110 {
		t.Fatalf("after = %+v", a)
	}
}
