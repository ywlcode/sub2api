package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestContentModerationBanScopeConfigCompatibility(t *testing.T) {
	settings := &contentModerationTestSettingRepo{values: map[string]string{SettingKeyContentModerationConfig: `{"enabled":true,"auto_ban_enabled":true}`}}
	svc := NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil)
	ctx := context.Background()
	cfg, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, ContentModerationBanScopeAll, cfg.BanScope)
	scope := ContentModerationBanScopeOpenAI
	cfg, err = svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{BanScope: &scope})
	require.NoError(t, err)
	require.Equal(t, ContentModerationBanScopeOpenAI, cfg.BanScope)
	cfg, err = svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{})
	require.NoError(t, err)
	require.Equal(t, ContentModerationBanScopeOpenAI, cfg.BanScope, "old clients omitting the field preserve the setting")
	scope = "invalid"
	_, err = svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{BanScope: &scope})
	require.Error(t, err)
	cfg, err = svc.GetConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, ContentModerationBanScopeOpenAI, cfg.BanScope)
	scope = ContentModerationBanScopeAll
	cfg, err = svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{BanScope: &scope})
	require.NoError(t, err)
	require.Equal(t, ContentModerationBanScopeAll, cfg.BanScope)
}

type platformBanTestRepo struct {
	ContentModerationRepository
	blocked bool
	err     error
	reads   int
}

func (r *platformBanTestRepo) IsOpenAIBlocked(context.Context, int64) (bool, error) {
	r.reads++
	return r.blocked, r.err
}

func TestContentModerationPlatformAccessIndependentOfAuditAndOtherPlatforms(t *testing.T) {
	r := &platformBanTestRepo{blocked: true}
	s := &ContentModerationService{repo: r}
	ctx := context.Background()
	require.Equal(t, http.StatusForbidden, infraerrors.Code(s.CheckPlatformAccess(ctx, 7, PlatformOpenAI)))
	for _, platform := range []string{PlatformAnthropic, PlatformGemini, PlatformGrok, PlatformAntigravity, PlatformKimi, PlatformTypeSafe} {
		require.NoError(t, s.CheckPlatformAccess(ctx, 7, platform), platform)
	}
	require.Equal(t, 1, r.reads, "other platforms need no OpenAI ban query")
	require.Error(t, s.CheckPlatformAccess(ctx, 7, PlatformComposite), "an unresolved pool could select OpenAI")
	r.blocked = false
	require.NoError(t, s.CheckPlatformAccess(ctx, 7, PlatformOpenAI), "release is visible immediately")
	r.err = errors.New("database unavailable")
	require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(s.CheckPlatformAccess(ctx, 7, PlatformOpenAI)))
	require.NoError(t, s.CheckPlatformAccess(ctx, 7, PlatformGemini))
}

func TestContentModerationOpenAIBanEvidenceAndEmail(t *testing.T) {
	r := &moderationRecoveryRepo{contentModerationTestRepo: &contentModerationTestRepo{}, result: ModerationHitResult{Accepted: true, Count: 3, BanApplied: true, OpenAIBlocked: true}}
	s := &ContentModerationService{repo: r}
	id, version := int64(7), int64(0)
	log := &ContentModerationLog{UserID: &id, UserEmail: "fixture@example.test", Flagged: true, moderationVersion: &version}
	cfg := defaultContentModerationConfig()
	cfg.AutoBanEnabled = true
	cfg.BanScope = ContentModerationBanScopeOpenAI
	require.True(t, s.applyFlaggedAccountSideEffects(context.Background(), cfg, log))
	require.Equal(t, ContentModerationBanScopeOpenAI, r.hits[0].BanScope)
	require.True(t, log.AutoBanned)
	require.Equal(t, ContentModerationBanScopeOpenAI, log.BanScope)
	body := buildContentModerationAccountDisabledEmailBody("Fixture", log, cfg)
	require.Contains(t, body, "OpenAI")
	require.Contains(t, body, "其他平台")
	require.NotContains(t, body, "所有 API 请求将被拒绝")
	require.NotContains(t, body, "账户已被自动禁用")
}
