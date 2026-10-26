package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type multiGroupRepoStub struct {
	GroupRepository
	groups map[int64]*Group
}

func (s multiGroupRepoStub) GetByIDLite(_ context.Context, id int64) (*Group, error) {
	if group := s.groups[id]; group != nil {
		return group, nil
	}
	return nil, ErrGroupNotFound
}

type multiGroupRateStub struct {
	UserGroupRateRepository
	rates  map[int64]float64
	denied map[int64][]string
}

func (s multiGroupRateStub) GetByUserAndGroup(_ context.Context, _, groupID int64) (*float64, error) {
	if rate, ok := s.rates[groupID]; ok {
		return &rate, nil
	}
	return nil, nil
}
func (s multiGroupRateStub) GetRPMOverrideByUserAndGroup(context.Context, int64, int64) (*int, error) {
	return nil, nil
}
func (s multiGroupRateStub) GetDeniedModelsByUserAndGroup(_ context.Context, _, groupID int64) ([]string, error) {
	return s.denied[groupID], nil
}

type multiGroupRouterStub map[int64]APIKeyGroupAvailability

func (s multiGroupRouterStub) ProbeAPIKeyGroup(_ context.Context, group *Group, _, _ string) (APIKeyGroupAvailability, error) {
	return s[group.ID], nil
}

func TestAPIKeyMultiGroupSelectionUsesActualGroupAndDoesNotMutateAuthSnapshot(t *testing.T) {
	primary := int64(1)
	groups := map[int64]*Group{
		1: {ID: 1, Status: StatusActive, RateMultiplier: 0.5},
		2: {ID: 2, Status: StatusActive, RateMultiplier: 0.8},
		3: {ID: 3, Status: StatusActive, RateMultiplier: 0.1},
	}
	key := &APIKey{ID: 11, UserID: 7, GroupID: &primary, GroupIDs: []int64{1, 2, 3}, Group: groups[1], User: &User{ID: 7, Balance: 10, UserGroupDeniedModels: []string{"original"}}}
	svc := &APIKeyService{groupRepo: multiGroupRepoStub{groups: groups}, userGroupRateRepo: multiGroupRateStub{rates: map[int64]float64{2: 0.2}}, groupRouter: multiGroupRouterStub{1: {Available: true, Load: 0.9}, 2: {Available: true}, 3: {Available: true}}}
	got, err := svc.SelectRequestGroup(context.Background(), key, []string{"gpt-5"}, "token", nil, nil, true)
	require.NoError(t, err)
	// Priority wins over a lower group multiplier, user override, or lower load.
	require.Equal(t, int64(1), *got.GroupID)
	require.Same(t, groups[1], got.Group)
	require.Equal(t, key.GroupIDs, got.GroupIDs)
	require.Equal(t, int64(1), *key.GroupID)
	require.Equal(t, []string{"original"}, key.User.UserGroupDeniedModels)
	require.NotSame(t, key.User, got.User)

	// A restricted primary falls through and bills the next eligible group.
	svc.userGroupRateRepo = multiGroupRateStub{denied: map[int64][]string{1: {"gpt-5"}}}
	got, err = svc.SelectRequestGroup(context.Background(), key, []string{"gpt-5"}, "token", nil, nil, true)
	require.NoError(t, err)
	require.Equal(t, int64(2), *got.GroupID)
	require.Same(t, groups[2], got.Group)
	require.Equal(t, int64(1), *key.GroupID)
	require.Equal(t, []string{"original"}, key.User.UserGroupDeniedModels)
}

func TestAPIKeyMultiGroupSelectionPreservesPriorityWhenBusyAndRejectsRevokedGroups(t *testing.T) {
	primary := int64(1)
	groups := map[int64]*Group{1: {ID: 1, Status: StatusActive, RateMultiplier: 0.1}, 2: {ID: 2, Status: StatusActive, RateMultiplier: 0.3}, 3: {ID: 3, Status: StatusActive, RateMultiplier: 0, IsExclusive: true}}
	key := &APIKey{UserID: 7, GroupID: &primary, GroupIDs: []int64{1, 2, 3}, Group: groups[1], User: &User{ID: 7, Balance: 10}}
	svc := &APIKeyService{groupRepo: multiGroupRepoStub{groups: groups}, groupRouter: multiGroupRouterStub{1: {Available: true, Busy: true, Load: 1}, 2: {Available: true, Load: 0.1}, 3: {Available: true}}}
	got, err := svc.SelectRequestGroup(context.Background(), key, nil, "token", nil, nil, true)
	require.NoError(t, err)
	require.Equal(t, int64(1), *got.GroupID)

	// When all groups are busy, keep the configured order for normal queuing.
	svc.groupRouter = multiGroupRouterStub{1: {Available: true, Busy: true, Load: 2}, 2: {Available: true, Busy: true, Load: 1}}
	got, err = svc.SelectRequestGroup(context.Background(), key, nil, "token", nil, nil, true)
	require.NoError(t, err)
	require.Equal(t, int64(1), *got.GroupID)

	_, err = svc.PinAPIKeyGroup(context.Background(), key, 3)
	require.ErrorIs(t, err, ErrGroupNotAllowed)
	svc.groupRouter = multiGroupRouterStub{}
	_, err = svc.SelectRequestGroup(context.Background(), key, nil, "token", nil, nil, true)
	require.Error(t, err)
}

func TestAPIKeyMultiGroupSelectionFollowsReorderedGroupsAndSkipsUnavailable(t *testing.T) {
	primary := int64(3)
	groups := map[int64]*Group{
		1: {ID: 1, Status: StatusActive},
		2: {ID: 2, Status: StatusActive},
		3: {ID: 3, Status: StatusActive},
	}
	key := &APIKey{UserID: 7, GroupID: &primary, GroupIDs: []int64{3, 2, 1}, User: &User{ID: 7}}
	svc := &APIKeyService{groupRepo: multiGroupRepoStub{groups: groups}, groupRouter: multiGroupRouterStub{1: {Available: true}, 2: {Available: true}, 3: {Available: true}}}
	for _, tc := range []struct {
		name string
		skip func(*APIKey) bool
		want int64
	}{
		{"configured order", nil, 3},
		{"request restrictions", func(key *APIKey) bool { return *key.GroupID != 3 }, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.SelectRequestGroup(context.Background(), key, nil, "token", nil, tc.skip, true)
			require.NoError(t, err)
			require.Equal(t, tc.want, *got.GroupID)
		})
	}
	svc.groupRouter = multiGroupRouterStub{1: {Available: true}, 2: {Available: true}}
	got, err := svc.SelectRequestGroup(context.Background(), key, nil, "token", nil, nil, true)
	require.NoError(t, err)
	require.Equal(t, int64(2), *got.GroupID)
	groups[2].Status = "inactive"
	got, err = svc.SelectRequestGroup(context.Background(), key, nil, "token", nil, nil, true)
	require.NoError(t, err)
	require.Equal(t, int64(1), *got.GroupID)
}

type multiGroupSubscriptionStub struct {
	UserSubscriptionRepository
	subscriptions map[int64]*UserSubscription
}

func (s multiGroupSubscriptionStub) GetActiveByUserIDAndGroupID(_ context.Context, _, groupID int64) (*UserSubscription, error) {
	if sub := s.subscriptions[groupID]; sub != nil {
		return sub, nil
	}
	return nil, ErrSubscriptionNotFound
}

func TestAPIKeyMultiGroupSelectionFallsThroughExhaustedSubscriptionsToBalance(t *testing.T) {
	now := time.Now()
	limit := 10.0
	primary := int64(1)
	groups := map[int64]*Group{
		1: {ID: 1, Status: StatusActive, SubscriptionType: SubscriptionTypeSubscription, DailyLimitUSD: &limit},
		2: {ID: 2, Status: StatusActive, SubscriptionType: SubscriptionTypeSubscription, WeeklyLimitUSD: &limit, MonthlyLimitUSD: &limit},
		3: {ID: 3, Status: StatusActive, SubscriptionType: SubscriptionTypeStandard},
	}
	subs := map[int64]*UserSubscription{}
	for _, id := range []int64{1, 2} {
		subs[id] = &UserSubscription{UserID: 7, GroupID: id, Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour),
			DailyWindowStart: &now, WeeklyWindowStart: &now, MonthlyWindowStart: &now}
	}
	key := &APIKey{UserID: 7, GroupID: &primary, GroupIDs: []int64{1, 2, 3}, Group: groups[1], User: &User{ID: 7, Balance: 10}}
	svc := &APIKeyService{groupRepo: multiGroupRepoStub{groups: groups}, userSubRepo: multiGroupSubscriptionStub{subscriptions: subs},
		groupRouter: multiGroupRouterStub{1: {Available: true, Busy: true}, 2: {Available: true}, 3: {Available: true}}}
	subService := &SubscriptionService{userSubRepo: svc.userSubRepo, now: func() time.Time { return now }}
	accepts := func(candidate *APIKey) bool {
		return candidate.Group.IsSubscriptionType() || candidate.User.Balance > 0
	}
	selectGroup := func(want int64) {
		t.Helper()
		got, err := svc.SelectRequestGroup(context.Background(), key, nil, "token", subService, accepts, true)
		require.NoError(t, err)
		require.Equal(t, want, *got.GroupID)
		require.Same(t, groups[want], got.Group)
		require.Equal(t, int64(1), *key.GroupID)
	}
	// An available subscription is used even while busy and with no balance.
	key.User.Balance = 0
	selectGroup(1)
	subs[1].DailyUsageUSD = limit
	selectGroup(2)
	subs[2].WeeklyUsageUSD = limit
	_, err := svc.SelectRequestGroup(context.Background(), key, nil, "token", subService, accepts, true)
	require.Error(t, err, "no balance may be charged after all subscriptions are exhausted")
	key.User.Balance = 10
	selectGroup(3)
	subs[2].WeeklyUsageUSD = 0
	subs[2].MonthlyUsageUSD = limit
	selectGroup(3)
	// A refreshed subscription quota takes precedence again on the next request.
	subs[1].DailyUsageUSD = 0
	selectGroup(1)
}

func TestNormalizeAPIKeyGroupIDsCompatibilityAndValidation(t *testing.T) {
	id := int64(7)
	ids, err := normalizeAPIKeyGroupIDs(&id, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{7}, ids)
	for _, invalid := range [][]int64{{7, 7}, {7, 0}, {8, 7}} {
		_, err := normalizeAPIKeyGroupIDs(&id, invalid)
		require.Error(t, err)
	}
	ids, err = normalizeAPIKeyGroupIDs(&id, []int64{7, 9, 8, 6, 5})
	require.NoError(t, err)
	require.Equal(t, []int64{7, 9, 8, 6, 5}, ids)
	_, err = normalizeAPIKeyGroupIDs(&id, []int64{7, 9, 8, 6, 5, 4})
	require.Error(t, err)
	svc := &APIKeyService{}
	key := &APIKey{ID: 1, UserID: 7, GroupID: &id, GroupIDs: []int64{7, 8}, User: &User{ID: 7}}
	roundtrip := svc.snapshotToAPIKey("unused", svc.snapshotFromAPIKey(context.Background(), key))
	require.Equal(t, []int64{7, 8}, roundtrip.GroupIDs)
}
