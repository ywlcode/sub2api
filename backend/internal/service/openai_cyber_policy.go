package service

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// opsCyberPolicyKey 在 gin context 中携带 cyber_policy 命中标记。
// 由 gateway 服务层在检测到上游 error.code=="cyber_policy" 时设置，
// handler 在 Forward 返回后读取以触发风控记录、邮件与 tokens=0 用量行。
const opsCyberPolicyKey = "ops_cyber_policy"

// errOpenAICyberPolicyForwarded 表示 cyber_policy 已按当前端点格式透传给客户端
// （error 已写出/下发）。compat 路径 ForwardAsChatCompletions / ForwardAsAnthropic 出口
// 据此丢弃 result 并返回该哨兵，使 handler 落入 tokens=0 免费用量行（对齐 /v1/responses），
// 既不计费、也不 failover、不重复写响应。
var errOpenAICyberPolicyForwarded = errors.New("openai cyber_policy forwarded to client")

// CyberPolicyMark 记录一次 cyber_policy 硬阻断的上游证据。
type CyberPolicyMark struct {
	Code           string // 固定 "cyber_policy"
	Message        string // 上游 error.message
	Body           string // 上游 response.failed / 400 原始 body（已截断；未脱敏，ops_error 落库由 sanitizeErrorBodyForStorage、风控日志由 redactContentModerationSecrets 统一脱敏）
	UpstreamStatus int    // 上游 HTTP 状态（流式=200，非流式=400）
	UpstreamInTok  int    // 上游已报 input tokens（如有）
	UpstreamOutTok int    // 上游已报 output tokens（如有）
}

// MarkOpsCyberPolicy 记录 cyber 标记；首个写入生效，后续忽略（同一 turn 只记一次）。
// WS 多轮场景由 handler 在每个 turn 结束后调用 ClearOpsCyberPolicy 重置。
func MarkOpsCyberPolicy(c *gin.Context, mark CyberPolicyMark) {
	if c == nil {
		return
	}
	if GetOpsCyberPolicy(c) != nil {
		return
	}
	mark.Code = "cyber_policy"
	mark.Message = strings.TrimSpace(mark.Message)
	mark.Body = strings.TrimSpace(mark.Body)
	c.Set(opsCyberPolicyKey, &mark)
}

// GetOpsCyberPolicy 返回 cyber 标记，未命中（或已被 Clear）返回 nil。
func GetOpsCyberPolicy(c *gin.Context) *CyberPolicyMark {
	if c == nil {
		return nil
	}
	if v, ok := c.Get(opsCyberPolicyKey); ok {
		if m, ok := v.(*CyberPolicyMark); ok && m != nil {
			return m
		}
	}
	return nil
}

// ClearOpsCyberPolicy 清除 cyber 标记（typed-nil 覆盖；gin context 无并发安全的
// 删除原语，Set 走内部锁，与异步 GetOpsCyberPolicy 不构成 data race）。
// 仅 WS 多轮路径在 turn 收尾调用；HTTP 单请求路径不调用（context 随请求销毁，
// 且中间件 shouldSkipOpsErrorLogForCyber 依赖标记防双写）。
// WS 路径 clear 发生在中间件收尾之前，连接响应状态为 101，不触发中间件 status>=400
// 落库分支，故无双写/漏写。
func ClearOpsCyberPolicy(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(opsCyberPolicyKey, (*CyberPolicyMark)(nil))
}

// detectOpenAICyberPolicy 精确识别 cyber_policy（对齐 codex api_bridge.rs:145 /
// sse/responses.rs:529）。命中返回 (true, "cyber_policy", message)。
func detectOpenAICyberPolicy(payload []byte) (bool, string, string) {
	code := gjson.GetBytes(payload, "error.code").String()
	if code == "" {
		code = gjson.GetBytes(payload, "response.error.code").String()
	}
	if !strings.EqualFold(strings.TrimSpace(code), "cyber_policy") {
		return false, "", ""
	}
	msg := gjson.GetBytes(payload, "error.message").String()
	if msg == "" {
		msg = gjson.GetBytes(payload, "response.error.message").String()
	}
	return true, "cyber_policy", strings.TrimSpace(msg)
}

func markOpenAICyberPolicyEvent(c *gin.Context, payload []byte, upstreamStatus int, usage *OpenAIUsage) bool {
	hit, code, message := detectOpenAICyberPolicy(payload)
	if !hit {
		return false
	}
	mark := CyberPolicyMark{
		Code:           code,
		Message:        message,
		Body:           truncateString(string(payload), 4096),
		UpstreamStatus: upstreamStatus,
	}
	if usage != nil {
		mark.UpstreamInTok = usage.InputTokens
		mark.UpstreamOutTok = usage.OutputTokens
	}
	MarkOpsCyberPolicy(c, mark)
	return true
}

// cyber_policy is a request-scoped policy rejection even when transported as
// HTTP 400 or an event on an HTTP 200 stream. Match its rules as 403 so a generic
// 400 overload rule cannot hide the configured policy message. Callers must mark
// the original payload before applying a rule; account health and audit evidence
// must never be derived from the rewritten client response.
func applyOpenAICyberPolicyPassthroughRule(
	c *gin.Context,
	account *Account,
	payload []byte,
	defaultStatus int,
	defaultErrType, defaultErrMsg string,
) (status int, errType, errMsg string, matched bool) {
	if hit, _, _ := detectOpenAICyberPolicy(payload); !hit {
		return defaultStatus, defaultErrType, defaultErrMsg, false
	}
	platform := PlatformOpenAI
	if account != nil {
		platform = account.Platform
	}
	return applyErrorPassthroughRule(c, platform, http.StatusForbidden,
		openAIStreamFailedEventPassthroughBody(payload, defaultErrMsg),
		defaultStatus, defaultErrType, defaultErrMsg)
}

// rewriteOpenAICyberPolicyForClient returns a client-only copy. It preserves the
// error code and event envelope so SSE/WS clients still recognize the terminal
// policy rejection. The returned status can only be used before HTTP is written.
func rewriteOpenAICyberPolicyForClient(c *gin.Context, account *Account, payload []byte) ([]byte, int, bool) {
	status, errType, message, matched := applyOpenAICyberPolicyPassthroughRule(
		c, account, payload, http.StatusForbidden, "invalid_request_error", "",
	)
	if !matched {
		return payload, 0, false
	}
	clientPayload := payload
	for _, path := range []string{"error", "response.error"} {
		if !strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, path+".code").String()), "cyber_policy") {
			continue
		}
		var err error
		clientPayload, err = sjson.SetBytes(clientPayload, path+".message", message)
		if err != nil {
			return payload, 0, false
		}
		clientPayload, err = sjson.SetBytes(clientPayload, path+".type", errType)
		if err != nil {
			return payload, 0, false
		}
	}
	// Some WS error events carry an explicit numeric status in addition to the
	// HTTP upgrade status. Keep that field consistent with the matched rule.
	for _, path := range []string{"status", "status_code", "error.status", "error.status_code"} {
		if gjson.GetBytes(payload, path).Type == gjson.Number {
			if rewritten, err := sjson.SetBytes(clientPayload, path, status); err == nil {
				clientPayload = rewritten
			}
		}
	}
	return clientPayload, status, true
}

func writeOpenAICyberPolicyClientError(c *gin.Context, status int, payload []byte) {
	body := openAIStreamFailedEventPassthroughBody(payload, "")
	MarkResponseCommitted(c)
	if !writeOpenAICompactSSEBridge(c, status, body) {
		c.Data(status, "application/json; charset=utf-8", body)
	}
}
