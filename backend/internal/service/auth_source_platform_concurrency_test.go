//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAuthSourcePlatformConcurrencySettings(t *testing.T) {
	ctx := context.Background()
	repo := &authSourceDefaultsRepoStub{}
	svc := NewSettingService(repo, &config.Config{})
	want := map[string]int{"openai": 5, "gemini": 0}
	provider := ProviderDefaultGrantSettings{Concurrency: 30, PlatformConcurrencyLimits: want}
	settings := &AuthSourceDefaultSettings{
		Email: provider, LinuxDo: provider, OIDC: provider, WeChat: provider,
		GitHub: provider, Google: provider, DingTalk: provider,
	}
	require.NoError(t, svc.UpdateAuthSourceDefaultSettings(ctx, settings))
	got, err := svc.GetAuthSourceDefaultSettings(ctx)
	require.NoError(t, err)
	for _, source := range []string{"email", "linuxdo", "oidc", "wechat", "github", "google", "dingtalk"} {
		defaults, ok := authSourceSignupSettings(got, source)
		require.True(t, ok)
		require.Equal(t, want, defaults.PlatformConcurrencyLimits)
	}
	// Older clients do not know this field; omission must not erase it.
	require.NoError(t, svc.UpdateAuthSourceDefaultSettings(ctx, &AuthSourceDefaultSettings{}))
	got, err = svc.GetAuthSourceDefaultSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, want, got.DingTalk.PlatformConcurrencyLimits)
	require.NoError(t, svc.UpdateAuthSourceDefaultSettings(ctx, &AuthSourceDefaultSettings{
		DingTalk: ProviderDefaultGrantSettings{PlatformConcurrencyLimits: map[string]int{}},
	}))
	got, err = svc.GetAuthSourceDefaultSettings(ctx)
	require.NoError(t, err)
	require.Empty(t, got.DingTalk.PlatformConcurrencyLimits)
	require.Equal(t, want, got.Email.PlatformConcurrencyLimits)
}

func TestAuthSourcePlatformConcurrencyValidation(t *testing.T) {
	for _, limits := range []map[string]int{{"openai": -1}, {"openai": 2147483648}, {"composite": 5}, {"unknown": 1}} {
		repo := &authSourceDefaultsRepoStub{}
		svc := NewSettingService(repo, &config.Config{})
		err := svc.UpdateAuthSourceDefaultSettings(context.Background(), &AuthSourceDefaultSettings{
			DingTalk: ProviderDefaultGrantSettings{PlatformConcurrencyLimits: limits},
		})
		require.Error(t, err)
		require.Empty(t, repo.updates)
	}
	cfg := &config.Config{}
	cfg.Runtime.RegionalConcurrencyFile = "/test/regional.yaml"
	svc := NewSettingService(&authSourceDefaultsRepoStub{}, cfg)
	require.Error(t, svc.UpdateAuthSourceDefaultSettings(context.Background(), &AuthSourceDefaultSettings{
		DingTalk: ProviderDefaultGrantSettings{PlatformConcurrencyLimits: map[string]int{"openai": 5}},
	}))
}

func TestAuthSourcePlatformConcurrencySignupPaths(t *testing.T) {
	for _, path := range []string{"email", "legacy_oauth", "dingtalk", "verified_email", "email_code", "auto_email"} {
		t.Run(path, func(t *testing.T) {
			repo := &userRepoStub{nextID: 71}
			values := map[string]string{SettingKeyRegistrationEnabled: "true"}
			for _, source := range []string{"email", "linuxdo", "dingtalk"} {
				values["auth_source_default_"+source+"_grant_on_signup"] = "true"
				values["auth_source_default_"+source+"_concurrency"] = "30"
				values[SettingKeyAuthSourcePlatformConcurrencyLimits(source)] = `{"openai":5,"gemini":0}`
			}
			cache := &emailCacheStub{data: &VerificationCodeData{
				Code: "246810", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
			}}
			svc := newOAuthEmailFlowAuthService(repo, &redeemCodeRepoStub{}, &refreshTokenCacheStub{}, values, cache, nil)
			ctx := context.Background()
			var user *User
			var err error
			switch path {
			case "email":
				_, user, err = svc.Register(ctx, "signup@example.com", "secret-123")
			case "legacy_oauth":
				_, user, err = svc.LoginOrRegisterOAuth(ctx, "linuxdo-123@linuxdo-connect.invalid", "test")
			case "dingtalk":
				_, user, err = svc.LoginOrRegisterOAuthWithTokenPair(ctx, "dingtalk-123@dingtalk-connect.invalid", "test", "", "", "dingtalk")
			case "verified_email":
				_, user, err = svc.RegisterVerifiedOAuthEmailAccount(ctx, "signup@example.com", "secret-123", "", "dingtalk")
			case "email_code":
				_, user, err = svc.RegisterOAuthEmailAccount(ctx, "signup@example.com", "secret-123", "246810", "", "dingtalk")
			case "auto_email":
				user, err = svc.createEmailOAuthUser(ctx, "signup@example.com", "test", "dingtalk", "", "")
			}
			require.NoError(t, err)
			require.Equal(t, 30, user.Concurrency)
			require.Equal(t, map[string]int{"openai": 5, "gemini": 0}, user.PlatformConcurrencyLimits)
			require.Len(t, repo.created, 1)
			require.Equal(t, user.PlatformConcurrencyLimits, repo.created[0].PlatformConcurrencyLimits)
		})
	}
}

func TestAuthSourcePlatformConcurrencyDisabledSignup(t *testing.T) {
	svc := newAuthService(&userRepoStub{nextID: 72}, map[string]string{
		SettingKeyRegistrationEnabled:                          "true",
		SettingKeyAuthSourceDefaultEmailGrantOnSignup:          "false",
		SettingKeyAuthSourcePlatformConcurrencyLimits("email"): `{"openai":5}`,
	}, nil, nil)
	_, user, err := svc.Register(context.Background(), "disabled@example.com", "secret-123")
	require.NoError(t, err)
	require.Empty(t, user.PlatformConcurrencyLimits)
}
