//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestSiteReviewLegacyLimitedKeyForwardsToPrimary(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	_, err := p.db.Exec(`UPDATE accounts SET status='active',schedulable=true,credentials='{"model_mapping":{"deepseek-chat":"deepseek-chat"}}'::jsonb WHERE id=$1`, p.account)
	require.NoError(t, err)
	p.sync(t)
	var key string
	require.NoError(t, p.db.QueryRow(`SELECT key FROM api_keys WHERE id=$1`, p.key).Scan(&key))
	require.True(t, p.local.MatchLocalModel(key, "deepseek-chat", service.CompositeRouteEndpointResponses))
	_, err = p.db.Exec(`UPDATE api_keys SET context_token_limit=8192 WHERE id=$1`, p.key)
	require.NoError(t, err)
	for schema := range siteLegacySchemas {
		snap, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Schema: schema})
		require.NoError(t, err)
		require.NoError(t, p.local.Install(ctx, site.SyncResponse{Full: true, PrimaryID: "main", Version: snap.Version, Rows: snap.Rows}))
		require.False(t, p.local.MatchLocalModel(key, "deepseek-chat", service.CompositeRouteEndpointResponses), "unsupported Key must reach primary admission instead of a local inactive-key error")
		var projected string
		require.NoError(t, p.replica.QueryRow(`SELECT key FROM api_keys WHERE id=$1`, p.key).Scan(&projected))
		require.False(t, p.local.MatchLocalModel(projected, "deepseek-chat", service.CompositeRouteEndpointResponses), "a routing sentinel must never become a usable client credential")
		_, err = p.db.Exec(`UPDATE api_keys SET context_token_limit=0 WHERE id=$1`, p.key)
		require.NoError(t, err)
		restored, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Schema: schema, Version: snap.Version})
		require.NoError(t, err)
		require.False(t, restored.Full)
		require.NoError(t, p.local.Install(ctx, site.SyncResponse{PrimaryID: "main", BaseVersion: snap.Version, Version: restored.Version, Rows: restored.Rows}))
		require.True(t, p.local.MatchLocalModel(key, "deepseek-chat", service.CompositeRouteEndpointResponses))
		_, err = p.db.Exec(`UPDATE api_keys SET context_token_limit=8192 WHERE id=$1`, p.key)
		require.NoError(t, err)
	}
}

func TestSiteReviewPrimaryRestartKeepsLegacyProjection(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	for schema := range siteLegacySchemas {
		before, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Schema: schema})
		require.NoError(t, err)
		restarted, err := NewSiteStore(p.db, &config.Config{Site: site.Config{Mode: site.Primary, ID: "main", Peers: []site.Peer{p.peer}}})
		require.NoError(t, err)
		after, err := restarted.ExportPage(ctx, p.peer, site.SyncRequest{Schema: schema, Version: before.Version})
		require.NoError(t, err)
		require.False(t, after.Full, "restart must not replace a negotiated legacy schema with the default contract")
		require.Equal(t, before.Version, after.Version)
	}
}

func TestSiteReviewPrimaryDoesNotRegisterUnusedNamespace(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	peer := site.Peer{ID: "office", Token: "review-pair-token"}
	primary, err := NewSiteStore(db, &config.Config{Site: site.Config{Mode: site.Primary, ID: "main", Peers: []site.Peer{peer}}})
	require.NoError(t, err)
	_, err = primary.ExportPage(context.Background(), peer, site.SyncRequest{Schema: siteConfigSchema, ReplicaID: uuid.NewString()})
	require.NoError(t, err)
	var phantom int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM site_export_nodes WHERE node_id=$1`, peer.ID).Scan(&phantom))
	require.Zero(t, phantom, "fresh replicas use node@uuid; the unused node namespace must not capture every write")
}
