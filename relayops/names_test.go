package relayops

import (
	"regexp"
	"testing"
)

// The expressions these checks replaced; they stay here as the reference.
var (
	refHost   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	refDomain = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

var tricky = []string{
	"", "a", "A", "0", "-", ".", "a.", ".a", "a.b", "A.B", "a-b", "a--b", "-a", "a-", "a.-b", "a-.b",
	"a..b", "ab.cd.ef", "x.y-", "x.-y", "1.2.3.4", "203.0.113.9", "::1", "2001:db8::1", "a b", "a_b",
	"example.com", "Example.COM", "www.example.ir", "xn--bcher-kva.example", "a.b.", "a.b.c-", "a.b.-c",
	"ab", "a1", "1a", "a\n", "a\nb.c", "a.b\n", "\na.b", "a.b\x00", "ä.com", "a.ä", "é", "a.b c",
	"a/b.c", "a@b.c", "aaa-.bbb", "aaa.bbb-ccc.ddd", "a.b.c.d.e.f", "0.0", "-.-", "a.0", "0-0.0-0",
	"\xff", "a.\xff", "A-B.C", "localhost", "my-vps", "my-vps.", "vps.local", "a.b..c",
}

func TestHostAndDomainChecksMatchTheOldExpressions(t *testing.T) {
	for _, s := range tricky {
		if got, want := validHost(s), refHost.MatchString(s); got != want {
			t.Errorf("validHost(%q) = %v, the expression says %v", s, got, want)
		}
		if got, want := ValidDomain(s), refDomain.MatchString(s); got != want {
			t.Errorf("ValidDomain(%q) = %v, the expression says %v", s, got, want)
		}
	}
}

func FuzzHost(f *testing.F) {
	for _, s := range tricky {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := validHost(s), refHost.MatchString(s); got != want {
			t.Fatalf("validHost(%q) = %v, the expression says %v", s, got, want)
		}
	})
}

func FuzzValidDomain(f *testing.F) {
	for _, s := range tricky {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := ValidDomain(s), refDomain.MatchString(s); got != want {
			t.Fatalf("ValidDomain(%q) = %v, the expression says %v", s, got, want)
		}
	})
}
