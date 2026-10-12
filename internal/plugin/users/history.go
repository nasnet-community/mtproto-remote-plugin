package users

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

// HistoryFile keeps each user's past days apart from servers.json, so the
// frequent usage writes never rewrite a month of numbers.
const HistoryFile = "usage-history.json"

// HistoryDays is how many days a user's history covers, today included.
const HistoryDays = 30

// History is the finished days per server and user; the running day stays in User.Today.
type History struct {
	path string

	mu   sync.Mutex
	days map[uint]map[string][]Day // oldest first
}

// Rolled is one finished day to keep.
type Rolled struct {
	Server uint
	User   string
	Day    Day
}

func OpenHistory(dir string) (*History, error) {
	h := &History{path: filepath.Join(dir, HistoryFile), days: map[uint]map[string][]Day{}}
	b, err := os.ReadFile(h.path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	var f historyForm
	if err := json.Unmarshal(b, &f); err != nil {
		// numbers for a chart are not worth refusing to start over
		return h, fmt.Errorf("store: %s is corrupt, starting a new history: %w", h.path, err)
	}
	for _, srv := range f.Servers {
		h.days[srv.ID] = srv.Users
	}
	return h, nil
}

// DayOf is the UTC day t falls on.
func DayOf(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// FirstDay is the oldest day still in the window that ends on today.
func FirstDay(today string) string {
	t, err := time.Parse(time.DateOnly, today)
	if err != nil {
		return today
	}
	return t.AddDate(0, 0, -(HistoryDays - 1)).Format(time.DateOnly)
}

// Add keeps finished days and drops those older than the window, then saves. A day
// added twice (a write retried after a crash) replaces itself instead of counting twice.
func (h *History) Add(rolled []Rolled, today string) error {
	if len(rolled) == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range rolled {
		users := h.days[r.Server]
		if users == nil {
			users = map[string][]Day{}
			h.days[r.Server] = users
		}
		days := users[r.User]
		if i := slices.IndexFunc(days, func(d Day) bool { return d.Date == r.Day.Date }); i >= 0 {
			days[i] = r.Day
		} else {
			days = append(days, r.Day)
		}
		sort.Slice(days, func(i, j int) bool { return days[i].Date < days[j].Date })
		users[r.User] = days
	}
	h.pruneLocked(FirstDay(today))
	return h.saveLocked()
}

// Sum is a user's traffic over the days from first on (inclusive), today's running day excluded.
func (h *History) Sum(server uint, user, first string) (up, down uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, d := range h.days[server][user] {
		if d.Date >= first {
			up += d.Up
			down += d.Down
		}
	}
	return up, down
}

// Forget drops a user's history, or a whole server's with user "". It is saved with the next day added.
func (h *History) Forget(server uint, user string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if user == "" {
		delete(h.days, server)
		return
	}
	delete(h.days[server], user)
}

func (h *History) pruneLocked(first string) {
	for id, users := range h.days {
		for name, days := range users {
			days = slices.DeleteFunc(days, func(d Day) bool { return d.Date < first })
			if len(days) == 0 {
				delete(users, name)
			} else {
				users[name] = days
			}
		}
		if len(users) == 0 {
			delete(h.days, id)
		}
	}
}

type historyForm struct {
	Version int             `json:"version"`
	Servers []historyServer `json:"servers"`
}

type historyServer struct {
	ID    uint             `json:"id"`
	Users map[string][]Day `json:"users"`
}

func (h *History) saveLocked() error {
	f := historyForm{Version: 1}
	for id, users := range h.days {
		f.Servers = append(f.Servers, historyServer{ID: id, Users: users})
	}
	sort.Slice(f.Servers, func(i, j int) bool { return f.Servers[i].ID < f.Servers[j].ID })
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return WriteFile(h.path, b)
}
