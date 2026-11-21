//go:build integration

package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSiteDiscoveryScopeAndAdmissionHintsFollowBindings(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := t.Context()
	var local, additional int64
	var key string
	require.NoError(t, p.db.QueryRowContext(ctx, "SELECT group_id,key FROM api_keys WHERE id=$1", p.key).Scan(&local, &key))
	require.NoError(t, p.db.QueryRowContext(ctx, "INSERT INTO groups(name,platform) VALUES('primary only','composite') RETURNING id").Scan(&additional))
	_, err := p.db.ExecContext(ctx, "INSERT INTO api_key_groups(api_key_id,group_id) VALUES($1,$2),($1,$3) ON CONFLICT DO NOTHING", p.key, local, additional)
	require.NoError(t, err)
	_, err = p.db.ExecContext(ctx, "INSERT INTO groups(name,platform) VALUES('unbound','composite')")
	require.NoError(t, err)
	ids, err := p.primary.DiscoveryGroupIDs(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{local, additional}, ids)
	p.sync(t)
	require.True(t, p.local.HasDiscoveryKey(key))
	require.False(t, p.local.HasDiscoveryKey("unknown"))
	require.False(t, p.local.HasDiscoveryKey(""))

	_, err = p.db.ExecContext(ctx, "UPDATE api_keys SET concurrency_limit=2 WHERE id=$1", p.key)
	require.NoError(t, err)
	p.sync(t)
	require.False(t, p.local.HasDiscoveryKey(key), "the primary owns the original listing slot")
	_, err = p.db.ExecContext(ctx, "UPDATE api_keys SET concurrency_limit=0,ip_whitelist='[\"198.51.100.1\"]' WHERE id=$1", p.key)
	require.NoError(t, err)
	p.sync(t)
	require.False(t, p.local.HasDiscoveryKey(key), "preserve the original IP ACL enforcement point")

	_, err = p.db.ExecContext(ctx, "DELETE FROM account_groups WHERE group_id=$1 AND account_id=$2", local, p.account)
	require.NoError(t, err)
	ids, err = p.primary.DiscoveryGroupIDs(ctx)
	require.NoError(t, err)
	require.Empty(t, ids, "additional bindings cannot confer replica ownership after the sole grant is removed")
	p.sync(t)
	require.False(t, p.local.HasDiscoveryKey(key))
}
