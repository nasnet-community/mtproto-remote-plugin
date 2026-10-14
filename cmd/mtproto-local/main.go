// Command mtproto-local is Local MTProto, the MikroTik plugin that runs a Telegram
// MTProto proxy (telemt) on the router itself: people reach it through the
// router's domestic link, and it reaches Telegram through the foreign one.
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
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/agent/disguise"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/local/api"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/local/page"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/local/proxy"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/local/router"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/local/store"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/ui"
	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/users"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

// connsFor is the default connection limit: 32-bit ARM routers have the least memory.
func connsFor(goarch string) int {
	if goarch == "arm" {
		return 300
	}
	return 1000
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := env(key, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s %q is not a number", key, v)
	}
	return n, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("mtproto-local stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	host, port, dataDir := env("HOST", "0.0.0.0"), env("PORT", "8080"), env("DATA_DIR", "/data")
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("PORT %q is not a port", port)
	}
	base, err := ui.BasePath(os.Getenv("BASE_PATH"))
	if err != nil {
		return err
	}
	conns, err := envInt("MAX_CONNS", connsFor(runtime.GOARCH))
	if err != nil {
		return err
	}
	uid, err := envInt("TELEMT_UID", 65534)
	if err != nil {
		return err
	}
	gid, err := envInt("TELEMT_GID", uid)
	if err != nil {
		return err
	}
	st, err := store.Open(dataDir, time.Now())
	if err != nil {
		return err
	}
	hist, err := users.OpenHistory(dataDir)
	if err != nil {
		log.Warn("the usage history could not be read; a new one starts", "err", err)
	}
	ch, err := router.New(dataDir)
	if err != nil {
		return err
	}

	// the router sends what leaves from the cover address out the domestic link, so the
	// cover site is fetched and tested the way phones on that side see it
	cover := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if from := env("COVER_ADDR", "192.168.50.14"); from != "off" {
		if ours(from) {
			cover.LocalAddr = &net.TCPAddr{IP: net.ParseIP(from)}
		} else {
			log.Warn("the cover address is not on this container: the cover site is reached from its main address", "addr", from)
		}
	}
	check := disguise.Checker{ProxyOnly: true, Dial: cover.DialContext}
	runner := proxy.New(proxy.Config{
		Store: st, History: hist, Bin: env("TELEMT_BIN", "/usr/local/bin/telemt"), Dir: env("TELEMT_DIR", "/tmp/telemt"),
		Port: 8443, MaxConns: conns, UID: uint32(uid), GID: uint32(gid), Log: log,
		Check:     check.Check,
		CoverDial: func(ctx context.Context, addr string) (net.Conn, error) { return cover.DialContext(ctx, "tcp", addr) },
	})
	page, err := ui.NewFS(page.Files, base)
	if err != nil {
		return err
	}
	handler := api.New(api.Config{
		Runner: runner, Store: st, Router: ch, Version: version, Arch: arch(), Build: readBuild(),
		Conns:    api.Limits{Default: conns, Min: 100, Max: 5000},
		BasePath: base, Index: page, Log: log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := ch.SetHost(st.Get().LinkHost); err != nil {
		log.Warn("could not tell the router the address in links", "err", err)
	}
	if err := runner.Start(ctx); err != nil {
		return err
	}
	defer runner.Stop()
	go watchRouter(ctx, ch, runner, log)

	srv := &http.Server{
		Addr: net.JoinHostPort(host, port), Handler: handler,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("mtproto-local listening", "version", version, "addr", srv.Addr, "base", base, "max_conns", conns, "data", dataDir)
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

// ours says addr is one of this container's own addresses.
func ours(addr string) bool {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range as {
		if n, ok := a.(*net.IPNet); ok && n.IP.String() == addr {
			return true
		}
	}
	return false
}

// arch is the router's architecture as the page names it.
func arch() string {
	if runtime.GOARCH == "arm" {
		return "arm (32-bit)"
	}
	return runtime.GOARCH
}

// readBuild is the image's note on how telemt was built; telemt's own releases have none.
func readBuild() string {
	b, err := os.ReadFile(env("TELEMT_BUILD_NOTE", "/usr/local/share/telemt/BUILD"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// watchRouter follows the router script's reports: a router without a domestic link
// keeps the proxy off, and a moved port goes into the history.
func watchRouter(ctx context.Context, ch router.Channel, r *proxy.Runner, log *slog.Logger) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	var last router.State
	for {
		st, ok, err := ch.Read()
		switch {
		case err != nil:
			log.Warn("the router's state could not be read", "err", err)
		case ok:
			if st.Domestic {
				r.SetBlocked("")
			} else {
				r.SetBlocked("this router has no domestic link")
			}
			if last.Port != 0 && st.Port != 0 && st.Port != last.Port {
				r.Note("port_changed", fmt.Sprintf("from %d to %d", last.Port, st.Port))
			}
			last = st
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
