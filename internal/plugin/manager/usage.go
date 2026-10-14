package manager

import (
	"errors"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/hubnode"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/store"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/users"
)

// Usage lives in memory between flash writes (users.Book): the agent is read every
// few seconds, the router's flash written every few minutes.

type (
	Sample      = users.Sample
	UserUsage   = users.UserUsage
	ServerUsage = users.ServerUsage
)

func (m *Manager) onUsage(e *entry, r hubnode.UsageReport) {
	pending := e.book.Add(r, m.cfg.Now())
	// a user who just used up a quota is cut off now, not at the next write
	if srv, ok := m.cfg.Store.Get(e.id); ok && users.CrossesQuota(srv.Users, pending) {
		if err := m.flush(e); err != nil {
			m.cfg.Log.Warn("could not store usage; it is kept in memory and written again", "server", e.id, "err", err)
			return
		}
		m.UsersChanged(e.id)
	}
}

// saver is one server's users and baseline in the store, as a book writes them.
type saver struct {
	m  *Manager
	id uint
}

func (s saver) Users() ([]users.User, bool) {
	srv, ok := s.m.cfg.Store.Get(s.id)
	return srv.Users, ok
}

func (s saver) Save(fn func(us []users.User, base map[string]users.Usage)) error {
	err := s.m.cfg.Store.Mutate(s.id, func(srv *store.Server) error {
		if srv.Baseline == nil {
			srv.Baseline = map[string]users.Usage{}
		}
		fn(srv.Users, srv.Baseline)
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return users.ErrGone
	}
	return err
}

func (m *Manager) flush(e *entry) error {
	return e.book.Flush(m.cfg.Now(), m.cfg.History, e.id, saver{m, e.id})
}

// Flush writes a server's usage now, before something that reads or resets it.
func (m *Manager) Flush(id uint) error {
	if e := m.entry(id); e != nil {
		return m.flush(e)
	}
	return nil
}

func (m *Manager) flushAll() {
	m.mu.Lock()
	es := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		es = append(es, e)
	}
	m.mu.Unlock()
	for _, e := range es {
		if err := m.flush(e); err != nil {
			m.cfg.Log.Warn("could not store usage; it is kept in memory and written again", "server", e.id, "err", err)
		}
	}
}

func (m *Manager) flushLoop(done <-chan struct{}) {
	t := time.NewTicker(m.cfg.FlushEvery)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			m.flushAll()
		}
	}
}

// ForgetUser drops what memory holds for a deleted user.
func (m *Manager) ForgetUser(id uint, name string) {
	if e := m.entry(id); e != nil {
		e.book.Forget(name)
	}
	if m.cfg.History != nil {
		m.cfg.History.Forget(id, name)
	}
}

// StatsStep is how often usage is read from the agents.
func (m *Manager) StatsStep() time.Duration {
	if m.cfg.StatsEvery > 0 {
		return m.cfg.StatsEvery
	}
	return 30 * time.Second // hubnode's own default
}

// liveFor is how long the last poll's connection counts stay believable.
func (m *Manager) liveFor() time.Duration {
	if m.cfg.StatsEvery > 0 {
		return 3 * m.cfg.StatsEvery
	}
	return 90 * time.Second
}

// Usage is a server's and its users' numbers right now.
func (m *Manager) Usage(srv store.Server) ServerUsage {
	var b *users.Book
	up := false
	if e := m.entry(srv.ID); e != nil {
		b, up = e.book, e.node.Snapshot().Link.Up > 0
	}
	return b.Usage(srv.Users, m.cfg.Now(), up, m.liveFor(), m.cfg.History, srv.ID)
}

// Traffic is a server's recent rate samples, oldest first.
func (m *Manager) Traffic(id uint) []Sample {
	if e := m.entry(id); e != nil {
		return e.book.Samples()
	}
	return nil
}
