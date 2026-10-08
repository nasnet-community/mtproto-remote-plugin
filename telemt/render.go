package telemt

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrHubEgress is returned for settings that cannot work when the agent's
// Telegram traffic is captured and tunnelled: the middle proxy and ad tag make
// telemt contact Telegram's proxy servers, which the capture rules do not cover.
var ErrHubEgress = errors.New("telemt: the middle proxy and ad tag are not available with hub egress")

// User is one credential rendered into [access.*]. Name is the telemt username.
type User struct {
	Name    string
	Secret  string
	MaxIPs  int // 0 => unlimited
	Enabled bool

	// ExpiresAt stops the user on the agent even if the hub is unreachable.
	ExpiresAt time.Time
	// RateUpBps / RateDownBps cap this user; 0 falls back to the settings default.
	RateUpBps, RateDownBps int64
}

// DefaultMetricsPort is the loopback Prometheus port when Settings does not pin one.
const DefaultMetricsPort = MetricsPortBase

// RenderInput is everything the renderer needs for the agent's single telemt.
type RenderInput struct {
	Listen   string // bind address; "" or a wildcard => all interfaces
	Port     int    // public listen port (443 when zero)
	Settings Settings
	Users    []User
	// MaskSock is the agent's socket for the cover site: telemt's probes it
	// masks and its certificate fetch go there, since telemt may not dial out.
	MaskSock string
}

func (in RenderInput) port() int {
	if in.Port != 0 {
		return in.Port
	}
	return 443
}

func (in RenderInput) bindIP() string {
	ip := strings.TrimSpace(in.Listen)
	if ip == "" || ip == "0.0.0.0" || ip == "::" || ip == "*" {
		return ""
	}
	return ip
}

// MetricsPort is where telemt's Prometheus endpoint listens (127.0.0.1 only).
func (in RenderInput) MetricsPort() int {
	if in.Settings.MetricsPort != 0 {
		return in.Settings.MetricsPort
	}
	return DefaultMetricsPort
}

// Rendered is the split config.toml. Core needs a telemt restart when it
// changes; Access is reloaded live by telemt's file watcher.
type Rendered struct {
	Core   string
	Access string
}

func (r Rendered) Config() string { return r.Core + r.Access }

// Render builds config.toml for a hub-egress telemt: direct upstream (the agent's
// nft rules redirect the datacentre connections), no middle proxy, API off.
// Every key is a real telemt 3.4.x key (config_strict rejects unknown ones).
func Render(in RenderInput) (Rendered, error) {
	s := in.Settings
	if s.MiddleProxy || strings.TrimSpace(s.AdtagDefault) != "" {
		return Rendered{}, ErrHubEgress
	}

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("[general]\n")
	w("use_middle_proxy = false\n")
	w("log_level = %s\n", tomlString(logLevel(s.LogLevel)))
	w("disable_colors = true\n")
	w("beobachten = false\n") // it would write client addresses into the working dir
	modes := s.EffectiveModes()
	w("\n[general.modes]\n")
	w("classic = %t\n", modes.Classic)
	w("secure = %t\n", modes.Secure)
	w("tls = %t\n", modes.TLS)
	// it has no use for its public address with a direct upstream, and every probe is a dial out
	w("\n[network]\n")
	w("stun_use = false\n")
	w("stun_tcp_fallback = false\n")
	w("http_ip_detect_urls = []\n")
	w("\n[server]\n")
	w("port = %d\n", in.port())
	if ip := in.bindIP(); ip != "" {
		w("listeners = [{ ip = %s, port = %d }]\n", tomlString(ip), in.port())
	}
	if s.ListenBacklog > 0 {
		w("listen_backlog = %d\n", s.ListenBacklog)
	}
	w("metrics_listen = %s\n", tomlString(fmt.Sprintf("127.0.0.1:%d", in.MetricsPort())))
	w("metrics_whitelist = [\"127.0.0.1/32\"]\n")
	w("[server.api]\n")
	w("enabled = false\n")
	if domains := s.TLSDomains(); len(domains) > 0 {
		w("\n[censorship]\n")
		w("tls_domain = %s\n", tomlString(domains[0]))
		if len(domains) > 1 {
			w("tls_domains = %s\n", tomlStringArray(domains))
		}
		w("unknown_sni_action = %s\n", tomlString(unknownSNIAction(s.UnknownSNIAction)))
		w("mask = %t\n", boolOr(s.Mask, true))
		if in.MaskSock != "" {
			// exclusive with mask_host: the agent dials that host for telemt
			w("mask_unix_sock = %s\n", tomlString(in.MaskSock))
		} else if mh := strings.TrimSpace(s.MaskHost); mh != "" {
			w("mask_host = %s\n", tomlString(mh))
		}
		w("tls_emulation = %t\n", boolOr(s.TLSEmulation, true))
	}
	w("\n[[upstreams]]\n")
	w("type = \"direct\"\n")

	var a strings.Builder
	aw := func(format string, args ...any) { fmt.Fprintf(&a, format, args...) }
	if s.UniqueIPWindowSecs > 0 {
		aw("\n[access]\n")
		aw("user_max_unique_ips_window_secs = %d\n", s.UniqueIPWindowSecs)
	}
	aw("\n[access.users]\n")
	for _, u := range in.Users {
		aw("%s = %s\n", tomlString(u.Name), tomlString(u.Secret))
	}
	aw("\n[access.user_enabled]\n")
	for _, u := range in.Users {
		aw("%s = %t\n", tomlString(u.Name), u.Enabled)
	}
	aw("\n[access.user_max_unique_ips]\n")
	for _, u := range in.Users {
		aw("%s = %d\n", tomlString(u.Name), maxUniqueIPs(u.MaxIPs))
	}
	var expiring []User
	for _, u := range in.Users {
		if !u.ExpiresAt.IsZero() {
			expiring = append(expiring, u)
		}
	}
	if len(expiring) > 0 {
		aw("\n[access.user_expirations]\n")
		for _, u := range expiring {
			aw("%s = %s\n", tomlString(u.Name), u.ExpiresAt.UTC().Format(time.RFC3339))
		}
	}
	type limit struct {
		name     string
		up, down int64
	}
	var limits []limit
	for _, u := range in.Users {
		up, down := u.RateUpBps, u.RateDownBps
		if up == 0 && down == 0 {
			up, down = s.RateLimitUpBps, s.RateLimitDownBps
		}
		if up > 0 || down > 0 {
			limits = append(limits, limit{u.Name, up, down})
		}
	}
	if len(limits) > 0 {
		aw("\n[access.user_rate_limits]\n")
		for _, l := range limits {
			aw("%s = { up_bps = %d, down_bps = %d }\n", tomlString(l.name), l.up, l.down)
		}
	}
	return Rendered{Core: b.String(), Access: a.String()}, nil
}

func maxUniqueIPs(k int) int {
	if k < 0 {
		return 0
	}
	return k
}

func logLevel(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug", "verbose", "normal", "silent":
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return "normal"
	}
}

// "accept" (proxy the stranger to the mask host) is the safe default: dropping
// is a tell.
func unknownSNIAction(v string) string {
	if strings.ToLower(strings.TrimSpace(v)) == "drop" {
		return "drop"
	}
	return "accept"
}

func boolOr(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

// tomlString renders a TOML basic string, escaping everything that would end the
// line or be rejected by the parser (a bad config stops live reload too).
func tomlString(v string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func tomlStringArray(vs []string) string {
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, tomlString(v))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
