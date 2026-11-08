//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type selectionReuseRepo struct {
	stubOpenAIAccountRepo
	reads      int
	pauseReads int
	pause      bool
	pauseErr   error
}

func (r *selectionReuseRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	r.reads++
	return r.stubOpenAIAccountRepo.GetByID(ctx, id)
}

func (r *selectionReuseRepo) PerformancePaused(context.Context, []int64) (map[int64]bool, error) {
	r.pauseReads++
	return map[int64]bool{1: r.pause}, r.pauseErr
}

type selectionReuseCache struct {
	snapshotHydrationCache
	reads int
}

func (c *selectionReuseCache) GetAccount(ctx context.Context, id int64) (*Account, error) {
	c.reads++
	return c.snapshotHydrationCache.GetAccount(ctx, id)
}

func selectionReuseFixture(platform string) (*OpenAIGatewayService, *selectionReuseRepo, *selectionReuseCache) {
	account := func(key string) *Account {
		return &Account{ID: 1, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive,
			Schedulable: true, Concurrency: 3, GroupIDs: []int64{2},
			Credentials: map[string]any{"api_key": key, "model_mapping": map[string]any{"model": "model"}}}
	}
	stale, live := account("stale-key"), account("current-db-key")
	repo := &selectionReuseRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{*live}}}
	cache := &selectionReuseCache{snapshotHydrationCache: snapshotHydrationCache{
		snapshot: []*Account{stale}, accounts: map[int64]*Account{1: stale},
	}}
	snapshot := NewSchedulerSnapshotService(cache, nil, repo, nil, nil)
	concurrency := NewConcurrencyService(&mockConcurrencyCache{})
	concurrency.performancePauses = repo
	return &OpenAIGatewayService{schedulerSnapshot: snapshot, accountRepo: repo, concurrencyService: concurrency}, repo, cache
}

func TestLegacySelectionReusesCompleteDBAccountAndRechecksEachAttempt(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformDeepseek} {
		t.Run(platform, func(t *testing.T) {
			svc, repo, cache := selectionReuseFixture(platform)
			group := int64(2)
			ctx := context.Background()
			for attempt := 1; attempt <= 2; attempt++ {
				result, err := svc.selectAccountWithLoadAwareness(ctx, &group, platform, "", "model", nil, false, "", true)
				require.NoError(t, err)
				require.True(t, result.Acquired)
				require.Equal(t, "current-db-key", result.Account.Credentials["api_key"], "a stale Redis snapshot must not overwrite the newer DB account")
				require.Equal(t, attempt, repo.reads, "each selection attempt retains its full DB recheck")
				require.Equal(t, attempt, cache.reads, "skip only the redundant hydration after DB recheck")
				result.ReleaseFunc()
			}
			require.GreaterOrEqual(t, repo.pauseReads, 8, "live pause checks remain on both attempts")
		})
	}
}

func TestLegacyNonBatchSelectionReusesWinnerWhenAnotherCandidateWasCheckedLast(t *testing.T) {
	svc, repo, cache := selectionReuseFixture(PlatformOpenAI)
	// Disable batch selection. All candidates are checked before sorting; the
	// winner is intentionally not the last object returned by the DB recheck.
	svc.concurrencyService = nil
	second := repo.accounts[0]
	second.ID, second.Priority = 2, 10
	second.Credentials = map[string]any{"api_key": "second-key", "model_mapping": map[string]any{"model": "model"}}
	repo.accounts = append(repo.accounts, second)
	cachedSecond := second
	cache.accounts[2] = &cachedSecond
	cache.snapshot = append(cache.snapshot, &cachedSecond)
	group := int64(2)
	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &group, "", "model", nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), selection.Account.ID)
	require.Equal(t, "current-db-key", selection.Account.Credentials["api_key"])
	require.Equal(t, 2, repo.reads, "both candidates retain their full database checks")
	require.Equal(t, 2, cache.reads, "neither of the two final hydration steps reloads the winner")
	selection.ReleaseFunc()
}

func TestSelectionReuseRetainsFinalPauseCancellationAndSlotRelease(t *testing.T) {
	for _, failure := range []string{"paused", "database error", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			svc, repo, cache := selectionReuseFixture(PlatformDeepseek)
			ctx := withOpenAISelectionAccountReuse(context.Background())
			group := int64(2)
			account := svc.recheckSelectedOpenAIAccountFromDB(ctx, cache.accounts[1], &group, PlatformDeepseek, "model", false, "")
			require.NotNil(t, account)
			switch failure {
			case "paused":
				repo.pause = true
			case "database error":
				repo.pauseErr = errors.New("pause authority unavailable")
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			released := 0
			result, err := svc.newAcquiredSelectionResult(ctx, account, func() { released++ }, "slot")
			require.Error(t, err)
			require.Nil(t, result)
			require.Equal(t, 1, released)
			require.Zero(t, cache.reads)
			require.False(t, account.PerformancePaused, "live checks must not mutate the stored DB object")
		})
	}
}

func TestSelectionReuseDoesNotAcceptUnverifiedAccountOrSurviveNewRound(t *testing.T) {
	svc, repo, cache := selectionReuseFixture(PlatformDeepseek)
	ctx := withOpenAISelectionAccountReuse(context.Background())
	group := int64(2)
	account := svc.recheckSelectedOpenAIAccountFromDB(ctx, cache.accounts[1], &group, PlatformDeepseek, "model", false, "")
	require.NotNil(t, account)
	other := *account
	for _, tc := range []struct {
		ctx context.Context
		a   *Account
	}{
		{context.Background(), account},
		{withOpenAISelectionAccountReuse(ctx), account},
		{ctx, &other},
	} {
		got, err := svc.hydrateSelectedAccount(tc.ctx, tc.a)
		require.NoError(t, err)
		require.Equal(t, "stale-key", got.Credentials["api_key"], "unverified and new-round callers must still load the snapshot")
	}
	require.Equal(t, 3, cache.reads)
	require.Equal(t, 1, repo.reads)
}
