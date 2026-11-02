package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestDingTalkFirstBindPlatformConcurrency(t *testing.T) {
	ctx := context.Background()
	for _, enabled := range []string{"true", "false"} {
		t.Run(enabled, func(t *testing.T) {
			h, client := newOAuthPendingFlowTestHandlerWithDependencies(t, oauthPendingFlowTestHandlerOptions{
				settingValues: map[string]string{
					service.SettingKeyAuthSourceDefaultDingTalkGrantOnFirstBind:       enabled,
					service.SettingKeyAuthSourceDefaultDingTalkConcurrency:            "30",
					service.SettingKeyAuthSourcePlatformConcurrencyLimits("dingtalk"): `{"openai":5,"gemini":0}`,
				},
			})
			original := map[string]int{"openai": 12, "gemini": 3, "anthropic": 7}
			user, err := client.User.Create().SetEmail("first-bind@example.com").SetPasswordHash("test").
				SetConcurrency(10).SetPlatformConcurrencyLimits(original).Save(ctx)
			require.NoError(t, err)
			require.NoError(t, h.authService.ApplyProviderDefaultSettingsOnFirstBind(ctx, user.ID, "dingtalk"))
			got, err := client.User.Get(ctx, user.ID)
			require.NoError(t, err)
			if enabled == "false" {
				require.Equal(t, original, got.PlatformConcurrencyLimits)
				require.Equal(t, 10, got.Concurrency)
				return
			}
			require.Equal(t, map[string]int{"openai": 5, "gemini": 0, "anthropic": 7}, got.PlatformConcurrencyLimits)
			require.Equal(t, 40, got.Concurrency) // Existing total concurrency grant remains additive.
			require.Equal(t, 1, countProviderGrantRecords(t, client, user.ID, "dingtalk", "first_bind"))
			// A repeat bind must not reapply defaults after an administrator edits the user.
			require.NoError(t, client.User.UpdateOneID(user.ID).SetPlatformConcurrencyLimits(original).Exec(ctx))
			require.NoError(t, h.authService.ApplyProviderDefaultSettingsOnFirstBind(ctx, user.ID, "dingtalk"))
			got, err = client.User.Get(ctx, user.ID)
			require.NoError(t, err)
			require.Equal(t, original, got.PlatformConcurrencyLimits)
			require.Equal(t, 40, got.Concurrency)
		})
	}
}

func TestDingTalkFirstBindPlatformConcurrencyRollback(t *testing.T) {
	h, client := newOAuthPendingFlowTestHandlerWithDependencies(t, oauthPendingFlowTestHandlerOptions{
		settingValues: map[string]string{
			service.SettingKeyAuthSourceDefaultDingTalkGrantOnFirstBind:       "true",
			service.SettingKeyAuthSourcePlatformConcurrencyLimits("dingtalk"): `{"openai":5}`,
		},
	})
	ctx := context.Background()
	// Missing users fail after the grant reservation; the reservation must roll back.
	require.Error(t, h.authService.ApplyProviderDefaultSettingsOnFirstBind(ctx, 9999, "dingtalk"))
	require.Equal(t, 0, countProviderGrantRecords(t, client, 9999, "dingtalk", "first_bind"))
}
