package installer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// coverSite is an HTTPS site on loopback; h2 says whether it offers HTTP/2.
func coverSite(t *testing.T, h2 bool) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.EnableHTTP2 = h2
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// The VPS resolves the name and opens the connection; nothing is put on the box.
func TestCoverIsTestedFromTheVPSBeforeAnythingIsInstalled(t *testing.T) {
	h := startFakeHost(t)
	h.site("mirror.example.ir", coverSite(t, true))
	res, checked := CheckCover(context.Background(), connect(t, h), "mirror.example.ir")
	if !checked || !res.OK || !res.TLS13 || !res.X25519 || !res.H2 {
		t.Fatalf("checked=%v res=%+v", checked, res)
	}
	// the test server's certificate isn't trusted, which only warns
	if res.CertOK || res.LatencyMS != 0 {
		t.Fatalf("res = %+v", res)
	}
	if !slices.Equal(res.Addrs, []string{"192.0.2.7", "2001:db8::7"}) {
		t.Fatalf("addrs = %v", res.Addrs)
	}
	if !slices.Contains(h.forwards(), "mirror.example.ir:443") {
		t.Fatalf("the box was asked to open %v", h.forwards())
	}
	for _, c := range h.commands() {
		if c != "id -u" && !strings.HasPrefix(c, "getent ") {
			t.Fatalf("the test ran %q on the box", c)
		}
	}
}

func TestACoverSiteWithoutH2FailsTheEarlyTest(t *testing.T) {
	h := startFakeHost(t)
	h.site("mirror.example.ir", coverSite(t, false))
	res, checked := CheckCover(context.Background(), connect(t, h), "mirror.example.ir")
	if !checked || res.OK || !strings.Contains(res.Err, "h2") {
		t.Fatalf("checked=%v res=%+v", checked, res)
	}
}

func TestACoverSiteTheVPSCannotReachFails(t *testing.T) {
	h := startFakeHost(t)
	res, checked := CheckCover(context.Background(), connect(t, h), "mirror.example.ir")
	if !checked || res.OK || !strings.Contains(res.Err, "cannot connect") {
		t.Fatalf("checked=%v res=%+v", checked, res)
	}
}

func TestACoverNameTheVPSCannotResolveFails(t *testing.T) {
	h := startFakeHost(t)
	h.env["FAKE_NO_NAMES"] = "nowhere.example.ir"
	res, checked := CheckCover(context.Background(), connect(t, h), "nowhere.example.ir")
	if !checked || res.OK || !strings.Contains(res.Err, "cannot resolve") {
		t.Fatalf("checked=%v res=%+v", checked, res)
	}
}

// A box that won't forward (or can't look names up) leaves the test to the agent.
func TestABoxThatCannotBeAskedLeavesTheCoverUnchecked(t *testing.T) {
	h := startFakeHost(t)
	h.site("mirror.example.ir", coverSite(t, true))
	h.setForwarding(false)
	if res, checked := CheckCover(context.Background(), connect(t, h), "mirror.example.ir"); checked {
		t.Fatalf("forwarding off: checked, res=%+v", res)
	}
	h.setForwarding(true)
	h.removeTool("getent")
	if res, checked := CheckCover(context.Background(), connect(t, h), "mirror.example.ir"); checked {
		t.Fatalf("no getent: checked, res=%+v", res)
	}
}

// Once the panel has tested the site, the agent doesn't test it again after the upload.
func TestACheckedCoverIsNotTestedAgain(t *testing.T) {
	bad := strings.Replace(fakeAgent, `"ok":true`, `"ok":false,"err":"no h2"`, 1)
	h := startFakeHost(t)
	req := request(t)
	req.Agent = bin(bad)
	req.CoverChecked = true
	if _, err := Install(context.Background(), connect(t, h), req, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.commands() {
		if strings.Contains(c, "disguise-check") {
			t.Fatalf("the agent tested the site again: %q", c)
		}
	}
}
