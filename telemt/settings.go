// Package telemt holds the portable, testable MTProto (Telegram) pieces
// shared by the hub and the agent: the settings blob stored in
// core_inbounds.settings, per-user secret generation, and client proxy-link
// building. The runtime relay (telemt) is supervised by the agent core.
//
// Link building is mode-aware. An MTProto secret is 16 raw bytes, but what a
// client is handed depends on the camouflage mode the relay offers:
//
//	classic     <32 hex>                      plain obfuscated2
//	secure (dd) dd<32 hex>                    random-padding obfuscation
//	FakeTLS(ee) ee<32 hex><hex(sni domain)>   wrapped in a real TLS 1.3 handshake
//
// Handing a client the classic form when the relay has classic disabled makes
// the tunnel simply not connect, so the link must be derived from the inbound's
// enabled modes — never hardcoded.
package telemt

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Mode is one MTProto camouflage mode offered by the relay.
type Mode string

const (
	ModeClassic Mode = "classic"
	ModeSecure  Mode = "secure"
	ModeTLS     Mode = "tls"
)

// Modes is the set of camouflage modes an inbound offers.
type Modes struct {
	Classic bool `json:"classic"`
	Secure  bool `json:"secure"`
	TLS     bool `json:"tls"`
}

// Any reports whether at least one mode is enabled.
func (m Modes) Any() bool { return m.Classic || m.Secure || m.TLS }

// List returns the enabled modes weakest-first (classic, secure, tls) — the
// order telemt's [general.modes] section is rendered in.
func (m Modes) List() []Mode {
	var out []Mode
	if m.Classic {
		out = append(out, ModeClassic)
	}
	if m.Secure {
		out = append(out, ModeSecure)
	}
	if m.TLS {
		out = append(out, ModeTLS)
	}
	return out
}

// Preferred returns the strongest enabled mode: FakeTLS beats secure beats
// classic. It returns "" when no mode is enabled.
func (m Modes) Preferred() Mode {
	switch {
	case m.TLS:
		return ModeTLS
	case m.Secure:
		return ModeSecure
	case m.Classic:
		return ModeClassic
	default:
		return ""
	}
}

// DefaultModes is what an inbound offers when its settings name no mode. It
// mirrors the agent renderer's default so hub links and relay config agree.
func DefaultModes() Modes { return Modes{Classic: true, Secure: true, TLS: true} }

// Settings mirrors the mtproto core_inbounds.settings JSONB. It is the single
// source of truth for the blob: the hub writes it, the agent's telemt renderer
// reads it. Every field maps to a telemt 3.4.x config key (verified against the
// binary's general.config_strict validator).
type Settings struct {
	// --- camouflage -------------------------------------------------------
	Modes Modes `json:"modes"` // [general.modes]; none set => all offered

	// TLSDomain is the primary FakeTLS SNI ([censorship] tls_domain). Empty
	// disables the whole [censorship] section.
	TLSDomain string `json:"tls_domain"`
	// TLSDomainsExtra are additional SNI domains ([censorship] tls_domains).
	// Each yields its own ee-link, letting one inbound impersonate several sites.
	TLSDomainsExtra []string `json:"tls_domains_extra"`
	// MaskHost is where a probe that fails authentication is proxied
	// ([censorship] mask_host). Empty => the primary TLS domain.
	MaskHost string `json:"mask_host"`
	// UnknownSNIAction is [censorship] unknown_sni_action ("accept"|"drop").
	UnknownSNIAction string `json:"unknown_sni_action"`
	// Mask enables proxying bad clients to the mask host instead of dropping
	// them ([censorship] mask). Defaults on — dropping is a DPI tell.
	Mask *bool `json:"mask"`
	// TLSEmulation makes telemt mirror the real site's TLS fingerprint
	// ([censorship] tls_emulation). Defaults on.
	TLSEmulation *bool `json:"tls_emulation"`

	// LinkMode pins which camouflage mode client links are issued for
	// ("auto"|"classic"|"secure"|"tls"). "auto" (default) picks the strongest
	// enabled mode.
	LinkMode string `json:"link_mode"`
	// LinkAllModes issues one link per enabled mode instead of a single link,
	// so a user whose network blocks one mode can fall back to another.
	LinkAllModes bool `json:"link_all_modes"`

	// --- Telegram middle proxy -------------------------------------------
	MiddleProxy  bool   `json:"middle_proxy"`  // [general] use_middle_proxy; direct egress only
	AdtagDefault string `json:"adtag_default"` // [general] ad_tag; requires middle proxy

	// --- access limits ----------------------------------------------------
	// UniqueIPWindowSecs is [access] user_max_unique_ips_window_secs: how long
	// a device's IP counts against the per-user unique-IP (K) limit.
	UniqueIPWindowSecs int `json:"unique_ip_window_secs"`
	// RateLimitUpBps / RateLimitDownBps cap every user of this inbound
	// ([access.user_rate_limits] {up_bps, down_bps}). 0 => unlimited.
	RateLimitUpBps   int64 `json:"rate_limit_up_bps"`
	RateLimitDownBps int64 `json:"rate_limit_down_bps"`

	// --- server tuning ----------------------------------------------------
	LogLevel      string `json:"log_level"`      // [general] log_level: debug|verbose|normal|silent
	ListenBacklog int    `json:"listen_backlog"` // [server] listen_backlog; 0 => telemt default

	// --- loopback ports ---------------------------------------------------
	MetricsPort int `json:"metrics_port"` // Prometheus scrape port; 0 => 14300+id
	SocksPort   int `json:"socks_port"`   // loopback egress relay port; 0 => 12300+id
}

// Loopback port bases: an inbound's egress relay and telemt metrics listen on
// 127.0.0.1 at base+(id mod 1000) unless its settings pin a port.
const (
	RelayPortBase   = 12300
	MetricsPortBase = 14300
)

// LoopbackPorts returns the relay and metrics ports inbound id listens on.
func (s Settings) LoopbackPorts(id uint) (relay, metrics int) {
	return s.LoopbackPortsAt(int(id % 1000))
}

// LoopbackPortsAt is LoopbackPorts for an already-derived id offset.
func (s Settings) LoopbackPortsAt(offset int) (relay, metrics int) {
	relay, metrics = s.SocksPort, s.MetricsPort
	if relay == 0 {
		relay = RelayPortBase + offset
	}
	if metrics == 0 {
		metrics = MetricsPortBase + offset
	}
	return relay, metrics
}

// ParseSettings unmarshals an mtproto settings blob and applies defaults. An
// empty blob, and one with every mode disabled, both mean "offer all modes" —
// matching what the relay does with an empty [general.modes].
func ParseSettings(raw json.RawMessage) (Settings, error) {
	var s Settings
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, fmt.Errorf("mtproto: parse settings: %w", err)
		}
	}
	if !s.Modes.Any() {
		s.Modes = DefaultModes()
	}
	return s, nil
}

// TLSDomains returns the FakeTLS SNI domains in link order: the primary first,
// then the extras, de-duplicated and with blanks dropped.
func (s Settings) TLSDomains() []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range append([]string{s.TLSDomain}, s.TLSDomainsExtra...) {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// EffectiveModes returns the modes clients may actually be issued links for.
// FakeTLS needs an SNI domain to encode into the secret, so with no domain
// configured the tls mode is dropped even if the operator checked it.
func (s Settings) EffectiveModes() Modes {
	m := s.Modes
	if len(s.TLSDomains()) == 0 {
		m.TLS = false
	}
	return m
}

// GenerateSecret mints a 16-byte (32 hex char) MTProto secret.
func GenerateSecret() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate mtproto secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// NormalizeSecret strips any client-facing dd/ee prefix and trailing domain hex
// from a stored secret and validates it as 32 lowercase hex chars. Stored
// secrets are always the bare 16 bytes; this guards against an operator pasting
// a full client secret into the field.
func NormalizeSecret(secret string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(secret))
	// Length disambiguates, NOT the prefix: "ee1122…" can be a perfectly valid
	// bare 32-char secret, so a bare secret is only ever recognised by its
	// length. Prefixes are stripped only when there is more than a bare secret
	// present (dd + 32, or ee + 32 + hex(domain)).
	switch {
	case len(s) == 32:
	case len(s) == 34 && strings.HasPrefix(s, "dd"):
		s = s[2:]
	case len(s) > 34 && strings.HasPrefix(s, "ee"):
		s = s[2:34] // drop the ee prefix and the trailing hex(domain)
	default:
		return "", fmt.Errorf("mtproto: secret must be 32 hex chars, got %d", len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("mtproto: secret is not hex: %w", err)
	}
	return s, nil
}

// ClientSecret renders the secret a client is given for one camouflage mode.
// domain is the FakeTLS SNI and is required for (and only used by) ModeTLS.
func ClientSecret(mode Mode, secret, domain string) (string, error) {
	base, err := NormalizeSecret(secret)
	if err != nil {
		return "", err
	}
	switch mode {
	case ModeClassic:
		return base, nil
	case ModeSecure:
		return "dd" + base, nil
	case ModeTLS:
		domain = strings.TrimSpace(domain)
		if domain == "" {
			return "", fmt.Errorf("mtproto: FakeTLS link needs an SNI domain")
		}
		return "ee" + base + hex.EncodeToString([]byte(domain)), nil
	default:
		return "", fmt.Errorf("mtproto: unknown camouflage mode %q", mode)
	}
}

// Link is one issuable client link.
type Link struct {
	Mode   Mode   `json:"mode"`
	Domain string `json:"domain,omitempty"` // FakeTLS SNI, for ModeTLS
	URI    string `json:"uri"`
}

// BuildProxyLink renders a Telegram proxy link for an already-rendered
// client secret:
//
//	tg://proxy?port=<port>&secret=<secret>&server=<host>
func BuildProxyLink(host string, port int, clientSecret string) string {
	q := url.Values{}
	q.Set("server", host)
	q.Set("port", fmt.Sprintf("%d", port))
	q.Set("secret", clientSecret)
	return "tg://proxy?" + q.Encode()
}

// BuildLinks renders the client links an inbound's settings call for.
//
// With LinkAllModes the result is one link per enabled mode (and, for FakeTLS,
// one per configured SNI domain) weakest-first, so a client blocked on one mode
// can fall back. Otherwise it is the single link for LinkMode, defaulting to the
// strongest enabled mode.
//
// It returns an error only when no link can be issued at all — the settings
// enable no usable mode. Callers surface that rather than silently emitting a
// classic link the relay would reject.
func BuildLinks(host string, port int, secret string, s Settings) ([]Link, error) {
	modes := s.EffectiveModes()
	domains := s.TLSDomains()

	build := func(mode Mode) ([]Link, error) {
		if mode != ModeTLS {
			cs, err := ClientSecret(mode, secret, "")
			if err != nil {
				return nil, err
			}
			return []Link{{Mode: mode, URI: BuildProxyLink(host, port, cs)}}, nil
		}
		var out []Link
		for _, d := range domains {
			cs, err := ClientSecret(ModeTLS, secret, d)
			if err != nil {
				return nil, err
			}
			out = append(out, Link{Mode: ModeTLS, Domain: d, URI: BuildProxyLink(host, port, cs)})
		}
		return out, nil
	}

	if s.LinkAllModes {
		var out []Link
		for _, m := range modes.List() {
			ls, err := build(m)
			if err != nil {
				return nil, err
			}
			out = append(out, ls...)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("mtproto: no camouflage mode is usable for client links")
		}
		return out, nil
	}

	mode := modes.Preferred()
	if pinned := Mode(strings.TrimSpace(strings.ToLower(s.LinkMode))); pinned != "" && pinned != "auto" {
		// A pinned mode the relay does not offer would hand out a link that
		// cannot connect; fall back to the strongest mode it does offer.
		switch {
		case pinned == ModeClassic && modes.Classic,
			pinned == ModeSecure && modes.Secure,
			pinned == ModeTLS && modes.TLS:
			mode = pinned
		}
	}
	if mode == "" {
		return nil, fmt.Errorf("mtproto: no camouflage mode is usable for client links")
	}
	ls, err := build(mode)
	if err != nil {
		return nil, err
	}
	if !s.LinkAllModes && len(ls) > 1 {
		ls = ls[:1] // one pinned link: the primary SNI domain
	}
	return ls, nil
}
