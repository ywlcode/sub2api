package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestSchedulerConfigNotificationUsesAuthSubscription(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscriber := NewAPIKeyCache(client)
	received := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- subscriber.SubscribeAuthCacheInvalidation(ctx, func(value string) { received <- value })
	}()
	require.Eventually(t, func() bool { return server.PubSubNumSub(authCacheInvalidateChannel)[authCacheInvalidateChannel] == 1 }, time.Second, time.Millisecond)
	message := service.RequestConfigInvalidationPrefix + "catalog:0"
	publisher, ok := NewSchedulerCache(client).(*schedulerCache)
	require.True(t, ok)
	require.NoError(t, publisher.NotifyRequestConfigInvalidation(ctx, message))
	select {
	case got := <-received:
		require.Equal(t, message, got)
	case <-time.After(time.Second):
		t.Fatal("missing catalog notification")
	}
	cancel()
	<-done
}
