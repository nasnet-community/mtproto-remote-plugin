//go:build unix

package link

import (
	"net"
	"syscall"
)

// waitFD parks with no buffer; the poller is edge-triggered, so a MSG_PEEK must really probe for queued data.
func waitFD(c net.Conn) error {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return nil
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return err
	}
	var probe [1]byte
	var seen error
	err = rc.Read(func(fd uintptr) bool {
		_, _, err := syscall.Recvfrom(int(fd), probe[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
			return false // nothing yet: wait for the next readiness edge
		}
		if err != nil && err != syscall.EINTR {
			seen = err // Linux reports a reset once: the Read after this would see EOF
		}
		return true
	})
	if err == nil {
		err = seen
	}
	return err
}
