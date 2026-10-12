package hubnode

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/link"
	"github.com/nasnet-community/mtproto-remote-plugin/reality"
	"github.com/nasnet-community/mtproto-remote-plugin/realityserver"
	"github.com/nasnet-community/mtproto-remote-plugin/tgnet"
)

// fakeAgent answers the control calls from memory.
type fakeAgent struct {
	mu       sync.Mutex
	applied  string
	applies  int
	failWith string
	running  bool
	crash    bool
	users    []ctrl.UserStat
	coverOK  bool
	checked  []string
	onApply  func(rev string)    // runs before an apply, outside the lock
	onCheck  func(domain string) // runs before a cover check, outside the lock
}

func (f *fakeAgent) handlers() map[string]ctrl.Handler {
	return map[string]ctrl.Handler{
		ctrl.MStateApply: func(_ context.Context, b json.RawMessage, _ io.Reader) (any, error) {
			var s ctrl.State
			json.Unmarshal(b, &s)
			f.mu.Lock()
			hook := f.onApply
			f.mu.Unlock()
			if hook != nil {
				hook(s.Rev)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.applies++
			if f.failWith != "" {
				return nil, &ctrl.Error{Code: "bad_state", Msg: f.failWith}
			}
			f.applied = s.Rev
			return ctrl.StateAck{Rev: s.Rev}, nil
		},
		ctrl.MStatus: func(context.Context, json.RawMessage, io.Reader) (any, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			var st ctrl.Status
			st.StateRev = f.applied
			st.Telemt.Running, st.Telemt.CrashLoop = f.running, f.crash
			return st, nil
		},
		ctrl.MStats: func(context.Context, json.RawMessage, io.Reader) (any, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return ctrl.Stats{Users: append([]ctrl.UserStat(nil), f.users...)}, nil
		},
		ctrl.MDisguiseCheck: func(_ context.Context, b json.RawMessage, _ io.Reader) (any, error) {
			var c ctrl.DisguiseCheck
			json.Unmarshal(b, &c)
			f.mu.Lock()
			hook := f.onCheck
			f.mu.Unlock()
			if hook != nil {
				hook(c.Domain)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.checked = append(f.checked, c.Domain)
			if f.coverOK {
				return ctrl.DisguiseResult{OK: true, TLS13: true, X25519: true, H2: true, CertOK: true, Addrs: []string{"192.0.2.1"}}, nil
			}
			return ctrl.DisguiseResult{Err: "cannot connect to " + c.Domain + ":443 from the VPS"}, nil
		},
	}
}

func (f *fakeAgent) checks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.checked...)
}

func (f *fakeAgent) set(fn func(*fakeAgent)) { f.mu.Lock(); fn(f); f.mu.Unlock() }
func (f *fakeAgent) get() (applied string, applies int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applied, f.applies
}

type rig struct {
	agent *fakeAgent
	cfg   link.HubConfig
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dest := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dest.EnableHTTP2 = true
	dest.StartTLS()
	t.Cleanup(dest.Close)
	priv, pub, _ := reality.GenerateKey()
	token := make([]byte, 32)
	rand.Read(token)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	fa := &fakeAgent{running: true, coverOK: true}
	ag := link.NewAgent(link.AgentConfig{
		AgentID: "a1", Tokens: [][]byte{token}, Listener: ln,
		Reality:  realityserver.ServerConfig{Dest: dest.Listener.Addr().String(), ServerNames: []string{"example.com"}, PrivateKey: priv, ShortIDs: [][8]byte{{1}}, MaxTimeDiff: time.Minute},
		Handlers: fa.handlers(),
	})
	go ag.Serve(ctx)
	for i := 0; !ag.Warm() && i < 300; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	var d net.Dialer
	return &rig{agent: fa, cfg: link.HubConfig{
		AgentID: "a1", Token: token, Addr: ln.Addr().String(),
		Reality: reality.ClientConfig{ServerName: "example.com", PublicKey: pub, ShortID: [8]byte{1}},
		Conns:   []link.ConnSpec{{Name: "c", Dial: d.DialContext}},
		Allow:   tgnet.New([]netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}),
		DialDC:  d.DialContext,
	}}
}

type collector struct {
	mu        sync.Mutex
	events    []Event
	usage     []UsageReport
	failUsage int
	applied   []string
}

func (c *collector) onApplied(s ctrl.State) {
	c.mu.Lock()
	c.applied = append(c.applied, s.Rev)
	c.mu.Unlock()
}
func (c *collector) appliedRevs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.applied...)
}

func (c *collector) onEvent(e Event) { c.mu.Lock(); c.events = append(c.events, e); c.mu.Unlock() }
func (c *collector) onUsage(r UsageReport) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failUsage > 0 {
		c.failUsage--
		return errors.New("db is busy")
	}
	c.usage = append(c.usage, r)
	return nil
}
func (c *collector) count(k EventKind) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e.Kind == k {
			n++
		}
	}
	return n
}
func (c *collector) usages() []UsageReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]UsageReport(nil), c.usage...)
}

func (r *rig) node(t *testing.T, mutate func(*Config)) (*Node, *collector, context.CancelFunc) {
	t.Helper()
	col := &collector{}
	cfg := Config{Link: r.cfg, Tick: 20 * time.Millisecond, StatusEvery: 60 * time.Millisecond, StatsEvery: 60 * time.Millisecond, OnEvent: col.onEvent, OnUsage: col.onUsage, OnApplied: col.onApplied}
	if mutate != nil {
		mutate(&cfg)
	}
	n := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	go n.Run(ctx)
	t.Cleanup(cancel)
	return n, col, cancel
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 600; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func state(rev string) ctrl.State { return ctrl.State{Rev: rev, Port: 443, Disguise: "example.com"} }

func TestNode(t *testing.T) {
	r := newRig(t)

	t.Run("the desired state is pushed once the link is up, and again when it changes", func(t *testing.T) {
		n, col, _ := r.node(t, nil)
		n.SetDesired(state("v1"))
		waitFor(t, func() bool { a, _ := r.agent.get(); return a == "v1" }, "v1 to reach the agent")
		waitFor(t, func() bool { return n.Snapshot().Healthy }, "a healthy snapshot")
		if col.count(EventPushed) < 1 {
			t.Error("no state_pushed event")
		}
		n.SetDesired(state("v2"))
		waitFor(t, func() bool { a, _ := r.agent.get(); return a == "v2" }, "v2 to reach the agent")
		if s := n.Snapshot(); s.DesiredRev != "v2" {
			t.Errorf("snapshot desired = %q", s.DesiredRev)
		}
	})

	t.Run("an agent that lost its state is given it again", func(t *testing.T) {
		r.agent.set(func(f *fakeAgent) { f.applied = "" })
		n, col, _ := r.node(t, nil)
		n.SetDesired(state("v3"))
		waitFor(t, func() bool { a, _ := r.agent.get(); return a == "v3" }, "the first push")
		pushes := func() int { _, c := r.agent.get(); return c }
		before := pushes()
		r.agent.set(func(f *fakeAgent) { f.applied = "old" }) // the agent restarted onto an older state
		waitFor(t, func() bool { a, _ := r.agent.get(); return a == "v3" }, "drift to be repaired")
		if pushes() <= before || col.count(EventDrift) == 0 {
			t.Errorf("no repair push (pushes %d -> %d, drift events %d)", before, pushes(), col.count(EventDrift))
		}
	})

	t.Run("a rejected state is retried with backoff, not hammered", func(t *testing.T) {
		r.agent.set(func(f *fakeAgent) { f.failWith = "bad user"; f.applies = 0; f.applied = "" })
		n, col, _ := r.node(t, nil)
		n.SetDesired(state("v4"))
		waitFor(t, func() bool { return col.count(EventPushFailed) >= 1 }, "the first failure")
		time.Sleep(1500 * time.Millisecond)
		if _, c := r.agent.get(); c > 3 {
			t.Fatalf("%d push attempts in ~2 s: the backoff is not working", c)
		}
		if s := n.Snapshot(); s.LastPushErr == "" || s.Healthy {
			t.Errorf("snapshot hides the failure: %+v", s)
		}
		r.agent.set(func(f *fakeAgent) { f.failWith = "" })
		waitFor(t, func() bool { a, _ := r.agent.get(); return a == "v4" }, "recovery once the agent accepts")
		waitFor(t, func() bool { return n.Snapshot().LastPushErr == "" }, "the error to clear")
	})

	t.Run("PushNow reports the agent's verdict", func(t *testing.T) {
		r.agent.set(func(f *fakeAgent) { f.failWith = "nope" })
		n, _, _ := r.node(t, nil)
		if _, err := n.PushNow(context.Background()); !errors.Is(err, ErrNoState) {
			t.Fatalf("before any state: %v", err)
		}
		n.SetDesired(state("v5"))
		waitFor(t, func() bool { return n.hub.Status().Up > 0 }, "the link")
		_, err := n.PushNow(context.Background())
		var ce *ctrl.Error
		if !errors.As(err, &ce) || ce.Code != "bad_state" {
			t.Fatalf("err = %v", err)
		}
		r.agent.set(func(f *fakeAgent) { f.failWith = "" })
		if ack, err := n.PushNow(context.Background()); err != nil || ack.Rev != "v5" {
			t.Fatalf("ack=%+v err=%v", ack, err)
		}
	})

	t.Run("usage becomes deltas: first sight is a baseline, restarts are handled", func(t *testing.T) {
		r.agent.set(func(f *fakeAgent) { f.users = []ctrl.UserStat{{Name: "u", OctetsUp: 1000, OctetsDown: 5000}} })
		n, col, _ := r.node(t, nil)
		_ = n
		waitFor(t, func() bool { return len(col.usages()) >= 1 }, "the first usage report")
		first := col.usages()[0]
		if len(first.Deltas) != 0 || first.Cumulative["u"].Up != 1000 {
			t.Fatalf("first report must only set the baseline: %+v", first)
		}
		r.agent.set(func(f *fakeAgent) { f.users = []ctrl.UserStat{{Name: "u", OctetsUp: 1500, OctetsDown: 5200}} })
		waitFor(t, func() bool { return sumDelta(col, "u").Up == 500 }, "a +500/+200 delta")
		if got := sumDelta(col, "u"); got.Down != 200 {
			t.Fatalf("delta = %+v", got)
		}
		// telemt restarted: counters start over, and the new value is all new traffic
		r.agent.set(func(f *fakeAgent) { f.users = []ctrl.UserStat{{Name: "u", OctetsUp: 30, OctetsDown: 40}} })
		waitFor(t, func() bool { return sumDelta(col, "u").Up == 530 }, "the post-restart traffic to be counted")
		// nothing changed: nothing is reported twice
		time.Sleep(300 * time.Millisecond)
		if got := sumDelta(col, "u"); got.Up != 530 || got.Down != 240 {
			t.Fatalf("a quiet period changed the total: %+v", got)
		}
	})

	t.Run("a failed delivery reports the same traffic again", func(t *testing.T) {
		r.agent.set(func(f *fakeAgent) { f.users = []ctrl.UserStat{{Name: "w", OctetsUp: 100, OctetsDown: 100}} })
		_, col, _ := r.node(t, func(c *Config) { c.Baseline = map[string]Usage{"w": {Up: 40, Down: 100}} })
		col.mu.Lock()
		col.failUsage = 3
		col.mu.Unlock()
		waitFor(t, func() bool { return sumDelta(col, "w").Up == 60 }, "the delta to arrive after the failed attempts")
		time.Sleep(300 * time.Millisecond)
		if got := sumDelta(col, "w"); got.Up != 60 {
			t.Fatalf("counted %d, want exactly 60 (a retry must not double count)", got.Up)
		}
	})

	t.Run("OnApplied sees every successful push", func(t *testing.T) {
		r.agent.set(func(f *fakeAgent) { f.failWith, f.applied = "", "" })
		n, col, _ := r.node(t, nil)
		n.SetDesired(state("a1"))
		waitFor(t, func() bool { return len(col.appliedRevs()) >= 1 }, "the background push to be reported")
		n.SetDesired(state("a2"))
		waitFor(t, func() bool { revs := col.appliedRevs(); return revs[len(revs)-1] == "a2" }, "a2")
		if _, err := n.PushNow(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a stored baseline stops a hub restart from double counting", func(t *testing.T) {
		r.agent.set(func(f *fakeAgent) { f.users = []ctrl.UserStat{{Name: "v", OctetsUp: 900, OctetsDown: 900}} })
		_, col, _ := r.node(t, func(c *Config) { c.Baseline = map[string]Usage{"v": {Up: 800, Down: 900}} })
		waitFor(t, func() bool { return sumDelta(col, "v").Up == 100 }, "only the growth since the stored baseline")
		if got := sumDelta(col, "v"); got.Down != 0 {
			t.Fatalf("delta = %+v", got)
		}
	})

	t.Run("telemt going down and coming back is reported, crash loops too", func(t *testing.T) {
		r.agent.set(func(f *fakeAgent) { f.running, f.crash, f.failWith = true, false, "" })
		n, col, _ := r.node(t, nil)
		n.SetDesired(ctrl.State{Rev: "v6", Port: 443, Disguise: "example.com", Users: []ctrl.User{{Name: "a"}}})
		waitFor(t, func() bool { return n.Snapshot().Agent != nil && n.Snapshot().Agent.Telemt.Running }, "a running telemt")
		r.agent.set(func(f *fakeAgent) { f.running, f.crash = false, true })
		waitFor(t, func() bool { return col.count(EventTelemtDown) >= 1 && col.count(EventCrashLoop) >= 1 }, "the down and crash-loop events")
		r.agent.set(func(f *fakeAgent) { f.running, f.crash = true, false })
		waitFor(t, func() bool { return col.count(EventTelemtUp) >= 1 }, "telemt_up")
	})

	t.Run("a proxy the panel stopped is not reported down", func(t *testing.T) {
		r.agent.set(func(f *fakeAgent) { f.running, f.crash, f.failWith = false, false, "" })
		n, col, _ := r.node(t, nil)
		n.SetDesired(ctrl.State{Rev: "s1", Port: 443, Disguise: "example.com", Users: []ctrl.User{{Name: "a"}}, Stopped: true})
		waitFor(t, func() bool { return n.Snapshot().Agent != nil }, "a status report")
		time.Sleep(300 * time.Millisecond)
		if col.count(EventTelemtDown) != 0 {
			t.Fatal("a stop from the panel was reported as telemt going down")
		}
	})

	t.Run("the cover site is re-checked and its transitions are reported", func(t *testing.T) {
		r.agent.set(func(f *fakeAgent) { f.coverOK, f.failWith, f.checked = true, "", nil })
		n, col, _ := r.node(t, func(c *Config) { c.DisguiseEvery = 80 * time.Millisecond })
		n.SetDesired(state("c1"))
		waitFor(t, func() bool { d := n.Snapshot().Disguise; return d != nil && d.OK }, "a first good check")
		if got := r.agent.checks(); got[0] != "example.com" {
			t.Fatalf("checked %v, want the node's domain", got)
		}
		// the first answer is a transition too, so an alert left firing across a restart clears
		waitFor(t, func() bool { return col.count(EventDisguiseUp) == 1 }, "disguise_up for the first good check")

		r.agent.set(func(f *fakeAgent) { f.coverOK = false })
		waitFor(t, func() bool { return col.count(EventDisguiseDown) == 1 }, "disguise_down")
		before := len(r.agent.checks())
		waitFor(t, func() bool { return len(r.agent.checks()) >= before+2 }, "more checks while it stays down")
		if c := col.count(EventDisguiseDown); c != 1 {
			t.Fatalf("%d down events for one outage", c)
		}
		if s := n.Snapshot(); s.Disguise == nil || s.Disguise.OK || s.Disguise.Err == "" || s.DisguiseAt.IsZero() {
			t.Fatalf("snapshot = %+v at %v", s.Disguise, s.DisguiseAt)
		}

		r.agent.set(func(f *fakeAgent) { f.coverOK = true })
		waitFor(t, func() bool { return col.count(EventDisguiseUp) == 2 }, "disguise_up after the outage")
		time.Sleep(300 * time.Millisecond)
		if up, down := col.count(EventDisguiseUp), col.count(EventDisguiseDown); up != 2 || down != 1 {
			t.Fatalf("steady state still emits: up=%d down=%d", up, down)
		}
	})

	t.Run("the default re-check runs once, not on every poll", func(t *testing.T) {
		r.agent.set(func(f *fakeAgent) { f.coverOK, f.checked = true, nil })
		n, _, _ := r.node(t, nil)
		n.SetDesired(state("c2"))
		waitFor(t, func() bool { return n.Snapshot().Disguise != nil }, "the first check")
		first := len(r.agent.checks())
		time.Sleep(400 * time.Millisecond) // several status polls
		if got := len(r.agent.checks()); got != first {
			t.Fatalf("%d more checks within the 6 h interval", got-first)
		}
		// a new domain does not wait for the interval
		n.SetDesired(ctrl.State{Rev: "c3", Port: 443, Disguise: "other.example.com"})
		waitFor(t, func() bool { c := r.agent.checks(); return c[len(c)-1] == "other.example.com" }, "the new domain to be checked")
	})
}

func sumDelta(c *collector, name string) Usage {
	var u Usage
	for _, r := range c.usages() {
		d := r.Deltas[name]
		u.Up += d.Up
		u.Down += d.Down
	}
	return u
}

// bareNode runs the link but not the control loop, so a test can drive each step.
func (r *rig) bareNode(t *testing.T) (*Node, *collector, context.Context) {
	t.Helper()
	col := &collector{}
	n := New(Config{Link: r.cfg, OnEvent: col.onEvent, OnApplied: col.onApplied})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go n.hub.Run(ctx)
	waitFor(t, func() bool { return n.hub.Status().Up > 0 }, "the link")
	return n, col, ctx
}

func (n *Node) pushPending() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.needPush
}

// A state set mid-push used to wait for a status poll to call it drift.
func TestStateSetDuringAPushIsStillPushed(t *testing.T) {
	r := newRig(t)
	n, col, ctx := r.bareNode(t)
	for _, tc := range []struct {
		name, first, second string
		push                func()
	}{
		{"background", "bg1", "bg2", func() { n.maybePush(ctx) }},
		{"PushNow", "now1", "now2", func() { n.PushNow(ctx) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered, gate := make(chan struct{}), make(chan struct{})
			r.agent.set(func(f *fakeAgent) {
				f.failWith = ""
				f.onApply = func(rev string) {
					if rev == tc.first {
						close(entered)
						<-gate
					}
				}
			})
			n.SetDesired(state(tc.first))
			done := make(chan struct{})
			go func() { tc.push(); close(done) }()
			<-entered
			n.SetDesired(state(tc.second))
			close(gate)
			<-done
			if !n.pushPending() {
				t.Fatalf("%s was set mid-push and is no longer pending", tc.second)
			}
			n.maybePush(ctx)
			if a, _ := r.agent.get(); a != tc.second {
				t.Fatalf("agent runs %q, want %q", a, tc.second)
			}
			if n.pushPending() {
				t.Error("still pending after the second push")
			}
		})
	}
	if c := col.count(EventDrift); c != 0 {
		t.Errorf("%d drift events without any status poll", c)
	}
}

// After a telemt restart, a direction already past its old value used to count as growth.
func TestRestartIsSeenInEitherDirection(t *testing.T) {
	n := &Node{last: map[string]Usage{"u": {Up: 1000, Down: 5000}, "v": {Up: 10, Down: 10}}}
	rep, _ := n.fold(ctrl.Stats{Users: []ctrl.UserStat{
		{Name: "u", OctetsUp: 1200, OctetsDown: 40},
		{Name: "v", OctetsUp: 15, OctetsDown: 12},
	}})
	if got := rep.Deltas["u"]; got != (Usage{Up: 1200, Down: 40}) {
		t.Errorf("restarted user: delta %+v, want all of the new counters", got)
	}
	if got := rep.Deltas["v"]; got != (Usage{Up: 5, Down: 2}) {
		t.Errorf("steady user: delta %+v", got)
	}
}

// A check in flight when the domain changed must not alert about the abandoned domain.
func TestCoverResultForAnOldDomainIsDropped(t *testing.T) {
	r := newRig(t)
	entered, gate := make(chan struct{}), make(chan struct{})
	r.agent.set(func(f *fakeAgent) {
		f.coverOK, f.checked = false, nil
		f.onCheck = func(d string) {
			if d == "old.example.com" {
				close(entered)
				<-gate
			}
		}
	})
	t.Cleanup(func() { r.agent.set(func(f *fakeAgent) { f.onCheck, f.coverOK = nil, true }) })
	n, col, ctx := r.bareNode(t)
	n.SetDesired(ctrl.State{Rev: "d1", Port: 443, Disguise: "old.example.com"})
	n.maybeCheckCover(ctx)
	<-entered
	n.SetDesired(ctrl.State{Rev: "d2", Port: 443, Disguise: "new.example.com"})
	close(gate)
	waitFor(t, func() bool { n.mu.Lock(); defer n.mu.Unlock(); return !n.covering }, "the old check to finish")
	if c := col.count(EventDisguiseDown); c != 0 || n.Snapshot().Disguise != nil {
		t.Fatalf("the old domain's result was kept: %d down events, snapshot %+v", c, n.Snapshot().Disguise)
	}
	n.maybeCheckCover(ctx)
	waitFor(t, func() bool { return col.count(EventDisguiseDown) == 1 }, "the new domain's result")
	col.mu.Lock()
	defer col.mu.Unlock()
	for _, e := range col.events {
		if e.Kind == EventDisguiseDown && e.Detail != "new.example.com" {
			t.Fatalf("down event for %q", e.Detail)
		}
	}
}

// Right after a switch the cached check is still the old site's, so the snapshot says whose it is.
func TestTheSnapshotNamesTheDomainItsCoverCheckIsFor(t *testing.T) {
	r := newRig(t)
	r.agent.set(func(f *fakeAgent) { f.coverOK = true })
	n, _, _ := r.node(t, nil)
	n.SetDesired(state("d1"))
	waitFor(t, func() bool { s := n.Snapshot(); return s.Disguise != nil && s.DisguiseDomain == "example.com" }, "the first check")

	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.agent.set(func(f *fakeAgent) {
		f.onCheck = func(d string) {
			if d == "other.example.com" {
				once.Do(func() { close(entered) })
				<-gate
			}
		}
	})
	n.SetDesired(ctrl.State{Rev: "d2", Port: 443, Disguise: "other.example.com"})
	<-entered
	if s := n.Snapshot(); s.Disguise == nil || s.DisguiseDomain != "example.com" {
		t.Fatalf("while the new site is checked: %+v for %q", s.Disguise, s.DisguiseDomain)
	}
	close(gate)
	waitFor(t, func() bool { return n.Snapshot().DisguiseDomain == "other.example.com" }, "the new site's check")
}

// What users have open rides along with the counters; an agent too old to say leaves HasLive unset.
func TestFoldCarriesWhatUsersHaveOpen(t *testing.T) {
	n := &Node{last: map[string]Usage{}}
	rep, _ := n.fold(ctrl.Stats{Live: true, Users: []ctrl.UserStat{
		{Name: "on", Connections: 3, Devices: 2},
		{Name: "idle"},
	}})
	if !rep.HasLive || rep.Live["on"] != (Live{Connections: 3, Devices: 2}) {
		t.Fatalf("live = %+v", rep)
	}
	if _, ok := rep.Live["idle"]; ok {
		t.Error("a user with nothing open is listed as live")
	}
	if old, _ := n.fold(ctrl.Stats{Users: []ctrl.UserStat{{Name: "on", Connections: 3}}}); old.HasLive || len(old.Live) != 0 {
		t.Errorf("an old agent's stats look live: %+v", old)
	}
}
