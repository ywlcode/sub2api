package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSettingsAuthSourcePlatformConcurrency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, repo := newDingTalkSettingsHandler()
	key := service.SettingKeyAuthSourcePlatformConcurrencyLimits("dingtalk")
	for _, tc := range []struct {
		name, body, expected string
		status               int
	}{
		{"save", `{"auth_source_default_dingtalk_platform_concurrency_limits":{"openai":5,"anthropic":8}}`, `{"openai":5,"anthropic":8}`, 200},
		{"omitted", `{}`, `{"openai":5,"anthropic":8}`, 200},
		{"null", `{"auth_source_default_dingtalk_platform_concurrency_limits":null}`, `{"openai":5,"anthropic":8}`, 200},
		{"negative", `{"auth_source_default_dingtalk_platform_concurrency_limits":{"openai":-1}}`, `{"openai":5,"anthropic":8}`, 400},
		{"fractional", `{"auth_source_default_dingtalk_platform_concurrency_limits":{"openai":1.5}}`, `{"openai":5,"anthropic":8}`, 400},
		{"composite", `{"auth_source_default_dingtalk_platform_concurrency_limits":{"composite":5}}`, `{"openai":5,"anthropic":8}`, 400},
		{"replace", `{"auth_source_default_dingtalk_platform_concurrency_limits":{"openai":0}}`, `{"openai":0}`, 200},
		{"clear", `{"auth_source_default_dingtalk_platform_concurrency_limits":{}}`, `{}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings", bytes.NewBufferString(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			h.UpdateSettings(c)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			require.JSONEq(t, tc.expected, repo.values[key])
			if tc.status == 200 {
				var result struct {
					Data map[string]json.RawMessage `json:"data"`
				}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
				require.JSONEq(t, tc.expected, string(result.Data[key]))
			}
		})
	}
}

func TestSettingsAuthSourcePlatformConcurrencyAudit(t *testing.T) {
	before := &service.AuthSourceDefaultSettings{}
	after := &service.AuthSourceDefaultSettings{DingTalk: service.ProviderDefaultGrantSettings{
		PlatformConcurrencyLimits: map[string]int{"openai": 5},
	}}
	changed := diffSettings(&service.SystemSettings{}, &service.SystemSettings{}, before, after, UpdateSettingsRequest{})
	require.Contains(t, changed, "auth_source_default_dingtalk_platform_concurrency_limits")
}
