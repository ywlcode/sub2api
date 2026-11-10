package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/jsonview"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// Text compaction has no protocol-level marker. Match the observed Codex prompt
// exactly, including when it arrives through the Messages bridge. Ordinary
// summarization requests and future, unrecognized client prompts stay unchanged.
const volcengineCompactionPrompt = `You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.

Include:
- Current progress and key decisions made
- Important context, constraints, or user preferences
- What remains to be done (clear next steps)
- Any critical data, examples, or references needed to continue

Be concise, structured, and focused on helping the next LLM seamlessly continue the work.`

const (
	volcengineCompactionTimeout       = 5 * time.Minute
	volcengineCompactionMaxEventBytes = 4 << 20
	volcengineCompactionMaxTextBytes  = 1 << 20
	volcengineCompactionDefaultTokens = 8192
)

func isVolcengineTextCompaction(body []byte) bool {
	if !bytes.Contains(body, []byte("CONTEXT CHECKPOINT COMPACTION")) {
		return false
	}
	fields := normalizationObjectFields(jsonview.Parse(body))
	tools := fields["tools"]
	if tools.Exists() && !tools.IsArray() && tools.Type != gjson.Null || len(tools.Array()) != 0 {
		return false
	}
	input := fields["input"].Array()
	if len(input) == 0 {
		return false
	}
	last := normalizationObjectFields(input[len(input)-1])
	if last["role"].String() != "user" || last["type"].Exists() && last["type"].String() != "message" {
		return false
	}
	content := last["content"]
	text := content.String()
	if content.IsArray() {
		var parts []string
		for _, part := range content.Array() {
			fields := normalizationObjectFields(part)
			if fields["type"].String() != "input_text" {
				return false
			}
			parts = append(parts, fields["text"].String())
		}
		text = strings.Join(parts, "\n")
	}
	return strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n")) == volcengineCompactionPrompt
}

type volcengineCompactionError struct {
	reason string
	usage  *apicompat.ResponsesUsage
	status int
}

func (e *volcengineCompactionError) Error() string {
	return "Volcengine context compaction did not produce a complete text summary: " + e.reason
}

// Buffer only recognized compaction turns. Nothing from an unsuccessful attempt
// reaches the client, so a same-account retry cannot splice responses or make
// Codex silently reuse the previous assistant answer as its new summary.
func forwardVolcengineTextCompaction(req *http.Request, body []byte, send func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	defer func() { _ = req.Body.Close() }()
	ctx, cancel := context.WithTimeout(req.Context(), volcengineCompactionTimeout)
	defer cancel()
	if caller, ok := ctx.Value(apiKeyHealthCallerContextKey{}).(context.Context); ok && caller != nil {
		stop := context.AfterFunc(caller, cancel)
		defer stop()
	}
	// Canonicalize top-level duplicates with encoding/json's last-value rule;
	// retain history as raw JSON instead of decoding a potentially large replay.
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	if value := request["max_output_tokens"]; len(value) == 0 || string(value) == "null" {
		request["max_output_tokens"] = json.RawMessage(fmt.Sprint(volcengineCompactionDefaultTokens))
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	var stream bool
	_ = json.Unmarshal(request["stream"], &stream)
	var total *apicompat.ResponsesUsage
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return nil, &volcengineCompactionError{reason: "request canceled or deadline exceeded", usage: total, status: http.StatusGatewayTimeout}
		}
		prepared := req.Clone(ctx)
		attemptBody := body
		prepared.Body = io.NopCloser(bytes.NewReader(attemptBody))
		prepared.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(attemptBody)), nil }
		prepared.ContentLength = int64(len(attemptBody))
		prepared.TransferEncoding = nil
		prepared.Header.Del("Content-Length")
		resp, err := send(prepared)
		_ = prepared.Body.Close()
		retryable, reason, status := true, "upstream connection failed", http.StatusBadGateway
		var final *apicompat.ResponsesResponse
		if resp != nil && resp.Body != nil {
			stopClose := context.AfterFunc(ctx, func() { _ = resp.Body.Close() })
			if err == nil && resp.StatusCode < 400 {
				final, retryable, reason = readVolcengineCompaction(resp)
			} else if err == nil {
				retryable = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
				reason, status = fmt.Sprintf("upstream HTTP %d", resp.StatusCode), mapUpstreamStatusCode(resp.StatusCode)
			}
			stopClose()
			_ = resp.Body.Close()
		}
		if final != nil {
			total = addVolcengineCompactionUsage(total, final.Usage)
		}
		if ctx.Err() != nil {
			return nil, &volcengineCompactionError{reason: "request canceled or deadline exceeded", usage: total, status: http.StatusGatewayTimeout}
		}
		if reason == "" && final != nil {
			final.Usage = total
			completed, encodeErr := encodeVolcengineCompactionResponse(resp, final, stream)
			if completed != nil {
				// The buffered response outlives this helper's timeout context.
				completed.Request = prepared.Clone(req.Context())
			}
			return completed, encodeErr
		}
		if attempt != 0 || !retryable {
			return nil, &volcengineCompactionError{reason: reason, usage: total, status: status}
		}
		// Keep history, model, tools, explicit output cap and all other settings.
		// Only disable thinking for the single recovery attempt.
		var reasoning map[string]json.RawMessage
		_ = json.Unmarshal(request["reasoning"], &reasoning)
		if reasoning == nil {
			reasoning = make(map[string]json.RawMessage)
		}
		reasoning["effort"] = json.RawMessage(`"none"`)
		request["reasoning"], _ = json.Marshal(reasoning)
		delete(request, "thinking")
		delete(request, "reasoning_effort")
		body, err = json.Marshal(request)
		if err != nil {
			return nil, err
		}
		logger.LegacyPrintf("service.openai_gateway", "volcengine_compaction_retry reason=%s", reason)
	}
	panic("unreachable")
}

type volcengineCompactionPart struct{ output, content int }
type volcengineCompactionText struct {
	parts map[volcengineCompactionPart]*strings.Builder
	bytes int
}

func (s *volcengineCompactionText) add(output, content int, text string, snapshot bool) bool {
	if output < 0 || content < 0 {
		return false
	}
	if s.parts == nil {
		s.parts = make(map[volcengineCompactionPart]*strings.Builder)
	}
	key := volcengineCompactionPart{output, content}
	part := s.parts[key]
	if part == nil {
		part = &strings.Builder{}
		s.parts[key] = part
	}
	if snapshot {
		if !strings.HasPrefix(text, part.String()) {
			return false
		}
		text = text[part.Len():]
	}
	if len(text) > volcengineCompactionMaxTextBytes-s.bytes || len(s.parts) > 1024 {
		return false
	}
	s.bytes += len(text)
	part.WriteString(text)
	return true
}

func (s *volcengineCompactionText) item(index int, item apicompat.ResponsesOutput) bool {
	if item.Type == "reasoning" {
		return true
	}
	if item.Type != "message" || item.Role != "assistant" || item.Status != "" && item.Status != "completed" {
		return false
	}
	for i, part := range item.Content {
		if part.Type != "output_text" || !s.add(index, i, part.Text, true) {
			return false
		}
	}
	return true
}

func (s *volcengineCompactionText) text() string {
	keys := make([]volcengineCompactionPart, 0, len(s.parts))
	for key := range s.parts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].output < keys[j].output || keys[i].output == keys[j].output && keys[i].content < keys[j].content
	})
	var out strings.Builder
	for _, key := range keys {
		out.WriteString(s.parts[key].String())
	}
	return out.String()
}

// A completed transport event alone is insufficient for a text summary. Never
// promote reasoning into output_text, or accept truncated/refused/tool output.
func readVolcengineCompaction(resp *http.Response) (*apicompat.ResponsesResponse, bool, string) {
	var final *apicompat.ResponsesResponse
	var text volcengineCompactionText
	terminal := ""
	if !isEventStreamResponse(resp.Header) {
		body, err := io.ReadAll(io.LimitReader(resp.Body, volcengineCompactionMaxEventBytes+1))
		if len(body) > volcengineCompactionMaxEventBytes {
			return nil, false, "response exceeded compaction size limit"
		}
		if err != nil {
			return nil, true, "upstream response ended unexpectedly"
		}
		final = &apicompat.ResponsesResponse{}
		if json.Unmarshal(body, final) != nil {
			return nil, true, "invalid response JSON"
		}
		terminal = "response." + final.Status
	} else {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64<<10), volcengineCompactionMaxEventBytes)
		for scanner.Scan() {
			data, ok := extractOpenAISSEDataLine(scanner.Text())
			if !ok {
				continue
			}
			var event apicompat.ResponsesStreamEvent
			if json.Unmarshal([]byte(data), &event) != nil {
				return nil, true, "invalid response event"
			}
			switch event.Type {
			case "response.output_text.delta", "response.output_text.done":
				value := event.Delta
				if event.Type == "response.output_text.done" {
					value = event.Text
				}
				if !text.add(event.OutputIndex, event.ContentIndex, value, event.Type == "response.output_text.done") {
					return nil, false, "inconsistent or oversized summary text"
				}
			case "response.output_item.done":
				if event.Item != nil && !text.item(event.OutputIndex, *event.Item) {
					return nil, false, "unexpected summary output item"
				}
			case "response.refusal.delta", "response.refusal.done":
				return nil, false, "summary refused by upstream"
			case "error":
				return nil, openAIStreamErrorEventShouldFailover([]byte(data), extractOpenAISSEErrorMessage([]byte(data))), "upstream rejected the summary request"
			case "response.completed", "response.done", "response.incomplete", "response.failed", "response.cancelled", "response.canceled":
				terminal, final = event.Type, event.Response
				if final != nil && final.Usage == nil {
					final.Usage = event.Usage
				}
			}
			if terminal != "" {
				break
			}
		}
		if terminal == "" {
			oversized := errors.Is(scanner.Err(), bufio.ErrTooLong) || errors.Is(scanner.Err(), errVolcengineSSEEventTooLarge)
			return nil, !oversized, "stream ended without a complete terminal event"
		}
	}
	if final == nil {
		return nil, true, "terminal event has no response object"
	}
	if terminal == "response.incomplete" && final.IncompleteDetails != nil && final.IncompleteDetails.Reason == "max_output_tokens" {
		return final, true, "summary reached max_output_tokens"
	}
	if terminal != "response.completed" && terminal != "response.done" || final.Status != "completed" || final.Error != nil || final.IncompleteDetails != nil {
		retryable := terminal == "response.failed" && openAIStreamFailedEventShouldFailover(mustMarshalVolcengineCompaction(final), "")
		return final, retryable, "upstream did not complete the summary"
	}
	for i, item := range final.Output {
		if !text.item(i, item) {
			return final, false, "unexpected summary output item"
		}
	}
	summary := text.text()
	if strings.TrimSpace(summary) == "" {
		return final, true, "completed response has no text summary"
	}
	if final.ID == "" {
		final.ID = "resp_" + uuid.NewString()
	}
	if final.CreatedAt == 0 {
		final.CreatedAt = time.Now().Unix()
	}
	final.Object = "response"
	final.Output = []apicompat.ResponsesOutput{{Type: "message", ID: "msg_" + uuid.NewString(), Role: "assistant", Status: "completed", Content: []apicompat.ResponsesContentPart{{Type: "output_text", Text: summary}}}}
	return final, false, ""
}

func mustMarshalVolcengineCompaction(final *apicompat.ResponsesResponse) []byte {
	data, _ := json.Marshal(map[string]any{"response": final})
	return data
}

func addVolcengineCompactionUsage(dst, src *apicompat.ResponsesUsage) *apicompat.ResponsesUsage {
	if src == nil {
		return dst
	}
	if dst == nil {
		dst = &apicompat.ResponsesUsage{}
	}
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.TotalTokens += src.TotalTokens
	dst.CacheCreationInputTokens += src.CacheCreationInputTokens
	if src.InputTokensDetails != nil {
		if dst.InputTokensDetails == nil {
			dst.InputTokensDetails = &apicompat.ResponsesInputTokensDetails{}
		}
		dst.InputTokensDetails.CachedTokens += src.InputTokensDetails.CachedTokens
		dst.InputTokensDetails.AudioTokens += src.InputTokensDetails.AudioTokens
		dst.InputTokensDetails.CacheCreationTokens += src.InputTokensDetails.CacheCreationTokens
		dst.InputTokensDetails.CacheWriteTokens += src.InputTokensDetails.CacheWriteTokens
	}
	if src.OutputTokensDetails != nil {
		if dst.OutputTokensDetails == nil {
			dst.OutputTokensDetails = &apicompat.ResponsesOutputTokensDetails{}
		}
		dst.OutputTokensDetails.ReasoningTokens += src.OutputTokensDetails.ReasoningTokens
		dst.OutputTokensDetails.AudioTokens += src.OutputTokensDetails.AudioTokens
		dst.OutputTokensDetails.AcceptedPredictionTokens += src.OutputTokensDetails.AcceptedPredictionTokens
		dst.OutputTokensDetails.RejectedPredictionTokens += src.OutputTokensDetails.RejectedPredictionTokens
	}
	return dst
}

func encodeVolcengineCompactionResponse(resp *http.Response, final *apicompat.ResponsesResponse, stream bool) (*http.Response, error) {
	var wire []byte
	var err error
	if !stream {
		wire, err = json.Marshal(final)
	} else {
		message := final.Output[0]
		part := message.Content[0]
		created := *final
		created.Status, created.Output, created.Usage = "in_progress", []apicompat.ResponsesOutput{}, nil
		added := message
		added.Status, added.Content = "in_progress", []apicompat.ResponsesContentPart{}
		events := []apicompat.ResponsesStreamEvent{
			{Type: "response.created", Response: &created},
			{Type: "response.output_item.added", Item: &added},
			{Type: "response.content_part.added", ItemID: message.ID, Part: &apicompat.ResponsesContentPart{Type: "output_text"}},
			{Type: "response.output_text.delta", ItemID: message.ID, Delta: part.Text},
			{Type: "response.output_text.done", ItemID: message.ID, Text: part.Text},
			{Type: "response.content_part.done", ItemID: message.ID, Part: &part},
			{Type: "response.output_item.done", Item: &message},
			{Type: "response.completed", Response: final},
		}
		var out bytes.Buffer
		for i := range events {
			events[i].SequenceNumber = i
			data, marshalErr := json.Marshal(events[i])
			if marshalErr != nil {
				return nil, marshalErr
			}
			fmt.Fprintf(&out, "event: %s\ndata: %s\n\n", events[i].Type, data)
		}
		wire = out.Bytes()
	}
	if err != nil {
		return nil, err
	}
	resp.Header = resp.Header.Clone()
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.Header.Set("Content-Type", "application/json")
	if stream {
		resp.Header.Set("Content-Type", "text/event-stream")
	}
	resp.Body, resp.ContentLength = io.NopCloser(bytes.NewReader(wire)), int64(len(wire))
	return resp, nil
}

// Exhausted compaction recovery is a request failure, not another scheduling
// attempt. Return a protocol error even if a heartbeat has committed HTTP 200.
func writeVolcengineCompactionError(c *gin.Context, err error, protocol string, stream bool, model string) (*OpenAIForwardResult, bool) {
	var failure *volcengineCompactionError
	if !errors.As(err, &failure) {
		return nil, false
	}
	result := &OpenAIForwardResult{Model: model, UsageUnavailable: failure.usage == nil, ObservedUsageOnError: failure.usage != nil}
	result.Usage = copyOpenAIUsageFromResponsesUsage(failure.usage)
	if c.Request != nil && c.Request.Context().Err() != nil {
		result.ClientDisconnect = true
		return result, true
	}
	message := failure.Error()
	setOpsUpstreamError(c, failure.status, message, "")
	MarkResponseCommitted(c)
	if stream {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Del("Content-Length")
		if protocol == APIProtocolAnthropic {
			_, _ = c.Writer.WriteString(buildAnthropicStreamErrorSSE("api_error", message))
		} else {
			source, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "server_error", "code": "volcengine_compaction_failed", "message": message}})
			_, _ = c.Writer.WriteString(buildOpenAIResponseFailedSSE("", model, time.Now().Unix(), source, message))
		}
		c.Writer.Flush()
	} else if protocol == APIProtocolAnthropic {
		writeAnthropicError(c, failure.status, "api_error", message)
	} else {
		writeResponsesError(c, failure.status, "volcengine_compaction_failed", message)
	}
	return result, true
}
