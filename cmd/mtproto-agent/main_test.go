package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/nasnet-community/mtproto-remote-plugin/layout"
)

// The unit runs a bare `mtproto-agent run`, so the defaults must stay the layout.
func TestRunDefaultsAreTheInstalledLayout(t *testing.T) {
	p, err := parsePaths(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := paths{layout.StatePath, layout.DataDir, layout.TelemtBin, layout.AgentBin, layout.TrialFile, layout.UnitPath}
	if p != want || !p.installed() {
		t.Fatalf("defaults = %+v", p)
	}
}

// Boxes installed with an older unit still start the agent with GOMEMLIMIT=80MiB.
func TestRaiseMemLimitLiftsALowerOneOnly(t *testing.T) {
	was := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(was) })

	debug.SetMemoryLimit(80 << 20)
	raiseMemLimit(layout.MemLimitMiB << 20)
	if got := debug.SetMemoryLimit(-1); got != layout.MemLimitMiB<<20 {
		t.Errorf("after 80MiB: limit = %d", got)
	}
	debug.SetMemoryLimit(1 << 30)
	raiseMemLimit(layout.MemLimitMiB << 20)
	if got := debug.SetMemoryLimit(-1); got != 1<<30 {
		t.Errorf("a higher limit was lowered to %d", got)
	}
}

func TestRunFlagsMoveEveryPath(t *testing.T) {
	p, err := parsePaths([]string{"-state", "/s/state.json", "-data", "/s/data", "-telemt", "/s/telemt",
		"-bin", "/s/agent", "-trial", "/s/trial.json", "-unit", "/s/unit"})
	if err != nil {
		t.Fatal(err)
	}
	if p != (paths{"/s/state.json", "/s/data", "/s/telemt", "/s/agent", "/s/trial.json", "/s/unit"}) || p.installed() {
		t.Fatalf("paths = %+v", p)
	}
	if _, err := parsePaths([]string{"stray"}); err == nil {
		t.Fatal("a stray argument was accepted")
	}
}

// The agent's own removal takes everything it put down, an update's staging included.
func TestTeardownRemovesWhatTheAgentLeft(t *testing.T) {
	dir := t.TempDir()
	p := paths{dir + "/state.json", dir + "/data", dir + "/bin/telemt", dir + "/bin/agent", dir + "/data/trial.json", dir + "/unit"}
	os.MkdirAll(dir+"/data/telemt/run", 0o755)
	os.MkdirAll(dir+"/bin", 0o755)
	files := []string{p.state, p.trial, p.unit, p.telemt, p.telemt + ".prev", p.bin, p.bin + ".prev",
		dir + "/bin/.telemt.new-123", dir + "/bin/.agent.new-456", dir + "/data/telemt/config.toml"}
	for _, f := range files {
		os.WriteFile(f, []byte("x"), 0o600)
	}
	if err := teardown(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	for _, f := range append(files, dir+"/data/telemt") {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s survived", f)
		}
	}
}

type call struct {
	name string
	args []string
}

func recorder(fail map[string]bool) (*[]call, runner) {
	var calls []call
	return &calls, func(_ context.Context, name string, args ...string) error {
		calls = append(calls, call{name, args})
		if fail[name] {
			return errors.New(name + ": not here")
		}
		return nil
	}
}

// The unit's /etc is read-only and its cgroup dies with it, so the last of the
// removal runs as a unit of its own: stop and disable us, then the unit file,
// then the account, in that order.
func TestTheServiceIsRemovedFromOutsideItsOwnUnit(t *testing.T) {
	for _, keep := range []bool{false, true} {
		calls, run := recorder(nil)
		waited := false
		if err := removeService(context.Background(), run, keep, func() { waited = true }); err != nil {
			t.Fatal(err)
		}
		if len(*calls) != 1 || (*calls)[0].name != "systemd-run" {
			t.Fatalf("calls = %+v", *calls)
		}
		args := (*calls)[0].args
		for _, want := range []string{"--no-block", "--collect"} {
			if !slices.Contains(args, want) {
				t.Errorf("systemd-run without %s: %v", want, args)
			}
		}
		script := args[len(args)-1]
		if strings.Contains(script, ";") {
			t.Errorf("a ; in the script comes back from systemd as an escape warning carrying it: %s", script)
		}
		order := []string{"systemctl disable --now " + layout.UnitName, "rm -f " + layout.UnitPath, "rm -rf " + layout.DataDir}
		if !keep {
			order = append(order, "rm -rf "+layout.ConfDir, "userdel "+layout.TelemtUser)
		} else if strings.Contains(script, "userdel") || strings.Contains(script, "rm -rf "+layout.ConfDir) || !strings.Contains(script, layout.KeepUser) {
			// an SSH sweep after us must still see the mark
			t.Errorf("an account that was there before, or its mark, is deleted: %s", script)
		}
		order = append(order, "systemctl daemon-reload")
		last := -1
		for _, step := range order {
			i := strings.Index(script, step)
			if i < 0 || i < last {
				t.Fatalf("keep=%v: %q missing or out of order in %s", keep, step, script)
			}
			last = i
		}
		if !waited {
			t.Error("the agent did not wait to be stopped: an exit of its own makes Restart=always start it again")
		}
	}
}

// Without systemd-run the agent still makes sure it is not started again.
func TestWithoutSystemdRunTheAgentStillStopsForGood(t *testing.T) {
	calls, run := recorder(map[string]bool{"systemd-run": true})
	waited := false
	if err := removeService(context.Background(), run, false, func() { waited = true }); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range (*calls)[1:] {
		got = append(got, c.name+" "+strings.Join(c.args, " "))
	}
	if want := []string{"systemctl disable " + layout.UnitName, "systemctl stop --no-block " + layout.UnitName}; !slices.Equal(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	if !waited {
		t.Error("the agent did not wait for the stop it asked for")
	}
}

// The real commands, against stand-ins on PATH.
func TestRemoveServiceRunsTheRealCommands(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	tool := func(name, body string) {
		os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nfor a in \"$@\"; do printf '%s|' \"$a\" >> "+log+"; done; echo >> "+log+"\n"+body+"\n"), 0o755)
	}
	tool("systemd-run", "exit 0")
	tool("systemctl", "exit 0")
	t.Setenv("PATH", dir)
	if err := removeService(context.Background(), runCmd, false, func() {}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if strings.Count(string(b), "--no-block|") != 1 || !strings.Contains(string(b), "userdel "+layout.TelemtUser) || strings.Contains(string(b), ";") {
		t.Fatalf("systemd-run got %q", b)
	}
	os.Remove(log)
	tool("systemd-run", "exit 1")
	if err := removeService(context.Background(), runCmd, false, func() {}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(log)
	if !strings.Contains(string(b), "disable|"+layout.UnitName+"|\nstop|--no-block|"+layout.UnitName+"|") {
		t.Fatalf("fallback ran %q", b)
	}
}

func TestTheKeepUserMarkIsWhereTheInstallerPutsIt(t *testing.T) {
	p, _ := parsePaths(nil)
	if keepUserFile(p) != layout.KeepUser {
		t.Fatalf("%s != %s", keepUserFile(p), layout.KeepUser)
	}
}

// Dev builds all say 0.0.0-dev; the hash of the running binary tells them apart.
func TestBuildIsTheHashOfTheRunningBinary(t *testing.T) {
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got := build(); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("build() = %q, want the hash of %s", got, p)
	}
}
