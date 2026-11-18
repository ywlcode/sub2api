//go:build integration

package site

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type droppingForwardWriter struct {
	to      net.Conn
	drop    *atomic.Bool
	dropped *atomic.Int64
}

func (w droppingForwardWriter) Write(p []byte) (int, error) {
	if w.drop.Load() {
		w.dropped.Add(int64(len(p)))
		return len(p), nil
	}
	return w.to.Write(p)
}

// This proxy acknowledges TCP while discarding TLS records in both directions
// on its first connection. TCP keepalive cannot detect this application-visible
// blackhole; H2 PING must. Later connections are healthy, without restarting the
// application. Everything uses loopback, with no Docker or firewall changes.
func TestSiteForwardTCPBlackholeRecovery(t *testing.T) {
	var served atomic.Int32
	f := newSitePoolFixture(t, true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		served.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, forwardCompleted)
	})
	close(f.remote.wait)
	primary, err := url.Parse(f.manager.cfg.PrimaryURL)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var blackhole atomic.Bool
	var dropped atomic.Int64
	var accepts atomic.Int32
	var mu sync.Mutex
	closing := false
	var connections []net.Conn
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			first := accepts.Add(1) == 1
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { _ = client.Close() }()
				mu.Lock()
				if closing {
					mu.Unlock()
					return
				}
				connections = append(connections, client)
				mu.Unlock()
				server, err := net.DialTimeout("tcp", primary.Host, 5*time.Second)
				if err != nil {
					return
				}
				defer func() { _ = server.Close() }()
				mu.Lock()
				if closing {
					mu.Unlock()
					return
				}
				connections = append(connections, server)
				mu.Unlock()
				drop := &atomic.Bool{}
				if first {
					drop = &blackhole
				}
				done := make(chan struct{})
				go func() {
					_, _ = io.Copy(droppingForwardWriter{to: server, drop: drop, dropped: &dropped}, client)
					_ = server.Close()
					close(done)
				}()
				_, _ = io.Copy(droppingForwardWriter{to: client, drop: drop, dropped: &dropped}, server)
				_ = client.Close()
				<-done
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		closing = true
		for _, c := range connections {
			_ = c.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	dial := f.manager.forwardTransport.DialContext
	f.manager.forwardTransport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dial(ctx, network, listener.Addr().String())
	}
	client := &http.Client{Transport: f.manager.proxy.Transport, Timeout: 50 * time.Second}
	post := func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, primary.String()+"/v1/responses", forwardTestBody())
		if err != nil {
			return nil, err
		}
		req.GetBody = nil
		return client.Do(req)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	require.NoError(t, f.manager.syncOnce(ctx))
	resp, err := post(ctx)
	requireForwardComplete(t, resp, err)
	blackhole.Store(true)
	var reused atomic.Bool
	stalledCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused.Store(info.Reused) }})
	failed := make(chan error, 1)
	start := time.Now()
	go func() {
		resp, err := post(stalledCtx)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		failed <- err
	}()
	require.Eventually(t, func() bool { return dropped.Load() > 0 }, 3*time.Second, 10*time.Millisecond)
	require.True(t, reused.Load(), "failure must hit the established pooled connection")
	controlStart := time.Now()
	require.NoError(t, f.manager.syncOnce(ctx))
	f.queueEvent(1)
	active, err := f.manager.deliverOnce(ctx)
	require.NoError(t, err)
	require.True(t, active)
	controlElapsed := time.Since(controlStart)
	require.Less(t, controlElapsed, 5*time.Second, "forward blackhole must not block sync or billing")
	require.ErrorContains(t, <-failed, "http2: client connection lost")
	elapsed := time.Since(start)
	require.Less(t, elapsed, 40*time.Second, "must not wait for minute-scale TCP retry exhaustion")
	require.EqualValues(t, 1, served.Load(), "no automatic replay on another connection")
	resp, err = post(ctx)
	requireForwardComplete(t, resp, err)
	require.EqualValues(t, 2, served.Load())
	require.EqualValues(t, 2, accepts.Load())
	require.True(t, f.manager.Ready())
	t.Logf("real TCP/TLS blackhole detected after %s; synchronization and billing completed in %s", elapsed, controlElapsed)
}
