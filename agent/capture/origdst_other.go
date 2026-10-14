//go:build !linux

package capture

import (
	"errors"
	"net"
	"net/netip"
)

func OriginalDst(*net.TCPConn) (netip.AddrPort, error) {
	return netip.AddrPort{}, errors.New("capture: original destination needs Linux")
}
