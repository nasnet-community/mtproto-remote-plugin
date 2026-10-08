// Command mtproto-remote is the MikroTik plugin: it installs MTProto agents on VPSes
// over SSH and carries their proxies' upstream connections through the router.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/api"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/manager"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/store"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/ui"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops"
	"github.com/nasnet-community/mtproto-remote-plugin/tgnet"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

// capFor is the most servers a router carries; 32-bit arm boxes have 256 MB.
func capFor(goarch string) int {
	if goarch == "arm" {
		return 1
	}
	return store.DefaultMaxServers
}

// limitsFor is MAX_SERVERS, never above the architecture's cap, and a default server's streams for each.
func limitsFor(env, goarch string) (store.Limits, error) {
	n := capFor(goarch)
	if env != "" {
		v, err := strconv.Atoi(env)
		if err != nil || v < 1 {
			return store.Limits{}, fmt.Errorf("MAX_SERVERS %q must be a positive number", env)
		}
		n = min(v, n)
	}
	return store.Limits{MaxServers: n, StreamBudget: n * store.DefaultMaxStreams}, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("mtproto-remote stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	host, port, dataDir := env("HOST", "0.0.0.0"), env("PORT", "8080"), env("DATA_DIR", "/data")
	// unset refreshes from Telegram's own list; "off" keeps only the built-in one
	cidrURL := tgnet.ListURL(env("CIDR_URL", ""))
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("PORT %q is not a port", port)
	}
	base, err := ui.BasePath(os.Getenv("BASE_PATH"))
	if err != nil {
		return err
	}
	lim, err := limitsFor(env("MAX_SERVERS", ""), runtime.GOARCH)
	if err != nil {
		return err
	}
	st, err := store.Open(dataDir, lim)
	if err != nil {
		return err
	}
	allow, err := tgnet.NewRefresher()
	if err != nil {
		return err
	}
	if tgnet.PinnedIsPlaceholder() {
		log.Warn("the datacentre address list is still the placeholder: nothing will be relayed")
	}

	// plain dials: RouterOS routes by our source address (agents domestic, the rest foreign)
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	fetch := &http.Client{Timeout: 30 * time.Second}
	download := &http.Client{Timeout: 10 * time.Minute}
	allow.URL = func() string { return cidrURL }
	allow.Client = func() *http.Client { return fetch }

	mgr := manager.New(manager.Config{
		Store: st, Allow: allow, Dial: dialer.DialContext, DialDC: dialer.DialContext,
		StatsEvery: time.Minute, // each usage poll with traffic is a write to the router's flash
		Log:        log,
	})
	allow.Changed = mgr.PrefixesChanged
	bins := relayops.NewBinaries(filepath.Join(dataDir, "bin"), func() *http.Client { return download })
	// dev only: until an agent release is pinned, install a local build from here
	if local := os.Getenv("AGENT_DIR"); local != "" {
		bins.LocalAgents = local
		log.Warn("installing agent builds from a local folder while no agent release is pinned", "dir", local)
	}
	svc := relayops.NewService(relayops.ServiceConfig{
		Store: st.Records(), Links: mgr.Links(), Admin: mgr.Admin(), SSH: relayops.NewSSH(), Log: log,
		Bins: bins,
	})
	page, err := ui.New(base)
	if err != nil {
		return err
	}
	handler := api.New(api.Config{
		Svc: svc, Manager: mgr, Store: st, Allow: allow, AllowURL: cidrURL,
		Version: version, BasePath: base, Index: page, Log: log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := mgr.Start(ctx); err != nil {
		return err
	}
	defer mgr.Stop()
	go allow.Run(ctx)

	srv := &http.Server{
		Addr: net.JoinHostPort(host, port), Handler: handler,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		BaseContext: func(net.Listener) context.Context { return ctx }, // ends job streams when we stop
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("mtproto-remote listening", "version", version, "addr", srv.Addr, "base", base, "max_servers", lim.MaxServers, "stream_budget", lim.StreamBudget, "data", dataDir)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}
