package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
)

func TestRoundTripKeepsPermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	in := &File{AgentID: "a1", RealityPriv: []byte{1, 2}, ShortIDs: [][]byte{{9}}, Tokens: [][]byte{{3}, {4}}, HubPort: 8443, CapturePort: 9443, TelemtUID: 998,
		Desired: &ctrl.State{Rev: "r1", Port: 443, Users: []ctrl.User{{Name: "u", Secret: "s", Enabled: true}}}}
	if err := Save(p, in); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	out, err := Load(p)
	if err != nil || out.AgentID != "a1" || len(out.Tokens) != 2 || out.Desired.Rev != "r1" || out.Desired.Users[0].Name != "u" {
		t.Fatalf("out = %+v err=%v", out, err)
	}
}

func TestMissingAndCorruptAreDistinct(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "none")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	p := filepath.Join(dir, "bad")
	os.WriteFile(p, []byte("{not json"), 0o600)
	if _, err := Load(p); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("corrupt: %v", err)
	}
}

func TestSaveLeavesNoTempFilesAndReplacesInPlace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	for i := 0; i < 3; i++ {
		if err := Save(p, &File{AgentID: "a", HubPort: 8000 + i}); err != nil {
			t.Fatal(err)
		}
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("want only state.json, got %v", ents)
	}
	if f, _ := Load(p); f.HubPort != 8002 {
		t.Fatalf("not replaced: %+v", f)
	}
}
