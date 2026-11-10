package apicompat

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// VolcengineReasoningParts extracts complete plaintext from an Ark DeepSeek
// reasoning item. This helper is only selected by the explicit account switch;
// it does not change generic OpenAI summary/ciphertext semantics.
func VolcengineReasoningParts(raw json.RawMessage) []string {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil || rawString(item["type"]) != "reasoning" {
		return nil
	}
	return volcengineReasoningParts(item)
}

func volcengineReasoningParts(item map[string]json.RawMessage) []string {
	collect := func(raw json.RawMessage, typ string) []string {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) != nil {
			return nil
		}
		var texts []string
		for _, part := range parts {
			if part.Type == typ && part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
		return texts
	}
	if parts := collect(item["content"], "reasoning_text"); len(parts) > 0 {
		return parts
	}
	// A summary accompanied by foreign ciphertext is not evidence of full
	// plaintext. Clients must resend the complete plaintext before converting it.
	if rawString(item["encrypted_content"]) != "" {
		return nil
	}
	return collect(item["summary"], "summary_text")
}

// Only unfinished reasoning items retain text. A completed item is emitted once
// and released; there is no cross-request reasoning cache.
type volcengineReasoningStream struct {
	pending  map[int]*volcengineReasoningItem
	finished map[int]bool
}

type volcengineReasoningItem struct {
	id      string
	raw     map[int]*strings.Builder
	summary map[int]*strings.Builder
}

func (s *volcengineReasoningStream) item(index int, id string) *volcengineReasoningItem {
	if s.pending == nil {
		s.pending = make(map[int]*volcengineReasoningItem)
		s.finished = make(map[int]bool)
	}
	item := s.pending[index]
	if item == nil {
		item = &volcengineReasoningItem{raw: make(map[int]*strings.Builder), summary: make(map[int]*strings.Builder)}
		s.pending[index] = item
	}
	if id != "" {
		item.id = id
	}
	return item
}

func (item *volcengineReasoningItem) snapshot(output *ResponsesOutput) {
	if output.ID != "" {
		item.id = output.ID
	}
	for i, part := range output.Content {
		if part.Type == "reasoning_text" && part.Text != "" {
			setVolcengineReasoningPart(item.raw, i, part.Text, true)
		}
	}
	if len(item.raw) > 0 {
		item.summary = nil
		return
	}
	for i, part := range output.Summary {
		if part.Type == "summary_text" && part.Text != "" {
			setVolcengineReasoningPart(item.summary, i, part.Text, true)
		}
	}
}

func setVolcengineReasoningPart(parts map[int]*strings.Builder, index int, text string, snapshot bool) {
	part := parts[index]
	if part == nil {
		part = &strings.Builder{}
		parts[index] = part
	}
	if snapshot {
		previous := part.String()
		if strings.HasPrefix(text, previous) {
			text = text[len(previous):]
		} else {
			part.Reset()
		}
	}
	_, _ = part.WriteString(text)
}

// A summary may be shorter than the raw text. Delay thinking until its item
// ends, so raw always wins regardless of event order and snapshots never repeat
// deltas. Ordinary assistant text continues through the existing stream path.
func (s *volcengineReasoningStream) events(evt *ResponsesStreamEvent) []*ResponsesStreamEvent {
	if (evt.Type == "response.output_item.added" || evt.Type == "response.output_item.done") && evt.Item != nil && evt.Item.Type == "reasoning" {
		if s.finished[evt.OutputIndex] {
			return nil
		}
		events := s.flushPending(evt.OutputIndex, false)
		s.item(evt.OutputIndex, evt.Item.ID).snapshot(evt.Item)
		if evt.Type == "response.output_item.done" {
			return append(events, s.flush(evt.OutputIndex)...)
		}
		return events
	}
	var raw, snapshot bool
	index := evt.SummaryIndex
	switch evt.Type {
	case "response.reasoning_text.delta", "response.reasoning_text.done":
		raw, index = true, evt.ContentIndex
		snapshot = evt.Type == "response.reasoning_text.done"
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		snapshot = evt.Type == "response.reasoning_summary_text.done"
	default:
		return s.otherEvents(evt)
	}
	if s.finished[evt.OutputIndex] {
		return nil
	}
	item := s.item(evt.OutputIndex, evt.ItemID)
	parts := item.summary
	if raw {
		parts = item.raw
	} else if len(item.raw) > 0 {
		return nil
	}
	text := evt.Delta
	if snapshot {
		text = evt.Text
	}
	if text != "" {
		setVolcengineReasoningPart(parts, index, text, snapshot)
		if raw {
			item.summary = nil
		}
	}
	return nil
}

func (s *volcengineReasoningStream) otherEvents(evt *ResponsesStreamEvent) []*ResponsesStreamEvent {
	terminal := evt.Type == "response.completed" || evt.Type == "response.done" || evt.Type == "response.incomplete"
	if terminal && evt.Response != nil {
		for index, output := range evt.Response.Output {
			if output.Type == "reasoning" && !s.finished[index] {
				s.item(index, output.ID).snapshot(&output)
			}
		}
	}
	// Recover a missing item.done before the next output item, rather than
	// leaving an old thinking block open while emitting text or a tool call.
	if terminal || evt.Type == "response.output_item.added" || evt.Type == "response.output_text.delta" || evt.Type == "response.output_text.done" || evt.Type == "response.function_call_arguments.delta" {
		return append(s.flushPending(evt.OutputIndex, terminal), evt)
	}
	return []*ResponsesStreamEvent{evt}
}

func (s *volcengineReasoningStream) flushPending(before int, all bool) []*ResponsesStreamEvent {
	var indices []int
	for index := range s.pending {
		if all || index < before {
			indices = append(indices, index)
		}
	}
	sort.Ints(indices)
	var events []*ResponsesStreamEvent
	for _, index := range indices {
		events = append(events, s.flush(index)...)
	}
	return events
}

func (s *volcengineReasoningStream) flush(index int) []*ResponsesStreamEvent {
	item := s.pending[index]
	if item == nil {
		return nil
	}
	delete(s.pending, index)
	parts := item.raw
	if len(parts) == 0 {
		parts = item.summary
	}
	indices := make([]int, 0, len(parts))
	for i := range parts {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	var text string
	if len(indices) == 1 {
		text = parts[indices[0]].String()
	} else {
		var joined strings.Builder
		for _, i := range indices {
			_, _ = joined.WriteString(parts[i].String())
		}
		text = joined.String()
	}
	if text == "" {
		return nil
	}
	s.finished[index] = true
	output := &ResponsesOutput{ID: item.id, Type: "reasoning"}
	return []*ResponsesStreamEvent{
		{Type: "response.output_item.added", OutputIndex: index, Item: output},
		{Type: "response.reasoning_text.delta", OutputIndex: index, ItemID: item.id, Delta: text},
		{Type: "response.output_item.done", OutputIndex: index, Item: output},
	}
}

func volcengineResponsesStreamError(evt *ResponsesStreamEvent) *ResponsesError {
	if evt.Type != "response.failed" && evt.Type != "error" && evt.Type != "response.cancelled" && evt.Type != "response.canceled" && (evt.Response == nil || evt.Response.Status != "failed" && evt.Response.Status != "cancelled" && evt.Response.Status != "canceled") {
		return nil
	}
	if evt.Response != nil && evt.Response.Error != nil {
		return evt.Response.Error
	}
	if evt.Error != nil {
		return evt.Error
	}
	failure := &ResponsesError{Code: evt.Code, Message: evt.Message}
	if failure.Code == "" {
		failure.Code = "upstream_error"
	}
	if failure.Message == "" {
		failure.Message = "Upstream response failed"
	}
	return failure
}

func validateVolcengineMessagesHistory(messages []AnthropicMessage) error {
	pending := make(map[string]bool)
	seen := make(map[string]bool)
	for _, message := range messages {
		var blocks []AnthropicContentBlock
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue // String content is handled by the existing converter.
		}
		for _, block := range blocks {
			switch block.Type {
			case "redacted_thinking":
				return fmt.Errorf("plaintext thinking is required for Ark DeepSeek; redacted thinking cannot be converted")
			case "thinking":
				if block.Thinking == "" {
					return fmt.Errorf("thinking block has no recoverable plaintext; resend the complete thinking history")
				}
			case "tool_use":
				if block.ID == "" || seen[block.ID] {
					return fmt.Errorf("tool history contains a missing or duplicate tool_use id")
				}
				seen[block.ID], pending[block.ID] = true, true
			case "tool_result":
				if !pending[block.ToolUseID] {
					return fmt.Errorf("tool_result %q has no matching pending call", block.ToolUseID)
				}
				delete(pending, block.ToolUseID)
			}
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("tool history is missing results; resend the complete call and result pairs")
	}
	return nil
}
