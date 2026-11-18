//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUsageClientContextExtraction(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		headers          map[string]string
		want             *UsageClientContext
	}{
		{name: "codex parent", headers: map[string]string{"User-Agent": "codex_cli_rs/0.125", "session_id": "root", "conversation_id": "main"}, want: &UsageClientContext{Client: "codex", SessionID: "root", ThreadID: "main"}},
		{name: "codex child shares session", headers: map[string]string{"session_id": "root", "conversation_id": "child", "x-openai-subagent": "collab_spawn", "x-codex-parent-thread-id": "main", "x-codex-turn-metadata": `{"thread_id":"child","session_id":"root","subagent_kind":"thread_spawn","turn_id":"turn-2"}`}, want: &UsageClientContext{Client: "codex", SessionID: "root", ThreadID: "child", ParentThreadID: "main", AgentRole: "subagent", AgentType: "thread_spawn", TurnID: "turn-2"}},
		{name: "nested websocket metadata", body: `{"type":"response.create","client_metadata":{},"response":{"client_metadata":{"session_id":"root","thread_id":"child","x-openai-subagent":"review","x-codex-parent-thread-id":"main","x-codex-turn-metadata":{"request_kind":"compaction","turn_id":"t1"}}}}`, want: &UsageClientContext{Client: "codex", SessionID: "root", ThreadID: "child", ParentThreadID: "main", AgentRole: "subagent", AgentType: "review", RequestKind: "compaction", TurnID: "t1"}},
		{name: "claude explicit main", path: "/v1/messages", headers: map[string]string{"x-claude-code-session-id": "root", "x-claude-code-request-class": "main"}, want: &UsageClientContext{Client: "claude_code", SessionID: "root", AgentRole: "main", RequestKind: "turn"}},
		{name: "claude child compaction", path: "/v1/messages", headers: map[string]string{"x-claude-code-session-id": "root", "x-claude-code-agent-id": "child", "x-claude-code-parent-agent-id": "parent", "x-claude-code-request-class": "compaction"}, want: &UsageClientContext{Client: "claude_code", SessionID: "root", AgentID: "child", ParentAgentID: "parent", AgentRole: "subagent", RequestKind: "compaction"}},
		{name: "claude metadata fallback", headers: map[string]string{"User-Agent": "claude-cli/2.1"}, body: `{"metadata":{"user_id":"{\"device_id\":\"device\",\"session_id\":\"root\"}"}}`, want: &UsageClientContext{Client: "claude_code", SessionID: "root"}},
		{name: "opencode session no agent guess", headers: map[string]string{"User-Agent": "opencode/2", "x-opencode-session": "root", "x-session-id": "root"}, want: &UsageClientContext{Client: "opencode", SessionID: "root"}},
		{name: "dsh unknown session", headers: map[string]string{"User-Agent": "dsh/1"}, want: &UsageClientContext{Client: "dsh"}},
		{name: "explicit compact endpoint", path: "/v1/responses/compact", want: &UsageClientContext{RequestKind: "compaction"}},
		{name: "prompt and cache hints are not identity", body: `{"prompt_cache_key":"root","input":"you are a subagent","tools":[{"name":"spawn_agent"}]}`, headers: map[string]string{"X-Session-Affinity": "root"}},
		{name: "conflicting session header body", headers: map[string]string{"session_id": "one"}, body: `{"client_metadata":{"session_id":"two"}}`, want: &UsageClientContext{IdentityStatus: "conflict"}},
		{name: "conflicting envelope payload", body: `{"type":"response.create","client_metadata":{"thread_id":"one"},"response":{"client_metadata":{"thread_id":"two"}}}`, want: &UsageClientContext{IdentityStatus: "conflict"}},
		{name: "unrelated response property ignored", body: `{"response":{"client_metadata":{"session_id":"not-a-session"}}}`},
		{name: "invalid scalar ignored", body: `{"client_metadata":{"session_id":42,"thread_id":"bad\nvalue"}}`, want: &UsageClientContext{IdentityStatus: "invalid"}},
		{name: "oversized header ignored", headers: map[string]string{"session_id": strings.Repeat("x", 256)}, want: &UsageClientContext{IdentityStatus: "invalid"}},
		{name: "escaped metadata keys", body: `{"client_\u006detadata":{"session_id":"root"}}`, want: &UsageClientContext{SessionID: "root"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "/v1/responses"
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", path, nil)
			for key, value := range tc.headers {
				c.Request.Header.Set(key, value)
			}
			body := []byte(tc.body)
			got := ExtractUsageClientContext(c, body)
			require.Equal(t, tc.want, got)
			// Snapshot survives reuse of the request byte buffer.
			for i := range body {
				body[i] = 'x'
			}
			require.Equal(t, tc.want, got)
		})
	}
	require.Nil(t, ExtractUsageClientContext(nil, nil))
}

func TestUsageClientContextBillingSnapshotsAcrossPlatforms(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformDeepseek, PlatformAnthropic} {
		t.Run(platform, func(t *testing.T) {
			logs := &openAIRecordUsageLogRepoStub{inserted: true}
			users, subs := &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}
			account := &Account{ID: 3, Platform: platform, Type: AccountTypeAPIKey}
			key := &APIKey{ID: 1, Group: &Group{Platform: platform, RateMultiplier: 1}}
			metadata := &UsageClientContext{Client: "codex", SessionID: "root", ThreadID: "child", ParentThreadID: "parent", AgentRole: "subagent"}
			if platform == PlatformAnthropic {
				svc := newGatewayRecordUsageServiceForTest(logs, users, subs)
				svc.cfg.RunMode = config.RunModeSimple
				require.NoError(t, svc.RecordUsage(context.Background(), &RecordUsageInput{
					Result: &ForwardResult{RequestID: "fixture-context", Model: "claude-sonnet-4", Usage: ClaudeUsage{InputTokens: 10}},
					APIKey: key, User: &User{ID: 2}, Account: account, ClientContext: metadata,
				}))
			} else {
				svc := newOpenAIRecordUsageServiceForTest(logs, users, subs, nil)
				svc.cfg.RunMode = config.RunModeSimple
				require.NoError(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
					Result: &OpenAIForwardResult{RequestID: "fixture-context", Model: "gpt-5.1", Usage: OpenAIUsage{InputTokens: 10}},
					APIKey: key, User: &User{ID: 2}, Account: account, ClientContext: metadata,
				}))
			}
			require.Equal(t, metadata, logs.lastLog.ClientContext)
			metadata.SessionID = "modified-later"
			require.Equal(t, "root", logs.lastLog.ClientContext.SessionID)
			raw, err := json.Marshal(logs.lastLog)
			require.NoError(t, err)
			var decoded UsageLog
			require.NoError(t, json.Unmarshal(raw, &decoded))
			require.Equal(t, logs.lastLog.ClientContext, decoded.ClientContext, "durable billing JSON must preserve optional attribution")
		})
	}
}

func TestUsageClientContextNormalization(t *testing.T) {
	require.Nil(t, NormalizeUsageClientContext(nil))
	require.Nil(t, NormalizeUsageClientContext(&UsageClientContext{Client: "unknown", SessionID: strings.Repeat("x", 256), AgentRole: "invented", RequestKind: "invented"}))
	value := &UsageClientContext{SessionID: "  same  ", AgentID: "\xff", ThreadID: "bad\x00id"}
	require.Equal(t, &UsageClientContext{SessionID: "same"}, NormalizeUsageClientContext(value))
}

func BenchmarkUsageClientContextLargeRequest(b *testing.B) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.125")
	body := []byte(`{"input":"` + strings.Repeat("a", 1024*1024) + `","client_metadata":{"session_id":"root","thread_id":"child","x-codex-parent-thread-id":"main"}}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for b.Loop() {
		_ = ExtractUsageClientContext(c, body)
	}
}
