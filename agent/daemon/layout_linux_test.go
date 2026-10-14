//go:build linux

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/agent/supervise"
)

// TestTelemtLayoutDrill runs a stand-in telemt as uid 998 against the real
// modes: it has to read its config through a data dir it cannot list, and
// work in a directory of its own (Go switches uid before it changes dir).
//
//	sudo MTPROTO_NETNS_DRILL=1 go test -count=1 -run TestTelemtLayoutDrill -v ./agent/daemon
func TestTelemtLayoutDrill(t *testing.T) {
	if os.Getenv("MTPROTO_NETNS_DRILL") != "1" || os.Geteuid() != 0 {
		t.Skip("set MTPROTO_NETNS_DRILL=1 and run as root")
	}
	work, err := os.MkdirTemp("", "layout-drill") // 0700, like the first installer made it
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)
	bin, err := os.MkdirTemp("", "layout-bin")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(bin)
	os.Chmod(bin, 0o755)
	stand := filepath.Join(bin, "telemt")
	os.WriteFile(stand, []byte(`#!/bin/sh
cat "$2" >/dev/null || { echo "LAYOUT cannot read the config"; exit 1; }
echo "LAYOUT cwd $(pwd) uid $(id -u)"
mkdir -p tlsfront && echo '{}' > telemt.limit.json || { echo "LAYOUT cannot write its working dir"; exit 1; }
ls "$(dirname "$2")/.." >/dev/null 2>&1 && echo "LAYOUT can list the data dir"
echo "LAYOUT ok"
exec sleep 600
`), 0o755)

	r := newRig(t, func(c *Config) {
		c.WorkDir, c.TelemtBin = work, stand
		c.NewProc = func(s supervise.Spec) Proc { s.MinBackoff = 50 * time.Millisecond; return supervise.New(s) }
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.d.proc.Run(ctx)
	r.apply(t, good("r1", user("u1")))

	var lines string
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(lines, "LAYOUT ok"); time.Sleep(50 * time.Millisecond) {
		lines = strings.Join(r.d.Logs().Tail(0), "\n")
		if strings.Contains(lines, "LAYOUT cannot") || time.Now().After(deadline) {
			t.Fatalf("telemt could not use its layout:\n%s", lines)
		}
	}
	run := filepath.Join(work, "telemt", "run")
	if !strings.Contains(lines, "LAYOUT cwd "+run+" uid 998") {
		t.Errorf("telemt ran elsewhere or as someone else:\n%s", lines)
	}
	if strings.Contains(lines, "LAYOUT can list the data dir") {
		t.Error("telemt can list the agent's data dir")
	}
	for path, want := range map[string]struct {
		mode     os.FileMode
		uid, gid uint32
	}{
		work:                          {0o711, 0, 0},
		filepath.Join(work, "telemt"): {0o750, 0, 998},
		run:                           {0o700, 998, 998},
		filepath.Join(work, "telemt", "config.toml"): {0o640, 0, 998},
	} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		sys := st.Sys().(*syscall.Stat_t)
		if st.Mode().Perm() != want.mode || sys.Uid != want.uid || sys.Gid != want.gid {
			t.Errorf("%s: %v %d:%d, want %v %d:%d", path, st.Mode().Perm(), sys.Uid, sys.Gid, want.mode, want.uid, want.gid)
		}
	}
	if err := r.d.proc.Stop(); err != nil {
		t.Fatal(err)
	}
}
