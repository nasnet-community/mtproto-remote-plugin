// Package users is what both plugins keep about a proxy's users: who they are,
// their limits, their traffic and its history. It stays free of the remote
// plugin's SSH and link code, so the local plugin can share it.
package users

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxUsers = 500 // per proxy, the agent's design envelope
	MaxNote  = 80
)

// Invalid is a value the user typed that can't be used; the API answers 400 with it.
type Invalid string

func (e Invalid) Error() string { return string(e) }

func invalid(format string, a ...any) error { return Invalid(fmt.Sprintf(format, a...)) }

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
	Note       string    `json:"note,omitempty"`
	LastSeenAt time.Time `json:"last_seen_at,omitzero"` // last poll with traffic or a connection open
	Today      Day       `json:"today,omitzero"`        // the current UTC day; older days live in the history file
}

// Day is one user's traffic on one UTC day.
type Day struct {
	Date string `json:"date"` // 2006-01-02
	Up   uint64 `json:"up"`
	Down uint64 `json:"down"`
}

func (u User) OverQuota() bool { return u.QuotaBytes > 0 && u.Up+u.Down >= u.QuotaBytes }

func (u User) Expired(now time.Time) bool { return !u.ExpiresAt.IsZero() && !now.Before(u.ExpiresAt) }

// State is how the page sums a user up; used counts what memory holds too.
func (u User) State(used uint64, now time.Time) string {
	switch {
	case !u.Enabled:
		return "disabled"
	case u.Expired(now):
		return "expired"
	case u.QuotaBytes > 0 && used >= u.QuotaBytes:
		return "over-quota"
	}
	return "active"
}

// ValidName is ^[A-Za-z0-9_.-]{1,32}$ without regexp, and refuses names a URL path cannot carry ("..").
func ValidName(s string) bool {
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

// CleanNote keeps a note to one short line.
func CleanNote(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > MaxNote {
		return "", invalid("a note is at most %d characters", MaxNote)
	}
	return s, nil
}

// Renewed moves an expiry on by days: from the old date while it is still ahead,
// else from today, to the end of that UTC day.
func Renewed(exp, now time.Time, days int) time.Time {
	if exp.After(now) {
		return exp.AddDate(0, 0, days)
	}
	d := now.UTC().AddDate(0, 0, days)
	return time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, time.UTC)
}

// WriteFile writes atomically: a crash leaves the old file or the new one, never half.
func WriteFile(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
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
