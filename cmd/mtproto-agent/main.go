// Command mtproto-agent is the program that runs on the VPS.
//
//	mtproto-agent run [flags]          the service (what systemd starts; flags move its files)
//	mtproto-agent version              print the version (the update smoke test)
//	mtproto-agent disguise-check HOST  can this VPS use HOST as its cover site? (JSON)
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/agent/daemon"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/disguise"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/state"
	"github.com/nasnet-community/mtproto-remote-plugin/agent/update"
	"github.com/nasnet-community/mtproto-remote-plugin/layout"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mtproto-agent run | version | disguise-check HOST")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "disguise-check":
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: mtproto-agent disguise-check HOST")
			os.Exit(2)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res := disguise.Checker{}.Check(ctx, os.Args[2])
		_ = json.NewEncoder(os.Stdout).Encode(res)
	case "run":
		if err := run(os.Args[2:]); err != nil {
			// stderr goes nowhere under the unit (StandardError=null), which is the
			// point: nothing about this box is written down. A failure is an exit code.
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

// paths are where the agent keeps its files. The defaults are the installed
// layout; a test points them at a scratch directory instead.
type paths struct {
	state, data, telemt, bin, trial, unit string
}

func (p paths) installed() bool {
	return p == paths{layout.StatePath, layout.DataDir, layout.TelemtBin, layout.AgentBin, layout.TrialFile, layout.UnitPath}
}

func parsePaths(args []string) (paths, error) {
	var p paths
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.StringVar(&p.state, "state", layout.StatePath, "state file")
	fs.StringVar(&p.data, "data", layout.DataDir, "data directory (telemt's config lives in data/telemt)")
	fs.StringVar(&p.telemt, "telemt", layout.TelemtBin, "telemt binary")
	fs.StringVar(&p.bin, "bin", layout.AgentBin, "this binary, as the updater swaps it")
	fs.StringVar(&p.trial, "trial", layout.TrialFile, "update trial marker")
	fs.StringVar(&p.unit, "unit", layout.UnitPath, "systemd unit file, removed on uninstall")
	if err := fs.Parse(args); err != nil {
		return p, err
	}
	if fs.NArg() > 0 {
		return p, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return p, nil
}

func run(args []string) error {
	p, err := parsePaths(args)
	if err != nil {
		return err
	}
	up := update.Paths{AgentBin: p.bin, TelemtBin: p.telemt, TrialFile: p.trial}

	// An update may have left this binary on trial. Before anything else, give up
	// on it if it has crash-looped or sat uncommitted too long.
	trial, rolledBack, err := update.Boot(up, time.Now())
	if err != nil {
		return fmt.Errorf("update trial: %w", err)
	}
	if rolledBack {
		return errors.New("the trial binary was rolled back; restarting into the previous one")
	}

	st, err := state.Load(p.state)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(st.HubPort))
	if err != nil {
		return fmt.Errorf("hub port: %w", err)
	}
	d, err := daemon.New(daemon.Config{
		StatePath: p.state, WorkDir: p.data,
		TelemtBin: p.telemt, AgentBin: p.bin, TrialFile: p.trial,
		Version: version, Build: build(), Listen: ln,
		Teardown: func(ctx context.Context) error { return teardown(ctx, p) },
	}, st)
	if err != nil {
		return err
	}
	d.SetTrial(trial)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return d.Run(ctx)
}

// build is the SHA-256 of this process's binary, so the hub can tell two builds of
// one version apart. /proc/self/exe is the file we started from, even after an update
// has put another one at our path.
func build() string {
	f, err := os.Open("/proc/self/exe")
	if err != nil {
		p, perr := os.Executable()
		if perr != nil {
			return ""
		}
		if f, err = os.Open(p); err != nil {
			return ""
		}
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// teardown is the agent's half of an uninstall over the link: remove what the
// installer put there, then have systemd stop us for good.
func teardown(ctx context.Context, p paths) error {
	_, err := os.Stat(keepUserFile(p))
	keepUser := err == nil
	for _, f := range []string{
		p.state, p.trial, p.unit,
		p.telemt, p.telemt + ".prev", p.bin + ".prev", p.telemt + ".prev.tmp", p.bin + ".prev.tmp",
	} {
		_ = os.Remove(f)
	}
	for _, bin := range []string{p.bin, p.telemt} { // staging an update cut short left behind
		left, _ := filepath.Glob(filepath.Join(filepath.Dir(bin), "."+filepath.Base(bin)+".new*"))
		for _, f := range left {
			_ = os.Remove(f)
		}
	}
	_ = os.RemoveAll(filepath.Join(p.data, "telemt"))
	_ = os.Remove(p.bin)
	if !p.installed() {
		return nil // a scratch run must not touch the box's user or service
	}
	return removeService(ctx, runCmd, keepUser, func() { time.Sleep(stopWait) })
}

// keepUserFile is the installer's mark on a telemt account that was there before it.
func keepUserFile(p paths) string {
	return filepath.Join(filepath.Dir(p.state), filepath.Base(layout.KeepUser))
}

type runner func(ctx context.Context, name string, args ...string) error

func runCmd(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

// stopWait bounds the wait for systemd to stop us after asking it to.
const stopWait = time.Minute

// removeService finishes in a unit of its own: /etc is read-only in ours
// (ProtectSystem=full) and whatever is left in our cgroup dies with us. That
// unit stops and disables us first, so Restart=always never runs a deleted binary.
func removeService(ctx context.Context, run runner, keepUser bool, wait func()) error {
	steps := []string{
		"systemctl disable --now " + layout.UnitName,
		"rm -f " + layout.UnitPath,
		"rm -rf " + layout.DataDir,
	}
	if keepUser {
		// leave only the mark, so an SSH sweep after us leaves the account too
		steps = append(steps, "for f in "+layout.ConfDir+"/* "+layout.ConfDir+"/.[!.]*", `do [ "$f" = `+layout.KeepUser+` ] || rm -rf "$f"`, "done")
	} else {
		steps = append(steps, "rm -rf "+layout.ConfDir, "userdel "+layout.TelemtUser)
	}
	steps = append(steps, "systemctl daemon-reload", "systemctl reset-failed "+layout.UnitName, "true")
	err := run(ctx, "systemd-run", "--no-block", "--collect", "--quiet", "--unit="+layout.UnitName+"-remove",
		"--description=mtproto-agent removal", "-p", "StandardOutput=null", "-p", "StandardError=null",
		"/bin/sh", "-c", strings.Join(steps, "\n")) // not ";": systemd escapes it in the unit, then warns with the whole script
	if err != nil {
		// the unit file and the account wait for an SSH sweep, but we are never started again
		_ = run(ctx, "systemctl", "disable", layout.UnitName)
		if err = run(ctx, "systemctl", "stop", "--no-block", layout.UnitName); err != nil {
			return err
		}
	}
	wait() // an exit of our own would be restarted; the stop is not
	return nil
}
