package main

import (
	"testing"

	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/store"
)

func TestCapFor(t *testing.T) {
	for arch, want := range map[string]int{"arm": 1, "arm64": 3, "amd64": 3} {
		if got := capFor(arch); got != want {
			t.Errorf("capFor(%q) = %d, want %d", arch, got, want)
		}
	}
}

func TestLimitsFor(t *testing.T) {
	cases := []struct {
		env, arch       string
		servers, budget int
		ok              bool
	}{
		{"", "amd64", 3, 6000, true},
		{"", "arm", 1, 2000, true}, // a 256 MB box carries one server's worth of streams
		{"2", "arm64", 2, 4000, true},
		{"9", "amd64", 3, 6000, true}, // never above 3
		{"3", "arm", 1, 2000, true},   // a 256 MB box stays at one
		{"0", "amd64", 0, 0, false},
		{"lots", "amd64", 0, 0, false},
	}
	for _, c := range cases {
		got, err := limitsFor(c.env, c.arch)
		if (err == nil) != c.ok || got != (store.Limits{MaxServers: c.servers, StreamBudget: c.budget}) {
			t.Errorf("limitsFor(%q, %q) = %+v, %v", c.env, c.arch, got, err)
		}
	}
	if got, _ := limitsFor("", "amd64"); got.StreamBudget != store.DefaultStreamBudget {
		t.Errorf("the default budget moved: %d", got.StreamBudget)
	}
}
