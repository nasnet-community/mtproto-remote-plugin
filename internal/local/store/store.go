// Package store keeps Local MTProto's one proxy, its users and their usage, in
// one JSON file under DATA_DIR.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/users"
)

const FileName = "proxy.json"

// Proxy is everything the owner set, and the counters' baseline.
type Proxy struct {
	CreatedAt time.Time
	Cover     string // "" until the first-run setup names one
	Users     []users.User
	Baseline  map[string]users.Usage // telemt's last cumulative counters
	Stopped   bool                   // kept off until the owner starts it again
	Options   users.ProxyOptions
	MaxConns  int    // 0: the router's default
	LinkHost  string // the address in links; "": the router's IP Cloud name
	LinkPort  int    // the outside port in links; 0: the router's own port
}

func (p Proxy) clone() Proxy {
	p.Users = slices.Clone(p.Users)
	p.Options = p.Options.Clone()
	base := make(map[string]users.Usage, len(p.Baseline))
	for k, v := range p.Baseline {
		base[k] = v
	}
	p.Baseline = base
	return p
}

type Store struct {
	path string

	mu sync.Mutex
	p  Proxy
}

// Open loads dir/proxy.json; a corrupt file is an error, never silently replaced.
func Open(dir string, now time.Time) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, FileName)}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.p = Proxy{CreatedAt: now.UTC(), Baseline: map[string]users.Usage{}}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f fileForm
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("store: %s is corrupt: %w", s.path, err)
	}
	s.p = f.proxy()
	return s, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Get() Proxy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.p.clone()
}

// Mutate is the one write path: fn edits a copy, kept only if fn and the write succeed.
func (s *Store) Mutate(fn func(*Proxy) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.p.clone()
	if err := fn(&next); err != nil {
		return err
	}
	b, err := json.MarshalIndent(stored(next), "", "  ")
	if err != nil {
		return err
	}
	if err := users.WriteFile(s.path, b); err != nil {
		return err
	}
	s.p = next
	return nil
}

// --- the file's own form ---

type fileForm struct {
	Version   int                    `json:"version"`
	CreatedAt time.Time              `json:"created_at"`
	Cover     string                 `json:"cover,omitempty"`
	Users     []users.User           `json:"users"`
	Baseline  map[string]storedUsage `json:"baseline,omitempty"`
	Stopped   bool                   `json:"proxy_stopped,omitempty"`
	Options   users.ProxyOptions     `json:"proxy_options,omitzero"`
	MaxConns  int                    `json:"max_conns,omitempty"`
	LinkHost  string                 `json:"link_host,omitempty"`
	LinkPort  int                    `json:"link_port,omitempty"`
}

type storedUsage struct {
	Up   uint64 `json:"up"`
	Down uint64 `json:"down"`
}

func stored(p Proxy) fileForm {
	f := fileForm{
		Version: 1, CreatedAt: p.CreatedAt, Cover: p.Cover, Users: p.Users, Stopped: p.Stopped, Options: p.Options,
		MaxConns: p.MaxConns, LinkHost: p.LinkHost, LinkPort: p.LinkPort,
	}
	if f.Users == nil {
		f.Users = []users.User{}
	}
	if len(p.Baseline) > 0 {
		f.Baseline = make(map[string]storedUsage, len(p.Baseline))
		for k, v := range p.Baseline {
			f.Baseline[k] = storedUsage{Up: v.Up, Down: v.Down}
		}
	}
	return f
}

func (f fileForm) proxy() Proxy {
	p := Proxy{
		CreatedAt: f.CreatedAt, Cover: f.Cover, Users: f.Users, Stopped: f.Stopped, Options: f.Options,
		MaxConns: f.MaxConns, LinkHost: f.LinkHost, LinkPort: f.LinkPort,
		Baseline: make(map[string]users.Usage, len(f.Baseline)),
	}
	for k, v := range f.Baseline {
		p.Baseline[k] = users.Usage{Up: v.Up, Down: v.Down}
	}
	return p
}
