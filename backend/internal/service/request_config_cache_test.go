package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type requestConfigWaitingContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *requestConfigWaitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestRequestConfigCacheCanceledLeaderDoesNotFailFollower(t *testing.T) {
	var cache requestConfigCache[int, string]
	type traceKey struct{}
	leader, cancel := context.WithCancel(context.WithValue(context.Background(), traceKey{}, "trace"))
	defer cancel()
	started, release := make(chan context.Context, 1), make(chan struct{})
	var calls atomic.Int32
	load := func(ctx context.Context) (string, int, error) {
		calls.Add(1)
		started <- ctx
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-release:
			return "fresh", 5, nil
		}
	}
	leaderDone := make(chan error, 1)
	go func() { _, err := cache.get(leader, 1, load); leaderDone <- err }()
	loadCtx := <-started
	follower := &requestConfigWaitingContext{Context: context.Background(), waiting: make(chan struct{})}
	followerDone := make(chan error, 1)
	go func() {
		value, err := cache.get(follower, 1, load)
		if err == nil && value != "fresh" {
			err = errors.New("incorrect configuration")
		}
		followerDone <- err
	}()
	<-follower.waiting // DoChan has joined the existing load before Done is read.
	cancel()
	require.ErrorIs(t, <-leaderDone, context.Canceled)
	require.NoError(t, loadCtx.Err())
	require.Equal(t, "trace", loadCtx.Value(traceKey{}))
	deadline, bounded := loadCtx.Deadline()
	require.True(t, bounded)
	require.LessOrEqual(t, time.Until(deadline), requestConfigLoadTimeout)
	close(release)
	require.NoError(t, <-followerDone)
	require.Equal(t, int32(1), calls.Load())
	value, err := cache.get(context.Background(), 1, load)
	require.NoError(t, err)
	require.Equal(t, "fresh", value)
	require.Equal(t, int32(1), calls.Load())
}

func TestRequestConfigCacheSharedLoadTimeoutIsNotCached(t *testing.T) {
	var cache requestConfigCache[int, string]
	_, err := cache.get(context.Background(), 1, func(ctx context.Context) (string, int, error) {
		<-ctx.Done()
		return "", 0, ctx.Err()
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	value, err := cache.get(context.Background(), 1, func(context.Context) (string, int, error) { return "recovered", 9, nil })
	require.NoError(t, err)
	require.Equal(t, "recovered", value)
}

func TestRequestConfigCacheCoalescesInvalidationAndCancellation(t *testing.T) {
	var cache requestConfigCache[int, string]
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	load := func(ctx context.Context) (string, int, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return "old", 3, nil
		}
		return "new", 3, nil
	}
	done := make(chan string, 1)
	go func() {
		value, err := cache.get(context.Background(), 1, load)
		if err != nil {
			done <- err.Error()
		} else {
			done <- value
		}
	}()
	<-started
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := cache.get(canceled, 1, load)
	require.ErrorIs(t, err, context.Canceled)
	cache.invalidate(nil)
	value, err := cache.get(context.Background(), 1, load)
	require.NoError(t, err)
	require.Equal(t, "new", value)
	close(release)
	require.Equal(t, "new", <-done, "a pre-invalidation loader must neither return nor repopulate old configuration")
	require.Equal(t, int32(2), calls.Load())
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := cache.get(context.Background(), 1, load)
			if err != nil || value != "new" {
				t.Errorf("value=%q err=%v", value, err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(2), calls.Load())
}

func TestRequestConfigCacheExpiryErrorsEmptyAndBounds(t *testing.T) {
	var cache requestConfigCache[int, []string]
	calls := 0
	load := func(context.Context) ([]string, int, error) { calls++; return nil, 64, nil }
	for range 2 {
		value, err := cache.get(context.Background(), 1, load)
		require.NoError(t, err)
		require.Nil(t, value)
	}
	require.Equal(t, 1, calls, "empty successful configurations must be cached")
	entry := cache.entries[1]
	entry.expires = time.Now().Add(-time.Second)
	cache.entries[1] = entry
	_, err := cache.get(context.Background(), 1, load)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	for range 2 {
		_, err = cache.get(context.Background(), 2, func(context.Context) ([]string, int, error) { calls++; return nil, 0, errors.New("offline") })
		require.Error(t, err)
	}
	require.Equal(t, 4, calls)
	for i := 3; i < 2100; i++ {
		_, err = cache.get(context.Background(), i, load)
		require.NoError(t, err)
	}
	require.LessOrEqual(t, len(cache.entries), 2048)
	var big requestConfigCache[int, string]
	for i := 0; i < 6; i++ {
		_, err = big.get(context.Background(), i, func(context.Context) (string, int, error) { return "large", 8 << 20, nil })
		require.NoError(t, err)
	}
	require.LessOrEqual(t, big.bytes, 16<<20)
	require.Len(t, big.entries, 2)
}

func TestRequestConfigCacheConcurrentColdLoad(t *testing.T) {
	var cache requestConfigCache[int, string]
	var calls atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cache.get(context.Background(), 1, func(context.Context) (string, int, error) {
				calls.Add(1)
				time.Sleep(10 * time.Millisecond)
				return "one", 3, nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), calls.Load())
}

func TestRequestConfigCacheCanceledFollowerDoesNotWaitForLeader(t *testing.T) {
	var cache requestConfigCache[int, string]
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	load := func(context.Context) (string, int, error) { close(started); <-release; return "ok", 2, nil }
	go func() { defer close(done); _, _ = cache.get(context.Background(), 1, load) }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	follower := make(chan error, 1)
	go func() { _, err := cache.get(ctx, 1, load); follower <- err }()
	cancel()
	select {
	case err := <-follower:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("follower waited for blocked leader")
	}
	close(release)
	<-done
}

func TestRequestConfigTargetedInvalidationDoesNotRestartOtherUsers(t *testing.T) {
	var cache requestConfigCache[int, string]
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	calls := 0
	go func() {
		_, err := cache.get(context.Background(), 1, func(context.Context) (string, int, error) {
			calls++
			close(started)
			<-release
			return "user one", 8, nil
		})
		done <- err
	}()
	<-started
	for range 10 {
		cache.invalidate(func(id int) bool { return id == 2 })
	}
	close(release)
	require.NoError(t, <-done)
	require.Equal(t, 1, calls)
	for i := 2; i < 4200; i++ {
		_, err := cache.get(context.Background(), i, func(context.Context) (string, int, error) { return "x", 1, nil })
		require.NoError(t, err)
	}
	require.LessOrEqual(t, len(cache.versions), 4096, "invalidation metadata must also be bounded")
}
