package cover

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTargetChoice(t *testing.T) {
	tg := Target{Primary: "a.example", Domains: []string{"a.example", "b.example"}}
	for sni, want := range map[string]string{
		"b.example": "b.example:443",
		"B.Example": "b.example:443",
		"a.example": "a.example:443",
		"":          "a.example:443",
		"evil.test": "a.example:443", // a stranger's name is never dialled
	} {
		if got := tg.addr(sni); got != want {
			t.Errorf("sni %q: %s, want %s", sni, got, want)
		}
	}
	tg.Host, tg.Port = "mask.example", 8443
	if got := tg.addr("b.example"); got != "mask.example:8443" {
		t.Errorf("a set mask host wins: %s", got)
	}
	if got := (Target{}).addr("b.example"); got != "" {
		t.Errorf("no cover site yet: %q", got)
	}
}

// selfSigned is a certificate for name, so a real TLS handshake can cross the forwarder.
func selfSigned(t *testing.T, name string) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

type rig struct {
	sock   string
	f      *Forwarder
	mu     sync.Mutex
	dialed []string
}

// newRig serves a forwarder on a socket, dialling up for every address.
func newRig(t *testing.T, max int, up func(addr string) (net.Conn, error)) *rig {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cover") // socket paths are short on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	r := &rig{sock: filepath.Join(dir, "mask.sock")}
	r.f = &Forwarder{Max: max, Hello: 300 * time.Millisecond, Dial: func(_ context.Context, addr string) (net.Conn, error) {
		r.mu.Lock()
		r.dialed = append(r.dialed, addr)
		r.mu.Unlock()
		return up(addr)
	}}
	r.f.Set(Target{Primary: "a.example", Domains: []string{"a.example", "b.example"}})
	ln, err := Listen(r.sock, -1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go r.f.Serve(ln)
	return r
}

func (r *rig) addrs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dialed...)
}

func TestAHandshakeCrossesTheForwarderToTheSiteItNames(t *testing.T) {
	cert := selfSigned(t, "b.example")
	r := newRig(t, 0, func(string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			s := tls.Server(b, &tls.Config{Certificates: []tls.Certificate{cert}})
			if s.Handshake() == nil {
				s.Write([]byte("from the site"))
			}
			s.Close()
		}()
		return a, nil
	})
	c, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tc := tls.Client(c, &tls.Config{ServerName: "b.example", InsecureSkipVerify: true})
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake through the forwarder: %v", err)
	}
	if got := tc.ConnectionState().PeerCertificates[0].Subject.CommonName; got != "b.example" {
		t.Errorf("certificate from %q", got)
	}
	b, _ := io.ReadAll(tc)
	if string(b) != "from the site" {
		t.Errorf("read %q", b)
	}
	if a := r.addrs(); len(a) != 1 || a[0] != "b.example:443" {
		t.Errorf("dialled %v", a)
	}
}

func TestBytesThatAreNotTLSStillReachThePrimarySite(t *testing.T) {
	got := make(chan string, 1)
	r := newRig(t, 0, func(string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			buf := make([]byte, 64)
			n, _ := io.ReadAtLeast(b, buf, len("GET / HTTP/1.1\r\n"))
			got <- string(buf[:n])
			b.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			b.Close()
		}()
		return a, nil
	})
	c, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\n"))
	select {
	case s := <-got:
		if s != "GET / HTTP/1.1\r\n" {
			t.Errorf("the site got %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached the site")
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, _ := io.ReadAll(c); !strings.HasPrefix(string(b), "HTTP/1.1 400") {
		t.Errorf("the answer did not come back: %q", b)
	}
	if a := r.addrs(); len(a) != 1 || a[0] != "a.example:443" {
		t.Errorf("dialled %v", a)
	}
}

func TestSilentClientsStillGetTheSiteAfterTheHelloWait(t *testing.T) {
	r := newRig(t, 0, func(string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() { b.Write([]byte("hi")); b.Close() }()
		return a, nil
	})
	c, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, _ := io.ReadAll(c); string(b) != "hi" {
		t.Errorf("read %q", b)
	}
	if a := r.addrs(); len(a) != 1 || a[0] != "a.example:443" {
		t.Errorf("dialled %v", a)
	}
}

func TestLiveForwardsAreBounded(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	r := newRig(t, 1, func(string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() { <-hold; b.Close() }()
		return a, nil
	})
	first, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	first.Write([]byte("x"))
	// past the hello wait the first one is dialled and holds its slot
	deadline := time.Now().Add(5 * time.Second)
	for len(r.addrs()) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the first forward never dialled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	second, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := second.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("past the bound a connection is closed at once, got %v", err)
	}
	if len(r.addrs()) != 1 {
		t.Errorf("dialled %v", r.addrs())
	}
}

func TestListenReplacesAStaleSocketAndLocksItDown(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cover")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "mask.sock")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o660 {
		t.Errorf("socket mode %v, want a 0660 socket", fi.Mode())
	}
}
