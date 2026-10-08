package relayops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/installer"
	"github.com/nasnet-community/mtproto-remote-plugin/release"
)

// maxBinary caps what we will unpack or keep: telemt is ~17 MB, the agent ~10.
const maxBinary = 64 << 20

// BinarySource hands the installer the exact bytes to put on a VPS.
type BinarySource interface {
	Agent(ctx context.Context, arch release.Arch) (installer.Bin, error)
	Telemt(ctx context.Context, arch release.Arch) (installer.Bin, error)
	// AgentBuild is the SHA-256 Agent would hand out, without fetching it ("" when there is none).
	AgentBuild(arch release.Arch) string
}

// Binaries fetches the pinned releases through the foreign exit, checks every
// byte against the pin in the shared release package, and keeps them on disk.
// Nothing is ever downloaded by the VPS itself.
type Binaries struct {
	Client func() *http.Client // must leave through the foreign exit
	Dir    string

	AgentPins  map[release.Arch]release.Asset
	TelemtPins map[release.Arch]release.Asset

	// LocalAgents holds mtproto-agent-linux-<arch> builds for an unreleased module; a pinned agent ignores it.
	LocalAgents string

	mu sync.Mutex
	// its own lock: a download holds mu for minutes, and pages ask for this all the time
	localMu sync.Mutex
	local   localHash // the last local build hashed
}

type localHash struct {
	path, sum string
	size      int64
	mod       time.Time
}

// NewBinaries uses the pins compiled into the shared module.
func NewBinaries(dir string, client func() *http.Client) *Binaries {
	return &Binaries{Client: client, Dir: dir, AgentPins: release.Agent, TelemtPins: release.Telemt}
}

func (b *Binaries) Agent(ctx context.Context, arch release.Arch) (installer.Bin, error) {
	if as, ok := b.AgentPins[arch]; ok && len(as.SHA256) != 64 && b.LocalAgents != "" {
		return localBin(filepath.Join(b.LocalAgents, "mtproto-agent-linux-"+string(arch)))
	}
	return b.get(ctx, b.AgentPins, arch)
}

func (b *Binaries) AgentBuild(arch release.Arch) string {
	as, ok := b.AgentPins[arch]
	if !ok {
		return ""
	}
	if len(as.SHA256) == 64 {
		return strings.ToLower(as.SHA256)
	}
	if b.LocalAgents == "" {
		return ""
	}
	path := filepath.Join(b.LocalAgents, "mtproto-agent-linux-"+string(arch))
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	// pages poll this, so a build is hashed again only once it has changed
	b.localMu.Lock()
	defer b.localMu.Unlock()
	if c := b.local; c.path == path && c.size == fi.Size() && c.mod.Equal(fi.ModTime()) {
		return c.sum
	}
	bin, err := localBin(path)
	if err != nil {
		return ""
	}
	b.local = localHash{path: path, sum: bin.SHA256, size: fi.Size(), mod: fi.ModTime()}
	return bin.SHA256
}

// localBin serves the bytes it hashed, so a file changed underneath cannot reach a VPS.
func localBin(path string) (installer.Bin, error) {
	f, err := os.Open(path)
	if err != nil {
		return installer.Bin{}, fmt.Errorf("relay: no local agent build: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxBinary+1))
	if err != nil {
		return installer.Bin{}, err
	}
	if len(raw) > maxBinary {
		return installer.Bin{}, fmt.Errorf("relay: %s is too large", path)
	}
	h := sha256.Sum256(raw)
	return installer.Bin{
		Open:   func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil },
		Size:   int64(len(raw)),
		SHA256: hex.EncodeToString(h[:]),
	}, nil
}

func (b *Binaries) Telemt(ctx context.Context, arch release.Arch) (installer.Bin, error) {
	return b.get(ctx, b.TelemtPins, arch)
}

func (b *Binaries) get(ctx context.Context, pins map[release.Arch]release.Asset, arch release.Arch) (installer.Bin, error) {
	as, err := release.Lookup(pins, arch)
	if err != nil {
		return installer.Bin{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return installer.Bin{}, err
	}
	// the cache holds the asset as pinned, checked against the pin on every use; no sidecar is trusted
	path := filepath.Join(b.Dir, as.SHA256)
	os.Remove(path + ".sha256")
	if bin, err := b.fromCache(path, as); err == nil {
		return bin, nil
	}
	os.Remove(path) // missing, rotted or swapped: fetch it again
	raw, err := b.download(ctx, as)
	if err != nil {
		return installer.Bin{}, err
	}
	tmp, err := os.CreateTemp(b.Dir, ".dl-*")
	if err != nil {
		return installer.Bin{}, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return installer.Bin{}, err
	}
	if err := tmp.Close(); err != nil {
		return installer.Bin{}, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return installer.Bin{}, err
	}
	bin, err := b.fromCache(path, as)
	if err != nil {
		os.Remove(path) // pinned, but not what we need (no such member): keep nothing
		return installer.Bin{}, err
	}
	return bin, nil
}

var errNotPinned = errors.New("relay: the cached file does not match its pin")

// fromCache checks the pin and unpacks in one pass, so what it hands out came from verified bytes.
func (b *Binaries) fromCache(path string, as release.Asset) (installer.Bin, error) {
	f, err := os.Open(path)
	if err != nil {
		return installer.Bin{}, err
	}
	defer f.Close()
	h := sha256.New()
	src := io.TeeReader(io.LimitReader(f, maxBinary+1), h)
	pinned := func() bool { return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), as.SHA256) }
	if as.Member == "" {
		n, err := io.Copy(io.Discard, src)
		if err != nil {
			return installer.Bin{}, err
		}
		if !pinned() {
			return installer.Bin{}, errNotPinned
		}
		return installer.Bin{Open: func() (io.ReadCloser, error) { return os.Open(path) }, Size: n, SHA256: strings.ToLower(as.SHA256)}, nil
	}
	tmp, err := os.CreateTemp(b.Dir, ".unpack-*")
	if err != nil {
		return installer.Bin{}, err
	}
	defer os.Remove(tmp.Name())
	mh := sha256.New()
	n, err := unpack(src, as.Member, io.MultiWriter(tmp, mh))
	if err == nil {
		_, err = io.Copy(io.Discard, src) // the hash has to cover the whole file
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return installer.Bin{}, err
	}
	if !pinned() {
		return installer.Bin{}, errNotPinned
	}
	out := path + "." + as.Member
	if err := os.Rename(tmp.Name(), out); err != nil {
		return installer.Bin{}, err
	}
	return installer.Bin{Open: func() (io.ReadCloser, error) { return os.Open(out) }, Size: n, SHA256: hex.EncodeToString(mh.Sum(nil))}, nil
}

// download returns the asset's bytes, only if they match the pin.
func (b *Binaries) download(ctx context.Context, as release.Asset) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, as.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.Client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("relay: fetching %s: %w", as.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relay: %s answered %s", as.URL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBinary+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBinary {
		return nil, errors.New("relay: the download is larger than any release we pin")
	}
	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), as.SHA256) {
		return nil, fmt.Errorf("relay: %s does not match its pinned hash", as.URL)
	}
	return data, nil
}

func unpack(r io.Reader, member string, w io.Writer) (int64, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("relay: %q is not in the archive", member)
		}
		if err != nil {
			return 0, err
		}
		if h.Typeflag != tar.TypeReg || filepath.Base(h.Name) != member {
			continue
		}
		if h.Size > maxBinary {
			return 0, errors.New("relay: the binary in the archive is too large")
		}
		return io.Copy(w, io.LimitReader(tr, maxBinary))
	}
}
