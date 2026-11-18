package site

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

type forwardTimeouts struct{ uploadIdle, headers, responseIdle time.Duration }

func (c Config) forwardTimeouts() forwardTimeouts {
	seconds := func(n *int, fallback int) time.Duration {
		if n == nil {
			return time.Duration(fallback) * time.Second
		}
		return time.Duration(*n) * time.Second
	}
	return forwardTimeouts{seconds(c.ForwardUploadIdleTimeoutSeconds, 120), seconds(c.ForwardResponseHeaderTimeoutSeconds, 600), seconds(c.ForwardResponseIdleTimeoutSeconds, 600)}
}

// This wrapper never retries a submitted generation. Connection replacement is
// owned by net/http's H2 liveness check; a stream timeout cancels only that stream.
// A reset/timeout after output remains a truncated stream, never a forged success.
type forwardRoundTripper struct {
	base     http.RoundTripper
	timeouts forwardTimeouts
}

func (t *forwardRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	g := &forwardGuard{ctx: ctx, cancel: cancel, timeouts: t.timeouts, requestID: req.Context().Value(ctxkey.RequestID)}
	g.setPhase("upload_idle", t.timeouts.uploadIdle)
	trace := &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			g.mu.Lock()
			defer g.mu.Unlock()
			if info.Err == nil && !g.response && !g.closed {
				g.uploaded = true
				g.setPhaseLocked("response_headers", t.timeouts.headers)
			}
		},
		GotConn: func(info httptrace.GotConnInfo) {
			g.mu.Lock()
			g.reused = info.Reused
			g.mu.Unlock()
		},
	}
	upstream := req.Clone(httptrace.WithClientTrace(ctx, trace))
	if req.Body != nil && req.Body != http.NoBody {
		upstream.Body = &forwardUploadBody{ReadCloser: req.Body, guard: g}
	}
	resp, err := t.base.RoundTrip(upstream)
	if err != nil {
		err = g.cause(err)
		g.failure(err)
		g.close()
		return resp, err
	}
	g.mu.Lock()
	g.response, g.protocol = true, resp.Proto
	g.stopTimerLocked()
	g.mu.Unlock()
	if err = context.Cause(ctx); err != nil {
		_ = resp.Body.Close()
		g.failure(err)
		g.close()
		return nil, err
	}
	// Bound the WebSocket handshake as well, but preserve duplex I/O and do not
	// apply an HTTP body idle budget to an established interactive session.
	if resp.StatusCode == http.StatusSwitchingProtocols {
		if duplex, ok := resp.Body.(io.ReadWriteCloser); ok {
			resp.Body = &forwardUpgradeBody{ReadWriteCloser: duplex, guard: g}
			return resp, nil
		}
	}
	resp.Body = &forwardResponseBody{ReadCloser: resp.Body, guard: g}
	return resp, nil
}

type forwardTimeoutError struct{ phase string }

func (e *forwardTimeoutError) Error() string { return "primary forwarding timed out during " + e.phase }
func (e *forwardTimeoutError) Timeout() bool { return true }
func (e *forwardTimeoutError) Unwrap() error { return context.DeadlineExceeded }

type forwardGuard struct {
	ctx                                context.Context
	cancel                             context.CancelCauseFunc
	timeouts                           forwardTimeouts
	requestID                          any
	mu                                 sync.Mutex
	timer                              *time.Timer
	deadline                           time.Time
	phase, protocol                    string
	closed, response, uploaded, reused bool
	responseBytes                      int64
	logged                             sync.Once
}

func (g *forwardGuard) setPhase(phase string, timeout time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.setPhaseLocked(phase, timeout)
	}
}

func (g *forwardGuard) setPhaseLocked(phase string, timeout time.Duration) {
	g.stopTimerLocked()
	g.phase = phase
	if timeout <= 0 {
		return
	}
	g.deadline = time.Now().Add(timeout)
	if g.timer == nil {
		g.timer = time.AfterFunc(timeout, g.expire)
	} else {
		g.timer.Reset(timeout)
	}
}

func (g *forwardGuard) stopTimerLocked() {
	g.deadline = time.Time{}
	if g.timer != nil {
		g.timer.Stop()
	}
}

func (g *forwardGuard) expire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.deadline.IsZero() {
		return
	}
	// A callback already runnable before Reset must not cancel a phase that
	// has since progressed. Check the current monotonic deadline under the lock.
	if left := time.Until(g.deadline); left > 0 {
		g.timer.Reset(left)
		return
	}
	g.cancel(&forwardTimeoutError{phase: g.phase})
}

func (g *forwardGuard) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	g.stopTimerLocked()
	g.cancel(context.Canceled)
}

func (g *forwardGuard) cause(err error) error {
	if cause := context.Cause(g.ctx); cause != nil {
		return cause
	}
	return err
}

func (g *forwardGuard) failure(err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return
	}
	g.logged.Do(func() {
		kind := "transport_error"
		var phase *forwardTimeoutError
		var network net.Error
		switch {
		case errors.As(err, &phase):
			kind = "timeout_" + phase.phase
		case strings.Contains(err.Error(), "http2: client connection lost"):
			kind = "http2_connection_lost"
		case strings.Contains(err.Error(), "received from peer"):
			kind = "http2_peer_reset"
		case errors.As(err, &network) && network.Timeout():
			kind = "network_timeout"
		case errors.Is(err, io.ErrUnexpectedEOF):
			kind = "unexpected_eof"
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		slog.Warn("site primary forwarding failed", "request_id", g.requestID, "kind", kind,
			"phase", g.phase, "protocol", g.protocol, "connection_reused", g.reused,
			"request_uploaded", g.uploaded, "response_bytes_read", g.responseBytes)
	})
}

type forwardUploadBody struct {
	io.ReadCloser
	guard *forwardGuard
}

func (b *forwardUploadBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		g := b.guard
		g.mu.Lock()
		if !g.closed && !g.response && !g.uploaded {
			g.setPhaseLocked("upload_idle", g.timeouts.uploadIdle)
		}
		g.mu.Unlock()
	}
	return n, err
}

type forwardResponseBody struct {
	io.ReadCloser
	guard *forwardGuard
}

type forwardUpgradeBody struct {
	io.ReadWriteCloser
	guard *forwardGuard
}

func (b *forwardUpgradeBody) Close() error {
	b.guard.close()
	return b.ReadWriteCloser.Close()
}

func (b *forwardResponseBody) Read(p []byte) (int, error) {
	g := b.guard
	// Count only blocked upstream reads, excluding downstream backpressure.
	g.setPhase("response_idle", g.timeouts.responseIdle)
	n, err := b.ReadCloser.Read(p)
	g.mu.Lock()
	g.responseBytes += int64(n)
	g.stopTimerLocked()
	g.mu.Unlock()
	err = g.cause(err)
	if err != nil {
		g.failure(err)
		g.close()
	}
	return n, err
}
func (b *forwardResponseBody) Close() error {
	b.guard.close()
	return b.ReadCloser.Close()
}
