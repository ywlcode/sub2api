//go:build unit

package handler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsageClientContextWebSocketRecordsEachTurn(t *testing.T) {
	got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:  `{"type":"response.create","model":"gpt-5.4","client_metadata":{"session_id":"root","thread_id":"child","turn_id":"t1","x-openai-subagent":"review","x-codex-parent-thread-id":"main"}}`,
		secondPayload: `{"type":"response.create","model":"gpt-5.4","client_metadata":{"session_id":"root","thread_id":"child","turn_id":"t2","x-openai-subagent":"review","x-codex-parent-thread-id":"main"}}`,
		userAgent:     testStringPtr("codex_cli_rs/0.125.0"),
	})
	require.Len(t, got.logs, 2)
	for i, turn := range []string{"t1", "t2"} {
		metadata := got.logs[i].ClientContext
		require.NotNil(t, metadata)
		require.Equal(t, "root", metadata.SessionID)
		require.Equal(t, "child", metadata.ThreadID)
		require.Equal(t, "main", metadata.ParentThreadID)
		require.Equal(t, "subagent", metadata.AgentRole)
		require.Equal(t, turn, metadata.TurnID, "capture metadata per WS turn before the async worker")
	}
}
