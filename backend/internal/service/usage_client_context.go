package service

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const maxUsageContextValueBytes = 255

// UsageClientContext is optional, client-declared attribution, never an identity
// for authorization, billing deduplication, scheduling or session blocking.
// Keep this wire shape additive: old secondary events omit it, and old peers
// ignore it. Only bounded scalar values are retained, not headers or prompts.
type UsageClientContext struct {
	Client         string `json:"client,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	ThreadID       string `json:"thread_id,omitempty"`
	ParentThreadID string `json:"parent_thread_id,omitempty"`
	AgentID        string `json:"agent_id,omitempty"`
	ParentAgentID  string `json:"parent_agent_id,omitempty"`
	AgentType      string `json:"agent_type,omitempty"`
	AgentRole      string `json:"agent_role,omitempty"`
	RequestKind    string `json:"request_kind,omitempty"`
	TurnID         string `json:"turn_id,omitempty"`
	IdentityStatus string `json:"identity_status,omitempty"`
}

// NormalizeUsageClientContext also bounds metadata arriving in a secondary's
// durable billing event. Invalid optional attribution never prevents billing.
func NormalizeUsageClientContext(in *UsageClientContext) *UsageClientContext {
	if in == nil {
		return nil
	}
	out := *in
	for _, field := range []*string{&out.SessionID, &out.ThreadID, &out.ParentThreadID, &out.AgentID, &out.ParentAgentID, &out.AgentType, &out.TurnID} {
		*field = usageContextValue(*field)
	}
	out.Client = usageContextEnum(out.Client, "codex", "claude_code", "opencode", "dsh")
	out.AgentRole = usageContextEnum(out.AgentRole, "main", "subagent")
	out.RequestKind = usageContextEnum(out.RequestKind, "turn", "compaction", "auxiliary", "workflow", "prewarm", "memory")
	out.IdentityStatus = usageContextEnum(out.IdentityStatus, "conflict", "invalid")
	if out == (UsageClientContext{}) {
		return nil
	}
	return &out
}

func usageContextValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxUsageContextValueBytes || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
		return ""
	}
	// gjson views may point into a large request. The async usage worker must
	// retain only these small strings, never the request's backing storage.
	return strings.Clone(value)
}

func usageContextEnum(value string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return candidate
		}
	}
	return ""
}

// ExtractUsageClientContext snapshots ingress metadata before async usage work.
// HTTP requests and each WS response.create frame share this parser. Missing
// markers stay unknown; prompts, tool names and prompt_cache_key are not proof
// of a conversation, a main agent or a subagent.
func ExtractUsageClientContext(c *gin.Context, body []byte) *UsageClientContext {
	if c == nil || c.Request == nil {
		return nil
	}
	values := make(map[string][]string)
	add := func(key, value string) {
		if value != "" {
			values[key] = append(values[key], value)
		}
	}
	header := func(key string, names ...string) {
		for _, name := range names {
			for _, value := range c.Request.Header.Values(name) {
				add(key, value)
			}
		}
	}
	header("session", "session_id", "session-id", "x-session-id", "x-opencode-session", "x-claude-code-session-id")
	header("thread", "thread_id", "thread-id", "conversation_id", "x-conversation-id")
	header("parent_thread", codexParentThreadIDHeader)
	header("agent", "x-claude-code-agent-id")
	header("parent_agent", "x-claude-code-parent-agent-id")
	header("agent_type", "x-claude-code-agent-type")
	header("subagent", openAISubagentHeader)
	header("class", "x-claude-code-request-class")

	addJSON := func(key string, value gjson.Result) {
		if value.Type == gjson.String && len(value.Raw) <= maxUsageContextValueBytes*6+2 {
			add(key, value.String())
		}
	}
	readTurn := func(raw string) {
		if len(raw) > 8192 || !gjson.Valid(raw) {
			return
		}
		view := gjson.Parse(raw)
		for key, path := range map[string]string{
			"session": "session_id", "thread": "thread_id", "parent_thread": "parent_thread_id",
			"turn": "turn_id", "subagent_kind": "subagent_kind", "kind": "request_kind",
		} {
			addJSON(key, view.Get(path))
		}
	}
	for _, raw := range c.Request.Header.Values(codexTurnMetadataHeader) {
		readTurn(raw)
	}
	// Skip body lookup entirely when it cannot contain metadata. Do not parse
	// multipart uploads or fully decode a large JSON conversation.
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '{' && (bytes.Contains(body, []byte("metadata")) || bytes.Contains(body, []byte(`\u`))) {
		root := parseRawJSONView(body)
		views := []gjson.Result{root}
		if strings.HasPrefix(root.Get("type").String(), "response.") {
			if payload := root.Get("response"); payload.IsObject() {
				views = append(views, payload)
			}
		}
		for _, view := range views {
			meta := view.Get("client_metadata")
			if meta.IsObject() {
				for key, path := range map[string]string{
					"session": "session_id", "thread": "thread_id", "turn": "turn_id",
					"parent_thread": codexParentThreadIDHeader, "subagent": openAISubagentHeader,
				} {
					addJSON(key, meta.Get(path))
				}
				turn := meta.Get(codexTurnMetadataHeader)
				if len(turn.Raw) <= 16*1024 && (turn.Type == gjson.String || turn.IsObject()) {
					readTurn(turn.String())
				}
			}
			userID := view.Get("metadata.user_id")
			if userID.Type == gjson.String && len(userID.Raw) <= 2048 {
				if parsed := ParseMetadataUserID(userID.String()); parsed != nil {
					add("session", parsed.SessionID)
				}
			}
		}
	}
	// Conflicting declarations must not silently correlate unrelated sessions.
	identityStatus := ""
	get := func(key string) string {
		var selected string
		for _, raw := range values[key] {
			value := usageContextValue(raw)
			if value == "" || (selected != "" && selected != value) {
				if key == "session" || key == "thread" {
					identityStatus = "conflict"
					if value == "" {
						identityStatus = "invalid"
					}
				}
				return ""
			}
			selected = value
		}
		return selected
	}
	out := &UsageClientContext{
		SessionID: get("session"), ThreadID: get("thread"), ParentThreadID: get("parent_thread"),
		AgentID: get("agent"), ParentAgentID: get("parent_agent"), AgentType: get("agent_type"), TurnID: get("turn"),
		RequestKind: usageContextEnum(get("kind"), "turn", "compaction", "prewarm", "memory"),
	}
	out.IdentityStatus = identityStatus
	subagent := get("subagent_kind")
	if subagent == "" {
		subagent = get("subagent")
	}
	if out.AgentType == "" {
		out.AgentType = subagent
	}
	if out.AgentID != "" || out.ParentAgentID != "" || out.ParentThreadID != "" || out.AgentType != "" {
		out.AgentRole = "subagent"
	}
	switch get("class") {
	case "main":
		if out.AgentRole == "" {
			out.AgentRole = "main"
		}
		out.RequestKind = "turn"
	case "subagent":
		out.AgentRole, out.RequestKind = "subagent", "turn"
	case "workflow":
		out.AgentRole, out.RequestKind = "subagent", "workflow"
	case "compaction", "auxiliary":
		out.RequestKind = get("class")
	}
	if subagent == "compact" || subagent == "compaction" {
		out.RequestKind = "compaction"
	}
	if c.Request.URL != nil && strings.HasSuffix(c.Request.URL.Path, "/responses/compact") {
		out.RequestKind = "compaction"
	}
	ua := strings.ToLower(c.GetHeader("User-Agent"))
	switch {
	case strings.Contains(ua, "codex") || subagent != "" || out.ParentThreadID != "":
		out.Client = "codex"
	case strings.Contains(ua, "claude-cli") || strings.Contains(ua, "claude-code") || c.GetHeader("x-claude-code-session-id") != "" || out.AgentID != "" || get("class") != "":
		out.Client = "claude_code"
	case strings.Contains(ua, "opencode"):
		out.Client = "opencode"
	case strings.Contains(ua, "deepseek-harness") || strings.HasPrefix(ua, "dsh/"):
		out.Client = "dsh"
	}
	return NormalizeUsageClientContext(out)
}
