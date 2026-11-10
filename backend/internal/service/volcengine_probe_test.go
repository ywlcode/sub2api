//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestVolcengineAccountProbeAlwaysUsesResponsesBaseURL(t *testing.T) {
	for _, protocol := range []string{APIProtocolChatCompletions, APIProtocolAnthropic, APIProtocolAdaptive, APIProtocolResponses} {
		t.Run(protocol, func(t *testing.T) {
			account := volcengineTestAccount()
			account.Credentials["api_protocol"] = protocol
			account.Credentials["base_url"] = "https://old-chat.example/v1"
			account.Credentials["api_base_urls"] = map[string]any{
				APIProtocolResponses:       " https://ark.example/api/v3 ",
				APIProtocolChatCompletions: "https://chat.example/v1",
				APIProtocolAnthropic:       "https://anthropic.example",
			}
			account.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: false}
			svc, upstream := adaptiveCNAccountTestService(account, adaptiveCNResponsesTestResponse())
			c, _ := newTestContext()
			require.NoError(t, svc.TestAccountConnection(c, account.ID, "ep-ark", "", AccountTestModeDefault))
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "https://ark.example/api/v3/responses", upstream.requests[0].URL.String())
			require.Equal(t, "ep-ark", gjson.GetBytes(upstream.bodies[0], "model").String())
			require.Equal(t, APIProtocolResponses, resolveUpstreamProtocol(account, APIProtocolAnthropic, "ep-ark", nil))
		})
	}
}
