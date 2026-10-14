//go:build linux

package capture

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/unix"
)

// SO_ORIGINAL_DST and IP6T_SO_ORIGINAL_DST share the value 80; the kernel reads
// the conntrack entry that the redirect created.
const soOriginalDst = 80

// OriginalDst returns the destination telemt dialled before the redirect.
func OriginalDst(c *net.TCPConn) (netip.AddrPort, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var res netip.AddrPort
	var gerr error
	err = rc.Control(func(fd uintptr) {
		if local, ok := c.LocalAddr().(*net.TCPAddr); ok && local.IP.To4() == nil {
			var sa unix.RawSockaddrInet6
			size := uint32(unsafe.Sizeof(sa))
			if _, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.SOL_IPV6, soOriginalDst,
				uintptr(unsafe.Pointer(&sa)), uintptr(unsafe.Pointer(&size)), 0); e != 0 {
				gerr = e
				return
			}
			port := binary.BigEndian.Uint16((*[2]byte)(unsafe.Pointer(&sa.Port))[:])
			res = netip.AddrPortFrom(netip.AddrFrom16(sa.Addr), port)
			return
		}
		var sa unix.RawSockaddrInet4
		size := uint32(unsafe.Sizeof(sa))
		if _, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.SOL_IP, soOriginalDst,
			uintptr(unsafe.Pointer(&sa)), uintptr(unsafe.Pointer(&size)), 0); e != 0 {
			gerr = e
			return
		}
		port := binary.BigEndian.Uint16((*[2]byte)(unsafe.Pointer(&sa.Port))[:])
		res = netip.AddrPortFrom(netip.AddrFrom4(sa.Addr), port)
	})
	if err != nil {
		return netip.AddrPort{}, err
	}
	if gerr != nil {
		return netip.AddrPort{}, fmt.Errorf("capture: original destination: %w", gerr)
	}
	return res, nil
}
