package relayops

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/nasnet-community/mtproto-remote-plugin/installer"
)

func TestNewSSHViaDialsThroughItsDialer(t *testing.T) {
	errOurs := errors.New("ours")
	var used []string
	ours := func(_ context.Context, _, addr string) (net.Conn, error) {
		used = append(used, "ours "+addr)
		return nil, errOurs
	}

	_, err := NewSSHVia(ours).Fingerprint(t.Context(), installer.Target{Host: "198.51.100.7", Port: 22})
	if !errors.Is(err, errOurs) || len(used) != 1 {
		t.Fatalf("err = %v, dials %q: want one through the given dialer", err, used)
	}

	// a target with a way of its own keeps it
	own := func(_ context.Context, _, addr string) (net.Conn, error) {
		used = append(used, "own "+addr)
		return nil, errOurs
	}
	_, _ = NewSSHVia(ours).Fingerprint(t.Context(), installer.Target{Host: "198.51.100.7", Port: 22, Dial: own})
	if len(used) != 2 || used[1] != "own 198.51.100.7:22" {
		t.Errorf("dials %q: the target's own dialer should have been used", used)
	}
}
