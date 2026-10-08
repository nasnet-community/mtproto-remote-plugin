// Package disguise checks a candidate site from the VPS itself. The borrowed
// site must be reachable from here, speak TLS 1.3 with X25519 and offer h2, or
// REALITY (and telemt's FakeTLS) would look wrong.
package disguise

import (
	"context"
	"crypto/tls"
	"net"
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
	conn := tls.Client(raw, cfg)
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
		conn2 := tls.Client(raw2, cfg2)
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
	return res
}
