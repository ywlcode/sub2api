//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSiteModerationAllPlatformsChangesOnlySecondaryAdmission(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	var key string
	require.NoError(t, p.db.QueryRow(`SELECT key FROM api_keys WHERE id=$1`, p.key).Scan(&key))
	cfg := &config.Config{Site: site.Config{Mode: site.Secondary, ID: p.peer.ID, Token: p.peer.Token}}
	keys := service.NewAPIKeyService(NewAPIKeyRepository(nil, p.replica), nil, nil, nil, nil, nil, cfg)
	p.local.BeforeAuthCommit = keys.BeginSiteAuthInstall
	p.local.AfterInstall = func(context.Context) error { keys.InvalidateSiteReplica(); return nil }
	router := gin.New()
	router.Use(gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(keys, nil, cfg)))
	router.GET("/v1/models", func(c *gin.Context) { c.Status(http.StatusOK) })
	request := func(status int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, status, w.Code, w.Body.String())
		if status == http.StatusUnauthorized {
			require.Contains(t, w.Body.String(), "USER_INACTIVE")
		}
	}
	request(http.StatusOK)
	request(http.StatusOK) // warm the replica's auth cache
	_, err := p.db.Exec(`INSERT INTO content_moderation_user_state(user_id,all_platforms_blocked,reason) VALUES($1,TRUE,'private review')`, p.user)
	require.NoError(t, err)
	ban := p.sync(t)
	require.False(t, ban.Full)
	require.Len(t, ban.Rows, 1)
	require.Equal(t, "users:"+strconv.FormatInt(p.user, 10), ban.Rows[0].Identity())
	require.NotContains(t, string(ban.Rows[0].Value), "private review")
	require.NotContains(t, string(ban.Rows[0].Value), "all_platforms_blocked")
	request(http.StatusUnauthorized)
	var status string
	require.NoError(t, p.db.QueryRow(`SELECT status FROM users WHERE id=$1`, p.user).Scan(&status))
	require.Equal(t, service.StatusActive, status)
	for legacy := range siteLegacySchemas {
		snap, err := p.primary.ExportPage(ctx, site.Peer{ID: "legacy", Token: p.peer.Token}, site.SyncRequest{Schema: legacy})
		require.NoError(t, err)
		found := false
		for _, row := range snap.Rows {
			if row.Identity() == ban.Rows[0].Identity() {
				var projected struct{ Status string }
				require.NoError(t, json.Unmarshal(row.Value, &projected))
				require.Equal(t, service.StatusDisabled, projected.Status)
				found = true
			}
		}
		require.True(t, found)
	}
	_, err = p.db.Exec(`UPDATE content_moderation_user_state SET all_platforms_blocked=FALSE WHERE user_id=$1`, p.user)
	require.NoError(t, err)
	p.sync(t)
	request(http.StatusOK)

	_, err = p.db.Exec(`UPDATE users SET status='disabled' WHERE id=$1`, p.user)
	require.NoError(t, err)
	_, err = p.db.Exec(`UPDATE content_moderation_user_state SET all_platforms_blocked=TRUE WHERE user_id=$1`, p.user)
	require.NoError(t, err)
	p.sync(t)
	request(http.StatusUnauthorized)
	_, err = p.db.Exec(`DELETE FROM content_moderation_user_state WHERE user_id=$1`, p.user)
	require.NoError(t, err)
	p.sync(t)
	request(http.StatusUnauthorized)
}

func TestSiteModerationOpenAIScopePreservesDomesticKeysAndForwardsMixedKeys(t *testing.T) {
	p := newSiteFollowupPair(t)
	_, err := p.db.Exec(`UPDATE accounts SET status='active',schedulable=true,credentials='{"model_mapping":{"deepseek-chat":"deepseek-chat"}}' WHERE id=$1`, p.account)
	require.NoError(t, err)
	var key string
	var groupID int64
	require.NoError(t, p.db.QueryRow(`SELECT key,group_id FROM api_keys WHERE id=$1`, p.key).Scan(&key, &groupID))
	_, err = p.db.Exec(`INSERT INTO content_moderation_user_state(user_id,openai_blocked) VALUES($1,TRUE)`, p.user)
	require.NoError(t, err)
	local := func(credential string) bool {
		return p.local.MatchLocalModel(credential, "deepseek-chat", service.CompositeRouteEndpointResponses)
	}
	p.sync(t)
	require.True(t, local(key))
	require.False(t, local("unknown-key"))

	var extraGroup int64
	require.NoError(t, p.db.QueryRow(`INSERT INTO groups(name,platform) VALUES('additional','deepseek') RETURNING id`).Scan(&extraGroup))
	_, err = p.db.Exec(`INSERT INTO api_key_groups(api_key_id,group_id,priority) VALUES($1,$2,0),($1,$3,1)`, p.key, groupID, extraGroup)
	require.NoError(t, err)
	p.sync(t)
	require.True(t, local(key))

	_, err = p.db.Exec(`UPDATE groups SET platform='openai' WHERE id=$1`, extraGroup)
	require.NoError(t, err)
	p.sync(t)
	require.False(t, local(key))
	require.False(t, p.local.HasDiscoveryKey(key))
	var projected string
	require.NoError(t, p.replica.QueryRow(`SELECT key FROM api_keys WHERE id=$1`, p.key).Scan(&projected))
	require.False(t, local(projected))
	_, err = p.db.Exec(`DELETE FROM content_moderation_user_state WHERE user_id=$1`, p.user)
	require.NoError(t, err)
	release := p.sync(t)
	require.False(t, release.Full)
	require.True(t, local(key))
}
