//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestModerationBannedUserCanLoginAndRefresh(t *testing.T) {
	ctx := context.Background()
	r := &contentModerationRepository{db: integrationDB}
	users := NewUserRepository(integrationEntClient, integrationDB)
	auth := service.NewAuthService(integrationEntClient, users, nil, NewRefreshTokenCache(integrationRedis),
		&config.Config{JWT: config.JWTConfig{Secret: "isolated-auth-test", ExpireHour: 1, RefreshTokenExpireDays: 1}}, nil, nil, nil, nil, nil, nil, nil, nil)
	panel := gin.New()
	panel.GET("/user/profile", gin.HandlerFunc(middleware.NewJWTAuthMiddleware(auth, service.NewUserService(users, nil, nil, nil), nil, nil)), func(c *gin.Context) {
		subject, ok := middleware.GetAuthSubjectFromContext(c)
		require.True(t, ok)
		c.JSON(http.StatusOK, subject.UserID)
	})
	profile := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/user/profile", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		panel.ServeHTTP(w, req)
		return w.Code
	}
	const password = "local-fixture-only"
	hash, err := auth.HashPassword(password)
	require.NoError(t, err)
	for _, scope := range []string{"all", "openai"} {
		t.Run(scope, func(t *testing.T) {
			email := fmt.Sprintf("audit-login-%d@example.test", time.Now().UnixNano())
			var id int64
			require.NoError(t, integrationDB.QueryRow(`INSERT INTO users(email,password_hash) VALUES($1,$2) RETURNING id`, email, hash).Scan(&id))
			t.Cleanup(func() {
				_, _ = integrationDB.Exec(`DELETE FROM content_moderation_user_actions WHERE user_id=$1`, id)
				_, _ = integrationDB.Exec(`DELETE FROM users WHERE id=$1`, id)
			})
			existing, _, err := auth.Login(ctx, email, password)
			require.NoError(t, err)
			since := time.Now().Add(-time.Hour)
			decision, err := r.ApplyModerationHit(ctx, service.ModerationHit{UserID: id, RequestID: "login-check", Action: "keyword_block", Since: since, AutoBan: true, BanScope: scope, Threshold: 1})
			require.NoError(t, err)
			require.True(t, decision.BanApplied)
			all, openai, err := r.ModerationPlatformAccess(ctx, id)
			require.NoError(t, err)
			require.Equal(t, scope == "all", all)
			require.Equal(t, scope == "openai", openai)
			fresh, user, err := auth.Login(ctx, email, password)
			require.NoError(t, err)
			require.True(t, user.IsActive())
			require.NotEmpty(t, fresh)
			require.Equal(t, http.StatusOK, profile(fresh))
			require.Equal(t, http.StatusOK, profile(existing))
			pair, oauthUser, err := auth.LoginOrRegisterOAuthWithTokenPairAndPromoCode(ctx, email, "", "", "", "", "dingtalk")
			require.NoError(t, err)
			require.Equal(t, id, oauthUser.ID)
			require.Equal(t, http.StatusOK, profile(pair.AccessToken))
			_, err = auth.RefreshToken(ctx, existing)
			require.NoError(t, err)
			page, err := r.MyModerationRecords(ctx, id, 1, 10, since, false)
			require.NoError(t, err)
			require.Equal(t, scope == "all", page.AccountBanned)
			require.Equal(t, scope == "openai", page.OpenAIBanned)

			_, err = integrationDB.Exec(`UPDATE users SET status='disabled' WHERE id=$1`, id)
			require.NoError(t, err)
			require.NoError(t, r.ManageModerationUser(ctx, id, service.ModerationUserAction{Action: "unban", Version: 1, Reason: "platform review", ActorID: id}))
			_, _, err = auth.Login(ctx, email, password)
			require.ErrorIs(t, err, service.ErrUserNotActive)
			require.Equal(t, http.StatusUnauthorized, profile(fresh))
			_, _, err = auth.LoginOrRegisterOAuthWithTokenPairAndPromoCode(ctx, email, "", "", "", "", "dingtalk")
			require.ErrorIs(t, err, service.ErrUserNotActive)
			_, err = auth.RefreshToken(ctx, fresh)
			require.ErrorIs(t, err, service.ErrUserNotActive)
		})
	}
}

func TestModerationPlatformMigrationRestoresOnlyOwnedAccounts(t *testing.T) {
	tx := testTx(t)
	type legacy struct {
		id   int64
		mode string
	}
	var cases []legacy
	deadline := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	for _, mode := range []string{"owned", "indefinite", "admin-disable-after-audit", "unknown", "active-again"} {
		var id int64
		require.NoError(t, tx.QueryRow(`INSERT INTO users(email,password_hash,status) VALUES($1,'fixture','disabled') RETURNING id`, mode+"@migration.example").Scan(&id))
		_, err := tx.Exec(`INSERT INTO content_moderation_user_state(user_id,version,generation,owned_status_revision,account_ban_expires_at)
			SELECT id,5,2,CASE WHEN $2='unknown' THEN NULL ELSE moderation_status_revision END,CASE WHEN $2='owned' THEN $3::timestamptz END FROM users WHERE id=$1`, id, mode, deadline)
		require.NoError(t, err)
		switch mode {
		case "admin-disable-after-audit":
			_, err = tx.Exec(`UPDATE users SET status='disabled' WHERE id=$1`, id)
		case "active-again":
			_, err = tx.Exec(`UPDATE users SET status='active' WHERE id=$1`, id)
		}
		require.NoError(t, err)
		cases = append(cases, legacy{id, mode})
	}
	migration, err := dbmigrations.FS.ReadFile("394_content_moderation_all_platforms.sql")
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		_, err = tx.Exec(string(migration))
		require.NoError(t, err)
		for _, tc := range cases {
			var status string
			var blocked bool
			var expiry sql.NullTime
			var version, generation, count int
			require.NoError(t, tx.QueryRow(`SELECT u.status,s.all_platforms_blocked,s.account_ban_expires_at,s.version,s.generation,s.recorded_ban_count
				FROM users u JOIN content_moderation_user_state s ON s.user_id=u.id WHERE u.id=$1`, tc.id).Scan(&status, &blocked, &expiry, &version, &generation, &count))
			converted := tc.mode == "owned" || tc.mode == "indefinite"
			require.Equal(t, converted, blocked, tc.mode)
			require.Equal(t, converted || tc.mode == "active-again", status == "active", tc.mode)
			if converted {
				require.Equal(t, 6, version)
			} else {
				require.Equal(t, 5, version)
			}
			require.Equal(t, 2, generation)
			require.Zero(t, count)
			require.Equal(t, tc.mode == "owned", expiry.Valid)
			if expiry.Valid {
				require.True(t, deadline.Equal(expiry.Time))
			}
		}
	}
}
