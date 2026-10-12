// Package disguise checks a candidate site from the VPS itself. The borrowed
// site must be reachable from here, speak TLS 1.3 with X25519, offer h2 and keep
// its handshake small enough for REALITY to copy, or the link would look wrong
// (or not come up at all).
package disguise

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
)

type Checker struct {
	// Dial lets tests (and odd networks) route the dial; the default connects to host:443.
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	Lookup  func(ctx context.Context, host string) ([]string, error)
	Timeout time.Duration
}

// Check never returns an error: every failure becomes a field the UI can show.
func (c Checker) Check(ctx context.Context, domain string) ctrl.DisguiseResult {
	domain = strings.TrimSpace(strings.ToLower(domain))
	var res ctrl.DisguiseResult
	if domain == "" || strings.ContainsAny(domain, " /:@") || !strings.Contains(domain, ".") {
		res.Err = "enter a plain domain name such as www.example.ir"
		return res
	}
	if c.Timeout == 0 {
		c.Timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	lookup := c.Lookup
	if lookup == nil {
		lookup = func(ctx context.Context, h string) ([]string, error) { return net.DefaultResolver.LookupHost(ctx, h) }
	}
	addrs, err := lookup(ctx, domain)
	if err != nil || len(addrs) == 0 {
		res.Err = "the VPS cannot resolve " + domain
		return res
	}
	res.Addrs = addrs
	dial := c.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	start := time.Now()
	raw, err := dial(ctx, "tcp", net.JoinHostPort(domain, "443"))
	if err != nil {
		res.Err = "cannot connect to " + domain + ":443 from the VPS"
		return res
	}
	defer raw.Close()
	// X25519 only: if the server accepts, it supports the curve REALITY needs
	cfg := &tls.Config{ServerName: domain, MinVersion: tls.VersionTLS13, CurvePreferences: []tls.CurveID{tls.X25519}, NextProtos: []string{"h2", "http/1.1"}}
	fl := &flight{Conn: raw}
	conn := tls.Client(fl, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		// retry without verification to tell "bad cert" from "no TLS 1.3 / X25519"
		raw2, derr := dial(ctx, "tcp", net.JoinHostPort(domain, "443"))
		if derr != nil {
			res.Err = "TLS 1.3 with X25519 failed: " + err.Error()
			return res
		}
		defer raw2.Close()
		cfg2 := cfg.Clone()
		cfg2.InsecureSkipVerify = true
		fl = &flight{Conn: raw2}
		conn2 := tls.Client(fl, cfg2)
		if err2 := conn2.HandshakeContext(ctx); err2 != nil {
			res.Err = "TLS 1.3 with X25519 failed: " + err2.Error()
			return res
		}
		st := conn2.ConnectionState()
		res.TLS13, res.X25519, res.H2 = st.Version == tls.VersionTLS13, true, st.NegotiatedProtocol == "h2"
		res.CertOK = false
		res.LatencyMS = int(time.Since(start).Milliseconds())
		res.OK = res.TLS13 && res.H2
		if !res.H2 {
			res.Err = "the site does not offer h2"
		}
		fl.judge(&res)
		return res
	}
	st := conn.ConnectionState()
	res.TLS13, res.X25519, res.CertOK = st.Version == tls.VersionTLS13, true, true
	res.H2 = st.NegotiatedProtocol == "h2"
	res.LatencyMS = int(time.Since(start).Milliseconds())
	res.OK = res.TLS13 && res.H2
	if !res.H2 {
		res.Err = "the site does not offer h2"
	}
	fl.judge(&res)
	return res
}

// realityMax is how much of the site's first flight REALITY buffers to copy its shape.
const realityMax = 8192

// flight notes the sizes of the site's TLS records as the handshake reads them.
type flight struct {
	net.Conn
	hdr   []byte // a record header split across reads
	skip  int    // body bytes of the current record still to come
	types []byte
	sizes []int // whole records, header included
}

func (f *flight) Read(b []byte) (int, error) {
	n, err := f.Conn.Read(b)
	f.note(b[:n])
	return n, err
}

func (f *flight) note(p []byte) {
	for len(p) > 0 && len(f.sizes) < 64 {
		if f.skip > 0 {
			k := min(f.skip, len(p))
			f.skip, p = f.skip-k, p[k:]
			continue
		}
		k := min(5-len(f.hdr), len(p))
		f.hdr, p = append(f.hdr, p[:k]...), p[k:]
		if len(f.hdr) == 5 {
			l := int(f.hdr[3])<<8 | int(f.hdr[4])
			f.types, f.sizes = append(f.types, f.hdr[0]), append(f.sizes, 5+l)
			f.skip, f.hdr = l, f.hdr[:0]
		}
	}
}

// copyBytes is what REALITY has to hold to copy the flight: after the ServerHello
// and a 6-byte ChangeCipherSpec, one encrypted record over 512 bytes stands for
// the rest; smaller ones mean every record up to Finished. -1: a shape it can't follow.
func (f *flight) copyBytes() int {
	const handshake, ccs, appData = 22, 20, 23
	i := slices.Index(f.types, handshake)
	if i < 0 || i+2 >= len(f.types) || f.types[i+1] != ccs || f.sizes[i+1] != 6 || f.types[i+2] != appData {
		return -1
	}
	if f.sizes[i+2] > 512 {
		return f.sizes[i+2]
	}
	n := 0
	for j := i + 2; j < len(f.types); j++ {
		if f.types[j] != appData {
			return -1
		}
		n += f.sizes[j]
	}
	return n
}

// judge fails a site that passed everything else but REALITY can't copy: no new
// link connection would come up with it.
func (f *flight) judge(res *ctrl.DisguiseResult) {
	if !res.OK {
		return
	}
	switch n := f.copyBytes(); {
	case n < 0:
		res.OK, res.Err = false, "the site's TLS handshake has a shape the link can't imitate"
	case n > realityMax:
		res.OK, res.Err = false, fmt.Sprintf("the site's TLS handshake is too big for the link to imitate (%d bytes; up to %d work)", n, realityMax)
	}
}
