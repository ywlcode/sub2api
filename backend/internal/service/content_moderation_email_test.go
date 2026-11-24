//go:build unit

package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

func TestBanNotificationEmailUsesPersistedDeadline(t *testing.T) {
	ctx := context.Background()
	expiresAt := time.Date(2026, 11, 1, 9, 30, 42, 0, time.UTC)
	localExpiry := expiresAt.In(timezone.Location())
	deadline := localExpiry.Format(time.DateTime) + " UTC" + localExpiry.Format("-07:00")
	for _, tc := range []struct{ scope, event, locale, scopeText, dashboardText, label string }{
		{ContentModerationBanScopeAll, NotificationEmailEventContentModerationDisabled, "en", "model requests on all platforms", "still sign in and use your dashboard", "Scheduled automatic release"},
		{ContentModerationBanScopeAll, NotificationEmailEventContentModerationDisabled, "zh", "全部平台的模型请求", "仍可正常登录并使用控制台", "预计自动解封时间"},
		{ContentModerationBanScopeOpenAI, NotificationEmailEventOpenAIRestricted, "en", "OpenAI model requests", "still sign in, use your dashboard, and use other platforms", "Scheduled automatic release"},
		{ContentModerationBanScopeOpenAI, NotificationEmailEventOpenAIRestricted, "zh", "OpenAI 模型请求", "仍可正常登录、使用控制台及其他平台", "预计自动解封时间"},
	} {
		for _, path := range []string{"official", "custom", "legacy", "invalid", "built-in"} {
			t.Run(tc.scope+"/"+tc.locale+"/"+path, func(t *testing.T) {
				smtp := startNotificationEmailTestSMTPServer(t)
				settings := &contentModerationTestSettingRepo{values: smtp.settings()}
				email := NewEmailService(settings, nil)
				svc := &ContentModerationService{emailService: email, settingRepo: settings}
				log := &ContentModerationLog{UserEmail: "user@example.com", BanScope: tc.scope, BanExpiresAt: &expiresAt, AutoBanned: true}
				settings.values[notificationEmailLocaleEmailKeyPrefix+notificationEmailHash(log.UserEmail)] = tc.locale
				if path != "built-in" {
					templates := NewNotificationEmailService(settings, email)
					switch path {
					case "custom", "legacy":
						body := "<HTML><BODY>Saved custom body</BODY></HTML>"
						if path == "custom" {
							body = "<p>Saved custom body {{ ban_expires_at }}</p>"
						}
						_, err := templates.UpdateTemplate(ctx, tc.event, tc.locale, "Restriction {{ban_expires_at}}", body)
						require.NoError(t, err)
					case "invalid":
						settings.values[notificationEmailTemplateKey(tc.event, tc.locale)] = "invalid JSON"
					}
				}
				require.NoError(t, svc.sendAccountDisabledEmail(ctx, defaultContentModerationConfig(), log))
				require.EqualValues(t, 1, smtp.messageCount())
				body := smtp.lastMessageBody(t)
				require.Equal(t, 1, strings.Count(body, deadline), body)
				if tc.locale == "zh" {
					require.Contains(t, body, "管理员可提前解除限制。")
				} else {
					require.Contains(t, body, "An administrator can lift the restriction sooner.")
				}
				switch path {
				case "custom":
					require.Contains(t, body, "<p>Saved custom body "+deadline+"</p>")
				case "legacy":
					require.Contains(t, body, "Saved custom body<p>"+tc.label)
					require.Contains(t, body, deadline+"</p><p>")
					require.Contains(t, body, "</p></BODY>")
				default:
					require.Contains(t, body, tc.scopeText)
					require.Contains(t, body, tc.dashboardText)
					require.Contains(t, body, tc.label)
				}
			})
		}
	}
}
