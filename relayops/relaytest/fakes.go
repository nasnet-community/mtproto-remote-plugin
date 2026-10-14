// Package relaytest holds stand-ins for everything relayops talks to, so the
// hubs can test their own wiring against the real service without SSH, a link
// or a database.
package relaytest

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/installer"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops"
	"github.com/nasnet-community/mtproto-remote-plugin/release"
)

// SSH is a VPS that does whatever the test says.
type SSH struct {
	Mu         sync.Mutex
	FP         string
	Arch_      release.Arch
	ArchErr    error
	InstallErr error
	Result     *installer.Result
	UninstallE error
	Targets    []installer.Target
	Installed  []installer.Request
	Uninstalls int
	// WasInstalled: an agent was on the box before; FailEarly: an install fails in its checks
	WasInstalled, FailEarly bool
	// Block holds Install until ctx ends (a host that never finishes)
	Block bool
	// Cover is what the cover test finds (nil: the site works); CoverUnchecked:
	// the box can't be asked; CoverErr: the login fails
	Cover          *ctrl.DisguiseResult
	CoverUnchecked bool
	CoverErr       error
	CoverChecks    []string
}

func (f *SSH) Fingerprint(_ context.Context, t installer.Target) (string, error) {
	f.Mu.Lock()
	f.Targets = append(f.Targets, t)
	f.Mu.Unlock()
	return f.FP, nil
}
func (f *SSH) Arch(_ context.Context, t installer.Target) (release.Arch, error) {
	f.Mu.Lock()
	f.Targets = append(f.Targets, t)
	f.Mu.Unlock()
	return f.Arch_, f.ArchErr
}
func (f *SSH) Install(ctx context.Context, t installer.Target, req installer.Request, p installer.Progress) (*installer.Result, error) {
	f.Mu.Lock()
	f.Targets = append(f.Targets, t)
	f.Installed = append(f.Installed, req)
	block := f.Block
	f.Mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if p != nil {
		p(installer.Step{Name: "check", State: "ok"})
		p(installer.Step{Name: "service", State: "ok"})
	}
	if f.InstallErr != nil {
		// like the real one: what it found comes back with the failure
		return &installer.Result{Facts: installer.Facts{Arch: f.Arch_, Installed: f.WasInstalled}, Started: !f.FailEarly}, f.InstallErr
	}
	if f.Result != nil {
		return f.Result, nil
	}
	return &installer.Result{AgentID: "agent-xyz", Creds: req.Creds, HubPort: req.HubPort, CapturePort: req.CapturePort, TelemtUID: 998,
		Facts: installer.Facts{Arch: f.Arch_, Installed: f.WasInstalled}, Started: true, Domain: strings.ToLower(req.Disguise)}, nil
}
func (f *SSH) CheckCover(_ context.Context, t installer.Target, domain string) (ctrl.DisguiseResult, bool, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Targets = append(f.Targets, t)
	f.CoverChecks = append(f.CoverChecks, domain)
	if f.CoverErr != nil {
		return ctrl.DisguiseResult{}, false, f.CoverErr
	}
	if f.Cover != nil {
		return *f.Cover, !f.CoverUnchecked, nil
	}
	return ctrl.DisguiseResult{OK: true, TLS13: true, X25519: true, H2: true, CertOK: true}, !f.CoverUnchecked, nil
}
func (f *SSH) Uninstall(_ context.Context, t installer.Target, _ installer.Progress) error {
	f.Mu.Lock()
	f.Targets = append(f.Targets, t)
	f.Uninstalls++
	f.Mu.Unlock()
	return f.UninstallE
}

// CallsOf counts the calls of one method.
func (l *Links) CallsOf(m string) int {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	n := 0
	for _, c := range l.Calls {
		if c == m {
			n++
		}
	}
	return n
}

// Bins hands out a one-byte "binary" with a fixed hash.
type Bins struct{ Err error }

func (b Bins) bin() installer.Bin {
	return installer.Bin{Size: 1, SHA256: strings.Repeat("a", 64), Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("x")), nil }}
}
func (b Bins) Agent(context.Context, release.Arch) (installer.Bin, error)  { return b.bin(), b.Err }
func (b Bins) Telemt(context.Context, release.Arch) (installer.Bin, error) { return b.bin(), nil }
func (b Bins) AgentBuild(release.Arch) string {
	if b.Err != nil {
		return ""
	}
	return b.bin().SHA256
}

// Admin is the hub's node list.
type Admin struct {
	Mu      sync.Mutex
	Hosts   map[uint]string
	Names   map[uint]string
	Next    uint
	CreateE error
	Deleted []uint
}

func (a *Admin) CreateNode(_ context.Context, name, host string) (uint, error) {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if a.CreateE != nil {
		return 0, a.CreateE
	}
	a.Next++
	if a.Hosts == nil {
		a.Hosts, a.Names = map[uint]string{}, map[uint]string{}
	}
	a.Hosts[a.Next], a.Names[a.Next] = host, name
	return a.Next, nil
}
func (a *Admin) DeleteNode(_ context.Context, id uint) error {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	a.Deleted = append(a.Deleted, id)
	delete(a.Hosts, id)
	delete(a.Names, id)
	return nil
}
func (a *Admin) SetHost(_ context.Context, id uint, host string) error {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if _, ok := a.Hosts[id]; !ok {
		return errors.New("no such node")
	}
	a.Hosts[id] = host
	return nil
}
func (a *Admin) Host(_ context.Context, id uint) (string, error) {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	h, ok := a.Hosts[id]
	if !ok {
		return "", errors.New("no such node")
	}
	return h, nil
}

// Links is the hub's running connections.
type Links struct {
	Mu           sync.Mutex
	Up           map[uint]bool
	Running      map[uint]bool
	Ensured      []uint
	Removed      []uint
	CredsSet     []relayops.Record
	Calls        []string
	CallErr      error
	RawErr       error
	RawReqs      []ctrl.UpdateRequest
	RawBytes     int
	AgentVersion string
	AgentBuild   string // what status and View report as the running build
	TelemtVer    string
	AfterRaw     func()
	Redials      int
	RedialUp     bool
	OnRedial     func(n int) bool              // the link after the nth redial (from 1), when set
	Respond      func(method string, resp any) // fills a control reply
	BootID       string                        // what status reports; a restarted agent has a new one
	// Fail picks a call's error by method and earlier count (lost reply: plain error; refusal: *ctrl.Error)
	Fail  func(method string, n int) error
	Reqs  []any // every request, in order
	count map[string]int
	// DownAfterUninstall takes the link down once the agent has been told to go
	DownAfterUninstall bool
	AdoptErr           error // what AdoptCover answers
	adopted            []Adopt
}

// Adopt is one AdoptCover call.
type Adopt struct {
	NodeID uint
	Domain string
}

func (l *Links) AdoptCover(_ context.Context, id uint, domain string) error {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	l.adopted = append(l.adopted, Adopt{id, domain})
	return l.AdoptErr
}

// Adopted is every AdoptCover call so far.
func (l *Links) Adopted() []Adopt {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	return append([]Adopt(nil), l.adopted...)
}

func (l *Links) Ensure(_ context.Context, id uint) error {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	l.Ensured = append(l.Ensured, id)
	if l.Running == nil {
		l.Running = map[uint]bool{}
	}
	l.Running[id] = true
	return nil
}
func (l *Links) View(id uint) (relayops.LinkView, bool) {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if !l.Running[id] {
		return relayops.LinkView{}, false
	}
	return relayops.LinkView{Up: l.Up[id], AgentVersion: l.AgentVersion, AgentBuild: l.AgentBuild, TelemtVersion: l.TelemtVer}, true
}
func (l *Links) IsRunning(id uint) bool { l.Mu.Lock(); defer l.Mu.Unlock(); return l.Running[id] }
func (l *Links) SetCreds(_ context.Context, r *relayops.Record) error {
	l.Mu.Lock()
	l.CredsSet = append(l.CredsSet, *r)
	l.Mu.Unlock()
	return nil
}
func (l *Links) Remove(id uint) {
	l.Mu.Lock()
	l.Removed = append(l.Removed, id)
	delete(l.Running, id)
	l.Mu.Unlock()
}
func (l *Links) Call(_ context.Context, id uint, m string, req, resp any) error {
	l.Mu.Lock()
	l.Calls = append(l.Calls, m)
	l.Reqs = append(l.Reqs, req)
	if l.count == nil {
		l.count = map[string]int{}
	}
	n := l.count[m]
	l.count[m]++
	respond := l.Respond
	err := l.CallErr
	if l.Fail != nil && err == nil {
		err = l.Fail(m, n)
	}
	if st, ok := resp.(*ctrl.Status); ok && respond == nil && err == nil {
		st.AgentVersion, st.AgentBuild, st.TelemtVersion, st.BootID = l.AgentVersion, l.AgentBuild, l.TelemtVer, l.BootID
	}
	if m == ctrl.MUninstall && err == nil && l.DownAfterUninstall {
		go func() {
			time.Sleep(50 * time.Millisecond)
			l.SetUp(id, false)
		}()
	}
	l.Mu.Unlock()
	if respond != nil && err == nil {
		respond(m, resp)
	}
	return err
}
func (l *Links) CallRaw(_ context.Context, _ uint, m string, req any, body io.Reader, _ any) error {
	l.Mu.Lock()
	l.Calls = append(l.Calls, m)
	if r, ok := req.(ctrl.UpdateRequest); ok {
		l.RawReqs = append(l.RawReqs, r)
	}
	err, after := l.RawErr, l.AfterRaw
	l.Mu.Unlock()
	n, _ := io.Copy(io.Discard, body)
	l.Mu.Lock()
	l.RawBytes += int(n)
	l.Mu.Unlock()
	if after != nil {
		after()
	}
	return err
}
func (l *Links) Redial(_ context.Context, id uint) error {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	l.Redials++
	if l.Up == nil {
		l.Up = map[uint]bool{}
	}
	l.Up[id] = l.RedialUp
	if l.OnRedial != nil {
		l.Up[id] = l.OnRedial(l.Redials)
	}
	return nil
}

// SetUp marks a node's link up or down.
func (l *Links) SetUp(id uint, up bool) {
	l.Mu.Lock()
	if l.Up == nil {
		l.Up = map[uint]bool{}
	}
	l.Up[id] = up
	l.Mu.Unlock()
}

// MemStore is an in-memory relayops.Store.
type MemStore struct {
	Mu    sync.Mutex
	Rows  map[uint]relayops.Record
	SaveE error // every Save fails with this
}

func NewMemStore() *MemStore { return &MemStore{Rows: map[uint]relayops.Record{}} }
func (s *MemStore) Get(_ context.Context, id uint) (*relayops.Record, error) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	r, ok := s.Rows[id]
	if !ok {
		return nil, relayops.ErrNotFound
	}
	return &r, nil
}
func (s *MemStore) Save(_ context.Context, r *relayops.Record) error {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if s.SaveE != nil {
		return s.SaveE
	}
	s.Rows[r.NodeID] = *r
	return nil
}

// List is for assertions; the service itself never lists.
func (s *MemStore) List(context.Context) ([]*relayops.Record, error) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	var out []*relayops.Record
	for _, r := range s.Rows {
		r := r
		out = append(out, &r)
	}
	return out, nil
}
func (s *MemStore) Delete(_ context.Context, id uint) error {
	s.Mu.Lock()
	delete(s.Rows, id)
	s.Mu.Unlock()
	return nil
}

// Rig is a Service wired to all of the above.
type Rig struct {
	Svc   *relayops.Service
	SSH   *SSH
	Admin *Admin
	Links *Links
	Store *MemStore
}

func NewRig(tweaks ...func(*relayops.ServiceConfig)) *Rig {
	r := &Rig{SSH: &SSH{FP: "SHA256:abc", Arch_: release.AMD64}, Admin: &Admin{}, Links: &Links{Up: map[uint]bool{}}, Store: NewMemStore()}
	cfg := relayops.ServiceConfig{
		Store: r.Store, Links: r.Links, Bins: Bins{}, SSH: r.SSH, Admin: r.Admin,
		LinkWait: 600 * time.Millisecond,
	}
	for _, t := range tweaks {
		t(&cfg)
	}
	r.Svc = relayops.NewService(cfg)
	return r
}

// GoodInstall is a request that passes validation.
func GoodInstall() relayops.InstallParams {
	return relayops.InstallParams{
		Name: "Relay 1", Disguise: "Mirror.Example.com",
		Login: relayops.Login{Host: "203.0.113.9", User: "root", Password: "hunter2-super-secret", HostKey: "SHA256:abc"},
	}
}

// Wait polls a job until it ends.
func (r *Rig) Wait(t testing.TB, id string) relayops.JobView {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if v, ok := r.Svc.Job(id); ok && v.State != relayops.JobRunning {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job never finished")
	return relayops.JobView{}
}

// Installed runs a good install to completion and returns node 1's job.
func (r *Rig) Installed(t testing.TB) {
	t.Helper()
	r.Links.SetUp(1, true)
	id, err := r.Svc.StartInstall(context.Background(), GoodInstall())
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Wait(t, id); v.State != relayops.JobOK {
		t.Fatalf("setup install: %+v", v)
	}
}
