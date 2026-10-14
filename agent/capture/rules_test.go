package capture

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func pfx(s ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, x := range s {
		out = append(out, netip.MustParsePrefix(x))
	}
	return out
}

const goldenV4 = `add table ip mtproto_agent
delete table ip mtproto_agent
table ip mtproto_agent {
	set dc {
		type ipv4_addr
		flags interval
		elements = { 203.0.113.0/24, 198.51.100.0/22 }
	}
	chain nat_out {
		type nat hook output priority -100; policy accept;
		meta skuid 998 ip daddr @dc tcp dport 443 redirect to :9443
	}
	chain filter_out {
		type filter hook output priority 0; policy accept;
		oifname "lo" accept
		ip daddr 127.0.0.0/8 accept
		meta skuid 998 ct direction original meta l4proto tcp reject with tcp reset
		meta skuid 998 ct direction original reject
	}
}
add table ip6 mtproto_agent
delete table ip6 mtproto_agent
table ip6 mtproto_agent {
	chain filter_out {
		type filter hook output priority 0; policy accept;
		oifname "lo" accept
		ip6 daddr ::1 accept
		meta skuid 998 ct direction original meta l4proto tcp reject with tcp reset
		meta skuid 998 ct direction original reject
	}
}
`

func TestScriptV4Golden(t *testing.T) {
	s, err := Rules{UID: 998, Port: 9443, V4: pfx("203.0.113.0/24", "198.51.100.0/22")}.Script()
	if err != nil {
		t.Fatal(err)
	}
	if s != goldenV4 {
		t.Fatalf("ruleset drifted:\n%s", s)
	}
}

func TestScriptBothFamiliesAreIndependentTables(t *testing.T) {
	s, err := Rules{UID: 998, Port: 9443, V4: pfx("203.0.113.0/24"), V6: pfx("2001:db8::/32")}.Script()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"table ip mtproto_agent {", "table ip6 mtproto_agent {",
		"type ipv6_addr", "ip6 daddr @dc tcp dport 443 redirect to :9443",
		"elements = { 2001:db8::/32 }",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	// each table is replaced atomically: add (so delete cannot fail), delete, define
	if strings.Count(s, "delete table") != 2 {
		t.Errorf("want one delete per table:\n%s", s)
	}
}

// telemt may reach nothing but Telegram, through the agent: in both families,
// whatever their prefixes, what it starts is refused unless it was redirected
// to the agent (or is loopback anyway), and an emptied family loses its stale
// redirect in the same transaction.
func TestEveryFamilyRefusesWhatTelemtStartsElsewhere(t *testing.T) {
	for fam, r := range map[string]Rules{
		"ip6": {UID: 5, Port: 1, V4: pfx("203.0.113.0/24")},
		"ip":  {UID: 5, Port: 1, V6: pfx("2001:db8::/32")},
		"ip ": {UID: 5, Port: 1, V4: pfx("203.0.113.0/24"), V6: pfx("2001:db8::/32")},
	} {
		fam = strings.TrimSpace(fam)
		s, err := r.Script()
		if err != nil {
			t.Fatal(err)
		}
		_, table, ok := strings.Cut(s, "table "+fam+" mtproto_agent {\n")
		if !ok {
			t.Fatalf("%s: no %s table at all:\n%s", fam, fam, s)
		}
		table, _, _ = strings.Cut(table, "\n}\n")
		loop := "ip daddr 127.0.0.0/8 accept"
		if fam == "ip6" {
			loop = "ip6 daddr ::1 accept"
		}
		for _, want := range []string{
			"chain filter_out {", `oifname "lo" accept`, loop,
			"meta skuid 5 ct direction original meta l4proto tcp reject with tcp reset",
			"meta skuid 5 ct direction original reject",
		} {
			if !strings.Contains(table, want) {
				t.Errorf("%s: missing %q in\n%s", fam, want, table)
			}
		}
		// the refusal comes after both accepts, or nothing would get through
		if strings.Index(table, "reject") < strings.Index(table, loop) {
			t.Errorf("%s: a refusal comes before the loopback accept:\n%s", fam, table)
		}
		if strings.Count(s, "delete table") != 2 {
			t.Errorf("both families are replaced every time:\n%s", s)
		}
	}
	s, _ := Rules{UID: 5, Port: 1, V4: pfx("203.0.113.0/24")}.Script()
	_, v6, _ := strings.Cut(s, "table ip6 mtproto_agent {\n")
	if strings.Contains(v6, "nat_out") || strings.Contains(v6, "@dc") {
		t.Errorf("a family with no prefixes redirects nothing:\n%s", v6)
	}
}

func TestScriptRefusesDangerousInputs(t *testing.T) {
	if _, err := (Rules{UID: 998, Port: 9443}).Script(); !errors.Is(err, ErrNoPrefixes) {
		t.Errorf("empty set: %v, want ErrNoPrefixes", err)
	}
	if _, err := (Rules{UID: 0, Port: 9443, V4: pfx("203.0.113.0/24")}).Script(); err == nil {
		t.Error("uid 0 accepted: the rules would capture the agent's own traffic")
	}
	if _, err := (Rules{UID: 998, Port: 0, V4: pfx("203.0.113.0/24")}).Script(); err == nil {
		t.Error("port 0 accepted")
	}
}

func TestPrefixesAreMasked(t *testing.T) {
	s, _ := Rules{UID: 5, Port: 1, V4: pfx("203.0.113.77/24")}.Script()
	if !strings.Contains(s, "elements = { 203.0.113.0/24 }") {
		t.Errorf("host bits leaked into the set:\n%s", s)
	}
}

func TestSplit(t *testing.T) {
	v4, v6 := Split(pfx("203.0.113.0/24", "2001:db8::/32", "198.51.100.0/24"))
	if len(v4) != 2 || len(v6) != 1 {
		t.Fatalf("v4=%v v6=%v", v4, v6)
	}
}

func TestTeardownIsIdempotentScript(t *testing.T) {
	s := Teardown()
	if !strings.Contains(s, "add table ip mtproto_agent\ndelete table ip mtproto_agent") ||
		!strings.Contains(s, "add table ip6 mtproto_agent\ndelete table ip6 mtproto_agent") {
		t.Fatalf("teardown:\n%s", s)
	}
}
