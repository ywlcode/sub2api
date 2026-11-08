package repository

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyCacheSubscriber_BlocksUntilContextCancellation(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer func() { _ = client.Close() }()
	cache := NewAPIKeyCache(client)
	ctx, cancel := context.WithCancel(context.Background())
	received := make(chan string, 1)
	returned := make(chan error, 1)
	go func() {
		returned <- cache.SubscribeAuthCacheInvalidation(ctx, func(value string) { received <- value })
	}()

	var value string
	require.Eventually(t, func() bool {
		require.NoError(t, client.Publish(context.Background(), authCacheInvalidateChannel, "hash").Err())
		select {
		case value = <-received:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, "hash", value)
	select {
	case err := <-returned:
		t.Fatalf("subscriber returned while connection was active: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-returned:
		require.True(t, errors.Is(err, context.Canceled))
	case <-time.After(time.Second):
		t.Fatal("subscriber did not stop after context cancellation")
	}
}

// Reconnect is owned by APIKeyService, which disables and invalidates caches
// before retrying. The repository must surface even a brief socket loss.
func TestAPIKeyCacheSubscriberReportsDisconnect(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = client.Close() })
	cache := &apiKeyCache{rdb: client}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready, done := make(chan struct{}, 1), make(chan error, 1)
	go func() {
		done <- cache.subscribeAuthCacheInvalidation(ctx, func(string) {}, func() { ready <- struct{}{} })
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("subscription did not start")
	}
	server.Close()
	require.NoError(t, server.Restart())
	select {
	case err := <-done:
		require.Error(t, err, "a lost notification interval must be reported even if Redis reconnects")
	case <-ctx.Done():
		t.Fatal("subscription hid the disconnect")
	}
}

type authSubscriberDroppingConn struct {
	net.Conn
	drop *atomic.Bool
}

func (c *authSubscriberDroppingConn) Read(p []byte) (int, error) {
	for {
		n, err := c.Conn.Read(p)
		if !c.drop.Load() || err != nil {
			return n, err
		}
		// Simulate a one-way black hole; socket and command connections stay up.
	}
}

func TestAPIKeyCacheSubscriberProbesIdleSocketAndDetectsBlackhole(t *testing.T) {
	server := miniredis.RunT(t)
	var drop atomic.Bool
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &authSubscriberDroppingConn{Conn: conn, drop: &drop}, nil
	}})
	t.Cleanup(func() { _ = client.Close() })
	cache := &apiKeyCache{rdb: client}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	ready, done := make(chan struct{}, 1), make(chan error, 1)
	go func() {
		done <- cache.subscribeAuthCacheInvalidation(ctx, func(string) {}, func() { ready <- struct{}{} })
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("subscription did not start")
	}
	// A healthy but idle connection must survive a full probe cycle.
	select {
	case err := <-done:
		t.Fatalf("idle subscriber returned: %v", err)
	case <-time.After(authCacheSubscriberIdle + authCacheSubscriberProbe):
	}
	drop.Store(true)
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(authCacheSubscriberIdle + authCacheSubscriberProbe + time.Second):
		t.Fatal("silent partition left the subscriber connected indefinitely")
	}
}
