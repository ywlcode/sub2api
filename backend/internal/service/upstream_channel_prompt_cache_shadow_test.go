package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type shadowChannelPolicyTestRepository struct {
	ChannelRepository
	mu       sync.Mutex
	accounts []UpstreamChannelPromptCacheShadowAccount
	err      error
	calls    int
	load     func(context.Context, int) ([]UpstreamChannelPromptCacheShadowAccount, error)
}

func (r *shadowChannelPolicyTestRepository) ListPromptCacheShadowAccounts(ctx context.Context) ([]UpstreamChannelPromptCacheShadowAccount, error) {
	r.mu.Lock()
	r.calls++
	calls, load := r.calls, r.load
	accounts := append([]UpstreamChannelPromptCacheShadowAccount(nil), r.accounts...)
	err := r.err
	r.mu.Unlock()
	if load != nil {
		return load(ctx, calls)
	}
	return accounts, err
}

func (r *shadowChannelPolicyTestRepository) set(accounts []UpstreamChannelPromptCacheShadowAccount, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts, r.err = accounts, err
}

func TestPromptCacheShadowChannelSwitchAndFailover(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	repo, ok := svc.channelService.repo.(*shadowChannelPolicyTestRepository)
	require.True(t, ok)
	ctx, first := svc.beginPromptCacheShadow(context.Background(), c, account)
	require.NotNil(t, first)
	capturePromptCacheShadow(ctx, shadowTestBody(), "http")
	first.finish(shadowTestResult(), nil)
	require.Equal(t, int64(11), (*events)[0]["upstream_channel_id"])

	// Moving the retry account into a disabled channel (or removing its
	// binding) must not inherit the previous account's observer context.
	repo.set(nil, nil)
	svc.channelService.InvalidatePromptCacheShadowPolicy()
	masked, disabled := svc.beginPromptCacheShadow(ctx, c, account)
	require.Nil(t, disabled)
	require.Nil(t, masked.Value(promptCacheShadowContextKey{}))
	capturePromptCacheShadow(masked, shadowTestBody(), "http")
	require.Len(t, *events, 1)

	// Enabling a new channel applies to new attempts without restarting.
	repo.set([]UpstreamChannelPromptCacheShadowAccount{{AccountID: account.ID, ChannelID: 22}}, nil)
	svc.channelService.InvalidatePromptCacheShadowPolicy()
	ctx, enabled := svc.beginPromptCacheShadow(masked, c, account)
	require.NotNil(t, enabled)
	capturePromptCacheShadow(ctx, shadowTestBody(), "http")
	r := shadowTestResult()
	before := *r
	enabled.finish(r, nil)
	require.Equal(t, before, *r, "channel observation never changes usage or billing")
	require.Len(t, *events, 2)
	require.Equal(t, int64(22), (*events)[1]["upstream_channel_id"])
	require.Equal(t, "hit", (*events)[1]["decision"], "disabling does not erase still-valid history")

	svc.channelService = nil
	_, disabled = svc.beginPromptCacheShadow(context.Background(), c, account)
	require.Nil(t, disabled, "no ownership information must disable diagnostics only")
}

func TestPromptCacheShadowChannelSwitchDuringWebSocket(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	repo, ok := svc.channelService.repo.(*shadowChannelPolicyTestRepository)
	require.True(t, ok)
	requests, completed := 0, 0
	hooks, cleanup := svc.wrapPromptCacheShadowHooks(c, account, &OpenAIWSIngressHooks{
		ObserveUpstreamRequest: func(int, []byte, ...http.Header) { requests++ },
		AfterTurn:              func(int, *OpenAIForwardResult, error) { completed++ },
	})
	defer cleanup()
	hooks.ObserveUpstreamRequest(1, shadowTestBody())
	hooks.AfterTurn(1, shadowTestResult(), nil)
	require.Len(t, *events, 1)
	repo.set(nil, nil)
	svc.channelService.InvalidatePromptCacheShadowPolicy()
	hooks.ObserveUpstreamRequest(2, shadowTestBody())
	hooks.AfterTurn(2, shadowTestResult(), nil)
	require.Len(t, *events, 1, "existing socket must obey a newly disabled channel")
	repo.set([]UpstreamChannelPromptCacheShadowAccount{{AccountID: account.ID, ChannelID: 22}}, nil)
	svc.channelService.InvalidatePromptCacheShadowPolicy()
	hooks.ObserveUpstreamRequest(3, shadowTestBody())
	hooks.AfterTurn(3, shadowTestResult(), nil)
	require.Len(t, *events, 2)
	require.Equal(t, int64(22), (*events)[1]["upstream_channel_id"])
	require.Equal(t, 3, requests)
	require.Equal(t, 3, completed, "business callbacks must not depend on observation")
}

func TestPromptCacheShadowChannelSnapshotSingleflightAndExpiry(t *testing.T) {
	repo := &shadowChannelPolicyTestRepository{accounts: []UpstreamChannelPromptCacheShadowAccount{{AccountID: 3, ChannelID: 11}}}
	svc := NewChannelService(repo, nil, nil, nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.Equal(t, int64(11), svc.PromptCacheShadowChannelForAccount(context.Background(), 3))
			require.Zero(t, svc.PromptCacheShadowChannelForAccount(context.Background(), 4))
		}()
	}
	wg.Wait()
	require.Equal(t, 1, repo.calls, "one snapshot query, not one query per request/account")
	repo.set(nil, nil)
	p := &svc.promptCacheShadowPolicy
	stale := *p.snapshot.Load()
	stale.loadedAt = time.Now().Add(-2 * upstreamPromptCacheShadowPolicyTTL)
	p.snapshot.Store(&stale)
	require.Zero(t, svc.PromptCacheShadowChannelForAccount(context.Background(), 3))
	require.Equal(t, 2, repo.calls, "missed notifications and automatic binding changes converge by TTL")
}

func TestPromptCacheShadowChannelCanceledLeaderDoesNotPoisonSnapshot(t *testing.T) {
	type policyContextKey struct{}
	repo := &shadowChannelPolicyTestRepository{load: func(ctx context.Context, _ int) ([]UpstreamChannelPromptCacheShadowAccount, error) {
		require.Equal(t, "preserved", ctx.Value(policyContextKey{}))
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "shared refresh still requires a bounded query")
		require.LessOrEqual(t, time.Until(deadline), 250*time.Millisecond)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return []UpstreamChannelPromptCacheShadowAccount{{AccountID: 3, ChannelID: 11}}, nil
	}}
	svc := NewChannelService(repo, nil, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), policyContextKey{}, "preserved"))
	cancel()
	require.Equal(t, int64(11), svc.PromptCacheShadowChannelForAccount(ctx, 3))
	require.Equal(t, int64(11), svc.PromptCacheShadowChannelForAccount(context.Background(), 3))
	require.Equal(t, 1, repo.calls, "canceled leader must not cache an empty global policy")
}

func TestPromptCacheShadowChannelPolicyFailureDoesNotObserveOrFlood(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	repo, ok := svc.channelService.repo.(*shadowChannelPolicyTestRepository)
	require.True(t, ok)
	repo.set(nil, errors.New("test database offline"))
	for i := 0; i < 10; i++ {
		ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
		require.Nil(t, a)
		capturePromptCacheShadow(ctx, shadowTestBody(), "http")
	}
	require.Equal(t, 1, repo.calls)
	require.Empty(t, *events)
	repo.set([]UpstreamChannelPromptCacheShadowAccount{{AccountID: account.ID, ChannelID: 11}}, nil)
	svc.channelService.InvalidatePromptCacheShadowPolicy()
	_, a := svc.beginPromptCacheShadow(context.Background(), c, account)
	require.NotNil(t, a)
	a.finish(nil, context.Canceled)
}

func TestPromptCacheShadowChannelInvalidationCannotPublishOldEnable(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	repo := &shadowChannelPolicyTestRepository{load: func(ctx context.Context, call int) ([]UpstreamChannelPromptCacheShadowAccount, error) {
		if call == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []UpstreamChannelPromptCacheShadowAccount{{AccountID: 3, ChannelID: 11}}, nil
		}
		return nil, nil
	}}
	svc := NewChannelService(repo, nil, nil, nil, nil)
	result := make(chan int64, 1)
	go func() { result <- svc.PromptCacheShadowChannelForAccount(context.Background(), 3) }()
	<-started
	svc.InvalidatePromptCacheShadowPolicy()
	require.Zero(t, svc.PromptCacheShadowChannelForAccount(context.Background(), 3))
	close(release)
	require.Zero(t, <-result, "in-flight pre-edit query must not resurrect enabled policy")
	require.Zero(t, svc.PromptCacheShadowChannelForAccount(context.Background(), 3))
}

type shadowPolicyTestPubSub struct{ callbacks []func() }

func (p *shadowPolicyTestPubSub) NotifyUpdate(context.Context) error {
	for _, f := range p.callbacks {
		f()
	}
	return nil
}

func (p *shadowPolicyTestPubSub) SubscribeUpdates(_ context.Context, f func()) {
	p.callbacks = append(p.callbacks, f)
}

func TestPromptCacheShadowChannelInvalidationAcrossInstances(t *testing.T) {
	repo := &shadowChannelPolicyTestRepository{accounts: []UpstreamChannelPromptCacheShadowAccount{{AccountID: 3, ChannelID: 11}}}
	pubsub := &shadowPolicyTestPubSub{}
	a := NewChannelService(repo, nil, nil, nil, pubsub)
	b := NewChannelService(repo, nil, nil, nil, pubsub)
	require.Equal(t, int64(11), a.PromptCacheShadowChannelForAccount(context.Background(), 3))
	require.Equal(t, int64(11), b.PromptCacheShadowChannelForAccount(context.Background(), 3))
	repo.set(nil, nil)
	a.InvalidatePromptCacheShadowPolicy()
	require.Zero(t, a.PromptCacheShadowChannelForAccount(context.Background(), 3))
	require.Zero(t, b.PromptCacheShadowChannelForAccount(context.Background(), 3))
}
