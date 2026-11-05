package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesReasoningSurvivesNonStreamingBridges(t *testing.T) {
	for _, tc := range []struct {
		name string
		item ResponsesOutput
		want string
	}{
		{"native", ResponsesOutput{Type: "reasoning", Content: []ResponsesContentPart{{Type: "reasoning_text", Text: "original thought"}}}, "original thought"},
		{"ark", ResponsesOutput{Type: "reasoning", Summary: []ResponsesSummary{{Type: "summary_text", Text: "original thought"}}}, "original thought"},
		{"raw_preferred", ResponsesOutput{Type: "reasoning", Content: []ResponsesContentPart{{Type: "reasoning_text", Text: "full original thought"}}, Summary: []ResponsesSummary{{Type: "summary_text", Text: "short summary"}}}, "full original thought"},
		{"ciphertext_only", ResponsesOutput{Type: "reasoning", EncryptedContent: "opaque"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &ResponsesResponse{ID: "resp_fixture", Status: "completed", Output: []ResponsesOutput{
				tc.item,
				{Type: "function_call", CallID: "call_1", Name: "lookup", Arguments: "{}"},
			}}
			chat := ResponsesToChatCompletions(response, "deepseek-flash")
			require.Equal(t, tc.want, chat.Choices[0].Message.ReasoningContent)
			require.Equal(t, "call_1", chat.Choices[0].Message.ToolCalls[0].ID)
			anthropic := ResponsesToAnthropic(response, "deepseek-flash")
			require.Equal(t, "thinking", anthropic.Content[0].Type)
			require.Equal(t, tc.want, anthropic.Content[0].Thinking)
			require.Equal(t, tc.item.EncryptedContent, anthropic.Content[0].Signature)
			require.Equal(t, "tool_use", anthropic.Content[1].Type)
			require.Equal(t, "call_1", anthropic.Content[1].ID)
		})
	}
}
