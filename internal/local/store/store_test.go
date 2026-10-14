package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/users"
)

func TestTheProxySurvivesAReopen(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	s, err := Open(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if p := s.Get(); !p.CreatedAt.Equal(now) || p.Cover != "" || len(p.Users) != 0 {
		t.Fatalf("a new proxy = %+v", p)
	}
	err = s.Mutate(func(p *Proxy) error {
		p.Cover, p.MaxConns, p.LinkHost, p.LinkPort, p.Stopped = "www.example.com", 300, "home.example.com", 41873, true
		p.Users = append(p.Users, users.User{Name: "alice", Secret: strings.Repeat("a", 32), Enabled: true})
		p.Baseline["alice"] = users.Usage{Up: 10, Down: 20}
		p.Options.ExtraCoverNames = []string{"cdn.example.com"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	p := again.Get()
	if p.Cover != "www.example.com" || p.MaxConns != 300 || p.LinkHost != "home.example.com" || p.LinkPort != 41873 || !p.Stopped ||
		len(p.Users) != 1 || p.Baseline["alice"] != (users.Usage{Up: 10, Down: 20}) || p.Options.ExtraCoverNames[0] != "cdn.example.com" ||
		!p.CreatedAt.Equal(now) {
		t.Fatalf("reopened = %+v", p)
	}
}

func TestAFailedChangeKeepsTheOldProxy(t *testing.T) {
	s, err := Open(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := s.Mutate(func(p *Proxy) error { p.Cover = "x.example.com"; return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if s.Get().Cover != "" {
		t.Fatal("a refused change was kept")
	}
	// what Get hands out is a copy
	p := s.Get()
	p.Baseline["x"] = users.Usage{Up: 1}
	if len(s.Get().Baseline) != 0 {
		t.Fatal("Get shares its baseline")
	}
}

func TestACorruptFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, time.Now()); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("err = %v", err)
	}
}
