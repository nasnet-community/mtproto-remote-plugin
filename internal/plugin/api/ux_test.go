package api

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/plugintest"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/store"
	"github.com/nasnet-community/mtproto-remote-plugin/release"
)

type liveUserJSON struct {
	Name        string  `json:"name"`
	State       string  `json:"state"`
	Enabled     bool    `json:"enabled"`
	Note        string  `json:"note"`
	Secret      string  `json:"secret"`
	Up          uint64  `json:"up"`
	Down        uint64  `json:"down"`
	Online      *bool   `json:"online"`
	Connections *int    `json:"connections"`
	Devices     *int    `json:"devices"`
	LastSeenAt  *string `json:"lastSeenAt"`
	TodayUp     uint64  `json:"todayUp"`
	TodayDown   uint64  `json:"todayDown"`
	MonthUp     uint64  `json:"monthUp"`
	MonthDown   uint64  `json:"monthDown"`
	ExpiresAt   *string `json:"expiresAt"`
	Links       struct {
		TG    string `json:"tg"`
		HTTPS string `json:"https"`
		Extra []struct {
			Domain string `json:"domain"`
			TG     string `json:"tg"`
			HTTPS  string `json:"https"`
		} `json:"extra"`
	} `json:"links"`
}

type serverJSON struct {
	ProxyStopped bool `json:"proxyStopped"`
	ProxyOptions struct {
		ExtraCoverNames  []string `json:"extraCoverNames"`
		DeviceWindowSecs int      `json:"deviceWindowSecs"`
		LogLevel         string   `json:"logLevel"`
	} `json:"proxyOptions"`
	Updates struct {
		Agent         bool   `json:"agent"`
		Telemt        bool   `json:"telemt"`
		AgentVersion  string `json:"agentVersion"`
		TelemtVersion string `json:"telemtVersion"`
	} `json:"updates"`
	Usage struct {
		Live        bool    `json:"live"`
		UsersOnline int     `json:"usersOnline"`
		Connections int     `json:"connections"`
		Devices     int     `json:"devices"`
		TodayUp     uint64  `json:"todayUp"`
		TodayDown   uint64  `json:"todayDown"`
		MonthUp     uint64  `json:"monthUp"`
		UpdatedAt   *string `json:"updatedAt"`
	} `json:"usage"`
}

func (e *env) server(id int) serverJSON {
	e.t.Helper()
	var s serverJSON
	e.json("GET", fmt.Sprintf("/api/servers/%d", id), nil, 200, &s)
	return s
}

func (e *env) users(id int) []liveUserJSON {
	e.t.Helper()
	var u []liveUserJSON
	e.json("GET", fmt.Sprintf("/api/servers/%d/users", id), nil, 200, &u)
	return u
}

func TestServerViewCarriesOptionsUpdatesAndUsage(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	s := e.server(id)
	if s.ProxyStopped || s.ProxyOptions.LogLevel != "normal" || s.ProxyOptions.ExtraCoverNames == nil || s.ProxyOptions.DeviceWindowSecs != 0 {
		t.Fatalf("defaults = %+v", s.ProxyOptions)
	}
	// the fake runs a proxy one behind the pin
	if !s.Updates.Telemt || s.Updates.TelemtVersion != release.TelemtVersion || s.Updates.AgentVersion != release.AgentVersion {
		t.Fatalf("updates = %+v", s.Updates)
	}
	// an agent that can't say who is online leaves it unknown
	e.agent.Set(func(a *plugintest.Agent) { a.Users = []ctrl.UserStat{{Name: "alice", OctetsUp: 10, OctetsDown: 20}} })
	wait(t, func() bool { return e.server(id).Usage.UpdatedAt != nil }, "a usage poll")
	if u := e.users(id)[0]; u.Online != nil || u.Connections != nil || e.server(id).Usage.Live {
		t.Fatalf("an old agent's user looks known: %+v", u)
	}

	// a current one says who is online, and traffic shows before it is on flash
	e.agent.Set(func(a *plugintest.Agent) {
		a.Live = true
		a.Users = []ctrl.UserStat{{Name: "alice", OctetsUp: 110, OctetsDown: 420, Connections: 3, Devices: 2}}
	})
	wait(t, func() bool { u := e.users(id)[0]; return u.Online != nil && *u.Online && u.Up == 100 }, "alice online with her traffic")
	u := e.users(id)[0]
	if *u.Connections != 3 || *u.Devices != 2 || u.Down != 400 || u.TodayUp != 100 || u.MonthDown != 400 || u.LastSeenAt == nil {
		t.Fatalf("alice = %+v", u)
	}
	s = e.server(id)
	if !s.Usage.Live || s.Usage.UsersOnline != 1 || s.Usage.Connections != 3 || s.Usage.Devices != 2 || s.Usage.TodayUp != 100 || s.Usage.MonthUp != 100 {
		t.Fatalf("usage = %+v", s.Usage)
	}
	var tr struct {
		StepSec int `json:"stepSec"`
		Samples []struct {
			Up   int64 `json:"up"`
			Down int64 `json:"down"`
		} `json:"samples"`
	}
	e.json("GET", fmt.Sprintf("/api/servers/%d/traffic", id), nil, 200, &tr)
	if tr.StepSec <= 0 || len(tr.Samples) == 0 {
		t.Fatalf("traffic = %+v", tr)
	}
	e.json("GET", "/api/servers/999/traffic", nil, 404, nil)
}

func TestNotesExistingSecretsAndExtraLinks(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	up := fmt.Sprintf("/api/servers/%d/users", id)

	var sam liveUserJSON
	e.json("POST", up, map[string]any{"name": "sam", "note": "  Sam's   phone ", "secret": "dd" + strings.Repeat("ab", 16)}, 201, &sam)
	if sam.Note != "Sam's phone" || sam.Secret != strings.Repeat("ab", 16) {
		t.Fatalf("sam = %+v", sam)
	}
	// an ee secret carries its old cover name; only the secret is kept
	e.json("POST", up, map[string]any{"name": "tom", "secret": "ee" + strings.Repeat("cd", 16) + hex.EncodeToString([]byte("old.example.net"))}, 201, nil)
	e.json("POST", up, map[string]any{"name": "dup", "secret": strings.Repeat("ab", 16)}, 409, nil)
	e.json("POST", up, map[string]any{"name": "bad", "secret": "not-hex"}, 400, nil)
	e.json("POST", up, map[string]any{"name": "long", "note": strings.Repeat("x", 81)}, 400, nil)
	e.json("PATCH", up+"/sam", map[string]any{"note": "Sam, upstairs"}, 200, &sam)
	if sam.Note != "Sam, upstairs" {
		t.Fatalf("note = %q", sam.Note)
	}

	sp := fmt.Sprintf("/api/servers/%d/settings", id)
	for _, bad := range []map[string]any{
		{"extraCoverNames": []string{plugintest.Domains[0]}}, // the cover site itself
		{"extraCoverNames": []string{"a.example.com", "a.example.com"}},
		{"extraCoverNames": []string{"a.example.com", "b.example.com", "c.example.com", "d.example.com", "e.example.com"}},
		{"extraCoverNames": []string{"not a domain"}},
		{"deviceWindowSecs": -1},
		{"logLevel": "loud"},
	} {
		e.json("PATCH", sp, map[string]any{"proxyOptions": bad}, 400, nil)
	}
	before := e.agent.LastState().Rev
	e.json("PATCH", sp, map[string]any{"proxyOptions": map[string]any{"extraCoverNames": []string{"CDN.Example.com"}, "deviceWindowSecs": 60, "logLevel": "verbose"}}, 200, nil)
	wait(t, func() bool { return e.agent.LastState().Rev != before }, "the options on the agent")
	var set map[string]any
	b, _ := json.Marshal(e.agent.LastState().Telemt)
	json.Unmarshal(b, &set)
	if set["log_level"] != "verbose" || set["unique_ip_window_secs"] != float64(60) || fmt.Sprint(set["tls_domains_extra"]) != "[cdn.example.com]" {
		t.Fatalf("telemt settings = %v", set)
	}
	for _, u := range e.users(id) {
		if len(u.Links.Extra) != 1 || u.Links.Extra[0].Domain != "cdn.example.com" ||
			!strings.HasSuffix(u.Links.Extra[0].TG, hex.EncodeToString([]byte("cdn.example.com"))) {
			t.Fatalf("%s's extra links = %+v", u.Name, u.Links.Extra)
		}
	}
	// back to the defaults: the agent's settings are as an untouched server's again
	e.json("PATCH", sp, map[string]any{"proxyOptions": map[string]any{"extraCoverNames": []string{}, "deviceWindowSecs": 0, "logLevel": "normal"}}, 200, nil)
	srv, _ := e.st.Get(uint(id))
	if b, _ := json.Marshal(e.m.Desired(srv).Telemt); !strings.Contains(string(b), `"log_level":"normal"`) || strings.Contains(string(b), "tls_domains_extra") || strings.Contains(string(b), "unique_ip") {
		t.Fatalf("defaults left keys behind: %s", b)
	}
}

func TestStopAndStartTheProxy(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	pp := fmt.Sprintf("/api/servers/%d/proxy", id)
	var s serverJSON
	e.json("POST", pp, map[string]any{"stopped": true}, 200, &s)
	if !s.ProxyStopped {
		t.Fatal("the server does not say it is stopped")
	}
	wait(t, func() bool { return e.agent.LastState().Stopped }, "the agent told to keep the proxy off")
	e.json("POST", pp, map[string]any{}, 400, nil)
	e.json("POST", pp, map[string]any{"stopped": false}, 200, &s)
	wait(t, func() bool { return !e.agent.LastState().Stopped }, "the proxy allowed again")
}

func TestBulkActions(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	up := fmt.Sprintf("/api/servers/%d/users", id)
	soon := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	past := time.Now().UTC().Add(-48 * time.Hour)
	e.json("POST", up, map[string]any{"name": "bob", "expiresAt": soon}, 201, nil)
	e.json("POST", up, map[string]any{"name": "carol", "expiresAt": past}, 201, nil)
	e.json("POST", up, map[string]any{"name": "dave"}, 201, nil)
	bp := up + "/bulk"
	var res struct {
		Changed int `json:"changed"`
	}
	e.json("POST", bp, map[string]any{"action": "pause", "names": []string{"bob", "dave", "nobody"}}, 200, &res)
	if res.Changed != 2 {
		t.Fatalf("paused %d", res.Changed)
	}
	wait(t, func() bool { u, _ := pushed(e, "dave"); return !u.Enabled }, "dave paused on the agent")
	e.json("POST", bp, map[string]any{"action": "pause", "names": []string{"bob"}}, 200, &res)
	if res.Changed != 0 {
		t.Fatalf("pausing a paused user changed %d", res.Changed)
	}
	e.json("POST", bp, map[string]any{"action": "resume", "names": []string{"bob", "dave"}}, 200, &res)
	if res.Changed != 2 {
		t.Fatalf("resumed %d", res.Changed)
	}

	e.json("POST", bp, map[string]any{"action": "renew", "names": []string{"bob", "carol", "dave"}}, 200, &res)
	if res.Changed != 2 {
		t.Fatalf("renewed %d: a user who never expires has nothing to renew", res.Changed)
	}
	byName := map[string]liveUserJSON{}
	for _, u := range e.users(id) {
		byName[u.Name] = u
	}
	if got := *byName["bob"].ExpiresAt; got != soon.AddDate(0, 0, 30).Format(time.RFC3339) {
		t.Errorf("bob's expiry moved from the old date: %s", got)
	}
	carol, _ := time.Parse(time.RFC3339, *byName["carol"].ExpiresAt)
	if want := time.Now().UTC().AddDate(0, 0, 30); carol.Format(time.DateOnly) != want.Format(time.DateOnly) || carol.Hour() != 23 || byName["carol"].State != "active" {
		t.Errorf("carol's expiry = %s, state %s", carol, byName["carol"].State)
	}
	if byName["dave"].ExpiresAt != nil {
		t.Error("dave gained an expiry")
	}

	e.json("POST", bp, map[string]any{"action": "delete", "names": []string{"bob", "carol"}}, 200, &res)
	if res.Changed != 2 || len(e.users(id)) != 2 {
		t.Fatalf("deleted %d, left %d", res.Changed, len(e.users(id)))
	}
	e.json("POST", bp, map[string]any{"action": "explode", "names": []string{"dave"}}, 400, nil)
	e.json("POST", bp, map[string]any{"action": "pause", "names": []string{}}, 400, nil)
}

func TestJobsListAndLinkPortChecks(t *testing.T) {
	e := newEnv(t, store.Limits{})
	id := e.install()
	var jobs []struct {
		Kind     string `json:"kind"`
		State    string `json:"state"`
		Target   string `json:"target"`
		ServerID int    `json:"serverId"`
		Steps    []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"steps"`
	}
	e.json("GET", "/api/jobs", nil, 200, &jobs)
	if len(jobs) != 1 || jobs[0].Kind != "install" || jobs[0].State != "ok" || jobs[0].Target != "Relay 1" || jobs[0].ServerID != id {
		t.Fatalf("jobs = %+v", jobs)
	}
	for _, st := range jobs[0].Steps {
		if st.State == "pending" {
			t.Errorf("a finished install still lists %s as pending", st.Name)
		}
	}
	lp := fmt.Sprintf("/api/servers/%d/link-port", id)
	srv, _ := e.st.Get(uint(id))
	e.json("POST", lp, map[string]any{"port": srv.Record.HubPort}, 400, nil)
	e.json("POST", lp, map[string]any{"port": 0}, 400, nil)
}

func TestAFailedAllowlistRefreshCarriesTheList(t *testing.T) {
	e := newEnv(t, store.Limits{})
	list := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }))
	defer list.Close()
	e.allow.URL = func() string { return list.URL }
	e.allow.Client = func() *http.Client { return list.Client() }
	code, body := e.do("POST", "/api/allowlist/refresh", nil)
	var got struct {
		Error     string         `json:"error"`
		Allowlist map[string]any `json:"allowlist"`
	}
	json.Unmarshal(body, &got)
	if code != 502 || got.Error == "" || got.Allowlist == nil || got.Allowlist["prefixes"] != float64(1) {
		t.Fatalf("refresh = %d %s", code, body)
	}
}

func TestInstallTakesItsPorts(t *testing.T) {
	e := newEnv(t, store.Limits{})
	b := installBody()
	b["hubPort"], b["capturePort"] = 8443, 8443
	e.json("POST", "/api/servers", b, 400, nil)
	b["hubPort"], b["capturePort"] = 22, 9443
	e.json("POST", "/api/servers", b, 400, nil)
}

func TestHumanWordsServerNotNode(t *testing.T) {
	for in, want := range map[string]string{
		"relay: invalid request: port 22 is the node's SSH port":                 "port 22 is the server's SSH port",
		"relay: the node's link is down":                                         "the server's link is down",
		"the panel could not reach the node on port 18446, so it stays on 18444": "the router could not reach the server on port 18446, so it stays on 18444",
	} {
		if got := human(in); got != want {
			t.Errorf("human(%q) = %q, want %q", in, got, want)
		}
	}
}
