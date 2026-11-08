//go:build unit

package service

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSchedulerSharesAdmissionGroupCacheAndInvalidation(t *testing.T) {
	ctx := context.Background()
	groups := &configGroupRepo{groups: map[int64]*Group{
		1: {ID: 1, Status: StatusActive, RequirePrivacySet: true, ModelRouting: map[string][]int64{"alias": {2}}},
		2: {ID: 2, Status: StatusActive},
	}}
	keys := &APIKeyService{groupRepo: groups, cache: &requestConfigAuthCacheStub{}}
	keys.authInvalidationConnected.Store(true)
	snapshot := NewSchedulerSnapshotService(nil, nil, nil, groups, nil)
	keys.SetGroupRouter(&GatewayService{schedulerSnapshot: snapshot})
	_, err := keys.requestGroup(ctx, 1) // Admission warms the existing cache.
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := snapshot.GetGroupByIDLite(ctx, 1)
			if err != nil {
				t.Error(err)
				return
			}
			g.ModelRouting["alias"][0] = 999
		}()
	}
	wg.Wait()
	require.Equal(t, 1, groups.calls, "100 scheduler lookups must reuse admission's query")
	require.Equal(t, int64(2), groups.groups[1].ModelRouting["alias"][0])
	gateway := &OpenAIGatewayService{schedulerSnapshot: snapshot}
	id := int64(1)
	require.True(t, gateway.loadOpenAIGroupRequiresPrivacySet(ctx, &id))
	groups.groups[1].RequirePrivacySet = false
	keys.applyRequestConfigInvalidation(RequestConfigInvalidationPrefix + "group:1")
	require.False(t, gateway.loadOpenAIGroupRequiresPrivacySet(ctx, &id))
	require.Equal(t, 2, groups.calls)
	other, err := snapshot.GetGroupByIDLite(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, int64(2), other.ID, "a composite member must use its own configuration")
	keys.authInvalidationConnected.Store(false)
	groups.groups[1].RequirePrivacySet = true
	require.True(t, gateway.loadOpenAIGroupRequiresPrivacySet(ctx, &id))
	delete(groups.groups, 1)
	require.True(t, gateway.loadOpenAIGroupRequiresPrivacySet(ctx, &id), "a failed live lookup retains fail-closed privacy")
}

type groupCacheLocalReader struct{ performanceLocalReader }

func (groupCacheLocalReader) GetLocalGroup(context.Context, int64) (*Group, error) {
	return &Group{ID: 1, RequirePrivacySet: true}, nil
}

func TestSchedulerGroupCachePreservesRegionalAuthority(t *testing.T) {
	groups := &configGroupRepo{groups: map[int64]*Group{1: {ID: 1}}}
	keys := &APIKeyService{groupRepo: groups, cache: &requestConfigAuthCacheStub{}}
	keys.authInvalidationConnected.Store(true)
	snapshot := NewSchedulerSnapshotService(nil, nil, nil, groups, nil)
	keys.SetGroupRouter(&GatewayService{schedulerSnapshot: snapshot})
	snapshot.SetLocalSnapshotReader(groupCacheLocalReader{})
	g, err := snapshot.GetGroupByIDLite(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, g.RequirePrivacySet)
	require.Zero(t, groups.calls)
}
