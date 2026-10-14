package users

import (
	"regexp"
	"strings"
	"testing"
)

var oldName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

func oldValidName(s string) bool { return oldName.MatchString(s) && strings.Trim(s, ".") != "" }

func TestValidNameMatchesTheOldRegexp(t *testing.T) {
	for _, s := range []string{
		"", ".", "..", "...", "a", "a.", ".a", "a b", "a\n", "\na", "alice", "Bob_2", "x-y.z",
		strings.Repeat("x", 32), strings.Repeat("x", 33), strings.Repeat(".", 32), "é", "aé", "a\x00",
		"\xff", "a\xffb", "a/b", "a%2e", "ä" + strings.Repeat("x", 30), "A-Z_a-z.0-9", "~", "a~b",
	} {
		if got, want := ValidName(s), oldValidName(s); got != want {
			t.Errorf("ValidName(%q) = %v, the regexp says %v", s, got, want)
		}
	}
}

func FuzzValidName(f *testing.F) {
	for _, s := range []string{"", ".", "alice", strings.Repeat("y", 32), "a b", "é"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := ValidName(s), oldValidName(s); got != want {
			t.Fatalf("ValidName(%q) = %v, the regexp says %v", s, got, want)
		}
	})
}
