// Package api is the plugin's HTTP API. No response carries a link secret or
// an SSH login; a user's own proxy secret and links are the one exception.
package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/hubnode"
	"github.com/nasnet-community/mtproto-remote-plugin/installer"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/manager"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/store"
	"github.com/nasnet-community/mtproto-remote-plugin/link"
	"github.com/nasnet-community/mtproto-remote-plugin/mux"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops"
	"github.com/nasnet-community/mtproto-remote-plugin/release"
	"github.com/nasnet-community/mtproto-remote-plugin/telemt"
	"github.com/nasnet-community/mtproto-remote-plugin/tgnet"
)

const (
	maxUsers  = 500 // per server, the agent's design envelope
	callLimit = 20 * time.Second
	// UIHeader is what the page sends on every API call.
	UIHeader = "X-MTProto-UI"
)

type Config struct {
	Svc      *relayops.Service
	Manager  *manager.Manager
	Store    *store.Store
	Allow    *tgnet.Refresher
	AllowURL string // shown in the UI; "" when no refresh is configured
	Version  string
	BasePath string       // "" or "/a/b", already normalised
	Index    http.Handler // the web app
	Log      *slog.Logger
	Now      func() time.Time
}

type API struct{ cfg Config }

// New serves every route at both / and BasePath/.
func New(cfg Config) http.Handler {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	a := &API{cfg: cfg}
	mux := http.NewServeMux()
	route := func(pattern string, h http.HandlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" "+path, h)
		if cfg.BasePath != "" {
			mux.HandleFunc(method+" "+cfg.BasePath+path, h)
		}
	}
	route("GET /health", a.health)
	route("GET /api/config", a.config)
	route("POST /api/probe", a.probe)
	route("POST /api/servers", a.install)
	route("GET /api/jobs", a.jobs)
	route("GET /api/jobs/{job}", a.job)
	route("GET /api/jobs/{job}/stream", a.jobStream)
	route("GET /api/servers", a.servers)
	route("GET /api/servers/{id}", a.server)
	route("PATCH /api/servers/{id}", a.rename)
	route("PATCH /api/servers/{id}/settings", a.settings)
	route("POST /api/servers/{id}/repair", a.repair)
	route("POST /api/servers/{id}/remove", a.remove)
	route("POST /api/servers/{id}/update", a.update)
	route("POST /api/servers/{id}/rotate", a.rotate)
	route("POST /api/servers/{id}/restart", a.restart)
	route("POST /api/servers/{id}/disguise-check", a.disguiseCheck)
	route("POST /api/servers/{id}/proxy", a.proxy)
	route("POST /api/servers/{id}/link-port", a.linkPort)
	route("GET /api/servers/{id}/logs", a.logs)
	route("GET /api/servers/{id}/traffic", a.traffic)
	route("GET /api/servers/{id}/users", a.users)
	route("POST /api/servers/{id}/users", a.createUser)
	route("POST /api/servers/{id}/users/bulk", a.bulk)
	route("PATCH /api/servers/{id}/users/{name}", a.updateUser)
	route("DELETE /api/servers/{id}/users/{name}", a.deleteUser)
	route("POST /api/servers/{id}/users/{name}/rotate-secret", a.rotateSecret)
	route("POST /api/servers/{id}/users/{name}/reset-usage", a.resetUsage)
	route("POST /api/allowlist/refresh", a.refreshAllowlist)
	noRoute := func(w http.ResponseWriter, _ *http.Request) {
		a.fail(w, &httpError{http.StatusNotFound, "no such route"})
	}
	mux.HandleFunc("/api/", noRoute)
	if cfg.BasePath != "" {
		mux.HandleFunc(cfg.BasePath+"/api/", noRoute)
	}
	if cfg.Index != nil {
		mux.Handle("GET /{$}", cfg.Index)
		if cfg.BasePath != "" {
			mux.Handle("GET "+cfg.BasePath+"/{$}", cfg.Index)
			mux.Handle("GET "+cfg.BasePath, cfg.Index)
		}
	}
	return a.sameSite(mux)
}

// sameSite keeps other sites' pages off the API: nothing here asks who you are.
func (a *API) sameSite(mux *http.ServeMux) http.Handler {
	open := map[string]bool{"/": true, "/health": true}
	stream := map[string]bool{"GET /api/jobs/{job}/stream": true}
	if b := a.cfg.BasePath; b != "" {
		open[b], open[b+"/"], open[b+"/health"] = true, true, true
		stream["GET "+b+"/api/jobs/{job}/stream"] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := (r.Method == http.MethodGet || r.Method == http.MethodHead) && open[r.URL.Path]
		if !page && !fromThisSite(r) {
			a.fail(w, &httpError{http.StatusForbidden, "requests from other sites are refused"})
			return
		}
		// no Sec-Fetch-Site: only the page sends UIHeader (EventSource can't, and a job holds no secrets)
		if !page && r.Header.Get("Sec-Fetch-Site") == "" && r.Header.Get(UIHeader) == "" {
			if _, pat := mux.Handler(r); r.Method != http.MethodGet || !stream[pat] {
				a.fail(w, &httpError{http.StatusForbidden, "requests from other sites are refused"})
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// fromThisSite relies on Sec-Fetch-Site; browsers without it are left to the UIHeader check.
func fromThisSite(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "":
	default:
		return false
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	// the panel rewrites Host and sets no X-Forwarded-Host, so usually there is nothing to compare
	fwd, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ",")
	origin := r.Header.Get("Origin")
	if fwd = strings.TrimSpace(fwd); fwd == "" || origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, fwd)
}

// --- plumbing ---

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func bad(format string, a ...any) error {
	return &httpError{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}
func conflict(format string, a ...any) error {
	return &httpError{http.StatusConflict, fmt.Sprintf(format, a...)}
}

var errNoServer = &httpError{http.StatusNotFound, "no such server"}

// noise are the shared packages' "pkg: " prefixes, which mean nothing to a user.
var noise = map[string]bool{
	"relay": true, "invalid request": true, "installer": true, "relayops": true,
	"hubnode": true, "link": true, "mux": true, "ctrl": true, "store": true,
}

// the shared packages run in a panel too; here the router is the one that connects
var wording = strings.NewReplacer("disguise", "cover site", "Disguise", "Cover site", "node's", "server's", "the node", "the server", "this node", "this server",
	"node ", "server ", "Node ", "Server ", "the panel", "the router", "The panel", "The router")

// human puts a shared package's error in the UI's words.
func human(s string) string {
	parts := strings.Split(s, ": ")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if !noise[p] {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		kept = parts[len(parts)-1:]
	}
	return wording.Replace(strings.Join(kept, ": "))
}

// dropped is how a call ends when its link goes away under it: a restart or a flap.
func dropped(err error) bool {
	for _, e := range []error{
		mux.ErrSessionClosed, mux.ErrGoAway, mux.ErrStreamReset, mux.ErrTimeout,
		io.EOF, io.ErrUnexpectedEOF, net.ErrClosed, syscall.ECONNRESET, syscall.EPIPE,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *API) fail(w http.ResponseWriter, err error) {
	var he *httpError
	var ce *ctrl.Error
	var cover *relayops.CoverError
	code := http.StatusInternalServerError
	body := map[string]any{"error": human(err.Error())}
	switch {
	case errors.As(err, &he):
		code = he.code
	case errors.As(err, &cover):
		code, body["cover"] = http.StatusUnprocessableEntity, coverJSON(cover.Result)
	case errors.Is(err, relayops.ErrVPS):
		code = http.StatusBadGateway
	case errors.Is(err, relayops.ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, relayops.ErrNotFound), errors.Is(err, store.ErrNotFound):
		code, body["error"] = http.StatusNotFound, "no such server"
	case errors.Is(err, relayops.ErrNeedLogin):
		code, body["needLogin"] = http.StatusConflict, true
	case errors.Is(err, relayops.ErrBusy):
		code, body["error"] = http.StatusConflict, "another job is already running on this server: wait for it to finish"
	case errors.Is(err, relayops.ErrUpToDate):
		code, body["error"] = http.StatusConflict, "this server already runs that version"
	case errors.Is(err, store.ErrServerCap), errors.Is(err, store.ErrStreamBudget),
		errors.Is(err, manager.ErrBusy), errors.Is(err, manager.ErrHostTaken), errors.Is(err, relayops.ErrUnknownArch):
		code = http.StatusConflict
	case errors.Is(err, relayops.ErrNotConnected), errors.Is(err, manager.ErrNotRunning), errors.Is(err, link.ErrNoLink):
		code, body["error"] = http.StatusServiceUnavailable, "the link to this server is down"
	case errors.As(err, &ce):
		code = http.StatusBadGateway
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		code, body["error"] = http.StatusBadGateway, "the agent did not answer in time"
	case dropped(err):
		code, body["error"] = http.StatusServiceUnavailable, "the link to this server dropped: try again in a moment"
	case errors.Is(err, mux.ErrTooManyOpen), errors.Is(err, link.ErrBusy):
		code, body["error"] = http.StatusServiceUnavailable, "the link to this server is busy: try again in a moment"
	case errors.Is(err, context.Canceled):
		code, body["error"] = http.StatusServiceUnavailable, "the request was cancelled"
	}
	if code == http.StatusInternalServerError {
		a.cfg.Log.Error("request failed", "err", err)
	}
	writeJSON(w, code, body)
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return bad("malformed request body")
	}
	return nil
}

func serverID(r *http.Request) (uint, error) {
	n, err := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err != nil || n == 0 {
		return 0, bad("bad server id")
	}
	return uint(n), nil
}

func (a *API) lookup(r *http.Request) (store.Server, error) {
	id, err := serverID(r)
	if err != nil {
		return store.Server{}, err
	}
	srv, ok := a.cfg.Store.Get(id)
	if !ok {
		return store.Server{}, errNoServer
	}
	return srv, nil
}

func (a *API) call(r *http.Request, id uint, method string, req, resp any) error {
	ctx, cancel := context.WithTimeout(r.Context(), callLimit)
	defer cancel()
	return a.cfg.Manager.Call(ctx, id, method, req, resp)
}

func utc(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// --- config, probe, install, jobs ---

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ok")
}

type allowlistView struct {
	Prefixes    int        `json:"prefixes"`
	Placeholder bool       `json:"placeholder"`
	RefreshedAt *time.Time `json:"refreshedAt"`
	LastError   string     `json:"lastError"`
	URL         string     `json:"url"`
}

func (a *API) allowURL() string {
	if a.cfg.Allow.URL != nil {
		return a.cfg.Allow.URL()
	}
	return a.cfg.AllowURL
}

func (a *API) allowlist() allowlistView {
	ok, lastErr := a.cfg.Allow.Status()
	return allowlistView{
		Prefixes: len(a.cfg.Allow.List.Prefixes()), Placeholder: tgnet.PinnedIsPlaceholder(),
		RefreshedAt: utc(ok), LastError: lastErr, URL: a.allowURL(),
	}
}

func (a *API) config(w http.ResponseWriter, _ *http.Request) {
	lim := a.cfg.Store.Limits()
	writeJSON(w, http.StatusOK, map[string]any{
		"version": a.cfg.Version, "maxServers": lim.MaxServers, "serverCount": len(a.cfg.Store.List()),
		"streamBudget": lim.StreamBudget, "streamsAllocated": a.cfg.Manager.Allocated(),
		"defaultMaxStreams": store.DefaultMaxStreams, "allowlist": a.allowlist(),
	})
}

func (a *API) refreshAllowlist(w http.ResponseWriter, r *http.Request) {
	if a.allowURL() == "" {
		a.fail(w, conflict("no refresh URL is configured"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := a.cfg.Allow.RefreshNow(ctx); err != nil {
		// the list as it stands, so the page can show the failure where the list is
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "the list could not be refreshed: " + err.Error(), "allowlist": a.allowlist()})
		return
	}
	writeJSON(w, http.StatusOK, a.allowlist())
}

func sshPort(p int) int {
	if p == 0 {
		return 22
	}
	return p
}

func (a *API) probe(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Host    string `json:"host"`
		SSHPort int    `json:"sshPort"`
		SSHUser string `json:"sshUser"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	host, port := strings.TrimSpace(b.Host), sshPort(b.SSHPort)
	ctx, cancel := context.WithTimeout(r.Context(), callLimit)
	defer cancel()
	fp, err := a.cfg.Svc.Fingerprint(ctx, host, port, b.SSHUser)
	if err != nil {
		if !errors.Is(err, relayops.ErrInvalid) {
			err = &httpError{http.StatusBadGateway, "could not reach the SSH server: " + err.Error()}
		}
		a.fail(w, err)
		return
	}
	status := "new"
	for _, s := range a.cfg.Store.List() {
		if s.Host != host || sshPort(s.Record.SSHPort) != port || s.Record.HostKey == "" {
			continue
		}
		if s.Record.HostKey == fp {
			status = "known"
			break
		}
		status = "changed"
	}
	writeJSON(w, http.StatusOK, map[string]any{"fingerprint": fp, "status": status})
}

type loginBody struct {
	SSHPort    int    `json:"sshPort"`
	SSHUser    string `json:"sshUser"`
	Password   string `json:"password"`
	PrivateKey string `json:"privateKey"`
	Passphrase string `json:"passphrase"`
	HostKey    string `json:"hostKey"`
}

func (b loginBody) login(host string) relayops.Login {
	l := relayops.Login{Host: host, Port: b.SSHPort, User: b.SSHUser, Password: b.Password, HostKey: b.HostKey}
	if b.PrivateKey != "" {
		l.Key = []byte(b.PrivateKey)
	}
	if b.Passphrase != "" {
		l.Passphrase = []byte(b.Passphrase)
	}
	return l
}

func (b loginBody) empty() bool { return b.Password == "" && b.PrivateKey == "" }

func (a *API) install(w http.ResponseWriter, r *http.Request) {
	var b struct {
		loginBody
		Name                 string `json:"name"`
		Host                 string `json:"host"`
		CoverDomain          string `json:"coverDomain"`
		MaxStreams           int    `json:"maxStreams"`
		FirstUser            string `json:"firstUser"`
		AllowUnverifiedCover bool   `json:"allowUnverifiedCover"`
		HubPort              int    `json:"hubPort"`
		CapturePort          int    `json:"capturePort"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	host := strings.TrimSpace(b.Host)
	cover := strings.ToLower(strings.TrimSpace(b.CoverDomain))
	if !relayops.ValidDomain(cover) {
		a.fail(w, bad("the cover site must be a domain name, like www.example.com"))
		return
	}
	if b.MaxStreams == 0 {
		b.MaxStreams = store.DefaultMaxStreams
	}
	if b.MaxStreams < 100 {
		a.fail(w, bad("max streams must be at least 100"))
		return
	}
	first := strings.TrimSpace(b.FirstUser)
	if first != "" && !validName(first) {
		a.fail(w, bad("a user name is 1 to 32 letters, digits, '.', '_' or '-'"))
		return
	}
	if b.empty() {
		a.fail(w, bad("give a password or a private key"))
		return
	}
	release, err := a.cfg.Manager.Reserve(manager.Install{Host: host, MaxStreams: b.MaxStreams, FirstUser: first})
	if err != nil {
		a.fail(w, err)
		return
	}
	id, err := a.cfg.Svc.StartInstall(r.Context(), relayops.InstallParams{
		Name: b.Name, Login: b.login(host), Disguise: cover, MaxStreams: b.MaxStreams,
		HubPort: b.HubPort, CapturePort: b.CapturePort, AllowUnverifiedDisguise: b.AllowUnverifiedCover,
	})
	if err != nil {
		release()
		a.fail(w, err)
		return
	}
	go a.releaseWhenDone(id, release)
	writeJSON(w, http.StatusAccepted, map[string]any{"jobId": id})
}

// releaseWhenDone gives back an install's reservation if it never registered.
func (a *API) releaseWhenDone(id string, release func()) {
	defer release()
	for end := time.Now().Add(2 * time.Hour); time.Now().Before(end); time.Sleep(time.Second) {
		if v, ok := a.cfg.Svc.Job(id); !ok || v.State != relayops.JobRunning {
			return
		}
	}
}

type jobView struct {
	ID         string           `json:"id"`
	Kind       string           `json:"kind"`
	State      string           `json:"state"`
	Steps      []installer.Step `json:"steps"`
	Error      string           `json:"error,omitempty"`
	Target     string           `json:"target"`
	ServerID   uint             `json:"serverId,omitempty"`
	StartedAt  time.Time        `json:"startedAt"`
	FinishedAt *time.Time       `json:"finishedAt,omitempty"`
}

func jobOf(v relayops.JobView) jobView {
	out := jobView{
		ID: v.ID, Kind: v.Kind, State: string(v.State), Error: human(v.Error),
		Target: v.Target, ServerID: v.NodeID, StartedAt: v.StartedAt.UTC(),
	}
	if v.FinishedAt != nil {
		out.FinishedAt = utc(*v.FinishedAt)
	}
	out.Steps = make([]installer.Step, len(v.Steps))
	for i, st := range v.Steps {
		st.Detail = human(st.Detail)
		out.Steps[i] = st
	}
	return out
}

// jobs is the job tray: what runs now and what ended in the last hour.
func (a *API) jobs(w http.ResponseWriter, _ *http.Request) {
	out := []jobView{}
	for _, v := range a.cfg.Svc.Jobs(20) {
		j := jobOf(v)
		if j.Target == "" && j.ServerID != 0 {
			if srv, ok := a.cfg.Store.Get(j.ServerID); ok {
				j.Target = srv.Name
			}
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) job(w http.ResponseWriter, r *http.Request) {
	v, ok := a.cfg.Svc.Job(r.PathValue("job"))
	if !ok {
		a.fail(w, &httpError{http.StatusNotFound, "no such job"})
		return
	}
	writeJSON(w, http.StatusOK, jobOf(v))
}

// jobStream sends the job on every change until it ends, final state included.
func (a *API) jobStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("job")
	if _, ok := a.cfg.Svc.Job(id); !ok {
		a.fail(w, &httpError{http.StatusNotFound, "no such job"})
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	keep := time.NewTicker(15 * time.Second)
	defer keep.Stop()
	var last []byte
	for {
		v, ok := a.cfg.Svc.Job(id)
		if !ok {
			return
		}
		b, _ := json.Marshal(jobOf(v))
		if !bytes.Equal(b, last) {
			last = b
			fmt.Fprintf(w, "data: %s\n\n", b)
			rc.Flush()
		}
		if v.State != relayops.JobRunning {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-keep.C:
			io.WriteString(w, ": keepalive\n\n")
			rc.Flush()
		case <-tick.C:
		}
	}
}

// --- servers ---

type connView struct {
	Name      string     `json:"name"`
	Up        bool       `json:"up"`
	Since     *time.Time `json:"since"`
	RTTms     int64      `json:"rttMs"`
	Streams   int        `json:"streams"`
	LastError string     `json:"lastError"`
}

type linkView struct {
	State        string     `json:"state"`
	Up           int        `json:"up"`
	Total        int        `json:"total"`
	Streams      int64      `json:"streams"`
	Relayed      uint64     `json:"relayed"`
	DialFailures uint64     `json:"dialFailures"`
	Violations   uint64     `json:"violations"`
	BytesToDC    uint64     `json:"bytesToDc"`
	BytesFromDC  uint64     `json:"bytesFromDc"`
	Conns        []connView `json:"conns"`
}

type agentView struct {
	Version       string    `json:"version"`
	TelemtVersion string    `json:"telemtVersion"`
	UptimeSec     int64     `json:"uptimeSec"`
	RSSBytes      uint64    `json:"rssBytes"`
	StateRev      string    `json:"stateRev"`
	ReportedAt    time.Time `json:"reportedAt"`
	Telemt        struct {
		Running   bool   `json:"running"`
		Restarts  int    `json:"restarts"`
		UptimeSec int64  `json:"uptimeSec"`
		LastExit  string `json:"lastExit"`
		CrashLoop bool   `json:"crashLoop"`
		Held      string `json:"held"` // why the agent keeps it stopped
	} `json:"telemt"`
	Capture struct {
		Rules    bool   `json:"rules"`
		Active   int    `json:"active"`
		Rejected uint64 `json:"rejected"`
	} `json:"capture"`
	Cover struct {
		Domain    string     `json:"domain"`
		OK        bool       `json:"ok"`
		CheckedAt *time.Time `json:"checkedAt"`
		Error     string     `json:"error"`
	} `json:"cover"`
}

type syncView struct {
	DesiredRev    string     `json:"desiredRev"`
	AppliedRev    string     `json:"appliedRev"`
	LastPushAt    *time.Time `json:"lastPushAt"`
	LastPushError string     `json:"lastPushError"`
	Healthy       bool       `json:"healthy"`
}

type eventView struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
}

type serverView struct {
	ID            uint         `json:"id"`
	Name          string       `json:"name"`
	Host          string       `json:"host"`
	SSHPort       int          `json:"sshPort"`
	SSHUser       string       `json:"sshUser"`
	HostKey       string       `json:"hostKey"`    // the pinned fingerprint; the UI sends it back on repair
	Registered    bool         `json:"registered"` // false: the install died before saving, so only removal works
	CreatedAt     time.Time    `json:"createdAt"`
	CoverDomain   string       `json:"coverDomain"`
	MaxStreams    int          `json:"maxStreams"`
	HubPort       int          `json:"hubPort"`
	Arch          string       `json:"arch"`
	AgentVersion  string       `json:"agentVersion"`
	TelemtVersion string       `json:"telemtVersion"`
	LastSeenAt    *time.Time   `json:"lastSeenAt"`
	UserCount     int          `json:"userCount"`
	Link          linkView     `json:"link"`
	Agent         *agentView   `json:"agent"`
	Sync          syncView     `json:"sync"`
	Events        *[]eventView `json:"events,omitempty"`
	ProxyStopped  bool         `json:"proxyStopped"`
	ProxyOptions  optionsView  `json:"proxyOptions"`
	Updates       updatesView  `json:"updates"`
	Usage         usageView    `json:"usage"`
}

type optionsView struct {
	ExtraCoverNames  []string `json:"extraCoverNames"`
	DeviceWindowSecs int      `json:"deviceWindowSecs"`
	LogLevel         string   `json:"logLevel"`
}

func optionsOf(o store.ProxyOptions) optionsView {
	v := optionsView{ExtraCoverNames: o.ExtraCoverNames, DeviceWindowSecs: o.DeviceWindowSecs, LogLevel: o.LogLevel}
	if v.ExtraCoverNames == nil {
		v.ExtraCoverNames = []string{}
	}
	if v.LogLevel == "" {
		v.LogLevel = "normal"
	}
	return v
}

// updatesView is what an update would install; a dev agent build may share the running version.
type updatesView struct {
	Agent         bool   `json:"agent"`
	Telemt        bool   `json:"telemt"`
	AgentVersion  string `json:"agentVersion"`
	TelemtVersion string `json:"telemtVersion"`
}

type usageView struct {
	Live        bool       `json:"live"`
	UsersOnline int        `json:"usersOnline"`
	Connections int        `json:"connections"`
	Devices     int        `json:"devices"`
	RateUp      int64      `json:"rateUp"`
	RateDown    int64      `json:"rateDown"`
	TodayUp     uint64     `json:"todayUp"`
	TodayDown   uint64     `json:"todayDown"`
	MonthUp     uint64     `json:"monthUp"`
	MonthDown   uint64     `json:"monthDown"`
	UpdatedAt   *time.Time `json:"updatedAt"`
}

func usageOf(u manager.ServerUsage) usageView {
	return usageView{
		Live: u.Live, UsersOnline: u.UsersOnline, Connections: u.Connections, Devices: u.Devices,
		RateUp: int64(u.RateUp), RateDown: int64(u.RateDown), TodayUp: u.TodayUp, TodayDown: u.TodayDown,
		MonthUp: u.MonthUp, MonthDown: u.MonthDown, UpdatedAt: utc(u.UpdatedAt),
	}
}

func linkOf(s hubnode.Snapshot) linkView {
	l := s.Link
	v := linkView{
		Up: l.Up, Total: len(l.Conns), Streams: l.Streams, Relayed: l.Relayed, DialFailures: l.DialFailures,
		Violations: l.Violations, BytesToDC: l.BytesToDC, BytesFromDC: l.BytesFromDC, Conns: []connView{},
	}
	switch {
	case v.Up > 0 && v.Up == v.Total:
		v.State = "up"
	case v.Up > 0:
		v.State = "degraded"
	default:
		v.State = "down"
	}
	for _, c := range l.Conns {
		v.Conns = append(v.Conns, connView{Name: c.Name, Up: c.Up, Since: utc(c.Since), RTTms: c.RTT.Milliseconds(), Streams: c.Streams, LastError: human(c.LastErr)})
	}
	return v
}

func agentOf(s hubnode.Snapshot) *agentView {
	st := s.Agent
	if st == nil {
		return nil
	}
	v := &agentView{
		Version: st.AgentVersion, TelemtVersion: st.TelemtVersion, UptimeSec: st.UptimeSec,
		RSSBytes: st.RSSBytes, StateRev: st.StateRev, ReportedAt: s.AgentAt.UTC(),
	}
	v.Telemt.Running, v.Telemt.Restarts, v.Telemt.UptimeSec = st.Telemt.Running, st.Telemt.Restarts, st.Telemt.UptimeSec
	v.Telemt.LastExit, v.Telemt.CrashLoop = st.Telemt.LastExit, st.Telemt.CrashLoop
	v.Telemt.Held = strings.ReplaceAll(human(st.Telemt.Held), "the panel", "the router")
	v.Capture.Rules, v.Capture.Active, v.Capture.Rejected = st.Capture.Rules, st.Capture.Active, st.Capture.Rejected
	// the agent forgets its check when it restarts; the router's own is kept, so the newer wins if it is of this site
	ok, at, why := st.Disguise.OK, st.Disguise.CheckedAt, st.Disguise.Err
	if c := s.Disguise; c != nil && s.DisguiseDomain == st.Disguise.Domain && s.DisguiseAt.After(at) {
		ok, at, why = c.OK, s.DisguiseAt, c.Err
	}
	v.Cover.Domain, v.Cover.OK, v.Cover.CheckedAt, v.Cover.Error = st.Disguise.Domain, ok && !at.IsZero(), utc(at), human(why)
	return v
}

func (a *API) view(srv store.Server, withEvents bool) serverView {
	r := srv.Record
	v := serverView{
		ID: srv.ID, Name: srv.Name, Host: srv.Host, SSHPort: r.SSHPort, SSHUser: r.SSHUser, HostKey: r.HostKey, Registered: srv.Registered(),
		CreatedAt: srv.CreatedAt.UTC(), CoverDomain: srv.CoverDomain(),
		MaxStreams: r.MaxStreams, HubPort: r.HubPort, Arch: r.Arch, AgentVersion: r.AgentVersion, TelemtVersion: r.TelemtVersion,
		UserCount: len(srv.Users), Link: linkView{State: "down", Conns: []connView{}},
	}
	if r.LastSeenAt != nil {
		v.LastSeenAt = utc(*r.LastSeenAt)
	}
	if snap, ok := a.cfg.Manager.Snapshot(srv.ID); ok {
		v.Link, v.Agent, v.Sync = linkOf(snap), agentOf(snap), syncOf(snap)
	} else {
		v.Sync.DesiredRev = a.cfg.Manager.Desired(srv).Rev
	}
	if withEvents {
		evs := eventsOf(a.cfg.Manager.Events(srv.ID))
		v.Events = &evs
	}
	v.ProxyStopped, v.ProxyOptions = srv.ProxyStopped, optionsOf(srv.ProxyOptions)
	due := a.cfg.Svc.Due(srv.ID, r.Arch)
	v.Updates = updatesView{Agent: due.Agent, Telemt: due.Telemt, AgentVersion: release.AgentVersion, TelemtVersion: release.TelemtVersion}
	v.Usage = usageOf(a.cfg.Manager.Usage(srv))
	return v
}

func syncOf(s hubnode.Snapshot) syncView {
	return syncView{DesiredRev: s.DesiredRev, AppliedRev: s.AppliedRev, LastPushAt: utc(s.LastPush), LastPushError: human(s.LastPushErr), Healthy: s.Healthy}
}

func eventsOf(in []manager.Event) []eventView {
	out := []eventView{}
	for _, e := range in {
		out = append(out, eventView{At: e.At.UTC(), Kind: e.Kind, Detail: strings.TrimSpace(human(e.Detail) + " " + human(e.Err))})
	}
	return out
}

func (a *API) servers(w http.ResponseWriter, _ *http.Request) {
	out := []serverView{}
	for _, srv := range a.cfg.Store.List() {
		out = append(out, a.view(srv, false))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) server(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.view(srv, true))
}

// fresh re-reads a server after a change.
func (a *API) fresh(w http.ResponseWriter, id uint) {
	srv, ok := a.cfg.Store.Get(id)
	if !ok {
		a.fail(w, errNoServer)
		return
	}
	writeJSON(w, http.StatusOK, a.view(srv, true))
}

func (a *API) rename(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	var b struct {
		Name string `json:"name"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	name := strings.TrimSpace(b.Name)
	if name == "" || len(name) > 100 {
		a.fail(w, bad("a name is 1 to 100 characters"))
		return
	}
	if err := a.cfg.Store.Mutate(srv.ID, func(s *store.Server) error { s.Name = name; return nil }); err != nil {
		a.fail(w, err)
		return
	}
	a.fresh(w, srv.ID)
}

func (a *API) settings(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	var b struct {
		CoverDomain  *string       `json:"coverDomain"`
		MaxStreams   *int          `json:"maxStreams"`
		ProxyOptions *optionsPatch `json:"proxyOptions"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	var cover string
	if b.CoverDomain != nil {
		cover = strings.ToLower(strings.TrimSpace(*b.CoverDomain))
		if !relayops.ValidDomain(cover) {
			a.fail(w, bad("the cover site must be a domain name, like www.example.com"))
			return
		}
	}
	if b.MaxStreams != nil && *b.MaxStreams < 100 {
		a.fail(w, bad("max streams must be at least 100"))
		return
	}
	mainCover := srv.CoverDomain()
	if b.CoverDomain != nil {
		mainCover = cover
	}
	opts := srv.ProxyOptions
	if b.ProxyOptions != nil {
		if opts, err = b.ProxyOptions.apply(srv.ProxyOptions, mainCover); err != nil {
			a.fail(w, err)
			return
		}
	} else if slices.Contains(opts.ExtraCoverNames, mainCover) {
		a.fail(w, bad("%s is already one of the extra cover names: remove it there first", mainCover))
		return
	}
	newCover := b.CoverDomain != nil && cover != srv.CoverDomain()
	newStreams := b.MaxStreams != nil && *b.MaxStreams != srv.Record.MaxStreams
	newOptions := b.ProxyOptions != nil
	if newCover {
		// the agent has to reach the site before it is told to use it
		var res ctrl.DisguiseResult
		if err := a.call(r, srv.ID, ctrl.MDisguiseCheck, ctrl.DisguiseCheck{Domain: cover}, &res); err != nil {
			a.fail(w, err)
			return
		}
		if !res.OK {
			a.fail(w, bad("%s does not work as a cover site from this VPS: %s", cover, res.Err))
			return
		}
	}
	// only what was sent: the check above is slow, and other saves may land meanwhile
	err = a.cfg.Manager.Edit(srv.ID, func(s *store.Server) error {
		if b.CoverDomain != nil {
			s.Cover = cover
		}
		if b.MaxStreams != nil {
			s.Record.MaxStreams = *b.MaxStreams
		}
		if newOptions {
			s.ProxyOptions = opts
		}
		return nil
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	switch {
	case newStreams:
		// the router's own stream cap is fixed per link (no setter), so the link restarts
		if err := a.cfg.Manager.Ensure(r.Context(), srv.ID); err != nil {
			a.fail(w, err)
			return
		}
	case newCover, newOptions:
		a.cfg.Manager.UsersChanged(srv.ID)
	}
	a.fresh(w, srv.ID)
}

// optionsPatch is a partial ProxyOptions: what is left out stays.
type optionsPatch struct {
	ExtraCoverNames  *[]string `json:"extraCoverNames"`
	DeviceWindowSecs *int      `json:"deviceWindowSecs"`
	LogLevel         *string   `json:"logLevel"`
}

const maxExtraCovers = 4

var logLevels = map[string]bool{"silent": true, "normal": true, "verbose": true, "debug": true}

func (p optionsPatch) apply(o store.ProxyOptions, cover string) (store.ProxyOptions, error) {
	if p.ExtraCoverNames != nil {
		names := []string{}
		for _, n := range *p.ExtraCoverNames {
			n = strings.ToLower(strings.TrimSpace(n))
			switch {
			case !relayops.ValidDomain(n):
				return o, bad("%q is not a domain name, like cdn.example.com", n)
			case n == cover:
				return o, bad("%s is the cover site already", n)
			case slices.Contains(names, n):
				return o, bad("%s is listed twice", n)
			}
			names = append(names, n)
		}
		if len(names) > maxExtraCovers {
			return o, bad("a server takes at most %d extra cover names", maxExtraCovers)
		}
		o.ExtraCoverNames = names
		if len(names) == 0 {
			o.ExtraCoverNames = nil
		}
	} else if slices.Contains(o.ExtraCoverNames, cover) {
		return o, bad("%s is already one of the extra cover names: remove it there first", cover)
	}
	if p.DeviceWindowSecs != nil {
		if *p.DeviceWindowSecs < 0 || *p.DeviceWindowSecs > 86400 {
			return o, bad("the device window is 0 to 86400 seconds")
		}
		o.DeviceWindowSecs = *p.DeviceWindowSecs
	}
	if p.LogLevel != nil {
		lvl := strings.ToLower(strings.TrimSpace(*p.LogLevel))
		if !logLevels[lvl] {
			return o, bad("the log level is silent, normal, verbose or debug")
		}
		if lvl == "normal" {
			lvl = "" // the default adds nothing to the agent's state
		}
		o.LogLevel = lvl
	}
	return o, nil
}

// proxy keeps the proxy off, or lets it run again.
func (a *API) proxy(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	var b struct {
		Stopped *bool `json:"stopped"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if b.Stopped == nil {
		a.fail(w, bad("say whether the proxy should be stopped"))
		return
	}
	if err := a.cfg.Store.Mutate(srv.ID, func(s *store.Server) error { s.ProxyStopped = *b.Stopped; return nil }); err != nil {
		a.fail(w, err)
		return
	}
	a.cfg.Manager.UsersChanged(srv.ID)
	a.fresh(w, srv.ID)
}

func (a *API) linkPort(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	var b struct {
		Port int `json:"port"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	id, err := a.cfg.Svc.StartLinkPort(r.Context(), srv.ID, b.Port)
	if err != nil {
		a.fail(w, err)
		return
	}
	accepted(w, id)
}

type sampleView struct {
	At   time.Time `json:"at"`
	Up   int64     `json:"up"`
	Down int64     `json:"down"`
}

// traffic is the last minutes of the server's rate, kept in memory only.
func (a *API) traffic(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	out := []sampleView{}
	for _, s := range a.cfg.Manager.Traffic(srv.ID) {
		out = append(out, sampleView{At: s.At.UTC(), Up: int64(s.Up), Down: int64(s.Down)})
	}
	step := max(1, int(a.cfg.Manager.StatsStep().Round(time.Second).Seconds()))
	writeJSON(w, http.StatusOK, map[string]any{"stepSec": step, "samples": out})
}

func accepted(w http.ResponseWriter, id string) {
	writeJSON(w, http.StatusAccepted, map[string]any{"jobId": id})
}

func (a *API) repair(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	var b struct {
		loginBody
		Reset bool `json:"reset"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	id, err := a.cfg.Svc.StartRepair(r.Context(), srv.ID, b.login(srv.Host), b.Reset)
	if err != nil {
		a.fail(w, err)
		return
	}
	accepted(w, id)
}

func (a *API) remove(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	var b struct {
		Force bool       `json:"force"`
		Login *loginBody `json:"login"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	var l *relayops.Login
	if b.Login != nil && !b.Login.empty() {
		lg := b.Login.login(srv.Host)
		l = &lg
	}
	id, err := a.cfg.Svc.StartUninstall(r.Context(), srv.ID, l, b.Force)
	if err != nil {
		a.fail(w, err)
		return
	}
	accepted(w, id)
}

func (a *API) update(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	var b struct {
		Kind string `json:"kind"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	id, err := a.cfg.Svc.StartUpdate(r.Context(), srv.ID, b.Kind)
	if err != nil {
		a.fail(w, err)
		return
	}
	accepted(w, id)
}

func (a *API) rotate(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	id, err := a.cfg.Svc.StartRotate(r.Context(), srv.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	accepted(w, id)
}

func (a *API) restart(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.call(r, srv.ID, ctrl.MServiceRestart, nil, nil); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) disguiseCheck(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	var b ctrl.DisguiseCheck
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	b.Domain = strings.ToLower(strings.TrimSpace(b.Domain))
	if !relayops.ValidDomain(b.Domain) {
		a.fail(w, bad("the cover site must be a domain name, like www.example.com"))
		return
	}
	var res ctrl.DisguiseResult
	if err := a.call(r, srv.ID, ctrl.MDisguiseCheck, b, &res); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, coverJSON(res))
}

// coverJSON is a cover site's test as the UI reads it.
func coverJSON(res ctrl.DisguiseResult) map[string]any {
	addrs := res.Addrs
	if addrs == nil {
		addrs = []string{}
	}
	return map[string]any{
		"ok": res.OK, "tls13": res.TLS13, "x25519": res.X25519, "h2": res.H2, "certOk": res.CertOK,
		"addrs": addrs, "latencyMs": res.LatencyMS, "error": human(res.Err),
	}
}

func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	n, err := strconv.Atoi(r.URL.Query().Get("lines"))
	if err != nil || n <= 0 {
		n = 300
	}
	n = min(n, 1000)
	var out ctrl.LogsResponse
	if err := a.call(r, srv.ID, ctrl.MLogsTail, ctrl.LogsRequest{Lines: n}, &out); err != nil {
		a.fail(w, err)
		return
	}
	if out.Lines == nil {
		out.Lines = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": out.Lines})
}

// --- users ---

// validName is ^[A-Za-z0-9_.-]{1,32}$ without regexp, and refuses names a URL path cannot carry ("..").
func validName(s string) bool {
	if len(s) < 1 || len(s) > 32 || strings.Trim(s, ".") == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

type linksView struct {
	TG    string          `json:"tg"`
	HTTPS string          `json:"https"`
	Extra []extraLinkView `json:"extra"`
}

type extraLinkView struct {
	Domain string `json:"domain"`
	TG     string `json:"tg"`
	HTTPS  string `json:"https"`
}

type userView struct {
	Name        string     `json:"name"`
	Enabled     bool       `json:"enabled"`
	State       string     `json:"state"`
	MaxIPs      int        `json:"maxIps"`
	ExpiresAt   *time.Time `json:"expiresAt"`
	QuotaBytes  uint64     `json:"quotaBytes"`
	Up          uint64     `json:"up"`
	Down        uint64     `json:"down"`
	CreatedAt   time.Time  `json:"createdAt"`
	Secret      string     `json:"secret"`
	Links       linksView  `json:"links"`
	Note        string     `json:"note"`
	Online      *bool      `json:"online"` // nil: the agent can't say, or its last word is stale
	Connections *int       `json:"connections"`
	Devices     *int       `json:"devices"`
	LastSeenAt  *time.Time `json:"lastSeenAt"`
	TodayUp     uint64     `json:"todayUp"`
	TodayDown   uint64     `json:"todayDown"`
	MonthUp     uint64     `json:"monthUp"`
	MonthDown   uint64     `json:"monthDown"`
}

// proxyLink is built here so the page never assembles a secret.
func proxyLink(host, cover, secret string) (tg, https string) {
	cs, err := telemt.ClientSecret(telemt.ModeTLS, secret, cover)
	if err != nil {
		cs = "ee" + secret + hex.EncodeToString([]byte(cover))
	}
	q := "server=" + url.QueryEscape(host) + "&port=443&secret=" + cs
	return "tg://proxy?" + q, "https://t.me/proxy?" + q
}

// linksOf is a user's link for the cover site, and one for each extra cover name.
func linksOf(srv store.Server, secret string) linksView {
	tg, https := proxyLink(srv.Host, srv.CoverDomain(), secret)
	v := linksView{TG: tg, HTTPS: https, Extra: []extraLinkView{}}
	for _, d := range srv.ProxyOptions.ExtraCoverNames {
		t, h := proxyLink(srv.Host, d, secret)
		v.Extra = append(v.Extra, extraLinkView{Domain: d, TG: t, HTTPS: h})
	}
	return v
}

func (a *API) userOf(srv store.Server, u store.User, uu manager.UserUsage) userView {
	v := userView{
		Name: u.Name, Enabled: u.Enabled, MaxIPs: u.MaxIPs, ExpiresAt: utc(u.ExpiresAt), QuotaBytes: u.QuotaBytes,
		Up: uu.Up, Down: uu.Down, CreatedAt: u.CreatedAt.UTC(), Secret: u.Secret, Links: linksOf(srv, u.Secret),
		Note: u.Note, LastSeenAt: utc(uu.LastSeen),
		TodayUp: uu.TodayUp, TodayDown: uu.TodayDown, MonthUp: uu.MonthUp, MonthDown: uu.MonthDown,
	}
	if uu.Live {
		on, conns, devs := uu.Connections > 0, uu.Connections, uu.Devices
		v.Online, v.Connections, v.Devices = &on, &conns, &devs
	}
	switch {
	case !u.Enabled:
		v.State = "disabled"
	case u.Expired(a.cfg.Now()):
		v.State = "expired"
	case u.QuotaBytes > 0 && uu.Up+uu.Down >= u.QuotaBytes:
		v.State = "over-quota"
	default:
		v.State = "active"
	}
	return v
}

func (a *API) users(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	use := a.cfg.Manager.Usage(srv)
	out := []userView{}
	for _, u := range srv.Users {
		out = append(out, a.userOf(srv, u, use.Users[u.Name]))
	}
	writeJSON(w, http.StatusOK, out)
}

var errNoUser = &httpError{http.StatusNotFound, "no such user"}

// mutateUser edits one user, pushes the result, and answers with the user.
func (a *API) mutateUser(w http.ResponseWriter, r *http.Request, code int, fn func(s *store.Server) (*store.User, error)) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	var out store.User
	err = a.cfg.Store.Mutate(srv.ID, func(s *store.Server) error {
		u, err := fn(s)
		if err != nil {
			return err
		}
		out = *u
		return nil
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.cfg.Manager.UsersChanged(srv.ID)
	srv, _ = a.cfg.Store.Get(srv.ID)
	writeJSON(w, code, a.userOf(srv, out, a.cfg.Manager.Usage(srv).Users[out.Name]))
}

const maxNote = 80

// cleanNote keeps a note to one short line.
func cleanNote(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxNote {
		return "", bad("a note is at most %d characters", maxNote)
	}
	return s, nil
}

func find(s *store.Server, name string) *store.User {
	for i := range s.Users {
		if s.Users[i].Name == name {
			return &s.Users[i]
		}
	}
	return nil
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name       string     `json:"name"`
		Enabled    *bool      `json:"enabled"`
		MaxIPs     int        `json:"maxIps"`
		ExpiresAt  *time.Time `json:"expiresAt"`
		QuotaBytes uint64     `json:"quotaBytes"`
		Note       string     `json:"note"`
		Secret     string     `json:"secret"` // keep someone's existing secret instead of making one
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if !validName(b.Name) {
		a.fail(w, bad("a user name is 1 to 32 letters, digits, '.', '_' or '-'"))
		return
	}
	if b.MaxIPs < 0 {
		a.fail(w, bad("max IPs cannot be negative"))
		return
	}
	note, err := cleanNote(b.Note)
	if err != nil {
		a.fail(w, err)
		return
	}
	var sec string
	if strings.TrimSpace(b.Secret) != "" {
		if sec, err = telemt.NormalizeSecret(b.Secret); err != nil {
			a.fail(w, bad("that secret is not usable: give 32 hex characters, or a dd or ee secret"))
			return
		}
	} else if sec, err = telemt.GenerateSecret(); err != nil {
		a.fail(w, err)
		return
	}
	u := store.User{Name: b.Name, Secret: sec, Enabled: b.Enabled == nil || *b.Enabled, MaxIPs: b.MaxIPs, QuotaBytes: b.QuotaBytes, Note: note, CreatedAt: a.cfg.Now().UTC()}
	if b.ExpiresAt != nil {
		u.ExpiresAt = b.ExpiresAt.UTC()
	}
	a.mutateUser(w, r, http.StatusCreated, func(s *store.Server) (*store.User, error) {
		if find(s, u.Name) != nil {
			return nil, conflict("there is already a user called %s", u.Name)
		}
		// two users on one secret could not be told apart
		if slices.ContainsFunc(s.Users, func(o store.User) bool { return o.Secret == u.Secret }) {
			return nil, conflict("another user on this server already has that secret")
		}
		if len(s.Users) >= maxUsers {
			return nil, conflict("a server holds at most %d users", maxUsers)
		}
		s.Users = append(s.Users, u)
		return &s.Users[len(s.Users)-1], nil
	})
}

func (a *API) updateUser(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Enabled    *bool           `json:"enabled"`
		MaxIPs     *int            `json:"maxIps"`
		ExpiresAt  json.RawMessage `json:"expiresAt"` // null clears, absent keeps
		QuotaBytes *uint64         `json:"quotaBytes"`
		Note       *string         `json:"note"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	var note string
	if b.Note != nil {
		var err error
		if note, err = cleanNote(*b.Note); err != nil {
			a.fail(w, err)
			return
		}
	}
	if b.MaxIPs != nil && *b.MaxIPs < 0 {
		a.fail(w, bad("max IPs cannot be negative"))
		return
	}
	var exp *time.Time
	if len(b.ExpiresAt) > 0 && string(b.ExpiresAt) != "null" {
		var t time.Time
		if err := json.Unmarshal(b.ExpiresAt, &t); err != nil {
			a.fail(w, bad("expiresAt must be an RFC 3339 time or null"))
			return
		}
		exp = &t
	}
	name := r.PathValue("name")
	a.mutateUser(w, r, http.StatusOK, func(s *store.Server) (*store.User, error) {
		u := find(s, name)
		if u == nil {
			return nil, errNoUser
		}
		if b.Enabled != nil {
			u.Enabled = *b.Enabled
		}
		if b.MaxIPs != nil {
			u.MaxIPs = *b.MaxIPs
		}
		if b.QuotaBytes != nil {
			u.QuotaBytes = *b.QuotaBytes
		}
		if b.Note != nil {
			u.Note = note
		}
		switch {
		case exp != nil:
			u.ExpiresAt = exp.UTC()
		case len(b.ExpiresAt) > 0:
			u.ExpiresAt = time.Time{}
		}
		return u, nil
	})
}

func (a *API) deleteUser(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	name := r.PathValue("name")
	err = a.cfg.Store.Mutate(srv.ID, func(s *store.Server) error {
		if find(s, name) == nil {
			return errNoUser
		}
		s.Users = slices.DeleteFunc(s.Users, func(u store.User) bool { return u.Name == name })
		delete(s.Baseline, name)
		return nil
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.cfg.Manager.ForgetUser(srv.ID, name)
	a.cfg.Manager.UsersChanged(srv.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) rotateSecret(w http.ResponseWriter, r *http.Request) {
	sec, err := telemt.GenerateSecret()
	if err != nil {
		a.fail(w, err)
		return
	}
	name := r.PathValue("name")
	a.mutateUser(w, r, http.StatusOK, func(s *store.Server) (*store.User, error) {
		u := find(s, name)
		if u == nil {
			return nil, errNoUser
		}
		u.Secret = sec
		return u, nil
	})
}

func (a *API) resetUsage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// what memory still holds belongs to before the reset
	if id, err := serverID(r); err == nil {
		if err := a.cfg.Manager.Flush(id); err != nil {
			a.fail(w, err)
			return
		}
	}
	a.mutateUser(w, r, http.StatusOK, func(s *store.Server) (*store.User, error) {
		u := find(s, name)
		if u == nil {
			return nil, errNoUser
		}
		u.Up, u.Down = 0, 0
		return u, nil
	})
}

// bulk acts on several users at once, with one push to the agent.
func (a *API) bulk(w http.ResponseWriter, r *http.Request) {
	srv, err := a.lookup(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	var b struct {
		Action string   `json:"action"`
		Names  []string `json:"names"`
		Days   int      `json:"days"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if len(b.Names) == 0 || len(b.Names) > maxUsers {
		a.fail(w, bad("pick the users to change"))
		return
	}
	if b.Days == 0 {
		b.Days = 30
	}
	if b.Days < 1 || b.Days > 3650 {
		a.fail(w, bad("renew by 1 to 3650 days"))
		return
	}
	picked := map[string]bool{}
	for _, n := range b.Names {
		picked[n] = true
	}
	now := a.cfg.Now()
	changed := 0
	edit := func(s *store.Server) error {
		switch b.Action {
		case "pause", "resume":
			for i := range s.Users {
				if u := &s.Users[i]; picked[u.Name] && u.Enabled != (b.Action == "resume") {
					u.Enabled = b.Action == "resume"
					changed++
				}
			}
		case "renew":
			for i := range s.Users {
				// a user who never expires has nothing to renew
				if u := &s.Users[i]; picked[u.Name] && !u.ExpiresAt.IsZero() {
					u.ExpiresAt = renewed(u.ExpiresAt, now, b.Days)
					changed++
				}
			}
		case "delete":
			before := len(s.Users)
			s.Users = slices.DeleteFunc(s.Users, func(u store.User) bool { return picked[u.Name] })
			for n := range picked {
				delete(s.Baseline, n)
			}
			changed = before - len(s.Users)
		default:
			return bad("the action is pause, resume, renew or delete")
		}
		if changed == 0 {
			return errUnchanged
		}
		return nil
	}
	err = a.cfg.Store.Mutate(srv.ID, edit)
	if errors.Is(err, errUnchanged) {
		writeJSON(w, http.StatusOK, map[string]any{"changed": 0})
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	if b.Action == "delete" {
		for n := range picked {
			a.cfg.Manager.ForgetUser(srv.ID, n)
		}
	}
	a.cfg.Manager.UsersChanged(srv.ID)
	writeJSON(w, http.StatusOK, map[string]any{"changed": changed})
}

var errUnchanged = errors.New("nothing to change")

// renewed moves an expiry on by days: from the old date while it is still ahead,
// else from today, to the end of that UTC day.
func renewed(exp, now time.Time, days int) time.Time {
	if exp.After(now) {
		return exp.AddDate(0, 0, days)
	}
	d := now.UTC().AddDate(0, 0, days)
	return time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, time.UTC)
}
