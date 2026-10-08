package logring

import (
	"fmt"
	"strings"
	"testing"
)

func TestMaskHidesAddressesButNotTimestampsOrVersions(t *testing.T) {
	cases := map[string]string{
		"conn from 198.51.100.23:51234 ok":       "conn from [ip]:51234 ok",
		"peer 2001:db8:85a3::8a2e:370:7334 gone": "peer [ip6] gone",
		"peer [2001:db8::1]:443 gone":            "peer [[ip6]]:443 gone",
		"fe80::1ff:fe23:4567:890a closed":        "[ip6] closed",
		"listening on 127.0.0.1:9091":            "listening on 127.0.0.1:9091",
		"bound to ::1 and ::":                    "bound to ::1 and ::",
		"2026-10-01T10:20:30Z telemt 3.4.25":     "2026-10-01T10:20:30Z telemt 3.4.25",
		"dc 149.154.167.51 reached, 10.0.0.1":    "dc [ip] reached, [ip]",
	}
	for in, want := range cases {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}

// Addresses glued to letters, underscores, colons or brackets are still addresses.
func TestMaskCatchesGluedAndBracketedForms(t *testing.T) {
	cases := map[string]string{
		"peer_198.51.100.23 closed":               "peer_[ip] closed",
		"ip198.51.100.23x":                        "ip[ip]x",
		"from=198.51.100.23:51234,":               "from=[ip]:51234,",
		"node7.198.51.100.23":                     "node[ip]",
		"peer=2001:db8::1 gone":                   "peer=[ip6] gone",
		"peer_2001:db8::1_x":                      "peer_[ip6]_x",
		"addr:2001:db8::1":                        "addr:[ip6]",
		"conn::2001:db8::7":                       "conn::[ip6]",
		"[2001:db8::1]:443":                       "[[ip6]]:443",
		"(2001:DB8:0:0:8:800:200C:417A)":          "([ip6])",
		"fe80::1%eth0 up":                         "[ip6]%eth0 up",
		"2001:db8::1:51234":                       "[ip6]",
		"mapped ::ffff:198.51.100.7 here":         "mapped ::ffff:[ip] here",
		"2001:0db8:0000:0000:0000:ff00:0042:8329": "[ip6]",
	}
	for in, want := range cases {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q)\n got  %q\n want %q", in, got, want)
		}
	}
	// what must stay readable
	for _, keep := range []string{
		"2026-10-01T10:20:30.123456Z  INFO telemt::proxy::client: up",
		"telemt::upstream::socks5::connect failed",
		"h2::proto::streams handshake::read middle::conn cache::fetch",
		"10:20:30 and 1:2:3 and 00:1a:2b:3c:4d:5e",
		"telemt 3.4.25 on 127.0.0.1:9091 and ::1 and ::",
	} {
		if got := Mask(keep); got != keep {
			t.Errorf("Mask(%q) = %q, want it unchanged", keep, got)
		}
	}
}

// A line is masked before it is cut, so an address straddling the cut cannot
// leave half of itself behind.
func TestLongLinesAreMaskedBeforeTheyAreCut(t *testing.T) {
	r := New(8)
	for _, off := range []int{12, 7, 3, 1} {
		fmt.Fprintf(r, "%s198.51.100.23 tail\n", strings.Repeat("x", maxLine-off))
	}
	// a line that never ends, written in pieces that split an address
	r.Write([]byte(strings.Repeat("y", maxLine+100) + "203.0."))
	r.Write([]byte("113.77 and more\nnext 2001:db8::"))
	r.Write([]byte("42 line\n"))
	for _, l := range r.Tail(0) {
		if len(l) > maxLine {
			t.Errorf("a %d byte line got through", len(l))
		}
		for _, frag := range []string{"198.5", "100.2", "203.0", "113.77", "2001:db8", ":42"} {
			if strings.Contains(l, frag) {
				t.Errorf("part of an address survived: %q in ...%q", frag, l[max(0, len(l)-40):])
			}
		}
	}
	if got := r.Tail(1); len(got) != 1 || got[0] != "next [ip6] line" {
		t.Fatalf("the line after the runaway one: %q", got)
	}
}

func TestRingKeepsNewestLinesAndMasksOnWrite(t *testing.T) {
	r := New(3)
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(r, "line %d from 203.0.113.%d\n", i, i)
	}
	got := r.Tail(0)
	want := []string{"line 3 from [ip]", "line 4 from [ip]", "line 5 from [ip]"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v", got)
	}
	if got := r.Tail(2); len(got) != 2 || got[1] != "line 5 from [ip]" {
		t.Fatalf("tail(2) = %v", got)
	}
}

func TestRingJoinsPartialWritesAndBoundsRunawayLines(t *testing.T) {
	r := New(4)
	r.Write([]byte("half a li"))
	r.Write([]byte("ne\nnext"))
	if got := r.Tail(0); len(got) != 1 || got[0] != "half a line" {
		t.Fatalf("got %v", got)
	}
	r.Write([]byte(strings.Repeat("x", 5000)))
	for _, l := range r.Tail(0) {
		if len(l) > maxLine {
			t.Fatalf("a %d byte line got through", len(l))
		}
	}
}

// Masking shrinks a line, so a raw cut can land inside the kept part: the
// front half of an address at the cut must not survive.
func TestACutAddressDoesNotSurviveMaskingShrinkage(t *testing.T) {
	v6s := strings.Repeat("2001:0db8:0000:0000:0000:ff00:0042:8329 ", 10)
	head := v6s + strings.Repeat("x", maxLine+margin+1-len(v6s)-len("203.0.113"))
	for name, write := range map[string]func(*Ring){
		"never ends": func(r *Ring) { r.Write([]byte(head + "203.0.113.77 and more")) },
		"ends later": func(r *Ring) { r.Write([]byte(head + "203.0.113.77 and more\n")) },
		"in pieces":  func(r *Ring) { r.Write([]byte(head + "203.0")); r.Write([]byte(".113.77 and more\n")) },
	} {
		r := New(4)
		write(r)
		got := r.Tail(0)
		if len(got) != 1 {
			t.Fatalf("%s: %d lines", name, len(got))
		}
		if l := got[0]; strings.Contains(l, "203.0") || strings.Contains(l, "113") || strings.Contains(l, "2001") {
			t.Errorf("%s: part of an address survived: ...%q", name, l[max(0, len(l)-40):])
		}
	}
}
