//go:build unit

package service

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"net/http"
	"testing"
)

func TestVolcengineAccountProbeUsesAllNativeEndpoints(t *testing.T) {
	account := volcengineTestAccount()
	account.Credentials["api_protocol"] = APIProtocolResponses
	account.Credentials["api_base_urls"] = map[string]any{
		APIProtocolChatCompletions: "https://chat.example/api/v3",
		APIProtocolResponses:       "https://responses.example/api/v3",
		APIProtocolAnthropic:       "https://messages.example/api/compatible",
	}
	account.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: false}
	svc, upstream := adaptiveCNAccountTestService(account, adaptiveCNChatTestResponse())
	upstream.responses = []*http.Response{adaptiveCNChatTestResponse(), adaptiveCNAnthropicTestResponse(), adaptiveCNResponsesTestResponse()}
	c, _ := newTestContext()
	require.NoError(t, svc.TestAccountConnection(c, account.ID, "ep-ark", "", AccountTestModeDefault))
	require.Len(t, upstream.requests, 3)
	for i, url := range []string{"https://chat.example/api/v3/chat/completions", "https://messages.example/api/compatible/v1/messages", "https://responses.example/api/v3/responses"} {
		require.Equal(t, url, upstream.requests[i].URL.String())
		require.Equal(t, "ep-ark", gjson.GetBytes(upstream.bodies[i], "model").String())
	}
}
