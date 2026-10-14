// Package tgnet decides which destinations the hub dials for an agent: Telegram's
// ranges on port 443. The pinned list ships in the binary; a refresh only adds.
package tgnet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Guard rails for a refreshed list: a hijacked URL must not be able to open the
// whole internet to agents.
const (
	maxRefreshPrefixes = 512
	minBits4           = 12
	minBits6           = 29
)

var ErrBadList = errors.New("tgnet: refused list")

// local ranges reach the hub's own host or LAN; documentation ranges stay allowed for the placeholder.
var local = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		"::/96", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2002::/16",
		"fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// checkPublic refuses a prefix that touches any local range.
func checkPublic(p netip.Prefix) error {
	if !p.IsValid() {
		return fmt.Errorf("%w: %s is not a prefix", ErrBadList, p)
	}
	for _, l := range local {
		if p.Overlaps(l) {
			return fmt.Errorf("%w: %s overlaps %s, which is not a datacentre range", ErrBadList, p, l)
		}
	}
	return nil
}

func isLocal(a netip.Addr) bool {
	for _, l := range local {
		if l.Contains(a) {
			return true
		}
	}
	return false
}

// Allowlist is safe for concurrent use.
type Allowlist struct {
	ports      []uint16
	allowLocal bool // NewWithPorts: tests reach fakes on loopback

	mu      sync.RWMutex
	pinned  []netip.Prefix
	extra   []netip.Prefix
	updated time.Time

	vmu        sync.Mutex
	violations []time.Time
	total      uint64
}

// New builds an allowlist from the pinned prefixes: port 443 only, never a local range.
func New(pinned []netip.Prefix) *Allowlist {
	var keep []netip.Prefix
	for _, p := range pinned {
		if checkPublic(p) == nil {
			keep = append(keep, p)
		}
	}
	return &Allowlist{pinned: keep, ports: []uint16{443}}
}

// NewWithPorts is for tests that dial a local fake on another port; it trusts its list as given.
func NewWithPorts(pinned []netip.Prefix, ports ...uint16) *Allowlist {
	return &Allowlist{pinned: append([]netip.Prefix(nil), pinned...), ports: ports, allowLocal: true}
}

// Permit reports whether ap is a Telegram address on an allowed port.
func (a *Allowlist) Permit(ap netip.AddrPort) bool {
	if a == nil {
		return false
	}
	okPort := false
	for _, p := range a.ports {
		if ap.Port() == p {
			okPort = true
		}
	}
	if !okPort {
		return false
	}
	addr := ap.Addr().Unmap()
	if !a.allowLocal && isLocal(addr) {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, p := range a.pinned {
		if p.Contains(addr) {
			return true
		}
	}
	for _, p := range a.extra {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Prefixes is the effective list (pinned + refreshed), for pushing to agents.
func (a *Allowlist) Prefixes() []netip.Prefix {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]netip.Prefix, 0, len(a.pinned)+len(a.extra))
	out = append(out, a.pinned...)
	out = append(out, a.extra...)
	return out
}

// ParseList reads one prefix (or bare address) per line; '#' starts a comment.
func ParseList(r io.Reader) ([]netip.Prefix, error) {
	var out []netip.Prefix
	sc := bufio.NewScanner(io.LimitReader(r, 1<<20))
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		p, err := netip.ParsePrefix(line)
		if err != nil {
			addr, aerr := netip.ParseAddr(line)
			if aerr != nil {
				return nil, fmt.Errorf("line %d: %q is not a prefix", n, line)
			}
			p = netip.PrefixFrom(addr, addr.BitLen())
		}
		out = append(out, p.Masked())
	}
	return out, sc.Err()
}

// Merge replaces the refreshed part of the list with extra, after the guard
// rails. Pinned prefixes are never removed. It returns how many prefixes are
// new compared with the previous refresh.
func (a *Allowlist) Merge(extra []netip.Prefix) (added int, err error) {
	if len(extra) == 0 {
		return 0, fmt.Errorf("%w: empty", ErrBadList)
	}
	if len(extra) > maxRefreshPrefixes {
		return 0, fmt.Errorf("%w: %d prefixes", ErrBadList, len(extra))
	}
	for _, p := range extra {
		if err := checkPublic(p); err != nil {
			return 0, err
		}
		min := minBits4
		if p.Addr().Is6() {
			min = minBits6
		}
		if p.Bits() < min {
			return 0, fmt.Errorf("%w: %s is too broad", ErrBadList, p)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	known := map[netip.Prefix]bool{}
	for _, p := range a.pinned {
		known[p] = true
	}
	for _, p := range a.extra {
		known[p] = true
	}
	var next []netip.Prefix
	seen := map[netip.Prefix]bool{}
	for _, p := range extra {
		if seen[p] {
			continue
		}
		seen[p] = true
		pinned := false
		for _, q := range a.pinned {
			if q == p {
				pinned = true
			}
		}
		if pinned {
			continue
		}
		if !known[p] {
			added++
		}
		next = append(next, p)
	}
	a.extra = next
	a.updated = time.Now()
	return added, nil
}

// Refresh fetches a list and merges it. Any failure leaves the current list
// exactly as it was.
func (a *Allowlist) Refresh(ctx context.Context, fetch func(context.Context) (io.ReadCloser, error)) (int, error) {
	rc, err := fetch(ctx)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	list, err := ParseList(rc)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrBadList, err)
	}
	return a.Merge(list)
}

func (a *Allowlist) UpdatedAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.updated
}

// Violation records a refused dial; ViolationsSince feeds the alert threshold.
func (a *Allowlist) Violation() {
	if a == nil {
		return
	}
	a.vmu.Lock()
	defer a.vmu.Unlock()
	now := time.Now()
	a.total++
	a.violations = append(a.violations, now)
	if len(a.violations) > 4096 {
		a.violations = a.violations[len(a.violations)-2048:]
	}
}

func (a *Allowlist) ViolationsSince(d time.Duration) int {
	a.vmu.Lock()
	defer a.vmu.Unlock()
	cut := time.Now().Add(-d)
	n := 0
	for i := len(a.violations) - 1; i >= 0 && a.violations[i].After(cut); i-- {
		n++
	}
	return n
}

func (a *Allowlist) ViolationsTotal() uint64 {
	if a == nil {
		return 0
	}
	a.vmu.Lock()
	defer a.vmu.Unlock()
	return a.total
}
