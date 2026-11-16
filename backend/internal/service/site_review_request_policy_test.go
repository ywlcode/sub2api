//go:build unit

package service

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSiteReviewRequestPolicyConsumers(t *testing.T) {
	primary := &config.Config{}
	replica := &config.Config{Site: site.Config{Mode: site.Secondary}}
	svc := &OpenAIGatewayService{cfg: replica}
	group := int64(9)
	body := []byte(`{"model":"gpt-test","input":[{"type":"agent_message","encrypted_content":"opaque"},{"role":"user","content":"continue"}]}`)
	for _, enabled := range []bool{true, false} {
		primary.Gateway.DisableEncryptedAgentOwnerBinding = enabled
		primary.Gateway.OpenAIScheduler.StrictSessionAffinityGroupIDs = nil
		if enabled {
			primary.Gateway.OpenAIScheduler.StrictSessionAffinityGroupIDs = []int64{group}
		}
		raw, err := primary.ExportSiteRequestPolicy()
		require.NoError(t, err)
		require.NoError(t, replica.InstallSiteRequestPolicy(raw))
		t.Run("affinity", func(t *testing.T) {
			require.Equal(t, enabled, svc.strictOpenAISessionAffinity(&group, PlatformOpenAI, ""))
		})
		t.Run("preserve_history", func(t *testing.T) {
			require.Equal(t, enabled, svc.preserveOpenAIAgentHistory(body))
		})
		t.Run("owner_binding", func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			ctx := svc.WithOpenAIAgentMessageRecovery(context.Background(), c, body, "review-session", "gpt-test")
			require.Equal(t, enabled, openAIAgentRecoveryFromContext(ctx).disableOwnerBinding)
		})
	}
	require.False(t, replica.Gateway.DisableEncryptedAgentOwnerBinding, "install must not mutate boot config")
}

func TestSiteReviewForceCodexRecoveryPolicy(t *testing.T) {
	primary := &config.Config{}
	primary.Gateway.ForceCodexCLI = true
	replica := &config.Config{Site: site.Config{Mode: site.Secondary}}
	raw, err := primary.ExportSiteRequestPolicy()
	require.NoError(t, err)
	require.NoError(t, replica.InstallSiteRequestPolicy(raw))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Extra: map[string]any{featureKeyCodexImageGenerationExplicitToolPolicy: codexImageGenerationExplicitToolPolicyStrip}}
	body, err := (&OpenAIGatewayService{cfg: replica}).projectOpenAI409ImageControls(context.Background(), c, account,
		[]byte(`{"model":"gpt-test","input":"hello","tools":[{"type":"image_generation"},{"type":"function","name":"keep"}]}`))
	require.NoError(t, err)
	require.Equal(t, "function", gjson.GetBytes(body, "tools.0.type").String(), "recovery must apply the same primary Codex policy as normal forwarding")
	require.Len(t, gjson.GetBytes(body, "tools").Array(), 1)
}
