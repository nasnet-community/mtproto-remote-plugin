package userapi

import (
	"net/http"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/users"
)

type UsageView struct {
	Live        bool       `json:"live"`
	UsersOnline int        `json:"usersOnline"`
	Connections int        `json:"connections"`
	Devices     int        `json:"devices"`
	RateUp      int64      `json:"rateUp"`
	RateDown    int64      `json:"rateDown"`
	TodayUp     uint64     `json:"todayUp"`
	TodayDown   uint64     `json:"todayDown"`
	MonthUp     uint64     `json:"monthUp"`
	MonthDown   uint64     `json:"monthDown"`
	UpdatedAt   *time.Time `json:"updatedAt"`
}

func UsageOf(u users.ServerUsage) UsageView {
	return UsageView{
		Live: u.Live, UsersOnline: u.UsersOnline, Connections: u.Connections, Devices: u.Devices,
		RateUp: int64(u.RateUp), RateDown: int64(u.RateDown), TodayUp: u.TodayUp, TodayDown: u.TodayDown,
		MonthUp: u.MonthUp, MonthDown: u.MonthDown, UpdatedAt: UTC(u.UpdatedAt),
	}
}

type OptionsView struct {
	ExtraCoverNames  []string `json:"extraCoverNames"`
	DeviceWindowSecs int      `json:"deviceWindowSecs"`
	LogLevel         string   `json:"logLevel"`
}

func OptionsOf(o users.ProxyOptions) OptionsView {
	v := OptionsView{ExtraCoverNames: o.ExtraCoverNames, DeviceWindowSecs: o.DeviceWindowSecs, LogLevel: o.LogLevel}
	if v.ExtraCoverNames == nil {
		v.ExtraCoverNames = []string{}
	}
	if v.LogLevel == "" {
		v.LogLevel = "normal"
	}
	return v
}

// CoverJSON is a cover site's test as the page reads it; word puts the error in the page's words.
func CoverJSON(res ctrl.DisguiseResult, word func(string) string) map[string]any {
	addrs := res.Addrs
	if addrs == nil {
		addrs = []string{}
	}
	return map[string]any{
		"ok": res.OK, "tls13": res.TLS13, "x25519": res.X25519, "h2": res.H2, "certOk": res.CertOK,
		"addrs": addrs, "latencyMs": res.LatencyMS, "error": word(res.Err),
	}
}

type SampleView struct {
	At   time.Time `json:"at"`
	Up   int64     `json:"up"`
	Down int64     `json:"down"`
}

// Traffic answers with the last minutes of a proxy's rate, polled every step.
func Traffic(w http.ResponseWriter, samples []users.Sample, step time.Duration) {
	out := []SampleView{}
	for _, s := range samples {
		out = append(out, SampleView{At: s.At.UTC(), Up: int64(s.Up), Down: int64(s.Down)})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"stepSec": max(1, int(step.Round(time.Second).Seconds())), "samples": out})
}
