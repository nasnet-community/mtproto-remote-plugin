package installer

import (
	"context"
	"errors"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/nasnet-community/mtproto-remote-plugin/agent/disguise"
	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
)

// CheckCover tests a cover site from the VPS before anything is put on it: the
// VPS resolves the name and opens the connection (an SSH forward), and the
// agent's own check runs over it from here. checked is false when the box
// won't forward for this login (AllowTcpForwarding no) or can't look names up
// for us; the agent then tests the site after the upload, as before.
func CheckCover(ctx context.Context, c *Conn, domain string) (res ctrl.DisguiseResult, checked bool) {
	checked = true
	lookup := func(ctx context.Context, host string) ([]string, error) {
		out, err := c.Run(ctx, "getent ahosts "+shq(host), nil)
		if err != nil {
			// 2 is getent's "no such name"; a lookup that ran out of time failed too
			var ee *ssh.ExitError
			if !(errors.As(err, &ee) && ee.ExitStatus() == 2) && ctx.Err() == nil {
				checked = false
			}
			return nil, err
		}
		return hostAddrs(out), nil
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := c.c.DialContext(ctx, network, addr)
		var oe *ssh.OpenChannelError
		if errors.As(err, &oe) && oe.Reason == ssh.Prohibited {
			checked = false
		}
		return conn, err
	}
	res = disguise.Checker{Dial: dial, Lookup: lookup}.Check(ctx, domain)
	// the time includes our own leg to the VPS, so it says nothing about the site
	res.LatencyMS = 0
	return res, checked
}

// hostAddrs is the addresses in getent's output, each once, in its order.
func hostAddrs(out string) []string {
	var addrs []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || net.ParseIP(f[0]) == nil || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		addrs = append(addrs, f[0])
	}
	return addrs
}
