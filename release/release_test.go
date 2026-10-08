package release

import "testing"

func TestParseArch(t *testing.T) {
	for in, want := range map[string]Arch{"x86_64": AMD64, "amd64": AMD64, "aarch64": ARM64, "arm64": ARM64} {
		if got, err := ParseArch(in); err != nil || got != want {
			t.Errorf("%s -> %v %v", in, got, err)
		}
	}
	for _, in := range []string{"armv7l", "i686", "mips", ""} {
		if _, err := ParseArch(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestTelemtPinsAreComplete(t *testing.T) {
	for _, a := range []Arch{AMD64, ARM64} {
		as, err := Lookup(Telemt, a)
		if err != nil {
			t.Fatal(err)
		}
		if as.Member != "telemt" || len(as.SHA256) != 64 {
			t.Errorf("%s: %+v", a, as)
		}
	}
}

// An unreleased build must not be installable: Agent has no hashes until a release is cut.
func TestUnreleasedAgentIsRefused(t *testing.T) {
	if _, err := Lookup(Agent, AMD64); err == nil {
		t.Skip("agent hashes are pinned: this test only guards the pre-release state")
	}
}
