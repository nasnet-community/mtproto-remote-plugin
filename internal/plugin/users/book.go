package users

import (
	"errors"
	"maps"
	"sync"
	"time"
)

// Usage lives in memory between flash writes: the proxy is read every few seconds,
// the router's flash written every few minutes (and at once when a quota runs out).

// Usage is traffic in bytes.
type Usage struct{ Up, Down uint64 }

// Live is one user's open connections and distinct client addresses.
type Live struct{ Connections, Devices int }

// Report is what one stats poll found.
type Report struct {
	Deltas     map[string]Usage // traffic since the previous report, per user
	Cumulative map[string]Usage // the proxy's counters now: persist these as the next baseline
	// Live is what each user has open now, when the proxy can say (HasLive); a user missing from it has nothing open
	Live    map[string]Live
	HasLive bool
}

// Stat is one user's counters as the proxy reports them.
type Stat struct {
	Name                 string
	Up, Down             uint64
	Connections, Devices int
}

// Fold turns counters into deltas against last (either going backwards means the
// proxy restarted) and returns the next baseline. A user missing from last is
// recorded, not counted: the alternative is to count traffic counted before a restart.
func Fold(last map[string]Usage, stats []Stat, live bool) (Report, map[string]Usage) {
	rep := Report{Deltas: map[string]Usage{}, Cumulative: map[string]Usage{}, Live: map[string]Live{}, HasLive: live}
	next := make(map[string]Usage, len(last)+len(stats))
	maps.Copy(next, last)
	for _, u := range stats {
		if live && (u.Connections > 0 || u.Devices > 0) {
			rep.Live[u.Name] = Live{Connections: u.Connections, Devices: u.Devices}
		}
		cur := Usage{Up: u.Up, Down: u.Down}
		rep.Cumulative[u.Name] = cur
		var d Usage
		if prev, seen := last[u.Name]; seen {
			if cur.Up < prev.Up || cur.Down < prev.Down {
				d = cur
			} else {
				d = Usage{Up: cur.Up - prev.Up, Down: cur.Down - prev.Down}
			}
		}
		next[u.Name] = cur
		if d.Up > 0 || d.Down > 0 {
			rep.Deltas[u.Name] = d
		}
	}
	return rep, next
}

// Sample is a proxy's traffic rate over the poll that ended At.
type Sample struct {
	At       time.Time
	Up, Down float64 // bytes per second
}

const sampleCap = 60 // 15 minutes at 15 s

// Book counts one proxy's usage in memory until it is written.
type Book struct {
	mu       sync.Mutex
	pending  map[string]Usage // counted, not yet on flash
	cum      map[string]Usage // the proxy's counters at the last poll: the next stored baseline
	seen     map[string]time.Time
	live     map[string]Live
	hasLive  bool
	polledAt time.Time
	samples  []Sample
	dirty    bool

	flushMu sync.Mutex // one write at a time
}

// NewBook starts a book; carried keeps a chart across a restart of the proxy's loop.
func NewBook(carried []Sample) *Book {
	return &Book{pending: map[string]Usage{}, cum: map[string]Usage{}, seen: map[string]time.Time{}, samples: carried}
}

// Add books a report and returns what memory holds, for a quota check.
func (b *Book) Add(r Report, now time.Time) map[string]Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	var up, down uint64
	for name, d := range r.Deltas {
		p := b.pending[name]
		b.pending[name] = Usage{Up: p.Up + d.Up, Down: p.Down + d.Down}
		b.seen[name] = now
		up, down = up+d.Up, down+d.Down
		b.dirty = true
	}
	for name, c := range r.Cumulative {
		// a user at zero still needs a baseline row, or a restart forgets it was seen
		if old, ok := b.cum[name]; !ok || old != c {
			b.cum[name] = c
			b.dirty = true
		}
	}
	b.hasLive, b.live = r.HasLive, r.Live
	for name, l := range r.Live {
		if l.Connections > 0 {
			b.seen[name] = now
			b.dirty = true
		}
	}
	if !b.polledAt.IsZero() {
		if step := now.Sub(b.polledAt).Seconds(); step > 0 {
			b.samples = append(b.samples, Sample{At: now, Up: float64(up) / step, Down: float64(down) / step})
			if len(b.samples) > sampleCap {
				b.samples = b.samples[len(b.samples)-sampleCap:]
			}
		}
	}
	b.polledAt = now
	return maps.Clone(b.pending)
}

// CrossesQuota says a user just used up a quota with what memory holds: cut them off now, not at the next write.
func CrossesQuota(us []User, pending map[string]Usage) bool {
	for _, u := range us {
		d, ok := pending[u.Name]
		if ok && u.QuotaBytes > 0 && !u.OverQuota() && u.Up+u.Down+d.Up+d.Down >= u.QuotaBytes {
			return true
		}
	}
	return false
}

// ErrGone is what a Saver returns when the proxy was removed meanwhile.
var ErrGone = errors.New("users: the proxy is gone")

// Saver reads and writes one proxy's users and counter baseline.
type Saver interface {
	Users() ([]User, bool)
	Save(fn func(us []User, base map[string]Usage)) error
}

// Flush writes what is counted in memory. The baseline goes in the same write, so
// a crash before it is recounted from the old baseline: nothing lost, nothing twice.
func (b *Book) Flush(now time.Time, hist *History, server uint, s Saver) error {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()
	b.mu.Lock()
	if !b.dirty {
		b.mu.Unlock()
		return nil
	}
	pending, cum, seen := maps.Clone(b.pending), maps.Clone(b.cum), maps.Clone(b.seen)
	b.mu.Unlock()

	today := DayOf(now)
	// a finished day goes to the history before the users let go of it
	if us, ok := s.Users(); ok && hist != nil {
		var rolled []Rolled
		for _, u := range us {
			if u.Today.Date != "" && u.Today.Date != today && u.Today.Up+u.Today.Down > 0 {
				rolled = append(rolled, Rolled{Server: server, User: u.Name, Day: u.Today})
			}
		}
		if err := hist.Add(rolled, today); err != nil {
			return err
		}
	}
	err := s.Save(func(us []User, base map[string]Usage) {
		for i := range us {
			u := &us[i]
			if u.Today.Date != today {
				u.Today = Day{Date: today}
			}
			if d, ok := pending[u.Name]; ok {
				u.Up, u.Down = u.Up+d.Up, u.Down+d.Down
				u.Today.Up, u.Today.Down = u.Today.Up+d.Up, u.Today.Down+d.Down
			}
			if t, ok := seen[u.Name]; ok && t.After(u.LastSeenAt) {
				u.LastSeenAt = t.UTC().Truncate(time.Second)
			}
		}
		maps.Copy(base, cum)
	})
	if errors.Is(err, ErrGone) {
		return nil // removed meanwhile
	}
	if err != nil {
		return err
	}
	// only what was written: traffic counted during the write waits for the next one
	b.mu.Lock()
	for name, d := range pending {
		p := b.pending[name]
		p.Up, p.Down = p.Up-min(p.Up, d.Up), p.Down-min(p.Down, d.Down)
		if p == (Usage{}) {
			delete(b.pending, name)
		} else {
			b.pending[name] = p
		}
	}
	for name, t := range seen {
		if b.seen[name].Equal(t) {
			delete(b.seen, name)
		}
	}
	b.dirty = len(b.pending) > 0 || len(b.seen) > 0 || !maps.Equal(b.cum, cum)
	b.mu.Unlock()
	return nil
}

// Forget drops what memory holds for a deleted user.
func (b *Book) Forget(name string) {
	b.mu.Lock()
	delete(b.pending, name)
	delete(b.seen, name)
	delete(b.cum, name)
	b.mu.Unlock()
}

// Samples is the recent rates, oldest first; a nil book has none.
func (b *Book) Samples() []Sample {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Sample(nil), b.samples...)
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

// ServerUsage sums a proxy's users.
type ServerUsage struct {
	Live                              bool
	UsersOnline, Connections, Devices int
	RateUp, RateDown                  float64
	TodayUp, TodayDown                uint64
	MonthUp, MonthDown                uint64
	UpdatedAt                         time.Time
	Users                             map[string]UserUsage
}

// Usage is a proxy's and its users' numbers right now. A nil book adds nothing from
// memory; up says the proxy is reachable, and liveFor how long a poll stays believable.
func (b *Book) Usage(us []User, now time.Time, up bool, liveFor time.Duration, hist *History, server uint) ServerUsage {
	today := DayOf(now)
	first := FirstDay(today)
	out := ServerUsage{Users: map[string]UserUsage{}}
	var pending map[string]Usage
	var seen map[string]time.Time
	var live map[string]Live
	if b != nil {
		b.mu.Lock()
		pending, seen, live = maps.Clone(b.pending), maps.Clone(b.seen), maps.Clone(b.live)
		out.UpdatedAt = b.polledAt
		fresh := !out.UpdatedAt.IsZero() && now.Sub(out.UpdatedAt) < liveFor && up
		out.Live = b.hasLive && fresh
		if n := len(b.samples); n > 0 && fresh {
			out.RateUp, out.RateDown = b.samples[n-1].Up, b.samples[n-1].Down
		}
		b.mu.Unlock()
	}
	for _, u := range us {
		d := pending[u.Name]
		v := UserUsage{Up: u.Up + d.Up, Down: u.Down + d.Down, LastSeen: u.LastSeenAt, Live: out.Live}
		if t, ok := seen[u.Name]; ok && t.After(v.LastSeen) {
			v.LastSeen = t
		}
		if u.Today.Date == today {
			v.TodayUp, v.TodayDown = u.Today.Up, u.Today.Down
		}
		v.TodayUp, v.TodayDown = v.TodayUp+d.Up, v.TodayDown+d.Down
		v.MonthUp, v.MonthDown = v.TodayUp, v.TodayDown
		if hist != nil {
			hu, hd := hist.Sum(server, u.Name, first)
			v.MonthUp, v.MonthDown = v.MonthUp+hu, v.MonthDown+hd
		}
		// an earlier day not rolled over yet still belongs to the window
		if u.Today.Date != today && u.Today.Date >= first {
			v.MonthUp, v.MonthDown = v.MonthUp+u.Today.Up, v.MonthDown+u.Today.Down
		}
		if out.Live {
			l := live[u.Name]
			v.Connections, v.Devices = l.Connections, l.Devices
			if l.Connections > 0 {
				out.UsersOnline++
			}
			out.Connections += l.Connections
			out.Devices += l.Devices
		}
		out.TodayUp, out.TodayDown = out.TodayUp+v.TodayUp, out.TodayDown+v.TodayDown
		out.MonthUp, out.MonthDown = out.MonthUp+v.MonthUp, out.MonthDown+v.MonthDown
		out.Users[u.Name] = v
	}
	return out
}
