// Package capture sends telemt's Telegram-bound connections into the tunnel
// without telemt knowing: nft redirects connections from telemt's uid to the
// datacentre prefixes onto a loopback listener, and the agent reads the
// original destination off the socket.
package capture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// Table is the nft table name in both the ip and ip6 families.
const Table = "mtproto_agent"

// Rules describes the capture for one telemt uid.
type Rules struct {
	UID  uint32 // telemt's uid; the match is on the socket owner, so the agent itself is unaffected
	Port int    // loopback port the agent listens on (v4 and v6)
	V4   []netip.Prefix
	V6   []netip.Prefix
}

var ErrNoPrefixes = errors.New("capture: no datacentre prefixes: refusing to install rules that would capture nothing")

// Script renders one atomic nft transaction that replaces both tables.
//
// telemt may reach nothing but Telegram, and only through the agent: its
// datacentre connections on 443 are redirected to the agent's loopback
// listener, and everything else it starts, in either family, is refused. Its
// answers to its own clients are untouched (ct direction original).
func (r Rules) Script() (string, error) {
	if len(r.V4) == 0 && len(r.V6) == 0 {
		return "", ErrNoPrefixes
	}
	if r.UID == 0 {
		return "", errors.New("capture: refusing to match uid 0: that would capture the agent itself")
	}
	if r.Port <= 0 || r.Port > 65535 {
		return "", fmt.Errorf("capture: bad port %d", r.Port)
	}
	var b strings.Builder
	family := func(fam, addrType, kw, loopback string, prefixes []netip.Prefix) {
		// add+delete+define in one file: atomic, and an emptied family loses its stale redirect
		fmt.Fprintf(&b, "add table %s %s\ndelete table %s %s\n", fam, Table, fam, Table)
		fmt.Fprintf(&b, "table %s %s {\n", fam, Table)
		if len(prefixes) > 0 {
			fmt.Fprintf(&b, "\tset dc {\n\t\ttype %s\n\t\tflags interval\n\t\telements = { %s }\n\t}\n", addrType, joinPrefixes(prefixes))
			fmt.Fprintf(&b, "\tchain nat_out {\n\t\ttype nat hook output priority -100; policy accept;\n")
			fmt.Fprintf(&b, "\t\tmeta skuid %d %s daddr @dc tcp dport 443 redirect to :%d\n\t}\n", r.UID, kw, r.Port)
		}
		fmt.Fprintf(&b, "\tchain filter_out {\n\t\ttype filter hook output priority 0; policy accept;\n")
		fmt.Fprintf(&b, "\t\toifname \"lo\" accept\n")
		// a redirected connection still shows its old interface here, but its new address
		fmt.Fprintf(&b, "\t\t%s daddr %s accept\n", kw, loopback)
		fmt.Fprintf(&b, "\t\tmeta skuid %d ct direction original meta l4proto tcp reject with tcp reset\n", r.UID)
		fmt.Fprintf(&b, "\t\tmeta skuid %d ct direction original reject\n\t}\n}\n", r.UID)
	}
	family("ip", "ipv4_addr", "ip", "127.0.0.0/8", r.V4)
	family("ip6", "ipv6_addr", "ip6", "::1", r.V6)
	return b.String(), nil
}

func joinPrefixes(ps []netip.Prefix) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Masked().String()
	}
	return strings.Join(out, ", ")
}

// Teardown removes both tables; it is safe to run when they do not exist.
func Teardown() string {
	return fmt.Sprintf("add table ip %[1]s\ndelete table ip %[1]s\nadd table ip6 %[1]s\ndelete table ip6 %[1]s\n", Table)
}

// Apply feeds a script to `nft -f -`.
func Apply(ctx context.Context, script string) error {
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// Present reports whether the capture table is installed (either family).
func Present(ctx context.Context) bool {
	for _, fam := range []string{"ip", "ip6"} {
		if exec.CommandContext(ctx, "nft", "list", "table", fam, Table).Run() == nil {
			return true
		}
	}
	return false
}

// Live: both tables in place, with the redirect in each family that has
// prefixes (v4, v6) and the refusal everywhere (a `flush ruleset` takes them all).
func Live(ctx context.Context, v4, v6 bool) bool {
	if !v4 && !v6 {
		return false
	}
	for fam, redirect := range map[string]bool{"ip": v4, "ip6": v6} {
		out, err := exec.CommandContext(ctx, "nft", "list", "table", fam, Table).Output()
		if err != nil || !bytes.Contains(out, []byte("chain filter_out")) || (redirect && !bytes.Contains(out, []byte("chain nat_out"))) {
			return false
		}
	}
	return true
}

// Split sorts prefixes into the v4 and v6 sets.
func Split(all []netip.Prefix) (v4, v6 []netip.Prefix) {
	for _, p := range all {
		if p.Addr().Is4() || p.Addr().Is4In6() {
			v4 = append(v4, p)
		} else {
			v6 = append(v6, p)
		}
	}
	return v4, v6
}
