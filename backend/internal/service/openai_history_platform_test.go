package service

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

type historyPlatformSettings struct {
	*history400SettingsRepo
	reads int
}

func (s *historyPlatformSettings) GetValue(ctx context.Context, key string) (string, error) {
	if key == SettingKeyOpenAI200OmitEncryptedHistoryEnabled || key == SettingKeyOpenAI400OmitEncryptedHistoryEnabled {
		s.reads++
	}
	return s.history400SettingsRepo.GetValue(ctx, key)
}

func TestOpenAIRejectedHistoryPlatformIsolation(t *testing.T) {
	for _, beta := range []int{200, 400} {
		for _, tc := range []struct {
			name, group, resolved, account string
		}{
			{"openai", PlatformOpenAI, "", PlatformOpenAI},
			{"deepseek", PlatformDeepseek, "", PlatformDeepseek},
			{"grok", PlatformGrok, "", PlatformGrok},
			{"anthropic", PlatformAnthropic, "", PlatformAnthropic},
			{"gemini", PlatformGemini, "", PlatformGemini},
			{"kimi", PlatformKimi, "", PlatformKimi},
			{"zhipu", PlatformZhipu, "", PlatformZhipu},
			{"composite_openai", PlatformComposite, PlatformOpenAI, PlatformOpenAI},
			{"composite_deepseek", PlatformComposite, PlatformDeepseek, PlatformDeepseek},
			{"account_overrides_route", PlatformOpenAI, PlatformOpenAI, PlatformDeepseek},
			{"unknown_platform", PlatformOpenAI, PlatformOpenAI, "unknown"},
			{"missing_account", PlatformOpenAI, PlatformOpenAI, ""},
		} {
			t.Run(strconv.Itoa(beta)+"/"+tc.name, func(t *testing.T) {
				svc := newStreamLineageHTTPService(nil)
				settings := &historyPlatformSettings{history400SettingsRepo: &history400SettingsRepo{values: map[string]string{
					SettingKeyOpenAI200OmitEncryptedHistoryEnabled: "true",
					SettingKeyOpenAI400OmitEncryptedHistoryEnabled: "true",
					SettingKeyOpenAI409OmitEncryptedHistoryEnabled: "true",
				}}}
				svc.settingService = NewSettingService(settings, svc.cfg)
				c, _ := newStreamLineageTestContext("platform-fixture")
				c.Set("api_key", &APIKey{ID: 7, Group: &Group{Platform: tc.group}, AllowLongContext: true})
				ctx := WithResolvedTargetPlatform(context.Background(), tc.resolved)
				// Identical model aliases and protocol never grant Beta access.
				body := history400Body(history400Input, true)
				ctx = svc.WithOpenAIAgentMessageRecovery(ctx, c, body, "platform-fixture", "gpt-5.1")
				account := newStreamLineageHTTPAccount()
				req, err := http.NewRequest(http.MethodPost, "https://api.openai.example/v1/responses", bytes.NewReader(body))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer synthetic-fixture")
				finish, err := svc.prepareOpenAIAgentRecoveryAttempt(ctx, account, req)
				require.NoError(t, err)
				defer finish(false)
				// The latest actual account remains authoritative even if an older
				// OpenAI route/admission context is present.
				account.Platform = tc.account
				if tc.account == "" {
					account = nil
				}
				settings.reads = 0
				prepare := svc.prepareOpenAI400HistoryRecovery
				diagnostic := openAI400HistoryDiagnostic
				if beta == 200 {
					prepare, diagnostic = svc.prepareOpenAI200HistoryRecovery, openAI200HistoryDiagnostic
				}
				next, clean, changed := prepare(ctx, c, account, body, 1)
				allowed := tc.account == PlatformOpenAI
				require.Equal(t, allowed, changed)
				require.Equal(t, allowed, openAIAgentRecoveryFromContext(next).omittedErrorHistory)
				if allowed {
					require.NotContains(t, string(clean), "AAAA-fixture-1111")
					require.Contains(t, string(clean), "visible reasoning one")
					require.Equal(t, 1, settings.reads)
				} else {
					require.Equal(t, body, clean)
					require.Same(t, ctx, next)
					require.Zero(t, settings.reads, "non-OpenAI accounts must not consult 200/400 settings")
					require.Nil(t, diagnostic(c), "non-OpenAI accounts must not record Beta decisions")
				}
			})
		}
	}
}
