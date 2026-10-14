package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
)

func req(b []byte) ctrl.UpdateRequest {
	h := sha256.Sum256(b)
	return ctrl.UpdateRequest{Kind: "agent", Size: int64(len(b)), SHA256: hex.EncodeToString(h[:])}
}

func script(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReceiveAcceptsGoodBodyAndRejectsEverythingElse(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "agent")
	body := []byte("#!/bin/sh\necho v2\n")

	path, err := Receive(bytes.NewReader(body), req(body), dest)
	if err != nil || read(t, path) != string(body) {
		t.Fatalf("good body: %v", err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", st.Mode())
	}
	os.Remove(path)

	bad := req(body)
	bad.SHA256 = strings.Repeat("0", 64)
	cases := map[string]struct {
		body []byte
		r    ctrl.UpdateRequest
	}{
		"wrong hash":     {body, bad},
		"short body":     {body[:5], req(body)},
		"long body":      {append(append([]byte{}, body...), 'x'), req(body)},
		"no size":        {body, ctrl.UpdateRequest{SHA256: req(body).SHA256}},
		"huge size":      {body, ctrl.UpdateRequest{Size: MaxSize + 1, SHA256: req(body).SHA256}},
		"missing sha256": {body, ctrl.UpdateRequest{Size: int64(len(body))}},
	}
	for name, c := range cases {
		if _, err := Receive(bytes.NewReader(c.body), c.r, dest); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, ".agent.new*")); len(left) != 0 {
			t.Errorf("%s: staged file left behind: %v", name, left)
			for _, f := range left {
				os.Remove(f)
			}
		}
	}
}

// Two uploads at once must not write into the same staging file.
func TestConcurrentReceivesStageSeparately(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "telemt")
	a, b := bytes.Repeat([]byte("a"), 1<<20), bytes.Repeat([]byte("b"), 1<<20)
	pa, pb := make(chan string, 1), make(chan string, 1)
	go func() { p, _ := Receive(bytes.NewReader(a), req(a), dest); pa <- p }()
	go func() { p, _ := Receive(bytes.NewReader(b), req(b), dest); pb <- p }()
	ga, gb := <-pa, <-pb
	if ga == "" || gb == "" || ga == gb {
		t.Fatalf("staged at %q and %q", ga, gb)
	}
	if read(t, ga) != string(a) || read(t, gb) != string(b) {
		t.Fatal("one upload ended up in the other's file")
	}
}

func TestSmoke(t *testing.T) {
	dir := t.TempDir()
	ok, bad, hang := filepath.Join(dir, "ok"), filepath.Join(dir, "bad"), filepath.Join(dir, "hang")
	script(t, ok, "exit 0")
	script(t, bad, "echo broken >&2; exit 3")
	script(t, hang, "exec sleep 60")
	if err := Smoke(context.Background(), ok, "version"); err != nil {
		t.Errorf("ok: %v", err)
	}
	if err := Smoke(context.Background(), bad, "version"); !errors.Is(err, ErrBadUpdate) || !strings.Contains(err.Error(), "broken") {
		t.Errorf("bad: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := Smoke(ctx, hang, "version"); err == nil {
		t.Error("a hanging binary passed")
	}
	if err := Smoke(context.Background(), filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing binary passed")
	}
}

func agentRig(t *testing.T) (Paths, string) {
	t.Helper()
	dir := t.TempDir()
	p := Paths{AgentBin: filepath.Join(dir, "mtproto-agent"), TrialFile: filepath.Join(dir, "trial.json")}
	script(t, p.AgentBin, "echo v1")
	staged := stage(p.AgentBin)
	script(t, staged, "echo v2")
	return p, staged
}

func TestAgentTrialCommitKeepsThePreviousBinary(t *testing.T) {
	p, staged := agentRig(t)
	if err := StartAgentTrial(p, staged, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, p.AgentBin), "v2") || !strings.Contains(read(t, prev(p.AgentBin)), "v1") {
		t.Fatal("swap did not put v2 in place with v1 as prev")
	}
	now := time.Now()
	tr, rolled, err := Boot(p, now)
	if err != nil || rolled || tr == nil || tr.Boots != 1 || tr.Version != "2.0.0" {
		t.Fatalf("first boot: %+v rolled=%v err=%v", tr, rolled, err)
	}
	if err := Commit(p); err != nil {
		t.Fatal(err)
	}
	if tr, rolled, _ := Boot(p, now); tr != nil || rolled {
		t.Fatalf("after commit the next boot is a normal one: %+v %v", tr, rolled)
	}
	if !strings.Contains(read(t, prev(p.AgentBin)), "v1") {
		t.Error("commit removed the previous binary")
	}
	if err := Commit(p); err != nil {
		t.Errorf("committing twice: %v", err)
	}
}

func TestCrashLoopingTrialRollsBack(t *testing.T) {
	p, staged := agentRig(t)
	StartAgentTrial(p, staged, "2.0.0")
	now := time.Now()
	for i := 1; i <= MaxBoots; i++ {
		if tr, rolled, err := Boot(p, now); err != nil || rolled || tr == nil {
			t.Fatalf("boot %d: %+v %v %v", i, tr, rolled, err)
		}
	}
	tr, rolled, err := Boot(p, now)
	if err != nil || !rolled || tr != nil {
		t.Fatalf("boot %d should roll back: %+v %v %v", MaxBoots+1, tr, rolled, err)
	}
	if !strings.Contains(read(t, p.AgentBin), "v1") {
		t.Fatal("v1 not restored")
	}
	if _, err := os.Stat(p.TrialFile); err == nil {
		t.Fatal("trial file survived the rollback")
	}
}

func TestExpiredTrialRollsBack(t *testing.T) {
	p, staged := agentRig(t)
	StartAgentTrial(p, staged, "2.0.0")
	_, rolled, err := Boot(p, time.Now().Add(TrialWindow+time.Second))
	if err != nil || !rolled || !strings.Contains(read(t, p.AgentBin), "v1") {
		t.Fatalf("rolled=%v err=%v bin=%q", rolled, err, read(t, p.AgentBin))
	}
}

func TestCorruptTrialFileRollsBack(t *testing.T) {
	p, staged := agentRig(t)
	StartAgentTrial(p, staged, "2.0.0")
	os.WriteFile(p.TrialFile, []byte("{garbage"), 0o600)
	_, rolled, err := Boot(p, time.Now())
	if err != nil || !rolled || !strings.Contains(read(t, p.AgentBin), "v1") {
		t.Fatalf("rolled=%v err=%v", rolled, err)
	}
}

func TestRollBackWhileRunning(t *testing.T) {
	p, staged := agentRig(t)
	StartAgentTrial(p, staged, "2.0.0")
	if err := RollBack(p); err != nil || !strings.Contains(read(t, p.AgentBin), "v1") {
		t.Fatalf("err=%v", err)
	}
}

func TestStartTrialRestoresIfItCannotRecordTheTrial(t *testing.T) {
	p, staged := agentRig(t)
	p.TrialFile = filepath.Join(t.TempDir(), "no", "such", "dir", "trial.json")
	if err := StartAgentTrial(p, staged, "2.0.0"); err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(read(t, p.AgentBin), "v1") {
		t.Fatal("v1 not restored after a failed trial start")
	}
}

func telemtRig(t *testing.T) (Paths, string) {
	t.Helper()
	dir := t.TempDir()
	p := Paths{TelemtBin: filepath.Join(dir, "telemt")}
	script(t, p.TelemtBin, "echo old")
	staged := stage(p.TelemtBin)
	script(t, staged, "echo new")
	return p, staged
}

func TestTelemtUpdateSucceedsWhenHealthy(t *testing.T) {
	p, staged := telemtRig(t)
	restarts := 0
	err := UpdateTelemt(context.Background(), p, staged,
		func() error { restarts++; return nil },
		func(context.Context) error { return nil }, time.Second)
	if err != nil || restarts != 1 || !strings.Contains(read(t, p.TelemtBin), "new") {
		t.Fatalf("err=%v restarts=%d", err, restarts)
	}
	if !strings.Contains(read(t, prev(p.TelemtBin)), "old") {
		t.Fatal("previous telemt not kept")
	}
}

func TestTelemtUpdateRollsBackWhenItNeverComesUp(t *testing.T) {
	p, staged := telemtRig(t)
	restarts := 0
	err := UpdateTelemt(context.Background(), p, staged,
		func() error { restarts++; return nil },
		func(context.Context) error { return errors.New("no") }, 400*time.Millisecond)
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("err = %v", err)
	}
	if restarts != 2 {
		t.Errorf("restarts = %d, want 2 (new, then old)", restarts)
	}
	if !strings.Contains(read(t, p.TelemtBin), "old") {
		t.Fatal("old telemt not restored")
	}
}

func TestTelemtUpdateRollsBackWhenRestartFails(t *testing.T) {
	p, staged := telemtRig(t)
	n := 0
	err := UpdateTelemt(context.Background(), p, staged,
		func() error {
			n++
			if n == 1 {
				return errors.New("exec failed")
			}
			return nil
		},
		func(context.Context) error { return nil }, time.Second)
	if !errors.Is(err, ErrRolledBack) || !strings.Contains(read(t, p.TelemtBin), "old") {
		t.Fatalf("err=%v", err)
	}
}

// A power cut at any point of a swap leaves a whole binary where the service
// manager looks: the old one until the new one is in place.
func TestASwapCutShortLeavesABinaryInPlace(t *testing.T) {
	p, staged := agentRig(t)
	midSwap = func() { panic("power cut") }
	defer func() { midSwap = func() {} }()
	func() {
		defer func() { recover() }()
		swap(p.AgentBin, staged)
	}()
	b, err := os.ReadFile(p.AgentBin)
	if err != nil || !strings.Contains(string(b), "v1") {
		t.Fatalf("after a cut mid-swap the agent binary is %q, %v", b, err)
	}
	midSwap = func() {}
	if err := swap(p.AgentBin, staged); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, p.AgentBin), "v2") || !strings.Contains(read(t, prev(p.AgentBin)), "v1") {
		t.Fatal("a swap that ran again did not put v2 in place with v1 as prev")
	}
	// the kept binary is its own file, not the new one under another name
	a, _ := os.Stat(p.AgentBin)
	b2, _ := os.Stat(prev(p.AgentBin))
	if os.SameFile(a, b2) {
		t.Fatal("bin and bin.prev are one file")
	}
}

// Where hard links are refused the previous binary is copied, mode and all.
func TestKeepPrevCopyKeepsTheBinaryRunnable(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	script(t, bin, "echo v1")
	if err := copyFile(bin, prev(bin)); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(prev(bin))
	if err != nil || st.Mode().Perm() != 0o755 || read(t, prev(bin)) != read(t, bin) {
		t.Fatalf("copy: %v %v", st, err)
	}
	if _, err := os.Stat(prev(bin) + ".tmp"); err == nil {
		t.Fatal("the copy's temp file was left behind")
	}
}
