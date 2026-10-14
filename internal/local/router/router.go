// Package router is the plugin's side of the router script: they talk through
// small files in router/ under the data folder. The script runs every minute, and
// writes only what changed, to spare the router's flash.
//
//	router/state    the script: port, IP Cloud name, addresses (key=value lines)
//	router/request  the plugin: "new-port"; the script removes it once done
//	router/host     the plugin: an address set by hand, which the script resolves
package router

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// State is what the router script last wrote.
type State struct {
	Port     int    // the router's port for the proxy, forwarded from the domestic link
	Address  string // the router's IP Cloud name; "" without IP Cloud
	Public   string // the public address IP Cloud sees
	WAN      string // the domestic link's address on the router
	Cloud    string // IP Cloud's status, as RouterOS words it
	Domestic bool   // the router has a domestic link to forward from
	Script   int    // the script's version
	At       time.Time
}

// Channel reads and writes the files in dir.
type Channel struct{ dir string }

func New(dataDir string) (Channel, error) {
	c := Channel{dir: filepath.Join(dataDir, "router")}
	return c, os.MkdirAll(c.dir, 0o755)
}

func (c Channel) path(name string) string { return filepath.Join(c.dir, name) }

// Read is the router's last word; ok is false until the script has written once.
func (c Channel) Read() (st State, ok bool, err error) {
	f, err := os.Open(c.path("state"))
	if errors.Is(err, os.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil {
		st.At = fi.ModTime()
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, found := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !found {
			continue
		}
		switch v = strings.TrimSpace(v); k {
		case "port":
			st.Port, _ = strconv.Atoi(v)
		case "address":
			st.Address = strings.ToLower(v)
		case "public":
			st.Public = v
		case "wan":
			st.WAN = v
		case "cloud":
			st.Cloud = v
		case "domestic":
			st.Domestic = v == "yes"
		case "script":
			st.Script, _ = strconv.Atoi(v)
		}
	}
	return st, true, sc.Err()
}

// AskNewPort leaves the request for the script's next run.
func (c Channel) AskNewPort() error {
	return os.WriteFile(c.path("request"), []byte("new-port\n"), 0o644)
}

// Asked is when a request was left that the script hasn't taken yet.
func (c Channel) Asked() (time.Time, bool) {
	fi, err := os.Stat(c.path("request"))
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// SetHost tells the script the address links use, written only when it changes.
func (c Channel) SetHost(host string) error {
	want := []byte(host)
	if len(want) > 0 {
		want = append(want, '\n')
	}
	if old, err := os.ReadFile(c.path("host")); err == nil && bytes.Equal(old, want) {
		return nil
	}
	return os.WriteFile(c.path("host"), want, 0o644)
}
