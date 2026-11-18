//go:build unit

package repository

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageClientContextOptionalBillingContract(t *testing.T) {
	command := service.UsageBillingCommand{RequestID: "context-compatibility", UserID: 1, APIKeyID: 2, AccountID: 3, BalanceCost: 1}
	command.Normalize()
	old := service.UsageBillingEvent{Version: 1, Command: command, Usage: service.UsageLog{
		RequestID: command.RequestID, UserID: 1, APIKeyID: 2, AccountID: 3, ActualCost: 1, CreatedAt: time.Now(),
	}}
	raw, err := json.Marshal(old)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "client_context")
	require.NoError(t, validateSiteBillingFields(raw))
	var newReader service.UsageBillingEvent
	require.NoError(t, json.Unmarshal(raw, &newReader))
	require.Nil(t, newReader.Usage.ClientContext)
	require.NoError(t, validateUsageBillingEvent(&newReader, service.UsageBillingEventKey{RequestID: command.RequestID, APIKeyID: 2}, command.RequestFingerprint))

	old.Usage.ClientContext = &service.UsageClientContext{SessionID: "session", AgentRole: "subagent"}
	raw, err = json.Marshal(old)
	require.NoError(t, err)
	// An old consumer's explicit wire projection ignores the new optional field.
	var oldReader struct {
		Version int                         `json:"version"`
		Command service.UsageBillingCommand `json:"command"`
		Usage   struct {
			UserID, APIKeyID, AccountID int64
			RequestID                   string
			ActualCost                  float64
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(raw, &oldReader))
	require.Equal(t, command.RequestFingerprint, oldReader.Command.RequestFingerprint)
	require.Equal(t, 1.0, oldReader.Usage.ActualCost)
	require.Equal(t, command.RequestID, oldReader.Usage.RequestID)
}
