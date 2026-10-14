package disguise

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func site(t *testing.T, mutate func(*httptest.Server)) (Checker, string) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if mutate != nil {
		mutate(srv)
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	var d net.Dialer
	return Checker{
		Dial: func(ctx context.Context, n, _ string) (net.Conn, error) {
			return d.DialContext(ctx, n, srv.Listener.Addr().String())
		},
		Lookup: func(context.Context, string) ([]string, error) { return []string{"203.0.113.9"}, nil },
	}, "example.com"
}

func TestGoodSiteWithUntrustedCertStillPassesButSaysSo(t *testing.T) {
	c, dom := site(t, func(s *httptest.Server) { s.EnableHTTP2 = true })
	r := c.Check(context.Background(), dom)
	if !r.OK || !r.TLS13 || !r.X25519 || !r.H2 || r.CertOK {
		t.Fatalf("%+v", r)
	}
	if len(r.Addrs) != 1 {
		t.Errorf("addrs = %v", r.Addrs)
	}
}

func TestSiteWithoutH2Fails(t *testing.T) {
	c, dom := site(t, nil)
	r := c.Check(context.Background(), dom)
	if r.OK || r.Err == "" || r.H2 {
		t.Fatalf("%+v", r)
	}
}

func TestTLS12OnlySiteFails(t *testing.T) {
	c, dom := site(t, func(s *httptest.Server) {
		s.EnableHTTP2 = true
		s.TLS = &tls.Config{MaxVersion: tls.VersionTLS12}
	})
	r := c.Check(context.Background(), dom)
	if r.OK || r.TLS13 {
		t.Fatalf("%+v", r)
	}
}

func TestBadInputAndUnresolvableAndUnreachable(t *testing.T) {
	for _, in := range []string{"", "nodots", "has space.com", "a.com/path", "user@a.com", "host:443"} {
		if r := (Checker{}).Check(context.Background(), in); r.OK || r.Err == "" {
			t.Errorf("%q accepted: %+v", in, r)
		}
	}
	r := Checker{Lookup: func(context.Context, string) ([]string, error) { return nil, net.UnknownNetworkError("x") }}.Check(context.Background(), "gone.example")
	if r.OK || r.Err == "" {
		t.Errorf("unresolvable: %+v", r)
	}
	r = Checker{
		Lookup: func(context.Context, string) ([]string, error) { return []string{"203.0.113.1"}, nil },
		Dial:   func(context.Context, string, string) (net.Conn, error) { return nil, net.ErrClosed },
	}.Check(context.Background(), "down.example")
	if r.OK || r.Err == "" {
		t.Errorf("unreachable: %+v", r)
	}
}

// the record layouts real sites sent the checker (www.microsoft.com is the one REALITY can't copy)
func TestCopyBytesFollowsREALITYsBuffer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		types []byte
		sizes []int
		want  int
	}{
		{"split, certificate too big", []byte{22, 20, 23, 23, 23, 23}, []int{127, 6, 53, 8273, 286, 74}, 8686},
		{"split, small enough", []byte{22, 20, 23, 23, 23, 23}, []int{127, 6, 53, 4738, 286, 74}, 5151},
		{"one record", []byte{22, 20, 23}, []int{127, 6, 2671}, 2671},
		{"one record too big", []byte{22, 20, 23}, []int{127, 6, 9000}, 9000},
		{"no ChangeCipherSpec", []byte{22, 23}, []int{127, 2671}, -1},
		{"an alert", []byte{22, 20, 21}, []int{127, 6, 7}, -1},
	} {
		f := &flight{types: tc.types, sizes: tc.sizes}
		if got := f.copyBytes(); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestFlightReadsRecordsAcrossReads(t *testing.T) {
	var wire []byte
	for _, r := range []struct {
		typ byte
		n   int
	}{{22, 122}, {20, 1}, {23, 300}} {
		wire = append(wire, r.typ, 3, 3, byte(r.n>>8), byte(r.n))
		wire = append(wire, make([]byte, r.n)...)
	}
	f := &flight{}
	for len(wire) > 0 { // 3 bytes at a time splits headers too
		k := min(3, len(wire))
		f.note(wire[:k])
		wire = wire[k:]
	}
	if got := f.sizes; len(got) != 3 || got[0] != 127 || got[1] != 6 || got[2] != 305 {
		t.Fatalf("sizes = %v", got)
	}
}

func TestSiteWithAHandshakeTooBigToCopyFails(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example.com"}, DNSNames: []string{"example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	chain := [][]byte{der}
	for len(chain)*len(der) < 10000 { // a long chain, like a big site's
		chain = append(chain, der)
	}
	c, dom := site(t, func(s *httptest.Server) {
		s.EnableHTTP2 = true
		s.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: chain, PrivateKey: key}}}
	})
	r := c.Check(context.Background(), dom)
	if r.OK || !r.TLS13 || !r.H2 || !strings.Contains(r.Err, "too big for the link") {
		t.Fatalf("%+v", r)
	}
}

func TestProxyOnlyNeedsNeitherH2NorASmallFlight(t *testing.T) {
	c, dom := site(t, nil)
	c.ProxyOnly = true
	if r := c.Check(context.Background(), dom); !r.OK || r.H2 || r.Err != "" {
		t.Fatalf("a site without h2 = %+v", r)
	}
	c, dom = site(t, func(s *httptest.Server) { s.TLS = &tls.Config{MaxVersion: tls.VersionTLS12} })
	c.ProxyOnly = true
	if r := c.Check(context.Background(), dom); r.OK {
		t.Fatalf("TLS 1.2 only still fails: %+v", r)
	}
}
