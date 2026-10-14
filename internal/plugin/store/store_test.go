package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/hubnode"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops"
)

const (
	token   = "7f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	pub     = "a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0"
	shortID = "c0ffee00d00dfeed"
)

func record(id uint) *relayops.Record {
	return &relayops.Record{
		NodeID: id, AgentID: "agent-1", HubPort: 8443, CapturePort: 9443, SSHPort: 22, SSHUser: "root",
		HostKey: "SHA256:abc", RealityPub: pub, ShortID: shortID, Token: token,
		Disguise: "cover.example.com", MaxStreams: 2000, Arch: "amd64",
	}
}

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRoundTripAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := open(t, dir)
	id, err := s.Create(NewServer{Name: "Relay 1", Host: "203.0.113.9", MaxStreams: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRecord(record(id)); err != nil {
		t.Fatal(err)
	}
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	err = s.Mutate(id, func(srv *Server) error {
		srv.Users = append(srv.Users, User{Name: "alice", Secret: strings.Repeat("ab", 16), Enabled: true, MaxIPs: 3, ExpiresAt: exp, QuotaBytes: 1 << 30, Up: 10, Down: 20, CreatedAt: exp})
		srv.Baseline = map[string]hubnode.Usage{"alice": {Up: 100, Down: 200}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v, want 0700", di.Mode().Perm())
	}

	again := open(t, dir)
	got, ok := again.Get(id)
	if !ok {
		t.Fatal("server lost on reopen")
	}
	if got.Name != "Relay 1" || got.Host != "203.0.113.9" || got.CreatedAt.IsZero() {
		t.Fatalf("server = %+v", got)
	}
	r := got.Record
	if r.Token != token || r.RealityPub != pub || r.ShortID != shortID {
		t.Fatalf("link secrets did not survive: %+v", r)
	}
	if r.NodeID != id || r.AgentID != "agent-1" || r.HubPort != 8443 || r.Disguise != "cover.example.com" || r.Arch != "amd64" || r.MaxStreams != 2000 {
		t.Fatalf("record = %+v", r)
	}
	if got.CoverDomain() != "cover.example.com" {
		t.Errorf("cover = %q", got.CoverDomain())
	}
	if len(got.Users) != 1 {
		t.Fatalf("users = %+v", got.Users)
	}
	u := got.Users[0]
	if u.Name != "alice" || !u.Enabled || u.MaxIPs != 3 || !u.ExpiresAt.Equal(exp) || u.QuotaBytes != 1<<30 || u.Up != 10 || u.Down != 20 {
		t.Fatalf("user = %+v", u)
	}
	if got.Baseline["alice"] != (hubnode.Usage{Up: 100, Down: 200}) {
		t.Fatalf("baseline = %+v", got.Baseline)
	}
}

func TestSecretsNeverInRecordJSON(t *testing.T) {
	s := open(t, t.TempDir())
	id, _ := s.Create(NewServer{Name: "a", Host: "203.0.113.9"})
	if err := s.SaveRecord(record(id)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.Path())
	if !strings.Contains(string(raw), token) {
		t.Fatal("the file should hold the token: nothing else does")
	}
	srv, _ := s.Get(id)
	b, err := json.Marshal(srv.Record)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{token, pub, shortID} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("relayops.Record leaks %q in JSON: %s", secret, b)
		}
	}
}

func TestMissingAndCorruptFiles(t *testing.T) {
	s := open(t, t.TempDir())
	if len(s.List()) != 0 {
		t.Fatal("a missing file should be an empty store")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	os.WriteFile(path, []byte(`{"servers": [`), 0o600)
	_, err := Open(dir, Limits{})
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("corrupt file: err = %v, want one naming %s", err, path)
	}
}

func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	id, _ := s.Create(NewServer{Name: "a", Host: "203.0.113.9"})
	for i := 0; i < 20; i++ {
		s.Mutate(id, func(srv *Server) error { srv.Name = "n" + strings.Repeat("x", i); return nil })
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != FileName {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("dir holds %v", names)
	}
}

func TestServerCap(t *testing.T) {
	s, _ := Open(t.TempDir(), Limits{MaxServers: 1})
	if _, err := s.Create(NewServer{Name: "a", Host: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(NewServer{Name: "b", Host: "203.0.113.10"}); !errors.Is(err, ErrServerCap) {
		t.Fatalf("second server on a 1-server router: %v", err)
	}
	if lim := s.Limits(); lim.MaxServers != 1 || lim.StreamBudget != DefaultStreamBudget {
		t.Errorf("limits = %+v", lim)
	}
}

func TestStreamBudget(t *testing.T) {
	s := open(t, t.TempDir())
	a, err := s.Create(NewServer{Name: "a", Host: "203.0.113.9", MaxStreams: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(NewServer{Name: "b", Host: "203.0.113.10", MaxStreams: 2500}); !errors.Is(err, ErrStreamBudget) {
		t.Fatalf("4000+2500 > 6000 was accepted: %v", err)
	}
	b, err := s.Create(NewServer{Name: "b", Host: "203.0.113.10", MaxStreams: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Allocated(); got != 6000 {
		t.Fatalf("allocated = %d", got)
	}
	r := record(b)
	r.MaxStreams = 2500
	if err := s.SaveRecord(r); !errors.Is(err, ErrStreamBudget) {
		t.Fatalf("a record past the budget was saved: %v", err)
	}
	err = s.Mutate(a, func(srv *Server) error { srv.Record.MaxStreams = 4100; return nil })
	if !errors.Is(err, ErrStreamBudget) {
		t.Fatalf("a settings change past the budget: %v", err)
	}
	if srv, _ := s.Get(a); srv.Record.MaxStreams != 4000 {
		t.Fatalf("a refused change was kept: %d", srv.Record.MaxStreams)
	}
	// going down is always fine
	if err := s.Mutate(a, func(srv *Server) error { srv.Record.MaxStreams = 1000; return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestSaveRecordKeepsWhatTheManagerOwns(t *testing.T) {
	s := open(t, t.TempDir())
	id, _ := s.Create(NewServer{Name: "a", Host: "203.0.113.9", MaxStreams: 2000})
	stale := record(id)
	if err := s.SaveRecord(stale); err != nil {
		t.Fatal(err)
	}
	seen := time.Now().UTC()
	s.Mutate(id, func(srv *Server) error {
		srv.Record.Disguise, srv.Record.AgentVersion, srv.Record.LastSeenAt = "new.example.com", "1.2.3", &seen
		return nil
	})
	// a rotation saves the copy it read before the cover site moved
	rotated := *stale
	rotated.Token = strings.Repeat("e", 64)
	if err := s.SaveRecord(&rotated); err != nil {
		t.Fatal(err)
	}
	srv, _ := s.Get(id)
	if srv.Record.Token != rotated.Token {
		t.Fatal("the new token was not saved")
	}
	if srv.Record.Disguise != "new.example.com" || srv.Record.AgentVersion != "1.2.3" || srv.Record.LastSeenAt == nil {
		t.Fatalf("a stale save reverted newer fields: %+v", srv.Record)
	}
}

func TestMutateFailureChangesNothing(t *testing.T) {
	s := open(t, t.TempDir())
	id, _ := s.Create(NewServer{Name: "a", Host: "203.0.113.9"})
	boom := errors.New("no")
	if err := s.Mutate(id, func(srv *Server) error { srv.Name = "changed"; return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if srv, _ := s.Get(id); srv.Name != "a" {
		t.Fatalf("name = %q", srv.Name)
	}
	if err := s.Mutate(99, func(*Server) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestCopiesAreIndependent(t *testing.T) {
	s := open(t, t.TempDir())
	id, _ := s.Create(NewServer{Name: "a", Host: "203.0.113.9", Users: []User{{Name: "u", Secret: strings.Repeat("0", 32)}}})
	srv, _ := s.Get(id)
	srv.Users[0].Name = "mutated"
	srv.Baseline["x"] = hubnode.Usage{Up: 1}
	again, _ := s.Get(id)
	if again.Users[0].Name != "u" || len(again.Baseline) != 0 {
		t.Fatalf("a returned copy aliases the store: %+v", again)
	}
}

func TestConcurrentMutate(t *testing.T) {
	s := open(t, t.TempDir())
	id, _ := s.Create(NewServer{Name: "a", Host: "203.0.113.9", Users: []User{{Name: "u", Secret: strings.Repeat("0", 32)}}})
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Mutate(id, func(srv *Server) error { srv.Users[0].Up++; return nil })
		}()
	}
	wg.Wait()
	if srv, _ := s.Get(id); srv.Users[0].Up != 40 {
		t.Fatalf("lost updates: up = %d", srv.Users[0].Up)
	}
}

func TestDeleteAndIDs(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	a, _ := s.Create(NewServer{Name: "a", Host: "203.0.113.9"})
	if err := s.Delete(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(a); err != nil {
		t.Fatalf("a second delete should be a no-op: %v", err)
	}
	b, _ := open(t, dir).Create(NewServer{Name: "b", Host: "203.0.113.9"})
	if b == a {
		t.Fatal("a deleted server's id was reused")
	}
}

func TestRecordsAdapter(t *testing.T) {
	s := open(t, t.TempDir())
	rs := s.Records()
	ctx := context.Background()
	if _, err := rs.Get(ctx, 7); !errors.Is(err, relayops.ErrNotFound) {
		t.Fatalf("missing record: %v", err)
	}
	if err := rs.Save(ctx, record(7)); !errors.Is(err, relayops.ErrNotFound) {
		t.Fatalf("a record without a server: %v", err)
	}
	id, _ := s.Create(NewServer{Name: "a", Host: "203.0.113.9"})
	if err := rs.Save(ctx, record(id)); err != nil {
		t.Fatal(err)
	}
	r, err := rs.Get(ctx, id)
	if err != nil || r.Token != token || r.NodeID != id {
		t.Fatalf("record = %+v, %v", r, err)
	}
	if err := rs.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(id); ok {
		t.Fatal("delete through the adapter left the server")
	}
}

func TestUserState(t *testing.T) {
	now := time.Now()
	u := User{Enabled: true, QuotaBytes: 100, Up: 60, Down: 40}
	if !u.OverQuota() {
		t.Error("100 of 100 bytes is over quota")
	}
	u.QuotaBytes = 0
	if u.OverQuota() {
		t.Error("no quota is never over")
	}
	u.ExpiresAt = now.Add(-time.Minute)
	if !u.Expired(now) {
		t.Error("expired a minute ago")
	}
	u.ExpiresAt = time.Time{}
	if u.Expired(now) {
		t.Error("no expiry never expires")
	}
}

// A rotation whose answer was lost must outlive a restart, or the router can't dial the server again.
func TestAPendingRotationSurvivesAReopen(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	id, _ := s.Create(NewServer{Name: "a", Host: "203.0.113.9", MaxStreams: 100})
	seed := strings.Repeat("5e", 32)
	r := record(id)
	r.NextSeed = seed
	if err := s.Records().Save(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	got, err := open(t, dir).Records().Get(context.Background(), id)
	if err != nil || got.NextSeed != seed || got.Token != token {
		t.Fatalf("after a reopen: %+v err=%v", got, err)
	}
	srv, _ := s.Get(id)
	if b, _ := json.Marshal(srv.Record); strings.Contains(string(b), seed) {
		t.Fatalf("relayops.Record leaks the seed in JSON: %s", b)
	}

	// finished: it goes from the file too
	got.NextSeed = ""
	if err := s.Records().Save(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(s.Path()); strings.Contains(string(raw), seed) {
		t.Fatal("a finished rotation's seed stayed in the file")
	}
	if again, _ := open(t, dir).Get(id); again.Record.NextSeed != "" {
		t.Fatalf("seed = %q", again.Record.NextSeed)
	}
}
