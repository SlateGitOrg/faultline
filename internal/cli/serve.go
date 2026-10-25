package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/archit2501/faultline/internal/control"
	"github.com/archit2501/faultline/internal/engine"
)

// shutdownTimeout bounds the graceful stop of the control API.
const shutdownTimeout = 5 * time.Second

func newLogger(e *env, level, format string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("%w: --log-level: %w", errUsage, err)
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(e.stderr, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(e.stderr, opts)), nil
	}
	return nil, fmt.Errorf("%w: --log-format must be text or json, got %q", errUsage, format)
}

// isLoopbackListen reports whether a listen address only accepts local
// connections. An empty host (":7070") binds every interface.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func serve(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "serve", "[flags]")
	listen := fs.String("listen", DefaultControl, "control API listen address")
	allow := fs.String("allow-upstream", "", "comma separated CIDRs/IPs proxies may forward to (default: loopback only)")
	maxTTL := fs.Duration("max-ttl", engine.DefaultMaxTTL, "longest lifetime any fault may have; faults without a TTL get this")
	maxProxies := fs.Int("max-proxies", engine.DefaultMaxProxies, "maximum number of proxies")
	maxFaults := fs.Int("max-faults", engine.DefaultMaxFaultsPerProxy, "maximum active faults per proxy")
	seed := fs.Uint64("seed", 0, "seed for fault sampling (0: random); fixes probability and jitter draws")
	addrFile := fs.String("addr-file", "", "write the bound control address to this file once listening")
	level := fs.String("log-level", "info", "log level: debug, info, warn, error")
	format := fs.String("log-format", "text", "log format: text or json")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	log, err := newLogger(e, *level, *format)
	if err != nil {
		return err
	}
	token := e.getenv(EnvToken)
	if token == "" && !isLoopbackListen(*listen) {
		return fmt.Errorf("%w: refusing to expose the control API on %s without authentication; set %s or listen on a loopback address", errUsage, *listen, EnvToken)
	}
	opts := engine.Options{
		MaxTTL: *maxTTL, MaxProxies: *maxProxies, MaxFaultsPerProxy: *maxFaults,
		Seed: *seed, Logger: log,
	}
	if *allow != "" {
		if opts.AllowUpstreams, err = engine.ParseAllowlist(*allow); err != nil {
			return fmt.Errorf("%w: --allow-upstream: %w", errUsage, err)
		}
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *listen)
	if err != nil {
		return err
	}
	eng := engine.New(opts)
	srv := &http.Server{
		Handler:           control.NewHandler(eng, control.HandlerOptions{Token: token, Logger: log}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	addr := ln.Addr().String()
	if *addrFile != "" {
		if err := os.WriteFile(*addrFile, []byte(addr+"\n"), 0o600); err != nil {
			_ = ln.Close()
			_ = eng.Close()
			return fmt.Errorf("write --addr-file: %w", err)
		}
	}
	allowed := make([]string, len(eng.Options().AllowUpstreams))
	for i, p := range eng.Options().AllowUpstreams {
		allowed[i] = p.String()
	}
	log.Info("faultline control API listening", "addr", addr, "auth", token != "",
		"allow_upstream", strings.Join(allowed, ","), "max_ttl", eng.Options().MaxTTL)

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err = <-served:
	case <-ctx.Done():
	}
	// Fail safe: whatever stopped us, no fault outlives the daemon.
	eng.SetKillSwitch(true, "daemon shutting down")
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(sctx)
	closeErr := eng.Close()
	log.Info("faultline stopped")
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return closeErr
}
