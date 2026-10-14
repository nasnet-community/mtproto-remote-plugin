package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/agent/supervise"
	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/local/proxy"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/local/router"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/local/store"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/users"
)

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
	return supervise.Status{Running: f.running, Restarts: f.restarts}
}

type env struct {
	h    http.Handler
	dir  string
	st   *store.Store
	proc *fakeProc
	r    *proxy.Runner
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ch, err := router.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{dir: dir, st: st, proc: &fakeProc{}}
	// a Unix socket's path is short: t.TempDir's names are too long on a Mac
	short, err := os.MkdirTemp("", "mtl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	e.r = proxy.New(proxy.Config{
		Store: st, Bin: "telemt", Dir: filepath.Join(short, "telemt"), Port: 8443, MaxConns: 1000,
		NewProc: func(supervise.Spec) proxy.Proc { return e.proc },
		Scrape:  func(context.Context, int) (string, error) { return "", nil },
		Version: func(context.Context, string) string { return "3.4.25" },
		Check: func(_ context.Context, d string) ctrl.DisguiseResult {
			if strings.HasPrefix(d, "bad.") {
				return ctrl.DisguiseResult{Err: "cannot connect to " + d + ":443 from the VPS"}
			}
			return ctrl.DisguiseResult{OK: true, TLS13: true, X25519: true}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := e.r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.h = New(Config{Runner: e.r, Store: st, Router: ch, Version: "1.0.0", Arch: "arm64", Conns: Limits{1000, 100, 5000}})
	return e
}

// routerSays is the router script's state file.
func (e *env) routerSays(t *testing.T, state string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, "router", "state"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *env) do(method, path string, body any) (int, map[string]any) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("X-MTProto-UI", "1")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		var list []any
		if json.Unmarshal(w.Body.Bytes(), &list) == nil {
			return w.Code, map[string]any{"list": list}
		}
	}
	return w.Code, out
}

func get(m map[string]any, path string) any {
	var v any = m
	for _, k := range strings.Split(path, ".") {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[k]
	}
	return v
}

const homeState = "port=41873\naddress=abcd1234.sn.mynetname.net\npublic=203.0.113.5\nwan=10.0.3.15\ncloud=updated\ndomestic=yes\nscript=1\n"

func TestSetupNeedsAWorkingCoverSiteAndGivesTheFirstLink(t *testing.T) {
	e := newEnv(t)
	code, v := e.do("GET", "/api/local", nil)
	if code != 200 || v["coverDomain"] != "" || get(v, "proxy.held") != "waiting for a cover site" || get(v, "address.reported") != false {
		t.Fatalf("before setup: %d %v", code, v)
	}
	if code, _ := e.do("GET", "/api/local/users", nil); code != http.StatusConflict {
		t.Fatalf("users before setup = %d", code)
	}
	code, v = e.do("POST", "/api/local/setup", map[string]any{"coverDomain": "bad.example.com", "firstUser": "mom"})
	if code != 400 || !strings.Contains(v["error"].(string), "does not work as a cover site from this router: cannot connect to bad.example.com:443 from the router") {
		t.Fatalf("a bad site = %d %v", code, v)
	}
	if code, v = e.do("POST", "/api/local/setup", map[string]any{"coverDomain": "www.example.com", "firstUser": "no spaces"}); code != 400 {
		t.Fatalf("a bad name = %d %v", code, v)
	}
	e.routerSays(t, homeState)
	code, v = e.do("POST", "/api/local/setup", map[string]any{"coverDomain": " WWW.Example.com ", "firstUser": "mom"})
	if code != 200 || v["coverDomain"] != "www.example.com" || v["userCount"] != 1.0 || get(v, "proxy.held") != "" || get(v, "cover.ok") != true {
		t.Fatalf("setup = %d %v", code, v)
	}
	if code, _ := e.do("POST", "/api/local/setup", map[string]any{"coverDomain": "www.example.com", "firstUser": "dad"}); code != http.StatusConflict {
		t.Fatalf("a second setup = %d", code)
	}
	code, v = e.do("GET", "/api/local/users", nil)
	list := v["list"].([]any)
	if code != 200 || len(list) != 1 {
		t.Fatalf("users = %d %v", code, v)
	}
	link := get(list[0].(map[string]any), "links.https").(string)
	if !strings.HasPrefix(link, "https://t.me/proxy?server=abcd1234.sn.mynetname.net&port=41873&secret=ee") {
		t.Fatalf("link = %s", link)
	}
	if !e.proc.Status().Running {
		t.Fatal("telemt isn't running after setup")
	}
}

func TestTheAddressInLinksCanBeSetByHand(t *testing.T) {
	e := newEnv(t)
	e.routerSays(t, "port=41873\naddress=\npublic=\nwan=10.0.3.15\ncloud=\ndomestic=yes\nscript=1\n")
	e.do("POST", "/api/local/setup", map[string]any{"coverDomain": "www.example.com", "firstUser": "mom"})
	if _, v := e.do("GET", "/api/local", nil); get(v, "address.host") != "" || get(v, "address.port") != 41873.0 {
		t.Fatalf("no IP Cloud: %v", v["address"])
	}
	for _, bad := range []map[string]any{{"linkHost": "not a host"}, {"linkHost": "2001:db8::1"}, {"linkPort": 70000}, {"maxConns": 50}, {"maxConns": 6000}} {
		if code, v := e.do("PATCH", "/api/local/settings", bad); code != 400 {
			t.Errorf("%v = %d %v", bad, code, v)
		}
	}
	code, v := e.do("PATCH", "/api/local/settings", map[string]any{"linkHost": "Home.Example.com", "linkPort": 443})
	if code != 200 || get(v, "address.host") != "home.example.com" || get(v, "address.port") != 443.0 || get(v, "address.customPort") != 443.0 {
		t.Fatalf("by hand = %d %v", code, v["address"])
	}
	if b, _ := os.ReadFile(filepath.Join(e.dir, "router", "host")); string(b) != "home.example.com\n" {
		t.Fatalf("the router isn't told: %q", b)
	}
	_, v = e.do("GET", "/api/local/users", nil)
	if link := get(v["list"].([]any)[0].(map[string]any), "links.tg").(string); !strings.Contains(link, "server=home.example.com&port=443&") {
		t.Fatalf("link = %s", link)
	}
	// empty goes back to the router's own
	if _, v = e.do("PATCH", "/api/local/settings", map[string]any{"linkHost": "", "linkPort": 0}); get(v, "address.port") != 41873.0 || get(v, "address.customHost") != "" {
		t.Fatalf("back = %v", v["address"])
	}
}

func TestSettingsCheckANewCoverSiteAndChangeTheLimit(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/api/local/setup", map[string]any{"coverDomain": "www.example.com", "firstUser": "mom"})
	if code, v := e.do("PATCH", "/api/local/settings", map[string]any{"coverDomain": "bad.example.org"}); code != 400 || !strings.Contains(v["error"].(string), "from the router") {
		t.Fatalf("a bad site = %d %v", code, v)
	}
	code, v := e.do("PATCH", "/api/local/settings", map[string]any{"coverDomain": "cdn.example.org", "maxConns": 500, "proxyOptions": map[string]any{"extraCoverNames": []string{"www.example.net"}}})
	if code != 200 || v["coverDomain"] != "cdn.example.org" || get(v, "maxConns.value") != 500.0 || get(v, "proxyOptions.extraCoverNames").([]any)[0] != "www.example.net" {
		t.Fatalf("settings = %d %v", code, v)
	}
	if code, v = e.do("PATCH", "/api/local/settings", map[string]any{"proxyOptions": map[string]any{"extraCoverNames": []string{"cdn.example.org"}}}); code != 400 {
		t.Fatalf("the main site as an extra = %d %v", code, v)
	}
	if code, v = e.do("PATCH", "/api/local/settings", map[string]any{"maxConns": 0}); get(v, "maxConns.value") != 1000.0 {
		t.Fatalf("back to the default = %d %v", code, v["maxConns"])
	}
}

func TestANewPortIsAskedOfTheRouter(t *testing.T) {
	e := newEnv(t)
	if code, _ := e.do("POST", "/api/local/new-port", nil); code != http.StatusConflict {
		t.Fatalf("before the router reported = %d", code)
	}
	e.routerSays(t, homeState)
	code, v := e.do("POST", "/api/local/new-port", nil)
	if code != 200 || get(v, "address.newPortAskedAt") == nil {
		t.Fatalf("asked = %d %v", code, v["address"])
	}
	if b, _ := os.ReadFile(filepath.Join(e.dir, "router", "request")); string(b) != "new-port\n" {
		t.Fatalf("request = %q", b)
	}
}

func TestStopStartRestartAndTheUsersRoutes(t *testing.T) {
	e := newEnv(t)
	e.routerSays(t, homeState)
	e.do("POST", "/api/local/setup", map[string]any{"coverDomain": "www.example.com", "firstUser": "mom"})
	if code, v := e.do("POST", "/api/local/users", map[string]any{"name": "dad", "quotaBytes": 5 * users.MaxNote}); code != 201 || v["state"] != "active" {
		t.Fatalf("a new user = %d %v", code, v)
	}
	if code, v := e.do("POST", "/api/local/users", map[string]any{"name": "dad"}); code != 409 || !strings.Contains(v["error"].(string), "already a user called dad") {
		t.Fatalf("twice = %d %v", code, v)
	}
	if code, _ := e.do("POST", "/api/local/restart", nil); code != 200 || e.proc.Status().Restarts != 1 {
		t.Fatalf("restart = %d", code)
	}
	if code, v := e.do("POST", "/api/local/proxy", map[string]any{"stopped": true}); code != 200 || v["proxyStopped"] != true || e.proc.Status().Running {
		t.Fatalf("stop = %d %v", code, v)
	}
	if code, _ := e.do("POST", "/api/local/restart", nil); code != http.StatusConflict {
		t.Fatalf("restart while stopped = %d", code)
	}
	if code, _ := e.do("POST", "/api/local/proxy", map[string]any{"stopped": false}); code != 200 || !e.proc.Status().Running {
		t.Fatalf("start = %d", code)
	}
	if code, v := e.do("POST", "/api/local/disguise-check", map[string]any{"domain": "bad.example.com"}); code != 200 || v["ok"] != false {
		t.Fatalf("check = %d %v", code, v)
	}
	if code, v := e.do("GET", "/api/config", nil); code != 200 || v["flavor"] != "local" {
		t.Fatalf("config = %d %v", code, v)
	}
}

func TestOtherSitesAreRefused(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest("POST", "/api/local/new-port", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site = %d", w.Code)
	}
	req = httptest.NewRequest("POST", "/api/local/new-port", nil) // no header at all, like a form post from an old browser
	w = httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("no header = %d", w.Code)
	}
}
