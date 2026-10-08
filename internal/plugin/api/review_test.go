package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/hubnode"
	"github.com/nasnet-community/mtproto-remote-plugin/installer"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/manager"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/plugintest"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/store"
	"github.com/nasnet-community/mtproto-remote-plugin/link"
	"github.com/nasnet-community/mtproto-remote-plugin/mux"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops"
)

// call sends one request with the given headers and returns its status and body.
func (e *env) call(method, path string, hdr map[string]string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.url+path, strings.NewReader("{}"))
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestOtherSitesCannotReadOrWrite(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	users := fmt.Sprintf("/api/servers/%d/users", id)

	// another site's page may not read the users' secrets, by any method or prefix
	for _, site := range []string{"cross-site", "same-site", "something-new"} {
		for _, p := range []string{users, base + users, "/api/config", base + "/api/jobs/x"} {
			code, body := e.call("GET", p, map[string]string{"Sec-Fetch-Site": site, "Origin": "http://evil.example"})
			if code != http.StatusForbidden || !strings.Contains(body, `"error"`) || strings.Contains(body, "secret") {
				t.Fatalf("GET %s from %s = %d %s", p, site, code, body)
			}
		}
		if code, _ := e.call("POST", "/api/probe", map[string]string{"Sec-Fetch-Site": site}); code != http.StatusForbidden {
			t.Fatalf("POST from %s = %d", site, code)
		}
	}
	// the page itself, and typing an API URL into the address bar
	for _, site := range []string{"same-origin", "none"} {
		if code, _ := e.call("GET", users, map[string]string{"Sec-Fetch-Site": site}); code != 200 {
			t.Fatalf("GET from %s = %d", site, code)
		}
	}
	// the panel opens the view in a new tab, from wherever
	for _, p := range []string{"/", base, base + "/", "/health", base + "/health"} {
		if code, _ := e.call("GET", p, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != 200 {
			t.Fatalf("page %s from another site = %d", p, code)
		}
	}

	// no Sec-Fetch-Site (Safari before 16.4): the panel rewrites Host, so Origin never matches it
	panel := map[string]string{"Origin": "http://192.168.88.1", "Host": "192.168.50.11:8080", UIHeader: "1"}
	if code, body := e.call("POST", "/api/probe", panel); code == http.StatusForbidden {
		t.Fatalf("a write through the panel without Sec-Fetch-Site = %d %s", code, body)
	}
	// a proxy that names the original host lets the Origin be checked
	fwd := map[string]string{"Origin": "http://192.168.88.1", "X-Forwarded-Host": "192.168.88.1", UIHeader: "1"}
	if code, _ := e.call("POST", "/api/probe", fwd); code == http.StatusForbidden {
		t.Fatal("a same-site write behind X-Forwarded-Host was refused")
	}
	fwd["Origin"] = "http://evil.example"
	if code, _ := e.call("POST", "/api/probe", fwd); code != http.StatusForbidden {
		t.Fatalf("a foreign Origin behind X-Forwarded-Host = %d", code)
	}
	fwd["Origin"] = "null"
	if code, _ := e.call("PATCH", fmt.Sprintf("/api/servers/%d", id), fwd); code != http.StatusForbidden {
		t.Fatalf("an opaque Origin behind X-Forwarded-Host = %d", code)
	}
}

// Without Sec-Fetch-Site, other sites can't send the page's header: the panel's CORS refuses its preflight.
func TestWithoutSecFetchSiteOnlyThePageHeaderGetsIn(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	var r struct {
		JobID string `json:"jobId"`
	}
	e.json("POST", fmt.Sprintf("/api/servers/%d/rotate", id), nil, 202, &r)
	e.waitJob(r.JobID)
	users := fmt.Sprintf("/api/servers/%d/users", id)
	evil := map[string]string{"Origin": "http://evil.example"}
	page := map[string]string{"Origin": "http://192.168.88.1", UIHeader: "1"}

	for _, p := range []string{users, base + users, "/api/config", base + "/api/servers", "/api/jobs/" + r.JobID, "/api/nope"} {
		for _, m := range []string{"GET", "HEAD"} {
			if code, body := e.call(m, p, evil); code != http.StatusForbidden || strings.Contains(body, "secret") {
				t.Fatalf("%s %s without the header = %d %s", m, p, code, body)
			}
		}
		if code, _ := e.call("GET", p, page); code == http.StatusForbidden {
			t.Fatalf("GET %s with the header was refused", p)
		}
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/probe"}, {"PATCH", fmt.Sprintf("/api/servers/%d", id)}, {"POST", base + users},
		{"DELETE", users + "/alice"}, {"OPTIONS", users}, {"POST", "/api/jobs/" + r.JobID + "/stream"},
	} {
		if code, _ := e.call(c.method, c.path, evil); code != http.StatusForbidden {
			t.Fatalf("%s %s without the header = %d", c.method, c.path, code)
		}
	}
	if code, _ := e.call("POST", "/api/probe", page); code == http.StatusForbidden {
		t.Fatal("a write with the header was refused")
	}
	// the header never overrides Sec-Fetch-Site
	if code, _ := e.call("GET", users, map[string]string{"Sec-Fetch-Site": "cross-site", UIHeader: "1"}); code != http.StatusForbidden {
		t.Fatalf("a cross-site read with the header = %d", code)
	}
	// EventSource can't set headers, and a job carries no secrets (call checks the body)
	for _, p := range []string{"/api/jobs/" + r.JobID + "/stream", base + "/api/jobs/" + r.JobID + "/stream"} {
		code, body := e.call("GET", p, evil)
		if code != 200 || !strings.Contains(body, `"id":"`+r.JobID+`"`) {
			t.Fatalf("GET %s without the header = %d %s", p, code, body)
		}
		e.check(p, []byte(body))
	}
	// the page and the health check stay open
	for _, p := range []string{"/", base, base + "/", "/health", base + "/health"} {
		if code, _ := e.call("GET", p, evil); code != 200 {
			t.Fatalf("page %s without the header = %d", p, code)
		}
	}
}

func coverOf(e *env, id int) map[string]any {
	var s map[string]any
	e.json("GET", fmt.Sprintf("/api/servers/%d", id), nil, 200, &s)
	ag, _ := s["agent"].(map[string]any)
	if ag == nil {
		return nil
	}
	return ag["cover"].(map[string]any)
}

func TestTheCoverStatusComesFromTheRoutersOwnCheck(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	// the agent says it has not checked yet (it just restarted); the router's check has
	wait(t, func() bool {
		c := coverOf(e, id)
		return c != nil && c["ok"] == true && c["checkedAt"] != nil && c["domain"] == plugintest.Domains[0]
	}, "the router's cover check in the view")
}

// right after a switch the router's cached check is still the old site's
func TestTheRoutersCheckCountsOnlyForTheSiteInUse(t *testing.T) {
	at := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	st := &ctrl.Status{}
	st.Disguise.Domain = "new.example.com" // the agent switched and has not checked yet
	snap := hubnode.Snapshot{Agent: st, Disguise: &ctrl.DisguiseResult{OK: true}, DisguiseAt: at, DisguiseDomain: "old.example.com"}
	if c := agentOf(snap).Cover; c.Domain != "new.example.com" || c.OK || c.CheckedAt != nil {
		t.Fatalf("the old site's check under the new name: %+v", c)
	}
	snap.DisguiseDomain = "new.example.com"
	if c := agentOf(snap).Cover; !c.OK || c.CheckedAt == nil || !c.CheckedAt.Equal(at) {
		t.Fatalf("the new site's own check: %+v", c)
	}
}

func TestACoverSiteNobodyCheckedYetSaysSo(t *testing.T) {
	e := newEnv(t, store.Limits{})
	e.agent.Set(func(a *plugintest.Agent) { a.CheckErr = "no route" })
	id := e.install()
	time.Sleep(500 * time.Millisecond) // several status polls and check attempts
	c := coverOf(e, id)
	if c == nil || c["ok"] != false || c["checkedAt"] != nil || c["error"] != "" {
		t.Fatalf("an unchecked cover site = %v", c)
	}
}

func TestTheStreamLimitCountsInstallsStillRunning(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	sp := fmt.Sprintf("/api/servers/%d/settings", id)
	release, err := e.m.Reserve(manager.Install{Host: "203.0.113.5", MaxStreams: 3000})
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	e.json("GET", "/api/config", nil, 200, &cfg)
	if cfg["streamsAllocated"] != 5000.0 {
		t.Fatalf("allocated with an install running = %v", cfg["streamsAllocated"])
	}
	code, body := e.do("PATCH", sp, map[string]any{"maxStreams": 3500})
	if code != http.StatusConflict || !strings.Contains(string(body), "3000 streams are left") {
		t.Fatalf("3500 + 3000 pending on a 6000 budget = %d %s", code, body)
	}
	release()
	e.json("PATCH", sp, map[string]any{"maxStreams": 3500}, 200, nil)
}

func TestASettingsSaveWritesOnlyWhatItWasSent(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.agent.Set(func(a *plugintest.Agent) {
		a.Gate = func(m string, b json.RawMessage) {
			if m == ctrl.MDisguiseCheck && bytes.Contains(b, []byte(plugintest.Domains[1])) {
				once.Do(func() { close(entered) })
				<-release
			}
		}
	})
	done := make(chan int)
	go func() {
		code, _ := e.do("PATCH", fmt.Sprintf("/api/servers/%d/settings", id), map[string]any{"coverDomain": plugintest.Domains[1]})
		done <- code
	}()
	<-entered
	// another save lands while the cover site is being checked
	if err := e.st.Mutate(uint(id), func(s *store.Server) error { s.Record.MaxStreams = 2500; return nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	if code := <-done; code != 200 {
		t.Fatalf("the cover save = %d", code)
	}
	srv, _ := e.st.Get(uint(id))
	if srv.Record.MaxStreams != 2500 || srv.Cover != plugintest.Domains[1] {
		t.Fatalf("after both saves: streams %d, cover %s", srv.Record.MaxStreams, srv.Cover)
	}
}

func TestACallCutByALinkRestartIsNotAServerError(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	var once sync.Once
	e.agent.Set(func(a *plugintest.Agent) {
		a.Gate = func(m string, _ json.RawMessage) {
			if m == ctrl.MLogsTail {
				once.Do(func() { close(entered) })
				<-release
			}
		}
	})
	type answer struct {
		code int
		body []byte
	}
	got := make(chan answer)
	go func() {
		code, body := e.do("GET", fmt.Sprintf("/api/servers/%d/logs", id), nil)
		got <- answer{code, body}
	}()
	<-entered
	e.json("PATCH", fmt.Sprintf("/api/servers/%d/settings", id), map[string]any{"maxStreams": 3000}, 200, nil)
	a := <-got
	if a.code != http.StatusServiceUnavailable || !strings.Contains(string(a.body), "try again") {
		t.Fatalf("a call cut by the restart = %d %s", a.code, a.body)
	}
}

type records struct {
	mu   sync.Mutex
	errs int
}

func (r *records) Enabled(context.Context, slog.Level) bool { return true }
func (r *records) Handle(_ context.Context, rec slog.Record) error {
	if rec.Level >= slog.LevelError {
		r.mu.Lock()
		r.errs++
		r.mu.Unlock()
	}
	return nil
}
func (r *records) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *records) WithGroup(string) slog.Handler      { return r }

func TestLinkDropsAreNotServerErrors(t *testing.T) {
	rec := &records{}
	a := &API{cfg: Config{Log: slog.New(rec)}}
	reset := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	for _, err := range []error{
		mux.ErrSessionClosed, fmt.Errorf("call: %w", mux.ErrSessionClosed), mux.ErrGoAway, mux.ErrStreamReset,
		io.EOF, io.ErrUnexpectedEOF, net.ErrClosed, reset,
	} {
		w := httptest.NewRecorder()
		a.fail(w, err)
		if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "mux") {
			t.Errorf("%v = %d %s", err, w.Code, w.Body)
		}
	}
	w := httptest.NewRecorder()
	a.fail(w, mux.ErrTooManyOpen)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "busy") {
		t.Errorf("a full link = %d %s", w.Code, w.Body)
	}
	if rec.errs != 0 {
		t.Fatalf("%d link drops were logged as errors", rec.errs)
	}
	a.fail(httptest.NewRecorder(), errors.New("something we did not expect"))
	if rec.errs != 1 {
		t.Fatal("an unexpected error was not logged")
	}
}

func TestErrorsUseTheUIsWords(t *testing.T) {
	for in, want := range map[string]string{
		"relay: invalid request: the name is too long":                                                "the name is too long",
		"installer: connect: dial tcp 192.0.2.1:22: connect: connection refused":                      "connect: dial tcp 192.0.2.1:22: connect: connection refused",
		"relay: install: installer: www.example.com is not usable as a disguise from this VPS: no h2": "install: www.example.com is not usable as a cover site from this VPS: no h2",
		"relay: this node's CPU architecture is unknown: run a repair once to record it":              "this server's CPU architecture is unknown: run a repair once to record it",
		"installer: unreadable disguise check: EOF":                                                   "unreadable cover site check: EOF",
		"relay: invalid request": "invalid request",
		"plain words":            "plain words",
	} {
		if got := human(in); got != want {
			t.Errorf("human(%q) = %q, want %q", in, got, want)
		}
	}
	v := jobOf(relayops.JobView{Steps: []installer.Step{{Name: "disguise", State: "failed", Detail: "installer: x.example.com is not usable as a disguise from this VPS: no h2"}}})
	if d := v.Steps[0].Detail; strings.Contains(d, "installer") || strings.Contains(d, "disguise") {
		t.Fatalf("a step's detail = %q", d)
	}
}

func TestLinkPushAndEventErrorsUseTheUIsWords(t *testing.T) {
	raw := "relay: invalid request: link: mux: ctrl: node 3 has unusable link credentials"
	want := "server 3 has unusable link credentials"
	if got := human("relay: node 3 has unusable link credentials"); got != want {
		t.Errorf("a bare node error = %q", got)
	}
	if got := human("Node 3 is gone"); got != "Server 3 is gone" {
		t.Errorf("a capital node = %q", got)
	}
	snap := hubnode.Snapshot{LastPushErr: raw, Link: link.Status{Conns: []link.ConnStatus{{Name: "link-1", LastErr: raw}}}}
	if got := linkOf(snap).Conns[0].LastError; got != want {
		t.Errorf("a connection's last error = %q", got)
	}
	if got := syncOf(snap).LastPushError; got != want {
		t.Errorf("the last push error = %q", got)
	}
	evs := eventsOf([]manager.Event{{Kind: "push_failed", Detail: "r1", Err: raw}, {Kind: "link_up"}})
	if got := evs[0].Detail; got != "r1 "+want {
		t.Errorf("an event's detail = %q", got)
	}
	if got := evs[1].Detail; got != "" {
		t.Errorf("an event with no detail = %q", got)
	}
}

func TestTheViewSaysWhetherAServerFinishedRegistering(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	half, err := e.st.Create(store.NewServer{Name: "half", Host: "192.0.2.9", MaxStreams: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]any
	e.json("GET", "/api/servers", nil, 200, &list)
	for _, s := range list {
		if want := s["id"] == float64(id); s["registered"] != want {
			t.Fatalf("server %v registered = %v", s["id"], s["registered"])
		}
	}
	var one map[string]any
	e.json("GET", fmt.Sprintf("/api/servers/%d", half), nil, 200, &one)
	if one["registered"] != false {
		t.Fatalf("the half-registered server's view = %v", one["registered"])
	}
}

func TestRefreshWithoutAURLSaysSo(t *testing.T) {
	e := newEnv(t, store.Limits{})
	code, body := e.do("POST", "/api/allowlist/refresh", nil)
	if code != http.StatusConflict || !strings.Contains(string(body), "no refresh URL is configured") {
		t.Fatalf("a refresh with no URL = %d %s", code, body)
	}
}

var oldName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

func oldValidName(s string) bool { return oldName.MatchString(s) && strings.Trim(s, ".") != "" }

func TestValidNameMatchesTheOldRegexp(t *testing.T) {
	for _, s := range []string{
		"", ".", "..", "...", "a", "a.", ".a", "a b", "a\n", "\na", "alice", "Bob_2", "x-y.z",
		strings.Repeat("x", 32), strings.Repeat("x", 33), strings.Repeat(".", 32), "é", "aé", "a\x00",
		"\xff", "a\xffb", "a/b", "a%2e", "ä" + strings.Repeat("x", 30), "A-Z_a-z.0-9", "~", "a~b",
	} {
		if got, want := validName(s), oldValidName(s); got != want {
			t.Errorf("validName(%q) = %v, the regexp says %v", s, got, want)
		}
	}
}

func FuzzValidName(f *testing.F) {
	for _, s := range []string{"", ".", "alice", strings.Repeat("y", 32), "a b", "é"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := validName(s), oldValidName(s); got != want {
			t.Fatalf("validName(%q) = %v, the regexp says %v", s, got, want)
		}
	})
}
