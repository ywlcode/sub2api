package service

import (
	"context"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// VolcengineToolUsage keeps the upstream's distinct counters, not billable
// quantities. In particular Messages search requests need not equal successful
// or billable searches. This evidence does not alter token or search charges.
type VolcengineToolUsage struct {
	WebSearch         *int64 `json:"web_search,omitempty"`
	WebSearchRequests *int64 `json:"web_search_requests,omitempty"`
	KnowledgeSearch   *int64 `json:"knowledge_search,omitempty"`
	MCP               *int64 `json:"mcp,omitempty"`
	DoubaoApp         *int64 `json:"doubao_app,omitempty"`
}

type volcengineToolUsageObserver struct {
	usage    VolcengineToolUsage
	protocol string
}

func newVolcengineToolUsageObserver(account *Account, protocol string) *volcengineToolUsageObserver {
	if !account.IsDeepSeekVolcengineCompatibilityEnabled() {
		return nil
	}
	return &volcengineToolUsageObserver{protocol: protocol}
}

func (o *volcengineToolUsageObserver) observe(payload []byte) {
	if o == nil || !gjson.ValidBytes(payload) {
		return
	}
	root := gjson.ParseBytes(payload)
	usage := root.Get("usage")
	if o.protocol == APIProtocolAnthropic && root.Get("type").String() == "message_start" {
		usage = root.Get("message.usage")
	} else if o.protocol == APIProtocolResponses && root.Get("response").IsObject() {
		usage = root.Get("response.usage")
	}
	// Usage is cumulative. A present nonnegative integer replaces the previous
	// snapshot (including zero); an absent or malformed counter leaves it alone.
	assign := func(path string, dst **int64) {
		v := usage.Get(path)
		if v.Type != gjson.Number {
			return
		}
		n, err := strconv.ParseInt(v.Raw, 10, 64)
		if err == nil && n >= 0 {
			*dst = &n
		}
	}
	if o.protocol == APIProtocolAnthropic {
		assign("server_tool_use.web_search_requests", &o.usage.WebSearchRequests)
	} else {
		assign("tool_usage.web_search", &o.usage.WebSearch)
		assign("tool_usage.knowledge_search", &o.usage.KnowledgeSearch)
		assign("tool_usage.mcp", &o.usage.MCP)
		assign("tool_usage.doubao_app", &o.usage.DoubaoApp)
	}
}

func (o *volcengineToolUsageObserver) snapshot() *VolcengineToolUsage {
	if o == nil || (o.usage.WebSearch == nil && o.usage.WebSearchRequests == nil && o.usage.KnowledgeSearch == nil && o.usage.MCP == nil && o.usage.DoubaoApp == nil) {
		return nil
	}
	usage := o.usage
	return &usage
}

func (o *volcengineToolUsageObserver) log(ctx context.Context, account *Account, requestID string) {
	if usage := o.snapshot(); usage != nil {
		logger.FromContext(ctx).With(zap.Int64("account_id", account.ID), zap.String("upstream_request_id", requestID),
			zap.String("protocol", o.protocol), zap.Any("native_tool_usage", usage)).Info("volcengine.tool_usage.observed")
	}
}
