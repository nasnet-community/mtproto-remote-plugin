package telemt

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

// settings goes through ParseSettings like the agent does, so the "no mode set
// means every mode" default is applied.
func settings(_ testing.TB, raw string) Settings {
	s, err := ParseSettings([]byte(raw))
	if err != nil {
		panic(err)
	}
	return s
}

func baseInput() RenderInput {
	return RenderInput{
		Port:     443,
		Settings: settings(testing.TB(nil), `{"tls_domain":"mirror.example.ir"}`),
		Users: []User{
			{Name: "alice@example.com", Secret: "00112233445566778899aabbccddeeff", Enabled: true, MaxIPs: 2},
			{Name: "bob", Secret: "ffeeddccbbaa99887766554433221100", Enabled: false},
		},
	}
}

const golden = `[general]
use_middle_proxy = false
log_level = "normal"
disable_colors = true
beobachten = false

[general.modes]
classic = true
secure = true
tls = true

[network]
stun_use = false
stun_tcp_fallback = false
http_ip_detect_urls = []

[server]
port = 443
metrics_listen = "127.0.0.1:14300"
metrics_whitelist = ["127.0.0.1/32"]
[server.api]
enabled = false

[censorship]
tls_domain = "mirror.example.ir"
unknown_sni_action = "accept"
mask = true
tls_emulation = true

[[upstreams]]
type = "direct"
`

func TestGoldenCore(t *testing.T) {
	r, err := Render(baseInput())
	if err != nil {
		t.Fatal(err)
	}
	if r.Core != golden {
		t.Fatalf("core drifted:\n%s", r.Core)
	}
}

func TestAccessSectionAndLimits(t *testing.T) {
	in := baseInput()
	in.Users[0].ExpiresAt = time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	in.Users[0].RateUpBps, in.Users[0].RateDownBps = 1000, 2000
	in.Settings.RateLimitUpBps = 50
	in.Settings.UniqueIPWindowSecs = 600
	r, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[access]\nuser_max_unique_ips_window_secs = 600",
		`"alice@example.com" = "00112233445566778899aabbccddeeff"`,
		`"bob" = false`,
		`"alice@example.com" = 2`,
		`"alice@example.com" = 2027-01-02T03:04:05Z`,
		`"alice@example.com" = { up_bps = 1000, down_bps = 2000 }`,
		`"bob" = { up_bps = 50, down_bps = 0 }`, // falls back to the settings default
	} {
		if !strings.Contains(r.Access, want) {
			t.Errorf("access section missing %q:\n%s", want, r.Access)
		}
	}
}

// What we render has to be TOML telemt's parser accepts, even for awkward names.
func TestRenderedConfigIsValidTOML(t *testing.T) {
	in := baseInput()
	in.Listen = "203.0.113.7"
	in.Settings.TLSDomainsExtra = []string{"other.example.ir"}
	in.Settings.MaskHost = "mask.example.ir"
	in.Users = append(in.Users,
		User{Name: `quote"d\name`, Secret: "aa", Enabled: true},
		User{Name: "tab\tand\nnewline", Secret: "bb", Enabled: true},
		User{Name: "ünïcode-名前", Secret: "cc", Enabled: true},
		User{Name: "ctl\x01\x7f", Secret: "dd", Enabled: true},
	)
	r, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if _, err := toml.Decode(r.Config(), &out); err != nil {
		t.Fatalf("invalid TOML: %v\n%s", err, r.Config())
	}
	users := out["access"].(map[string]any)["users"].(map[string]any)
	for _, name := range []string{`quote"d\name`, "tab\tand\nnewline", "ünïcode-名前", "ctl\x01\x7f"} {
		if _, ok := users[name]; !ok {
			t.Errorf("user %q did not survive the round trip", name)
		}
	}
	if out["server"].(map[string]any)["listeners"] == nil {
		t.Error("bind address not rendered")
	}
}

func TestHubEgressRefusesMiddleProxyAndAdTag(t *testing.T) {
	for name, mutate := range map[string]func(*Settings){
		"middle proxy": func(s *Settings) { s.MiddleProxy = true },
		"ad tag":       func(s *Settings) { s.AdtagDefault = "deadbeef" },
	} {
		in := baseInput()
		mutate(&in.Settings)
		if _, err := Render(in); !errors.Is(err, ErrHubEgress) {
			t.Errorf("%s: err = %v, want ErrHubEgress", name, err)
		}
	}
}

func TestNoTLSDomainDropsFakeTLSMode(t *testing.T) {
	in := baseInput()
	in.Settings = settings(t, `{}`)
	r, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Core, "tls = false") || strings.Contains(r.Core, "[censorship]") {
		t.Fatalf("FakeTLS without a domain must be off:\n%s", r.Core)
	}
}

// beobachten writes client addresses into telemt's working dir; it must stay off.
func TestClientIPRecordingIsOff(t *testing.T) {
	r, err := Render(baseInput())
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if _, err := toml.Decode(r.Config(), &out); err != nil {
		t.Fatal(err)
	}
	if v, ok := out["general"].(map[string]any)["beobachten"]; !ok || v != false {
		t.Fatalf("[general] beobachten = %v (set %v), want false", v, ok)
	}
	if _, ok := out["beobachten"]; ok {
		t.Fatal("the deprecated top-level key is rendered")
	}
}

// Core is what forces a restart; user changes must not touch it.
func TestAccessChangesLeaveCoreUntouched(t *testing.T) {
	a, _ := Render(baseInput())
	in := baseInput()
	in.Users = append(in.Users, User{Name: "carol", Secret: "ee", Enabled: true})
	in.Users[0].Enabled = false
	b, _ := Render(in)
	if a.Core != b.Core {
		t.Fatal("a user change altered the restart-sensitive section")
	}
	if a.Access == b.Access {
		t.Fatal("access section ignored the user change")
	}
}

func TestLinksForHubNodes(t *testing.T) {
	s := settings(t, `{"tls_domain":"mirror.example.ir"}`)
	ls, err := BuildLinks("203.0.113.7", 443, "00112233445566778899aabbccddeeff", s)
	if err != nil || len(ls) != 1 || ls[0].Mode != ModeTLS {
		t.Fatalf("links = %+v err=%v", ls, err)
	}
	if !strings.Contains(ls[0].URI, "secret=ee00112233445566778899aabbccddeeff"+"6d6972726f722e6578616d706c652e6972") {
		t.Fatalf("FakeTLS secret wrong: %s", ls[0].URI)
	}
}

// telemt may reach nothing but Telegram: the cover site (probes it masks, and
// the certificate it copies) goes to the agent's socket, never straight out.
func TestTheCoverSiteGoesThroughTheAgentsSocket(t *testing.T) {
	in := baseInput()
	in.MaskSock = "/var/lib/mtproto-agent/telemt/mask.sock"
	in.Settings.MaskHost = "mask.example.ir"
	r, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Core, "mask_unix_sock = \"/var/lib/mtproto-agent/telemt/mask.sock\"\n") {
		t.Errorf("no mask socket:\n%s", r.Core)
	}
	// telemt refuses both at once; the agent dials the tunnel's mask host itself
	if strings.Contains(r.Core, "mask_host") {
		t.Errorf("mask_host rendered next to the socket:\n%s", r.Core)
	}
	var out map[string]any
	if _, err := toml.Decode(r.Config(), &out); err != nil {
		t.Fatalf("invalid TOML: %v", err)
	}
	net := out["network"].(map[string]any)
	if net["stun_use"] != false || net["stun_tcp_fallback"] != false || len(net["http_ip_detect_urls"].([]any)) != 0 {
		t.Errorf("telemt still looks for its public address: %v", net)
	}
}
