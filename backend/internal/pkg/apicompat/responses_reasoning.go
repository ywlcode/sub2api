package apicompat

import "strings"

// responsesReasoningText prefers the visible summary, falling back to raw
// reasoning text from compatible upstreams that leave the summary empty.
func responsesReasoningText(item ResponsesOutput) string {
	var text strings.Builder
	for _, part := range item.Summary {
		if part.Type == "summary_text" {
			_, _ = text.WriteString(part.Text)
		}
	}
	if text.Len() == 0 {
		for _, part := range item.Content {
			if part.Type == "reasoning_text" {
				_, _ = text.WriteString(part.Text)
			}
		}
	}
	return text.String()
}

// Prefer full reasoning content when an upstream supplies it. Native DeepSeek
// returns reasoning_text, whereas Ark DeepSeek returns plaintext in summary.
// The fallback retains the existing summary-only behavior for other providers.
func responsesOutputReasoningText(item ResponsesOutput) string {
	var text strings.Builder
	for _, part := range item.Content {
		if part.Type == "reasoning_text" {
			_, _ = text.WriteString(part.Text)
		}
	}
	if text.Len() > 0 {
		return text.String()
	}
	for _, part := range item.Summary {
		if part.Type == "summary_text" {
			_, _ = text.WriteString(part.Text)
		}
	}
	return text.String()
}
