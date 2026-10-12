package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/agent/disguise"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/state"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/supervise"
	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/link"
	"github.com/nasnet-community/mtproto-remote-plugin/reality"
	"github.com/nasnet-community/mtproto-remote-plugin/tgnet"
)

type events struct {
	mu  sync.Mutex
	log []string
}

func (e *events) add(s string) { e.mu.Lock(); e.log = append(e.log, s); e.mu.Unlock() }
func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.log...)
}
func (e *events) index(s string) int {
	for i, x := range e.all() {
		if x == s {
			return i
		}
	}
	return -1
}
func (e *events) count(s string) int {
	n := 0
	for _, x := range e.all() {
		if x == s {
			n++
		}
	}
	return n
}

type fakeProc struct {
	ev      *events
	mu      sync.Mutex
	enabled bool
	st      supervise.Status
}

func (p *fakeProc) Run(ctx context.Context) { <-ctx.Done() }
func (p *fakeProc) Restart()                { p.ev.add("telemt restart") }
func (p *fakeProc) SetEnabled(on bool) {
	p.mu.Lock()
	p.enabled, p.st.Running = on, on
	p.mu.Unlock()
	if on {
		p.ev.add("telemt enable")
	} else {
		p.ev.add("telemt disable")
	}
}
func (p *fakeProc) Status() supervise.Status { p.mu.Lock(); defer p.mu.Unlock(); return p.st }
func (p *fakeProc) Stop() error {
	p.mu.Lock()
	p.enabled, p.st.Running = false, false
	p.mu.Unlock()
	p.ev.add("telemt stop")
	return nil
}

type rig struct {
	d      *Daemon
	ev     *events
	proc   *fakeProc
	dir    string
	exited chan struct{}

	mu        sync.Mutex
	scripts   []string
	live      atomic.Bool // what nft would say about the tables
	rulesFail atomic.Bool // nft refuses the next script
}

func (r *rig) script(i int) string { r.mu.Lock(); defer r.mu.Unlock(); return r.scripts[i] }
func (r *rig) applied() int        { r.mu.Lock(); defer r.mu.Unlock(); return len(r.scripts) }

// realProc swaps the fake for the real watchdog (as the login user: no uid switch).
func realProc(c *Config) {
	c.NewProc = func(s supervise.Spec) Proc {
		s.UID, s.GID, s.MinBackoff, s.StopGrace = 0, 0, 10*time.Millisecond, 300*time.Millisecond
		return supervise.New(s)
	}
}

// run starts the whole daemon until the test ends.
func (r *rig) run(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.d.cfg.Listen = ln
	r.d.st.CapturePort = freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func good(rev string, users ...ctrl.User) ctrl.State {
	return ctrl.State{
		Rev: rev, Port: 443, Disguise: "mirror.example.ir",
		Users:  users,
		DCNets: []string{"203.0.113.0/24", "2001:db8::/32"},
	}
}

func user(name string) ctrl.User {
	b := make([]byte, 16)
	rand.Read(b)
	return ctrl.User{Name: name, Secret: strings.Repeat("ab", 16), Enabled: true}
}

func newRig(t *testing.T, tweaks ...func(*Config)) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{ev: &events{}, dir: dir, exited: make(chan struct{}, 4)}
	r.live.Store(true)
	r.proc = &fakeProc{ev: r.ev}
	st := &state.File{AgentID: "a1", Disguise: "mirror.example.ir", CapturePort: 9443, HubPort: 8443, TelemtUID: 998, TelemtGID: 998,
		RealityPriv: bytes.Repeat([]byte{1}, 32), ShortIDs: [][]byte{{1, 2}}, Tokens: [][]byte{bytes.Repeat([]byte{7}, 32)}}
	bin := filepath.Join(dir, "bin") // not WorkDir/telemt: that is telemt's config dir
	os.MkdirAll(bin, 0o755)
	cfg := Config{
		StatePath: filepath.Join(dir, "state.json"), WorkDir: dir, TelemtBin: filepath.Join(bin, "telemt"),
		AgentBin: filepath.Join(bin, "agent"), TrialFile: filepath.Join(dir, "trial.json"), Version: "t1", Build: "b1",
		NewProc: func(supervise.Spec) Proc { return r.proc },
		ApplyRules: func(_ context.Context, s string) error {
			if r.rulesFail.Load() {
				r.ev.add("rules failed")
				return errors.New("nft: no such file or directory")
			}
			r.mu.Lock()
			r.scripts = append(r.scripts, s)
			r.mu.Unlock()
			r.live.Store(true)
			r.ev.add("rules")
			return nil
		},
		RemoveRules: func(context.Context) error { r.ev.add("rules removed"); return nil },
		RulesLive:   func(context.Context, bool, bool) bool { return r.live.Load() },
		Teardown:    func(context.Context) error { r.ev.add("teardown"); return nil },
		Exit:        func() { r.ev.add("exit"); r.exited <- struct{}{} },
		Scrape: func(context.Context, int) (string, error) {
			return "telemt_user_octets_from_client{user=\"u1\"} 10\ntelemt_user_octets_to_client{user=\"u1\"} 20\n", nil
		},
		Check:         disguise.Checker{Lookup: func(context.Context, string) ([]string, error) { return nil, errors.New("no resolver in tests") }},
		TelemtVersion: func(context.Context, string) string { return "3.4.25" },
	}
	for _, tw := range tweaks {
		tw(&cfg)
	}
	d, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	r.d = d
	return r
}

func (r *rig) apply(t *testing.T, s ctrl.State) ctrl.StateAck {
	t.Helper()
	ack, err := r.d.ApplyState(context.Background(), s, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return ack
}

func TestApplyStateOrdersRulesBeforeTelemtAndPersists(t *testing.T) {
	r := newRig(t)
	ack := r.apply(t, good("r1", user("u1")))
	if ack.Rev != "r1" || ack.Restarted {
		t.Fatalf("ack = %+v", ack)
	}
	if r.ev.index("rules") < 0 || r.ev.index("rules") > r.ev.index("telemt enable") {
		t.Fatalf("telemt must start behind the redirect: %v", r.ev.all())
	}
	cfg, err := os.ReadFile(filepath.Join(r.dir, "telemt", "config.toml"))
	if err != nil || !strings.Contains(string(cfg), `"u1" = "`+strings.Repeat("ab", 16)+`"`) || !strings.Contains(string(cfg), `tls_domain = "mirror.example.ir"`) {
		t.Fatalf("config: %v\n%s", err, cfg)
	}
	if !strings.Contains(r.script(0), "meta skuid 998") || !strings.Contains(r.script(0), "redirect to :9443") {
		t.Fatalf("rules:\n%s", r.script(0))
	}
	saved, err := state.Load(filepath.Join(r.dir, "state.json"))
	if err != nil || saved.Desired == nil || saved.Desired.Rev != "r1" {
		t.Fatalf("saved = %+v err=%v", saved, err)
	}
}

func TestUserChangesReloadLiveWithoutRestartOrNewRules(t *testing.T) {
	r := newRig(t)
	r.apply(t, good("r1", user("u1")))
	ack := r.apply(t, good("r2", user("u1"), user("u2")))
	if ack.Restarted || r.ev.count("telemt restart") != 0 {
		t.Fatalf("a user change restarted telemt: %v", r.ev.all())
	}
	if r.applied() != 1 {
		t.Fatalf("rules re-applied for an unchanged prefix list (%d times)", r.applied())
	}
	cfg, _ := os.ReadFile(filepath.Join(r.dir, "telemt", "config.toml"))
	if !strings.Contains(string(cfg), `"u2"`) {
		t.Fatal("new user not in the config")
	}
}

func TestCoreChangeRestartsTelemt(t *testing.T) {
	r := newRig(t)
	r.apply(t, good("r1", user("u1")))
	s := good("r2", user("u1"))
	s.Telemt = map[string]any{"log_level": "debug"}
	if ack := r.apply(t, s); !ack.Restarted || r.ev.count("telemt restart") != 1 {
		t.Fatalf("ack=%+v events=%v", ack, r.ev.all())
	}
	s = good("r3", user("u1"))
	s.Disguise = "other.example.ir"
	if ack := r.apply(t, s); !ack.Restarted {
		t.Fatal("a new disguise domain changes tls_domain: telemt must restart")
	}
	if r.d.st.Disguise != "other.example.ir" {
		t.Errorf("state disguise = %q", r.d.st.Disguise)
	}
}

func TestPrefixChangeReappliesRules(t *testing.T) {
	r := newRig(t)
	r.apply(t, good("r1", user("u1")))
	s := good("r2", user("u1"))
	s.DCNets = []string{"203.0.113.0/24", "198.51.100.0/24"}
	r.apply(t, s)
	if r.applied() != 2 || !strings.Contains(r.script(1), "198.51.100.0/24") {
		t.Fatalf("scripts: %d", r.applied())
	}
	// the same set in another order is not a change
	s.DCNets = []string{"198.51.100.0/24", "203.0.113.0/24"}
	r.apply(t, s)
	if r.applied() != 2 {
		t.Fatal("reordered prefixes re-applied the rules")
	}
}

func TestNoUsersStopsTelemtButKeepsTheRules(t *testing.T) {
	r := newRig(t)
	r.apply(t, good("r1", user("u1")))
	r.apply(t, good("r2"))
	if r.ev.index("telemt disable") < 0 {
		t.Fatalf("events: %v", r.ev.all())
	}
	if r.ev.count("rules removed") != 0 {
		t.Fatal("emptying the user list must not open the redirect")
	}
}

// The panel's Stop keeps telemt off with users still on the list; Start brings it back.
func TestAStoppedProxyStaysOffUntilStarted(t *testing.T) {
	r := newRig(t)
	r.apply(t, good("r1", user("u1")))
	stopped := good("r2", user("u1"))
	stopped.Stopped = true
	r.apply(t, stopped)
	if r.proc.Status().Running {
		t.Fatal("telemt still runs after the panel stopped it")
	}
	b, _ := call(t, r, ctrl.MStatus, nil)
	var st ctrl.Status
	json.Unmarshal(b, &st)
	if st.Telemt.Held != ctrl.HeldStopped {
		t.Fatalf("held = %q", st.Telemt.Held)
	}
	if r.ev.count("rules removed") != 0 {
		t.Fatal("stopping the proxy must not open the redirect")
	}
	r.apply(t, good("r3", user("u1")))
	b, _ = call(t, r, ctrl.MStatus, nil)
	st = ctrl.Status{} // held is left out when empty
	json.Unmarshal(b, &st)
	if !r.proc.Status().Running || st.Telemt.Held != "" {
		t.Fatalf("after start: running=%v held=%q", r.proc.Status().Running, st.Telemt.Held)
	}
}

func TestInvalidStatesAreRejectedBeforeAnythingChanges(t *testing.T) {
	r := newRig(t)
	bad := map[string]func(*ctrl.State){
		"port":          func(s *ctrl.State) { s.Port = 0 },
		"disguise":      func(s *ctrl.State) { s.Disguise = "not a domain" },
		"short secret":  func(s *ctrl.State) { s.Users[0].Secret = "abc" },
		"upper secret":  func(s *ctrl.State) { s.Users[0].Secret = strings.Repeat("AB", 16) },
		"dup user":      func(s *ctrl.State) { s.Users = append(s.Users, s.Users[0]) },
		"empty name":    func(s *ctrl.State) { s.Users[0].Name = "" },
		"no prefixes":   func(s *ctrl.State) { s.DCNets = nil },
		"everything":    func(s *ctrl.State) { s.DCNets = []string{"0.0.0.0/0"} },
		"too broad v6":  func(s *ctrl.State) { s.DCNets = []string{"2000::/3"} },
		"junk prefix":   func(s *ctrl.State) { s.DCNets = []string{"banana"} },
		"middle proxy":  func(s *ctrl.State) { s.Telemt = map[string]any{"middle_proxy": true} },
		"ad tag":        func(s *ctrl.State) { s.Telemt = map[string]any{"adtag_default": "00"} },
		"control chars": func(s *ctrl.State) { s.Users[0].Name = "a\nb" },
	}
	for name, mutate := range bad {
		s := good("r", user("u1"))
		mutate(&s)
		_, err := r.d.ApplyState(context.Background(), s, true)
		var ce *ctrl.Error
		if !errors.As(err, &ce) || ce.Code != "bad_state" {
			t.Errorf("%s: err = %v, want a bad_state error", name, err)
		}
	}
	if r.applied() != 0 || r.ev.count("telemt enable") != 0 {
		t.Fatalf("a rejected state changed the box: %v", r.ev.all())
	}
	if _, err := os.Stat(filepath.Join(r.dir, "telemt", "config.toml")); err == nil {
		t.Fatal("a rejected state wrote a config")
	}
}

func TestReplayAtBootDoesNotRewriteTheStateFile(t *testing.T) {
	r := newRig(t)
	s := good("r1", user("u1"))
	if _, err := r.d.ApplyState(context.Background(), s, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "state.json")); err == nil {
		t.Fatal("the boot replay wrote the state file")
	}
}

func call(t *testing.T, r *rig, method string, req any) (json.RawMessage, error) {
	t.Helper()
	h := r.d.Handlers()[method]
	if h == nil {
		t.Fatalf("no handler for %s", method)
	}
	var raw json.RawMessage
	if req != nil {
		raw, _ = json.Marshal(req)
	}
	out, err := h(context.Background(), raw, strings.NewReader(""))
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(out)
	return b, nil
}

func TestStatusStatsLogsAndRestartHandlers(t *testing.T) {
	r := newRig(t)
	r.apply(t, good("rev-9", user("u1")))
	r.d.ring.Write([]byte("client 198.51.100.7 connected\n"))

	b, _ := call(t, r, ctrl.MStatus, nil)
	var st ctrl.Status
	json.Unmarshal(b, &st)
	if st.AgentVersion != "t1" || st.AgentBuild != "b1" || st.StateRev != "rev-9" || !st.Telemt.Running || !st.Capture.Rules || st.Disguise.Domain != "mirror.example.ir" {
		t.Fatalf("status = %+v", st)
	}
	if st.Goroutines <= 0 || !strings.Contains(string(b), `"goroutines":`) {
		t.Fatalf("status has no goroutine count: %s", b)
	}
	b, _ = call(t, r, ctrl.MStats, nil)
	var stats ctrl.Stats
	json.Unmarshal(b, &stats)
	if len(stats.Users) != 1 || stats.Users[0].OctetsUp != 10 || stats.Users[0].OctetsDown != 20 {
		t.Fatalf("stats = %+v", stats)
	}
	b, _ = call(t, r, ctrl.MLogsTail, ctrl.LogsRequest{Lines: 5})
	var logs ctrl.LogsResponse
	json.Unmarshal(b, &logs)
	if len(logs.Lines) != 1 || logs.Lines[0] != "client [ip] connected" {
		t.Fatalf("logs = %+v (an address leaked or the line is missing)", logs)
	}
	call(t, r, ctrl.MServiceRestart, nil)
	if r.ev.count("telemt restart") != 1 {
		t.Fatal("service.restart did not restart telemt")
	}
}

// The pushed settings may move telemt's metrics port; stats must follow it.
func TestStatsScrapeTheConfiguredMetricsPort(t *testing.T) {
	r := newRig(t)
	var got int
	r.d.cfg.Scrape = func(_ context.Context, port int) (string, error) { got = port; return "", nil }
	s := good("r1", user("u1"))
	s.Telemt = map[string]any{"metrics_port": 14999}
	r.apply(t, s)
	if _, err := r.d.Stats(context.Background()); err != nil || got != 14999 {
		t.Fatalf("scraped port %d, err %v", got, err)
	}
}

func TestKeyRotationKeepsTheOldTokenUntilFinish(t *testing.T) {
	r := newRig(t)
	r.d.lk.Store(link.NewAgent(link.AgentConfig{Tokens: r.d.st.Tokens}))
	old := r.d.st.Tokens[0]
	newTok := bytes.Repeat([]byte{9}, 32)
	_, err := call(t, r, ctrl.MKeysRotate, ctrl.KeysRotate{Token: newTok, PrivateKey: bytes.Repeat([]byte{3}, 32), ShortIDs: [][]byte{{5, 5}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.d.st.Tokens) != 2 || !bytes.Equal(r.d.st.Tokens[0], newTok) || !bytes.Equal(r.d.st.Tokens[1], old) {
		t.Fatalf("tokens = %x", r.d.st.Tokens)
	}
	if _, err := call(t, r, ctrl.MKeysRotate, ctrl.KeysRotate{Finish: true}); err != nil {
		t.Fatal(err)
	}
	if len(r.d.st.Tokens) != 1 || !bytes.Equal(r.d.st.Tokens[0], newTok) {
		t.Fatalf("after finish: %x", r.d.st.Tokens)
	}
	saved, _ := state.Load(filepath.Join(r.dir, "state.json"))
	if len(saved.Tokens) != 1 {
		t.Fatalf("saved tokens: %d", len(saved.Tokens))
	}
	if _, err := call(t, r, ctrl.MKeysRotate, ctrl.KeysRotate{Token: []byte{1}}); err == nil {
		t.Fatal("a rotate with a short token and no key was accepted")
	}
}

func TestAgentUpdateStagesSwapsAndExits(t *testing.T) {
	r := newRig(t)
	os.WriteFile(r.d.cfg.AgentBin, []byte("#!/bin/sh\necho v1\n"), 0o755)
	body := []byte("#!/bin/sh\nexit 0\n")
	h := sha(body)
	raw, _ := json.Marshal(ctrl.UpdateRequest{Kind: "agent", Size: int64(len(body)), SHA256: h, Ver: "2.0.0"})
	out, err := r.d.Handlers()[ctrl.MUpdate](context.Background(), raw, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := out.(map[string]any); m["restarting"] != true {
		t.Fatalf("out = %v", out)
	}
	select {
	case <-r.exited:
	case <-time.After(3 * time.Second):
		t.Fatal("the agent never exited to pick up the new binary")
	}
	if b, _ := os.ReadFile(r.d.cfg.AgentBin); !bytes.Equal(b, body) {
		t.Fatal("new binary not in place")
	}
	if _, err := os.Stat(r.d.cfg.TrialFile); err != nil {
		t.Fatal("no trial opened")
	}
	// commit closes it
	if _, err := call(t, r, ctrl.MUpdateCommit, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.d.cfg.TrialFile); err == nil {
		t.Fatal("trial survived the commit")
	}
}

// telemt answers on its metrics port only once it has finished starting, so the
// update must wait for the new binary's build_info rather than for a live pid.
func TestTelemtUpdateWaitsForTheNewBinaryToAnswer(t *testing.T) {
	r := newRig(t)
	r.proc.st.Running = true
	os.WriteFile(r.d.cfg.TelemtBin, []byte("#!/bin/sh\necho telemt 1.0.0\n"), 0o755)
	body := []byte("#!/bin/sh\necho telemt 2.0.0\n")
	var calls atomic.Int32
	r.d.cfg.Scrape = func(context.Context, int) (string, error) {
		switch n := calls.Add(1); {
		case n <= 2:
			return "", errors.New("connection refused") // still starting
		case n <= 4:
			return `telemt_build_info{version="1.0.0"} 1`, nil // the old one, not yet gone
		}
		return `telemt_build_info{version="2.0.0"} 1`, nil
	}
	raw, _ := json.Marshal(ctrl.UpdateRequest{Kind: "telemt", Size: int64(len(body)), SHA256: sha(body), Ver: "v2.0.0"})
	out, err := r.d.Handlers()[ctrl.MUpdate](context.Background(), raw, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := out.(map[string]any); m["restarted"] != true {
		t.Fatalf("out = %v", out)
	}
	if calls.Load() < 5 {
		t.Fatalf("passed after %d checks: it did not wait for the new binary", calls.Load())
	}
	if b, _ := os.ReadFile(r.d.cfg.TelemtBin); !bytes.Equal(b, body) {
		t.Fatal("the new telemt is not in place")
	}
}

func TestTelemtUpdateThatNeverAnswersIsRolledBack(t *testing.T) {
	r := newRig(t)
	r.d.cfg.HealthWindow = 400 * time.Millisecond
	r.proc.st.Running = true
	old := []byte("#!/bin/sh\necho telemt 1.0.0\n")
	os.WriteFile(r.d.cfg.TelemtBin, old, 0o755)
	body := []byte("#!/bin/sh\necho telemt 2.0.0\n")
	r.d.cfg.Scrape = func(context.Context, int) (string, error) { return "", errors.New("connection refused") }
	raw, _ := json.Marshal(ctrl.UpdateRequest{Kind: "telemt", Size: int64(len(body)), SHA256: sha(body), Ver: "2.0.0"})
	_, err := r.d.Handlers()[ctrl.MUpdate](context.Background(), raw, bytes.NewReader(body))
	var ce *ctrl.Error
	if !errors.As(err, &ce) || ce.Code != "update_failed" {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(r.d.cfg.TelemtBin); !bytes.Equal(b, old) {
		t.Fatal("the previous telemt was not put back")
	}
	if n := r.ev.count("telemt restart"); n != 2 {
		t.Fatalf("telemt restarted %d times, want 2 (new, then the restored one)", n)
	}
}

func TestBadUpdateIsRejectedAndLeavesNothing(t *testing.T) {
	r := newRig(t)
	os.WriteFile(r.d.cfg.AgentBin, []byte("#!/bin/sh\necho v1\n"), 0o755)
	body := []byte("#!/bin/sh\nexit 1\n") // fails its smoke test
	raw, _ := json.Marshal(ctrl.UpdateRequest{Kind: "agent", Size: int64(len(body)), SHA256: sha(body)})
	_, err := r.d.Handlers()[ctrl.MUpdate](context.Background(), raw, bytes.NewReader(body))
	var ce *ctrl.Error
	if !errors.As(err, &ce) || ce.Code != "update_rejected" {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(r.d.cfg.AgentBin); !strings.Contains(string(b), "v1") {
		t.Fatal("the running binary was replaced by a failing one")
	}
	raw, _ = json.Marshal(ctrl.UpdateRequest{Kind: "nope", Size: 1, SHA256: strings.Repeat("0", 64)})
	if _, err := r.d.Handlers()[ctrl.MUpdate](context.Background(), raw, strings.NewReader("x")); err == nil {
		t.Fatal("an unknown kind was accepted")
	}
}

func TestUninstallRemovesRulesAndExits(t *testing.T) {
	r := newRig(t)
	r.apply(t, good("r1", user("u1")))
	call(t, r, ctrl.MUninstall, nil)
	select {
	case <-r.exited:
	case <-time.After(3 * time.Second):
		t.Fatal("uninstall never exited")
	}
	stop, rules, gone := r.ev.index("telemt stop"), r.ev.index("rules removed"), r.ev.index("teardown")
	if stop < 0 || rules < stop || gone < rules {
		t.Fatalf("want telemt stopped, then the rules, then the rest: %v", r.ev.all())
	}
	if _, err := r.d.ApplyState(context.Background(), good("r2", user("u1")), true); err == nil {
		t.Fatal("a state was applied while the agent was being removed")
	}
}

// The rules and the account go only once telemt has really exited: a SIGTERM
// alone left it running, uncaptured, for its whole grace.
func TestUninstallWaitsForTelemtToExit(t *testing.T) {
	var pid atomic.Int64
	alive := func() bool { p := int(pid.Load()); return p > 0 && syscall.Kill(p, 0) == nil }
	var leaked atomic.Bool
	r := newRig(t, realProc, func(c *Config) {
		c.RemoveRules = func(context.Context) error { leaked.Store(alive()); return nil }
	})
	// the fake says when its trap is set; a SIGTERM before that would kill it at once
	ready := filepath.Join(r.dir, "trap-set")
	os.WriteFile(r.d.cfg.TelemtBin, []byte("#!/bin/sh\ntrap '' TERM\n: > '"+ready+"'\nwhile :; do sleep 0.05; done\n"), 0o755)
	r.run(t)
	r.apply(t, good("r1", user("u1")))
	waitUntil(t, "telemt to run", func() bool {
		st := r.d.proc.Status()
		pid.Store(int64(st.PID))
		_, err := os.Stat(ready)
		return st.Running && err == nil
	})
	call(t, r, ctrl.MUninstall, nil)
	select {
	case <-r.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("uninstall never exited")
	}
	if leaked.Load() {
		t.Fatal("the capture rules were removed while telemt was still running")
	}
	if alive() {
		t.Fatal("telemt outlived the uninstall")
	}
}

// A fresh agent, or one whose saved state does not apply, keeps telemt off and
// says why; the hub's push is what lets it run.
func TestTelemtStaysOffUntilAStateIsLive(t *testing.T) {
	r := newRig(t, realProc)
	marker := filepath.Join(r.dir, "telemt-ran")
	os.WriteFile(r.d.cfg.TelemtBin, []byte("#!/bin/sh\ntouch "+marker+"\nexec sleep 600\n"), 0o755)
	s := good("r1", user("u1"))
	r.d.st.Desired = &s
	r.rulesFail.Store(true)
	r.run(t)
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("telemt started although its capture rules never went in")
	}
	st := r.d.Status(context.Background())
	if st.Telemt.Running || !strings.Contains(st.Telemt.Held, "saved state") || st.StateRev != "" {
		t.Fatalf("status after a failed replay: running %v held %q rev %q", st.Telemt.Running, st.Telemt.Held, st.StateRev)
	}
	r.rulesFail.Store(false)
	if _, err := call(t, r, ctrl.MStateApply, s); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "telemt to start after the push", func() bool { _, err := os.Stat(marker); return err == nil })
	if st := r.d.Status(context.Background()); st.Telemt.Held != "" || st.StateRev != "r1" {
		t.Fatalf("after the push: held %q rev %q", st.Telemt.Held, st.StateRev)
	}
}

func TestAFreshAgentWaitsForItsFirstState(t *testing.T) {
	r := newRig(t, realProc)
	marker := filepath.Join(r.dir, "telemt-ran")
	os.WriteFile(r.d.cfg.TelemtBin, []byte("#!/bin/sh\ntouch "+marker+"\nexec sleep 600\n"), 0o755)
	r.run(t)
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("telemt started with no state at all")
	}
	if st := r.d.Status(context.Background()); st.Telemt.Running || st.Telemt.Held == "" || st.Telemt.CrashLoop {
		t.Fatalf("status = %+v", st.Telemt)
	}
}

// `systemctl restart nftables` flushes everything: telemt must stop before it
// can dial out directly, and run again once the rules are back.
func TestMissingRulesStopTelemtUntilTheyAreBack(t *testing.T) {
	r := newRig(t, func(c *Config) { c.RuleCheckEvery = 20 * time.Millisecond })
	r.run(t)
	r.apply(t, good("r1", user("u1")))
	n := len(r.ev.all())
	r.rulesFail.Store(true)
	r.live.Store(false)
	waitUntil(t, "telemt to be held", func() bool { return strings.Contains(r.d.Status(context.Background()).Telemt.Held, "capture rules") })
	if r.proc.Status().Running {
		t.Fatal("telemt kept running without its rules")
	}
	r.rulesFail.Store(false)
	// telemt is re-enabled just after the counter moves, so wait for the whole end state
	waitUntil(t, "the rules to come back", func() bool {
		st := r.d.Status(context.Background())
		return st.Capture.Restored == 1 && st.Telemt.Held == "" && st.Capture.Rules && r.proc.Status().Running
	})
	ev := r.ev.all()[n:]
	idx := func(s string) int {
		for i, x := range ev {
			if x == s {
				return i
			}
		}
		return -1
	}
	if stop, rules, on := idx("telemt stop"), idx("rules"), idx("telemt enable"); stop < 0 || rules < stop || on < rules {
		t.Fatalf("want stop, rules, enable: %v", ev)
	}
	if st := r.d.Status(context.Background()); st.Telemt.Held != "" || !st.Capture.Rules || !r.proc.Status().Running {
		t.Fatalf("after the repair: %+v %+v", st.Telemt, st.Capture)
	}
}

// Status, state.get, the cover check and stats all run while a push changes
// what they read.
func TestHandlersDoNotRaceWithApplyState(t *testing.T) {
	r := newRig(t)
	r.d.lk.Store(link.NewAgent(link.AgentConfig{Tokens: r.d.st.Tokens}))
	r.d.op = r.d.lk.Load()
	r.apply(t, good("r0", user("u1")))
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, m := range []string{ctrl.MStatus, ctrl.MStateGet, ctrl.MDisguiseCheck, ctrl.MStats} {
		h := r.d.Handlers()[m]
		body, _ := json.Marshal(ctrl.DisguiseCheck{Domain: "other.example.ir"})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if out, err := h(ctx, body, strings.NewReader("")); err == nil {
					json.Marshal(out)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.d.telemtAnswers("1.0.0")(ctx)
			r.d.permitted(netip.MustParseAddrPort("203.0.113.5:443"))
		}
	}()
	for i := 1; i < 40; i++ {
		s := good(fmt.Sprintf("r%d", i), user("u1"))
		if i%2 == 1 {
			s.Disguise, s.Telemt, s.DCNets = "other.example.ir", map[string]any{"metrics_port": 15000 + i}, []string{"198.51.100.0/24"}
		}
		if _, err := r.d.ApplyState(ctx, s, true); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

// The hub's per-agent stream cap is the agent's too, from boot and on change.
func TestPushedMaxStreamsReachTheLink(t *testing.T) {
	r := newRig(t)
	s := good("r1", user("u1"))
	s.MaxStreams = 3000
	r.d.st.Desired = &s
	r.run(t)
	waitUntil(t, "the link", func() bool { return r.d.lk.Load() != nil })
	waitUntil(t, "the saved cap", func() bool { return r.d.lk.Load().MaxStreams() == 3000 })
	s.Rev, s.MaxStreams = "r2", 7000
	if _, err := call(t, r, ctrl.MStateApply, s); err != nil {
		t.Fatal(err)
	}
	if got := r.d.lk.Load().MaxStreams(); got != 7000 {
		t.Fatalf("cap after the push = %d", got)
	}
	s.Rev, s.MaxStreams = "r3", 30000
	if _, err := call(t, r, ctrl.MStateApply, s); err == nil {
		t.Fatal("a cap past 20000 was accepted")
	}
}

// The version reported is the binary's own, not a constant.
func TestTelemtVersionComesFromTheBinary(t *testing.T) {
	r := newRig(t, func(c *Config) { c.TelemtVersion = nil })
	os.WriteFile(r.d.cfg.TelemtBin, []byte("#!/bin/sh\n[ \"$1\" = --version ] && echo 'telemt 3.9.1'\n"), 0o755)
	r.run(t)
	waitUntil(t, "the version", func() bool { return r.d.Status(context.Background()).TelemtVersion == "3.9.1" })
}

func TestEachAgentProcessHasItsOwnBootID(t *testing.T) {
	a, b := newRig(t), newRig(t)
	ia, ib := a.d.Status(context.Background()).BootID, b.d.Status(context.Background()).BootID
	if ia == "" || ia == ib || ia != a.d.Status(context.Background()).BootID {
		t.Fatalf("boot ids %q %q", ia, ib)
	}
}

// A bad rotation is refused before anything is saved: a short id the REALITY
// config rejects would otherwise stop every later start.
func TestRotateValidatesBeforeSaving(t *testing.T) {
	r := newRig(t)
	r.d.lk.Store(link.NewAgent(link.AgentConfig{Tokens: r.d.st.Tokens}))
	_, err := call(t, r, ctrl.MKeysRotate, ctrl.KeysRotate{Token: bytes.Repeat([]byte{9}, 32), PrivateKey: bytes.Repeat([]byte{3}, 32), ShortIDs: [][]byte{bytes.Repeat([]byte{1}, 9)}})
	var ce *ctrl.Error
	if !errors.As(err, &ce) || ce.Code != "bad_request" {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(r.d.cfg.StatePath); err == nil {
		t.Fatal("the refused rotation was saved")
	}
	if len(r.d.st.Tokens) != 1 || len(r.d.st.ShortIDs[0]) != 2 {
		t.Fatalf("memory changed: %x %x", r.d.st.Tokens, r.d.st.ShortIDs)
	}
}

// The hub repeats a rotation whose reply it lost; the repeat changes nothing.
func TestARepeatedRotationIsHarmless(t *testing.T) {
	r := newRig(t)
	r.d.lk.Store(link.NewAgent(link.AgentConfig{Tokens: r.d.st.Tokens}))
	old := r.d.st.Tokens[0]
	req := ctrl.KeysRotate{Token: bytes.Repeat([]byte{9}, 32), PrivateKey: bytes.Repeat([]byte{3}, 32), ShortIDs: [][]byte{{5, 5}}}
	for i := 0; i < 2; i++ {
		if _, err := call(t, r, ctrl.MKeysRotate, req); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.d.st.Tokens) != 2 || !bytes.Equal(r.d.st.Tokens[0], req.Token) || !bytes.Equal(r.d.st.Tokens[1], old) {
		t.Fatalf("tokens after a repeat = %x", r.d.st.Tokens)
	}
}

// telemt reads its config as its own uid and works in a directory of its own;
// the agent lays both out on every push, which also heals older installs.
func TestTelemtGetsItsOwnWorkingDir(t *testing.T) {
	var spec supervise.Spec
	r := newRig(t, func(c *Config) {
		c.NewProc = func(s supervise.Spec) Proc { spec = s; return &fakeProc{ev: &events{}} }
	})
	r.proc = r.d.proc.(*fakeProc)
	os.Chmod(r.dir, 0o700) // what the first installer made of it
	r.apply(t, good("r1", user("u1")))
	run := filepath.Join(r.dir, "telemt", "run")
	if spec.Dir != run {
		t.Fatalf("telemt runs in %q, want %q", spec.Dir, run)
	}
	for path, want := range map[string]os.FileMode{r.dir: 0o711, filepath.Join(r.dir, "telemt"): 0o750, run: 0o700, filepath.Join(r.dir, "telemt", "config.toml"): 0o640} {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", path, err, st.Mode().Perm(), want)
		}
	}
}

// Two updates at once, or one while the last agent update is still on trial,
// would stage over each other or commit the wrong process.
func TestUpdatesAreRefusedWhileAnotherIsOpen(t *testing.T) {
	r := newRig(t)
	os.WriteFile(r.d.cfg.AgentBin, []byte("#!/bin/sh\necho v1\n"), 0o755)
	os.WriteFile(r.d.cfg.TrialFile, []byte(`{"version":"2.0.0"}`), 0o600)
	body := []byte("#!/bin/sh\nexit 0\n")
	raw, _ := json.Marshal(ctrl.UpdateRequest{Kind: "agent", Size: int64(len(body)), SHA256: sha(body), Ver: "3.0.0"})
	_, err := r.d.Handlers()[ctrl.MUpdate](context.Background(), raw, bytes.NewReader(body))
	var ce *ctrl.Error
	if !errors.As(err, &ce) || ce.Code != "update_busy" {
		t.Fatalf("an update during a trial: %v", err)
	}
	if b, _ := os.ReadFile(r.d.cfg.AgentBin); !strings.Contains(string(b), "v1") {
		t.Fatal("the binary on trial was replaced")
	}
	os.Remove(r.d.cfg.TrialFile)

	// a telemt update holds the slot while it waits for the new binary to answer
	r.proc.st.Running = true
	r.apply(t, good("r1", user("u1")))
	release := make(chan struct{})
	r.d.cfg.Scrape = func(ctx context.Context, _ int) (string, error) {
		select {
		case <-release:
			return `telemt_build_info{version="2.0.0"} 1`, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	os.WriteFile(r.d.cfg.TelemtBin, []byte("#!/bin/sh\necho telemt 1.0.0\n"), 0o755)
	tb := []byte("#!/bin/sh\necho telemt 2.0.0\n")
	traw, _ := json.Marshal(ctrl.UpdateRequest{Kind: "telemt", Size: int64(len(tb)), SHA256: sha(tb), Ver: "2.0.0"})
	first := make(chan error, 1)
	go func() {
		_, err := r.d.Handlers()[ctrl.MUpdate](context.Background(), traw, bytes.NewReader(tb))
		first <- err
	}()
	waitUntil(t, "the first update to be waiting", func() bool { return r.ev.count("telemt restart") > 0 })
	_, err = r.d.Handlers()[ctrl.MUpdate](context.Background(), traw, bytes.NewReader(tb))
	if !errors.As(err, &ce) || ce.Code != "update_busy" {
		t.Fatalf("a second update at once: %v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("the first update: %v", err)
	}
}

func sha(b []byte) string {
	h := sha256Sum(b)
	return h
}

// --- the whole agent, end to end ---

// TestEndToEnd runs the daemon with real REALITY, mux and capture handling, a
// real hub link, and a fake datacentre. Only nft, the telemt process and the
// kernel's original-destination lookup are faked.
func TestEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dest := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dest.EnableHTTP2 = true
	dest.StartTLS()
	defer dest.Close()
	dc, _ := net.Listen("tcp", "127.0.0.1:0")
	defer dc.Close()
	go func() {
		for {
			c, err := dc.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	dcAP := netip.MustParseAddrPort(dc.Addr().String())

	priv, pub, _ := reality.GenerateKey()
	token := bytes.Repeat([]byte{4}, 32)
	hubLn, _ := net.Listen("tcp", "127.0.0.1:0")
	capPort := freePort(t)

	r := newRig(t)
	r.d.cfg.Listen = hubLn
	r.d.cfg.RealityDest = func(string) string { return dest.Listener.Addr().String() }
	r.d.cfg.DCPort = dcAP.Port()
	var origDst atomic.Pointer[netip.AddrPort] // the test flips this while the capture loop reads it
	origDst.Store(&dcAP)
	r.d.cfg.OrigDst = func(*net.TCPConn) (netip.AddrPort, error) { return *origDst.Load(), nil }
	r.d.st.Disguise = "example.com"
	r.d.disguise = "example.com"
	r.d.st.RealityPriv, r.d.st.ShortIDs, r.d.st.Tokens, r.d.st.CapturePort = priv, [][]byte{{4, 2}}, [][]byte{token}, capPort
	go r.d.Run(ctx)

	var d net.Dialer
	hub := link.NewHub(link.HubConfig{
		AgentID: "a1", Token: token, Addr: hubLn.Addr().String(),
		Reality: reality.ClientConfig{ServerName: "example.com", PublicKey: pub, ShortID: [8]byte{4, 2}},
		Conns:   []link.ConnSpec{{Name: "c", Dial: d.DialContext}},
		Allow:   tgnet.NewWithPorts([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/16")}, dcAP.Port()),
		DialDC:  d.DialContext,
	})
	go hub.Run(ctx)
	wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	if err := hub.WaitUp(wctx); err != nil {
		t.Fatalf("hub never connected: %v", err)
	}

	// the hub pushes a state over the link
	st := good("e2e-1", user("u1"))
	st.Disguise = "example.com"
	st.DCNets = []string{"127.0.0.0/16"}
	var ack ctrl.StateAck
	if err := hub.Call(ctx, ctrl.MStateApply, st, &ack); err != nil || ack.Rev != "e2e-1" {
		t.Fatalf("state.apply: %+v %v", ack, err)
	}
	var status ctrl.Status
	if err := hub.Call(ctx, ctrl.MStatus, nil, &status); err != nil || status.StateRev != "e2e-1" || !status.Telemt.Running {
		t.Fatalf("status: %+v %v", status, err)
	}

	// a "telemt" connection to the capture port reaches the datacentre and back
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(capPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("obfuscated2-init"))
	buf := make([]byte, len("obfuscated2-init"))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "obfuscated2-init" {
		t.Fatalf("round trip: %q %v", buf, err)
	}
	c.Write([]byte("second"))
	b2 := make([]byte, 6)
	if _, err := io.ReadFull(c, b2); err != nil || string(b2) != "second" {
		t.Fatalf("second: %q %v", b2, err)
	}

	// a destination the hub did not allow never leaves: the agent refuses it first
	unlisted := netip.MustParseAddrPort("10.9.9.9:443")
	origDst.Store(&unlisted)
	c2, _ := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(capPort)))
	defer c2.Close()
	c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c2.Read(make([]byte, 1)); err == nil {
		t.Fatal("a connection to an unlisted address was forwarded")
	}
	if r.d.rejected.Load() == 0 {
		t.Error("rejection not counted")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// A push that finds the rules gone stops telemt at once, and keeps it stopped
// when nft will not take them back; the periodic check is not the only guard.
func TestAPushThatFindsTheRulesGoneStopsTelemt(t *testing.T) {
	r := newRig(t)
	r.apply(t, good("r1", user("u1")))
	n := len(r.ev.all())
	r.live.Store(false)
	r.rulesFail.Store(true)
	if _, err := r.d.ApplyState(context.Background(), good("r2", user("u1")), true); err == nil {
		t.Fatal("the push succeeded without its rules")
	}
	if r.proc.Status().Running {
		t.Fatalf("telemt kept running without its rules: %v", r.ev.all()[n:])
	}
	ev := r.ev.all()[n:]
	stop, failed := -1, -1
	for i, e := range ev {
		if e == "telemt stop" && stop < 0 {
			stop = i
		}
		if e == "rules failed" && failed < 0 {
			failed = i
		}
	}
	if stop < 0 || failed < stop {
		t.Fatalf("want telemt stopped before the rules are tried again: %v", ev)
	}
	if st := r.d.Status(context.Background()); !strings.Contains(st.Telemt.Held, "capture rules") {
		t.Fatalf("held = %q", st.Telemt.Held)
	}
	// a later push that gets them back in starts it again
	r.rulesFail.Store(false)
	r.apply(t, good("r3", user("u1")))
	if !r.proc.Status().Running || r.d.Status(context.Background()).Telemt.Held != "" {
		t.Fatal("telemt did not come back with its rules")
	}
}

// The hub moves its SNI only once it has the ack, so a new cover site leaves
// the old name accepted for a while: a reply lost on the way must not lock it out.
func TestTheOldCoverNameStaysAcceptedForAWhile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dest := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dest.EnableHTTP2 = true
	dest.StartTLS()
	defer dest.Close()
	priv, pub, _ := reality.GenerateKey()
	token := bytes.Repeat([]byte{4}, 32)
	hubLn, _ := net.Listen("tcp", "127.0.0.1:0")

	r := newRig(t, func(c *Config) { c.CoverGrace = 2 * time.Second })
	r.d.cfg.Listen = hubLn
	r.d.cfg.RealityDest = func(string) string { return dest.Listener.Addr().String() }
	r.d.st.Disguise, r.d.disguise = "example.com", "example.com"
	r.d.st.RealityPriv, r.d.st.ShortIDs, r.d.st.Tokens, r.d.st.CapturePort = priv, [][]byte{{4, 2}}, [][]byte{token}, freePort(t)
	go r.d.Run(ctx)

	up := func(sni string, within time.Duration) error {
		var d net.Dialer
		hub := link.NewHub(link.HubConfig{
			AgentID: "a1", Token: token, Addr: hubLn.Addr().String(),
			Reality: reality.ClientConfig{ServerName: sni, PublicKey: pub, ShortID: [8]byte{4, 2}},
			Conns:   []link.ConnSpec{{Name: "c", Dial: d.DialContext}},
		})
		hctx, stop := context.WithCancel(ctx)
		defer stop()
		go hub.Run(hctx)
		wctx, wcancel := context.WithTimeout(ctx, within)
		defer wcancel()
		return hub.WaitUp(wctx)
	}
	if err := up("example.com", 30*time.Second); err != nil {
		t.Fatalf("no link before the switch: %v", err)
	}
	s := good("r1", user("u1"))
	s.Disguise = "new.example.com"
	r.apply(t, s)
	if err := up("example.com", 10*time.Second); err != nil {
		t.Fatalf("the old name was refused right after the switch: %v", err)
	}
	if err := up("new.example.com", 10*time.Second); err != nil {
		t.Fatalf("the new name: %v", err)
	}
	time.Sleep(2500 * time.Millisecond)
	if err := up("example.com", 3*time.Second); err == nil {
		t.Fatal("the old name is still accepted long after the switch")
	}
	if err := up("new.example.com", 10*time.Second); err != nil {
		t.Fatalf("the new name after the grace: %v", err)
	}
}

// telemt may reach nothing but Telegram: its config points the cover site at
// the agent's socket, and the agent dials the site the state names.
func TestTheCoverSiteIsDialledByTheAgentForTelemt(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "mask") // socket paths are short on macOS
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "mask.sock")
	dialled := make(chan string, 4)
	r := newRig(t, func(c *Config) {
		c.MaskSock = sock
		c.CoverDial = func(_ context.Context, addr string) (net.Conn, error) {
			dialled <- addr
			a, b := net.Pipe()
			go func() {
				b.Read(make([]byte, 64)) // a site answers what it is asked
				b.Write([]byte("cover site"))
				b.Close()
			}()
			return a, nil
		}
	})
	r.run(t)
	r.apply(t, good("r1", user("u1")))
	cfg, err := os.ReadFile(filepath.Join(r.dir, "telemt", "config.toml"))
	if err != nil || !strings.Contains(string(cfg), `mask_unix_sock = "`+sock+`"`) || strings.Contains(string(cfg), "mask_host") {
		t.Fatalf("config: %v\n%s", err, cfg)
	}
	if !strings.Contains(string(cfg), "stun_use = false") {
		t.Errorf("telemt still probes for its address:\n%s", cfg)
	}
	var c net.Conn
	waitUntil(t, "the cover socket", func() bool {
		c, err = net.Dial("unix", sock)
		return err == nil
	})
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\n\r\n")) // not a ClientHello: the primary site gets it
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	b, _ := io.ReadAll(c)
	if string(b) != "cover site" {
		t.Errorf("read %q", b)
	}
	select {
	case a := <-dialled:
		if a != "mirror.example.ir:443" {
			t.Errorf("dialled %s, want the state's cover site", a)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent never dialled the cover site")
	}
}

// The link opens a new port beside the old one, and closes the old only once told the new one works.
func TestTheLinkPortMovesInSteps(t *testing.T) {
	old, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, func(c *Config) {
		c.Listen = old
		c.ListenPort = func(port int) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port)) }
	})
	t.Cleanup(func() { r.d.ports.Close() })
	r.apply(t, good("r1", user("u1")))
	answers := func(port int) bool {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
		if err == nil {
			c.Close()
		}
		return err == nil
	}
	saved := func() *state.File {
		f, err := state.Load(filepath.Join(r.dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	oldPort := old.Addr().(*net.TCPAddr).Port

	to := freePort(t)
	for i := 0; i < 2; i++ { // a repeat changes nothing
		if _, err := call(t, r, ctrl.MLinkPort, ctrl.LinkPort{Port: to}); err != nil {
			t.Fatal(err)
		}
	}
	if !answers(oldPort) || !answers(to) {
		t.Fatal("both ports answer while the link moves")
	}
	if f := saved(); f.HubPort != 8443 || f.NextHubPort != to {
		t.Fatalf("saved %d → %d", f.HubPort, f.NextHubPort)
	}
	for i := 0; i < 2; i++ {
		if _, err := call(t, r, ctrl.MLinkPort, ctrl.LinkPort{Finish: true}); err != nil {
			t.Fatal(err)
		}
	}
	if answers(oldPort) || !answers(to) {
		t.Fatal("after the move only the new port answers")
	}
	if f := saved(); f.HubPort != to || f.NextHubPort != 0 {
		t.Fatalf("saved %d → %d", f.HubPort, f.NextHubPort)
	}

	// a move the panel couldn't use is taken back
	other := freePort(t)
	if _, err := call(t, r, ctrl.MLinkPort, ctrl.LinkPort{Port: other}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, r, ctrl.MLinkPort, ctrl.LinkPort{Abort: true}); err != nil {
		t.Fatal(err)
	}
	if answers(other) || !answers(to) || saved().HubPort != to || saved().NextHubPort != 0 {
		t.Fatal("an aborted move left the new port open")
	}

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	for port, code := range map[int]string{
		443: "port_taken", 9443: "port_taken", 0: "bad_request", 70000: "bad_request",
		busy.Addr().(*net.TCPAddr).Port: "port_busy",
	} {
		_, err := call(t, r, ctrl.MLinkPort, ctrl.LinkPort{Port: port})
		var ce *ctrl.Error
		if !errors.As(err, &ce) || ce.Code != code {
			t.Errorf("port %d: %v, want %s", port, err, code)
		}
	}
	if saved().NextHubPort != 0 {
		t.Fatal("a refused port was saved")
	}
}

// An agent that restarts mid-move listens on both ports, so the panel gets in on either.
func TestAnUnfinishedMoveIsReopenedAtStart(t *testing.T) {
	next := freePort(t)
	r := newRig(t, func(c *Config) {
		c.ListenPort = func(port int) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port)) }
	})
	r.d.st.NextHubPort = next
	r.run(t)
	waitUntil(t, "the new port", func() bool {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(next), time.Second)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
}

// Users with no traffic yet are reported at zero, so their first bytes count.
func TestStatsReportConnectionsDevicesAndQuietUsers(t *testing.T) {
	r := newRig(t)
	r.proc.st.Running = true
	r.d.cfg.Scrape = func(context.Context, int) (string, error) {
		return "telemt_user_octets_from_client{user=\"u1\"} 10\n" +
			"telemt_user_connections_current{user=\"u1\"} 2\n" +
			"telemt_user_unique_ips_current{user=\"u1\"} 1\n" +
			"telemt_user_unique_ips_current{user=\"quiet\"} 0\n", nil
	}
	st, err := r.d.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ctrl.UserStat{}
	for _, u := range st.Users {
		got[u.Name] = u
	}
	if !st.Live || got["u1"].Connections != 2 || got["u1"].Devices != 1 || got["u1"].OctetsUp != 10 {
		t.Fatalf("stats = %+v", st)
	}
	if q, ok := got["quiet"]; !ok || q.OctetsUp != 0 {
		t.Fatalf("a user with no traffic is missing: %+v", st.Users)
	}
	r.proc.st.Running = false
	if st, _ := r.d.Stats(context.Background()); !st.Live || len(st.Users) != 0 {
		t.Fatalf("a stopped proxy has nobody on it: %+v", st)
	}
}
