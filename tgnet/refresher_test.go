package tgnet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
)

func TestPinnedListIsTelegramsOwn(t *testing.T) {
	ps, err := PinnedPrefixes()
	if err != nil {
		t.Fatal(err)
	}
	if PinnedIsPlaceholder() {
		t.Fatal("the pinned list is still the placeholder")
	}
	have := map[netip.Prefix]bool{}
	var v4, v6 int
	for _, p := range ps {
		have[p] = true
		if p.Addr().Is4() {
			v4++
		} else {
			v6++
		}
	}
	// the main datacentre block and one v6 range, so a truncated file fails here
	for _, want := range []string{"149.154.160.0/20", "91.108.4.0/22", "2001:b28:f23d::/48"} {
		if !have[netip.MustParsePrefix(want)] {
			t.Errorf("%s is missing from the pinned list", want)
		}
	}
	if v4 == 0 || v6 == 0 {
		t.Errorf("want both families, got %d v4 and %d v6", v4, v6)
	}
}

func TestTheListURLDefaultsToTelegramsAndCanBeTurnedOff(t *testing.T) {
	for in, want := range map[string]string{
		"":                             DefaultURL,
		"  ":                           DefaultURL,
		"off":                          "",
		"OFF":                          "",
		"https://example.org/cidr.txt": "https://example.org/cidr.txt",
	} {
		if got := ListURL(in); got != want {
			t.Errorf("ListURL(%q) = %q, want %q", in, got, want)
		}
	}
	if DefaultURL != "https://core.telegram.org/resources/cidr.txt" {
		t.Errorf("DefaultURL = %q", DefaultURL)
	}
}

func TestARefreshOfThePinnedListChangesNothing(t *testing.T) {
	s, changed := service(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(pinnedCIDR))
	})
	before := len(s.List.Prefixes())
	if err := s.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.List.Prefixes()) != before || atomic.LoadInt32(changed) != 0 {
		t.Fatalf("prefixes %d -> %d, changed %d", before, len(s.List.Prefixes()), *changed)
	}
}

func service(t *testing.T, handler http.HandlerFunc) (*Refresher, *int32) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	s, err := NewRefresher()
	if err != nil {
		t.Fatal(err)
	}
	var changed int32
	s.URL = func() string { return srv.URL }
	s.Client = func() *http.Client { return srv.Client() }
	s.Changed = func() { atomic.AddInt32(&changed, 1) }
	return s, &changed
}

func TestRefreshAddsRangesAndTellsTheAgents(t *testing.T) {
	s, changed := service(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("# fresh\n198.51.100.0/24\n"))
	})
	if err := s.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.List.Permit(netip.MustParseAddrPort("198.51.100.5:443")) {
		t.Fatal("refreshed range not allowed")
	}
	if atomic.LoadInt32(changed) != 1 {
		t.Fatalf("changed called %d times", *changed)
	}
	// the same list again changes nothing and tells nobody
	if err := s.RefreshNow(context.Background()); err != nil || atomic.LoadInt32(changed) != 1 {
		t.Fatalf("second refresh: %v changed=%d", err, *changed)
	}
	if ok, e := s.Status(); ok.IsZero() || e != "" {
		t.Fatalf("status = %v %q", ok, e)
	}
}

func TestBadRefreshesKeepTheList(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"server error": func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusBadGateway) },
		"html":         func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html>sign in</html>")) },
		"everything":   func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("0.0.0.0/0\n")) },
		"empty":        func(w http.ResponseWriter, _ *http.Request) {},
	} {
		t.Run(name, func(t *testing.T) {
			s, changed := service(t, h)
			before := len(s.List.Prefixes())
			if err := s.RefreshNow(context.Background()); err == nil {
				t.Fatal("a bad list was accepted")
			}
			if len(s.List.Prefixes()) != before || atomic.LoadInt32(changed) != 0 {
				t.Fatal("the list or the agents were touched")
			}
			if _, e := s.Status(); e == "" {
				t.Error("the failure is not visible in the status")
			}
		})
	}
}

func TestNoURLMeansNoFetch(t *testing.T) {
	s, _ := NewRefresher()
	s.URL = func() string { return "" }
	s.Client = func() *http.Client { t.Fatal("fetched without a URL"); return nil }
	if err := s.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
}
