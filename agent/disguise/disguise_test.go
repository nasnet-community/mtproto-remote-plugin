package disguise

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
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
