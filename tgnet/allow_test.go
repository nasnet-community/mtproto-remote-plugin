package tgnet

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func pfx(s ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, x := range s {
		out = append(out, netip.MustParsePrefix(x))
	}
	return out
}

func TestPermitChecksPrefixAndPort(t *testing.T) {
	a := New(pfx("203.0.113.0/24", "2001:db8::/32"))
	cases := []struct {
		ap   string
		want bool
	}{
		{"203.0.113.7:443", true},
		{"203.0.113.7:80", false},
		{"203.0.114.7:443", false},
		{"[2001:db8::1]:443", true},
		{"[2001:db9::1]:443", false},
		{"[::ffff:203.0.113.9]:443", true}, // v4-mapped counts as v4
		{"127.0.0.1:443", false},
	}
	for _, c := range cases {
		if got := a.Permit(netip.MustParseAddrPort(c.ap)); got != c.want {
			t.Errorf("Permit(%s) = %v, want %v", c.ap, got, c.want)
		}
	}
}

func TestParseListHandlesCommentsAndBareAddresses(t *testing.T) {
	got, err := ParseList(strings.NewReader("# header\n203.0.113.0/24  # inline\n\n198.51.100.9\n2001:db8::/32\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := pfx("203.0.113.0/24", "198.51.100.9/32", "2001:db8::/32")
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] %v != %v", i, got[i], want[i])
		}
	}
	if _, err := ParseList(strings.NewReader("not-an-ip\n")); err == nil {
		t.Error("garbage line accepted")
	}
}

func TestRefreshOnlyAddsAndNeverShrinksPinned(t *testing.T) {
	a := New(pfx("203.0.113.0/24"))
	n, err := a.Merge(pfx("198.51.100.0/24", "203.0.113.0/24"))
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v, want 1 new", n, err)
	}
	if !a.Permit(netip.MustParseAddrPort("198.51.100.1:443")) {
		t.Fatal("refreshed prefix not allowed")
	}
	// a later refresh that drops it removes it, but pinned stays
	if _, err := a.Merge(pfx("192.0.2.0/24")); err != nil {
		t.Fatal(err)
	}
	if a.Permit(netip.MustParseAddrPort("198.51.100.1:443")) {
		t.Error("dropped prefix still allowed")
	}
	if !a.Permit(netip.MustParseAddrPort("203.0.113.1:443")) {
		t.Error("pinned prefix lost")
	}
}

func TestBadRefreshesLeaveTheListAlone(t *testing.T) {
	a := New(pfx("203.0.113.0/24"))
	a.Merge(pfx("198.51.100.0/24"))
	bad := map[string][]netip.Prefix{
		"empty":     nil,
		"everyone":  pfx("0.0.0.0/0"),
		"too broad": pfx("10.0.0.0/8"),
		"v6 broad":  pfx("2000::/3"),
	}
	for name, list := range bad {
		if _, err := a.Merge(list); !errors.Is(err, ErrBadList) {
			t.Errorf("%s: err = %v, want ErrBadList", name, err)
		}
	}
	var many []netip.Prefix
	for i := 0; i < 600; i++ {
		many = append(many, netip.PrefixFrom(netip.AddrFrom4([4]byte{100, byte(i >> 8), byte(i), 0}), 24))
	}
	if _, err := a.Merge(many); !errors.Is(err, ErrBadList) {
		t.Errorf("oversized list: %v", err)
	}
	if !a.Permit(netip.MustParseAddrPort("198.51.100.1:443")) || !a.Permit(netip.MustParseAddrPort("203.0.113.1:443")) {
		t.Fatal("a rejected refresh changed the list")
	}
}

func TestRefreshFetchFailureKeepsList(t *testing.T) {
	a := New(pfx("203.0.113.0/24"))
	_, err := a.Refresh(context.Background(), func(context.Context) (io.ReadCloser, error) { return nil, errors.New("offline") })
	if err == nil {
		t.Fatal("want the fetch error")
	}
	if !a.Permit(netip.MustParseAddrPort("203.0.113.1:443")) {
		t.Fatal("list lost")
	}
	_, err = a.Refresh(context.Background(), func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("<html>captive portal</html>")), nil
	})
	if !errors.Is(err, ErrBadList) {
		t.Fatalf("html body: %v, want ErrBadList", err)
	}
}

func TestViolationWindow(t *testing.T) {
	a := New(nil)
	for i := 0; i < 5; i++ {
		a.Violation()
	}
	if got := a.ViolationsSince(time.Minute); got != 5 {
		t.Fatalf("got %d", got)
	}
	time.Sleep(30 * time.Millisecond)
	if got := a.ViolationsSince(10 * time.Millisecond); got != 0 {
		t.Fatalf("window did not expire: %d", got)
	}
	if a.ViolationsTotal() != 5 {
		t.Fatal("total lost")
	}
}

// These used to merge, and Permit then allowed 127.0.0.1, 192.168.88.1 and [::1].
func TestRefreshRefusesRangesOffTheInternet(t *testing.T) {
	a := New(pfx("203.0.113.0/24"))
	for _, bad := range []string{
		"127.0.0.0/12", "10.0.0.0/12", "172.16.0.0/12", "192.168.0.0/16", "192.160.0.0/12",
		"100.64.0.0/12", "169.254.0.0/16", "224.0.0.0/12", "0.0.0.0/12", "255.255.255.255/32", "198.18.0.0/15",
		"::1/128", "::/128", "fe80::/29", "fc00::/29", "fd00::/29", "ff00::/29",
		"::ffff:203.0.113.0/120", "64:ff9b::/96", "2002::/29",
	} {
		// one bad entry refuses the whole list
		if _, err := a.Merge(pfx("198.51.100.0/24", bad)); !errors.Is(err, ErrBadList) {
			t.Errorf("%s: err = %v, want ErrBadList", bad, err)
		}
	}
	for _, ap := range []string{"198.51.100.1:443", "127.0.0.1:443", "192.168.88.1:443", "[::1]:443"} {
		if a.Permit(netip.MustParseAddrPort(ap)) {
			t.Errorf("%s permitted after only refused refreshes", ap)
		}
	}
}

func TestPermitNeverReachesLocalAddresses(t *testing.T) {
	a := New(pfx("127.0.0.0/8", "10.0.0.0/8", "::1/128", "fe80::/10", "203.0.113.0/24"))
	for _, ap := range []string{"127.0.0.1:443", "10.1.2.3:443", "[::1]:443", "[::ffff:127.0.0.1]:443", "[fe80::1]:443"} {
		if a.Permit(netip.MustParseAddrPort(ap)) {
			t.Errorf("Permit(%s) = true", ap)
		}
	}
	if !a.Permit(netip.MustParseAddrPort("203.0.113.5:443")) {
		t.Error("the public prefix was lost")
	}
	// what agents are told to capture must be clean too
	for _, p := range a.Prefixes() {
		if checkPublic(p) != nil {
			t.Errorf("%s would be pushed to agents", p)
		}
	}
}

func TestPinnedListMustBeOnTheInternet(t *testing.T) {
	for _, list := range []string{"192.168.0.0/16\n", "203.0.113.0/24\n::1\n"} {
		if _, err := parsePinned(list); !errors.Is(err, ErrBadList) {
			t.Errorf("%q: err = %v, want ErrBadList", list, err)
		}
	}
	// the shipped placeholder uses documentation ranges, which must still load
	if _, err := parsePinned(pinnedCIDR); err != nil {
		t.Fatalf("placeholder: %v", err)
	}
}

// A hub handed a nil list (through link's interface) refuses everything rather than panicking.
func TestANilAllowlistRefusesEverything(t *testing.T) {
	var a *Allowlist
	if a.Permit(netip.MustParseAddrPort("203.0.113.7:443")) {
		t.Fatal("a nil list permitted a dial")
	}
	a.Violation()
	if a.ViolationsTotal() != 0 {
		t.Fatal("a nil list counted")
	}
}
