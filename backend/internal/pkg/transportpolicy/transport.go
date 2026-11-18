// Package transportpolicy supplies connection liveness settings. Callers still
// own independent pools, protocol selection, request deadlines and retry policy.
package transportpolicy

import (
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/http2"
)

const (
	KeepAliveIdle        = 15 * time.Second
	KeepAliveInterval    = 5 * time.Second
	KeepAliveCount       = 3
	TCPUserTimeout       = KeepAliveIdle + KeepAliveInterval*KeepAliveCount
	HTTP2ReadIdleTimeout = 15 * time.Second
	HTTP2PingTimeout     = 15 * time.Second
)

// NewDialer bounds both connection establishment and silently dead TCP sockets.
// It does not impose a total lifetime on a healthy model stream.
func NewDialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAliveConfig: net.KeepAliveConfig{
		Enable: true, Idle: KeepAliveIdle, Interval: KeepAliveInterval, Count: KeepAliveCount,
	}, Control: dialControl}
}

// ConfigureHTTP2 probes connection activity, not token activity. A model may
// think without producing content while its peer continues acknowledging PINGs.
// Only net/http retires a failed connection; never close a whole healthy pool in
// response to a single stream reset. role must be a static, non-secret label.
func ConfigureHTTP2(t *http.Transport, idle, ping time.Duration, role string) (*http2.Transport, error) {
	h2, err := http2.ConfigureTransports(t)
	if err != nil {
		return nil, err
	}
	h2.ReadIdleTimeout, h2.PingTimeout = idle, ping
	h2.CountError = func(kind string) {
		// The H2 library can report a canceled health check during intentional
		// pool closure too. Request failures are logged at their read boundary;
		// connection diagnostics alone must not raise a user-failure alarm.
		slog.Debug("http2_connection_error", "role", role, "kind", kind)
	}
	return h2, nil
}
