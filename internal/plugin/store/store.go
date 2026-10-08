// Package store keeps the plugin's servers, their users and usage in one JSON
// file under DATA_DIR.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/hubnode"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops"
)

const (
	FileName            = "servers.json"
	DefaultMaxServers   = 3
	DefaultStreamBudget = 6000
	DefaultMaxStreams   = 2000
)

var (
	ErrNotFound     = errors.New("no such server")
	ErrServerCap    = errors.New("server limit reached")
	ErrStreamBudget = errors.New("stream budget exceeded")
)

// Limits bound what one router carries; zero fields take the defaults.
type Limits struct {
	MaxServers   int
	StreamBudget int
}

type User struct {
	Name       string    `json:"name"`
	Secret     string    `json:"secret"` // 32 lowercase hex
	Enabled    bool      `json:"enabled"`
	MaxIPs     int       `json:"max_ips,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitzero"`
	QuotaBytes uint64    `json:"quota_bytes,omitempty"`
	Up         uint64    `json:"up"` // since the last reset
	Down       uint64    `json:"down"`
	CreatedAt  time.Time `json:"created_at"`
}

func (u User) OverQuota() bool { return u.QuotaBytes > 0 && u.Up+u.Down >= u.QuotaBytes }

func (u User) Expired(now time.Time) bool { return !u.ExpiresAt.IsZero() && !now.Before(u.ExpiresAt) }

type Server struct {
	ID        uint
	Name      string
	Host      string
	CreatedAt time.Time
	// Cover is the site we want; Record.Disguise is the one applied, which the link presents.
	Cover    string
	Record   relayops.Record
	Users    []User
	Baseline map[string]hubnode.Usage // the agent's last cumulative counters
}

// CoverDomain is the cover site the server should use.
func (s Server) CoverDomain() string {
	if s.Cover != "" {
		return s.Cover
	}
	return s.Record.Disguise
}

// Registered is true once an install has saved the link credentials.
func (s Server) Registered() bool { return s.Record.Token != "" }

// NewServer is what an install registers before its record exists.
type NewServer struct {
	Name, Host string
	MaxStreams int
	Users      []User
}

type Store struct {
	path string
	lim  Limits

	mu      sync.Mutex
	nextID  uint
	servers []Server
}

// Open loads dir/servers.json; a corrupt file is an error, never silently replaced.
func Open(dir string, lim Limits) (*Store, error) {
	if lim.MaxServers <= 0 {
		lim.MaxServers = DefaultMaxServers
	}
	if lim.StreamBudget <= 0 {
		lim.StreamBudget = DefaultStreamBudget
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, FileName), lim: lim, nextID: 1}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f fileForm
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("store: %s is corrupt: %w", s.path, err)
	}
	for _, ss := range f.Servers {
		s.servers = append(s.servers, ss.server())
		s.nextID = max(s.nextID, ss.ID+1)
	}
	s.nextID = max(s.nextID, f.NextID)
	slices.SortFunc(s.servers, func(a, b Server) int { return int(a.ID) - int(b.ID) })
	return s, nil
}

func (s *Store) Path() string   { return s.path }
func (s *Store) Limits() Limits { return s.lim }

func (s *Store) List() []Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Server, len(s.servers))
	for i, srv := range s.servers {
		out[i] = clone(srv)
	}
	return out
}

func (s *Store) Get(id uint) (Server, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.index(id); i >= 0 {
		return clone(s.servers[i]), true
	}
	return Server{}, false
}

// Allocated is the sum of max_streams across servers.
func (s *Store) Allocated() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return allocated(s.servers)
}

// Create registers a server (relayops' CreateNode) within the cap and the budget.
func (s *Store) Create(n NewServer) (uint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.servers) >= s.lim.MaxServers {
		return 0, s.capErr()
	}
	id := s.nextID
	srv := Server{ID: id, Name: n.Name, Host: n.Host, CreatedAt: time.Now().UTC(), Users: slices.Clone(n.Users), Baseline: map[string]hubnode.Usage{}}
	srv.Record.NodeID, srv.Record.MaxStreams = id, n.MaxStreams
	next := append(slices.Clone(s.servers), srv)
	if err := s.commit(next, id+1); err != nil {
		return 0, err
	}
	return id, nil
}

// Delete forgets a server; deleting one that is gone is not an error.
func (s *Store) Delete(id uint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return nil
	}
	return s.commit(slices.Delete(slices.Clone(s.servers), i, i+1), s.nextID)
}

// SaveRecord is relayops' Store.Save.
func (s *Store) SaveRecord(r *relayops.Record) error {
	return s.Mutate(r.NodeID, func(srv *Server) error {
		in := *r
		if srv.Registered() {
			// relayops saves a copy read before its job ran; what the manager learned since is newer
			old := srv.Record
			in.Disguise, in.MaxStreams = old.Disguise, old.MaxStreams
			in.AgentVersion, in.TelemtVersion, in.LastSeenAt = old.AgentVersion, old.TelemtVersion, old.LastSeenAt
		}
		srv.Record = in
		if srv.Cover == "" {
			srv.Cover = in.Disguise
		}
		return nil
	})
}

// Mutate is the one write path: fn edits a copy, kept only if fn and the write succeed.
func (s *Store) Mutate(id uint, fn func(*Server) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return ErrNotFound
	}
	cur := clone(s.servers[i])
	if err := fn(&cur); err != nil {
		return err
	}
	cur.ID, cur.Record.NodeID = id, id
	next := slices.Clone(s.servers)
	next[i] = cur
	return s.commit(next, s.nextID)
}

func (s *Store) index(id uint) int {
	return slices.IndexFunc(s.servers, func(srv Server) bool { return srv.ID == id })
}

func (s *Store) capErr() error {
	if s.lim.MaxServers == 1 {
		return fmt.Errorf("%w: this router runs at most 1 server", ErrServerCap)
	}
	return fmt.Errorf("%w: this router runs at most %d servers", ErrServerCap, s.lim.MaxServers)
}

// commit refuses only changes that grow an over-budget total, so a lowered limit locks nothing.
func (s *Store) commit(next []Server, nextID uint) error {
	if after, before := allocated(next), allocated(s.servers); after > s.lim.StreamBudget && after > before {
		return fmt.Errorf("%w: %d of %d streams would be allocated", ErrStreamBudget, after, s.lim.StreamBudget)
	}
	f := fileForm{Version: 1, NextID: nextID}
	for _, srv := range next {
		f.Servers = append(f.Servers, stored(srv))
	}
	if err := save(s.path, &f); err != nil {
		return err
	}
	s.servers, s.nextID = next, nextID
	return nil
}

func allocated(ss []Server) int {
	n := 0
	for _, srv := range ss {
		n += srv.Record.MaxStreams
	}
	return n
}

func clone(s Server) Server {
	s.Users = slices.Clone(s.Users)
	base := make(map[string]hubnode.Usage, len(s.Baseline))
	for k, v := range s.Baseline {
		base[k] = v
	}
	s.Baseline = base
	if s.Record.LastSeenAt != nil {
		t := *s.Record.LastSeenAt
		s.Record.LastSeenAt = &t
	}
	if s.Record.KeysRotatedAt != nil {
		t := *s.Record.KeysRotatedAt
		s.Record.KeysRotatedAt = &t
	}
	return s
}

// save writes atomically: a crash leaves the old file or the new one, never half.
func save(path string, f *fileForm) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".servers-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// Records adapts the store to relayops.Store.
func (s *Store) Records() relayops.Store { return records{s} }

type records struct{ s *Store }

func (r records) Get(_ context.Context, id uint) (*relayops.Record, error) {
	srv, ok := r.s.Get(id)
	if !ok {
		return nil, relayops.ErrNotFound
	}
	rec := srv.Record
	return &rec, nil
}

func (r records) Save(_ context.Context, rec *relayops.Record) error {
	err := r.s.SaveRecord(rec)
	if errors.Is(err, ErrNotFound) {
		return relayops.ErrNotFound
	}
	return err
}

func (r records) Delete(_ context.Context, id uint) error { return r.s.Delete(id) }

// --- the file's own form ---

// relayops.Record hides the link secrets from JSON, so the file spells every field out.
type fileForm struct {
	Version int            `json:"version"`
	NextID  uint           `json:"next_id"`
	Servers []storedServer `json:"servers"`
}

type storedServer struct {
	ID        uint                   `json:"id"`
	Name      string                 `json:"name"`
	Host      string                 `json:"host"`
	CreatedAt time.Time              `json:"created_at"`
	Cover     string                 `json:"cover,omitempty"`
	Record    storedRecord           `json:"record"`
	Users     []User                 `json:"users"`
	Baseline  map[string]storedUsage `json:"baseline,omitempty"`
}

type storedUsage struct {
	Up   uint64 `json:"up"`
	Down uint64 `json:"down"`
}

type storedRecord struct {
	AgentID       string     `json:"agent_id"`
	HubPort       int        `json:"hub_port"`
	CapturePort   int        `json:"capture_port"`
	SSHPort       int        `json:"ssh_port"`
	SSHUser       string     `json:"ssh_user"`
	HostKey       string     `json:"host_key"`
	RealityPub    string     `json:"reality_pub"`
	ShortID       string     `json:"short_id"`
	Token         string     `json:"token"`
	NextSeed      string     `json:"next_seed,omitempty"` // a rotation the agent may hold already
	KeysRotatedAt *time.Time `json:"keys_rotated_at,omitempty"`
	Disguise      string     `json:"disguise"`
	MaxStreams    int        `json:"max_streams"`
	Arch          string     `json:"arch"`
	AgentVersion  string     `json:"agent_version"`
	TelemtVersion string     `json:"telemt_version"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
}

func stored(s Server) storedServer {
	r := s.Record
	out := storedServer{
		ID: s.ID, Name: s.Name, Host: s.Host, CreatedAt: s.CreatedAt, Cover: s.Cover, Users: s.Users,
		Record: storedRecord{
			AgentID: r.AgentID, HubPort: r.HubPort, CapturePort: r.CapturePort, SSHPort: r.SSHPort, SSHUser: r.SSHUser,
			HostKey: r.HostKey, RealityPub: r.RealityPub, ShortID: r.ShortID, Token: r.Token, NextSeed: r.NextSeed, KeysRotatedAt: r.KeysRotatedAt,
			Disguise: r.Disguise, MaxStreams: r.MaxStreams, Arch: r.Arch,
			AgentVersion: r.AgentVersion, TelemtVersion: r.TelemtVersion, LastSeenAt: r.LastSeenAt,
		},
	}
	if out.Users == nil {
		out.Users = []User{}
	}
	if len(s.Baseline) > 0 {
		out.Baseline = make(map[string]storedUsage, len(s.Baseline))
		for k, v := range s.Baseline {
			out.Baseline[k] = storedUsage{Up: v.Up, Down: v.Down}
		}
	}
	return out
}

func (ss storedServer) server() Server {
	r := ss.Record
	out := Server{
		ID: ss.ID, Name: ss.Name, Host: ss.Host, CreatedAt: ss.CreatedAt, Cover: ss.Cover, Users: ss.Users,
		Record: relayops.Record{
			NodeID: ss.ID, AgentID: r.AgentID, HubPort: r.HubPort, CapturePort: r.CapturePort, SSHPort: r.SSHPort, SSHUser: r.SSHUser,
			HostKey: r.HostKey, RealityPub: r.RealityPub, ShortID: r.ShortID, Token: r.Token, NextSeed: r.NextSeed, KeysRotatedAt: r.KeysRotatedAt,
			Disguise: r.Disguise, MaxStreams: r.MaxStreams, Arch: r.Arch,
			AgentVersion: r.AgentVersion, TelemtVersion: r.TelemtVersion, LastSeenAt: r.LastSeenAt,
		},
		Baseline: make(map[string]hubnode.Usage, len(ss.Baseline)),
	}
	for k, v := range ss.Baseline {
		out.Baseline[k] = hubnode.Usage{Up: v.Up, Down: v.Down}
	}
	return out
}
