// Package state is the agent's only persistent file: its credentials and the last
// desired state it accepted, so it can come back up with the hub unreachable.
// It holds no client addresses and nothing else.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
)

type File struct {
	AgentID     string   `json:"agent_id"`
	RealityPriv []byte   `json:"reality_priv"`
	ShortIDs    [][]byte `json:"short_ids"`
	Tokens      [][]byte `json:"tokens"` // two while a rotation settles
	HubPort     int      `json:"hub_port"`
	NextHubPort int      `json:"next_hub_port,omitempty"` // opened beside HubPort while the link moves
	CapturePort int      `json:"capture_port"`
	TelemtUID   uint32   `json:"telemt_uid"`
	TelemtGID   uint32   `json:"telemt_gid"`
	Disguise    string   `json:"disguise"` // REALITY dest/SNI until a pushed state changes it

	Desired *ctrl.State `json:"desired,omitempty"`
}

var ErrNotFound = errors.New("state: no state file")

func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("state: %s is corrupt: %w", path, err)
	}
	return &f, nil
}

// Save writes atomically: a crash leaves the old file or the new one, never half.
func Save(path string, f *File) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".state-*")
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
