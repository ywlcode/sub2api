package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVolcengineAnthropicResponsesHistoryOptIn(t *testing.T) {
	req := &AnthropicRequest{Model: "ep-ark", MaxTokens: 17, Tools: []AnthropicTool{{Name: "lookup"}}, OutputConfig: &AnthropicOutputConfig{Effort: "max"}, Messages: []AnthropicMessage{
		{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"  old plan\n","signature":"foreign"},{"type":"text","text":"<thinking>quoted text</thinking>"}]`)},
		{Role: "user", Content: json.RawMessage(`"continue"`)},
	}}
	legacy, err := AnthropicToResponsesWithMinOutputTokens(req, 128)
	require.NoError(t, err)
	defaults, err := AnthropicToResponsesWithOptions(req, ResponsesConversionOptions{MinOutputTokens: 128})
	require.NoError(t, err)
	require.Equal(t, legacy, defaults)
	require.Equal(t, 128, *legacy.MaxOutputTokens)
	converted, err := AnthropicToResponsesWithOptions(req, ResponsesConversionOptions{MinOutputTokens: 128, PreserveReasoningContent: true, PreserveReasoningEffort: true, VolcengineCompatibility: true})
	require.NoError(t, err)
	var input []ResponsesInputItem
	require.NoError(t, json.Unmarshal(converted.Input, &input))
	require.Equal(t, "  old plan\n", input[0].Summary[0].Text)
	require.Empty(t, input[0].EncryptedContent)
	require.JSONEq(t, `[{"type":"output_text","text":"<thinking>quoted text</thinking>"}]`, string(input[1].Content))
	require.Equal(t, 17, *converted.MaxOutputTokens)
	require.Equal(t, "max", converted.Reasoning.Effort)
}

func TestVolcengineMessagesRejectUnrecoverableHistory(t *testing.T) {
	for _, tc := range []struct{ content, errorText string }{
		{`[{"type":"thinking","signature":"foreign"}]`, "complete thinking history"},
		{`[{"type":"thinking","thinking":""}]`, "complete thinking history"},
		{`[{"type":"redacted_thinking","data":"foreign"}]`, "plaintext thinking"},
		{`[{"type":"tool_use","id":"c1","name":"lookup","input":{}}]`, "missing results"},
		{`[{"type":"tool_result","tool_use_id":"c1","content":"orphan"}]`, "no matching pending call"},
	} {
		req := &AnthropicRequest{Messages: []AnthropicMessage{{Role: "assistant", Content: json.RawMessage(tc.content)}}}
		_, err := AnthropicToResponsesWithOptions(req, ResponsesConversionOptions{VolcengineCompatibility: true})
		require.ErrorContains(t, err, tc.errorText)
		_, err = AnthropicToResponses(req)
		require.NoError(t, err, "other accounts retain legacy conversion")
	}
}

func TestVolcengineReasoningPlaintextPrecedence(t *testing.T) {
	for _, tc := range []struct {
		item  string
		parts []string
	}{
		{`{"type":"reasoning","content":[{"type":"reasoning_text","text":" full A "},{"type":"reasoning_text","text":"B\n"}],"summary":[{"type":"summary_text","text":"short"}],"encrypted_content":"foreign"}`, []string{" full A ", "B\n"}},
		{`{"type":"reasoning","summary":[{"type":"summary_text","text":" full "}]}`, []string{" full "}},
		{`{"type":"reasoning","summary":[{"type":"summary_text","text":"short"}],"encrypted_content":"foreign"}`, nil},
		{`{"type":"reasoning","id":"lost","summary":[]}`, nil},
	} {
		require.Equal(t, tc.parts, VolcengineReasoningParts(json.RawMessage(tc.item)))
	}
}

func TestVolcengineReasoningStreamSnapshotsAndRepresentations(t *testing.T) {
	for _, firstKind := range []string{"raw", "summary"} {
		t.Run(firstKind, func(t *testing.T) {
			state := NewResponsesEventToAnthropicState()
			state.VolcengineCompatibility = true
			var text strings.Builder
			process := func(event ResponsesStreamEvent) {
				for _, out := range ResponsesEventToAnthropicEvents(&event, state) {
					if out.Delta != nil {
						text.WriteString(out.Delta.Thinking)
					}
				}
			}
			process(ResponsesStreamEvent{Type: "response.created", Response: &ResponsesResponse{ID: "r", Model: "ep-ark"}})
			for i, thought := range []string{"原文A", "原文B"} {
				process(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: i, Item: &ResponsesOutput{Type: "reasoning", ID: thought}})
				delta, done, other := "response.reasoning_text.delta", "response.reasoning_text.done", "response.reasoning_summary_text.delta"
				if firstKind == "summary" {
					delta, done, other = other, "response.reasoning_summary_text.done", delta
				}
				process(ResponsesStreamEvent{Type: delta, OutputIndex: i, Delta: thought})
				process(ResponsesStreamEvent{Type: other, OutputIndex: i, Delta: "duplicate"})
				process(ResponsesStreamEvent{Type: done, OutputIndex: i, Text: thought + "!"})
				process(ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: i, Item: &ResponsesOutput{Type: "reasoning", ID: thought, Content: []ResponsesContentPart{{Type: "reasoning_text", Text: thought + "!"}}, Summary: []ResponsesSummary{{Type: "summary_text", Text: thought + "!"}}}})
			}
			require.Equal(t, "原文A!原文B!", text.String())
		})
	}
}

func TestVolcengineStreamTerminalOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		state := NewResponsesEventToAnthropicState()
		state.VolcengineCompatibility, state.MessageStartSent = enabled, true
		out := FinalizeResponsesAnthropicStream(state)
		require.Equal(t, enabled, out[0].Type == "error")
	}
	state := NewResponsesEventToAnthropicState()
	state.VolcengineCompatibility = true
	require.Equal(t, "error", FinalizeResponsesAnthropicStream(state)[0].Type)
	require.Empty(t, FinalizeResponsesAnthropicStream(state))
}

func TestVolcengineStreamErrorsNeverFinishSuccessfully(t *testing.T) {
	for _, event := range []ResponsesStreamEvent{
		{Type: "error", Code: "overloaded", Message: "upstream unavailable"},
		{Type: "error", Error: &ResponsesError{Code: "overloaded", Message: "upstream unavailable"}},
		{Type: "response.done", Response: &ResponsesResponse{Status: "failed", Error: &ResponsesError{Code: "overloaded", Message: "upstream unavailable"}}},
		{Type: "response.cancelled"},
	} {
		state := NewResponsesEventToAnthropicState()
		state.VolcengineCompatibility = true
		out := ResponsesEventToAnthropicEvents(&event, state)
		require.Len(t, out, 1)
		require.Equal(t, "error", out[0].Type)
		require.NotEmpty(t, out[0].Error.Message)
		require.Empty(t, FinalizeResponsesAnthropicStream(state))
		require.Empty(t, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.completed"}, state))
	}
}

func TestVolcengineStreamIncompleteAndCompleted(t *testing.T) {
	for _, status := range []string{"completed", "incomplete"} {
		state := NewResponsesEventToAnthropicState()
		state.VolcengineCompatibility = true
		ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.created", Response: &ResponsesResponse{ID: "r"}}, state)
		response := &ResponsesResponse{ID: "r", Status: status, IncompleteDetails: &ResponsesIncompleteDetails{Reason: "max_output_tokens"}}
		out := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response." + status, Response: response}, state)
		require.Equal(t, "message_stop", out[len(out)-1].Type)
		want := "end_turn"
		if status == "incomplete" {
			want = "max_tokens"
		}
		require.Equal(t, want, out[len(out)-2].Delta.StopReason)
		require.Empty(t, FinalizeResponsesAnthropicStream(state))
		require.Empty(t, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.output_text.delta", Delta: "late"}, state))
	}
}

func TestVolcengineReasoningStreamRecoversCompletePlaintext(t *testing.T) {
	full := ResponsesOutput{ID: "rs-full", Type: "reasoning", Content: []ResponsesContentPart{{Type: "reasoning_text", Text: " complete 原文\n"}}, Summary: []ResponsesSummary{{Type: "summary_text", Text: "short summary"}}}
	created := ResponsesStreamEvent{Type: "response.created", Response: &ResponsesResponse{ID: "resp-full", Model: "ep-ark"}}
	added := ResponsesStreamEvent{Type: "response.output_item.added", Item: &ResponsesOutput{ID: full.ID, Type: "reasoning"}}
	done := ResponsesStreamEvent{Type: "response.output_item.done", Item: &full}
	terminal := ResponsesStreamEvent{Type: "response.completed", Response: &ResponsesResponse{ID: "resp-full", Status: "completed", Output: []ResponsesOutput{full}}}
	for _, tc := range []struct {
		name   string
		events []ResponsesStreamEvent
	}{
		{"summary_before_raw", []ResponsesStreamEvent{created, added, {Type: "response.reasoning_summary_text.delta", Delta: "short summary"}, {Type: "response.reasoning_text.delta", Delta: " complete 原文"}, done, terminal}},
		{"raw_before_summary", []ResponsesStreamEvent{created, added, {Type: "response.reasoning_text.delta", Delta: " complete 原文"}, {Type: "response.reasoning_summary_text.delta", Delta: "short summary"}, done, terminal}},
		{"raw_only_at_item_done", []ResponsesStreamEvent{created, added, {Type: "response.reasoning_summary_text.delta", Delta: "short summary"}, done, terminal}},
		{"item_done_without_added", []ResponsesStreamEvent{created, done, terminal}},
		{"terminal_only", []ResponsesStreamEvent{terminal}},
		{"empty_done_then_terminal", []ResponsesStreamEvent{created, added, {Type: "response.output_item.done", Item: &ResponsesOutput{ID: full.ID, Type: "reasoning"}}, terminal}},
		{"raw_only_at_terminal", []ResponsesStreamEvent{created, added, {Type: "response.reasoning_summary_text.delta", Delta: "short summary"}, terminal}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := NewResponsesEventToAnthropicState()
			state.VolcengineCompatibility = true
			var thinking strings.Builder
			starts, stops, messageStarts := 0, 0, 0
			for _, evt := range tc.events {
				for _, out := range ResponsesEventToAnthropicEvents(&evt, state) {
					switch out.Type {
					case "message_start":
						messageStarts++
					case "content_block_start":
						starts++
					case "content_block_stop":
						stops++
					}
					if out.Delta != nil {
						thinking.WriteString(out.Delta.Thinking)
					}
				}
			}
			require.Equal(t, ResponsesToAnthropic(terminal.Response, "ep-ark").Content[0].Thinking, thinking.String())
			require.Equal(t, 1, messageStarts)
			require.Equal(t, 1, starts)
			require.Equal(t, starts, stops)
			require.Empty(t, state.volcengineReasoning.pending)
			require.True(t, state.MessageStopSent)
			require.Empty(t, FinalizeResponsesAnthropicStream(state))
		})
	}
}

func TestVolcengineReasoningStreamPartsAndBlockOrder(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	state.VolcengineCompatibility = true
	var got strings.Builder
	var blocks []string
	process := func(evt ResponsesStreamEvent) {
		for _, out := range ResponsesEventToAnthropicEvents(&evt, state) {
			if out.ContentBlock != nil {
				blocks = append(blocks, out.ContentBlock.Type)
			}
			if out.Delta != nil {
				got.WriteString(out.Delta.Thinking)
			}
		}
	}
	for i := range 2 {
		process(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: i, Item: &ResponsesOutput{Type: "reasoning"}})
		process(ResponsesStreamEvent{Type: "response.reasoning_summary_text.delta", OutputIndex: i, SummaryIndex: 1, Delta: "B\n"})
		process(ResponsesStreamEvent{Type: "response.reasoning_summary_text.delta", OutputIndex: i, SummaryIndex: 0, Delta: " A"})
		process(ResponsesStreamEvent{Type: "response.reasoning_summary_text.done", OutputIndex: i, SummaryIndex: 0, Text: " A!"})
		process(ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: i, Item: &ResponsesOutput{Type: "reasoning"}})
		require.Empty(t, state.volcengineReasoning.pending, "finished plaintext must be released")
	}
	process(ResponsesStreamEvent{Type: "response.output_text.delta", OutputIndex: 2, Delta: "answer"})
	process(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 3, Item: &ResponsesOutput{Type: "function_call", CallID: "call-1", Name: "lookup"}})
	process(ResponsesStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: 3, Arguments: "{}"})
	process(ResponsesStreamEvent{Type: "response.completed", Response: &ResponsesResponse{Status: "completed"}})
	require.Equal(t, " A!B\n A!B\n", got.String())
	require.Equal(t, []string{"thinking", "thinking", "text", "tool_use"}, blocks)
}

func TestVolcengineReasoningStreamFlushesMissingDoneBeforeNextOutput(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	state.VolcengineCompatibility = true
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.reasoning_summary_text.delta", Delta: "full thought"}, state)
	out := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.output_text.delta", OutputIndex: 1, Delta: "answer"}, state)
	require.Equal(t, "message_start", out[0].Type)
	require.Equal(t, "thinking", out[1].ContentBlock.Type)
	require.Equal(t, "full thought", out[2].Delta.Thinking)
	require.Equal(t, "content_block_stop", out[3].Type)
	require.Equal(t, "text", out[4].ContentBlock.Type)
	require.Equal(t, "answer", out[5].Delta.Text)
	require.Empty(t, state.volcengineReasoning.pending)
	require.Equal(t, "error", FinalizeResponsesAnthropicStream(state)[0].Type)
}

func TestVolcengineReasoningStreamReleasesBuffersOnTermination(t *testing.T) {
	for _, failure := range []string{"eof", "response.failed", "response.cancelled"} {
		t.Run(failure, func(t *testing.T) {
			state := NewResponsesEventToAnthropicState()
			state.VolcengineCompatibility = true
			ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.reasoning_summary_text.delta", Delta: strings.Repeat("x", 64<<10)}, state)
			require.NotEmpty(t, state.volcengineReasoning.pending)
			var out []AnthropicStreamEvent
			if failure == "eof" {
				out = FinalizeResponsesAnthropicStream(state)
			} else {
				out = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: failure}, state)
			}
			require.Equal(t, "error", out[0].Type)
			require.Nil(t, state.volcengineReasoning.pending)
			require.Empty(t, FinalizeResponsesAnthropicStream(state))
		})
	}
}
