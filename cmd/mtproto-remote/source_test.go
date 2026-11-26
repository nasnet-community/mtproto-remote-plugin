package main

import (
	"io"
	"log/slog"
	"net"
	"testing"
)

func TestSourceAddr(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		value string
		want  string // "" for none: the system picks
	}{
		{"", ""},
		{"not-an-address", ""},
		{"192.0.2.77", ""}, // no interface here holds it
		{"127.0.0.1", "127.0.0.1"},
		{" 127.0.0.1 ", "127.0.0.1"},
	} {
		t.Setenv("ADDR_TEST", tc.value)
		got := sourceAddr(log, "ADDR_TEST")
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%q: got %v, want none", tc.value, got)
		case tc.want != "" && (got == nil || got.IP.String() != tc.want):
			t.Errorf("%q: got %v, want %s", tc.value, got, tc.want)
		}
	}
}

func TestDialerFromLeavesFromTheAddress(t *testing.T) {
	if d := dialerFrom(nil); d.LocalAddr != nil {
		t.Fatalf("no address gave LocalAddr %#v, which is not a nil net.Addr", d.LocalAddr)
	}

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()

	local := &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}
	c, err := dialerFrom(local).DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.LocalAddr().(*net.TCPAddr).IP; !got.Equal(local.IP) {
		t.Errorf("left from %v, want %v", got, local.IP)
	}
}
