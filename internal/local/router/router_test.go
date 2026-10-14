package router

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateIsReadFromTheScriptsLines(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.Read(); ok || err != nil {
		t.Fatalf("before the script ran: %v %v", ok, err)
	}
	state := "port=41873\naddress=ABCD1234.sn.mynetname.net\npublic=203.0.113.5\nwan=10.0.3.15\ncloud=updated\ndomestic=yes\nscript=1\njunk\n"
	if err := os.WriteFile(filepath.Join(dir, "router", "state"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	st, ok, err := c.Read()
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	if st.Port != 41873 || st.Address != "abcd1234.sn.mynetname.net" || st.Public != "203.0.113.5" || st.WAN != "10.0.3.15" ||
		st.Cloud != "updated" || !st.Domestic || st.Script != 1 || st.At.IsZero() {
		t.Fatalf("state = %+v", st)
	}
	// RouterOS leaves empty values empty
	os.WriteFile(filepath.Join(dir, "router", "state"), []byte("port=41873\naddress=\npublic=\ndomestic=no\n"), 0o644)
	if st, _, _ := c.Read(); st.Domestic || st.Address != "" || st.Port != 41873 {
		t.Fatalf("state = %+v", st)
	}
}

func TestANewPortIsAskedForUntilTheScriptTakesIt(t *testing.T) {
	dir := t.TempDir()
	c, _ := New(dir)
	if _, ok := c.Asked(); ok {
		t.Fatal("asked before asking")
	}
	if err := c.AskNewPort(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "router", "request")); string(b) != "new-port\n" {
		t.Fatalf("request = %q", b)
	}
	if _, ok := c.Asked(); !ok {
		t.Fatal("not pending")
	}
	os.Remove(filepath.Join(dir, "router", "request")) // what the script does
	if _, ok := c.Asked(); ok {
		t.Fatal("still pending")
	}
}

func TestTheHostIsWrittenOnlyWhenItChanges(t *testing.T) {
	dir := t.TempDir()
	c, _ := New(dir)
	p := filepath.Join(dir, "router", "host")
	if err := c.SetHost("home.example.com"); err != nil {
		t.Fatal(err)
	}
	first, _ := os.Stat(p)
	os.Chtimes(p, first.ModTime().Add(-3600e9), first.ModTime().Add(-3600e9))
	if err := c.SetHost("home.example.com"); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(p); !again.ModTime().Equal(first.ModTime().Add(-3600e9)) {
		t.Fatal("rewritten without a change")
	}
	c.SetHost("")
	if b, _ := os.ReadFile(p); len(b) != 0 {
		t.Fatalf("host = %q", b)
	}
}
