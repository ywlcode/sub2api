package service

import (
	"context"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

type apiKeyHealthWSConn struct {
	events  [][]byte
	writes  int
	readErr error
}

func (c *apiKeyHealthWSConn) WriteJSON(context.Context, any) error {
	c.writes++
	return nil
}
func (c *apiKeyHealthWSConn) ReadMessage(context.Context) ([]byte, error) {
	if len(c.events) == 0 {
		if c.readErr != nil {
			return nil, c.readErr
		}
		return nil, io.EOF
	}
	data := c.events[0]
	c.events = c.events[1:]
	return data, nil
}
func (c *apiKeyHealthWSConn) Ping(context.Context) error { return nil }
func (c *apiKeyHealthWSConn) Close() error               { return nil }
func (c *apiKeyHealthWSConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	data, err := c.ReadMessage(ctx)
	return coderws.MessageText, data, err
}
func (c *apiKeyHealthWSConn) WriteFrame(ctx context.Context, _ coderws.MessageType, data []byte) error {
	return c.WriteJSON(ctx, data)
}

func TestAPIKeyHealthWSLeaseRetriesAndCooldown(t *testing.T) {
	limiter, account, cache, _ := newAPIKeyHealthFixture(t)
	conn := &apiKeyHealthWSConn{events: [][]byte{
		[]byte(`{"type":"error","error":{"message":"failed"}}`),
		[]byte(`{"type":"response.failed","response":{"status":"failed"}}`),
		[]byte(`{"type":"response.completed","response":{"status":"completed"}}`),
	}}
	lease := &openAIWSConnLease{accountID: account.ID, conn: newOpenAIWSConn("health-test", account.ID, conn, nil),
		health: newAPIKeyHealthWSTracker(context.Background(), limiter, account)}
	for i := 0; i < 3; i++ {
		require.NoError(t, lease.WriteJSONWithContextTimeout(context.Background(), map[string]any{"type": "response.create"}, time.Second))
		_, err := lease.ReadMessageWithContextTimeout(context.Background(), time.Second)
		require.NoError(t, err)
	}
	require.Equal(t, []apiKeyHealthSample{{false, 200}, {false, 200}, {true, 200}}, cache.samples)
	// Reading an idle close after a terminal must not produce another sample.
	_, err := lease.ReadMessageWithContextTimeout(context.Background(), time.Second)
	require.ErrorIs(t, err, io.EOF)
	require.Len(t, cache.samples, 3)
	cache.state.UntilUnix = time.Now().Add(15 * time.Minute).Unix()
	err = lease.WriteJSONWithContextTimeout(context.Background(), map[string]any{"type": "response.create"}, time.Second)
	var blocked *UpstreamFailoverError
	require.ErrorAs(t, err, &blocked)
	require.Equal(t, 3, conn.writes)
}

func TestAPIKeyHealthWSFrameTimeoutAndClientCancellation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		cancel bool
	}{
		{"read timeout", os.ErrDeadlineExceeded, false},
		{"first output timeout", &openAIWSPassthroughFirstOutputTimeoutError{}, false},
		{"upstream close", io.EOF, false},
		{"client cancellation", context.Canceled, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			limiter, account, cache, _ := newAPIKeyHealthFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			upstream := &apiKeyHealthWSConn{readErr: tt.err}
			conn := &apiKeyHealthWSFrameConn{FrameConn: upstream, health: newAPIKeyHealthWSTracker(ctx, limiter, account)}
			// Session control frames do not open request samples.
			require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"session.update"}`)))
			require.Zero(t, cache.beginCalls)
			require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create"}`)))
			if tt.cancel {
				cancel()
			}
			_, _, err := conn.ReadFrame(ctx)
			require.ErrorIs(t, err, tt.err)
			_, _, _ = conn.ReadFrame(ctx)
			if tt.cancel {
				require.Empty(t, cache.samples)
			} else {
				require.Equal(t, []apiKeyHealthSample{{false, 0}}, cache.samples)
			}
		})
	}
}

type apiKeyHealthWSDialer struct {
	calls  int
	status int
	err    error
}

func (d *apiKeyHealthWSDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	d.calls++
	if d.err != nil {
		return nil, d.status, nil, d.err
	}
	return &apiKeyHealthWSConn{}, http.StatusSwitchingProtocols, nil, nil
}

func TestAPIKeyHealthWSForegroundDialFailuresCountAndBlockFurtherDials(t *testing.T) {
	limiter, account, cache, _ := newAPIKeyHealthFixture(t)
	cache.tripAt = 1
	account.Concurrency = 1
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	pool := newOpenAIWSConnPool(cfg)
	t.Cleanup(pool.Close)
	dialer := &apiKeyHealthWSDialer{status: 502, err: io.EOF}
	pool.setClientDialerForTest(dialer)
	req := openAIWSAcquireRequest{Account: account, WSURL: "wss://upstream.example/v1/responses"}
	_, err := acquireAPIKeyHealthWS(context.Background(), pool, limiter, req)
	require.Error(t, err)
	require.Equal(t, []apiKeyHealthSample{{false, 502}}, cache.samples)
	_, err = acquireAPIKeyHealthWS(context.Background(), pool, limiter, req)
	var blocked *UpstreamFailoverError
	require.ErrorAs(t, err, &blocked)
	require.Equal(t, 1, dialer.calls)
}

func TestAPIKeyHealthWSSuccessfulHandshakeIsNotASuccessSample(t *testing.T) {
	limiter, account, cache, _ := newAPIKeyHealthFixture(t)
	account.Concurrency = 1
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	pool := newOpenAIWSConnPool(cfg)
	t.Cleanup(pool.Close)
	pool.setClientDialerForTest(&apiKeyHealthWSDialer{})
	lease, err := acquireAPIKeyHealthWS(context.Background(), pool, limiter, openAIWSAcquireRequest{
		Account: account, WSURL: "wss://upstream.example/v1/responses",
	})
	require.NoError(t, err)
	lease.Release()
	require.Empty(t, cache.samples)
}
