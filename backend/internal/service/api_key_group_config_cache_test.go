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

type requestConfigAuthCacheStub struct{ APIKeyCache }

func (*requestConfigAuthCacheStub) PublishAuthCacheInvalidation(context.Context, string) error {
	return nil
}

type configGroupRepo struct {
	GroupRepository
	mu     sync.Mutex
	groups map[int64]*Group
	calls  int
}

func (r *configGroupRepo) GetByIDLite(ctx context.Context, id int64) (*Group, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if g := r.groups[id]; g != nil {
		return g, nil
	}
	return nil, ErrGroupNotFound
}

type configPolicyRepo struct {
	UserGroupRateRepository
	policies map[userGroupPolicyKey]userGroupPolicy
	calls    int
	err      error
}

func (r *configPolicyRepo) GetRequestPolicy(ctx context.Context, userID, groupID int64) (*int, []string, error) {
	r.calls++
	p := r.policies[userGroupPolicyKey{userID, groupID}]
	return p.RPM, p.Denied, r.err
}

type configCountingRouter struct{ calls int }

func (r *configCountingRouter) ProbeAPIKeyGroup(context.Context, *Group, string, string) (APIKeyGroupAvailability, error) {
	r.calls++
	return APIKeyGroupAvailability{Available: true}, nil
}

func TestMultiGroupConfigCacheKeepsLiveProbeAndUserIsolation(t *testing.T) {
	groups := &configGroupRepo{groups: map[int64]*Group{1: {ID: 1, Status: StatusActive, ModelRouting: map[string][]int64{"alias": {2}}}, 2: {ID: 2, Status: StatusActive}}}
	limit := 12
	policies := &configPolicyRepo{policies: map[userGroupPolicyKey]userGroupPolicy{{7, 1}: {RPM: &limit, Denied: []string{"hidden"}}}}
	router := &configCountingRouter{}
	svc := &APIKeyService{groupRepo: groups, userGroupRateRepo: policies, cache: &requestConfigAuthCacheStub{}, groupRouter: router}
	svc.authInvalidationConnected.Store(true)
	key := &APIKey{UserID: 7, User: &User{ID: 7}, GroupIDs: []int64{1, 2}}
	ctx := context.Background()
	for range 3 {
		selected, err := svc.SelectRequestGroup(ctx, key, []string{"gpt-5"}, "token", nil, nil, false)
		require.NoError(t, err)
		require.Equal(t, int64(1), *selected.GroupID)
		require.Equal(t, 12, *selected.User.UserGroupRPMOverride)
		selected.Group.ModelRouting["alias"][0] = 999
		selected.User.UserGroupDeniedModels[0] = "corrupt"
		*selected.User.UserGroupRPMOverride = 999
	}
	require.Equal(t, 1, groups.calls)
	require.Equal(t, 1, policies.calls)
	require.Equal(t, 3, router.calls, "cached configuration must not cache availability/admission")
	require.Equal(t, int64(2), groups.groups[1].ModelRouting["alias"][0])
	require.Equal(t, 12, limit)
	// Same group, different user: never reuse the first user's restrictions.
	other := &APIKey{UserID: 8, User: &User{ID: 8}, GroupIDs: []int64{1, 2}}
	selected, err := svc.SelectRequestGroup(ctx, other, []string{"hidden"}, "token", nil, nil, false)
	require.NoError(t, err)
	require.Equal(t, int64(1), *selected.GroupID)
	require.Empty(t, selected.DeniedModelsInGroup())
	policies.policies[userGroupPolicyKey{7, 1}] = userGroupPolicy{Denied: []string{"gpt-5"}}
	svc.applyRequestConfigInvalidation(RequestConfigInvalidationPrefix + "user:7")
	selected, err = svc.SelectRequestGroup(ctx, key, []string{"gpt-5"}, "token", nil, nil, false)
	require.NoError(t, err)
	require.Equal(t, int64(2), *selected.GroupID)
	// Revoked group configuration must become effective on notification.
	groups.groups[2].Status = "disabled"
	svc.applyRequestConfigInvalidation(RequestConfigInvalidationPrefix + "group:2")
	_, err = svc.SelectRequestGroup(ctx, key, []string{"gpt-5"}, "token", nil, nil, false)
	require.Error(t, err)
}

func TestMultiGroupConfigCacheBypassAndPolicyErrors(t *testing.T) {
	groups := &configGroupRepo{groups: map[int64]*Group{1: {ID: 1, Status: StatusActive}}}
	policies := &configPolicyRepo{err: errors.New("offline")}
	svc := &APIKeyService{groupRepo: groups, userGroupRateRepo: policies, cache: &requestConfigAuthCacheStub{}}
	svc.authInvalidationConnected.Store(true)
	for range 2 {
		_, err := svc.requestGroupPolicy(context.Background(), 7, 1)
		require.Error(t, err)
	}
	require.Equal(t, 2, policies.calls)
	policies.err = nil
	_, err := svc.requestGroup(context.Background(), 1)
	require.NoError(t, err)
	svc.authInvalidationConnected.Store(false)
	groups.groups[1].Status = "disabled"
	got, err := svc.requestGroup(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, "disabled", got.Status)
	require.Equal(t, 2, groups.calls, "disconnected notifications must bypass the configuration cache")
}

type configNotificationBus struct {
	requestConfigAuthCacheStub
	mu        sync.Mutex
	listeners []func(string)
}

func (b *configNotificationBus) PublishAuthCacheInvalidation(ctx context.Context, message string) error {
	b.mu.Lock()
	listeners := append([]func(string){}, b.listeners...)
	b.mu.Unlock()
	for _, listener := range listeners {
		listener(message)
	}
	return nil
}
func (b *configNotificationBus) SubscribeAuthCacheInvalidation(ctx context.Context, handler func(string)) error {
	b.mu.Lock()
	b.listeners = append(b.listeners, handler)
	b.mu.Unlock()
	NotifyAuthCacheSubscriptionReady(ctx)
	<-ctx.Done()
	return ctx.Err()
}

func TestRequestConfigInvalidationAcrossReplicas(t *testing.T) {
	bus := &configNotificationBus{}
	groups := &configGroupRepo{groups: map[int64]*Group{1: {ID: 1, Status: StatusActive, Name: "before"}}}
	a, b := &APIKeyService{cache: bus, groupRepo: groups}, &APIKeyService{cache: bus, groupRepo: groups}
	a.StartAuthCacheInvalidationSubscriber(context.Background())
	b.StartAuthCacheInvalidationSubscriber(context.Background())
	defer a.StopAuthCacheInvalidationSubscriber()
	defer b.StopAuthCacheInvalidationSubscriber()
	require.Eventually(t, func() bool { return a.authInvalidationConnected.Load() && b.authInvalidationConnected.Load() }, time.Second, time.Millisecond)
	for _, svc := range []*APIKeyService{a, b} {
		v, err := svc.requestGroup(context.Background(), 1)
		require.NoError(t, err)
		require.Equal(t, "before", v.Name)
	}
	groups.mu.Lock()
	groups.groups[1].Name = "after"
	groups.mu.Unlock()
	a.invalidateRequestConfig("group", 1)
	for _, svc := range []*APIKeyService{a, b} {
		v, err := svc.requestGroup(context.Background(), 1)
		require.NoError(t, err)
		require.Equal(t, "after", v.Name)
	}
	require.Equal(t, 4, groups.calls)
}

// Verify notifications arriving while routes are registered cannot race with
// publication of the gateway callback to an already running auth subscriber.
func TestRequestConfigGatewayAttachment(t *testing.T) {
	svc := &APIKeyService{}
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			svc.applyRequestConfigInvalidation(RequestConfigInvalidationPrefix + "all:0")
		}
	}()
	gateway := &GatewayService{compositeResolver: NewCompositeRouteResolver(nil)}
	svc.SetGroupRouter(gateway)
	stop.Store(true)
	<-done
	require.Same(t, gateway, svc.requestConfigGateway.Load())
}

func (r *configPolicyRepo) DeleteByGroupID(_ context.Context, groupID int64) error {
	for key := range r.policies {
		if key.groupID == groupID {
			delete(r.policies, key)
		}
	}
	return nil
}
func (r *configGroupRepo) DeleteCascade(_ context.Context, id int64) ([]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.groups, id)
	return nil, nil
}

type requestConfigKeyRepo struct{ APIKeyRepository }

func (requestConfigKeyRepo) ListKeysByGroupID(context.Context, int64) ([]string, error) {
	return nil, nil
}

func TestMultiGroupConfigCacheAdminClearAndDelete(t *testing.T) {
	ctx := context.Background()
	groups := &configGroupRepo{groups: map[int64]*Group{1: {ID: 1, Status: StatusActive}}}
	policies := &configPolicyRepo{policies: map[userGroupPolicyKey]userGroupPolicy{{7, 1}: {Denied: []string{"hidden"}}}}
	keys := &APIKeyService{groupRepo: groups, userGroupRateRepo: policies, cache: &requestConfigAuthCacheStub{}}
	keys.authInvalidationConnected.Store(true)
	admin := &adminServiceImpl{groupRepo: groups, userGroupRateRepo: policies, apiKeyRepo: requestConfigKeyRepo{}, authCacheInvalidator: keys}
	_, err := keys.requestGroup(ctx, 1)
	require.NoError(t, err)
	policy, err := keys.requestGroupPolicy(ctx, 7, 1)
	require.NoError(t, err)
	require.Equal(t, []string{"hidden"}, policy.Denied)
	require.NoError(t, admin.ClearGroupRateMultipliers(ctx, 1))
	policy, err = keys.requestGroupPolicy(ctx, 7, 1)
	require.NoError(t, err)
	require.Empty(t, policy.Denied)
	require.NoError(t, admin.DeleteGroup(ctx, 1))
	_, err = keys.requestGroup(ctx, 1)
	require.ErrorIs(t, err, ErrGroupNotFound)
}
