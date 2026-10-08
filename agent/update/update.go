// Package update replaces the agent's and telemt's binaries over the link and
// takes the old one back if the new one does not prove itself. A remote VPS with
// a broken agent is unreachable (SSH is removed after install), so every swap
// keeps the previous binary and has a way back that needs nobody's help:
//
//   - agent: the new binary boots in "trial". It must reach the hub and receive
//     update.commit within TrialWindow, and must not crash-loop; otherwise the
//     previous binary is moved back and the service restarts into it.
//   - telemt: the agent restarts it and checks it came up within HealthWindow;
//     if not, the previous binary is moved back.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
)

const (
	TrialWindow  = 5 * time.Minute
	MaxBoots     = 3               // a trial binary that starts more often than this is crash-looping
	HealthWindow = 2 * time.Minute // telemt's own start-up can take a minute
	MaxSize      = 64 << 20
)

var (
	ErrRolledBack = errors.New("update: the new binary failed its check and the previous one was restored")
	ErrBadUpdate  = errors.New("update: rejected")
)

// Paths locates everything an update touches. Binaries are staged next to their
// destination so the final swap is a same-filesystem rename.
type Paths struct {
	AgentBin  string // the running agent
	TelemtBin string
	TrialFile string // present while an agent trial is open
}

func prev(bin string) string  { return bin + ".prev" }
func stage(bin string) string { return filepath.Join(filepath.Dir(bin), "."+filepath.Base(bin)+".new") }

// Receive streams the body of an update call to disk and checks it against the
// size and hash in the header. Nothing is left behind on failure.
func Receive(r io.Reader, req ctrl.UpdateRequest, dest string) (string, error) {
	if req.Size <= 0 || req.Size > MaxSize {
		return "", fmt.Errorf("%w: size %d", ErrBadUpdate, req.Size)
	}
	if len(req.SHA256) != 64 {
		return "", fmt.Errorf("%w: missing sha256", ErrBadUpdate)
	}
	// a fresh name each time: two uploads must never write into one file
	f, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".new-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(path)
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, req.Size+1))
	if err != nil {
		return fail(err)
	}
	if n != req.Size {
		return fail(fmt.Errorf("%w: got %d bytes, header said %d", ErrBadUpdate, n, req.Size))
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, req.SHA256) {
		return fail(fmt.Errorf("%w: sha256 mismatch", ErrBadUpdate))
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, os.Chmod(path, 0o755)
}

// Smoke runs the staged binary with a version flag; a binary that cannot even
// do that never gets swapped in.
func Smoke(ctx context.Context, path string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s %s: %v: %s", ErrBadUpdate, filepath.Base(path), strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// midSwap runs between keeping the old binary and moving the new one in; tests crash there.
var midSwap = func() {}

// swap keeps the old bin as bin.prev, then renames next over bin: there is a whole binary at bin throughout.
func swap(bin, next string) error {
	if _, err := os.Stat(bin); err == nil {
		if err := keepPrev(bin); err != nil {
			return err
		}
	}
	midSwap()
	return os.Rename(next, bin)
}

// keepPrev makes bin.prev a second name for bin, or a copy where links do not work.
func keepPrev(bin string) error {
	if err := os.Remove(prev(bin)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if os.Link(bin, prev(bin)) == nil {
		return nil
	}
	return copyFile(bin, prev(bin))
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := to + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, to)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// restore moves bin.prev back over bin.
func restore(bin string) error {
	if _, err := os.Stat(prev(bin)); err != nil {
		return fmt.Errorf("update: no previous binary at %s", prev(bin))
	}
	return os.Rename(prev(bin), bin)
}

// --- agent: trial boot ---

type Trial struct {
	Version string    `json:"version"`
	Started time.Time `json:"started"`
	Boots   int       `json:"boots"`
}

// StartAgentTrial swaps the staged agent in and opens the trial. The caller then
// exits so the service manager starts the new binary.
func StartAgentTrial(p Paths, staged, version string) error {
	if err := swap(p.AgentBin, staged); err != nil {
		return err
	}
	if err := writeTrial(p.TrialFile, Trial{Version: version, Started: time.Now()}); err != nil {
		_ = restore(p.AgentBin)
		return err
	}
	return nil
}

// Boot is called first thing at agent start. In a trial it counts the boot and
// rolls back when the binary has crash-looped or its window ran out; the caller
// must exit after a rollback so the service restarts into the old binary.
func Boot(p Paths, now time.Time) (trial *Trial, rolledBack bool, err error) {
	t, err := readTrial(p.TrialFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		// an unreadable trial file is as good as an expired one
		return nil, true, rollback(p)
	}
	t.Boots++
	if t.Boots > MaxBoots || now.Sub(t.Started) > TrialWindow {
		return nil, true, rollback(p)
	}
	return &t, false, writeTrial(p.TrialFile, t)
}

// Expiry is how long a booted trial agent has left to be committed.
func (t Trial) Expiry() time.Time { return t.Started.Add(TrialWindow) }

// Commit closes the trial; the previous binary stays as a manual way back.
func Commit(p Paths) error {
	err := os.Remove(p.TrialFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// RollBack abandons the running trial (the window ran out while running).
func RollBack(p Paths) error { return rollback(p) }

func rollback(p Paths) error {
	rerr := restore(p.AgentBin)
	if err := os.Remove(p.TrialFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return rerr
}

func writeTrial(path string, t Trial) error {
	b, _ := json.Marshal(t)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readTrial(path string) (Trial, error) {
	var t Trial
	b, err := os.ReadFile(path)
	if err != nil {
		return t, err
	}
	return t, json.Unmarshal(b, &t)
}

// --- telemt: restart and verify ---

// UpdateTelemt swaps the staged telemt in, restarts it and waits for healthy to
// pass. If it never does, the previous binary goes back and telemt restarts again.
func UpdateTelemt(ctx context.Context, p Paths, staged string, restart func() error, healthy func(context.Context) error, window time.Duration) error {
	if err := swap(p.TelemtBin, staged); err != nil {
		return err
	}
	if err := restart(); err == nil {
		wctx, cancel := context.WithTimeout(ctx, window)
		defer cancel()
		for {
			if healthy(wctx) == nil {
				return nil
			}
			select {
			case <-time.After(250 * time.Millisecond):
				continue
			case <-wctx.Done():
			}
			break
		}
	}
	if err := restore(p.TelemtBin); err != nil {
		return fmt.Errorf("update: telemt failed its check and could not be restored: %w", err)
	}
	_ = restart()
	return ErrRolledBack
}
