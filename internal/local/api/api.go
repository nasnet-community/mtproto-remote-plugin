// Package api is Local MTProto's HTTP API. No response carries anything secret
// but a user's own proxy secret and links.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/local/proxy"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/local/router"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/local/store"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/userapi"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/users"
	"github.com/nasnet-community/mtproto-remote-plugin/telemt"
)

const checkLimit = 20 * time.Second

// Limits bound the connection limit the owner may set.
type Limits struct{ Default, Min, Max int }

type Config struct {
	Runner   *proxy.Runner
	Store    *store.Store
	Router   router.Channel
	Version  string
	Arch     string
	Build    string // how telemt was built: "" for telemt's own release
	Conns    Limits
	BasePath string       // "" or "/a/b", already normalised
	Index    http.Handler // the web app
	Log      *slog.Logger
	Now      func() time.Time
}

type API struct {
	cfg     Config
	started time.Time
}

// New serves every route at both / and BasePath/.
func New(cfg Config) http.Handler {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	a := &API{cfg: cfg, started: cfg.Now()}
	mux := http.NewServeMux()
	route := func(pattern string, h http.HandlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" "+path, h)
		if cfg.BasePath != "" {
			mux.HandleFunc(method+" "+cfg.BasePath+path, h)
		}
	}
	route("GET /health", userapi.Health)
	route("GET /api/config", a.config)
	route("GET /api/local", a.get)
	route("PATCH /api/local/settings", a.settings)
	route("POST /api/local/setup", a.setup)
	route("POST /api/local/new-port", a.newPort)
	route("POST /api/local/restart", a.restart)
	route("POST /api/local/proxy", a.proxy)
	route("POST /api/local/disguise-check", a.disguiseCheck)
	route("GET /api/local/logs", a.logs)
	route("GET /api/local/traffic", a.traffic)
	userapi.Handlers{Lookup: a.usersOf, Fail: a.fail, Now: cfg.Now, Here: "this proxy"}.Routes(route, "/api/local")
	noRoute := func(w http.ResponseWriter, _ *http.Request) {
		a.fail(w, &userapi.Error{Code: http.StatusNotFound, Msg: "no such route"})
	}
	mux.HandleFunc("/api/", noRoute)
	open := map[string]bool{"/": true, "/health": true}
	if b := cfg.BasePath; b != "" {
		mux.HandleFunc(b+"/api/", noRoute)
		open[b], open[b+"/"], open[b+"/health"] = true, true, true
	}
	if cfg.Index != nil {
		mux.Handle("GET /{$}", cfg.Index)
		if cfg.BasePath != "" {
			mux.Handle("GET "+cfg.BasePath+"/{$}", cfg.Index)
			mux.Handle("GET "+cfg.BasePath, cfg.Index)
		}
	}
	return userapi.SameSite(mux, open, map[string]bool{}, a.fail)
}

// the shared cover site check speaks of a VPS; here the router does the checking
var wording = strings.NewReplacer("from the VPS", "from the router", "the VPS", "the router")

func word(s string) string { return wording.Replace(s) }

func (a *API) fail(w http.ResponseWriter, err error) {
	if code, msg, ok := userapi.Known(err); ok {
		userapi.WriteJSON(w, code, map[string]any{"error": word(msg)})
		return
	}
	code, msg := http.StatusInternalServerError, err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		code, msg = http.StatusGatewayTimeout, "that took too long: try again"
	} else {
		a.cfg.Log.Error("request failed", "err", err)
	}
	userapi.WriteJSON(w, code, map[string]any{"error": msg})
}

func (a *API) config(w http.ResponseWriter, _ *http.Request) {
	userapi.WriteJSON(w, http.StatusOK, map[string]any{"version": a.cfg.Version, "flavor": "local"})
}

// --- the proxy's view ---

type proxyView struct {
	Running    bool      `json:"running"`
	Restarts   int       `json:"restarts"`
	UptimeSec  int64     `json:"uptimeSec"`
	LastExit   string    `json:"lastExit"`
	CrashLoop  bool      `json:"crashLoop"`
	Held       string    `json:"held"` // why it is kept off
	ApplyError string    `json:"applyError"`
	Version    string    `json:"version"`
	Build      string    `json:"build"`
	RSSBytes   uint64    `json:"rssBytes"`
	Port       int       `json:"port"` // inside the container
	ReportedAt time.Time `json:"reportedAt"`
}

type coverView struct {
	Domain    string     `json:"domain"`
	OK        bool       `json:"ok"`
	CheckedAt *time.Time `json:"checkedAt"`
	Error     string     `json:"error"`
}

type addressView struct {
	Host       string     `json:"host"` // what links use; "" while unknown
	Port       int        `json:"port"` // what links use; 0 while unknown
	CustomHost string     `json:"customHost"`
	CustomPort int        `json:"customPort"` // 0: the router's own port
	RouterPort int        `json:"routerPort"`
	CloudName  string     `json:"cloudName"`
	Public     string     `json:"publicAddress"`
	WAN        string     `json:"wanAddress"`
	Cloud      string     `json:"cloudStatus"`
	Domestic   bool       `json:"domestic"`
	Reported   bool       `json:"reported"` // the router script has written its state
	ReportedAt *time.Time `json:"reportedAt"`
	Asked      *time.Time `json:"newPortAskedAt"` // a new port the router hasn't taken yet
}

type connsView struct {
	Value   int `json:"value"`
	Default int `json:"default"`
	Min     int `json:"min"`
	Max     int `json:"max"`
}

type eventView struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
}

type view struct {
	Version      string              `json:"version"`
	Arch         string              `json:"arch"`
	CreatedAt    time.Time           `json:"createdAt"`
	CoverDomain  string              `json:"coverDomain"`
	UserCount    int                 `json:"userCount"`
	ProxyStopped bool                `json:"proxyStopped"`
	ProxyOptions userapi.OptionsView `json:"proxyOptions"`
	MaxConns     connsView           `json:"maxConns"`
	Proxy        proxyView           `json:"proxy"`
	Cover        coverView           `json:"cover"`
	Address      addressView         `json:"address"`
	Usage        userapi.UsageView   `json:"usage"`
	Events       []eventView         `json:"events"`
	UptimeSec    int64               `json:"uptimeSec"`
	RSSBytes     uint64              `json:"rssBytes"` // the plugin's own memory
}

// linkAddr is where links point: the address set by hand, else the router's IP
// Cloud name, else the public address IP Cloud sees.
func linkAddr(p store.Proxy, st router.State) (string, int) {
	host := p.LinkHost
	if host == "" {
		host = st.Address
	}
	if host == "" {
		host = st.Public
	}
	port := p.LinkPort
	if port == 0 {
		port = st.Port
	}
	return host, port
}

func (a *API) view(p store.Proxy) view {
	now := a.cfg.Now()
	st, reported, err := a.cfg.Router.Read()
	if err != nil {
		a.cfg.Log.Warn("the router's state could not be read", "err", err)
	}
	ps := a.cfg.Runner.Status()
	v := view{
		Version: a.cfg.Version, Arch: a.cfg.Arch, CreatedAt: p.CreatedAt.UTC(), CoverDomain: p.Cover, UserCount: len(p.Users),
		ProxyStopped: p.Stopped, ProxyOptions: userapi.OptionsOf(p.Options),
		MaxConns: connsView{Value: a.cfg.Runner.MaxConns(p), Default: a.cfg.Conns.Default, Min: a.cfg.Conns.Min, Max: a.cfg.Conns.Max},
		Proxy: proxyView{
			Running: ps.Running, Restarts: ps.Restarts, UptimeSec: int64(ps.Uptime.Seconds()), LastExit: ps.LastExit,
			CrashLoop: ps.CrashLoop, Held: ps.Held, ApplyError: ps.ApplyErr, Version: ps.Version, Build: a.cfg.Build,
			RSSBytes: ps.RSS, Port: telemtPort, ReportedAt: now.UTC(),
		},
		Cover: coverView{Domain: p.Cover},
		Usage: userapi.UsageOf(a.cfg.Runner.Usage(p)), Events: []eventView{},
		UptimeSec: int64(now.Sub(a.started).Seconds()), RSSBytes: selfRSS(),
	}
	if c := ps.Cover; c != nil {
		v.Cover.OK, v.Cover.CheckedAt, v.Cover.Error = c.OK, userapi.UTC(ps.CoverAt), word(c.Err)
	}
	host, port := linkAddr(p, st)
	v.Address = addressView{
		Host: host, Port: port, CustomHost: p.LinkHost, CustomPort: p.LinkPort, RouterPort: st.Port,
		CloudName: st.Address, Public: st.Public, WAN: st.WAN, Cloud: st.Cloud, Domestic: st.Domestic, Reported: reported,
	}
	if reported {
		v.Address.ReportedAt = userapi.UTC(st.At)
	}
	if at, ok := a.cfg.Router.Asked(); ok {
		v.Address.Asked = userapi.UTC(at)
	}
	for _, e := range a.cfg.Runner.Events() {
		v.Events = append(v.Events, eventView{At: e.At.UTC(), Kind: e.Kind, Detail: word(e.Detail)})
	}
	return v
}

// telemtPort is where telemt listens in the container; the router forwards its own port here.
const telemtPort = 8443

// selfRSS is the plugin's resident memory, from /proc.
func selfRSS() uint64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			if f := strings.Fields(v); len(f) > 0 {
				kb, _ := strconv.ParseUint(f[0], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}

func (a *API) get(w http.ResponseWriter, _ *http.Request) {
	userapi.WriteJSON(w, http.StatusOK, a.view(a.cfg.Store.Get()))
}

func (a *API) fresh(w http.ResponseWriter) { a.get(w, nil) }

// --- settings ---

// checkCover tests a site from the router before the proxy uses it.
func (a *API) checkCover(r *http.Request, domain string) (ctrl.DisguiseResult, error) {
	ctx, cancel := context.WithTimeout(r.Context(), checkLimit)
	defer cancel()
	res := a.cfg.Runner.CheckCover(ctx, domain)
	return res, ctx.Err()
}

func validHost(s string) bool {
	if ip := net.ParseIP(s); ip != nil {
		return ip.To4() != nil
	}
	return users.ValidDomain(s)
}

func (a *API) settings(w http.ResponseWriter, r *http.Request) {
	var b struct {
		CoverDomain  *string             `json:"coverDomain"`
		MaxConns     *int                `json:"maxConns"`
		ProxyOptions *users.OptionsPatch `json:"proxyOptions"`
		LinkHost     *string             `json:"linkHost"`
		LinkPort     *int                `json:"linkPort"`
	}
	if err := userapi.Decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	p := a.cfg.Store.Get()
	var cover string
	if b.CoverDomain != nil {
		cover = strings.ToLower(strings.TrimSpace(*b.CoverDomain))
		if !users.ValidDomain(cover) {
			a.fail(w, userapi.Bad("the cover site must be a domain name, like www.example.com"))
			return
		}
	}
	if b.MaxConns != nil && *b.MaxConns != 0 && (*b.MaxConns < a.cfg.Conns.Min || *b.MaxConns > a.cfg.Conns.Max) {
		a.fail(w, userapi.Bad("the connection limit is %d to %d", a.cfg.Conns.Min, a.cfg.Conns.Max))
		return
	}
	var host string
	if b.LinkHost != nil {
		host = strings.ToLower(strings.TrimSpace(*b.LinkHost))
		if host != "" && !validHost(host) {
			a.fail(w, userapi.Bad("the address must be a domain name or an IPv4 address, like home.example.com"))
			return
		}
	}
	if b.LinkPort != nil && (*b.LinkPort < 0 || *b.LinkPort > 65535) {
		a.fail(w, userapi.Bad("the port is 1 to 65535, or empty for the router's own"))
		return
	}
	mainCover := p.Cover
	if b.CoverDomain != nil {
		mainCover = cover
	}
	opts := p.Options
	if b.ProxyOptions != nil {
		var err error
		if opts, err = b.ProxyOptions.Apply(p.Options, mainCover); err != nil {
			a.fail(w, err)
			return
		}
	}
	if b.CoverDomain != nil && cover != p.Cover {
		if p.Cover == "" {
			a.fail(w, userapi.Conflict("set the proxy up first"))
			return
		}
		// the router has to reach the site before the proxy is told to use it
		res, err := a.checkCover(r, cover)
		if err != nil {
			a.fail(w, err)
			return
		}
		if !res.OK {
			a.fail(w, userapi.Bad("%s does not work as a cover site from this router: %s", cover, word(res.Err)))
			return
		}
	}
	// only what was sent: the check above is slow, and other saves may land meanwhile
	err := a.cfg.Store.Mutate(func(s *store.Proxy) error {
		if b.CoverDomain != nil {
			s.Cover = cover
		}
		if b.MaxConns != nil {
			s.MaxConns = *b.MaxConns
		}
		if b.ProxyOptions != nil {
			s.Options = opts
		}
		if b.LinkHost != nil {
			s.LinkHost = host
		}
		if b.LinkPort != nil {
			s.LinkPort = *b.LinkPort
		}
		return nil
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	if b.LinkHost != nil {
		if err := a.cfg.Router.SetHost(host); err != nil {
			a.cfg.Log.Warn("could not tell the router the address in links", "err", err)
		}
	}
	if b.CoverDomain != nil || b.MaxConns != nil || b.ProxyOptions != nil {
		a.cfg.Runner.Changed()
	}
	a.fresh(w)
}

// setup is the first run: a cover site and the first user, in one go.
func (a *API) setup(w http.ResponseWriter, r *http.Request) {
	var b struct {
		CoverDomain string `json:"coverDomain"`
		FirstUser   string `json:"firstUser"`
	}
	if err := userapi.Decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	cover := strings.ToLower(strings.TrimSpace(b.CoverDomain))
	name := strings.TrimSpace(b.FirstUser)
	switch {
	case a.cfg.Store.Get().Cover != "":
		a.fail(w, userapi.Conflict("the proxy is set up already"))
		return
	case !users.ValidDomain(cover):
		a.fail(w, userapi.Bad("the cover site must be a domain name, like www.example.com"))
		return
	case !users.ValidName(name):
		a.fail(w, userapi.Bad("a user name is 1 to 32 letters, digits, '.', '_' or '-'"))
		return
	}
	res, err := a.checkCover(r, cover)
	if err != nil {
		a.fail(w, err)
		return
	}
	if !res.OK {
		a.fail(w, userapi.Bad("%s does not work as a cover site from this router: %s", cover, word(res.Err)))
		return
	}
	sec, err := telemt.GenerateSecret()
	if err != nil {
		a.fail(w, err)
		return
	}
	err = a.cfg.Store.Mutate(func(s *store.Proxy) error {
		if s.Cover != "" {
			return userapi.Conflict("the proxy is set up already")
		}
		s.Cover = cover
		if !userIn(s.Users, name) {
			s.Users = append(s.Users, users.User{Name: name, Secret: sec, Enabled: true, CreatedAt: a.cfg.Now().UTC()})
		}
		return nil
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.cfg.Runner.Changed()
	a.fresh(w)
}

func userIn(us []users.User, name string) bool {
	for _, u := range us {
		if u.Name == name {
			return true
		}
	}
	return false
}

// newPort asks the router for another random port; its script moves the forward within a minute.
func (a *API) newPort(w http.ResponseWriter, _ *http.Request) {
	if st, ok, _ := a.cfg.Router.Read(); !ok || !st.Domestic {
		a.fail(w, userapi.Conflict("the router hasn't set the proxy's port up yet"))
		return
	}
	if err := a.cfg.Router.AskNewPort(); err != nil {
		a.fail(w, err)
		return
	}
	a.fresh(w)
}

func (a *API) restart(w http.ResponseWriter, _ *http.Request) {
	p := a.cfg.Store.Get()
	if st := a.cfg.Runner.Status(); st.Held != "" {
		a.fail(w, userapi.Conflict("the proxy isn't running: %s", st.Held))
		return
	} else if p.Stopped {
		a.fail(w, userapi.Conflict("start the proxy first"))
		return
	}
	a.cfg.Runner.Restart()
	userapi.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// proxy keeps the proxy off, or lets it run again.
func (a *API) proxy(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Stopped *bool `json:"stopped"`
	}
	if err := userapi.Decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if b.Stopped == nil {
		a.fail(w, userapi.Bad("say whether the proxy should be stopped"))
		return
	}
	if err := a.cfg.Store.Mutate(func(s *store.Proxy) error { s.Stopped = *b.Stopped; return nil }); err != nil {
		a.fail(w, err)
		return
	}
	a.cfg.Runner.Changed()
	a.fresh(w)
}

func (a *API) disguiseCheck(w http.ResponseWriter, r *http.Request) {
	var b ctrl.DisguiseCheck
	if err := userapi.Decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	b.Domain = strings.ToLower(strings.TrimSpace(b.Domain))
	if !users.ValidDomain(b.Domain) {
		a.fail(w, userapi.Bad("the cover site must be a domain name, like www.example.com"))
		return
	}
	res, err := a.checkCover(r, b.Domain)
	if err != nil {
		a.fail(w, err)
		return
	}
	userapi.WriteJSON(w, http.StatusOK, userapi.CoverJSON(res, word))
}

func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.URL.Query().Get("lines"))
	if err != nil || n <= 0 {
		n = 300
	}
	lines := a.cfg.Runner.Logs(min(n, 1000))
	if lines == nil {
		lines = []string{}
	}
	userapi.WriteJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (a *API) traffic(w http.ResponseWriter, _ *http.Request) {
	userapi.Traffic(w, a.cfg.Runner.Traffic(), a.cfg.Runner.StatsStep())
}

// --- users ---

// localProxy is the one proxy, for the users' routes both plugins share.
type localProxy struct {
	a *API
	p store.Proxy
}

func (a *API) usersOf(*http.Request) (userapi.Proxy, error) {
	p := a.cfg.Store.Get()
	if p.Cover == "" {
		return nil, userapi.Conflict("set the proxy up first")
	}
	return localProxy{a, p}, nil
}

func (l localProxy) Users() []users.User { return l.p.Users }

func (l localProxy) Edit(fn func(us *[]users.User, base map[string]users.Usage) error) error {
	return l.a.cfg.Store.Mutate(func(s *store.Proxy) error {
		if s.Baseline == nil {
			s.Baseline = map[string]users.Usage{}
		}
		return fn(&s.Users, s.Baseline)
	})
}

func (l localProxy) Links(secret string) userapi.LinksView {
	st, _, _ := l.a.cfg.Router.Read()
	host, port := linkAddr(l.p, st)
	return userapi.Links(host, port, l.p.Cover, l.p.Options.ExtraCoverNames, secret)
}

func (l localProxy) Usage() users.ServerUsage { return l.a.cfg.Runner.Usage(l.p) }
func (l localProxy) Changed()                 { l.a.cfg.Runner.Changed() }
func (l localProxy) Forget(name string)       { l.a.cfg.Runner.Forget(name) }
func (l localProxy) Flush() error             { return l.a.cfg.Runner.Flush() }
