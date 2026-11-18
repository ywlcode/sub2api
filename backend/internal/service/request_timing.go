package service

import (
	"context"
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/tidwall/gjson"
)

// Called only for validated, complete native Messages events. Protocol metadata
// and empty thinking blocks must not start the content clock.
func observeNativeAnthropicContentTiming(ctx context.Context, payload string) {
	if requesttiming.From(ctx) == nil {
		return
	}
	content := volcengineAnthropicHasContent(payload)
	requesttiming.Output(ctx, content, content, "")
	event := gjson.Parse(payload)
	switch event.Get("type").String() {
	case "content_block_delta":
		if event.Get("delta.text").String() != "" {
			requesttiming.Stage(ctx, "first_visible_text")
		}
		if event.Get("delta.thinking").String() != "" {
			requesttiming.Stage(ctx, "first_reasoning")
		}
	case "content_block_start":
		if event.Get("content_block.text").String() != "" {
			requesttiming.Stage(ctx, "first_visible_text")
		}
		if event.Get("content_block.thinking").String() != "" {
			requesttiming.Stage(ctx, "first_reasoning")
		}
	}
}

func observeNativeChatContentTiming(ctx context.Context, payload string) {
	if requesttiming.From(ctx) == nil || !gjson.Valid(payload) {
		return
	}
	content := false
	gjson.Get(payload, "choices").ForEach(func(_, choice gjson.Result) bool {
		delta := choice.Get("delta")
		if delta.Get("content").String() != "" {
			requesttiming.Stage(ctx, "first_visible_text")
			content = true
		}
		if delta.Get("reasoning_content").String() != "" || delta.Get("reasoning").String() != "" {
			requesttiming.Stage(ctx, "first_reasoning")
			content = true
		}
		delta.Get("tool_calls").ForEach(func(_, call gjson.Result) bool {
			content = content || call.Get("function.name").String() != "" || call.Get("function.arguments").String() != ""
			return true
		})
		return true
	})
	requesttiming.Output(ctx, content, content, "")
}

func timingTerminal(event string) string {
	switch event {
	case "response.completed", "response.done":
		return "completed"
	case "response.failed", "error":
		return "failed"
	case "response.incomplete":
		return "incomplete"
	}
	return ""
}

// Optional repository capability avoids changing billing's existing contract.
func (s *UsageService) RequestTimings(ctx context.Context, id int64) ([]json.RawMessage, error) {
	if _, err := s.GetByID(ctx, id); err != nil {
		return nil, err
	}
	if repo, ok := s.usageRepo.(interface {
		RequestTimings(context.Context, int64) ([]json.RawMessage, error)
	}); ok {
		return repo.RequestTimings(ctx, id)
	}
	return []json.RawMessage{}, nil
}
