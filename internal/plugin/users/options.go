package users

import (
	"slices"
	"strings"

	"github.com/nasnet-community/mtproto-remote-plugin/telemt"
)

// ProxyOptions are the advanced telemt settings the router owner may change.
type ProxyOptions struct {
	ExtraCoverNames  []string `json:"extra_cover_names,omitempty"`
	DeviceWindowSecs int      `json:"device_window_secs,omitempty"` // 0: telemt's own default
	LogLevel         string   `json:"log_level,omitempty"`          // "": normal
}

// Clone is a copy that shares nothing with o.
func (o ProxyOptions) Clone() ProxyOptions {
	o.ExtraCoverNames = slices.Clone(o.ExtraCoverNames)
	return o
}

const MaxExtraCovers = 4

var logLevels = map[string]bool{"silent": true, "normal": true, "verbose": true, "debug": true}

// OptionsPatch is a partial ProxyOptions: what is left out stays.
type OptionsPatch struct {
	ExtraCoverNames  *[]string `json:"extraCoverNames"`
	DeviceWindowSecs *int      `json:"deviceWindowSecs"`
	LogLevel         *string   `json:"logLevel"`
}

// Apply checks the patch against cover, the main cover site, and returns the result.
func (p OptionsPatch) Apply(o ProxyOptions, cover string) (ProxyOptions, error) {
	o = o.Clone()
	if p.ExtraCoverNames != nil {
		names := []string{}
		for _, n := range *p.ExtraCoverNames {
			n = strings.ToLower(strings.TrimSpace(n))
			switch {
			case !ValidDomain(n):
				return o, invalid("%q is not a domain name, like cdn.example.com", n)
			case n == cover:
				return o, invalid("%s is the cover site already", n)
			case slices.Contains(names, n):
				return o, invalid("%s is listed twice", n)
			}
			names = append(names, n)
		}
		if len(names) > MaxExtraCovers {
			return o, invalid("use at most %d extra cover names", MaxExtraCovers)
		}
		o.ExtraCoverNames = names
		if len(names) == 0 {
			o.ExtraCoverNames = nil
		}
	} else if slices.Contains(o.ExtraCoverNames, cover) {
		return o, invalid("%s is already one of the extra cover names: remove it there first", cover)
	}
	if p.DeviceWindowSecs != nil {
		if *p.DeviceWindowSecs < 0 || *p.DeviceWindowSecs > 86400 {
			return o, invalid("the device window is 0 to 86400 seconds")
		}
		o.DeviceWindowSecs = *p.DeviceWindowSecs
	}
	if p.LogLevel != nil {
		lvl := strings.ToLower(strings.TrimSpace(*p.LogLevel))
		if !logLevels[lvl] {
			return o, invalid("the log level is silent, normal, verbose or debug")
		}
		if lvl == "normal" {
			lvl = "" // the default adds nothing to the agent's state
		}
		o.LogLevel = lvl
	}
	return o, nil
}

// TelemtSettings is FakeTLS only, like the old plugin: user links are ee-links. An
// option left at its default adds no key, so the revision of an untouched server stays the same.
func TelemtSettings(o ProxyOptions) map[string]any {
	set := map[string]any{"modes": telemt.Modes{TLS: true}, "log_level": "normal"}
	if o.LogLevel != "" {
		set["log_level"] = o.LogLevel
	}
	if len(o.ExtraCoverNames) > 0 {
		set["tls_domains_extra"] = o.ExtraCoverNames
	}
	if o.DeviceWindowSecs > 0 {
		set["unique_ip_window_secs"] = o.DeviceWindowSecs
	}
	return set
}

// ValidDomain keeps obvious junk out, by the same rule as relayops.ValidDomain.
func ValidDomain(s string) bool {
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || !alnum(l[0]) || !alnum(l[len(l)-1]) {
			return false
		}
		for i := 1; i < len(l)-1; i++ {
			if !alnum(l[i]) && l[i] != '-' {
				return false
			}
		}
	}
	return true
}

func alnum(c byte) bool { return 'a' <= c && c <= 'z' || '0' <= c && c <= '9' }
