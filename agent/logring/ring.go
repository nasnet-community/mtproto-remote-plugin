// Package logring keeps the agent's and telemt's recent output in memory only.
// Nothing here ever touches disk or journald, and addresses are masked as the
// text arrives, so what the hub fetches cannot leak a client's IP.
package logring

import (
	"bytes"
	"net/netip"
	"regexp"
	"strings"
	"sync"
)

const maxLine = 2048

// a runaway line is cut this far past maxLine, so an address near the cut is whole when masked
const margin = 64

var (
	// no \b, whole dotted runs: "peer_198.51.100.23" is an address too (over-masking is fine)
	v4 = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3,}`)
	lo = regexp.MustCompile(`^127(?:\.\d{1,3}){3}$`)
	// runs of hex digits and colons; netip decides what inside one is an address
	hexRun = regexp.MustCompile(`[0-9A-Fa-f:]{3,}`)
)

// Mask replaces IP addresses with a placeholder; loopback stays readable.
func Mask(s string) string {
	s = v4.ReplaceAllStringFunc(s, func(m string) string {
		if lo.MatchString(m) {
			return m
		}
		return "[ip]"
	})
	return hexRun.ReplaceAllStringFunc(s, maskRun)
}

// maskRun masks v6 addresses whatever is glued to them ("conn::2001:db8::1"), in bounded work.
func maskRun(run string) string {
	if strings.Count(run, ":") < 2 {
		return run
	}
	var b strings.Builder
	for i := 0; i < len(run); {
		// a later start can reach further: "::2001:db8::1" is glue then an address
		start, end := -1, -1
		for s := i; s < min(i+3, len(run)); s++ {
			if e := addrEnd(run, s); e > end {
				start, end = s, e
			}
		}
		if start < 0 {
			b.WriteByte(run[i])
			i++
			continue
		}
		// a group cut short ("...::1:51234") is swallowed whole
		for end < len(run) && run[end] != ':' {
			end++
		}
		b.WriteString(run[i:start])
		b.WriteString("[ip6]")
		i = end
	}
	return b.String()
}

// addrEnd is the end of the longest address starting at s, or -1.
func addrEnd(run string, s int) int {
	for e := min(len(run), s+39); e >= s+3; e-- {
		if looksLikeClient(run[s:e]) {
			return e
		}
	}
	return -1
}

// looksLikeClient spares Rust paths ("socks5::connect"), :: and ::1: a client has a digit and 3 groups or a full one.
func looksLikeClient(s string) bool {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is6() || a.Zone() != "" || a.IsLoopback() || a.IsUnspecified() {
		return false
	}
	if !strings.ContainsAny(s, "0123456789") {
		return false
	}
	groups, full := 0, false
	for _, g := range strings.Split(s, ":") {
		if g != "" {
			groups++
			full = full || len(g) == 4
		}
	}
	return groups >= 3 || full
}

// Ring is an io.Writer that retains the last N lines.
type Ring struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
	part  []byte
	skip  bool // the rest of a line that was already cut
}

func New(lines int) *Ring { return &Ring{lines: make([]string, lines)} }

func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		line, rest, ended := bytes.Cut(p, []byte{'\n'})
		p = rest
		if !r.skip {
			// keeping more than this is pointless (it is cut) and makes masking slow
			if room := maxLine + margin + 1 - len(r.part); len(line) > room {
				line = line[:room]
			}
			r.part = append(r.part, line...)
		}
		switch {
		case ended:
			if !r.skip {
				r.add(string(r.part))
			}
			r.part, r.skip = r.part[:0], false
		case len(r.part) > maxLine+margin: // a line that never ends
			r.add(string(r.part))
			r.part, r.skip = r.part[:0], true
		}
	}
	return n, nil
}

func (r *Ring) add(line string) {
	if len(line) > maxLine+margin {
		// cut mid-line, and masking may pull the cut inside what is kept: drop a possible half address
		line = strings.TrimRight(line, "0123456789abcdefABCDEF.:")
	}
	// mask first: cutting first could leave half an address behind
	line = Mask(strings.TrimRight(line, "\r"))
	if len(line) > maxLine {
		line = line[:maxLine]
	}
	r.lines[r.next] = line
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}
}

// Tail returns up to n of the newest lines, oldest first.
func (r *Ring) Tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []string
	if r.full {
		all = append(all, r.lines[r.next:]...)
	}
	all = append(all, r.lines[:r.next]...)
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}
