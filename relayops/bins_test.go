package relayops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nasnet-community/mtproto-remote-plugin/installer"
	"github.com/nasnet-community/mtproto-remote-plugin/release"
)

func tarball(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "dist/" + name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.WriteHeader(&tar.Header{Name: "dist/README", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi\n"))
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func serve(t *testing.T, files map[string][]byte) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if b, ok := files[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestTelemtIsDownloadedVerifiedUnpackedAndCached(t *testing.T) {
	bin := []byte("\x7fELF-pretend-telemt")
	tgz := tarball(t, "telemt", bin)
	srv, hits := serve(t, map[string][]byte{"/telemt.tgz": tgz})
	b := &Binaries{
		Dir: t.TempDir(), Client: func() *http.Client { return srv.Client() },
		TelemtPins: map[release.Arch]release.Asset{release.AMD64: {URL: srv.URL + "/telemt.tgz", SHA256: sum(tgz), Member: "telemt"}},
	}
	got, err := b.Telemt(context.Background(), release.AMD64)
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := got.Open()
	data, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(data, bin) || got.Size != int64(len(bin)) || got.SHA256 != sum(bin) {
		t.Fatalf("got %d bytes, sha %s", len(data), got.SHA256)
	}
	// the second use comes from disk
	if _, err := b.Telemt(context.Background(), release.AMD64); err != nil || atomic.LoadInt32(hits) != 1 {
		t.Fatalf("second fetch: %v hits=%d", err, *hits)
	}
}

func TestAgentBinaryNeedsNoUnpacking(t *testing.T) {
	bin := []byte("agent-bytes")
	srv, _ := serve(t, map[string][]byte{"/agent": bin})
	b := &Binaries{Dir: t.TempDir(), Client: func() *http.Client { return srv.Client() },
		AgentPins: map[release.Arch]release.Asset{release.ARM64: {URL: srv.URL + "/agent", SHA256: sum(bin)}}}
	got, err := b.Agent(context.Background(), release.ARM64)
	if err != nil || got.SHA256 != sum(bin) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestAnythingThatDoesNotMatchThePinIsRefusedAndNotKept(t *testing.T) {
	good := tarball(t, "telemt", []byte("real"))
	evil := tarball(t, "telemt", []byte("tampered"))
	srv, _ := serve(t, map[string][]byte{"/telemt.tgz": evil, "/missing-member.tgz": tarball(t, "other", []byte("x")), "/junk": []byte("not a gzip")})
	dir := t.TempDir()
	mk := func(path, pin, member string) *Binaries {
		return &Binaries{Dir: dir, Client: func() *http.Client { return srv.Client() },
			TelemtPins: map[release.Arch]release.Asset{release.AMD64: {URL: srv.URL + path, SHA256: pin, Member: member}}}
	}
	cases := map[string]*Binaries{
		"tampered download": mk("/telemt.tgz", sum(good), "telemt"),
		"not found":         mk("/nothing", sum(good), "telemt"),
		"member missing":    mk("/missing-member.tgz", sum(tarball(t, "other", []byte("x"))), "telemt"),
		"not a gzip":        mk("/junk", sum([]byte("not a gzip")), "telemt"),
		"no pin at all":     mk("/telemt.tgz", "", "telemt"),
	}
	for name, b := range cases {
		if _, err := b.Telemt(context.Background(), release.AMD64); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("failed downloads left files behind: %v", ents)
	}
}

func TestACachedFileThatRotsIsReplacedNotUploaded(t *testing.T) {
	bin := []byte("agent-bytes")
	srv, hits := serve(t, map[string][]byte{"/agent": bin})
	dir := t.TempDir()
	b := &Binaries{Dir: dir, Client: func() *http.Client { return srv.Client() },
		AgentPins: map[release.Arch]release.Asset{release.AMD64: {URL: srv.URL + "/agent", SHA256: sum(bin)}}}
	if _, err := b.Agent(context.Background(), release.AMD64); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, sum(bin)), []byte("corrupted on disk"), 0o600)
	got, err := b.Agent(context.Background(), release.AMD64)
	if err != nil || got.SHA256 != sum(bin) {
		t.Fatalf("the rotted cache entry was handed out: %+v %v", got, err)
	}
	if atomic.LoadInt32(hits) != 2 {
		t.Fatalf("expected a re-download, got %d fetches", *hits)
	}
}

func TestACachedTelemtIsReusedAndCheckedAgainstItsPin(t *testing.T) {
	bin := []byte("telemt-bytes")
	tgz := tarball(t, "telemt", bin)
	srv, hits := serve(t, map[string][]byte{"/t.tgz": tgz})
	dir := t.TempDir()
	b := &Binaries{Dir: dir, Client: func() *http.Client { return srv.Client() },
		TelemtPins: map[release.Arch]release.Asset{release.AMD64: {URL: srv.URL + "/t.tgz", SHA256: sum(tgz), Member: "telemt"}}}
	b.Telemt(context.Background(), release.AMD64)
	b.Telemt(context.Background(), release.AMD64)
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("a healthy unpacked cache entry was not reused (%d fetches)", *hits)
	}
	os.WriteFile(filepath.Join(dir, sum(tgz)), []byte("swapped"), 0o600)
	got, err := b.Telemt(context.Background(), release.AMD64)
	if err != nil || got.SHA256 != sum(bin) {
		t.Fatalf("swapped entry used: %+v %v", got, err)
	}
}

// Whoever can write the cache can write a sidecar too: only the pin is trusted.
func TestACacheEntrySwappedWithItsSidecarIsNotTrusted(t *testing.T) {
	agent := []byte("agent-bytes")
	telemt := []byte("telemt-bytes")
	tgz := tarball(t, "telemt", telemt)
	srv, hits := serve(t, map[string][]byte{"/agent": agent, "/t.tgz": tgz})
	dir := t.TempDir()
	b := &Binaries{Dir: dir, Client: func() *http.Client { return srv.Client() },
		AgentPins:  map[release.Arch]release.Asset{release.AMD64: {URL: srv.URL + "/agent", SHA256: sum(agent)}},
		TelemtPins: map[release.Arch]release.Asset{release.AMD64: {URL: srv.URL + "/t.tgz", SHA256: sum(tgz), Member: "telemt"}}}
	if _, err := b.Agent(context.Background(), release.AMD64); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Telemt(context.Background(), release.AMD64); err != nil {
		t.Fatal(err)
	}
	evil := []byte("evil")
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if filepath.Ext(p) == ".sha256" {
			os.WriteFile(p, []byte(sum(evil)), 0o600)
		} else {
			os.WriteFile(p, evil, 0o600)
		}
	}
	// a sidecar that never existed before is just as untrusted
	os.WriteFile(filepath.Join(dir, sum(agent)+".sha256"), []byte(sum(evil)), 0o600)
	os.WriteFile(filepath.Join(dir, sum(tgz)+".sha256"), []byte(sum(evil)), 0o600)
	for name, get := range map[string]func(context.Context, release.Arch) (installer.Bin, error){"agent": b.Agent, "telemt": b.Telemt} {
		bin, err := get(context.Background(), release.AMD64)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rc, _ := bin.Open()
		got, _ := io.ReadAll(rc)
		rc.Close()
		if bytes.Equal(got, evil) || bin.SHA256 == sum(evil) {
			t.Fatalf("%s: the swapped cache entry was handed out", name)
		}
	}
	if n := atomic.LoadInt32(hits); n != 4 {
		t.Fatalf("%d fetches, want 4: both swapped entries are fetched again", n)
	}
}

func TestUnreleasedAgentCannotBeInstalled(t *testing.T) {
	b := NewBinaries(t.TempDir(), func() *http.Client { return http.DefaultClient })
	if _, err := b.Agent(context.Background(), release.AMD64); err == nil {
		t.Skip("the agent hashes are pinned: this guards the pre-release state only")
	}
}

// A dev box with no agent release can install a local build, and only then.
func TestALocalAgentIsUsedOnlyForAnUnreleasedBuild(t *testing.T) {
	local := t.TempDir()
	dev := []byte("a local agent build")
	if err := os.WriteFile(filepath.Join(local, "mtproto-agent-linux-amd64"), dev, 0o755); err != nil {
		t.Fatal(err)
	}
	unreleased := &Binaries{Dir: t.TempDir(), LocalAgents: local,
		AgentPins: map[release.Arch]release.Asset{release.AMD64: {URL: "https://example.invalid/agent"}}}
	bin, err := unreleased.Agent(context.Background(), release.AMD64)
	if err != nil {
		t.Fatal(err)
	}
	if bin.SHA256 != sum(dev) || bin.Size != int64(len(dev)) {
		t.Fatalf("local bin = %s / %d bytes", bin.SHA256, bin.Size)
	}
	rc, _ := bin.Open()
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != string(dev) {
		t.Fatalf("local bin bytes = %q", got)
	}

	// a released build never reads the local folder
	pinned := []byte("the released agent")
	srv, _ := serve(t, map[string][]byte{"/agent": pinned})
	released := &Binaries{Client: srv.Client, Dir: t.TempDir(), LocalAgents: local,
		AgentPins: map[release.Arch]release.Asset{release.AMD64: {URL: srv.URL + "/agent", SHA256: sum(pinned)}}}
	if bin, err := released.Agent(context.Background(), release.AMD64); err != nil || bin.SHA256 != sum(pinned) {
		t.Fatalf("released build used %s, %v", bin.SHA256, err)
	}

	// an unreleased build with no local file says what is missing
	if _, err := unreleased.Agent(context.Background(), release.ARM64); err == nil {
		t.Fatal("an arch with no local build was accepted")
	}
}

// Pages ask which build an update would install: the pin, or a local build's hash, never a download.
func TestAgentBuildIsWhatAgentWouldHandOut(t *testing.T) {
	local := t.TempDir()
	path := filepath.Join(local, "mtproto-agent-linux-amd64")
	one, two := []byte("build one"), []byte("build two, a little longer")
	if err := os.WriteFile(path, one, 0o755); err != nil {
		t.Fatal(err)
	}
	unpinned := map[release.Arch]release.Asset{release.AMD64: {URL: "https://example.invalid/a"}, release.ARM64: {URL: "https://example.invalid/b"}}
	b := &Binaries{Dir: t.TempDir(), LocalAgents: local, AgentPins: unpinned}
	if got := b.AgentBuild(release.AMD64); got != sum(one) {
		t.Fatalf("local build = %s", got)
	}
	if err := os.WriteFile(path, two, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := b.AgentBuild(release.AMD64); got != sum(two) {
		t.Fatalf("a new local build was not noticed: %s", got)
	}
	if got := b.AgentBuild(release.ARM64); got != "" {
		t.Fatalf("no arm64 build, yet %q", got)
	}
	if got := (&Binaries{AgentPins: unpinned}).AgentBuild(release.AMD64); got != "" {
		t.Fatalf("nothing to install, yet %q", got)
	}
	pinned := &Binaries{LocalAgents: local, AgentPins: map[release.Arch]release.Asset{release.AMD64: {URL: "https://example.invalid/a", SHA256: strings.Repeat("AB", 32)}}}
	if got := pinned.AgentBuild(release.AMD64); got != strings.Repeat("ab", 32) {
		t.Fatalf("pinned build = %s", got)
	}
}
