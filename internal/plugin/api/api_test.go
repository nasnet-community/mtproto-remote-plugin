package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/manager"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/plugintest"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/store"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/ui"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops/relaytest"
	"github.com/nasnet-community/mtproto-remote-plugin/tgnet"
)

const (
	base     = "/api/plugin/view/telegram-mtproto"
	password = "hunter2-super-secret"
	privKey  = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA-not-a-real-key\n-----END OPENSSH PRIVATE KEY-----"
	phrase   = "open-sesame-passphrase"
)

var wait = plugintest.Wait

type env struct {
	t     *testing.T
	agent *plugintest.Agent
	st    *store.Store
	m     *manager.Manager
	rig   *relaytest.Rig
	allow *tgnet.Refresher
	srv   *httptest.Server
	url   string

	mu      sync.Mutex
	secrets map[string]bool
}

func newEnv(t *testing.T, lim store.Limits) *env {
	t.Helper()
	a := plugintest.Start(t)
	st, err := store.Open(t.TempDir(), lim)
	if err != nil {
		t.Fatal(err)
	}
	rf := &tgnet.Refresher{List: tgnet.New([]netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")})}
	m := manager.New(manager.Config{Store: st, Allow: rf, Conns: 1, StatusEvery: 100 * time.Millisecond, StatsEvery: 100 * time.Millisecond, FlushEvery: 100 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); m.Stop() })
	m.Start(ctx)
	rig := relaytest.NewRig(func(c *relayops.ServiceConfig) {
		c.Store, c.Links, c.Admin, c.LinkWait = st.Records(), m.Links(), m.Admin(), 20*time.Second
	})
	rig.SSH.Result = a.Result()
	h := New(Config{
		Svc: rig.Svc, Manager: m, Store: st, Allow: rf, Version: "1.2.3", BasePath: base,
		Index: ui.NewHandler([]byte("<html><head></head><body>app</body></html>"), base),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e := &env{t: t, agent: a, st: st, m: m, rig: rig, allow: rf, srv: srv, url: srv.URL, secrets: map[string]bool{}}
	for _, s := range []string{password, privKey, phrase, "not-a-real-key"} {
		e.secrets[s] = true
	}
	return e
}

// learn adds every link secret the store holds now; old ones stay on the list.
func (e *env) learn() {
	e.mu.Lock()
	defer e.mu.Unlock()
	add := func(h string) {
		if h == "" {
			return
		}
		e.secrets[h] = true
		if b, err := hex.DecodeString(h); err == nil {
			e.secrets[base64.StdEncoding.EncodeToString(b)] = true
		}
	}
	for _, s := range e.st.List() {
		add(s.Record.Token)
		add(s.Record.RealityPub)
		add(s.Record.ShortID)
		add(s.Record.NextSeed)
	}
	res := e.agent.Result()
	add(hex.EncodeToString(res.Creds.Token))
	add(hex.EncodeToString(res.Creds.Pub))
	add(hex.EncodeToString(res.Creds.ShortID[:]))
}

// leak returns a secret the body carries, if any.
func (e *env) leak(body []byte) string {
	e.learn()
	e.mu.Lock()
	defer e.mu.Unlock()
	for s := range e.secrets {
		if bytes.Contains(body, []byte(s)) {
			return s
		}
	}
	return ""
}

// check fails the test if a body carries any secret.
func (e *env) check(path string, body []byte) {
	e.t.Helper()
	if s := e.leak(body); s != "" {
		e.t.Fatalf("%s leaked a secret (%.12s...): %s", path, s, body)
	}
}

func (e *env) do(method, path string, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, e.url+path, rd)
	req.Header.Set(UIHeader, "1") // as the page sends it
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	e.check(path, out)
	return resp.StatusCode, out
}

func (e *env) json(method, path string, body any, want int, into any) {
	e.t.Helper()
	code, out := e.do(method, path, body)
	if code != want {
		e.t.Fatalf("%s %s = %d, want %d: %s", method, path, code, want, out)
	}
	if into != nil {
		if err := json.Unmarshal(out, into); err != nil {
			e.t.Fatalf("%s %s: %v: %s", method, path, err, out)
		}
	}
}

func locked(mu *sync.Mutex, f func() bool) bool {
	mu.Lock()
	defer mu.Unlock()
	return f()
}

func hasKeys(t *testing.T, what string, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("%s has no %q: %v", what, k, m)
		}
	}
}

func installBody() map[string]any {
	return map[string]any{
		"name": "Relay 1", "host": "127.0.0.1", "sshPort": 22, "sshUser": "root",
		"password": password, "hostKey": "SHA256:abc", "coverDomain": plugintest.Domains[0], "firstUser": "alice",
	}
}

func (e *env) waitJob(id string) map[string]any {
	e.t.Helper()
	var job map[string]any
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		e.json("GET", "/api/jobs/"+id, nil, 200, &job)
		if job["state"] != "running" {
			return job
		}
	}
	e.t.Fatalf("job never finished: %v", job)
	return nil
}

// install runs a good install and returns the server id once its link is up.
func (e *env) install() int {
	e.t.Helper()
	var r struct {
		JobID string `json:"jobId"`
	}
	e.json("POST", "/api/servers", installBody(), 202, &r)
	job := e.waitJob(r.JobID)
	if job["state"] != "ok" {
		e.t.Fatalf("install job: %v", job)
	}
	id := int(job["serverId"].(float64))
	wait(e.t, func() bool {
		var s map[string]any
		e.json("GET", fmt.Sprintf("/api/servers/%d", id), nil, 200, &s)
		return s["link"].(map[string]any)["state"] == "up" && s["agent"] != nil
	}, "the link and a status report")
	return id
}

func TestTheSecretCheckBites(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	srv, _ := e.st.Get(uint(id))
	tok, _ := hex.DecodeString(srv.Record.Token)
	for _, body := range []string{
		`{"t":"` + srv.Record.Token + `"}`,
		`{"t":"` + base64.StdEncoding.EncodeToString(tok) + `"}`,
		`{"k":"` + srv.Record.RealityPub + `"}`,
		`{"s":"` + srv.Record.ShortID + `"}`,
		`{"p":"` + password + `"}`,
	} {
		if e.leak([]byte(body)) == "" {
			t.Errorf("the check missed %s", body)
		}
	}
}

func TestHealthAndPageAtBothPrefixes(t *testing.T) {
	e := newEnv(t, store.Limits{})
	for _, p := range []string{"", base} {
		code, body := e.do("GET", p+"/health", nil)
		if code != 200 || string(body) != "ok" {
			t.Fatalf("%s/health = %d %q", p, code, body)
		}
		code, body = e.do("GET", p+"/", nil)
		if code != 200 || !strings.Contains(string(body), `<base href="`+base+`/">`) {
			t.Fatalf("%s/ = %d %s", p, code, body)
		}
		var cfg map[string]any
		e.json("GET", p+"/api/config", nil, 200, &cfg)
	}
	if code, _ := e.do("GET", base, nil); code != 200 {
		t.Fatalf("the base path without a slash = %d", code)
	}
}

func TestConfig(t *testing.T) {
	e := newEnv(t, store.Limits{MaxServers: 1})
	var cfg struct {
		Version           string `json:"version"`
		MaxServers        int    `json:"maxServers"`
		ServerCount       int    `json:"serverCount"`
		StreamBudget      int    `json:"streamBudget"`
		StreamsAllocated  int    `json:"streamsAllocated"`
		DefaultMaxStreams int    `json:"defaultMaxStreams"`
		Allowlist         struct {
			Prefixes    int     `json:"prefixes"`
			Placeholder bool    `json:"placeholder"`
			RefreshedAt *string `json:"refreshedAt"`
			LastError   *string `json:"lastError"`
			URL         *string `json:"url"`
		} `json:"allowlist"`
	}
	e.json("GET", "/api/config", nil, 200, &cfg)
	if cfg.Version != "1.2.3" || cfg.MaxServers != 1 || cfg.ServerCount != 0 || cfg.StreamBudget != 6000 || cfg.StreamsAllocated != 0 || cfg.DefaultMaxStreams != 2000 {
		t.Fatalf("config = %+v", cfg)
	}
	al := cfg.Allowlist
	if al.Prefixes != 1 || al.Placeholder != tgnet.PinnedIsPlaceholder() || al.RefreshedAt != nil || al.LastError == nil || al.URL == nil || *al.URL != "" {
		t.Fatalf("allowlist = %+v", al)
	}
}

// A bad cover site comes back with the request, with what failed, and holds nothing.
func TestABadCoverSiteIsRefusedWithItsTest(t *testing.T) {
	e := newEnv(t, store.Limits{MaxServers: 1})
	e.rig.SSH.Cover = &ctrl.DisguiseResult{TLS13: true, X25519: true, CertOK: true, Addrs: []string{"192.0.2.7"}, Err: "the site does not offer h2"}
	var out struct {
		Error string `json:"error"`
		Cover struct {
			OK    bool     `json:"ok"`
			TLS13 bool     `json:"tls13"`
			H2    bool     `json:"h2"`
			Addrs []string `json:"addrs"`
			Error string   `json:"error"`
		} `json:"cover"`
	}
	e.json("POST", "/api/servers", installBody(), 422, &out)
	if !strings.Contains(out.Error, "does not work as a cover site") || out.Cover.OK || !out.Cover.TLS13 || out.Cover.H2 ||
		len(out.Cover.Addrs) != 1 || !strings.Contains(out.Cover.Error, "h2") {
		t.Fatalf("out = %+v", out)
	}
	var list []map[string]any
	e.json("GET", "/api/servers", nil, 200, &list)
	if len(list) != 0 || len(e.rig.SSH.Installed) != 0 {
		t.Fatalf("servers = %v, installs = %d", list, len(e.rig.SSH.Installed))
	}

	// a login that fails comes back the same way, and the slot is free again
	e.rig.SSH.Cover = nil
	e.rig.SSH.CoverErr = fmt.Errorf("installer: login: ssh: unable to authenticate")
	var failed map[string]any
	e.json("POST", "/api/servers", installBody(), 502, &failed)
	if msg, _ := failed["error"].(string); !strings.Contains(msg, "unable to authenticate") {
		t.Fatalf("error = %v", failed)
	}
	e.rig.SSH.CoverErr = nil
	e.json("POST", "/api/servers", installBody(), 202, nil)
}

func TestInstallAndManage(t *testing.T) {
	e := newEnv(t, store.Limits{})

	var probe map[string]any
	e.json("POST", "/api/probe", map[string]any{"host": "127.0.0.1", "sshPort": 22, "sshUser": "root"}, 200, &probe)
	if probe["fingerprint"] != "SHA256:abc" || probe["status"] != "new" {
		t.Fatalf("probe = %v", probe)
	}
	e.json("POST", "/api/probe", map[string]any{"host": "bad host!", "sshPort": 22}, 400, nil)

	bad := installBody()
	bad["coverDomain"] = "not a domain"
	e.json("POST", "/api/servers", bad, 400, nil)
	bad = installBody()
	bad["maxStreams"] = 50
	e.json("POST", "/api/servers", bad, 400, nil)
	bad["maxStreams"] = 7000
	e.json("POST", "/api/servers", bad, 409, nil)
	bad = installBody()
	bad["firstUser"] = "no spaces"
	e.json("POST", "/api/servers", bad, 400, nil)
	bad = installBody()
	delete(bad, "password")
	e.json("POST", "/api/servers", bad, 400, nil)

	// the install, followed over SSE
	var r struct {
		JobID string `json:"jobId"`
	}
	e.json("POST", "/api/servers", installBody(), 202, &r)
	resp, err := http.Get(e.url + base + "/api/jobs/" + r.JobID + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("stream content type %q", ct)
	}
	var events []map[string]any
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		e.check("stream", []byte(line))
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var j map[string]any
		if err := json.Unmarshal([]byte(data), &j); err != nil {
			t.Fatalf("bad event %q", line)
		}
		events = append(events, j)
	}
	resp.Body.Close()
	if len(events) == 0 {
		t.Fatal("no events")
	}
	last := events[len(events)-1]
	if last["state"] != "ok" || last["kind"] != "install" {
		t.Fatalf("the stream ended on %v", last)
	}
	hasKeys(t, "job", last, "id", "kind", "state", "steps", "serverId", "startedAt", "finishedAt")
	for _, st := range last["steps"].([]any) {
		hasKeys(t, "step", st.(map[string]any), "name", "state")
	}
	id := int(last["serverId"].(float64))
	sp := fmt.Sprintf("/api/servers/%d", id)
	wait(t, func() bool {
		var s map[string]any
		e.json("GET", sp, nil, 200, &s)
		return s["link"].(map[string]any)["state"] == "up" && s["agent"] != nil
	}, "the link and a status report")

	// the list and the detail
	var list []map[string]any
	e.json("GET", "/api/servers", nil, 200, &list)
	if len(list) != 1 {
		t.Fatalf("servers = %v", list)
	}
	s := list[0]
	hasKeys(t, "server", s, "id", "name", "host", "sshPort", "sshUser", "hostKey", "createdAt", "coverDomain", "maxStreams", "hubPort", "arch", "agentVersion", "telemtVersion", "lastSeenAt", "userCount", "link", "agent", "sync")
	if _, ok := s["events"]; ok {
		t.Error("the list carries events")
	}
	if s["name"] != "Relay 1" || s["host"] != "127.0.0.1" || s["coverDomain"] != plugintest.Domains[0] || s["maxStreams"] != 2000.0 || s["userCount"] != 1.0 || s["arch"] != "amd64" {
		t.Fatalf("server = %v", s)
	}
	// repair and remove prefill the login from these
	if s["sshPort"] != 22.0 || s["sshUser"] != "root" || s["hostKey"] == "" {
		t.Fatalf("server login fields = %v %v %v", s["sshPort"], s["sshUser"], s["hostKey"])
	}
	var detail map[string]any
	e.json("GET", sp, nil, 200, &detail)
	hasKeys(t, "link", detail["link"].(map[string]any), "state", "up", "total", "streams", "relayed", "dialFailures", "violations", "bytesToDc", "bytesFromDc", "conns")
	conn := detail["link"].(map[string]any)["conns"].([]any)[0].(map[string]any)
	hasKeys(t, "conn", conn, "name", "up", "since", "rttMs", "streams", "lastError")
	ag := detail["agent"].(map[string]any)
	hasKeys(t, "agent", ag, "version", "telemtVersion", "uptimeSec", "rssBytes", "stateRev", "reportedAt", "telemt", "capture", "cover")
	hasKeys(t, "telemt", ag["telemt"].(map[string]any), "running", "restarts", "uptimeSec", "lastExit", "crashLoop")
	hasKeys(t, "capture", ag["capture"].(map[string]any), "rules", "active", "rejected")
	hasKeys(t, "cover", ag["cover"].(map[string]any), "domain", "ok", "checkedAt", "error")
	hasKeys(t, "sync", detail["sync"].(map[string]any), "desiredRev", "appliedRev", "lastPushAt", "lastPushError", "healthy")
	evs, _ := detail["events"].([]any)
	if len(evs) == 0 || evs[0].(map[string]any)["kind"] != "link_up" {
		t.Fatalf("events = %v", detail["events"])
	}
	hasKeys(t, "event", evs[0].(map[string]any), "at", "kind", "detail")

	// the stored host key now answers the probe
	e.json("POST", "/api/probe", map[string]any{"host": "127.0.0.1", "sshPort": 22}, 200, &probe)
	if probe["status"] != "known" {
		t.Fatalf("probe after install = %v", probe)
	}
	e.rig.SSH.Mu.Lock()
	e.rig.SSH.FP = "SHA256:zzz"
	e.rig.SSH.Mu.Unlock()
	e.json("POST", "/api/probe", map[string]any{"host": "127.0.0.1", "sshPort": 22}, 200, &probe)
	if probe["status"] != "changed" {
		t.Fatalf("probe with a new key = %v", probe)
	}
	e.json("POST", "/api/servers", installBody(), 409, nil) // the host is a server already

	// rename
	e.json("PATCH", sp, map[string]any{"name": "Frankfurt"}, 200, &s)
	if s["name"] != "Frankfurt" {
		t.Fatalf("rename = %v", s)
	}
	e.json("PATCH", sp, map[string]any{"name": " "}, 400, nil)

	// control passthroughs
	var ok map[string]any
	e.json("POST", sp+"/restart", nil, 200, &ok)
	if ok["ok"] != true || locked(&e.agent.Mu, func() bool { return e.agent.Restarts == 1 }) == false {
		t.Fatalf("restart = %v", ok)
	}
	e.agent.Set(func(a *plugintest.Agent) { a.Logs = []string{"one", "two", "three"} })
	var logs struct {
		Lines []string `json:"lines"`
	}
	e.json("GET", sp+"/logs?lines=2", nil, 200, &logs)
	if len(logs.Lines) != 2 || logs.Lines[1] != "three" {
		t.Fatalf("logs = %v", logs)
	}
	var cc map[string]any
	e.json("POST", sp+"/disguise-check", map[string]any{"domain": "other.example.com"}, 200, &cc)
	hasKeys(t, "cover check", cc, "ok", "tls13", "x25519", "h2", "certOk", "addrs", "latencyMs", "error")
	if cc["ok"] != true {
		t.Fatalf("check = %v", cc)
	}
	e.json("POST", sp+"/disguise-check", map[string]any{"domain": "nope"}, 400, nil)

	// update telemt over the link
	e.json("POST", sp+"/update", map[string]any{"kind": "telemt"}, 202, &r)
	if job := e.waitJob(r.JobID); job["state"] != "ok" || job["kind"] != "update-telemt" {
		t.Fatalf("update = %v", job)
	}
	if !locked(&e.agent.Mu, func() bool { return len(e.agent.Updates) == 1 && e.agent.Updates[0].Kind == "telemt" }) {
		t.Fatal("the agent got no telemt update")
	}
	e.json("POST", sp+"/update", map[string]any{"kind": "kernel"}, 400, nil)

	// rotate the link keys
	e.json("POST", sp+"/rotate", nil, 202, &r)
	if job := e.waitJob(r.JobID); job["state"] != "ok" {
		t.Fatalf("rotate = %v", job)
	}
	wait(t, func() bool {
		e.json("GET", sp, nil, 200, &s)
		return s["link"].(map[string]any)["state"] == "up"
	}, "the link on the new keys")

	var cfg map[string]any
	e.json("GET", "/api/config", nil, 200, &cfg)
	if cfg["serverCount"] != 1.0 || cfg["streamsAllocated"] != 2000.0 {
		t.Fatalf("config = %v", cfg)
	}

	// and remove it while the link is up: no login needed
	e.json("POST", sp+"/remove", map[string]any{}, 202, &r)
	if job := e.waitJob(r.JobID); job["state"] != "ok" || job["kind"] != "uninstall" {
		t.Fatalf("remove = %v", job)
	}
	e.json("GET", "/api/servers", nil, 200, &list)
	if len(list) != 0 || !locked(&e.agent.Mu, func() bool { return e.agent.Uninstalls == 1 }) {
		t.Fatalf("after remove: %v", list)
	}
	e.json("GET", sp, nil, 404, nil)
}

type userJSON struct {
	Name       string  `json:"name"`
	Enabled    bool    `json:"enabled"`
	State      string  `json:"state"`
	MaxIPs     int     `json:"maxIps"`
	ExpiresAt  *string `json:"expiresAt"`
	QuotaBytes uint64  `json:"quotaBytes"`
	Up         uint64  `json:"up"`
	Down       uint64  `json:"down"`
	CreatedAt  string  `json:"createdAt"`
	Secret     string  `json:"secret"`
	Links      struct {
		TG    string `json:"tg"`
		HTTPS string `json:"https"`
	} `json:"links"`
}

func pushed(e *env, name string) (ctrl.User, bool) {
	for _, u := range e.agent.LastState().Users {
		if u.Name == name {
			return u, true
		}
	}
	return ctrl.User{}, false
}

func TestUsers(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	up := fmt.Sprintf("/api/servers/%d/users", id)

	var users []userJSON
	e.json("GET", up, nil, 200, &users)
	if len(users) != 1 || users[0].Name != "alice" || users[0].State != "active" || len(users[0].Secret) != 32 {
		t.Fatalf("users = %+v", users)
	}
	sec := "ee" + users[0].Secret + hex.EncodeToString([]byte(plugintest.Domains[0]))
	if want := "tg://proxy?server=127.0.0.1&port=443&secret=" + sec; users[0].Links.TG != want {
		t.Fatalf("tg link = %s, want %s", users[0].Links.TG, want)
	}
	if want := "https://t.me/proxy?server=127.0.0.1&port=443&secret=" + sec; users[0].Links.HTTPS != want {
		t.Fatalf("https link = %s", users[0].Links.HTTPS)
	}
	wait(t, func() bool { _, ok := pushed(e, "alice"); return ok }, "alice on the agent")

	var bob userJSON
	e.json("POST", up, map[string]any{"name": "bob", "maxIps": 2, "quotaBytes": 1 << 30}, 201, &bob)
	if !bob.Enabled || bob.MaxIPs != 2 || bob.QuotaBytes != 1<<30 || bob.ExpiresAt != nil || bob.CreatedAt == "" {
		t.Fatalf("bob = %+v", bob)
	}
	wait(t, func() bool { u, ok := pushed(e, "bob"); return ok && u.MaxIPs == 2 }, "bob on the agent")
	e.json("POST", up, map[string]any{"name": "bob"}, 409, nil)
	for _, bad := range []string{"", "has space", "..", strings.Repeat("x", 33)} {
		e.json("POST", up, map[string]any{"name": bad}, 400, nil)
	}
	e.json("POST", up, map[string]any{"name": "neg", "maxIps": -1}, 400, nil)

	bp := up + "/bob"
	var got userJSON
	e.json("PATCH", bp, map[string]any{"enabled": false}, 200, &got)
	if got.Enabled || got.State != "disabled" {
		t.Fatalf("disabled bob = %+v", got)
	}
	wait(t, func() bool { u, _ := pushed(e, "bob"); return !u.Enabled }, "bob disabled on the agent")

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	e.json("PATCH", bp, map[string]any{"enabled": true, "expiresAt": past}, 200, &got)
	if got.State != "expired" || got.ExpiresAt == nil {
		t.Fatalf("expired bob = %+v", got)
	}
	wait(t, func() bool { u, _ := pushed(e, "bob"); return u.Enabled && !u.ExpiresAt.IsZero() }, "bob's expiry on the agent")
	e.json("PATCH", bp, map[string]any{"maxIps": 5}, 200, &got)
	if got.ExpiresAt == nil || got.MaxIPs != 5 {
		t.Fatalf("an omitted expiry was cleared: %+v", got)
	}
	e.json("PATCH", bp, map[string]any{"expiresAt": nil}, 200, &got)
	if got.ExpiresAt != nil || got.State != "active" {
		t.Fatalf("a null expiry was kept: %+v", got)
	}

	old := got.Secret
	e.json("POST", bp+"/rotate-secret", nil, 200, &got)
	if got.Secret == old || len(got.Secret) != 32 || !strings.Contains(got.Links.TG, got.Secret) {
		t.Fatalf("rotated = %+v", got)
	}
	wait(t, func() bool { u, _ := pushed(e, "bob"); return u.Secret == got.Secret }, "the new secret on the agent")

	// usage comes from the agent's counters; a reset zeroes what we booked
	e.agent.Set(func(a *plugintest.Agent) { a.Users = []ctrl.UserStat{{Name: "bob", OctetsUp: 0}} })
	wait(t, func() bool { srv, _ := e.st.Get(uint(id)); _, ok := srv.Baseline["bob"]; return ok }, "bob's baseline")
	e.agent.Set(func(a *plugintest.Agent) { a.Users = []ctrl.UserStat{{Name: "bob", OctetsUp: 300, OctetsDown: 700}} })
	wait(t, func() bool { e.json("GET", up, nil, 200, &users); return len(users) == 2 && users[1].Up == 300 }, "bob's usage")
	if users[1].Down != 700 {
		t.Fatalf("bob = %+v", users[1])
	}
	e.json("POST", bp+"/reset-usage", nil, 200, &got)
	if got.Up != 0 || got.Down != 0 {
		t.Fatalf("reset = %+v", got)
	}

	e.json("PATCH", up+"/nobody", map[string]any{"enabled": false}, 404, nil)
	var okBody map[string]any
	e.json("DELETE", bp, nil, 200, &okBody)
	if okBody["ok"] != true {
		t.Fatalf("delete = %v", okBody)
	}
	wait(t, func() bool { _, ok := pushed(e, "bob"); return !ok }, "bob gone from the agent")
	e.json("DELETE", bp, nil, 404, nil)

	// settings: a new cover site is checked from the VPS first
	sp := fmt.Sprintf("/api/servers/%d/settings", id)
	e.agent.Set(func(a *plugintest.Agent) { a.CheckOK = false })
	code, body := e.do("PATCH", sp, map[string]any{"coverDomain": plugintest.Domains[1]})
	if code != 400 || !strings.Contains(string(body), "h2") {
		t.Fatalf("a failing cover site = %d %s", code, body)
	}
	e.agent.Set(func(a *plugintest.Agent) { a.CheckOK = true })
	var srv map[string]any
	e.json("PATCH", sp, map[string]any{"coverDomain": plugintest.Domains[1]}, 200, &srv)
	if srv["coverDomain"] != plugintest.Domains[1] {
		t.Fatalf("server = %v", srv)
	}
	wait(t, func() bool { return e.agent.LastState().Disguise == plugintest.Domains[1] }, "the new cover site on the agent")
	e.json("GET", up, nil, 200, &users)
	if !strings.HasSuffix(users[0].Links.TG, hex.EncodeToString([]byte(plugintest.Domains[1]))) {
		t.Fatalf("links still name the old cover site: %s", users[0].Links.TG)
	}
	e.json("PATCH", sp, map[string]any{"maxStreams": 50}, 400, nil)
	e.json("PATCH", sp, map[string]any{"maxStreams": 6500}, 409, nil)
	e.json("PATCH", sp, map[string]any{"maxStreams": 3000}, 200, &srv)
	if srv["maxStreams"] != 3000.0 {
		t.Fatalf("server = %v", srv)
	}
	wait(t, func() bool { return e.agent.LastState().MaxStreams == 3000 }, "the new stream cap on the agent")
	wait(t, func() bool {
		e.json("GET", fmt.Sprintf("/api/servers/%d", id), nil, 200, &srv)
		return srv["link"].(map[string]any)["state"] == "up"
	}, "the link back after the restart")
}

func TestRemoveWithTheLinkDown(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	sp := fmt.Sprintf("/api/servers/%d", id)
	e.agent.Stop()
	wait(t, func() bool {
		var s map[string]any
		e.json("GET", sp, nil, 200, &s)
		return s["link"].(map[string]any)["state"] == "down"
	}, "the link to drop")

	e.json("POST", sp+"/restart", nil, 503, nil)
	code, body := e.do("POST", sp+"/remove", map[string]any{})
	var refusal map[string]any
	json.Unmarshal(body, &refusal)
	if code != 409 || refusal["needLogin"] != true || refusal["error"] == "" {
		t.Fatalf("remove without a login = %d %s", code, body)
	}
	// a login is tried over SSH and never echoed
	var r struct {
		JobID string `json:"jobId"`
	}
	e.json("POST", sp+"/remove", map[string]any{"login": map[string]any{"sshUser": "root", "password": password, "privateKey": privKey, "passphrase": phrase}}, 202, &r)
	if job := e.waitJob(r.JobID); job["state"] != "ok" {
		t.Fatalf("remove with a login = %v", job)
	}
	if !locked(&e.rig.SSH.Mu, func() bool { return e.rig.SSH.Uninstalls == 1 }) {
		t.Fatal("the login was not used")
	}
	e.json("GET", sp, nil, 404, nil)
}

func TestForcedRemove(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	sp := fmt.Sprintf("/api/servers/%d", id)
	e.agent.Stop()
	wait(t, func() bool { s, _ := e.m.Snapshot(uint(id)); return s.Link.Up == 0 }, "the link to drop")
	var r struct {
		JobID string `json:"jobId"`
	}
	e.json("POST", sp+"/remove", map[string]any{"force": true}, 202, &r)
	if job := e.waitJob(r.JobID); job["state"] != "ok" {
		t.Fatalf("forced remove = %v", job)
	}
	e.json("GET", sp, nil, 404, nil)
}

func TestRepair(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	var r struct {
		JobID string `json:"jobId"`
	}
	e.json("POST", fmt.Sprintf("/api/servers/%d/repair", id), map[string]any{"sshPort": 22, "sshUser": "root", "password": password, "hostKey": "SHA256:abc"}, 202, &r)
	if job := e.waitJob(r.JobID); job["state"] != "ok" || job["kind"] != "repair" {
		t.Fatalf("repair = %v", job)
	}
	e.json("POST", fmt.Sprintf("/api/servers/%d/repair", id), map[string]any{"sshUser": "root"}, 400, nil)
}

func TestLimits(t *testing.T) {
	e := newEnv(t, store.Limits{MaxServers: 1})
	e.install()
	other := installBody()
	other["host"] = "127.0.0.2"
	code, body := e.do("POST", "/api/servers", other)
	if code != 409 || !strings.Contains(string(body), "at most 1 server") {
		t.Fatalf("a second server on a 1-server router = %d %s", code, body)
	}

	e2 := newEnv(t, store.Limits{})
	big := installBody()
	big["maxStreams"] = 5000
	e2.install2(big)
	other["maxStreams"] = 2000
	code, body = e2.do("POST", "/api/servers", other)
	if code != 409 || !strings.Contains(string(body), "stream") {
		t.Fatalf("past the stream budget = %d %s", code, body)
	}
}

func (e *env) install2(body map[string]any) {
	e.t.Helper()
	var r struct {
		JobID string `json:"jobId"`
	}
	e.json("POST", "/api/servers", body, 202, &r)
	if job := e.waitJob(r.JobID); job["state"] != "ok" {
		e.t.Fatalf("install: %v", job)
	}
}

func TestErrors(t *testing.T) {
	e := newEnv(t, store.Limits{})
	var msg map[string]any
	e.json("GET", "/api/servers/99", nil, 404, nil)
	e.json("GET", "/api/servers/zero", nil, 400, nil)
	e.json("GET", "/api/jobs/nope", nil, 404, nil)
	e.json("GET", "/api/jobs/nope/stream", nil, 404, nil)
	e.json("GET", "/api/servers/99/users", nil, 404, nil)
	e.json("POST", "/api/servers/99/rotate", nil, 404, nil)
	e.json("POST", "/api/servers", "{not json", 400, nil)
	e.json("GET", "/api/nothing-here", nil, 404, &msg)
	e.json("GET", base+"/api/nothing-here", nil, 404, &msg)
	code, body := e.do("GET", "/api/servers/99", nil)
	if json.Unmarshal(body, &msg); code != 404 || msg["error"] == "" {
		t.Fatalf("an error body = %s", body)
	}

	// a stored server whose agent cannot be reached
	id, _ := e.st.Create(store.NewServer{Name: "gone", Host: "127.0.0.1", MaxStreams: 100})
	rec := relayops.Record{NodeID: id, AgentID: "x", HubPort: 1, RealityPub: strings.Repeat("ab", 32), ShortID: "0102", Token: strings.Repeat("cd", 32), Disguise: "cover.example.com", MaxStreams: 100}
	e.st.SaveRecord(&rec)
	e.m.Ensure(context.Background(), id)
	e.json("POST", fmt.Sprintf("/api/servers/%d/restart", id), nil, 503, nil)
	e.json("POST", fmt.Sprintf("/api/servers/%d/rotate", id), nil, 503, nil)
	e.json("GET", fmt.Sprintf("/api/servers/%d/logs", id), nil, 503, nil)
	var s map[string]any
	e.json("GET", fmt.Sprintf("/api/servers/%d", id), nil, 200, &s)
	if s["agent"] != nil || s["link"].(map[string]any)["state"] != "down" {
		t.Fatalf("an unreachable server = %v", s)
	}
}

func TestAllowlistRefresh(t *testing.T) {
	e := newEnv(t, store.Limits{})
	var al map[string]any
	list := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			http.Error(w, "no", 500)
			return
		}
		io.WriteString(w, "198.51.100.0/24\n")
	}))
	defer list.Close()
	url := list.URL + "/bad"
	e.allow.URL = func() string { return url }
	e.allow.Client = func() *http.Client { return list.Client() }
	e.json("POST", "/api/allowlist/refresh", nil, 502, nil)
	url = list.URL + "/ok"
	e.json("POST", "/api/allowlist/refresh", nil, 200, &al)
	hasKeys(t, "allowlist", al, "prefixes", "placeholder", "refreshedAt", "lastError", "url")
	if al["prefixes"] != 2.0 || al["refreshedAt"] == nil || al["lastError"] != "" {
		t.Fatalf("after a refresh: %v", al)
	}
}

func TestAPendingRotationNeverLeavesTheRouter(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	seed := strings.Repeat("5e", 32)
	if err := e.st.Mutate(uint(id), func(s *store.Server) error { s.Record.NextSeed = seed; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/api/servers", fmt.Sprintf("/api/servers/%d", id), fmt.Sprintf("/api/servers/%d/users", id), "/api/config"} {
		code, body := e.do("GET", p, nil) // checked against every secret, the seed included
		if code != 200 || bytes.Contains(body, []byte(seed)) {
			t.Fatalf("%s = %d %s", p, code, body)
		}
	}
	if !locked(&e.mu, func() bool { return e.secrets[seed] }) {
		t.Fatal("the secret check does not know the seed")
	}
}

func TestASecondJobOnABusyServerIsAConflict(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	entered, hold := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.agent.Set(func(a *plugintest.Agent) {
		a.Gate = func(m string, _ json.RawMessage) {
			if m == ctrl.MKeysRotate {
				once.Do(func() { close(entered) })
				<-hold
			}
		}
	})
	defer func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	}()
	var r struct {
		JobID string `json:"jobId"`
	}
	e.json("POST", fmt.Sprintf("/api/servers/%d/rotate", id), nil, 202, &r)
	<-entered
	code, body := e.do("POST", fmt.Sprintf("/api/servers/%d/update", id), map[string]any{"kind": "telemt"})
	var msg map[string]any
	json.Unmarshal(body, &msg)
	if code != http.StatusConflict || msg["error"] != "another job is already running on this server: wait for it to finish" {
		t.Fatalf("a second job = %d %s", code, body)
	}
	close(hold)
	e.waitJob(r.JobID)
}

func TestTheViewSaysWhyTheProxyIsHeld(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	e.agent.Set(func(a *plugintest.Agent) { a.Running, a.Held = false, "waiting for the panel's first state" })
	var held any
	wait(t, func() bool {
		var s map[string]any
		e.json("GET", fmt.Sprintf("/api/servers/%d", id), nil, 200, &s)
		held = s["agent"].(map[string]any)["telemt"].(map[string]any)["held"]
		return held != nil && held != ""
	}, "the held reason")
	if held != "waiting for the router's first state" {
		t.Fatalf("held = %q", held)
	}
}
