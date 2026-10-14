package realityserver

import (
	"net"
	"time"

	xr "github.com/xtls/reality"
)

type ServerConfig struct {
	Dest        string // host:port of the borrowed TLS 1.3 site
	ServerNames []string
	PrivateKey  []byte
	ShortIDs    [][8]byte
	MaxTimeDiff time.Duration
}

func (c ServerConfig) build() *xr.Config {
	var d net.Dialer
	cfg := &xr.Config{
		DialContext:            d.DialContext,
		Type:                   "tcp",
		Dest:                   c.Dest,
		PrivateKey:             c.PrivateKey,
		MaxTimeDiff:            c.MaxTimeDiff,
		SessionTicketsDisabled: true,
		ServerNames:            map[string]bool{},
		ShortIds:               map[[8]byte]bool{},
	}
	for _, n := range c.ServerNames {
		cfg.ServerNames[n] = true
	}
	for _, s := range c.ShortIDs {
		cfg.ShortIds[s] = true
	}
	return cfg
}
