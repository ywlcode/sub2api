//go:build integration

package repository

import (
	"context"
	"database/sql"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type secondaryAuthCountingSQL struct {
	sqlExecutor
	queries int
}

func (q *secondaryAuthCountingSQL) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.queries++
	return q.sqlExecutor.QueryContext(ctx, query, args...)
}

func TestSecondaryAuthProjectionMatchesFreshEntAuth(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	c := tx.Client()
	counted := &secondaryAuthCountingSQL{sqlExecutor: tx}
	r := newAPIKeyRepositoryWithSQL(c, counted)
	g, err := c.Group.Create().SetName("secondary-auth-group").SetPlatform(service.PlatformDeepseek).
		SetStatus(service.StatusActive).SetRateMultiplier(0.17).SetDailyLimitUsd(10).
		SetCodexCliOnly(true).SetStreamOnly(true).SetBlockSubagents(true).
		SetProfitControlEnabled(true).SetProfitMinMargin(0.2).SetProfitSafetyBuffer(0.05).
		SetModelRoutingEnabled(true).SetModelRouting(map[string][]int64{"test": {11, 12}}).
		SetMaxReasoningEffort("high").SetForceOpenaiFast(true).Save(ctx)
	require.NoError(t, err)
	extra, err := c.Group.Create().SetName("secondary-auth-extra").Save(ctx)
	require.NoError(t, err)
	u, err := c.User.Create().SetEmail("secondary-auth@example.invalid").SetPasswordHash("must-not-be-read").
		SetUsername("auth-user").SetStatus(service.StatusActive).SetBalance(23.45).
		SetConcurrency(5).SetPlatformConcurrencyLimits(map[string]int{"deepseek": 3}).
		SetRestrictPublicGroups(true).AddAllowedGroups(g, extra).Save(ctx)
	require.NoError(t, err)
	k, err := c.APIKey.Create().SetKey("secondary-auth-digest").SetUserID(u.ID).SetGroupID(g.ID).
		SetName("secondary-auth-key").SetStatus(service.StatusActive).
		SetModelMapping(map[string]string{"alias": "test"}).SetReasoningEffort("high").
		SetAllowLongContext(true).SetConcurrencyLimit(4).SetQuota(5).SetQuotaUsed(1.23).
		SetExpiresAt(time.Now().UTC().Add(time.Hour).Truncate(time.Second)).
		SetIPWhitelist([]string{"192.0.2.0/24"}).SetIPBlacklist([]string{"192.0.2.1"}).
		SetRateLimit5h(2).SetRateLimit1d(3).SetRateLimit7d(4).Save(ctx)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO api_key_groups(api_key_id,group_id,priority) VALUES ($1,$2,1),($1,$3,0)`, k.ID, extra.ID, g.ID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO user_group_rate_multipliers(user_id,group_id,rate_multiplier,denied_models) VALUES ($1,$2,1,'["blocked-model"]')`, u.ID, g.ID)
	require.NoError(t, err)
	check := func() *service.APIKey {
		t.Helper()
		old, err := r.GetByKeyForAuth(ctx, k.Key)
		require.NoError(t, err)
		if old.User != nil {
			slices.Sort(old.User.AllowedGroups)
			old.User.UserGroupDeniedModels = []string{"blocked-model"}
		}
		counted.queries = 0
		got, err := r.GetByKeyForSecondaryAuth(ctx, k.Key)
		require.NoError(t, err)
		require.Equal(t, 1, counted.queries, "all auth state, including denied models, needs only one SQL statement")
		require.Equal(t, old, got)
		if got.User != nil {
			require.Empty(t, got.User.PasswordHash)
		}
		counted.queries = 0
		live, err := r.GetSecondaryAuthState(ctx, k.Key)
		require.NoError(t, err)
		require.Equal(t, 1, counted.queries)
		require.Equal(t, got.ID, live.KeyID)
		require.Equal(t, got.UserID, live.UserID)
		require.Equal(t, got.GroupID, live.GroupID)
		require.Equal(t, got.Status, live.KeyStatus)
		require.Equal(t, got.Quota, live.Quota)
		require.Equal(t, got.QuotaUsed, live.QuotaUsed)
		require.Equal(t, got.ExpiresAt, live.ExpiresAt)
		require.Equal(t, got.User != nil, live.UserExists)
		require.Equal(t, got.Group != nil, live.GroupExists)
		if got.User != nil {
			require.Equal(t, got.User.Balance, live.Balance)
			require.Equal(t, got.User.Status, live.UserStatus)
		}
		if got.Group != nil {
			require.Equal(t, got.Group.Status, live.GroupStatus)
		}
		return got
	}
	check()
	_, err = tx.ExecContext(ctx, `UPDATE api_keys SET status='disabled',quota_used=5 WHERE id=$1`, k.ID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE users SET balance=0 WHERE id=$1`, u.ID)
	require.NoError(t, err)
	got := check()
	require.Equal(t, service.StatusDisabled, got.Status)
	require.Zero(t, got.User.Balance)
	require.True(t, got.IsQuotaExhausted())
	_, err = tx.ExecContext(ctx, `UPDATE groups SET deleted_at=NOW() WHERE id=$1`, g.ID)
	require.NoError(t, err)
	require.Nil(t, check().Group)
	_, err = tx.ExecContext(ctx, `UPDATE users SET deleted_at=NOW() WHERE id=$1`, u.ID)
	require.NoError(t, err)
	require.Nil(t, check().User)
	_, err = tx.ExecContext(ctx, `UPDATE api_keys SET deleted_at=NOW() WHERE id=$1`, k.ID)
	require.NoError(t, err)
	_, err = r.GetByKeyForSecondaryAuth(ctx, k.Key)
	require.ErrorIs(t, err, service.ErrAPIKeyNotFound)
	_, err = r.GetByKeyForSecondaryAuth(ctx, "missing-key")
	require.ErrorIs(t, err, service.ErrAPIKeyNotFound)
	_, err = r.GetSecondaryAuthState(ctx, k.Key)
	require.ErrorIs(t, err, service.ErrAPIKeyNotFound)
}

func TestSecondaryAuthNoGroupAndInvalidPermissionsFailClosed(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	c := tx.Client()
	r := newAPIKeyRepositoryWithSQL(c, tx)
	u, err := c.User.Create().SetEmail("no-group@example.invalid").SetPasswordHash("unused").Save(ctx)
	require.NoError(t, err)
	k, err := c.APIKey.Create().SetUserID(u.ID).SetKey("secondary-no-group").SetName("no-group").Save(ctx)
	require.NoError(t, err)
	old, err := r.GetByKeyForAuth(ctx, k.Key)
	require.NoError(t, err)
	got, err := r.GetByKeyForSecondaryAuth(ctx, k.Key)
	require.NoError(t, err)
	require.Equal(t, old, got)
	g, err := c.Group.Create().SetName("invalid-denied-models").Save(ctx)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE api_keys SET group_id=$1 WHERE id=$2`, g.ID, k.ID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO user_group_rate_multipliers(user_id,group_id,rate_multiplier,denied_models) VALUES ($1,$2,1,'{"unexpected":true}')`, u.ID, g.ID)
	require.NoError(t, err)
	_, err = r.GetByKeyForSecondaryAuth(ctx, k.Key)
	require.ErrorContains(t, err, "denied models")
}

func TestSecondaryAuthPreparedPlanStillReadsFreshState(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("postgres", integrationPostgresDSN)
	require.NoError(t, err)
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	u, err := integrationEntClient.User.Create().SetEmail("prepared-auth@example.invalid").SetPasswordHash("unused").Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, u.ID); require.NoError(t, err) })
	k, err := integrationEntClient.APIKey.Create().SetKey("prepared-secondary-auth").SetUserID(u.ID).SetName("prepared-auth").Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.ExecContext(ctx, `DELETE FROM api_keys WHERE id=$1`, k.ID)
		require.NoError(t, err)
	})
	r := newAPIKeyRepositoryWithSQL(integrationEntClient, db)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() { _, err := r.GetByKeyForSecondaryAuth(ctx, k.Key); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	stmt := r.secondaryAuthStmt.Load()
	require.NotNil(t, stmt)
	_, err = db.ExecContext(ctx, `UPDATE api_keys SET status='disabled' WHERE id=$1`, k.ID)
	require.NoError(t, err)
	got, err := r.GetByKeyForSecondaryAuth(ctx, k.Key)
	require.NoError(t, err)
	require.Equal(t, service.StatusDisabled, got.Status)
	require.Same(t, stmt, r.secondaryAuthStmt.Load(), "reuse a plan, never the previous auth result")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = r.GetByKeyForSecondaryAuth(cancelled, k.Key)
	require.ErrorIs(t, err, context.Canceled)
	_, err = r.GetByKeyForSecondaryAuth(ctx, k.Key)
	require.NoError(t, err, "a cancelled request must not poison the prepared statement")
}
