package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/relay"
)

type logWriter func([]byte) (int, error)

func (f logWriter) Write(p []byte) (int, error) { return f(p) }

func TestServeDashboardAndRelayWithoutDatabase(t *testing.T) {
	config := relay.Config{Listen: "127.0.0.1:0", StatsListen: "127.0.0.1:0", Key: "relay-fixture", UpstreamKey: "upstream-fixture", UpstreamURL: "http://127.0.0.1:1", MaxBodyBytes: 1 << 20, UploadTimeout: time.Second, ResponseHeaderTimeout: time.Second}
	lines := make(chan string, 2)
	logger := slog.New(slog.NewJSONHandler(logWriter(func(p []byte) (int, error) { lines <- string(p); return len(p), nil }), nil))
	handler, err := relay.NewHandler(config, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, config, handler, logger) }()
	var started struct {
		Listen      string `json:"listen"`
		StatsListen string `json:"stats_listen"`
	}
	select {
	case line := <-lines:
		if err := json.Unmarshal([]byte(line), &started); err != nil {
			t.Fatal(err)
		}
	case err := <-done:
		t.Fatalf("process failed to start: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("server did not start")
	}
	client := &http.Client{Timeout: time.Second}
	for _, tc := range []struct{ url, want string }{{"http://" + started.StatsListen + "/relay", "最近 0 / 2000"}, {"http://" + started.Listen + "/health", `"status":"ok"`}} {
		response, err := client.Get(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != 200 || !strings.Contains(string(body), tc.want) {
			t.Fatalf("unavailable %s: %d %v", tc.url, response.StatusCode, err)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown left a listener running")
	}
	for _, address := range []string{started.Listen, started.StatsListen} {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			t.Errorf("listener survived shutdown: %s", address)
		}
	}
}

func TestServeReleasesRelayPortWhenStatsPortIsOccupied(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserve.Addr().String()
	_ = reserve.Close()
	config := relay.Config{Listen: address, StatsListen: busy.Addr().String()}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := serve(context.Background(), config, &relay.Handler{}, logger); err == nil {
		t.Fatal("occupied stats listener ignored")
	}
	available, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("failed startup leaked relay port: %v", err)
	}
	_ = available.Close()
}
