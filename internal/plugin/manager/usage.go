package manager

import (
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/hubnode"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/store"
)

// Usage lives in memory between flash writes: the agent is read every few seconds,
// the router's flash written every few minutes (and at once when a quota runs out).

// Sample is a server's traffic rate over the poll that ended At.
type Sample struct {
	At       time.Time
	Up, Down float64 // bytes per second
}

const sampleCap = 60 // 15 minutes at 15 s

type usage struct {
	mu       sync.Mutex
	pending  map[string]hubnode.Usage // counted, not yet on flash
	cum      map[string]hubnode.Usage // the agent's counters at the last poll: the next stored baseline
	seen     map[string]time.Time     // last poll with traffic or a connection open
	live     map[string]hubnode.Live
	hasLive  bool
	polledAt time.Time
	samples  []Sample
	dirty    bool
}

func newUsage() *usage {
	return &usage{pending: map[string]hubnode.Usage{}, cum: map[string]hubnode.Usage{}, seen: map[string]time.Time{}}
}

func (m *Manager) onUsage(e *entry, r hubnode.UsageReport) {
	now := m.cfg.Now()
	u := e.usage
	u.mu.Lock()
	var up, down uint64
	for name, d := range r.Deltas {
		p := u.pending[name]
		u.pending[name] = hubnode.Usage{Up: p.Up + d.Up, Down: p.Down + d.Down}
		u.seen[name] = now
		up, down = up+d.Up, down+d.Down
		u.dirty = true
	}
	for name, c := range r.Cumulative {
		// a user at zero still needs a baseline row, or a restart forgets it was seen
		if old, ok := u.cum[name]; !ok || old != c {
			u.cum[name] = c
			u.dirty = true
		}
	}
	u.hasLive, u.live = r.HasLive, r.Live
	for name, l := range r.Live {
		if l.Connections > 0 {
			u.seen[name] = now
			u.dirty = true
		}
	}
	if !u.polledAt.IsZero() {
		if step := now.Sub(u.polledAt).Seconds(); step > 0 {
			u.samples = append(u.samples, Sample{At: now, Up: float64(up) / step, Down: float64(down) / step})
			if len(u.samples) > sampleCap {
				u.samples = u.samples[len(u.samples)-sampleCap:]
			}
		}
	}
	u.polledAt = now
	pending := maps.Clone(u.pending)
	u.mu.Unlock()

	// a user who just used up a quota is cut off now, not at the next write
	if srv, ok := m.cfg.Store.Get(e.id); ok && crossesQuota(srv, pending) {
		if err := m.flush(e); err != nil {
			m.cfg.Log.Warn("could not store usage; it is kept in memory and written again", "server", e.id, "err", err)
			return
		}
		m.UsersChanged(e.id)
	}
}

func crossesQuota(srv store.Server, pending map[string]hubnode.Usage) bool {
	for _, u := range srv.Users {
		d, ok := pending[u.Name]
		if ok && u.QuotaBytes > 0 && !u.OverQuota() && u.Up+u.Down+d.Up+d.Down >= u.QuotaBytes {
			return true
		}
	}
	return false
}

// flush writes what is counted in memory. The baseline goes in the same write, so
// a crash before it is recounted from the old baseline: nothing lost, nothing twice.
func (m *Manager) flush(e *entry) error {
	e.flushMu.Lock()
	defer e.flushMu.Unlock()
	u := e.usage
	u.mu.Lock()
	if !u.dirty {
		u.mu.Unlock()
		return nil
	}
	pending, cum, seen := maps.Clone(u.pending), maps.Clone(u.cum), maps.Clone(u.seen)
	u.mu.Unlock()

	today := store.DayOf(m.cfg.Now())
	// a finished day goes to the history before servers.json lets go of it
	if srv, ok := m.cfg.Store.Get(e.id); ok && m.cfg.History != nil {
		var rolled []store.Rolled
		for _, us := range srv.Users {
			if us.Today.Date != "" && us.Today.Date != today && us.Today.Up+us.Today.Down > 0 {
				rolled = append(rolled, store.Rolled{Server: e.id, User: us.Name, Day: us.Today})
			}
		}
		if err := m.cfg.History.Add(rolled, today); err != nil {
			return err
		}
	}
	err := m.cfg.Store.Mutate(e.id, func(s *store.Server) error {
		for i := range s.Users {
			us := &s.Users[i]
			if us.Today.Date != today {
				us.Today = store.Day{Date: today}
			}
			if d, ok := pending[us.Name]; ok {
				us.Up, us.Down = us.Up+d.Up, us.Down+d.Down
				us.Today.Up, us.Today.Down = us.Today.Up+d.Up, us.Today.Down+d.Down
			}
			if t, ok := seen[us.Name]; ok && t.After(us.LastSeenAt) {
				us.LastSeenAt = t.UTC().Truncate(time.Second)
			}
		}
		if s.Baseline == nil {
			s.Baseline = map[string]hubnode.Usage{}
		}
		for name, c := range cum {
			s.Baseline[name] = c
		}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil // removed meanwhile
	}
	if err != nil {
		return err
	}
	// only what was written: traffic counted during the write waits for the next one
	u.mu.Lock()
	for name, d := range pending {
		p := u.pending[name]
		p.Up, p.Down = p.Up-min(p.Up, d.Up), p.Down-min(p.Down, d.Down)
		if p == (hubnode.Usage{}) {
			delete(u.pending, name)
		} else {
			u.pending[name] = p
		}
	}
	for name, t := range seen {
		if u.seen[name].Equal(t) {
			delete(u.seen, name)
		}
	}
	u.dirty = len(u.pending) > 0 || len(u.seen) > 0 || !maps.Equal(u.cum, cum)
	u.mu.Unlock()
	return nil
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
		e.usage.mu.Lock()
		delete(e.usage.pending, name)
		delete(e.usage.seen, name)
		delete(e.usage.cum, name)
		e.usage.mu.Unlock()
	}
	if m.cfg.History != nil {
		m.cfg.History.Forget(id, name)
	}
}

// UserUsage is a user's numbers with what is not on flash yet folded in.
type UserUsage struct {
	Up, Down           uint64 // since the last reset
	TodayUp, TodayDown uint64
	MonthUp, MonthDown uint64 // the last 30 days, today included
	LastSeen           time.Time
	Live               bool // Connections and Devices are known
	Connections        int
	Devices            int
}

// ServerUsage sums a server's users.
type ServerUsage struct {
	Live                              bool
	UsersOnline, Connections, Devices int
	RateUp, RateDown                  float64
	TodayUp, TodayDown                uint64
	MonthUp, MonthDown                uint64
	UpdatedAt                         time.Time
	Users                             map[string]UserUsage
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
	now := m.cfg.Now()
	today := store.DayOf(now)
	first := store.FirstDay(today)
	out := ServerUsage{Users: map[string]UserUsage{}}
	var pending map[string]hubnode.Usage
	var seen map[string]time.Time
	var live map[string]hubnode.Live
	if e := m.entry(srv.ID); e != nil {
		linkUp := e.node.Snapshot().Link.Up > 0
		e.usage.mu.Lock()
		pending, seen, live = maps.Clone(e.usage.pending), maps.Clone(e.usage.seen), maps.Clone(e.usage.live)
		out.UpdatedAt = e.usage.polledAt
		fresh := !out.UpdatedAt.IsZero() && now.Sub(out.UpdatedAt) < m.liveFor() && linkUp
		out.Live = e.usage.hasLive && fresh
		if n := len(e.usage.samples); n > 0 && fresh {
			out.RateUp, out.RateDown = e.usage.samples[n-1].Up, e.usage.samples[n-1].Down
		}
		e.usage.mu.Unlock()
	}
	for _, us := range srv.Users {
		d := pending[us.Name]
		v := UserUsage{Up: us.Up + d.Up, Down: us.Down + d.Down, LastSeen: us.LastSeenAt, Live: out.Live}
		if t, ok := seen[us.Name]; ok && t.After(v.LastSeen) {
			v.LastSeen = t
		}
		if us.Today.Date == today {
			v.TodayUp, v.TodayDown = us.Today.Up, us.Today.Down
		}
		v.TodayUp, v.TodayDown = v.TodayUp+d.Up, v.TodayDown+d.Down
		v.MonthUp, v.MonthDown = v.TodayUp, v.TodayDown
		if m.cfg.History != nil {
			hu, hd := m.cfg.History.Sum(srv.ID, us.Name, first)
			v.MonthUp, v.MonthDown = v.MonthUp+hu, v.MonthDown+hd
		}
		// an earlier day not rolled over yet still belongs to the window
		if us.Today.Date != today && us.Today.Date >= first {
			v.MonthUp, v.MonthDown = v.MonthUp+us.Today.Up, v.MonthDown+us.Today.Down
		}
		if out.Live {
			l := live[us.Name]
			v.Connections, v.Devices = l.Connections, l.Devices
			if l.Connections > 0 {
				out.UsersOnline++
			}
			out.Connections += l.Connections
			out.Devices += l.Devices
		}
		out.TodayUp, out.TodayDown = out.TodayUp+v.TodayUp, out.TodayDown+v.TodayDown
		out.MonthUp, out.MonthDown = out.MonthUp+v.MonthUp, out.MonthDown+v.MonthDown
		out.Users[us.Name] = v
	}
	return out
}

// Traffic is a server's recent rate samples, oldest first.
func (m *Manager) Traffic(id uint) []Sample {
	e := m.entry(id)
	if e == nil {
		return nil
	}
	e.usage.mu.Lock()
	defer e.usage.mu.Unlock()
	return append([]Sample(nil), e.usage.samples...)
}
