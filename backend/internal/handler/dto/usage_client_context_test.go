package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageClientContextVisibleInUserAndAdminDTO(t *testing.T) {
	metadata := &service.UsageClientContext{Client: "codex", SessionID: "root", ThreadID: "child", ParentThreadID: "main", AgentRole: "subagent"}
	log := &service.UsageLog{UserID: 1, APIKeyID: 2, ClientContext: metadata}
	for _, value := range []any{UsageLogFromServiceForUser(log, false), UsageLogFromServiceAdmin(log)} {
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		var result struct {
			ClientContext *service.UsageClientContext `json:"client_context"`
		}
		require.NoError(t, json.Unmarshal(raw, &result))
		require.Equal(t, metadata, result.ClientContext)
	}
	raw, err := json.Marshal(UsageLogFromServiceForUser(&service.UsageLog{}, false))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "client_context", "legacy records must not invent attribution")
}
