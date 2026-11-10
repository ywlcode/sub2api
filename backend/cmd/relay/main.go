// The relay is a separate entrypoint: no gateway initialization, database,
// Redis, scheduler, billing, UI or background workers are linked into it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/relay"
)

var Version = "dev"
var Commit = "unknown"
var Date = "unknown"

func main() {
	version := flag.Bool("version", false, "print relay version")
	health := flag.Bool("healthcheck", false, "check the local relay health endpoint")
	flag.Parse()
	if *version {
		fmt.Printf("Sub2API Relay %s (commit: %s, built: %s)\n", Version, Commit, Date)
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if *health {
		address := os.Getenv("RELAY_LISTEN")
		if address == "" {
			address = "0.0.0.0:8080"
		}
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			os.Exit(1)
		}
		numericPort, err := strconv.Atoi(port)
		if err != nil || numericPort < 1 || numericPort > 65535 {
			os.Exit(1)
		}
		client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
		res, err := client.Get("http://127.0.0.1:" + strconv.Itoa(numericPort) + "/health") //nolint:gosec // G704: destination is loopback with a validated numeric port, from operator configuration only.
		if err != nil {
			os.Exit(1)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	config, err := relay.LoadConfig()
	if err != nil {
		logger.Error("relay_config_error", "error", err)
		os.Exit(1)
	}
	handler, err := relay.NewHandler(config, logger)
	if err != nil {
		logger.Error("relay_start_error", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, config, handler, logger); err != nil {
		logger.Error("relay_server_error", "error", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, config relay.Config, handler *relay.Handler, logger *slog.Logger) error {
	server := &http.Server{Addr: config.Listen, Handler: handler, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute}
	stats := &http.Server{Addr: config.StatsListen, Handler: handler.StatsHandler(), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen relay: %w", err)
	}
	defer func() { _ = listener.Close() }()
	statsListener, err := net.Listen("tcp", stats.Addr)
	if err != nil {
		return fmt.Errorf("listen relay stats: %w", err)
	}
	defer func() { _ = statsListener.Close() }()
	defer func() { _ = server.Close(); _ = stats.Close() }()
	done := make(chan error, 2)
	go func() { done <- server.Serve(listener) }()
	go func() { done <- stats.Serve(statsListener) }()
	logger.Info("relay_started", "version", Version, "commit", Commit, "listen", listener.Addr().String(), "stats_listen", statsListener.Addr().String(), "timing_window", relay.TimingWindowSize)
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = stats.Shutdown(shutdown)
		return server.Shutdown(shutdown)
	}
}
