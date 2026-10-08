package relayops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/installer"
)

// Login is how to reach a VPS over SSH. It lives in memory for one job only: it
// is never stored, logged or returned.
type Login struct {
	Host       string
	Port       int
	User       string
	Password   string
	Key        []byte
	Passphrase []byte
	HostKey    string
}

func (s *Service) target(l Login) installer.Target {
	return installer.Target{Host: l.Host, Port: l.Port, User: l.User, Password: l.Password, Key: l.Key, Passphrase: l.Passphrase, HostKey: l.HostKey, Dial: s.cfg.Dial}
}

// forget drops the secrets once the job is done with them.
func (l *Login) forget() {
	if l != nil {
		l.Password, l.Key, l.Passphrase = "", nil, nil
	}
}

type InstallParams struct {
	Name                    string
	Login                   Login
	Disguise                string
	MaxStreams              int
	HubPort, CapturePort    int
	AllowUnverifiedDisguise bool
}

type ServiceConfig struct {
	Store Store
	Links Links
	Bins  BinarySource
	SSH   SSH
	Admin NodeAdmin
	Log   *slog.Logger
	// LinkWait is how long an install waits for the agent to answer over the link.
	LinkWait time.Duration
	// UpdateWait is how long an updated agent has to come back (default 3 min).
	UpdateWait time.Duration
	// JobTimeout caps every job; zero means a cap per kind (20 min for an install).
	JobTimeout time.Duration
	// Dial is how SSH leaves the hub (router mode pins a path); nil dials plainly.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

type Service struct {
	cfg  ServiceConfig
	mu   sync.Mutex
	jobs map[string]*job
	busy map[string]bool // nodes and hosts with a job running
}

// ErrBusy: one job per node (and per host) at a time; the APIs answer 409.
var ErrBusy = errors.New("relay: another job is already running on this node")

var jobTimeouts = map[string]time.Duration{
	"install": 20 * time.Minute, "repair": 20 * time.Minute, "uninstall": 10 * time.Minute,
	"update-agent": 15 * time.Minute, "update-telemt": 15 * time.Minute, "rotate": 10 * time.Minute,
	"link-port": 10 * time.Minute,
	"address":   10 * time.Minute,
}

func NewService(cfg ServiceConfig) *Service {
	if cfg.LinkWait == 0 {
		cfg.LinkWait = 45 * time.Second
	}
	if cfg.UpdateWait == 0 {
		cfg.UpdateWait = updateWait
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Service{cfg: cfg, jobs: map[string]*job{}, busy: map[string]bool{}}
}

func nodeKey(id uint) string  { return fmt.Sprintf("node %d", id) }
func hostKey(h string) string { return "host " + strings.ToLower(h) }

// start runs a job under a deadline, one per node or host: two on one box step on each other.
func (s *Service) start(kind string, keys []string, fn func(ctx context.Context, j *job) error) (string, error) {
	s.mu.Lock()
	for _, k := range keys {
		if s.busy[k] {
			s.mu.Unlock()
			return "", ErrBusy
		}
	}
	for _, k := range keys {
		s.busy[k] = true
	}
	s.mu.Unlock()
	j := s.newJob(kind)
	timeout := s.cfg.JobTimeout
	if timeout == 0 {
		timeout = jobTimeouts[kind]
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := fn(ctx, j)
		cancel()
		s.mu.Lock()
		for _, k := range keys {
			delete(s.busy, k)
		}
		s.mu.Unlock()
		j.finish(err)
	}()
	return j.v.ID, nil
}

// --- jobs ---

type JobState string

const (
	JobRunning JobState = "running"
	JobOK      JobState = "ok"
	JobFailed  JobState = "failed"
)

type JobView struct {
	ID         string           `json:"id"`
	Kind       string           `json:"kind"`
	State      JobState         `json:"state"`
	Steps      []installer.Step `json:"steps"`
	Error      string           `json:"error,omitempty"`
	NodeID     uint             `json:"node_id,omitempty"`
	StartedAt  time.Time        `json:"started_at"`
	FinishedAt *time.Time       `json:"finished_at,omitempty"`
}

type job struct {
	mu sync.Mutex
	v  JobView
}

func (j *job) step(s installer.Step) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i := range j.v.Steps {
		if j.v.Steps[i].Name == s.Name {
			j.v.Steps[i] = s
			return
		}
	}
	j.v.Steps = append(j.v.Steps, s)
}

func (j *job) setNode(id uint) {
	j.mu.Lock()
	j.v.NodeID = id
	j.mu.Unlock()
}

func (j *job) finish(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	j.v.FinishedAt = &now
	if err != nil {
		j.v.State, j.v.Error = JobFailed, err.Error()
		return
	}
	j.v.State = JobOK
}

func (j *job) snapshot() JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := j.v
	v.Steps = append([]installer.Step(nil), v.Steps...)
	return v
}

func (s *Service) newJob(kind string) *job {
	b := make([]byte, 8)
	rand.Read(b)
	j := &job{v: JobView{ID: hex.EncodeToString(b), Kind: kind, State: JobRunning, StartedAt: time.Now()}}
	s.mu.Lock()
	defer s.mu.Unlock()
	// keep a bounded history: the UI only ever asks about recent jobs
	if len(s.jobs) >= 50 {
		for id, old := range s.jobs {
			if v := old.snapshot(); v.State != JobRunning && time.Since(v.StartedAt) > time.Hour {
				delete(s.jobs, id)
			}
		}
	}
	s.jobs[j.v.ID] = j
	return j
}

func (s *Service) Job(id string) (JobView, bool) {
	s.mu.Lock()
	j := s.jobs[id]
	s.mu.Unlock()
	if j == nil {
		return JobView{}, false
	}
	return j.snapshot(), true
}

// --- validation ---

var ErrInvalid = errors.New("relay: invalid request")

// ValidDomain keeps obvious junk out; plain loops because regexp bloats the router plugin.
func ValidDomain(s string) bool {
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !label(l, false, "-") {
			return false
		}
	}
	return true
}

// validHost is a name of letters, digits, dots and dashes, alphanumeric at both ends.
func validHost(s string) bool { return label(s, true, ".-") }

// label: alphanumeric first and last byte, inner bytes alphanumeric or in inner.
func label(s string, upper bool, inner string) bool {
	if s == "" || !alnum(s[0], upper) || !alnum(s[len(s)-1], upper) {
		return false
	}
	for i := 1; i < len(s)-1; i++ {
		if !alnum(s[i], upper) && strings.IndexByte(inner, s[i]) < 0 {
			return false
		}
	}
	return true
}

func alnum(c byte, upper bool) bool {
	return 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || upper && 'A' <= c && c <= 'Z'
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func (l *Login) normalize() error {
	l.Host = strings.TrimSpace(l.Host)
	if l.Host == "" || len(l.Host) > 253 || (!validHost(l.Host) && net.ParseIP(l.Host) == nil) {
		return invalid("the host is not an address or a name")
	}
	if l.Port == 0 {
		l.Port = 22
	}
	if l.Port < 1 || l.Port > 65535 {
		return invalid("ssh port %d", l.Port)
	}
	if l.User = strings.TrimSpace(l.User); l.User == "" {
		l.User = "root"
	}
	if l.Password == "" && len(l.Key) == 0 {
		return invalid("give a password or a private key")
	}
	if !strings.HasPrefix(l.HostKey, "SHA256:") {
		return invalid("confirm the server's host key fingerprint first")
	}
	return nil
}

func (p *InstallParams) normalize() error {
	if err := p.Login.normalize(); err != nil {
		return err
	}
	if p.Name = strings.TrimSpace(p.Name); p.Name == "" || len(p.Name) > 100 {
		return invalid("name must be 1 to 100 characters")
	}
	p.Disguise = strings.ToLower(strings.TrimSpace(p.Disguise))
	if !ValidDomain(p.Disguise) {
		return invalid("the cover site is not a domain name")
	}
	if p.MaxStreams == 0 {
		p.MaxStreams = 5000
	}
	if p.MaxStreams < 100 || p.MaxStreams > 20000 {
		return invalid("max streams must be 100 to 20000")
	}
	if p.HubPort == 0 {
		p.HubPort = installer.DefaultHubPort
	}
	if p.CapturePort == 0 {
		p.CapturePort = installer.DefaultCapturePort
	}
	for _, port := range []int{p.HubPort, p.CapturePort} {
		if port < 1024 || port > 65535 || port == 22 {
			return invalid("port %d is not usable", port)
		}
	}
	if p.HubPort == p.CapturePort {
		return invalid("the link port and the capture port must differ")
	}
	return nil
}

// Fingerprint learns a server's host key so the operator can confirm it.
func (s *Service) Fingerprint(ctx context.Context, host string, port int, user string) (string, error) {
	l := Login{Host: host, Port: port, User: user}
	l.Password = "x" // not used: Probe never authenticates
	l.HostKey = "SHA256:probe"
	if err := l.normalize(); err != nil {
		return "", err
	}
	return s.cfg.SSH.Fingerprint(ctx, installer.Target{Host: l.Host, Port: l.Port, User: l.User, Dial: s.cfg.Dial})
}

// --- install ---

// StartInstall validates and tests the cover site from the VPS, then installs in
// the background. The test comes first so a bad site is refused in seconds,
// before anything is downloaded or put on the box. The returned id is polled
// (or streamed) for progress.
func (s *Service) StartInstall(ctx context.Context, p InstallParams) (string, error) {
	if err := p.normalize(); err != nil {
		return "", err
	}
	key := hostKey(p.Login.Host)
	s.mu.Lock()
	busy := s.busy[key]
	s.mu.Unlock()
	if busy {
		return "", ErrBusy
	}
	checked, err := s.checkCover(ctx, p)
	if err != nil {
		return "", err
	}
	return s.start("install", []string{key}, func(ctx context.Context, j *job) error {
		defer p.Login.forget() // the secrets end with the job
		if checked {
			j.step(installer.Step{Name: "disguise", State: "ok", Detail: p.Disguise})
		}
		return s.runInstall(ctx, j, p, checked)
	})
}

// coverWait caps the test before an install: the SSH login plus the site's own checks.
const coverWait = 45 * time.Second

// ErrVPS marks a VPS the panel could not reach or sign in to; the text is the SSH layer's own.
var ErrVPS = errors.New("relay: the VPS could not be reached")

type vpsError struct{ err error }

func (e vpsError) Error() string   { return e.err.Error() }
func (e vpsError) Unwrap() []error { return []error{ErrVPS, e.err} }

// CoverError is a cover site that failed its test from the VPS; nothing was installed.
type CoverError struct {
	Domain string
	Result ctrl.DisguiseResult
}

func (e *CoverError) Error() string {
	why := e.Result.Err
	if why == "" {
		why = "it failed the test"
	}
	return fmt.Sprintf("%s does not work as a cover site from this VPS: %s", e.Domain, why)
}

// checkCover tests the cover site from the VPS before the job starts. A box that
// can't be asked that way leaves the test to the agent, after the upload.
func (s *Service) checkCover(ctx context.Context, p InstallParams) (bool, error) {
	if p.AllowUnverifiedDisguise {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, coverWait)
	defer cancel()
	res, checked, err := s.cfg.SSH.CheckCover(ctx, s.target(p.Login), p.Disguise)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = errors.New("the VPS did not answer in time")
		}
		return false, vpsError{err}
	}
	if checked && !res.OK {
		return false, &CoverError{Domain: p.Disguise, Result: res}
	}
	return checked, nil
}

func (s *Service) runInstall(ctx context.Context, j *job, p InstallParams, coverChecked bool) error {
	t := s.target(p.Login)
	arch, err := s.cfg.SSH.Arch(ctx, t)
	if err != nil {
		return err
	}
	j.step(installer.Step{Name: "download", State: "running"})
	agent, err := s.cfg.Bins.Agent(ctx, arch)
	if err != nil {
		j.step(installer.Step{Name: "download", State: "failed", Detail: err.Error()})
		return err
	}
	telemt, err := s.cfg.Bins.Telemt(ctx, arch)
	if err != nil {
		j.step(installer.Step{Name: "download", State: "failed", Detail: err.Error()})
		return err
	}
	j.step(installer.Step{Name: "download", State: "ok"})

	creds, err := installer.NewCreds()
	if err != nil {
		return err
	}
	res, err := s.cfg.SSH.Install(ctx, t, installer.Request{
		Agent: agent, Telemt: telemt, Disguise: p.Disguise,
		HubPort: p.HubPort, CapturePort: p.CapturePort, Creds: creds,
		AllowUnverifiedDisguise: p.AllowUnverifiedDisguise, CoverChecked: coverChecked,
	}, j.step)
	if err != nil {
		s.undo(ctx, t, res)
		return err
	}

	// the VPS is set up; now the panel has to remember it
	j.step(installer.Step{Name: "register", State: "running"})
	nodeID, err := s.cfg.Admin.CreateNode(ctx, p.Name, p.Login.Host)
	if err != nil {
		s.undo(ctx, t, res)
		j.step(installer.Step{Name: "register", State: "failed", Detail: err.Error()})
		return err
	}
	row := &Record{
		NodeID: nodeID, AgentID: res.AgentID, HubPort: res.HubPort, CapturePort: res.CapturePort,
		SSHPort: p.Login.Port, SSHUser: p.Login.User, HostKey: p.Login.HostKey,
		RealityPub: hex.EncodeToString(res.Creds.Pub), ShortID: hex.EncodeToString(res.Creds.ShortID[:]), Token: hex.EncodeToString(res.Creds.Token),
		Disguise: p.Disguise, MaxStreams: p.MaxStreams, Arch: string(arch),
	}
	if err := s.cfg.Store.Save(ctx, row); err != nil {
		_ = s.cfg.Admin.DeleteNode(ctx, nodeID)
		s.undo(ctx, t, res)
		j.step(installer.Step{Name: "register", State: "failed", Detail: err.Error()})
		return err
	}
	j.step(installer.Step{Name: "register", State: "ok"})
	j.setNode(nodeID)

	// the node stays registered even if the link does not come up: Repair and
	// Delete need it, and the failure says what to check
	return s.waitLink(ctx, j, nodeID, row.HubPort)
}

// undo takes a half-finished install off the VPS, with time of its own if the job ran out.
// Only a box this job changed and that had no agent before: an agent that was there stays.
func (s *Service) undo(ctx context.Context, t installer.Target, res *installer.Result) {
	if res == nil || !res.Started || res.Facts.Installed {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	if err := s.cfg.SSH.Uninstall(ctx, t, nil); err != nil {
		s.cfg.Log.Warn("relay: could not clean up a failed install", "err", err)
	}
}

// waitLink starts the node's link and waits for it.
func (s *Service) waitLink(ctx context.Context, j *job, nodeID uint, hubPort int) error {
	j.step(installer.Step{Name: "link", State: "running"})
	if err := s.cfg.Links.Ensure(ctx, nodeID); err != nil {
		j.step(installer.Step{Name: "link", State: "failed", Detail: err.Error()})
		return err
	}
	return s.waitUp(ctx, j, nodeID, hubPort)
}

func (s *Service) waitUp(ctx context.Context, j *job, nodeID uint, hubPort int) error {
	j.step(installer.Step{Name: "link", State: "running"})
	if s.linkComesUp(ctx, nodeID, s.cfg.LinkWait) {
		j.step(installer.Step{Name: "link", State: "ok"})
		return nil
	}
	err := fmt.Errorf("the agent is running but the panel cannot reach it: check that TCP port %d is open in the VPS firewall and the provider's security group", hubPort)
	j.step(installer.Step{Name: "link", State: "failed", Detail: err.Error()})
	return err
}

// linkComesUp polls the node's link for up to d.
func (s *Service) linkComesUp(ctx context.Context, nodeID uint, d time.Duration) bool {
	return s.poll(ctx, d, func() bool { return s.linkUp(nodeID) })
}

func (s *Service) poll(ctx context.Context, d time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if ok() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return false
		}
	}
}

// --- repair ---

// StartRepair re-runs the install over an existing node. Unless reset is set the
// credentials already on the VPS are kept and adopted.
func (s *Service) StartRepair(ctx context.Context, nodeID uint, l Login, reset bool) (string, error) {
	row, err := s.cfg.Store.Get(ctx, nodeID)
	if err != nil {
		return "", err
	}
	host, err := s.cfg.Admin.Host(ctx, nodeID)
	if err != nil {
		return "", err
	}
	l.Host = host
	l.fill(row)
	if err := l.normalize(); err != nil {
		return "", err
	}
	return s.start("repair", []string{nodeKey(nodeID), hostKey(host)}, func(ctx context.Context, j *job) error {
		defer l.forget()
		return s.runRepair(ctx, j, row, l, reset)
	})
}

// fill takes the rest from the record; sshd may have moved, so the caller's port and key win.
func (l *Login) fill(row *Record) {
	if l.Port == 0 {
		l.Port = row.SSHPort
	}
	if l.HostKey == "" {
		l.HostKey = row.HostKey
	}
	if l.User == "" {
		l.User = row.SSHUser
	}
}

func (s *Service) runRepair(ctx context.Context, j *job, row *Record, l Login, reset bool) error {
	t := s.target(l)
	arch, err := s.cfg.SSH.Arch(ctx, t)
	if err != nil {
		return err
	}
	agent, err := s.cfg.Bins.Agent(ctx, arch)
	if err != nil {
		return err
	}
	telemt, err := s.cfg.Bins.Telemt(ctx, arch)
	if err != nil {
		return err
	}
	creds, err := installer.NewCreds()
	if err != nil {
		return err
	}
	res, err := s.cfg.SSH.Install(ctx, t, installer.Request{
		Agent: agent, Telemt: telemt, Disguise: row.Disguise, AgentID: row.AgentID,
		HubPort: row.HubPort, CapturePort: row.CapturePort, Creds: creds, KeepState: !reset, Repair: true,
	}, j.step)
	if err != nil {
		return err
	}
	// adopt what is on the box (a lost rotation leaves it different) and the login that worked
	updated := *row
	if updated.Arch == "" {
		updated.Arch = string(arch)
	}
	updated.RealityPub = hex.EncodeToString(res.Creds.Pub)
	updated.ShortID = hex.EncodeToString(res.Creds.ShortID[:])
	updated.Token = hex.EncodeToString(res.Creds.Token)
	updated.NextSeed = ""
	updated.SSHPort, updated.HostKey, updated.SSHUser = l.Port, l.HostKey, l.User
	if res.AgentID != "" {
		updated.AgentID = res.AgentID
	}
	if res.Domain != "" {
		updated.Disguise = res.Domain // REALITY answers to the box's cover site, so the SNI follows it
	}
	if err := s.cfg.Store.Save(ctx, &updated); err != nil {
		return err
	}
	// before any redial, or the link keeps presenting a name the agent no longer answers to
	if a, ok := s.cfg.Links.(CoverAdopter); ok && updated.Disguise != row.Disguise {
		if err := a.AdoptCover(ctx, row.NodeID, updated.Disguise); err != nil {
			return fmt.Errorf("relay: could not record the cover site the box runs: %w", err)
		}
	}
	j.setNode(row.NodeID)
	// done only once the link is back on what was adopted
	if !s.cfg.Links.IsRunning(row.NodeID) {
		return s.waitLink(ctx, j, row.NodeID, updated.HubPort)
	}
	if err := s.cfg.Links.SetCreds(ctx, &updated); err != nil {
		return err
	}
	if err := s.cfg.Links.Redial(ctx, row.NodeID); err != nil {
		return err
	}
	return s.waitUp(ctx, j, row.NodeID, updated.HubPort)
}

// --- uninstall ---

var ErrNeedLogin = errors.New("relay: the node is unreachable over its link: give SSH credentials, or force the delete and remove the agent by hand")

// StartUninstall removes the agent (over the link, then an SSH sweep if given a login) and the node; force forgets it regardless.
func (s *Service) StartUninstall(ctx context.Context, nodeID uint, l *Login, force bool) (string, error) {
	row, err := s.cfg.Store.Get(ctx, nodeID)
	if err != nil {
		return "", err
	}
	keys := []string{nodeKey(nodeID)}
	if l != nil {
		host, err := s.cfg.Admin.Host(ctx, nodeID)
		if err != nil {
			return "", err
		}
		l.Host = host
		l.fill(row)
		if err := l.normalize(); err != nil {
			return "", err
		}
		keys = append(keys, hostKey(host))
	}
	linkUp := s.linkUp(nodeID)
	if !linkUp && l == nil && !force {
		return "", ErrNeedLogin
	}
	return s.start("uninstall", keys, func(ctx context.Context, j *job) error {
		defer l.forget()
		return s.runUninstall(ctx, j, nodeID, l, linkUp, force)
	})
}

func (s *Service) runUninstall(ctx context.Context, j *job, nodeID uint, l *Login, linkUp, force bool) error {
	removed := false
	var rerr error
	if linkUp {
		j.step(installer.Step{Name: "remove", State: "running"})
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		rerr = s.cfg.Links.Call(cctx, nodeID, ctrl.MUninstall, nil, nil)
		cancel()
		removed = rerr == nil
		if removed && l != nil {
			// let the agent finish taking itself away before SSH sweeps up
			s.poll(ctx, 20*time.Second, func() bool { return !s.linkUp(nodeID) })
		}
	}
	if l != nil {
		progress := j.step
		if removed {
			progress = nil // the agent is gone already; this only sweeps up after it
		}
		if err := s.cfg.SSH.Uninstall(ctx, s.target(*l), progress); err != nil {
			if removed {
				s.cfg.Log.Warn("relay: the SSH sweep after the agent removed itself failed", "node", nodeID, "err", err)
			} else {
				rerr = err
			}
		} else {
			removed = true
		}
	}
	if !removed && !force {
		if rerr == nil {
			rerr = ErrNeedLogin
		}
		j.step(installer.Step{Name: "remove", State: "failed", Detail: rerr.Error()})
		return rerr
	}
	if removed {
		j.step(installer.Step{Name: "remove", State: "ok"})
	}
	j.step(installer.Step{Name: "forget", State: "running"})
	s.cfg.Links.Remove(nodeID)
	if err := s.cfg.Store.Delete(ctx, nodeID); err != nil {
		j.step(installer.Step{Name: "forget", State: "failed", Detail: err.Error()})
		return err
	}
	if err := s.cfg.Admin.DeleteNode(ctx, nodeID); err != nil {
		j.step(installer.Step{Name: "forget", State: "failed", Detail: err.Error()})
		return err
	}
	j.step(installer.Step{Name: "forget", State: "ok"})
	return nil
}
