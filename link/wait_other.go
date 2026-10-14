//go:build !unix

package link

import "net"

// No cheap readiness probe here: the Read that follows just blocks (and holds
// its buffer meanwhile).
func waitFD(net.Conn) error { return nil }
